package types

import (
	"encoding/json"
	"fmt"
)

// MessageType represents the logical protocol type.
type MessageType string

const (
	MsgNewTx       MessageType = "NewTx"
	MsgNewBlock    MessageType = "NewBlock"
	MsgRequestTip  MessageType = "RequestTip"
	MsgResponseTip MessageType = "ResponseTip"

	// V3-B: transport-level control (tcp + discovery + identity).
	MsgHello    MessageType = "Hello"
	MsgHelloAck MessageType = "HelloAck"
	MsgAuth     MessageType = "Auth"
	MsgGetPeers MessageType = "GetPeers"
	MsgPeers    MessageType = "Peers"

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

type wireMessage struct {
	Type      MessageType     `json:"type"`
	From      string          `json:"from"`
	To        string          `json:"to,omitempty"`
	Timestamp int64           `json:"timestamp"`
	TraceID   string          `json:"traceId,omitempty"`
	Payload   json.RawMessage `json:"payload"`
}

func (m Message) MarshalJSON() ([]byte, error) {
	var payload json.RawMessage
	switch p := m.Payload.(type) {
	case nil:
		payload = []byte("null")
	case json.RawMessage:
		payload = p
	default:
		raw, err := json.Marshal(p)
		if err != nil {
			return nil, err
		}
		payload = raw
	}
	return json.Marshal(wireMessage{
		Type:      m.Type,
		From:      m.From,
		To:        m.To,
		Timestamp: m.Timestamp,
		TraceID:   m.TraceID,
		Payload:   payload,
	})
}

func (m *Message) UnmarshalJSON(b []byte) error {
	var w wireMessage
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	m.Type = w.Type
	m.From = w.From
	m.To = w.To
	m.Timestamp = w.Timestamp
	m.TraceID = w.TraceID

	var payload any
	switch w.Type {
	case MsgHello:
		payload = &HelloPayload{}
	case MsgHelloAck:
		payload = &HelloAckPayload{}
	case MsgAuth:
		payload = &AuthPayload{}
	case MsgGetPeers:
		payload = &GetPeersPayload{}
	case MsgPeers:
		payload = &PeersPayload{}
	case MsgInvTx:
		payload = &InvTxPayload{}
	case MsgGetTx:
		payload = &GetTxPayload{}
	case MsgTx:
		payload = &TxPayload{}
	case MsgInvBlock:
		payload = &InvBlockPayload{}
	case MsgGetBlock:
		payload = &GetBlockPayload{}
	case MsgBlock:
		payload = &BlockPayload{}
	case MsgGetTip:
		payload = &GetTipPayload{}
	case MsgTip:
		payload = &TipPayload{}
	case MsgGetHeaders:
		payload = &GetHeadersPayload{}
	case MsgHeaders:
		payload = &HeadersPayload{}
	case MsgGetBlocks:
		payload = &GetBlocksPayload{}
	case MsgBlocks:
		payload = &BlocksPayload{}
	default:
		// Preserve unknown payload as raw JSON to keep decoding forward-compatible.
		m.Payload = json.RawMessage(w.Payload)
		return nil
	}

	if err := json.Unmarshal(w.Payload, payload); err != nil {
		return fmt.Errorf("decode payload for %q: %w", w.Type, err)
	}

	// Store as value where possible, to preserve existing inproc type-assert patterns.
	switch p := payload.(type) {
	case *HelloPayload:
		m.Payload = *p
	case *HelloAckPayload:
		m.Payload = *p
	case *AuthPayload:
		m.Payload = *p
	case *GetPeersPayload:
		m.Payload = *p
	case *PeersPayload:
		m.Payload = *p
	case *InvTxPayload:
		m.Payload = *p
	case *GetTxPayload:
		m.Payload = *p
	case *TxPayload:
		m.Payload = *p
	case *InvBlockPayload:
		m.Payload = *p
	case *GetBlockPayload:
		m.Payload = *p
	case *BlockPayload:
		m.Payload = *p
	case *GetTipPayload:
		m.Payload = *p
	case *TipPayload:
		m.Payload = *p
	case *GetHeadersPayload:
		m.Payload = *p
	case *HeadersPayload:
		m.Payload = *p
	case *GetBlocksPayload:
		m.Payload = *p
	case *BlocksPayload:
		m.Payload = *p
	default:
		m.Payload = payload
	}
	return nil
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

// HelloPayload starts a TCP handshake (V3-B).
type HelloPayload struct {
	Magic      string `json:"magic"`
	Version    int    `json:"version"`
	PubKey     []byte `json:"pubKey"`
	ListenAddr string `json:"listenAddr"`
	Nonce      []byte `json:"nonce"`
	Timestamp  int64  `json:"timestamp"` // unix ms
}

// HelloAckPayload authenticates the responder by signing the initiator nonce,
// and provides a responder nonce for the initiator to sign back in Auth.
type HelloAckPayload struct {
	Magic       string `json:"magic"`
	Version     int    `json:"version"`
	PubKey      []byte `json:"pubKey"`
	ListenAddr  string `json:"listenAddr"`
	Nonce       []byte `json:"nonce"`
	EchoNonce   []byte `json:"echoNonce"`
	SigOverEcho []byte `json:"sigOverEcho"`
	Timestamp   int64  `json:"timestamp"` // unix ms
}

// AuthPayload completes the handshake by signing the responder nonce.
type AuthPayload struct {
	EchoNonce []byte `json:"echoNonce"`
	Sig       []byte `json:"sig"`
	Timestamp int64  `json:"timestamp"` // unix ms
}

type GetPeersPayload struct {
	Max int `json:"max"`
}

type PeersPayload struct {
	Addrs []string `json:"addrs"`
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
