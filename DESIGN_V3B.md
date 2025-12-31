# simchain-go V3-B 迭代版本文件：多进程 TCP 网络 + seed 发现 + 最小身份绑定

本文件定义下一次迭代（V3-B）的**目标、范围、接口约束、验收标准与实施里程碑**，用于在当前 V3-A（单进程 inproc Transport/Syncer/Store）基础上，将模拟器演进为**同机多进程**可运行的节点网络。

V3-B 的原则：**先跑通跨进程的网络模型**，并在此基础上加“最低限度”的健壮性与身份绑定；不追求生产级安全性与公网可用性。

---

## 0. 背景与前置（当前 V3-A 的关键点）

当前架构已经拆分出：
- `Transport`：投递抽象（当前实现为 inproc 适配 `NetworkBus`）
- `Syncer`：headers-first + window + retry + peer backoff（后台追链状态机）
- `PeerManager`：best peer 选择与冷却
- `Store`：只持久化主链（blocks by height + manifest）

当前系统的一项重要假设是：消息在进程内以 Go struct 传递，`types.Message.Payload` 使用 `any` 并通过类型断言消费。

V3-B 的第一性问题：**跨进程传输后，Payload 不能再依赖类型断言**。因此 V3-B 必须先定义可编码（可序列化）的消息层。

---

## 1. V3-B 目标与非目标

### 1.1 目标（必须）
- **多进程运行（同机）**：不同终端启动多个 `simchain` 进程，彼此通过 TCP 互联，并能完成交易传播、出块、分叉收敛、late join 同步。
- **Transport 可替换**：新增 `transport/tcp`，尽量不改 `Node/Syncer/PeerManager/Store` 的核心逻辑。
- **seed 自动发现（本机）**：节点可通过 seed 获取 peer 列表并建立连接，不需要手工全量配置 `--peers`。
- **最小身份绑定**：节点身份与密钥绑定，握手阶段做一次挑战-响应签名验证（防止“随便冒充 nodeID”）。
- **基本健壮性**：处理 TCP 粘包/拆包、消息大小上限、读写超时、连接/速率上限（防止本机误用或简单 DoS）。

### 1.2 非目标（V3-B 暂不做）
- 公网/NAT 穿透与跨机部署。
- TLS/WSS、证书链、加密传输（本机回环无需）。
- 生产级抗攻击（Sybil、Eclipse、资源耗尽、协议模糊测试、持久化 mempool 等）。
- 复杂交易校验（UTXO/签名/费用/脚本等）。

---

## 2. 运行模型（多进程）

### 2.1 进程与节点
- 每个 OS 进程运行 1 个 Node（一个 `--listen` 地址）。
- 同一台机器上启动多个终端：每个终端一个 `simchain`。

### 2.2 CLI 形态（建议）
新增 TCP 模式参数（保留现有 inproc 多节点模式不动）：
- `--transport=tcp|inproc`（默认 `inproc`，兼容现有用法）
- `--id=<nodeID>`（TCP 模式可选：默认由 key 推导；若显式提供但与推导值不一致则报错）
- `--listen=127.0.0.1:7001`（TCP 模式必填）
- `--seeds=127.0.0.1:7000,127.0.0.1:7002`（可选）
- `--data-dir=data/<nodeID>`（默认按推导 nodeID；显式提供则优先使用该目录加载/保存 key 与链数据）
- `--max-peers=8`、`--outbound-peers=4`（连接策略）

兼容性：`--nodes=N` 的单进程仿真仍可用于快速回归与测试。

---

## 3. 最小身份绑定（ed25519）

### 3.1 节点密钥
- 每个节点在 `data/<nodeID>/` 下保存 `node_key.json`（或二进制文件）：
  - `ed25519.PrivateKey`（或 seed）
  - `ed25519.PublicKey`
- `nodeID` 由 `pubkey` 推导：
  - `nodeID = hex(sha256(pubkey))[:40]`（示例，固定长度便于日志/索引）

### 3.2 握手协议（最小）
目的：对端证明“我持有该 pubkey 的私钥”，并告知 `listenAddr` 用于发现。

建议三步（每条消息都走统一 framing + message 编码）：
1) `Hello{magic, version, pubkey, listenAddr, nonceA, timestamp}`
2) `HelloAck{nonceB, echoNonceA}`
3) `Auth{sig = Sign(priv, nonceB || magic || version)}`

验证：
- `magic/version` 不一致：断开
- `nodeID(pubkey)` 与连接表冲突：断开（同一 nodeID 只保留一个连接）
- `sig` 验证失败：断开

实现注意：
- 握手完成前，对端声称的 `From/nodeID` 都是**不可信**的；以 `pubkey -> nodeID` 的推导结果为准，并在验证 `Auth` 通过后才把连接纳入 `Peers()` 与路由表。

注：V3-B 只做身份绑定与防误连，不做加密与强认证链路。

---

## 4. 消息层：可编码 Message（跨进程必要改造）

### 4.1 问题
当前 `types.Message.Payload any` 依赖 inproc 的类型信息；跨进程 JSON 编解码后会变成 `map[string]any`，类型断言会失效。

### 4.2 方案（建议）
在 `internal/types/message.go` 中实现：
- `Message.MarshalJSON`：写入 `type/from/to/timestamp/traceId/payload`
- `Message.UnmarshalJSON`：先解析 `type`，再把 `payload` 解码到对应的 payload struct

