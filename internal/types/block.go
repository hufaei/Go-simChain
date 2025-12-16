package types

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// Hash is a 32-byte SHA256 hash.
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

// MarshalJSON encodes the hash as a 64-char hex string.
func (h Hash) MarshalJSON() ([]byte, error) {
	return json.Marshal(h.String())
}

// UnmarshalJSON decodes a 64-char hex string into a hash.
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

// BlockMeta is a compact representation of a block used for announcements and headers-first sync.
// It contains enough data to validate PoW (Header + Nonce -> Hash), but not the full transaction list.
type BlockMeta struct {
	Header BlockHeader `json:"header"`
	Nonce  uint64      `json:"nonce"`
	Hash   Hash        `json:"hash"`
}

// NewGenesisBlock creates a deterministic genesis block shared by all nodes.
// Difficulty is set to 0 so it is always valid.
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
