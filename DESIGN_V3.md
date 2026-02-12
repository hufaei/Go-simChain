# simchain-go V3 统一设计文档

本文档整合 V3 系列迭代（V3-A/B/C），描述从单进程仿真到多进程 TCP 网络的完整演进路径。

---

## 1. 版本演进概览

| 阶段 | 核心目标 | 关键交付 |
|------|----------|----------|
| **V3-A** | 架构拆分 + 主链持久化 | Transport/Syncer/Store 分离；重启恢复 |
| **V3-B** | 多进程 TCP 网络 | TCP 传输；ed25519 身份绑定；seed 发现 |
| **V3-C** | 健壮性 + 可观测性 | Mempool 约束；连接限流；peer 性能指标 |

---

## 2. 系统架构

### 2.1 分层架构图

```
┌─────────────────────────────────────────────────────────────┐
│                      Node (Orchestrator)                    │
│  ┌─────────┐ ┌─────────┐ ┌─────────┐ ┌─────────────────────┐│
│  │  Miner  │ │ Mempool │ │  Chain  │ │       Syncer        ││
│  └────┬────┘ └────┬────┘ └────┬────┘ └──────────┬──────────┘│
│       │          │          │                   │           │
│       └──────────┴──────────┴───────────────────┘           │
│                           │                                 │
│  ┌────────────────────────┴────────────────────────────────┐│
│  │                    PeerManager                          ││
│  │         (peer 状态、评分、选择、冷却)                    ││
│  └────────────────────────┬────────────────────────────────┘│
│                           │                                 │
│  ┌────────────────────────┴────────────────────────────────┐│
│  │                     Transport                           ││
│  │              ┌─────────┬─────────┐                      ││
│  │              │ inproc  │   TCP   │                      ││
│  │              └─────────┴─────────┘                      ││
│  └─────────────────────────────────────────────────────────┘│
│                           │                                 │
│  ┌────────────────────────┴────────────────────────────────┐│
│  │                       Store                             ││
│  │              (主链持久化: blocks + manifest)             ││
│  └─────────────────────────────────────────────────────────┘│
└─────────────────────────────────────────────────────────────┘
```

### 2.2 组件职责

| 组件 | 职责 | 关键接口 |
|------|------|----------|
| **Node** | 编排者：初始化组件、路由消息、触发挖矿 | `StartMining()`, `HandleMessage()` |
| **Miner** | PoW 挖矿循环 | 内嵌于 Node |
| **Mempool** | 未确认交易池（TTL + 容量限制） | `Add()`, `Pop()`, `CleanExpired()` |
| **Chain** | 链索引、cumWork 选 tip、reorg 计算 | `AddBlock()`, `Locator()` |
| **Syncer** | headers-first 同步状态机 | `HandleInvBlock()`, `HandleTx()` |
| **PeerManager** | peer 评分与选择 | `BestPeer()`, `ReportTimeout()` |
| **Transport** | 网络抽象（inproc/TCP） | `Broadcast()`, `Send()`, `Peers()` |
| **Store** | 主链持久化 | `ApplyTipChange()`, `LoadMainChain()` |

---

## 3. 数据流与状态机

### 3.1 消息传播流程

#### 3.1.1 交易传播

```
┌──────────┐    InvTx     ┌──────────┐    GetTx    ┌──────────┐
│  Node A  │ ──────────▶  │  Node B  │ ──────────▶ │  Node A  │
│          │              │          │             │          │
│          │    Tx        │          │◀─────────── │          │
│          │◀──────────── │          │             │          │
└──────────┘              └──────────┘             └──────────┘
```

**状态转换**：
1. Node A 收到交易 → 写入 Mempool → 广播 `InvTx(txid)`
2. Node B 收到 `InvTx` → 检查是否已知 → 发送 `GetTx`
3. Node A 响应完整 `Tx` → Node B 写入 Mempool → 继续 gossip

#### 3.1.2 区块传播

```
┌──────────┐   InvBlock   ┌──────────┐   GetBlock  ┌──────────┐
│  Miner   │ ──────────▶  │  Node B  │ ──────────▶ │  Miner   │
│          │              │          │             │          │
│          │    Block     │          │◀─────────── │          │
│          │◀──────────── │          │             │          │
└──────────┘              └──────────┘             └──────────┘
```

**InvBlock 轻校验**（Syncer 层）：
1. 检查 `meta.Hash == HashHeaderNonce(header, nonce)`
2. 验证 `MeetsDifficulty(hash, difficulty)`
3. 校验 `difficulty == chain.Difficulty()`
4. 通过后才发起 `GetBlock` 请求

