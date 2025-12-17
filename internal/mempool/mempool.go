package mempool

import (
	"sync"

	"simchain-go/internal/types"
)

// Mempool stores pending transactions in arrival order.
type Mempool struct {
	mu   sync.Mutex
	txs  []types.Transaction
	seen map[string]struct{}
}

func NewMempool() *Mempool {
	return &Mempool{
		txs:  make([]types.Transaction, 0),
		seen: make(map[string]struct{}),
	}
}

// Add inserts a tx if not already seen. Returns true if added.
func (m *Mempool) Add(tx types.Transaction) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.seen[tx.ID]; ok {
		return false
	}
	m.seen[tx.ID] = struct{}{}
	m.txs = append(m.txs, tx)
	return true
}

// Pop removes up to max txs from the front.
func (m *Mempool) Pop(max int) []types.Transaction {
	m.mu.Lock()
	defer m.mu.Unlock()
	if max <= 0 || len(m.txs) == 0 {
		return nil
	}
	if max > len(m.txs) {
		max = len(m.txs)
	}
	out := append([]types.Transaction(nil), m.txs[:max]...)
	for _, tx := range out {
		delete(m.seen, tx.ID)
	}
	m.txs = m.txs[max:]
	return out
}

// Return re-adds transactions (used on reorg) if not already seen.
func (m *Mempool) Return(txs []types.Transaction) {
	for _, tx := range txs {
		m.Add(tx)
	}
}

// RemoveByID removes any txs whose IDs are in the given set.
func (m *Mempool) RemoveByID(ids map[string]struct{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(ids) == 0 || len(m.txs) == 0 {
		return
	}
	filtered := m.txs[:0]
	for _, tx := range m.txs {
		if _, ok := ids[tx.ID]; ok {
			delete(m.seen, tx.ID)
			continue
		}
		filtered = append(filtered, tx)
	}
	m.txs = filtered
}

func (m *Mempool) Size() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.txs)
}

// Snapshot returns up to max pending transactions in arrival order.
// It returns copies suitable for debugging/printing.
func (m *Mempool) Snapshot(max int) []types.Transaction {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.txs) == 0 || max == 0 {
		return nil
	}
	if max < 0 || max > len(m.txs) {
		max = len(m.txs)
	}
	return append([]types.Transaction(nil), m.txs[:max]...)
}
