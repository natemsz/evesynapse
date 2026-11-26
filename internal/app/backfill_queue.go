package app

import (
	"context"
	"sync"
)

// backfillQueue is a bounded queue of ids for one backfill worker,
// with each id on it at most once. The channel is made on first use.
// The zero value is ready to use.
type backfillQueue struct {
	mu     sync.Mutex
	ch     chan int64
	queued map[int64]bool
}

// channel returns the queue's channel, making it on first use.
func (q *backfillQueue) channel() chan int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.ch == nil {
		q.ch = make(chan int64, pilotBackfillQueueSize)
		q.queued = map[int64]bool{}
	}
	return q.ch
}

// offer queues each positive id not already queued. It never blocks:
// when the queue is full the id is dropped and full is called with it,
// and a later offer queues it again.
func (q *backfillQueue) offer(ids []int64, full func(id int64)) {
	ch := q.channel()
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		q.mu.Lock()
		if !q.queued[id] {
			select {
			case ch <- id:
				q.queued[id] = true
			default:
				full(id)
			}
		}
		q.mu.Unlock()
	}
}

// drain hands queued ids to handle one at a time until ctx ends. An
// id can be offered again once handle returns.
func (q *backfillQueue) drain(ctx context.Context, handle func(id int64)) {
	ch := q.channel()
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-ch:
			handle(id)
			q.mu.Lock()
			delete(q.queued, id)
			q.mu.Unlock()
		}
	}
}
