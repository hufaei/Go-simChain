package syncer

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestMetricsTracker_RecordRequest(t *testing.T) {
	mt := newMetricsTracker()

	// Record a successful request
	startTime := time.Now().Add(-50 * time.Millisecond)
	mt.recordRequest("peer1", startTime, true)

	metrics := mt.getPeerMetrics("peer1")
	if metrics == nil {
		t.Fatal("expected metrics for peer1")
	}

	if metrics.totalRequests != 1 {
		t.Errorf("expected totalRequests=1, got %d", metrics.totalRequests)
	}
	if metrics.successCount != 1 {
		t.Errorf("expected successCount=1, got %d", metrics.successCount)
	}
	if metrics.failureCount != 0 {
		t.Errorf("expected failureCount=0, got %d", metrics.failureCount)
	}
	if metrics.avgRTT < 40*time.Millisecond || metrics.avgRTT > 60*time.Millisecond {
		t.Errorf("expected avgRTT around 50ms, got %v", metrics.avgRTT)
	}
}

func TestMetricsTracker_RecordFailure(t *testing.T) {
	mt := newMetricsTracker()

	// Record a failed request
	startTime := time.Now()
	mt.recordRequest("peer1", startTime, false)

	metrics := mt.getPeerMetrics("peer1")
	if metrics == nil {
		t.Fatal("expected metrics for peer1")
	}

	if metrics.totalRequests != 1 {
		t.Errorf("expected totalRequests=1, got %d", metrics.totalRequests)
	}
	if metrics.successCount != 0 {
		t.Errorf("expected successCount=0, got %d", metrics.successCount)
	}
	if metrics.failureCount != 1 {
		t.Errorf("expected failureCount=1, got %d", metrics.failureCount)
	}
	if metrics.consecutiveFailures != 1 {
		t.Errorf("expected consecutiveFailures=1, got %d", metrics.consecutiveFailures)
	}
}

func TestMetricsTracker_ConsecutiveFailures(t *testing.T) {
	mt := newMetricsTracker()

	// Record multiple consecutive failures
	for i := 0; i < 3; i++ {
		mt.recordRequest("peer1", time.Now(), false)
	}

	metrics := mt.getPeerMetrics("peer1")
	if metrics.consecutiveFailures != 3 {
		t.Errorf("expected consecutiveFailures=3, got %d", metrics.consecutiveFailures)
	}

	// A success should reset consecutive failures
	mt.recordRequest("peer1", time.Now(), true)
	metrics = mt.getPeerMetrics("peer1")
	if metrics.consecutiveFailures != 0 {
		t.Errorf("expected consecutiveFailures=0 after success, got %d", metrics.consecutiveFailures)
	}
}

func TestMetricsTracker_GetBestPeer(t *testing.T) {
	mt := newMetricsTracker()

	// peer1: high success rate, low RTT
	for i := 0; i < 10; i++ {
		mt.recordRequest("peer1", time.Now().Add(-20*time.Millisecond), true)
	}

	// peer2: lower success rate
	for i := 0; i < 10; i++ {
		if i < 7 {
			mt.recordRequest("peer2", time.Now().Add(-30*time.Millisecond), true)
		} else {
			mt.recordRequest("peer2", time.Now(), false)
		}
	}

	// peer3: high success rate but higher RTT
	for i := 0; i < 10; i++ {
		mt.recordRequest("peer3", time.Now().Add(-100*time.Millisecond), true)
	}

	candidates := []string{"peer1", "peer2", "peer3"}
	best := mt.getBestPeer(candidates)

	// peer1 should be selected (high success rate + low RTT)
	if best != "peer1" {
		t.Errorf("expected best peer to be peer1, got %s", best)
	}
}

func TestMetricsTracker_ShouldBackoffPeer(t *testing.T) {
	mt := newMetricsTracker()

	// Initially, no backoff
	if mt.shouldBackoffPeer("peer1") {
		t.Error("expected no backoff for peer1 initially")
	}

	// After 4 consecutive failures, should backoff
	for i := 0; i < 4; i++ {
		mt.recordRequest("peer1", time.Now(), false)
	}

	if !mt.shouldBackoffPeer("peer1") {
		t.Error("expected backoff after 4 consecutive failures")
	}

	// A success should clear backoff
	mt.recordRequest("peer1", time.Now(), true)
	if mt.shouldBackoffPeer("peer1") {
		t.Error("expected no backoff after success")
	}
}

