# simchain-go 文件结构与职责（V1 + V2）

本文按 `DESIGN.md`（V1：广播整块/整交易）与 `DESIGN_V2.md`（V2：inv 公告 + 按需拉取 + late join 同步）来解读当前仓库：每个目录/文件在系统中承担什么职责，以及它们如何拼出“交易产生 → 传播 → 打包挖矿 → 出块传播 → 分叉/重组 → 收敛”的全流程。

---

## 目录树（到文件粒度的分工说明）

> 约定：`cmd/` 是可执行入口；`internal/` 是内部实现（不对外暴露 API）；`.idea/` 是 IDE 配置，可忽略。

- `README.md`
  - 项目简介 + 如何运行模拟/测试的最短路径命令。
- `DESIGN.md`
  - V1 说明书：总体目标、数据模型、PoW、节点/链/网络分工、日志与测试建议。
- `DESIGN_V2.md`
  - V2 演进设计：对齐“比特币风格”的 `inv + getdata`（先公告、再按需拉取），以及“节点中途加入”的 headers-first 同步流程。
- `go.mod`
  - Go module 定义：模块名 `simchain-go`，声明 Go 版本（目前为 `go 1.25`）。

- `cmd/`
  - `cmd/simchain/main.go`
    - **模拟器 Runner（主程序入口）**：解析参数、创建 genesis、初始化 `NetworkBus`、创建 N 个节点并启动挖矿。
    - **交易注入器（Tx Injector）**：按 `--tx-interval` 周期生成 payload 并投递到某个节点。
      - V2 下节点只广播 `InvTx(txid)`，其他节点会 `GetTx` 拉取完整交易。
    - **Late join 演示**：支持 `--bootstrap-nodes` + `--join-delay` 在模拟运行中途加入新节点；新节点先 `InitialSync()` 拉齐到网络 tip，再开始挖矿。
    - **网络故障注入**：`--net-delay`/`--drop-rate` 用于模拟延迟/丢包。
    - **收尾与统计**：运行 `--duration` 后停止挖矿，打印每个节点 mined/received/height。

