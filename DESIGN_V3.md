# simchain-go V3（V3-A）设计文档：架构拆分 + 同步状态机增强 + 主链持久化

本文是在当前 V2（`inv + get`、headers-first、late join、单进程 `NetworkBus`）基础上的 **V3-A 迭代设计**。

V3-A 的明确取舍（已选定）：
- 仍然运行在 **单进程 inproc 网络**（继续用 `NetworkBus` 的“内存网络”能力做模拟与调试）。
- 引入更清晰的工程架构拆分：`Transport / PeerManager / Syncer / Store`。
- **持久化只保存“主链”**（不保存分叉块与 orphan，不持久化 mempool）。

---

## 1. V3-A 目标与非目标

### 1.1 目标
- **代码结构更清晰**：Node 负责编排；同步、网络、存储各自独立。
- **同步更像真实节点**：有同步状态机、in-flight 窗口、超时重试、peer 选择/切换（即使仍是 inproc）。
- **可重启**：节点重启后能从磁盘恢复主链 tip，并继续同步到网络最新 tip。
- **可观测**：保留当前“块状 pretty 日志”，并让调试输出与共识逻辑解耦。

### 1.2 非目标（V3-A 暂不做）
- 不做 TCP/真实 P2P 连接（这是 V3-B 或后续增强）。
- 不做真实交易校验（签名、UTXO、费用等）。
- 不做 compact blocks（BIP152 风格）与复杂带宽优化。

---

## 2. V2 现状回顾（作为 V3-A 的基线）

当前实现已经具备：
- 传播模型：`InvTx/InvBlock` 公告 + `GetTx/GetBlock` 拉取。
- late join：`GetTip -> GetHeaders(locator) -> GetBlocks` 拉齐。
- 链模块：cumWork 选 tip、orphan 暂存、reorg 差异集（Removed/Added）。
- 调试：tip 变化触发的全网 `STATE_DUMP` 与结束 `SIMULATION_SUMMARY`。

V3-A 的核心问题不是“缺功能”，而是把这些功能 **工程化**：
- 把 `InitialSync()` 从 Node 中抽离成长期运行的 Syncer；
- 把“请求-响应 + retry + window + peer 选择”做成可理解的状态机；
- 把“确认后的主链数据”写入磁盘并支持重启恢复。

---

## 3. V3-A 顶层架构（建议拆分）

把当前 Node 拆为下面几层组件（依赖方向从上到下）：

```
Node (orchestrator)
 ├─ Transport (inproc)
 ├─ PeerManager (peer 状态、评分、选择)
 ├─ Syncer (headers-first + blocks window + retry)
 ├─ Mempool (未确认交易池)
 ├─ Miner (挖矿循环)
 ├─ Chain (索引、cumWork、reorg)
 └─ Store (主链持久化：blocks + manifest)
```

关键原则：
- **Transport 负责投递，PeerManager 负责选择**：上层不直接依赖 `NetworkBus`。
- **Syncer 负责“追上网络”**：不再是一次性的 `InitialSync()`，而是一个可以持续运行的循环（也能处理丢包/延迟）。
- **Store 只保存主链**：在 tip 变化时应用增量写入（append 或 reorg rollback）。

---

## 4. Transport（V3-A：仅 inproc）

### 4.1 接口（建议）

`internal/transport/transport.go`
- `Register(id string, handler func(Message))`
- `Unregister(id string)`
- `Peers() []string`
- `Broadcast(from string, msg Message)`：用于 inv 公告
- `Send(to string, msg Message)`：用于 get/响应
- （可选）`SetDelay/SetDropRate`：保留故障注入（从 inproc transport 暴露）

### 4.2 实现

`internal/transport/inproc` 用当前 `internal/network/NetworkBus` 适配即可：
- `Broadcast`/`Send` 语义不变
- `Peers()` 直接返回已注册节点 ID

---

## 5. PeerManager（V3-A：逻辑 peer 管理）

虽然没有真实连接，但仍需要“像真实节点那样选 peer”：
- 维护每个 peer 的：
  - 最近 tip（height/cumWork/hash）
  - 失败次数/超时次数
  - 最近响应延迟（RTT）
  - 当前是否可用（cooldown/backoff）
- 提供能力：
  - `BestPeer()`：优先 cumWork 最大；其次 RTT 更低；再其次失败率更低
  - `ReportSuccess(peer, rtt)` / `ReportTimeout(peer)` / `ReportError(peer)`

数据来源：
- Syncer 周期性 `GetTip` 探测并更新 peer tip
- 请求/响应的 RTT 由 `TraceID` 对应的 RPC 完成时间计算

---

## 6. Syncer（V3-A 核心）：状态机 + 窗口 + 重试

### 6.1 Syncer 目标
让节点在以下情况下都能最终追上网络：
- late join（本地只有 genesis 或落后很多）
- 网络有延迟/丢包
- peer “质量”参差不齐（响应慢/偶尔不回）

