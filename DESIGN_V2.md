# simchain-go V2 设计文档：按需拉取 + 节点加入同步

> 本文是基于当前 `DESIGN.md` 与现有实现的 **V2 演进设计**：补齐“节点中途加入如何追链/追交易”的能力，并把当前“直接广播整笔交易/整块”的模式，升级为更通用的 **公告（inv）+ 按需拉取（getdata）** 的同步方式。

---

## 0. 背景：当前版本（V1）的问题

当前系统（V1）核心是：节点挖到块/收到交易后，通过 `NetworkBus.Broadcast` 把 **完整交易** 或 **完整区块** 广播给所有节点；接收方直接处理并入库。

这带来几个典型局限：
- **中途加入的节点无法追历史**：只会收到加入后的广播，加入前的区块/交易不会自动补齐。
- **广播整块/整交易不通用**：真实系统通常先广播“我有了什么”（hash/摘要），再按需拉取数据，避免无谓带宽消耗，也便于缺块重传与限流。
- **缺少标准同步状态机**：例如 headers-first、block locator、请求窗口、超时重试、peer 选择等（V2 可做极简版）。

---

## 1. V2 目标与非目标

### 1.1 目标
- 支持 **节点随时加入**：新节点能够从已有节点同步到当前主链 tip（以及必要的分叉信息）。
- 同步模式从“广播数据”升级为：
  - **广播公告（Inventory / Inv）**：只广播 `txid`、`blockhash` 或 `blockheader` 摘要；
  - **按需拉取（GetData）**：接收方在校验/判断需要后，定向向某个 peer 请求完整数据。
- 仍保持：单机多节点、极简 PoW、可观察日志、尽量少依赖（优先标准库）。

### 1.2 非目标（V2 暂不做）
- 不实现真实 P2P 连接管理（握手加密、NAT、peer discovery、黑名单等）。
- 不做复杂交易校验（签名、余额/UTXO），交易仍是 payload-only。
- 不追求高性能；只做正确性与可理解性。

---

## 2. 核心概念（统一口径）

### 2.1 主链是什么
- “主链”不是一个服务器或中心节点，而是 **每个节点本地**在所有已知区块中选出的那条“最优链”。
- V2 仍按 **累积工作量最大（cumWork 最大）** 选 tip（难度固定时≈最长链）。

### 2.2 mempool 是什么
- `mempool` 是 **每个节点本地**的“未确认交易池”：
  - 存放：已看到但还没被自己认为“写入主链”的交易。
  - 作用：矿工从 mempool 取交易打包；链重组时把旧主链块中的交易退回 mempool，避免交易凭空消失。

### 2.3 fork / reorg 是什么
- `fork`（分叉）：同一父块下出现多个子块（并发挖矿 + 网络延迟导致）。
- `reorg`（链重组）：节点发现另一条分叉链的累积工作量更大，于是主链 tip 切到那条链。
- V2 中 reorg 仍由链模块计算出差异集：`CommonAncestor + Removed + Added`，用于修正 mempool（退回 Removed 的交易、删除 Added 的交易）。

---

## 3. V2 总体架构：从“广播数据”到“公告+拉取”

### 3.1 V2 数据流（高层）

1) 产生交易：
- 节点收到本地提交交易 → 先入本地 mempool → 向网络广播 `InvTx(txid)`（只告知“我有 txid”）。

2) 交易传播：
- 其他节点收到 `InvTx`：
  - 若本地没有该 txid，则向某个 peer 发送 `GetTx(txid)`（定向请求）。
  - 对方回 `Tx(transaction)`，接收方再入 mempool（并可继续 `InvTx` 扩散，或采用抑制机制避免风暴）。

3) 挖矿出块：
- 矿工从 mempool Pop 交易挖到块 → 先写入本地链 → 广播 `InvBlock(blockhash, header摘要)`。

4) 区块传播：
- 其他节点收到 `InvBlock`：
  - 若 blockhash 未见过，并且“值得请求”（见 5.2 校验策略），则发送 `GetBlock(blockhash)`。
  - 对方回 `Block(block)`，接收方做完整校验并 `AddBlock`。

5) 节点加入同步：
- 新节点加入后主动发起 `GetTip` / `GetHeaders` / `GetBlocks`（见第 6 章流程），拉齐到当前 tip。

### 3.2 为什么要“先公告再拉取”
- 降低广播负载：不是每个节点都需要立即接收完整数据（尤其是大块/大量交易）。
- 提升鲁棒性：丢包后可重试请求；可限制请求窗口避免被淹没。
- 更贴近真实协议：现实常见 inv/getdata/headers-first 的组合。

---

## 4. 协议与消息设计（建议）

### 4.1 消息通道：广播 vs 定向

V2 需要两类投递：
- **广播（gossip）**：把 “Inv” 发送给所有其他节点（或部分节点）。
- **定向请求/响应**：`GetXxx` / `Xxx` 必须能指定 `To`，只发给目标 peer。

