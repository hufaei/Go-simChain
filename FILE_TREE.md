# simchain-go 文件结构与职责（V1 + V2 + V3-A）

本文按 `DESIGN.md`（V1）、`DESIGN_V2.md`（V2：inv 公告 + 按需拉取）、`DESIGN_V3.md`（V3-A：Transport/Syncer/Store 拆分 + 主链持久化）来解读当前仓库：每个目录/文件在系统中承担什么职责，以及它们如何拼出“交易产生 → 传播 → 挖矿出块 → 分叉/重组 → 收敛 → 主链落盘”的全流程。

---

## 目录树（到文件粒度的分工说明）

- `README.md`
  - 项目简介 + 常用运行/测试命令。
- `DESIGN.md`
  - V1 说明书：数据模型、PoW、Node/Chain/Network 分工、日志与测试策略。
- `DESIGN_V2.md`
  - V2 说明书：`Inv*` 公告 + `Get*/*` 按需拉取（避免广播整块/整交易）、headers-first、late join。
- `DESIGN_V3.md`
  - V3-A 设计：引入 `Transport/Syncer/Store` 的架构拆分与主链持久化（默认目录 `data/<nodeID>/...`）。
- `DESIGN_V3B.md`
  - V3-B 版本文件：多进程 TCP 网络 + seed 自动发现 + 最小身份绑定（ed25519）。
- `go.mod`
  - Go module 定义：模块名 `simchain-go`。

- `cmd/`
  - `cmd/simchain/main.go`
    - **Runner**：解析参数、初始化 `Transport(inproc)`、创建/启动节点、注入交易、停止并输出汇总。
    - **持久化目录（V3-A）**：每个节点默认写到 `data/<nodeID>/manifest.json` + `data/<nodeID>/blocks/<height>.json`。
    - **调试输出（块状 pretty）**：tip 变化触发 `STATE_DUMP`（可通过 `--debug-dump-on-tip=false` 关闭）。
    - **结束汇总**：`SIMULATION_SUMMARY`（含每节点 hash 尝试次数与占比）。

- `internal/`
  - `internal/types/`（协议/数据结构）
    - `internal/types/transaction.go`：极简交易 `Transaction{ID, Timestamp, Payload}`。
    - `internal/types/block.go`：`Hash/BlockHeader/Block`，以及 V2/V3 公告用的 `BlockMeta`。
    - `internal/types/message.go`
      - V2 主要消息：`InvTx/GetTx/Tx`、`InvBlock/GetBlock/Block`
      - 同步消息：`GetTip/Tip/GetHeaders/Headers/GetBlocks/Blocks`
      - `TraceID`：用于一次请求-一次响应的轻量 RPC 匹配（initial sync 用）。

  - `internal/crypto/`（PoW 与哈希）
    - `internal/crypto/pow.go`：`HashHeaderNonce`、`LeadingZeroBits/MeetsDifficulty`、`HashTransactions`、`WorkForDifficulty`。
    - `internal/crypto/pow_test.go`：PoW 难度判断测试。

  - `internal/mempool/`（未确认交易池）
    - `internal/mempool/mempool.go`
      - `Add/Pop/Return/RemoveByID/Size`
      - `Snapshot(max)`：调试输出用（查看 mempool 前 N 笔交易）。

  - `internal/blockchain/`（链索引 + 主链选择 + reorg）
    - `internal/blockchain/chain.go`
      - 区块校验与接入、orphan 连接、cumWork 选 tip、reorg 差异集（CommonAncestor/Removed/Added）
      - V2/V3 同步辅助：`Locator()`、`MainChainMetasFromLocator()`、`MainChainBlocks()`（调试/落盘用）
    - `internal/blockchain/chain_test.go`：分叉/重组与 locator 行为测试。

  - `internal/network/`（V2 仍在用的“内存网络内核”）
    - `internal/network/bus.go`
      - `Broadcast`（扇出）+ `Send`（定向）+ 延迟/丢包注入
      - V3-A 中它被 `internal/transport/inproc` 适配为 `Transport` 接口

  - `internal/transport/`（V3-A：传输抽象）
    - `internal/transport/transport.go`：Transport 接口（Broadcast/Send/Peers/Register...）
    - `internal/transport/inproc/inproc.go`：inproc 实现（适配 `NetworkBus`）

  - `internal/syncer/`（V3-A：同步器）
    - `internal/syncer/syncer.go`
      - late join/落后同步：由后台状态机持续执行 headers-first catch-up（不再依赖一次性 `InitialSync()`）
      - 已演进为明确状态机：`SyncingHeaders/SyncingBlocks/Backoff`，周期性探测 peers tip，落后时做 headers-first catch-up
      - block 拉取窗口（in-flight window）：限制并发 `GetBlock` 数量、超时重试、必要时换 peer，避免丢包/不响应导致卡死
      - 同步阶段 blocks 拉取已批量化：`GetBlocks/Blocks` 按 `BlocksBatchSize` 请求区块（inv/parent 仍用 `GetBlock/Block`）
      - **InvBlock 处理已迁入 Syncer**：轻校验 PoW 后决定是否发送 `GetBlock` 拉取完整区块，避免同步策略散落在 Node 中
      - **InvTx 处理已迁入 Syncer**：收到 `InvTx(txid)` 后按需发送 `GetTx` 拉取完整交易，并用 inflight 去重；收到 `Tx` 后调用 `cfg.OnTx` 交由 Node 决定是否写入 mempool/txStore，接纳后再统一 gossip `InvTx`
  - `internal/peer/`（V3-A：peer 选择/退避）
    - `internal/peer/peermanager.go`
      - 维护每个 peer 的 tip/cumWork/RTT/超时与冷却窗口（backoff）
      - 提供 `BestPeer()` 给 Syncer 选择同步来源

  - `internal/store/`（V3-A：主链持久化）
    - `internal/store/store.go`
      - 只持久化主链（blocks 按高度存文件 + manifest）
      - `ApplyTipChange`：tip 增长 append；reorg 回滚到共同祖先后再写入新主链段

  - `internal/node/`（节点：挖矿 + 协议处理 + 编排）
    - `internal/node/node.go`
      - 挖矿：mempool 取交易 → PoW → `chain.AddBlock` → 广播 `InvBlock(meta)`
      - 协议：作为 `GetTx/GetBlock` 的响应提供者，并把 `InvTx/Tx`、`InvBlock/Block` 路由交给 `Syncer` 决定拉取/传播策略；同步请求 `GetTip/GetHeaders/GetBlocks`
      - V3-A：同步由 `Syncer.Start()` 后的后台状态机负责（Node 更像 orchestrator）
      - 可观测：`DebugState()` 给 Runner 的 `STATE_DUMP` 使用
