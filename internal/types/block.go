package types

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// Hash 是 32 字节 SHA256 哈希。
type Hash [32]byte

func (h Hash) String() string {
	return hex.EncodeToString(h[:])
}

func (h Hash) IsZero() bool {
	for _, b := range h {
		if b != 0 {
			return false
		}
	}
	return true
}

// MarshalJSON 把 hash 编码成 64 位 hex 字符串。
func (h Hash) MarshalJSON() ([]byte, error) {
	return json.Marshal(h.String())
}

// UnmarshalJSON 从 64 位 hex 字符串解码成 hash。
//
// 注意：这里必须用指针接收器（*Hash）才能把结果写回变量，这是 encoding/json 的常见用法。
func (h *Hash) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	raw, err := hex.DecodeString(s)
	if err != nil {
		return err
	}
	if len(raw) != len(h[:]) {
		return fmt.Errorf("invalid hash length: %d", len(raw))
	}
	copy(h[:], raw)
	return nil
}

type BlockHeader struct {
	Height     uint64 `json:"height"`
	PrevHash   Hash   `json:"prevHash"`
	Timestamp  int64  `json:"timestamp"` // unix ms
	Difficulty uint32 `json:"difficulty"`
	MinerID    string `json:"minerId"`
	TxRoot     Hash   `json:"txRoot"`
}

type Block struct {
	Header BlockHeader   `json:"header"`
	Nonce  uint64        `json:"nonce"`
	Hash   Hash          `json:"hash"`
	Txs    []Transaction `json:"txs"`
}

// BlockMeta 是区块的紧凑表示，用于 inv 公告与 headers-first 同步。
// 它包含足够的字段做 PoW 轻校验（Header + Nonce -> Hash），但不包含交易列表。
type BlockMeta struct {
	Header BlockHeader `json:"header"`
	Nonce  uint64      `json:"nonce"`
	Hash   Hash        `json:"hash"`
}

// NewGenesisBlock 创建所有节点共享的确定性 genesis 区块。
// Difficulty 为 0，确保始终有效。
func NewGenesisBlock() *Block {
	header := BlockHeader{
		Height:     0,
		PrevHash:   Hash{},
		Timestamp:  0,
		Difficulty: 0,
		MinerID:    "genesis",
		TxRoot:     Hash{},
	}
	return &Block{
		Header: header,
		Nonce:  0,
	}
}
