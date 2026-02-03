# simchain-go TCP 协议约定（V3-B）

本文件聚焦 **TCP 传输层的消息编码约定**，确保跨进程通信保持一致，并补充必要的字段解释与约束。

## 编码外壳（Frame）

每条消息使用长度前缀 framing：

- `uint32` big-endian 长度 `L`
- 后随 `L` 字节 JSON payload

传输层会拒绝 `L <= 0` 或 `L > MaxMessageSize` 的消息（避免内存滥用）。

## Message JSON 结构

`internal/types.Message` 的 JSON 形态如下：

```json
{
  "type": "GetTip",
  "from": "<nodeID>",
  "to": "<nodeID>",
  "timestamp": 1710000000000,
  "traceId": "<uuid/opaque>",
  "payload": { "...": "..." }
}
```

字段说明：
- `type`：消息类型，决定 payload 解码结构。
- `from`：发送方 nodeID（握手后可信）。
- `to`：可选，点对点消息的目标。
- `timestamp`：发送时的 unix ms。
- `traceId`：可选，用于请求-响应的轻量关联。
- `payload`：各类消息的具体负载（见下表）。

## Payload 映射

传输层 JSON 解码后，会根据 `type` 将 `payload` 解码到对应结构体：

- `Hello` -> `HelloPayload`
- `HelloAck` -> `HelloAckPayload`
- `Auth` -> `AuthPayload`
- `GetPeers` -> `GetPeersPayload`
- `Peers` -> `PeersPayload`
- `InvTx` -> `InvTxPayload`
- `GetTx` -> `GetTxPayload`
- `Tx` -> `TxPayload`
- `InvBlock` -> `InvBlockPayload`
- `GetBlock` -> `GetBlockPayload`
- `Block` -> `BlockPayload`
- `GetTip` -> `GetTipPayload`
- `Tip` -> `TipPayload`
- `GetHeaders` -> `GetHeadersPayload`
- `Headers` -> `HeadersPayload`
- `GetBlocks` -> `GetBlocksPayload`
- `Blocks` -> `BlocksPayload`

若遇到未知 `type`，payload 会保留为原始 JSON（`json.RawMessage`），以便向前兼容。

## 版本与兼容性建议

- 新增消息类型时，尽量保持旧节点可忽略未知类型。
- 新增字段时优先使用可选字段，避免破坏已有节点。
- 如果需要破坏性修改，建议更新 `magic/version` 并在握手阶段拒绝旧版本。