### 3.2 Syncer 状态机

```
                    ┌───────────────┐
                    │     Idle      │◀────────────────────┐
                    └───────┬───────┘                     │
                            │ behind best peer            │ caught up
                            ▼                             │
                    ┌───────────────┐                     │
            ┌──────▶│SyncingHeaders │─────────────────────┤
            │       └───────┬───────┘                     │
            │               │ got headers                 │
            │               ▼                             │
            │       ┌───────────────┐                     │
            │       │SyncingBlocks  │─────────────────────┘
            │       └───────┬───────┘
            │               │ timeout/error
            │               ▼
            │       ┌───────────────┐
            └───────│   Backoff     │
                    └───────────────┘
                            │ cooldown expired
                            ▼
                    (return to Idle)
```

**状态说明**：
- **Idle**：本地已接近 best peer tip；监听 `InvBlock`，定期 probe tips
- **SyncingHeaders**：发送 `GetHeaders(locator)` 获取缺失区块元数据
- **SyncingBlocks**：维护下载窗口，批量拉取区块
- **Backoff**：同步失败后冷却，避免反复请求同一 peer

### 3.3 窗口式区块下载

```
┌─────────────────────────────────────────────────────────┐
│                    Inflight Window                      │
│  ┌────┐ ┌────┐ ┌────┐ ┌────┐ ... ┌────┐                │
│  │Blk1│ │Blk2│ │Blk3│ │Blk4│     │BlkN│                │
│  └──┬─┘ └──┬─┘ └──┬─┘ └──┬─┘     └──┬─┘                │
│     │      │      │      │          │                   │
│   peer   peer   peer   peer      peer                   │
│   + deadline + attempts                                 │
└─────────────────────────────────────────────────────────┘
         │
         ▼ timeout
    ┌────────────┐
    │  Retry or  │
    │  Switch    │
    │   Peer     │
    └────────────┘
```

**配置参数**：
| 参数 | 默认值 | 说明 |
|------|--------|------|
| `MaxInflightBlocks` | 16 | 同时在途的请求数上限 |
| `BlockRetryLimit` | 3 | 单块最大重试次数 |
| `HeaderRetryLimit` | 2 | GetHeaders 最大重试次数 |
| `SyncTimeout` | 2s | 单次请求超时 |
| `BackoffDuration` | 1.2s | 失败后冷却时间 |

---

## 4. TCP 网络层（V3-B/C）

### 4.1 连接生命周期

```
┌─────────┐                              ┌─────────┐
│ Node A  │                              │ Node B  │
└────┬────┘                              └────┬────┘
     │                                        │
     │  ────────── TCP Connect ─────────▶    │
     │                                        │
     │  ◀─────────── Accept ────────────     │
     │                                        │
     │  ──── Hello{pubkey,nonce,addr} ───▶   │
     │                                        │
     │  ◀── HelloAck{nonce,echoNonce} ────   │
     │                                        │
     │  ──── Auth{sig(nonce+magic)} ─────▶   │
     │                                        │
     │        [Handshake Complete]            │
     │                                        │
     │  ◀────── GetPeers ─────────────────   │
     │  ──────── Peers{addrs} ────────────▶  │
     │                                        │
     │        [Normal Message Flow]           │
     │                                        │
```

### 4.2 消息帧格式

```
┌────────────────┬─────────────────────────────────┐
│  Length (4B)   │         JSON Payload            │
│  Big-Endian    │    (types.Message 序列化)        │
└────────────────┴─────────────────────────────────┘
```

**约束**：
- `MaxMessageSize`: 2MB
- `Length == 0` 或 `Length > MaxMessageSize`: 断开连接

### 4.3 连接管理策略

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `MaxPeers` | 8 | 最大连接总数 |
| `OutboundPeers` | 4 | 目标出站连接数 |
| `MaxConnsPerIP` | 2 | 每 IP 最大并发连接 |
| `ReadTimeout` | 4s | 单次读超时 |
| `WriteTimeout` | 4s | 单次写超时 |
| `IdleTimeout` | 20s | 空闲断开时间 |

### 4.4 速率限制（V3-C）

```
┌─────────────────────────────────────────┐
│           Per-Peer Rate Limiter         │
│                                         │
│   RateLimit:  1 MB/s (bytes/second)     │
│   RateBurst:  2 MB   (burst capacity)   │
│                                         │
│   Implementation: golang.org/x/time/rate│
└─────────────────────────────────────────┘
```

超过限制时：`WaitN` 阻塞直到配额可用，或超时断开。

