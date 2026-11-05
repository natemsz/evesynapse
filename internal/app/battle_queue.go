package app

// ---------------------------------------------------------------------------
// Battle reports, the game log queue.
//
// A game log is the largest thing the site takes in, so both ways in
// (the upload form and Axon's posts) go through one queue:
//
//   - Before a byte of the body is read, the account's hourly counts
//     (uploads, Axon posts, bytes received) and its place in the
//     queue are checked, and the queue's own room. A request
//     carries at most battleWireMax; the page gzips a file before
//     sending it and Axon always does.
//   - The body is read into memory, its owner checked from the log's
//     header, and the log waits in the queue. The account's hourly
//     budget of text (the size the log unpacks to) is charged then.
//   - A fixed set of readers (BATTLE_LOG_READERS, by default three
//     eighths of the cores) takes logs one account at a time, round
//     robin, so one account's pile never holds up another's. A log
//     of typical size reads in tens of milliseconds; the line
//     stays short.
//
// The queue is bounded by the text waiting (the readers' work) and
// the bytes held; past either, a log is refused with a time to come
// back. Nothing is written to disk: a restart drops what waits, and
// the page and Axon send it again. Results stay an hour for the
// page to show.
// ---------------------------------------------------------------------------

import (
	"context"
	"encoding/binary"
	"fmt"
	"runtime"
	"sync"
	"time"
)

const (
	battleWireMax        = 8 << 20   // the most one request carries
	battleUploadsPerHour = 30        // the upload form, per account
	battlePostsPerHour   = 120       // Axon's posts, per account
	battleBytesPerHour   = 96 << 20  // received per account per hour
	battleTextPerHour    = 1 << 30   // unpacked text per account per hour
	battleWaitingPerUser = 4         // an account's logs in the queue
	battleQueueText      = 2 << 30   // text waiting, all accounts
	battleQueueBytes     = 512 << 20 // bodies held, all accounts
	battleFightsPerLog   = 50        // fights stored from one log
	battleResultsKept    = 10        // results shown per account
	battleReadTimeout    = 2 * time.Minute
)

// battleJob is one log waiting to be read.
type battleJob struct {
	userID   int64
	charID   int64
	name     string // the file's name, or "Pasted log"
	session  string
	listener string // the pilot's name, for a log whose header has none
	body     []byte // the log as sent, gzipped or not
	text     int64  // what it unpacks to, as far as it says
	queued   time.Time
}

// battleResult is what reading one log came to, for the page.
type battleResult struct {
	Name   string
	At     time.Time
	Text   string
	IDs    []int64
	Failed bool
	read   int64 // text actually read, to settle the account's charge
}

type battleAccount struct {
	uploads, posts []time.Time
	bytes, text    []battleBytes
	waiting        []*battleJob
	reading        *battleJob
	results        []battleResult
}

type battleBytes struct {
	at time.Time
	n  int64
}

// battleQueue holds the logs waiting and each account's counts.
type battleQueue struct {
	mu        sync.Mutex
	accounts  map[int64]*battleAccount
	turn      []int64 // accounts with logs waiting, in reading order
	text      int64
	bytes     int64
	wake      chan struct{}
	lastSweep time.Time
}

func newBattleQueue() *battleQueue {
	return &battleQueue{accounts: map[int64]*battleAccount{}, wake: make(chan struct{}, 1)}
}

// battleQueue is the app's queue, made on first use.
func (app *Application) battleQueue() *battleQueue {
	app.battleQueueOnce.Do(func() { app.battleLogs = newBattleQueue() })
	return app.battleLogs
}

// battleLogReaders is how many logs are read at once: the configured
// count, else three eighths of the cores, at least two.
func battleLogReaders(configured int) int {
	if configured > 0 {
		return configured
	}
	return max(2, runtime.NumCPU()*3/8)
}

func (q *battleQueue) account(userID int64) *battleAccount {
	a := q.accounts[userID]
	if a == nil {
		a = &battleAccount{}
		q.accounts[userID] = a
	}
	return a
}

