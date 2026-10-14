package app

import (
	"context"
	"sync"
	"time"
)

// Peer calls in a worker cycle (the gossip round, the pull, the seed
// heartbeats) run a few at a time under one deadline for the whole
// round. One at a time, each with its own 15 s client timeout, N
// slow peers could stretch a cycle by N x 15 s; now a round costs at
// most swarmRoundDeadline whatever the peers do.
const swarmParallelPeers = 4

// swarmRoundDeadline is a var so tests can shorten it.
var swarmRoundDeadline = 20 * time.Second

// swarmFanOut calls fn for 0..n-1, at most swarmParallelPeers at
// once, all under one context that ends swarmRoundDeadline from now
// (or sooner with ctx). fn must only do network work and record its
// result in its own slot: the caller handles the results afterwards,
// one at a time.
func swarmFanOut(ctx context.Context, n int, fn func(ctx context.Context, i int)) {
	if n <= 0 {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, swarmRoundDeadline)
	defer cancel()
	slots := make(chan struct{}, swarmParallelPeers)
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