func TestMetricsTracker_BackoffAfterRecentTimeout(t *testing.T) {
	mt := newMetricsTracker()

	// Record a failure (sets lastTimeout)
	mt.recordRequest("peer1", time.Now(), false)

	// Should backoff if timeout was recent (< 30s ago)
	if !mt.shouldBackoffPeer("peer1") {
		t.Error("expected backoff shortly after timeout")
	}
}

func TestMetricsTracker_GetBestPeerWithBackoff(t *testing.T) {
	mt := newMetricsTracker()

	// peer1: backed off due to consecutive failures
	for i := 0; i < 5; i++ {
		mt.recordRequest("peer1", time.Now(), false)
	}

	// peer2: good performance
	for i := 0; i < 10; i++ {
		mt.recordRequest("peer2", time.Now().Add(-20*time.Millisecond), true)
	}

	candidates := []string{"peer1", "peer2"}
	best := mt.getBestPeer(candidates)

	// Should select peer2 since peer1 is backed off
	if best != "peer2" {
		t.Errorf("expected best peer to be peer2 (peer1 backed off), got %s", best)
	}
}

func TestMetricsTracker_GetBestPeerAllBackedOff(t *testing.T) {
	mt := newMetricsTracker()

	// All peers are backed off
	for i := 0; i < 5; i++ {
		mt.recordRequest("peer1", time.Now(), false)
		mt.recordRequest("peer2", time.Now(), false)
	}

	candidates := []string{"peer1", "peer2"}
	best := mt.getBestPeer(candidates)

	// Should return a fallback (first candidate)
	if best == "" {
		t.Error("expected a fallback peer when all are backed off")
	}
}

func TestMetricsTracker_DumpMetrics(t *testing.T) {
	mt := newMetricsTracker()

	// Record some metrics
	mt.recordRequest("peer1", time.Now().Add(-50*time.Millisecond), true)
	mt.recordRequest("peer1", time.Now().Add(-60*time.Millisecond), true)
	mt.recordRequest("peer2", time.Now(), false)

	dump := mt.dumpMetrics()

	// Should contain peer IDs and metrics
	if dump == "No peer metrics available" {
		t.Error("expected metrics in dump")
	}

	// Basic sanity checks
	if len(dump) < 50 {
		t.Errorf("dump seems too short: %s", dump)
	}
}

func TestMetricsTracker_Reset(t *testing.T) {
	mt := newMetricsTracker()

	// Record some metrics
	mt.recordRequest("peer1", time.Now(), true)
	mt.recordRequest("peer2", time.Now(), false)

	// Reset
	mt.reset()

	// Should have no metrics
	if mt.getPeerMetrics("peer1") != nil {
		t.Error("expected no metrics for peer1 after reset")
	}
	if mt.getPeerMetrics("peer2") != nil {
		t.Error("expected no metrics for peer2 after reset")
	}
}

// TestMetricsTracker_Concurrent verifies thread safety under concurrent access
func TestMetricsTracker_Concurrent(t *testing.T) {
	mt := newMetricsTracker()
	var wg sync.WaitGroup

	// Number of goroutines
	numWriters := 50
	numReaders := 20

	// Concurrent writes
	for i := 0; i < numWriters; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			peerID := fmt.Sprintf("peer%d", id%10)
			// Simulate multiple requests
			for j := 0; j < 10; j++ {
				success := (j % 3) != 0 // 2/3 success rate
				mt.recordRequest(peerID, time.Now().Add(-time.Duration(j*10)*time.Millisecond), success)
			}
		}(i)
	}

	// Concurrent reads
	for i := 0; i < numReaders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Simulate peer selection
			candidates := []string{"peer1", "peer2", "peer3", "peer4", "peer5"}
			for j := 0; j < 20; j++ {
				_ = mt.getBestPeer(candidates)
				// Also test individual metrics retrieval
				_ = mt.getPeerMetrics("peer1")
				_ = mt.shouldBackoffPeer("peer2")
			}
		}()
	}

	wg.Wait()

	// Verify no panics or data races occurred
	// Check that metrics were recorded
	metrics := mt.getPeerMetrics("peer0")
	if metrics == nil {
		t.Error("expected metrics for peer0 after concurrent writes")
	}

	// Verify dump works after concurrent access
	dump := mt.dumpMetrics()
	if dump == "No peer metrics available" {
		t.Error("expected metrics to be available after concurrent access")
	}
}