// admit checks a request before its body is read: size is its
// Content-Length (an unknown one is charged the wire limit). It
// counts the attempt, or says why not and when to come back.
func (q *battleQueue) admit(userID int64, axon bool, size int64, now time.Time) (string, time.Duration) {
	if size < 0 || size > battleWireMax {
		size = battleWireMax
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.sweep(now)
	a := q.account(userID)
	a.prune(now)
	count, limit, what := &a.uploads, battleUploadsPerHour, "uploads"
	if axon {
		count, limit, what = &a.posts, battlePostsPerHour, "game log posts"
	}
	switch {
	case len(a.waiting) >= battleWaitingPerUser:
		return fmt.Sprintf("%d of your logs are already waiting to be read", len(a.waiting)), time.Minute
	case len(*count) >= limit:
		return fmt.Sprintf("%d %s an hour at most", limit, what), (*count)[0].Add(time.Hour).Sub(now)
	case sum(a.bytes)+size > battleBytesPerHour:
		return fmt.Sprintf("%d MB of game logs an hour at most", battleBytesPerHour>>20), a.bytes[0].at.Add(time.Hour).Sub(now)
	case sum(a.text) >= battleTextPerHour:
		return fmt.Sprintf("%d MB of game log text an hour at most", battleTextPerHour>>20), a.text[0].at.Add(time.Hour).Sub(now)
	case q.bytes+size > battleQueueBytes:
		return "the site has many logs to read right now", 2 * time.Minute
	}
	*count = append(*count, now)
	a.bytes = append(a.bytes, battleBytes{now, size})
	return "", 0
}

// enqueue sets a read body waiting and charges its text. It returns
// how many logs are ahead of it, or why not.
func (q *battleQueue) enqueue(j *battleJob, now time.Time) (int, string, time.Duration) {
	j.text = battleTextSize(j.body)
	j.queued = now
	q.mu.Lock()
	defer q.mu.Unlock()
	a := q.account(j.userID)
	a.prune(now)
	switch {
	case len(a.waiting) >= battleWaitingPerUser:
		return 0, fmt.Sprintf("%d of your logs are already waiting to be read", len(a.waiting)), time.Minute
	case sum(a.text)+j.text > battleTextPerHour:
		return 0, fmt.Sprintf("%d MB of game log text an hour at most", battleTextPerHour>>20), time.Hour
	case q.text+j.text > battleQueueText || q.bytes+int64(len(j.body)) > battleQueueBytes:
		return 0, "the site has many logs to read right now", 2 * time.Minute
	}
	a.text = append(a.text, battleBytes{now, j.text})
	if len(a.waiting) == 0 && a.reading == nil {
		q.turn = append(q.turn, j.userID)
	}
	a.waiting = append(a.waiting, j)
	q.text += j.text
	q.bytes += int64(len(j.body))
	ahead := 0
	for _, o := range q.accounts {
		ahead += len(o.waiting)
		if o.reading != nil {
			ahead++
		}
	}
	select {
	case q.wake <- struct{}{}:
	default:
	}
	return ahead - 1, "", 0
}

// next takes the next account's oldest log, round robin.
func (q *battleQueue) next() *battleJob {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.turn) > 0 {
		id := q.turn[0]
		q.turn = q.turn[1:]
		a := q.accounts[id]
		if a == nil || len(a.waiting) == 0 || a.reading != nil {
			continue
		}
		j := a.waiting[0]
		a.waiting = a.waiting[1:]
		a.reading = j
		return j
	}
	return nil
}

// done files a log's result and puts its account back in line if
// more of its logs wait.
func (q *battleQueue) done(j *battleJob, res battleResult) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.text -= j.text
	q.bytes -= int64(len(j.body))
	a := q.account(j.userID)
	a.reading = nil
	// A gzip stream may claim less text than it holds: charge what
	// was read beyond the claim.
	if extra := res.read - j.text; extra > 0 {
		a.text = append(a.text, battleBytes{time.Now(), extra})
	}
	a.results = append(a.results, res)
	if over := len(a.results) - battleResultsKept; over > 0 {
		a.results = a.results[over:]
	}
	if len(a.waiting) > 0 {
		q.turn = append(q.turn, j.userID)
		select {
		case q.wake <- struct{}{}:
		default:
		}
	}
}