- `internal/`
  - `internal/types/`（协议/数据结构：Block、Tx、Message）
    - `internal/types/transaction.go`
      - **极简交易模型**：`Transaction{ID, Timestamp, Payload}`（无签名/余额/UTXO 校验，只用于演示传播与进块）。
      - `NewTransaction(payload)`：生成交易 ID（随机字节 + 时间戳）。
    - `internal/types/block.go`
      - **Hash 与区块结构**：
        - `type Hash [32]byte`：统一哈希类型（hex 输出 + JSON 编解码）。
        - `BlockHeader{Height, PrevHash, Timestamp, Difficulty, MinerID, TxRoot}`
        - `Block{Header, Nonce, Hash, Txs}`
      - `BlockMeta{Header, Nonce, Hash}`：V2 用于 `InvBlock` 公告与 headers-first 同步（足以轻校验 PoW，但不含交易列表）。
      - `NewGenesisBlock()`：创建确定性 genesis（实际 genesis hash 在 `blockchain.NewBlockchain` 内计算并落库）。
    - `internal/types/message.go`
      - **节点间消息类型**（内存网络“协议层”）：
        - V1（保留但非主路径）：`NewTx/NewBlock/RequestTip/ResponseTip`
        - V2（主路径）：
          - 交易：`InvTx/GetTx/Tx`
          - 区块：`InvBlock/GetBlock/Block`
          - 加入同步：`GetTip/Tip/GetHeaders/Headers/GetBlocks/Blocks`
      - `Message{Type, From, To, Timestamp, TraceID, Payload any}`：V2 的 `TraceID` 用于一次请求-一次响应的 RPC 式同步。

  - `internal/crypto/`（哈希与 PoW 难度相关）
    - `internal/crypto/pow.go`
      - `HashHeaderNonce(h, nonce)`：`SHA256(Serialize(header) || nonceLE)`（PoW/区块哈希）。
      - `HashTransactions(txs)`：简化版 TxRoot（拼 txid 后哈希，非 Merkle Tree）。
      - `LeadingZeroBits/MeetsDifficulty`：前导零 bit 难度判断。
      - `WorkForDifficulty`：近似 `2^difficulty`，用于累积工作量主链选择。
    - `internal/crypto/pow_test.go`
      - 覆盖前导零与难度判断的边界/正确性。

  - `internal/blockchain/`（区块索引、累积工作量、分叉与重组）
    - `internal/blockchain/chain.go`
      - **链索引**：`blocks/parent/height/cumWork/tip/orphans`，支持乱序接入、孤块暂存、累积工作量选 tip。
      - **完整校验**：difficulty/height/prevHash/timestamp/TxRoot/hash/PoW。
      - **重组信息**：`Reorg{CommonAncestor, Removed, Added}`（给节点用于修正 mempool）。
      - **V2 同步辅助**：
        - `Locator(max)`：生成 block locator（tip 往回指数抽样）。
        - `MainChainMetasFromLocator(locator, max)`：从共同祖先之后返回主链上的 `BlockMeta`（用于 headers-first）。
    - `internal/blockchain/chain_test.go`
      - 分叉/重组选链测试（V1 就有）。
      - V2：增加 locator → metas 的 headers-first 行为测试。

  - `internal/mempool/`（交易池：去重 + FIFO）
    - `internal/mempool/mempool.go`
      - `Add/Pop/Return/RemoveByID/Size`：
        - `Pop` 给矿工组块；
        - `Return` 用于 reorg 时退回旧主链交易；
        - `RemoveByID` 用于新主链确认后剔除交易，避免重复打包。

  - `internal/network/`（内存网络：广播 + 定向发送 + 故障注入）
    - `internal/network/bus.go`
      - `Register/Unregister`：节点订阅上线/下线。
      - `PeerIDs()`：当前已注册的节点列表（V2 late join 同步选 peer 用）。
      - `Broadcast(from, msg)`：扇出广播（V2 主要用于 `InvTx/InvBlock` 公告）。
      - `Send(to, msg)`：定向投递（V2 的 `Get*/*` 请求-响应走这里）。
      - `SetDelay/SetDropRate`：模拟延迟/丢包。

  - `internal/node/`（节点：挖矿循环 + inv/get + 加入同步）
    - `internal/node/node.go`
      - **挖矿**：`minerLoop` 从 mempool 取交易 → 试 nonce → 出块后本地 `AddBlock` → 广播 `InvBlock(meta)`（不广播整块）。
      - **交易传播（V2）**：
        - 本地提交：入池 + 记录 tx → 广播 `InvTx(txid)`
        - 收到 `InvTx`：未知则 `GetTx` 拉取；收到 `Tx` 后入池并再 `InvTx` 扩散
      - **区块传播（V2）**：
        - 收到 `InvBlock(meta)`：先轻校验 PoW，再 `GetBlock(hash)` 拉整块；收到 `Block` 后 `AddBlock`
      - **重组后 mempool 修正**：`applyReorg/removeTxsFromBlock`
      - **节点加入同步（V2）**：`InitialSync()`（GetTip → GetHeaders → GetBlocks 批量拉齐）

---

## “设计文档 → 代码落点”速查

- **PoW / 难度 / 哈希**：`internal/crypto/pow.go`
- **区块/交易/消息模型**：`internal/types/*.go`
- **主链选择 + 分叉重组**：`internal/blockchain/chain.go`
- **mempool（未确认交易池）**：`internal/mempool/mempool.go`
- **网络（广播 inv + 定向拉取）**：`internal/network/bus.go`
- **节点（inv/get + 挖矿 + initial sync）**：`internal/node/node.go`
- **端到端入口（含 late join flags）**：`cmd/simchain/main.go`