---

## 5. Mempool 约束（V3-C）

### 5.1 配置参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `MaxTxs` | 5000 | 最大交易数 |
| `MaxBytes` | 10 MB | 最大字节数 |
| `TTL` | 10 min | 交易过期时间 |
| `InvBroadcastCooldown` | 30s | 重复广播冷却 |

### 5.2 容量管理策略

```
┌─────────────────────────────────────────────────────┐
│                   Mempool FCFS                      │
│                                                     │
│  Add(tx):                                           │
│    ├─ seen[tx.ID]? → reject (duplicate)            │
│    ├─ len(txs) >= MaxTxs? → reject (full)          │
│    ├─ totalBytes + txSize > MaxBytes? → reject     │
│    └─ accept → append to queue                     │
│                                                     │
│  CleanExpired() [every 30s]:                        │
│    └─ remove txs where now > expiresAt             │
└─────────────────────────────────────────────────────┘
```

### 5.3 广播去重

```go
// 避免同一交易短时间内重复广播
if time.Since(lastInvBroadcast[txid]) < InvBroadcastCooldown {
    return // skip broadcast
}
```

---

## 6. Peer 性能指标（V3-C）

### 6.1 指标采集

```go
type peerMetrics struct {
    totalRequests       int           // 总请求数
    successCount        int           // 成功次数
    failureCount        int           // 失败次数
    avgRTT              time.Duration // 平均往返时间
    consecutiveFailures int           // 连续失败次数
    lastTimeout         time.Time     // 最近超时时间
}
```

### 6.2 Peer 选择算法

```
Best Peer Selection:
  1. Filter out backed-off peers (consecutiveFailures > 3)
  2. Sort by:
     a. Success rate (descending)
     b. Average RTT (ascending)
  3. Return top candidate
```

### 6.3 Backoff 策略

- **触发条件**：连续失败 > 3 次，或最近 30s 内有超时
- **效果**：临时排除该 peer 作为同步源
- **恢复**：成功响应后清除 `consecutiveFailures` 和 `lastTimeout`

---

## 7. 主链持久化

### 7.1 文件布局

```
data/<nodeID>/
├── manifest.json        # 链状态元数据
├── node_key.json        # ed25519 密钥（TCP 模式）
└── blocks/
    ├── 0000000000000000.json  # genesis
    ├── 0000000000000001.json
    ├── 0000000000000002.json
    └── ...
```

### 7.2 Manifest 结构

```json
{
  "tipHeight": 1234,
  "tipHash": "000000abcd...",
  "difficulty": 16,
  "updatedAt": 1700000000
}
```

### 7.3 写入触发点

```
TipChangeEvent
      │
      ▼
  ┌──────────────────────────────────────┐
  │            Store.ApplyTipChange      │
  │                                      │
  │  if reorg == nil:                    │
  │      AppendBlock(newTip)             │
  │  else:                               │
  │      RollbackTo(commonAncestor)      │
  │      for each added block:           │
  │          AppendBlock(block)          │
  └──────────────────────────────────────┘
```

---

## 8. 运行模式

### 8.1 Inproc 模式（单进程多节点）

```bash
# 启动 2 节点仿真，运行 30 秒
./simchain-node.exe -nodes 2 -difficulty 16 -duration 30s
```

**用途**：快速测试、算法验证、调试

### 8.2 TCP 模式（多进程）

**节点 1（Seed）**：
```bash
./simchain-node.exe -transport tcp -listen 127.0.0.1:50001 \
    -nodes 1 -miner-sleep 2s -duration 0
```

**节点 2**：
```bash
./simchain-node.exe -transport tcp -listen 127.0.0.1:50002 \
    -seeds 127.0.0.1:50001 -nodes 1 -miner-sleep 2s -duration 0
```

**节点 3**：
```bash
./simchain-node.exe -transport tcp -listen 127.0.0.1:50003 \
    -seeds 127.0.0.1:50001 -nodes 1 -miner-sleep 2s -duration 0
```

### 8.3 CLI 客户端

```bash
# 提交交易到节点
./simchain-cli.exe -connect 127.0.0.1:50001 -cmd submit -data "hello world"
```

---

## 9. 构建与验证指南

### 9.1 构建命令

```bash
# 构建节点
go build -o simchain-node.exe ./cmd/node

# 构建客户端
go build -o simchain-cli.exe ./cmd/cli
```

### 9.2 验证脚本

#### PowerShell 脚本 (testPool.ps1)

验证 Mempool 容量限制与交易提交功能。