建议为 `NetworkBus` 增加：
- `Send(to string, msg Message)`：定向投递
- `Broadcast(from string, msg Message)`：仍用于广播，但只用于 Inv 类消息

并明确 `Message.To` 的语义：`To` 非空则必须走 `Send`，`Broadcast` 忽略 `To`。

### 4.2 消息类型（V2 建议集合）

> 下列命名是建议：V2 起采用 Bitcoin 风格的 `Inv*/Get*/*` 消息集合（更贴近业界，也更利于扩展/调试）。

- **交易相关**
  - `InvTx`：广播，载荷 `{TxID}`
  - `GetTx`：定向请求，载荷 `{TxID}`
  - `Tx`：定向响应（或广播禁用），载荷 `{Transaction}`

- **区块相关**
  - `InvBlock`：广播，载荷 `{BlockHash, Header}`（Header 可选但推荐携带，便于先验校验 PoW/高度）
  - `GetBlock`：定向请求，载荷 `{BlockHash}`
  - `Block`：定向响应，载荷 `{Block}`

- **加入/追链（headers-first 极简版）**
  - `GetTip`：定向请求 `{}`（或带上已知 tip）
  - `Tip`：定向响应 `{TipHash, TipHeight, CumWork}`
  - `GetHeaders`：定向请求 `{Locator[]Hash, StopHash(optional)}`
  - `Headers`：定向响应 `{Headers[]BlockHeader, Hashes[]Hash}`（或每个 header 自带 hash）
  - `GetBlocks`：定向请求 `{Hashes[]Hash}`（批量请求）
  - `NotFound`：定向响应 `{Items[]Hash/TxID}`（可选）

> Locator：类似比特币的 “block locator”，用一组从 tip 逆向抽样的 hash 表示“我已经有这些点”，对方从共同祖先之后开始给 headers。

---

## 5. 校验策略：何时请求、何时丢弃

### 5.1 交易（Tx）校验（极简）
V2 仍是 payload-only tx，所以“校验”主要是：
- 基本字段合法（ID 非空、timestamp 合理范围、payload 长度限制等）
- 去重：txid 已 seen 则忽略
- 可加入简单防御：每个 peer 的 tx 请求速率限制

### 5.2 区块公告（InvBlock）收到后：先做“轻校验”
为了避免“看见一个 hash 就盲目拉全块”，建议 InvBlock 携带 header 或足够信息做轻校验：
- 若携带 `Header + Nonce + Hash`（或 header + nonce，hash 可重算）：
  - 能立即验证 PoW 是否满足难度（不需要 txs）
  - 能检查 `PrevHash` 是否已知（未知则先记为待请求/待连接，或先请求 parent header）
  - 能检查 `Height` 是否为 parent+1（如果 parent 已知）
- 但无法验证 `TxRoot`（需要 txs），因此 **完整区块校验仍必须在拉到 block 后做**。

建议行为：
- PoW 明显不满足 / 难度不一致：直接丢弃，不请求 block
- 父未知：加入 “pending blocks” 队列，优先请求 parent（headers-first 同步会覆盖此需求）
- 父已知且 PoW 通过：请求完整 block

---

## 6. 节点加入（Join）与追链流程（V2 重点）

### 6.1 Join 的目标
新节点 N 加入时，最终要达到：
- 拥有从 genesis 到当前主链 tip 的完整区块数据（至少主链全量；分叉可选）
- 本地 tip 与大多数节点一致（收敛）
- mempool 至少能接收加入之后的 inv/tx（历史 mempool 不强求完全一致）

### 6.2 极简加入流程（可行且容易实现）

假设 N 已经能拿到一个或多个 peer（在本项目里就是 Runner 创建节点时已知全部节点 ID，或 bus 提供注册列表）。

**阶段 A：获取网络 tip（选同步目标）**
1. N 向若干 peer 发送 `GetTip`
2. 收集 `Tip{height, cumWork, tipHash}`，选择 “最优 peer”（cumWork 最大；相同则随便）

**阶段 B：headers-first 同步（快速定位共同祖先）**
3. N 构造 `Locator`：
   - 从本地 tip 往回取 hash：tip、tip-1、tip-2、tip-4、tip-8…指数退避，直到 genesis
4. N → best peer：`GetHeaders{Locator}`
5. peer → N：`Headers{headers...}`（从共同祖先之后开始，最多返回一个上限比如 2000 个）
6. N 对 headers 做轻校验并“临时插入 header 索引”（或直接作为待拉取的 blockhash 列表）
7. N 计算缺失的 blockhash 列表，分批发送 `GetBlocks{hashes...}`
8. peer 分批返回 `Block{block}`；N 对每个 block 做完整校验并 `AddBlock`
9. 重复 4-8，直到 N 的 tip 达到 best peer 的 tip（或没有更多 headers）

