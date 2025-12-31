# simchain-go

本目录是一个独立的、本地极简 PoW 区块链模拟器（Go 版）。

- 该项目除了设计思路外大部分基于AI-辅助生成，仅供学习和测试使用。
- 仅用于本地学习/测试。
- 详细设计与实现计划见 DESIGN(/_*).md。

## 运行模拟

```powershell
cd simchain-go
go run ./cmd/simchain --nodes=2 --difficulty=16 --duration=30s
```

## TCP 多进程模式（V3-B）

在不同终端启动多个进程，每个进程 1 个节点，通过 TCP 在本机回环互联（仅支持 `127.0.0.1/localhost`）。

终端 1（seed/普通节点）：
```powershell
go run ./cmd/simchain --transport=tcp --listen=127.0.0.1:7000 --duration=0 --tx-interval=0
```

终端 2：
```powershell
go run ./cmd/simchain --transport=tcp --listen=127.0.0.1:7001 --seeds=127.0.0.1:7000 --duration=0
```

终端 3：
```powershell
go run ./cmd/simchain --transport=tcp --listen=127.0.0.1:7002 --seeds=127.0.0.1:7000 --duration=0
```

说明：
- `--duration=0` 表示一直运行到 Ctrl+C。
- 节点会在 `--data-dir`（默认 `data/tcp-<port>`）下生成密钥并落盘主链数据。

也可以用脚本一键跑一个短 demo（会把二进制与日志写到 `data/tcp-demo/<timestamp>/...`）：
```powershell
.\scripts\run-tcp-demo.ps1 -BasePort 7000 -Nodes 3 -DurationSec 20 -Difficulty 16 -TxInterval "1s"
```

常用参数：
- `--nodes` 节点数（默认 2）
- `--transport` 传输方式：`inproc|tcp`（默认 `inproc`）
- `--listen` TCP 监听地址（tcp 模式必填）
- `--seeds` TCP seed 列表（逗号分隔，tcp 模式可选）
- `--data-dir` 数据目录（tcp 模式可选）
- `--difficulty` 难度（前导零 bit 数，默认 16）
- `--duration` 运行时长（默认 30s）
- `--tx-interval` 注入交易间隔（默认 1s）
- `--max-tx-per-block` 每块最大交易数（默认 50）
- `--miner-sleep` 每轮挖矿后休眠（默认 10ms）
- `--seed` 随机种子，便于复现实验

## 运行测试

```powershell
go test ./...
```

也可以只跑 V3-B 的 TCP 集成测试：
```powershell
go test ./internal/integration -run TestTCPE2E -count=1
```
