package app

import (
	"context"
	"sync"
	"sync/atomic"

	db "evesynapse/internal/db/sqlc"
)

// The worker refreshes several characters at once. Each character is
// still worked through by one goroutine from start to finish, in the
// order it always was, so nothing about one character's pass changed;
// what changed is that one slow answer from ESI no longer holds up
// everybody behind it. Everything a character's pass stores is keyed by
// that character, and what the passes share (the fetch allowance, the
// ESI client, the name caches) was already safe to use from several
// goroutines, because pages fetch alongside the worker.

// Bounds on WORKER_LANES, and what it is when unset. ESI's rate limits
// are per character, so lanes do not add up against one budget; what
// they share is the application's error limit and the database pool.
const (
	defaultWorkerLanes = 4
	mostWorkerLanes    = 16
)

// fetchesPerLane sizes the default fetch allowance: what one lane,
// fetching one after another at about 0.3 seconds each, gets through
// in well under the cycle's minute.
const fetchesPerLane = 120

// parseWorkerLanes reads WORKER_LANES: the default when unset or
// unreadable, else the number kept between 1 and mostWorkerLanes.
func parseWorkerLanes(raw string) int {
	return intSetting("WORKER_LANES", raw, 1, mostWorkerLanes, defaultWorkerLanes)
}

// workerLanes is how many characters the worker refreshes at once. A
// Config that was not loaded from the environment (the test fixtures)
// has one lane: the cycle in its old order, one character at a time.
func (app *Application) workerLanes() int {
	if app.cfg.workerLanes > 0 {
		return app.cfg.workerLanes
	}
	return 1
}

// keyedLocks is one lock per id, made the first time the id is locked.
// They are never dropped: one small lock per character ever seen.
type keyedLocks struct {
	mu    sync.Mutex
	locks map[int64]*sync.Mutex
}

// lock takes id's lock and returns what releases it.
func (k *keyedLocks) lock(id int64) (unlock func()) {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = map[int64]*sync.Mutex{}
	}
	l := k.locks[id]
	if l == nil {
		l = &sync.Mutex{}
		k.locks[id] = l
	}
	k.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// refreshCharacters is the cycle's character pass: every character in
// the order given, workerLanes of them at a time. It stops handing out
// characters once the fetch allowance is spent (the rest are counted as
// deferred and keep their place in the due order), once ESI says to
// back off, or when ctx ends; characters already started finish their
// current request and stop at their next one.
func (c *cycleState) refreshCharacters(ctx context.Context, characters []db.Character) {
	lanes := c.app.workerLanes()
	if lanes > len(characters) {
		lanes = len(characters)
	}
	jobs := make(chan db.Character)
	var merge sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < lanes; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ch := range jobs {
				one := c.forCharacter()
				one.refreshCharacterGuarded(ctx, ch)
				merge.Lock()
				c.absorb(one)
				merge.Unlock()
			}
		}()
	}
	handed := 0
feed:
	for _, ch := range characters {
		if ctx.Err() != nil || c.halt.Load() {
			break
		}
		if c.allowance.exhausted() {
			c.deferred = len(characters) - handed
			break
		}
		select {
		case jobs <- ch:
			handed++
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
}

// forCharacter is a cycle state for one character's pass: its own
// counts, and the cycle's allowance and stop signal.
func (c *cycleState) forCharacter() *cycleState {
	return &cycleState{app: c.app, allowance: c.allowance, halt: c.halt}
}

// absorb adds one character's counts to the cycle's.
func (c *cycleState) absorb(one *cycleState) {
	c.refreshed += one.refreshed
	c.namesResolved += one.namesResolved
	c.failed += one.failed
	c.rateHeld += one.rateHeld
	c.limited = c.limited || one.limited
}

// refreshCharacterGuarded is refreshCharacter under the worker's panic
// guard. The guard sits per character: a lane that died would stop
// taking characters and leave the cycle waiting on it.
func (c *cycleState) refreshCharacterGuarded(ctx context.Context, ch db.Character) {
	defer recoverWorkerPanic("character refresh")
	c.refreshCharacter(ctx, ch)
}

// newHalt is a cycle's stop signal, not yet given.
func newHalt() *atomic.Bool { return new(atomic.Bool) }
