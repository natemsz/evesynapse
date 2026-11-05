package app

// ---------------------------------------------------------------------------
// Battle reports, limits on reading game logs.
//
// A game log is the largest thing the site takes in, so both ways in
// (the upload form and Axon's posts) pass the same gate before a byte
// of the body is read:
//
//   - a request carries at most battleWireMax; the page gzips a file
//     before sending it and Axon always does, so a 30 MB session
//     travels as a few MB, and the text unpacks to at most
//     gamelog.MaxText;
//   - each account has one log in flight at a time, an hourly count
//     of uploads and of Axon posts, and an hourly budget of bytes
//     received, shared by both ways in; an attempt counts whether
//     it succeeds or not;
//   - reading a log and storing its fights takes one of
//     battleParseSlots, server wide; a request waits in a short
//     line for one, and a full line or a long wait is answered 503
//     with Retry-After instead of growing a queue.
//
// The counts live in memory (one app box); a restart forgets them.
// ---------------------------------------------------------------------------

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	battleWireMax        = 8 << 20  // the most one request carries
	battleUploadsPerHour = 12       // the upload form, per account
	battlePostsPerHour   = 120      // Axon's posts, per account
	battleBytesPerHour   = 96 << 20 // received per account per hour, both ways
	battleParseSlots     = 2        // logs read at once, server wide
	battleParseLine      = 8        // requests waiting for a slot
	battleParseWait      = 20 * time.Second
	battleFightsPerLog   = 50 // fights stored from one log
)

var errBattleBusy = errors.New("battle reports: every reading slot is taken")

// battleGate admits game logs.
type battleGate struct {
	mu        sync.Mutex
	use       map[int64]*battleUse
	lastSweep time.Time
	slots     chan struct{}
	waiting   int
}

type battleUse struct {
	busy    bool
	uploads []time.Time
	posts   []time.Time
	bytes   []battleBytes
}

type battleBytes struct {
	at time.Time
	n  int64
}

var battleLogGate = newBattleGate()

func newBattleGate() *battleGate {
	return &battleGate{use: map[int64]*battleUse{}, slots: make(chan struct{}, battleParseSlots)}
}

// admit lets one request from the account carry size bytes (its
// Content-Length; an unknown length is charged the wire limit). It
// returns a release to call when the request is done, or why not
// and when to try again.
func (g *battleGate) admit(userID int64, axon bool, size int64, now time.Time) (func(), string, time.Duration) {
	if size < 0 || size > battleWireMax {
		size = battleWireMax
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sweep(now)
	u := g.use[userID]
	if u == nil {
		u = &battleUse{}
		g.use[userID] = u
	}
	u.prune(now)
	count, limit, what := &u.uploads, battleUploadsPerHour, "uploads"
	if axon {
		count, limit, what = &u.posts, battlePostsPerHour, "game log posts"
	}
	var sent int64
	for _, b := range u.bytes {
		sent += b.n
	}
	switch {
	case u.busy:
		return nil, "one game log at a time: the last one is still being read", 30 * time.Second
	case len(*count) >= limit:
		return nil, fmt.Sprintf("%d %s an hour at most", limit, what), (*count)[0].Add(time.Hour).Sub(now)
	case sent+size > battleBytesPerHour:
		return nil, fmt.Sprintf("%d MB of game logs an hour at most", battleBytesPerHour>>20), u.bytes[0].at.Add(time.Hour).Sub(now)
	}
	u.busy = true
	*count = append(*count, now)
	u.bytes = append(u.bytes, battleBytes{now, size})
	return func() {
		g.mu.Lock()
		u.busy = false
		g.mu.Unlock()
	}, "", 0
}

// read runs fn in a reading slot, after a bounded wait for one.
func (g *battleGate) read(ctx context.Context, fn func() error) error {
	g.mu.Lock()
	if g.waiting >= battleParseLine {
		g.mu.Unlock()
		return errBattleBusy
	}
	g.waiting++
	g.mu.Unlock()
	t := time.NewTimer(battleParseWait)
	defer t.Stop()
	var err error
	select {
	case g.slots <- struct{}{}:
	case <-t.C:
		err = errBattleBusy
	case <-ctx.Done():
		err = ctx.Err()
	}
	g.mu.Lock()
	g.waiting--
	g.mu.Unlock()
	if err != nil {
		return err
	}
	defer func() { <-g.slots }()
	return fn()
}

func (u *battleUse) prune(now time.Time) {
	cut := now.Add(-time.Hour)
	keep := func(ts []time.Time) []time.Time {
		i := 0
		for i < len(ts) && !ts[i].After(cut) {
			i++
		}
		return ts[i:]
	}
	u.uploads, u.posts = keep(u.uploads), keep(u.posts)
	i := 0
	for i < len(u.bytes) && !u.bytes[i].at.After(cut) {
		i++
	}
	u.bytes = u.bytes[i:]
}

// sweep drops idle accounts' entries, at most once a minute.
func (g *battleGate) sweep(now time.Time) {
	if now.Sub(g.lastSweep) < time.Minute {
		return
	}
	g.lastSweep = now
	for id, u := range g.use {
		u.prune(now)
		if !u.busy && len(u.uploads) == 0 && len(u.posts) == 0 && len(u.bytes) == 0 {
			delete(g.use, id)
		}
	}
}

// retryAfter sets Retry-After in whole seconds, at least one.
func retryAfter(w http.ResponseWriter, d time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(max(1, int(d.Seconds()+0.999))))
}
