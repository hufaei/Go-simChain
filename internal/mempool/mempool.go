package mempool

import (
	"fmt"
	"sync"
	"time"

	"simchain-go/internal/types"
)

// Config defines configuration for Mempool.
type Config struct {
	MaxTxs               int
	MaxBytes             int
	TTL                  time.Duration
	InvBroadcastCooldown time.Duration
}

// DefaultConfig returns reasonable default values.
func DefaultConfig() Config {
	return Config{
		MaxTxs:               5000,
		MaxBytes:             10 * 1024 * 1024, // 10MB
		TTL:                  10 * time.Minute,
		InvBroadcastCooldown: 30 * time.Second,
	}
}

type txEntry struct {
	tx        types.Transaction
	addedAt   time.Time
	expiresAt time.Time
	size      int
}

// Mempool stores pending transactions in arrival order with constraints.
type Mempool struct {
	mu  sync.Mutex
	cfg Config

	// Storage
	txs  []txEntry
	seen map[string]struct{}

	// TTL management
	txExpiry map[string]time.Time

	// Capacity tracking
	totalBytes int

	// Broadcast deduplication
	lastInvBroadcast map[string]time.Time
}

// New creates a new Mempool with the given config.
func New(cfg Config) *Mempool {
	if cfg.MaxTxs <= 0 {
		cfg.MaxTxs = 5000
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = 10 * 1024 * 1024
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 10 * time.Minute
	}
	if cfg.InvBroadcastCooldown <= 0 {
		cfg.InvBroadcastCooldown = 30 * time.Second
	}

	return &Mempool{
		cfg:              cfg,
		txs:              make([]txEntry, 0),
		seen:             make(map[string]struct{}),
		txExpiry:         make(map[string]time.Time),
		lastInvBroadcast: make(map[string]time.Time),
	}
}

// NewMempool maintains backward compatibility (uses defaults).
func NewMempool() *Mempool {
	return New(DefaultConfig())
}

// Add inserts a tx if not already seen and within capacity limits.
// Returns true if added, false if rejected (duplicate or full).
func (m *Mempool) Add(tx types.Transaction) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.seen[tx.ID]; ok {
		return false
	}

	txSize := estimateTxSize(tx)

	// FCFS Capacity Check (Drop-tail)
	if len(m.txs) >= m.cfg.MaxTxs {
		return false
	}
	if m.totalBytes+txSize > m.cfg.MaxBytes {
		return false
	}

	now := time.Now()
	entry := txEntry{
		tx:        tx,
		addedAt:   now,
		expiresAt: now.Add(m.cfg.TTL),
		size:      txSize,
	}

	m.seen[tx.ID] = struct{}{}
	m.txExpiry[tx.ID] = entry.expiresAt
	m.txs = append(m.txs, entry)
	m.totalBytes += txSize

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

	// Extract transactions
	poppedEntries := m.txs[:max]
	out := make([]types.Transaction, len(poppedEntries))

	for i, entry := range poppedEntries {
		out[i] = entry.tx
		// Cleanup auxiliary maps
		delete(m.seen, entry.tx.ID)
		delete(m.txExpiry, entry.tx.ID)
		delete(m.lastInvBroadcast, entry.tx.ID) // Optional: clear broadcast history?
		m.totalBytes -= entry.size
	}

	// Update slice
	m.txs = m.txs[max:]

	// Safety check for totalBytes (avoid negative drift)
	if len(m.txs) == 0 {
		m.totalBytes = 0
	}

	return out
}

// Return re-adds transactions (used on reorg) if not already seen.
// Note: We might want to allow re-adding even if full?
// For now, we apply the same strict rules.
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
	// Reset totalBytes and recalculate
	m.totalBytes = 0

	for _, entry := range m.txs {
		if _, ok := ids[entry.tx.ID]; ok {
			delete(m.seen, entry.tx.ID)
			delete(m.txExpiry, entry.tx.ID)
			// Removed
			continue
		}
		filtered = append(filtered, entry)
		m.totalBytes += entry.size
	}

	m.txs = filtered
}

// Size returns the count of transactions.
func (m *Mempool) Size() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.txs)
}

// Snapshot returns up to max pending transactions in arrival order.
func (m *Mempool) Snapshot(max int) []types.Transaction {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.txs) == 0 || max == 0 {
		return nil
	}
	if max < 0 || max > len(m.txs) {
		max = len(m.txs)
	}

	out := make([]types.Transaction, max)
	for i := 0; i < max; i++ {
		out[i] = m.txs[i].tx
	}
	return out
}

// CleanExpired removes transactions that have exceeded their TTL.
func (m *Mempool) CleanExpired() {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	// Optimization: check if we need to clean at all?
	// But we need to check every expiration time.
	// Since txs are roughly ordered by time (arrival), we might optimize later.
	// For now, simple filtering.

	filtered := m.txs[:0]
	m.totalBytes = 0

	for _, entry := range m.txs {
		if now.After(entry.expiresAt) {
			delete(m.seen, entry.tx.ID)
			delete(m.txExpiry, entry.tx.ID)
			delete(m.lastInvBroadcast, entry.tx.ID)
			continue
		}
		filtered = append(filtered, entry)
		m.totalBytes += entry.size
	}
	m.txs = filtered
}

// CanBroadcast checks if a txInv can be broadcast (deduplication).
func (m *Mempool) CanBroadcast(txid string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	lastTime, ok := m.lastInvBroadcast[txid]
	if !ok {
		return true // Never broadcast before
	}

	return time.Since(lastTime) >= m.cfg.InvBroadcastCooldown
}

// MarkBroadcast records the time of a broadcast.
func (m *Mempool) MarkBroadcast(txid string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastInvBroadcast[txid] = time.Now()
}

// estimateTxSize estimates the memory size of a transaction.
func estimateTxSize(tx types.Transaction) int {
	// ID (string header + content) + Payload + Timestamp
	s := len(tx.ID) + len(tx.Payload)
	s += 8   // Timestamp (int64)
	s += 100 // Overhead for struct fields, slices headers, etc.
	return s
}

// DebugInfo returns internal state for viewing.
func (m *Mempool) DebugInfo() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return fmt.Sprintf("Txs: %d/%d, Bytes: %d/%d", len(m.txs), m.cfg.MaxTxs, m.totalBytes, m.cfg.MaxBytes)
}