### 6.2 状态机（建议）

- `Idle`：本地已接近 best peer tip；仍然监听 `InvBlock`，并定期 refresh `GetTip`
- `SyncingHeaders`：
  - 构造 locator（来自 Chain）
  - `GetHeaders` 获取共同祖先之后的 metas（主链视角）
  - 得到缺失 blockhash 列表，进入 `SyncingBlocks`
- `SyncingBlocks`：
  - 维护 block 下载窗口（例如最多 32 个 in-flight）
  - `GetBlocks` 批量拉取（或将来升级为 `GetData`）
  - 收到 blocks 后本地完整校验并 `AddBlock`
  - window 补齐，直到没有缺块或追到 peer tip
- `Backoff`：
  - 当前 peer 超时/错误，降低评分、短暂冷却、切换 best peer

### 6.3 必要的数据结构
- `knownBlocks/knownTxs`：避免重复请求/重复 inv 扩散
- `inflightBlocks/inflightTx`：避免并发重复拉取
- `deadline`：每个 inflight 记录发起时间，用于超时检测与重试

### 6.4 与现有消息的兼容
V3-A 不必更改协议，只需把逻辑从 Node 迁移到 Syncer：
- 使用现有 `GetTip/Tip`
- 使用现有 `GetHeaders/Headers`（metas）
- 使用现有 `GetBlocks/Blocks`

---

## 7. Store（V3-A：只持久化主链）

### 7.1 设计目标
只保存“确认后的主链数据”，并支持：
- 重启恢复：加载主链区块直到 tip
- 主链增长：append 新 tip 区块
- reorg：回滚到共同祖先高度后，再按 Added 写入新主链段

### 7.2 不保存的内容
- 分叉块、orphan：不落盘（重启后通过同步重新获取即可）
- mempool：不落盘（重启后通过 inv/tx 再次传播即可）

### 7.3 文件布局（建议）
每个节点一个数据目录（runner 传入或按 nodeID 默认生成）：

```
data/<nodeID>/
  manifest.json
  blocks/
    0000000000000000.json   (genesis)
    0000000000000001.json
    0000000000000002.json
    ...
```

`manifest.json`（建议字段）：
- `tipHeight uint64`
- `tipHash string`
- `difficulty uint32`
- `updatedAt int64`

区块文件内容：直接存 `types.Block` 的 JSON（包含 tx 列表）。

### 7.4 Store 接口（建议）
`internal/store/store.go`
- `Load() (tipHeight, tipHash, blocksUpToTip)`：启动恢复主链（按高度顺序）
- `AppendBlock(b *Block)`：tip 纯增长时写入 `blocks/<height>.json` 并更新 manifest
- `RollbackTo(height uint64)`：删除 `height+1..oldTip` 的块文件，并更新 manifest
- `Close()`

### 7.5 写入触发点（建议）
以“主链 tip 变化”为唯一写入入口（事件驱动，避免散落）：
- Node 在每次 tip 变化后发出 `TipChangeEvent{Reorg, NewTipHash, Height}`
- Store 订阅该事件（或由 Node orchestrator 调用）：
  - 若 `Reorg == nil`：append 新 tip 区块
  - 若 `Reorg != nil`：取出共同祖先高度 → rollback → 按 Added 顺序写入

> 因为链模块能按 hash 查到完整块，所以 Store 写入可以从 Chain 获取 `BlockByHash`，不必依赖网络再次拉取。

---

## 8. Node（V3-A：Orchestrator 的职责边界）

Node 在 V3-A 中应尽量“薄”：
- 初始化：创建 Chain/Mempool/Miner/Syncer/PeerManager/Store/Transport
- 组装消息路由：
  - inv/get 交给 Syncer（或 Syncer 调用 Node 提供的发送函数）
  - miner 产生的新块交给 Chain
  - Chain tip 变化事件交给 Store 与 Observability（日志）
- 生命周期：Start/Stop（确保 goroutine 可退出）

---

## 9. 迭代里程碑（V3-A：推荐顺序）

1) 抽 `Transport` 接口并用 inproc 实现适配（不改变行为）
2) 引入 `PeerManager`（先让它只负责 best peer 选择）
3) 把 `InitialSync()` 重构为 `Syncer`（状态机 + window + retry）
4) 引入 `Store`（仅主链持久化），完成“重启恢复 tip + 继续同步”
5) （可选）把 debug dump 从 runner 回调迁移成统一的 “Event Log” 模块（结构化输出）

---

## 10. 验收清单（V3-A）

- late join：节点中途加入能稳定追到网络 tip（有丢包/延迟也能最终追上）
- 重启恢复：kill 进程后重启，节点能从磁盘恢复 tip，并继续同步到最新
- reorg 持久化：发生 reorg 后磁盘上只保留新主链（旧主链分支段被回滚删除）
- 可观测：日志清晰，不影响共识与同步逻辑

