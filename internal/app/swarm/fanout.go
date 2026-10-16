package swarm

import (
	"context"
	"sync"
	"time"
)

// Peer calls in a worker cycle (the gossip round, the pull, the seed
// heartbeats) run a few at a time under one deadline for the whole
// round. One at a time, each with its own 15 s client timeout, N
// slow peers could stretch a cycle by N x 15 s; now a round costs at
// most SwarmRoundDeadline whatever the peers do.
const SwarmParallelPeers = 4

// SwarmRoundDeadline is a var so tests can shorten it.
var SwarmRoundDeadline = 20 * time.Second

// SwarmFanOut calls fn for 0..n-1, at most SwarmParallelPeers at
// once, all under one context that ends SwarmRoundDeadline from now
// (or sooner with ctx). fn must only do network work and record its
// result in its own slot: the caller handles the results afterwards,
// one at a time.
func SwarmFanOut(ctx context.Context, n int, fn func(ctx context.Context, i int)) {
	if n <= 0 {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, SwarmRoundDeadline)
	defer cancel()
	slots := make(chan struct{}, SwarmParallelPeers)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		select {
		case slots <- struct{}{}:
		case <-rctx.Done():
			// Out of time: the rest are not started. fn sees the
			// ended context for a slot it never got, so each slot
			// still records an error.
			fn(rctx, i)
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-slots }()
			fn(rctx, i)
		}(i)
	}
	wg.Wait()
}