// run reads logs with n readers until ctx ends.
func (q *battleQueue) run(ctx context.Context, n int, read func(context.Context, *battleJob) battleResult) {
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if j := q.next(); j != nil {
					q.read(ctx, j, read)
					continue
				}
				select {
				case <-ctx.Done():
					return
				case <-q.wake:
				case <-time.After(5 * time.Second):
				}
			}
		}()
	}
	wg.Wait()
}

// drain reads every waiting log in the calling goroutine.
func (q *battleQueue) drain(ctx context.Context, read func(context.Context, *battleJob) battleResult) {
	for j := q.next(); j != nil; j = q.next() {
		q.read(ctx, j, read)
	}
}

func (q *battleQueue) read(ctx context.Context, j *battleJob, read func(context.Context, *battleJob) battleResult) {
	rctx, cancel := context.WithTimeout(ctx, battleReadTimeout)
	defer cancel()
	res := read(rctx, j)
	res.Name, res.At = j.name, time.Now()
	j.body = nil
	q.done(j, res)
}

// battleQueueView is an account's place in the queue, for the page.
type battleQueueView struct {
	Waiting []battleWaitingRow
	Results []battleResult
}

type battleWaitingRow struct {
	Name    string
	Reading bool
	Ahead   int
}

// status is the account's logs waiting and read in the last hour.
func (q *battleQueue) status(userID int64, now time.Time) battleQueueView {
	q.mu.Lock()
	defer q.mu.Unlock()
	var v battleQueueView
	a := q.accounts[userID]
	if a == nil {
		return v
	}
	if a.reading != nil {
		v.Waiting = append(v.Waiting, battleWaitingRow{Name: a.reading.name, Reading: true})
	}
	for _, j := range a.waiting {
		ahead := 0
		for _, o := range q.accounts {
			for _, w := range o.waiting {
				if w.queued.Before(j.queued) {
					ahead++
				}
			}
		}
		v.Waiting = append(v.Waiting, battleWaitingRow{Name: j.name, Ahead: ahead})
	}
	for i := len(a.results) - 1; i >= 0; i-- {
		if now.Sub(a.results[i].At) < time.Hour {
			v.Results = append(v.Results, a.results[i])
		}
	}
	return v
}

func (a *battleAccount) prune(now time.Time) {
	cut := now.Add(-time.Hour)
	times := func(ts []time.Time) []time.Time {
		i := 0
		for i < len(ts) && !ts[i].After(cut) {
			i++
		}
		return ts[i:]
	}
	sizes := func(bs []battleBytes) []battleBytes {
		i := 0
		for i < len(bs) && !bs[i].at.After(cut) {
			i++
		}
		return bs[i:]
	}
	a.uploads, a.posts = times(a.uploads), times(a.posts)
	a.bytes, a.text = sizes(a.bytes), sizes(a.text)
	i := 0
	for i < len(a.results) && !a.results[i].At.After(cut) {
		i++
	}
	a.results = a.results[i:]
}

// sweep drops idle accounts, at most once a minute.
func (q *battleQueue) sweep(now time.Time) {
	if now.Sub(q.lastSweep) < time.Minute {
		return
	}
	q.lastSweep = now
	for id, a := range q.accounts {
		a.prune(now)
		if a.reading == nil && len(a.waiting) == 0 && len(a.uploads)+len(a.posts)+len(a.bytes)+len(a.text)+len(a.results) == 0 {
			delete(q.accounts, id)
		}
	}
}

func sum(bs []battleBytes) int64 {
	var n int64
	for _, b := range bs {
		n += b.n
	}
	return n
}

// battleTextSize is what a body unpacks to: a gzip stream says so in
// its last four bytes (capped at the reader's limit; the reader
// enforces it whatever the stream claims), plain text is its length.
func battleTextSize(b []byte) int64 {
	const most = 32 << 20
	if len(b) >= 18 && b[0] == 0x1F && b[1] == 0x8B {
		return min(int64(binary.LittleEndian.Uint32(b[len(b)-4:])), most)
	}
	return min(int64(len(b)), most)
}
