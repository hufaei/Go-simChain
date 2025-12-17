package peer

import (
	"math/big"
	"sync"
	"time"

	"simchain-go/internal/types"
)

// Info is the local node's view of a peer.
// In V3-A (inproc), a peer is identified by nodeID; in V3-B (tcp) it can map to a connection.
type Info struct {
	ID string

	// Last advertised tip.
	TipHeight uint64
	TipHash   types.Hash
	CumWork   *big.Int

	// Observability / selection hints.
	LastRTT        time.Duration
	Timeouts       int
	Errors         int
	CooldownUntil  time.Time
	LastSuccessful time.Time
	LastUpdated    time.Time
}

type Manager struct {
	mu    sync.Mutex
	peers map[string]*Info

	// If a peer times out/errors, we back off for this duration.
	backoff time.Duration
}

func NewManager(backoff time.Duration) *Manager {
	if backoff <= 0 {
		backoff = 800 * time.Millisecond
	}
	return &Manager{
		peers:   make(map[string]*Info),
		backoff: backoff,
	}
}

func (m *Manager) Upsert(id string) {
	if id == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.peers[id]; ok {
		return
	}
	m.peers[id] = &Info{ID: id, CumWork: big.NewInt(0)}
}

func (m *Manager) UpdateTip(id string, height uint64, tipHash types.Hash, cumWork *big.Int, rtt time.Duration) {
	if id == "" || cumWork == nil {
		return
	}
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.ensureLocked(id)
	p.TipHeight = height
	p.TipHash = tipHash
	p.CumWork = new(big.Int).Set(cumWork)
	p.LastRTT = rtt
	p.LastUpdated = now
	p.LastSuccessful = now
	if p.CooldownUntil.Before(now) {
		p.CooldownUntil = time.Time{}
	}
}

func (m *Manager) ReportTimeout(id string) {
	m.reportFailure(id, true)
}

func (m *Manager) ReportError(id string) {
	m.reportFailure(id, false)
}

func (m *Manager) reportFailure(id string, timeout bool) {
	if id == "" {
		return
	}
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.ensureLocked(id)
	if timeout {
		p.Timeouts++
	} else {
		p.Errors++
	}
	p.LastUpdated = now
	p.CooldownUntil = now.Add(m.backoff)
}

func (m *Manager) BestPeer(now time.Time) (id string, height uint64, tipHash types.Hash, cumWork *big.Int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var best *Info
	for _, p := range m.peers {
		if !p.CooldownUntil.IsZero() && p.CooldownUntil.After(now) {
			continue
		}
		if best == nil {
			best = p
			continue
		}
		// Prefer higher cumulative work.
		if p.CumWork != nil && best.CumWork != nil {
			if p.CumWork.Cmp(best.CumWork) > 0 {
				best = p
				continue
			}
			if p.CumWork.Cmp(best.CumWork) < 0 {
				continue
			}
		}
		// Tie-breaker: higher height.
		if p.TipHeight > best.TipHeight {
			best = p
			continue
		}
		if p.TipHeight < best.TipHeight {
			continue
		}
		// Final tie-breaker: lower RTT (if known).
		if best.LastRTT == 0 || (p.LastRTT > 0 && p.LastRTT < best.LastRTT) {
			best = p
		}
	}

	if best == nil {
		return "", 0, types.Hash{}, nil
	}
	return best.ID, best.TipHeight, best.TipHash, new(big.Int).Set(best.CumWork)
}

func (m *Manager) Snapshot() []Info {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Info, 0, len(m.peers))
	for _, p := range m.peers {
		cp := *p
		if p.CumWork != nil {
			cp.CumWork = new(big.Int).Set(p.CumWork)
		}
		out = append(out, cp)
	}
	return out
}

func (m *Manager) ensureLocked(id string) *Info {
	p, ok := m.peers[id]
	if ok {
		return p
	}
	p = &Info{ID: id, CumWork: big.NewInt(0)}
	m.peers[id] = p
	return p
}