**前置条件**：
```powershell
# 清空 data 目录（推荐）
Remove-Item -Recurse -Force data\* 2>$null

# 启动节点
./simchain-node.exe -transport tcp -listen 127.0.0.1:50001 -nodes 1 -miner-sleep 2s -duration 0
```

**运行**：
```powershell
./testPool.ps1
```

**预期结果**：
- 脚本发送 5500 笔交易
- 节点日志 `STATE_DUMP` 显示 `mempool size` 增长至 5000 后稳定
- 超出容量的交易被拒绝（FCFS drop-tail）

#### Python 脚本 (verify_v3c.py)

验证 TCP 连接限制与抗攻击能力。

**连接数限制测试**：
```bash
python verify_v3c.py tcp-limit --port 50001 --count 10
```

预期：仅 `MaxConnsPerIP`（默认 2）个连接成功维持。

**慢速攻击测试 (Slowloris)**：
```bash
python verify_v3c.py slowloris --port 50001
```

预期：节点在 `ReadTimeout`（默认 4s）后主动断开部分数据连接。

### 9.3 集成测试

```bash
# 运行所有测试
go test ./...

# 运行 TCP 集成测试
go test ./internal/integration/... -v

# 运行 Mempool 测试
go test ./internal/mempool/... -v
```

---

## 10. 可观测性

### 10.1 日志事件

| 事件 | 格式 | 说明 |
|------|------|------|
| `BLOCK_MINED` | `node=X h=N hash=Y nonce=Z txs=M` | 本地挖出新块 |
| `BLOCK_RECEIVED` | `node=X from=Y h=N hash=Z` | 收到网络区块 |
| `TX_ACCEPTED` | `node=X tx=Y` | 交易写入 mempool |
| `CHAIN_SWITCH` | `node=X oldTip=Y newTip=Z forkDepth=N` | 发生 reorg |
| `TCP_PEER_CONNECTED` | `dir=in/out peer=X addr=Y` | TCP 连接建立 |
| `STATE_DUMP` | (见下文) | tip 变化时全局状态快照 |

### 10.2 STATE_DUMP 格式

```
STATE_DUMP
trigger node=node-1 from=node-1 height=42 tip=000000abc123 reorg=false
  node=node-1 tipHeight=42 tip=000000abc123
  mempool size=15 txids=[tx-1, tx-2, ...]
  mainchain:
    - h=42 hash=000000abc123 txs=3 txids=[tx-a, tx-b, tx-c]
    - h=41 hash=000000def456 txs=2 txids=[tx-d, tx-e]
    ...
```

### 10.3 Peer 指标输出

```
Peer Metrics:
  peer-abc: requests=100, success=95 (95.0%), fail=5, avgRTT=50ms, consecutiveFail=0
  peer-def: requests=80, success=70 (87.5%), fail=10, avgRTT=120ms, consecutiveFail=2
```

---

## 11. 设计约束与非目标

### 11.1 当前约束

- **仅支持 Loopback**：TCP 模式仅允许 `127.0.0.1` / `localhost`
- **无加密传输**：不使用 TLS/WSS（本机回环无需）
- **简化交易**：无签名验证、UTXO、费用计算
- **固定难度**：不支持动态难度调整

### 11.2 后续迭代方向（V4+）

| 方向 | 说明 |
|------|------|
| 真实交易验证 | UTXO 模型、数字签名、脚本 |
| 费用市场 | 交易打包优先级、费率估算 |
| 公网部署 | NAT 穿透、TLS 加密、DNS seed |
| 结构化指标 | Prometheus/OpenTelemetry 集成 |

---

## 12. 附录：协议消息类型

| 消息类型 | 用途 | 方向 |
|----------|------|------|
| `InvTx` | 交易公告 | broadcast |
| `GetTx` | 请求完整交易 | request |
| `Tx` | 返回完整交易 | response |
| `InvBlock` | 区块公告（仅 meta） | broadcast |
| `GetBlock` | 请求单个区块 | request |
| `Block` | 返回单个区块 | response |
| `GetTip` | 查询对端 tip | request |
| `Tip` | 返回 tip 信息 | response |
| `GetHeaders` | 请求区块头列表 | request |
| `Headers` | 返回区块头列表 | response |
| `GetBlocks` | 批量请求区块 | request |
| `Blocks` | 批量返回区块 | response |
| `GetPeers` | 请求 peer 地址列表 | request |
| `Peers` | 返回地址列表 | response |
| `Hello` | 握手第一步 | handshake |
| `HelloAck` | 握手第二步 | handshake |
| `Auth` | 握手第三步（签名验证） | handshake |
