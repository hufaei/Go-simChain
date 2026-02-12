# simchain-go V3-C 里程碑文档：TCP 健壮性 + 同步收敛 + 可观测性（对标 Bitcoin 节点行为）

本文件定义 V3-C 的**目标、范围、接口约束、验收标准与实施里程碑**，用于在当前 V3-B（同机多进程 TCP 网络 + seed 发现 + 最小身份绑定）基础上，使系统在"非理想网络条件下"依旧稳定同步、可观测、可诊断。

V3-C 的原则：**优先补齐"真实节点必备行为"**，对标 Bitcoin 的工程实践（headers-first 同步、inflight 控制、peer 冷却与观测），但保持模拟器的轻量与易理解。

---

## 0. 背景与前置（当前 V3-B 的关键能力）

当前系统已经具备：
- TCP 多进程网络（本机回环）、seed 发现、最小身份绑定
- headers-first 同步协议 + inv/get 传播模型
- 主链持久化（仅主链，不持久化分叉/交易池）
- 基础可观测输出（STATE_DUMP / SIMULATION_SUMMARY）

V3-C 的核心问题：
- 在 TCP 条件下，**同步过程仍偏"理想网络"**，需要更严格的超时/重试/peer 选择策略
- mempool/消息广播缺少现实约束（TTL、去重、限速）
- 缺少针对 peer 行为与同步耗时的**可观测指标**

---

## 0.5 当前 V3-B 能力盘点

### Syncer 现状
- ✅ **已实现**：
  - headers-first 同步协议
  - window-based 块下载
  - 基本的 retry 机制
  - PeerManager 的 best peer 选择
- ❌ **缺失**：
  - inflight 请求的 deadline tracking
  - peer 性能指标记录（RTT、成功率）
  - 基于性能的动态 peer 切换策略
  - 超时/重试次数的详细统计

### TCP Transport 现状  
- ✅ **已实现**：
  - 连接数限制 (maxPeers/outbound)
  - 基本的读写超时控制
  - 消息大小限制 (MaxMessageSize)
  - Loopback 限制
- ❌ **缺失**：
  - per-peer 消息速率限制
  - 细粒度的异常 peer 处理策略
  - 连接质量指标收集

### Mempool 现状
- ✅ **已实现**：
  - 基本的交易池存储
  - InvTx/GetTx/Tx 消息传播
- ❌ **缺失**：
  - 交易 TTL 清理机制
  - 容量上限控制
  - 广播去重逻辑
  - 交易优先级管理

### 可观测性现状
- ✅ **已实现**：
  - 基础日志输出（连接、握手、同步事件）
  - STATE_DUMP 和 SIMULATION_SUMMARY
- ❌ **缺失**：
  - 结构化的 peer 性能指标
  - 同步耗时追踪
  - inflight 状态可视化
  - 超时/重试统计

---

## 1. V3-C 目标与非目标

### 1.1 目标（必须）
- **同步健壮性**：完善 Syncer 状态机（inflight 窗口、超时重试、peer 冷却/切换），在高延迟/丢包时仍能收敛。
- **mempool 现实约束**：引入 TTL、容量上限、去重控制，避免无限增长与广播风暴。
- **TCP 行为改良**：限速/限流、连接上限与异常 peer 处理策略的落地。
- **可观测性增强**：输出同步耗时、peer RTT、inflight 数量、超时/重试次数。

### 1.2 非目标（V3-C 暂不做）
- 真实交易验证（签名/UTXO/脚本）。
- 交易费用市场与打包策略（留到 V4）。
- 公网部署、跨机 NAT 穿透、加密传输。
- 结构化指标系统（如 Prometheus）—— V3-C 阶段使用日志输出即可

---

## 1.5 实施接口说明

### 需要修改的组件

#### 1. `internal/node/syncer.go`
- **新增**：`inflightTracker` 结构，记录每个 inflight 请求的：
  - 请求时间
  - deadline
  - 请求的 peer ID
  - 请求的块/header 范围
- **新增**：`peerMetrics` map，记录每个 peer 的：
  - 平均 RTT
  - 成功/失败次数
  - 最近超时时间（用于 backoff）
- **增强**：retry 逻辑，在超时时切换到其他 peer

#### 2. `internal/node/mempool.go`
- **新增字段**：
  - `txExpiry map[TxID]time.Time` - 记录交易过期时间
  - `maxTxs int` - 最大交易数配置
  - `maxBytes int` - 最大字节数配置（可选）
  - `seenInvs map[TxID]time.Time` - 记录已广播的 InvTx，避免重复
- **新增方法**：
  - `cleanExpired()` - 定期清理过期交易
  - `checkCapacity()` - 容量检查

#### 3. `internal/transport/tcp/tcp.go`
- **新增**：per-peer rate limiter
  - 简单的 token bucket 实现
  - 配置参数：`MaxMsgsPerSec` per peer
- **增强**：异常检测逻辑
  - 记录每个 peer 的无效消息计数
  - 达到阈值后断开连接并 backoff

### 新增组件（可选）

#### `internal/node/syncer_metrics.go`
专门用于 Syncer 指标收集和输出的辅助模块，包括：
- `recordPeerRTT(peerID, duration)`
- `recordInflight(count)`
- `recordTimeout(peerID)`
- `recordRetry(peerID)`
- `dumpMetrics()` - 定期输出到日志

---

## 2. 迭代方向（对齐 Bitcoin 行为的关键点）

