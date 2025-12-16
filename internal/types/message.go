package types

// MessageType represents the logical protocol type.
type MessageType string

const (
	MsgNewTx       MessageType = "NewTx"
	MsgNewBlock    MessageType = "NewBlock"
	MsgRequestTip  MessageType = "RequestTip"
	MsgResponseTip MessageType = "ResponseTip"

	// V2: inventory + on-demand fetch (Bitcoin-style).
	MsgInvTx    MessageType = "InvTx"
	MsgGetTx    MessageType = "GetTx"
	MsgTx       MessageType = "Tx"
	MsgInvBlock MessageType = "InvBlock"
	MsgGetBlock MessageType = "GetBlock"
	MsgBlock    MessageType = "Block"

	// V2: join sync / headers-first.
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
	TraceID   string      `json:"traceId,omitempty"`
	Payload   any         `json:"payload"`
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
