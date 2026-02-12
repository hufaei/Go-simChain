package mempool

import (
	"testing"
	"time"

	"simchain-go/internal/types"
)

func TestMempool_Add_CapacityLimit(t *testing.T) {
	// 限制 2 个交易，1000 字节
	cfg := Config{
		MaxTxs:   2,
		MaxBytes: 1000,
		TTL:      time.Minute,
	}
	mp := New(cfg)

	tx1 := types.Transaction{ID: "tx1", Payload: "p1"}
	tx2 := types.Transaction{ID: "tx2", Payload: "p2"}
	tx3 := types.Transaction{ID: "tx3", Payload: "p3"}

	if !mp.Add(tx1) {
		t.Error("failed to add tx1")
	}
	if !mp.Add(tx2) {
		t.Error("failed to add tx2")
	}

	// Should be rejected (MaxTxs limit)
	if mp.Add(tx3) {
		t.Error("expected tx3 to be rejected due to capacity limit")
	}

	if mp.Size() != 2 {
		t.Errorf("expected size 2, got %d", mp.Size())
	}
}

func TestMempool_Add_ByteLimit(t *testing.T) {
	// 限制非常小的字节数
	cfg := Config{
		MaxTxs:   100,
		MaxBytes: 10, // Very small limit
		TTL:      time.Minute,
	}
	mp := New(cfg)

	tx1 := types.Transaction{ID: "tx1", Payload: "payload_larger_than_10_bytes"}

	if mp.Add(tx1) {
		t.Error("expected tx1 to be rejected due to byte limit")
	}
}

func TestMempool_TTL_Cleanup(t *testing.T) {
	cfg := Config{
		MaxTxs:   100,
		MaxBytes: 10000,
		TTL:      50 * time.Millisecond,
	}
	mp := New(cfg)

	mp.Add(types.Transaction{ID: "tx1"})

	// Not expired yet
	mp.CleanExpired()
	if mp.Size() != 1 {
		t.Error("expected tx1 to remain")
	}

	// Wait for expiry
	time.Sleep(100 * time.Millisecond)
	mp.CleanExpired()

	if mp.Size() != 0 {
		t.Error("expected tx1 to be cleaned up")
	}

	// Check internal maps are cleaned
	mp.mu.Lock()
	if len(mp.seen) != 0 {
		t.Error("expected seen map to be empty")
	}
	if len(mp.txExpiry) != 0 {
		t.Error("expected txExpiry map to be empty")
	}
	if mp.totalBytes != 0 {
		t.Error("expected totalBytes to be 0")
	}
	mp.mu.Unlock()
}

func TestMempool_BroadcastDedup(t *testing.T) {
	cfg := Config{
		MaxTxs:               100,
		InvBroadcastCooldown: 100 * time.Millisecond,
	}
	mp := New(cfg)

	txID := "tx1"

	// Initially allowed
	if !mp.CanBroadcast(txID) {
		t.Error("expected initial broadcast to be allowed")
	}

	// Mark as broadcast
	mp.MarkBroadcast(txID)

	// Immediate retry should be denied
	if mp.CanBroadcast(txID) {
		t.Error("expected immediate retry to be denied")
	}

	// Wait for cooldown
	time.Sleep(150 * time.Millisecond)
	if !mp.CanBroadcast(txID) {
		t.Error("expected broadcast after cooldown to be allowed")
	}
}

func TestMempool_Pop_UpdatesTotalBytes(t *testing.T) {
	cfg := DefaultConfig()
	mp := New(cfg)

	tx := types.Transaction{ID: "tx1"}
	mp.Add(tx)

	initialBytes := 0
	mp.mu.Lock()
	initialBytes = mp.totalBytes
	mp.mu.Unlock()

	if initialBytes == 0 {
		t.Error("expected totalBytes > 0")
	}

	popped := mp.Pop(1)
	if len(popped) != 1 {
		t.Fatal("expected 1 tx popped")
	}

	mp.mu.Lock()
	finalBytes := mp.totalBytes
	mp.mu.Unlock()

	if finalBytes != 0 {
		t.Errorf("expected totalBytes 0 after pop, got %d", finalBytes)
	}
}
