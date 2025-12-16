# simchain-go

本目录是一个独立的、本地极简 PoW 区块链模拟器（Go 版）。

- 与 talelens 主项目完全解耦。
- 仅用于本地学习/测试。
- 详细设计与实现计划见 DESIGN.md。

## 运行模拟

```powershell
cd simchain-go
go run ./cmd/simchain --nodes=2 --difficulty=16 --duration=30s
```

常用参数：
- `--nodes` 节点数（默认 2）
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
