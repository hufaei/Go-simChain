package types

// MessageType represents the logical protocol type.
type MessageType string

const (
	MsgNewTx       MessageType = "NewTx"
	MsgNewBlock    MessageType = "NewBlock"
	MsgRequestTip  MessageType = "RequestTip"
	MsgResponseTip MessageType = "ResponseTip"

	// V2: inventory + on-demand fetch (Bitcoin-style).
	//
	// 核心思路：只广播“我有某个对象”的摘要公告（Inv*），
	// 真正的完整数据通过定向请求（Get*）与定向响应（*）按需拉取。
	MsgInvTx    MessageType = "InvTx"
	MsgGetTx    MessageType = "GetTx"
	MsgTx       MessageType = "Tx"
	MsgInvBlock MessageType = "InvBlock"
	MsgGetBlock MessageType = "GetBlock"
	MsgBlock    MessageType = "Block"

	// V2: join sync / headers-first.
	//
	// 节点“中途加入”时的 headers-first 同步：
	// 先询问 peers 的 tip，再用 locator 机制通过 GetHeaders/Headers 找出缺失区块，
	// 最后用 GetBlocks/Blocks 批量下载完整区块并本地校验接入。
	MsgGetTip     MessageType = "GetTip"
	MsgTip        MessageType = "Tip"
	MsgGetHeaders MessageType = "GetHeaders"
	MsgHeaders    MessageType = "Headers"
	MsgGetBlocks  MessageType = "GetBlocks"
	MsgBlocks     MessageType = "Blocks"
)

// Message is a generic node-to-node message.
// Payload uses concrete structs below.
type Message struct {
	Type      MessageType `json:"type"`
	From      string      `json:"from"`
	To        string      `json:"to,omitempty"`
	Timestamp int64       `json:"timestamp"`
	// TraceID correlates a single request/response round trip.
	// 本仓库把它当作轻量的“RPC”相关 ID，用于 initial sync 的一次请求-一次响应匹配。
	TraceID string `json:"traceId,omitempty"`
	Payload any    `json:"payload"`
}

type NewTxPayload struct {
	Tx Transaction `json:"tx"`
}

type NewBlockPayload struct {
	Block *Block `json:"block"`
}

type RequestTipPayload struct {
	KnownTipHash string `json:"knownTipHash,omitempty"`
	KnownHeight  uint64 `json:"knownHeight,omitempty"`
}

type ResponseTipPayload struct {
	TipHash   string  `json:"tipHash"`
	TipHeight uint64  `json:"tipHeight"`
	CumWork   string  `json:"cumWork"`
	Blocks    []Block `json:"blocks,omitempty"`
}

type InvTxPayload struct {
	TxID string `json:"txId"`
}

type GetTxPayload struct {
	TxID string `json:"txId"`
}

type TxPayload struct {
	Tx Transaction `json:"tx"`
}

type InvBlockPayload struct {
	Meta BlockMeta `json:"meta"`
}

type GetBlockPayload struct {
	Hash Hash `json:"hash"`
}

type BlockPayload struct {
	Block *Block `json:"block"`
}

type GetTipPayload struct{}

type TipPayload struct {
	TipHash   Hash   `json:"tipHash"`
	TipHeight uint64 `json:"tipHeight"`
	CumWork   string `json:"cumWork"`
}

type GetHeadersPayload struct {
	Locator []Hash `json:"locator"`
	Max     int    `json:"max"`
}

type HeadersPayload struct {
	Metas []BlockMeta `json:"metas"`
}

type GetBlocksPayload struct {
	Hashes []Hash `json:"hashes"`
}

type BlocksPayload struct {
	Blocks []*Block `json:"blocks"`
}
