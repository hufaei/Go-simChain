package syncer

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// peerMetrics records performance statistics for a single peer.
type peerMetrics struct {
	peerID              string
	totalRequests       int
	successCount        int
	failureCount        int
	totalRTT            time.Duration // cumulative RTT
	avgRTT              time.Duration // average RTT
	lastTimeout         time.Time
	consecutiveFailures int
	lastSuccess         time.Time
}

// metricsTracker manages performance metrics for all peers.
type metricsTracker struct {
	mu    sync.Mutex
	peers map[string]*peerMetrics
}

// newMetricsTracker creates a new metrics tracker.
func newMetricsTracker() *metricsTracker {
	return &metricsTracker{
		peers: make(map[string]*peerMetrics),
	}
}

// recordRequest records a request and its outcome (success or failure).
// startTime is when the request was sent, used to calculate RTT.
func (mt *metricsTracker) recordRequest(peerID string, startTime time.Time, success bool) {
	mt.mu.Lock()
	defer mt.mu.Unlock()

	m, ok := mt.peers[peerID]
	if !ok {
		m = &peerMetrics{peerID: peerID}
		mt.peers[peerID] = m
	}

	m.totalRequests++
	rtt := time.Since(startTime)

	if success {
		m.successCount++
		m.consecutiveFailures = 0
		m.lastSuccess = time.Now()
		m.lastTimeout = time.Time{} // Clear lastTimeout on success (V3-C fix)
		// Update RTT only on success
		m.totalRTT += rtt
		if m.successCount > 0 {
			m.avgRTT = m.totalRTT / time.Duration(m.successCount)
		}
	} else {
		m.failureCount++
		m.consecutiveFailures++
		m.lastTimeout = time.Now()
	}
}

// getBestPeer selects the best peer from the given candidates based on performance.
// Returns empty string if no suitable peer is found.
func (mt *metricsTracker) getBestPeer(candidates []string) string {
	mt.mu.Lock()
	defer mt.mu.Unlock()

	if len(candidates) == 0 {
		return ""
	}

	// Filter out peers that should be backed off
	viable := make([]string, 0, len(candidates))
	for _, peerID := range candidates {
		if !mt.shouldBackoffPeerLocked(peerID) {
			viable = append(viable, peerID)
		}
	}

	if len(viable) == 0 {
		// All peers are backed off, return any candidate as fallback
		return candidates[0]
	}

	// Sort by success rate (descending) and RTT (ascending)
	sort.Slice(viable, func(i, j int) bool {
		mi := mt.peers[viable[i]]
		mj := mt.peers[viable[j]]

		// Peers without metrics go to the end
		if mi == nil {
			return false
		}
		if mj == nil {
			return true
		}

		// Calculate success rates
		ratei := 0.0
		if mi.totalRequests > 0 {
			ratei = float64(mi.successCount) / float64(mi.totalRequests)
		}
		ratej := 0.0
		if mj.totalRequests > 0 {
			ratej = float64(mj.successCount) / float64(mj.totalRequests)
		}

		// If success rates differ significantly, prefer higher success rate
		if ratei-ratej > 0.1 {
			return true
		}
		if ratej-ratei > 0.1 {
			return false
		}

		// Otherwise, prefer lower RTT
		return mi.avgRTT < mj.avgRTT
	})

	return viable[0]
}

// shouldBackoffPeer checks if a peer should be temporarily avoided.
func (mt *metricsTracker) shouldBackoffPeer(peerID string) bool {
	mt.mu.Lock()
	defer mt.mu.Unlock()
	return mt.shouldBackoffPeerLocked(peerID)
}

// shouldBackoffPeerLocked is the internal version assuming lock is held.
func (mt *metricsTracker) shouldBackoffPeerLocked(peerID string) bool {
	m, ok := mt.peers[peerID]
	if !ok {
		return false // no metrics yet, don't backoff
	}

	// Backoff if:
	// 1. More than 3 consecutive failures
	// 2. Last timeout was within the last 30 seconds
	if m.consecutiveFailures > 3 {
		return true
	}

	if !m.lastTimeout.IsZero() && time.Since(m.lastTimeout) < 30*time.Second {
		return true
	}

	return false
}

// dumpMetrics returns a formatted string of all peer metrics for debugging.
func (mt *metricsTracker) dumpMetrics() string {
	mt.mu.Lock()
	defer mt.mu.Unlock()

	if len(mt.peers) == 0 {
		return "No peer metrics available"
	}

	// Sort peers by ID for consistent output
	peerIDs := make([]string, 0, len(mt.peers))
	for peerID := range mt.peers {
		peerIDs = append(peerIDs, peerID)
	}
	sort.Strings(peerIDs)

	result := "Peer Metrics:\n"
	for _, peerID := range peerIDs {
		m := mt.peers[peerID]
		successRate := 0.0
		if m.totalRequests > 0 {
			successRate = float64(m.successCount) / float64(m.totalRequests) * 100
		}

		result += fmt.Sprintf("  %s: requests=%d, success=%d (%.1f%%), fail=%d, avgRTT=%v, consecutiveFail=%d\n",
			peerID, m.totalRequests, m.successCount, successRate, m.failureCount, m.avgRTT, m.consecutiveFailures)
	}

	return result
}

// getPeerMetrics returns a copy of metrics for a specific peer.
// Returns nil if peer not found.
func (mt *metricsTracker) getPeerMetrics(peerID string) *peerMetrics {
	mt.mu.Lock()
	defer mt.mu.Unlock()

	m, ok := mt.peers[peerID]
	if !ok {
		return nil
	}

	// Return a copy to avoid data races
	copy := *m
	return &copy
}

// reset clears all metrics (useful for testing).
func (mt *metricsTracker) reset() {
	mt.mu.Lock()
	defer mt.mu.Unlock()
	mt.peers = make(map[string]*peerMetrics)
}
