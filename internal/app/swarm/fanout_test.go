package swarm

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestSwarmFanOutBounds: every peer is called, never more than
// SwarmParallelPeers at once.
func TestSwarmFanOutBounds(t *testing.T) {
	var now, peak, calls atomic.Int64
	SwarmFanOut(context.Background(), 10, func(ctx context.Context, i int) {
		calls.Add(1)
		cur := now.Add(1)
		for {
			p := peak.Load()
			if cur <= p || peak.CompareAndSwap(p, cur) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		now.Add(-1)
	})
	if calls.Load() != 10 {
		t.Errorf("called %d peers, want 10", calls.Load())
	}
	if p := peak.Load(); p > SwarmParallelPeers || p < 2 {
		t.Errorf("peak concurrency %d, want 2..%d", p, SwarmParallelPeers)
	}
}

// TestSwarmFanOutSharedDeadline: N peers that never answer cost one
// round deadline, not N timeouts, and each sees its context end.
func TestSwarmFanOutSharedDeadline(t *testing.T) {
	old := SwarmRoundDeadline
	SwarmRoundDeadline = 100 * time.Millisecond
	defer func() { SwarmRoundDeadline = old }()

	var ended atomic.Int64
	start := time.Now()
	SwarmFanOut(context.Background(), 12, func(ctx context.Context, i int) {
		<-ctx.Done() // a peer that never answers
		ended.Add(1)
	})
	if took := time.Since(start); took > time.Second {
		t.Errorf("12 silent peers took %v, want about one 100ms deadline", took)
	}
	if ended.Load() != 12 {
		t.Errorf("%d peers saw the deadline, want all 12", ended.Load())
	}
}
