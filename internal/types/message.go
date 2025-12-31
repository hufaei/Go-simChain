package types

import (
	"encoding/json"
	"fmt"
)

// MessageType 表示节点之间的逻辑消息类型（协议类型）。
type MessageType string

const (
	// V3-B：传输层控制消息（tcp + 发现 + 身份绑定）。
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

// Message 是节点之间通用的消息封装。
// Payload 使用本文件里定义的具体 payload 结构体。
//
// 说明（V3-B）：
//   - inproc：Payload 直接携带具体结构体（便于类型断言）。
//   - tcp：消息经过 JSON 编解码；这里通过自定义 Marshal/Unmarshal，按 Type 把 payload 解回具体结构体，
//     避免退化成 map[string]any。
type Message struct {
	Type      MessageType `json:"type"`
	From      string      `json:"from"`
	To        string      `json:"to,omitempty"`
	Timestamp int64       `json:"timestamp"`
	// TraceID 用于“一次请求-一次响应”的轻量 RPC 匹配（主要给同步流程使用）。
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

// MarshalJSON 必须使用指针接收器（*Message），以避免同一个类型上出现“值接收器 + 指针接收器”的混用；
// 同时也要求调用方在跨进程编码时使用 `json.Marshal(&msg)`（本仓库的 tcp framing 已统一处理）。
func (m *Message) MarshalJSON() ([]byte, error) {
	if m == nil {
		return []byte("null"), nil
	}
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
	// 注意：UnmarshalJSON 必须使用指针接收器（*Message），才能把解析结果写回 m，
	// 这是 encoding/json 的常见用法。
	var w wireMessage
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	m.Type = w.Type
	m.From = w.From
	m.To = w.To
	m.Timestamp = w.Timestamp
	m.TraceID = w.TraceID

	if w.Payload == nil {
		m.Payload = nil
		return nil
	}

	switch w.Type {
	case MsgHello:
		var pl HelloPayload
		if err := json.Unmarshal(w.Payload, &pl); err != nil {
			return fmt.Errorf("decode payload for %q: %w", w.Type, err)
		}
		m.Payload = pl
	case MsgHelloAck:
		var pl HelloAckPayload
		if err := json.Unmarshal(w.Payload, &pl); err != nil {
			return fmt.Errorf("decode payload for %q: %w", w.Type, err)
		}
		m.Payload = pl
	case MsgAuth:
		var pl AuthPayload
		if err := json.Unmarshal(w.Payload, &pl); err != nil {
			return fmt.Errorf("decode payload for %q: %w", w.Type, err)
		}
		m.Payload = pl
	case MsgGetPeers:
		var pl GetPeersPayload
		if err := json.Unmarshal(w.Payload, &pl); err != nil {
			return fmt.Errorf("decode payload for %q: %w", w.Type, err)
		}
		m.Payload = pl
	case MsgPeers:
		var pl PeersPayload
		if err := json.Unmarshal(w.Payload, &pl); err != nil {
			return fmt.Errorf("decode payload for %q: %w", w.Type, err)
		}
		m.Payload = pl
	case MsgInvTx:
		var pl InvTxPayload
		if err := json.Unmarshal(w.Payload, &pl); err != nil {
			return fmt.Errorf("decode payload for %q: %w", w.Type, err)
		}
		m.Payload = pl
	case MsgGetTx:
		var pl GetTxPayload
		if err := json.Unmarshal(w.Payload, &pl); err != nil {
			return fmt.Errorf("decode payload for %q: %w", w.Type, err)
		}
		m.Payload = pl
	case MsgTx:
		var pl TxPayload
		if err := json.Unmarshal(w.Payload, &pl); err != nil {
			return fmt.Errorf("decode payload for %q: %w", w.Type, err)
		}
		m.Payload = pl
	case MsgInvBlock:
		var pl InvBlockPayload
		if err := json.Unmarshal(w.Payload, &pl); err != nil {
			return fmt.Errorf("decode payload for %q: %w", w.Type, err)
		}
		m.Payload = pl
	case MsgGetBlock:
		var pl GetBlockPayload
		if err := json.Unmarshal(w.Payload, &pl); err != nil {
			return fmt.Errorf("decode payload for %q: %w", w.Type, err)
		}
		m.Payload = pl
	case MsgBlock:
		var pl BlockPayload
		if err := json.Unmarshal(w.Payload, &pl); err != nil {
			return fmt.Errorf("decode payload for %q: %w", w.Type, err)
		}
		m.Payload = pl
	case MsgGetTip:
		var pl GetTipPayload
		if err := json.Unmarshal(w.Payload, &pl); err != nil {
			return fmt.Errorf("decode payload for %q: %w", w.Type, err)
		}
		m.Payload = pl
	case MsgTip:
		var pl TipPayload
		if err := json.Unmarshal(w.Payload, &pl); err != nil {
			return fmt.Errorf("decode payload for %q: %w", w.Type, err)
		}
		m.Payload = pl
	case MsgGetHeaders:
		var pl GetHeadersPayload
		if err := json.Unmarshal(w.Payload, &pl); err != nil {
			return fmt.Errorf("decode payload for %q: %w", w.Type, err)
		}
		m.Payload = pl
	case MsgHeaders:
		var pl HeadersPayload
		if err := json.Unmarshal(w.Payload, &pl); err != nil {
			return fmt.Errorf("decode payload for %q: %w", w.Type, err)
		}
		m.Payload = pl
	case MsgGetBlocks:
		var pl GetBlocksPayload
		if err := json.Unmarshal(w.Payload, &pl); err != nil {
			return fmt.Errorf("decode payload for %q: %w", w.Type, err)
		}
		m.Payload = pl
	case MsgBlocks:
		var pl BlocksPayload
		if err := json.Unmarshal(w.Payload, &pl); err != nil {
			return fmt.Errorf("decode payload for %q: %w", w.Type, err)
		}
		m.Payload = pl
	default:
		// 未知消息：保留原始 JSON，便于向前兼容。
		m.Payload = w.Payload
		return nil
	}
	return nil
}

// HelloPayload：TCP 握手第一步（发起方 -> 响应方）。
type HelloPayload struct {
	Magic      string `json:"magic"`
	Version    int    `json:"version"`
	PubKey     []byte `json:"pubKey"`
	ListenAddr string `json:"listenAddr"`
	Nonce      []byte `json:"nonce"`
	Timestamp  int64  `json:"timestamp"` // unix ms
}

// HelloAckPayload：TCP 握手第二步（响应方 -> 发起方）。
//
// 响应方需要：
// - 回显发起方 nonce（EchoNonce）
// - 用自己的私钥签名该 nonce（SigOverEcho）
// - 生成一个新的 nonce，让发起方在 Auth 中签回（Nonce）
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

// AuthPayload：TCP 握手第三步（发起方 -> 响应方），签回响应方 nonce。
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
