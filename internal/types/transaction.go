package types

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// Transaction is a minimal payload-only transaction for local simulation.
type Transaction struct {
	ID        string `json:"id"`
	Timestamp int64  `json:"timestamp"` // unix ms
	Payload   string `json:"payload"`
}

// NewTransaction creates a new transaction with a random ID.
func NewTransaction(payload string) Transaction {
	return Transaction{
		ID:        newTxID(),
		Timestamp: time.Now().UnixMilli(),
		Payload:   payload,
	}
}

func newTxID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // best-effort randomness; ignore error for demo
	return fmt.Sprintf("%d-%s", time.Now().UnixNano(), hex.EncodeToString(b[:]))
}