Payload 映射（示例）：
- `MsgInvTx` -> `InvTxPayload`
- `MsgGetTx` -> `GetTxPayload`
- `MsgTx` -> `TxPayload`
- `MsgInvBlock` -> `InvBlockPayload`
- `MsgGetBlock` -> `GetBlockPayload`
- `MsgBlock` -> `BlockPayload`
- `MsgGetTip` -> `GetTipPayload`
- `MsgTip` -> `TipPayload`
- `MsgGetHeaders` -> `GetHeadersPayload`
- `MsgHeaders` -> `HeadersPayload`
- `MsgGetBlocks` -> `GetBlocksPayload`
- `MsgBlocks` -> `BlocksPayload`
- （新增）`MsgGetPeers/MsgPeers` -> `GetPeersPayload/PeersPayload`
- （新增）`MsgHello/MsgHelloAck/MsgAuth` -> `HelloPayload/...`

约束：
- 所有消息必须可 `json.Marshal`/`json.Unmarshal` 且字段稳定。
- 增加 `MaxMessageBytes`（例如 2MB）用于 TCP framing 限制。
- 若不希望污染“进程内 message”的使用体验，可引入一个仅用于网络传输的 `WireMessage{Type, From, To, Timestamp, TraceID, Payload(json.RawMessage)}`，解码后再转换为当前的 `types.Message`（两者职责更清晰）。

---

## 5. TCP Transport：framing、连接管理与广播

### 5.1 Framing（粘包/拆包处理）
使用长度前缀：
- `uint32` big-endian 长度 `L`
- 后随 `L` 字节 JSON payload（一个 `types.Message`）

读取流程：
1) 读满 4 字节长度
2) 若 `L == 0` 或 `L > MaxMessageBytes`：断开
3) 读满 `L` 字节
4) `json.Unmarshal` 成 `types.Message`

### 5.2 连接管理（本机）
- 监听地址默认 `127.0.0.1`，拒绝非 loopback 连接（V3-B 范围内）。
- 每个 peer 建立一个 `Conn`，包含读循环与**单写协程**（通过有界写队列串行写入，避免多 goroutine 并发写同一 conn）。
- 必须设置 `read/write deadline` 与 `idle timeout`，超时断开并进入 backoff。
- 限制：
  - `maxConns`（总连接上限）
  - `maxInbound`（入站连接上限）
  - `maxOutbound`（出站连接上限）

### 5.3 `Transport` 语义映射
- `Register(nodeID, handler)`：本地节点注册处理函数
- `Peers()`：返回已握手成功的 peer nodeID 列表
- `Send(to, msg)`：找到对应 peer conn 写一条消息
- `Broadcast(from, msg)`：遍历 peers 并发发送（可做轻量 rate limit）

---

## 6. seed 自动发现（本机）

### 6.1 发现协议
新增消息：
- `GetPeers{}`：请求 peer 地址列表
- `Peers{addrs: []string}`：返回若干 `ip:port`（本机下基本是 `127.0.0.1:port`）

### 6.2 seed 行为（推荐：seed 也是普通节点）
- seed 参与正常挖矿/同步，避免特殊分支。
- seed 额外维护一个地址表 `addr -> lastSeen`：
  - 来源：握手时对端宣告的 `listenAddr`
  - 更新：收到对端任何有效消息即可刷新 `lastSeen`
- `Peers` 返回：按 `lastSeen` 新鲜度排序/随机抽样，过滤自己与明显失效项。

### 6.3 节点启动流程（建议）
1) 启动 TCP listener
2) 连接 seeds（有则优先，没有则只接受入站）
3) 对每个 seed：握手成功后 `GetPeers`
4) 从返回列表中随机挑选若干地址拨号，直到达到 `outboundPeers`
5) 后台周期性向 seeds/peers 重新 `GetPeers`（低频），用于补足连接与容错

---

## 7. 验收清单（必须通过）

在本机上启动 3 个进程（其中 1 个为 seed），满足：
- **交易传播**：任意一个节点注入交易，其他节点最终通过 `InvTx/GetTx/Tx` 接收并进入 mempool。
- **出块与广播**：任意节点挖到块后，通过 `InvBlock/GetBlock/Block` 传播，其他节点最终接入链。
- **late join**：第三个节点晚加入（启动晚/重启），能够通过 `GetTip/GetHeaders/GetBlocks` 追上最新 tip。
- **reorg**：制造短暂分叉（通过不同节点同时挖矿、或人为 drop/delay），最终收敛到累积工作量更大的主链；`Store` 只保留新主链段。
- **重启恢复**：kill 一个节点再启动，能从 `data/<nodeID>` 恢复 tip 并继续同步。
- **身份绑定**：任意连接必须通过 ed25519 challenge-response；伪造 nodeID/错误签名必须被拒绝。

---

## 8. 实施里程碑（建议顺序）

1) **Message 编码改造**：`types.Message` 支持按 `Type` 解码 payload（并补齐新增 discovery/handshake 消息类型）
2) **Key/NodeID**：生成/加载 ed25519 key；nodeID 从 pubkey 推导；落盘到 dataDir
3) **TCP framing**：实现 length-prefix 读写；最大消息大小 + deadline
4) **TCP transport**：连接表、peer->conn、Send/Broadcast、Register/handler 回调
5) **handshake**：Hello/HelloAck/Auth；拒绝非 loopback；拒绝重复 nodeID
6) **seed discovery**：GetPeers/Peers；seed 地址表；连接补齐逻辑
7) **cmd/simchain tcp 模式**：单节点进程 CLI（inproc 模式保持不变）
8) **回归与验收脚本（可选）**：提供 powershell 示例命令，方便三终端复现验收场景

---

## 9. 兼容性与迁移说明

- V3-B 不要求移除 `inproc`，它仍是快速测试/调试模式。
- `types.Message` 的编码改动将影响 inproc（但应保持行为一致）；建议优先保证 `go test ./...` 通过。