### 2.1 同步状态机强化（Syncer）
- **headers-first** 仍为主，但要保证：
  - inflight 窗口大小可配置（例如 32）
  - 每个 inflight 记录 deadline
  - 超时触发 retry / peer 冷却
- **peer 选择/切换策略**：
  - 基于 cumWork + RTT + 失败率选择 best peer
  - 对连续超时的 peer 做冷却（backoff）
- **阻断重复下载**：维护 knownBlocks / inflightBlocks，防止重复拉取

### 2.2 mempool 现实约束
- **TTL 清理**：过期交易自动移除（建议 TTL = 10分钟）
- **容量上限**：例如 maxTxs = 5000 或 maxBytes = 10MB
- **广播去重/限速**：
  - 记录已发送的 InvTx，避免同一 tx 短时间内重复广播
  - 限制广播频率（如每个 peer 每秒不超过 100 条 InvTx）

### 2.3 TCP 连接与消息控制
- **连接上限**：已实现 max inbound/outbound/total（V3-B）
- **速率限制**：对单 peer 的消息频率做基本限制
  - 建议：每个 peer 每秒不超过 200 条消息
  - 超限时暂停读取或断开连接
- **异常 peer 处理**：
  - 无效消息/过大消息累计超过阈值 → 断开
  - 连续超时（如 3 次）→ 断开并 backoff

### 2.4 可观测性与诊断
- **增加关键指标输出**（通过日志）：
  - peer RTT（均值/近期）
  - inflight 数量
  - 超时与重试次数
  - 同步耗时（从落后到追平）
- **扩展 STATE_DUMP**：
  - 增量输出 peer 状态（连接时长、RTT、成功率）
  - 增量输出同步状态（当前 tip、inflight 详情、backoff peers）

---

## 3. 里程碑（建议顺序）

### Milestone 1: Syncer 状态机补齐
- **目标**：在现有 `internal/node/syncer.go` 基础上增强
- **任务**：
  - 实现 `inflightTracker`：记录所有 inflight 请求及其 deadline
  - 实现 `peerMetrics`：收集 peer RTT 和成功/失败统计
  - 增强 retry 逻辑：超时时切换到其他 peer
  - 实现 peer 冷却/切换策略
- **验收**：
  - 手动模拟高延迟（sleep）场景，验证超时重试
  - 验证 peer 切换逻辑正常工作

### Milestone 2: mempool 约束落地
- **目标**：在现有 `internal/node/mempool.go` 基础上增强
- **任务**：
  - 增加 TTL 字段和清理逻辑
  - 增加 size limit 检查
  - 实现广播去重逻辑
- **验收**：
  - 验证过期交易自动清理
  - 验证达到容量上限后拒绝新交易
  - 验证不会重复广播同一 tx

### Milestone 3: TCP 连接与限流
- **目标**：在现有 `internal/transport/tcp/tcp.go` 基础上增强
- **任务**：
  - 实现 per-peer rate limiter
  - 增强异常 peer 断开逻辑
- **验收**：
  - 验证超速 peer 被限流或断开
  - 验证无效消息过多时断开连接

### Milestone 4: 可观测性增强
- **目标**：在现有日志基础上增加结构化输出
- **任务**：
  - 在 Syncer 中输出 RTT、超时、重试统计
  - 扩展 STATE_DUMP 输出 peer 状态与同步详情
  - （可选）实现 `syncer_metrics.go` 辅助模块
- **验收**：
  - 日志中可观测到 peer RTT、超时、重试与 inflight 数量
  - STATE_DUMP 包含详细的 peer 和同步状态

### Milestone 5: 回归与验收脚本
- **目标**：增强现有 `internal/integration/tcp_test.go`
- **任务**：
  - 增加高延迟/丢包场景测试
  - 验证 late join 同步在恶劣条件下仍能收敛
- **验收**：
  - 所有 V3-C 验收清单项通过测试

---

## 4. 验收清单（必须通过）

在本机 3~5 个进程 + TCP 模式下满足：
- **高延迟/轻度丢包**场景下仍能完成 late join 同步
- **重复/失效消息**不会导致死锁或无限重试
- mempool **不会无限膨胀**（TTL/容量生效）
- 日志中可观测到 **peer RTT、超时、重试与 inflight 数量**
- 短暂分叉仍能收敛到累积工作量更大的主链

---

## 5. 测试策略

### 单元测试
- `internal/node/syncer_test.go`：增加 inflightTracker 和 peerMetrics 单元测试
- `internal/node/mempool_test.go`：增加 TTL 清理和容量限制测试
- `internal/transport/tcp/rate_limit_test.go`（新增）：rate limiter 单元测试

### 集成测试  
- 扩展 `internal/integration/tcp_test.go`：
  - 高延迟场景（使用 `time.Sleep` 模拟）
  - peer 断开/重连场景
  - mempool 容量测试
- 保持与现有 `TestTCPE2E` 的兼容性

### 手动测试
- 使用 `scripts/run-tcp-demo.sh` 验证多进程场景
- 通过日志观察 peer RTT、inflight、超时等指标

---

## 6. 自审（Self Review）

- [x] 里程碑目标清晰且可执行（避免"概念性"描述）
- [x] 明确区分"必须项"与"后续项"
- [x] 文档结构与现有 V3-B 文档风格一致
- [x] 验收清单可在本机 TCP 模式下验证
- [x] 不涉及 V4 范围（交易验证/费用市场）
- [x] **新增**：明确了需要修改的组件和实施接口
- [x] **新增**：盘点了 V3-B 当前能力，明确缺失项
- [x] **新增**：补充了测试策略说明