**阶段 C：进入正常 gossip**
10. N 开始接收/发送 `InvTx/InvBlock`，并按需拉取缺失 tx/block

### 6.3 Join 过程中的并发与限流（必须有）
否则容易“请求风暴”：
- block 拉取窗口：一次最多 in-flight N 个 block 请求（如 32）
- headers 最大返回数量：如 2000
- 超时重试：GetBlock/GetTx 超时则换 peer 重试（简化版可以只重试一次）

---

## 7. 正常运行状态机（V2）

### 7.1 交易流（Tx gossip）

**本地提交**
- `SubmitTx(payload)`：
  - tx = NewTransaction
  - mempool.Add(tx)
  - Broadcast `InvTx(tx.ID)`

**收到 InvTx**
- 若 txid 已见过：忽略
- 否则：挑一个 peer（优先 msg.From），发送 `GetTx(txid)`

**收到 Tx**
- 做基本校验 + mempool.Add
- 可选择继续 Broadcast `InvTx(txid)`（或只转发给部分节点，避免爆炸；本项目可先全转发）

### 7.2 区块流（Block gossip）

**矿工挖到块**
- 本地 `AddBlock(block)` 成功后：
  - Broadcast `InvBlock{hash, header}`

**收到 InvBlock**
- 若 hash 已 known：忽略
- 若 header 校验不过：丢弃
- 若父未知：进入 pending（并触发 headers-sync 或请求 parent）
- 若父已知：Send `GetBlock(hash)` 给 msg.From（或 best peer）

**收到 Block**
- 完整校验后 `AddBlock`
- 若触发 reorg：根据 `Reorg` 修正 mempool
- 成功接入后再广播 `InvBlock`（可选，作为扩散）

---

## 8. 链与索引层（对现有实现的改动建议）

现有 `internal/blockchain` 已具备：
- 区块存储与索引（blocks/parent/height/cumWork）
- orphan 管理（父未知暂存）
- tip 选择与 reorg 计算

V2 推荐新增/强化：
- **Header-only 索引（可选）**：如果 headers-first 不想一次性拿全块，可先存 header 索引，再按需拉 block body。
  - 简化做法：仍然只有拿到完整 block 才 `AddBlock`，headers 仅用于生成“要拉取的 hash 列表”，不入库。
- **已知集合（known set）**：
  - knownBlocks（hash->seen）
  - knownTxs（txid->seen）
  - 用于抑制重复 inv 与重复请求

---

## 9. NetworkBus（V2 语义）

当前 `NetworkBus` 只有广播；V2 至少需要：
- `Send(to, msg)`：定向投递（请求/响应必须走这里）
- `Broadcast(from, msg)`：只用于 inv（或少量控制消息）
- （可选）模拟网络分区/丢包/延迟：继续保留 `SetDelay/SetDropRate`

并建议在 bus 层对消息做“路由约束”：
- `msg.To != ""`：只允许 `Send`，否则视为协议错误（便于排查）

---

## 10. 日志与可观测性（V2 推荐）

为便于理解“公告-拉取-校验-入库”链路，建议新增/统一日志：
- `INV_TX node=X from=Y tx=...`
- `GET_TX node=X to=Y tx=...`
- `TX_RECEIVED node=X from=Y tx=...`
- `INV_BLOCK node=X from=Y h=... hash=...`
- `GET_BLOCK node=X to=Y hash=...`
- `BLOCK_RECEIVED node=X from=Y h=... hash=...`
- `SYNC_START node=X peer=Y`
- `SYNC_HEADERS node=X got=N`
- `SYNC_BLOCKS node=X requested=N received=N`
- `SYNC_DONE node=X height=... tip=...`
- 现有 `CHAIN_SWITCH` 保留

---

## 11. 里程碑（推荐实施顺序）

1) **网络层支持定向消息**
- `NetworkBus.Send` + Node 侧路由

2) **引入 inv/get 协议（先做区块，再做交易）**
- 区块：`InvBlock` + `GetBlock` + `Block`
- 交易：`InvTx` + `GetTx` + `Tx`

3) **Known 集合 + 请求窗口 + 超时重试（防风暴）**

4) **Join 同步：GetTip + headers-first + 批量 GetBlocks**

5) **测试与验收**
- 单元：inv/get 的 known 抑制、缺块拉取、孤块连接
- 集成：节点延迟加入仍能最终 tip 收敛一致

---

## 12. 验收清单（V2）

- 启动 2 个节点先运行 10s，再加入第 3 个节点：
  - 第 3 个节点能在合理时间内追到与其他节点相同的 `tipHeight`（最好也能 tipHash 一致）
- 正常运行时不再广播完整 `Block/Tx`（除响应 GetData 外）
- 丢包/延迟下仍能通过重试拉齐（在合理上限内）
- 出现分叉时能 reorg 且 mempool 不丢交易、不重复打包
