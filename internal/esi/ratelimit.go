package esi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// ESI's rate limits. There are two, and they are not the same kind of
// thing:
//
// The error limit (status 420) is one budget for the whole
// application: too many failed requests, and every request is refused
// for the rest of the minute. One 420 means stop everything.
//
// The rate limit (status 429) is a budget of tokens per group of
// routes, per application, per character. Running one character's
// budget out says nothing about any other character's. One 429 means
// leave that character alone for as long as Retry-After says.
//
// Both wrap ErrErrorLimit, so code that stops a pass on a limit keeps
// doing so. A 429 also wraps ErrRateLimited, and the character it was
// about is remembered, so that a caller working through many
// characters can set that one aside and carry on with the rest.
//
// Both are also seen coming, and that is what governs how fast the
// app calls ESI, rather than fixed per-pass counts. Every response
// says which group its route is in and how many tokens are left; the
// client learns route -> group, and before each request estimates
// the bucket's tokens (what was left, plus what the window has given
// back since). A bucket down to its reserve, or an error budget down
// to its last few, is refused here, with the same errors ESI would
// have answered, before ESI ever has to refuse it.
// ---------------------------------------------------------------------------

// ErrRateLimited marks a 429: the rate limit of one character (or of
// the application's unauthenticated requests), not of everything.
var ErrRateLimited = errors.New("ESI rate limit")

const (
	// rateLimitDefaultWait is how long a character is left alone
	// after a 429 that named no Retry-After, and rateLimitMaxWait the
	// longest any Retry-After is honoured for (the window is 15
	// minutes; anything longer is a mistake somewhere).
	rateLimitDefaultWait = time.Minute
	rateLimitMaxWait     = 15 * time.Minute

	// rateLowWater: when a character's budget in some group is down to
	// this few tokens, the character is left alone for rateLowWait
	// before ESI has to refuse anything. CCP asks for exactly this:
	// slow down as the remaining tokens approach zero, do not operate
	// at the limit. Six tokens is three successful requests, or one
	// refused one; the smallest budgets seen are 30 tokens. Tokens
	// come back as the window slides, so a minute's pause is usually
	// enough and is tried again after it.
	rateLowWater = 6
	rateLowWait  = time.Minute
)

type characterKey struct{}

// WithCharacter marks the requests made with ctx as being on behalf of
// one character, which is who a 429 answer to them is about.
func WithCharacter(ctx context.Context, characterID int64) context.Context {
	return context.WithValue(ctx, characterKey{}, characterID)
}

func characterFrom(ctx context.Context) int64 {
	id, _ := ctx.Value(characterKey{}).(int64)
	return id
}

// RateHeadroom is the lowest a rate-limit budget was seen at lately:
// for one group of routes, how much was left and for which character.
type RateHeadroom struct {
	Group       string
	CharacterID int64
	Remaining   int
	Limit       string // as ESI states it, e.g. "150/15m"; "" if not stated
	At          time.Time
}

// rateLimits is what the client remembers about ESI's rate limits.
type rateLimits struct {
	mu     sync.Mutex
	until  map[int64]time.Time     // character (0: unauthenticated) -> left alone until
	groups map[string]RateHeadroom // group -> the lowest budget seen since last read

	// routeGroup is which rate-limit group each route is in, as ESI
	// named it (route: the path with its numbers as {id}).
	routeGroup map[string]string
	// buckets is each group's latest state for each character
	// (0: unauthenticated), what the pre-request check reads.
	buckets map[bucketKey]bucketState
	// routeHold is a route ESI answered 429 for, per character, held
	// until its Retry-After (keyed by route in place of group).
	routeHold map[bucketKey]time.Time
}

type bucketKey struct {
	group     string
	character int64
}

// bucketState is one bucket as of its latest response.
type bucketState struct {
	remaining int
	limit     int           // tokens per window; 0 when not stated
	window    time.Duration // 0 when not stated
	at        time.Time
}

// rateReserveShare is how much of a bucket is kept back: requests stop
// when the estimated tokens fall to a tenth of the limit (and never
// below rateLowWater), before ESI would refuse anything.
const rateReserveShare = 0.10

// reserve is the tokens a bucket keeps back.
func (b bucketState) reserve() int {
	r := int(float64(b.limit) * rateReserveShare)
	if r < rateLowWater {
		r = rateLowWater
	}
	return r
}

// estimate is the bucket's tokens now: what was left at its latest
// response, plus the share of the limit the window has given back
// since (ESI's buckets refill as the window slides). Without a stated
// limit and window only the latest count is known.
func (b bucketState) estimate(now time.Time) int {
	if b.limit <= 0 || b.window <= 0 {
		return b.remaining
	}
	back := int(float64(b.limit) * float64(now.Sub(b.at)) / float64(b.window))
	if n := b.remaining + back; n < b.limit {
		return n
	}
	return b.limit
}

// parseRateLimit reads ESI's X-Ratelimit-Limit, e.g. "150/15m": tokens
// per window. ok is false when it is missing or not in that form.
func parseRateLimit(v string) (tokens int, window time.Duration, ok bool) {
	n, w, found := strings.Cut(strings.TrimSpace(v), "/")
	if !found {
		return 0, 0, false
	}
	tokens, err := strconv.Atoi(strings.TrimSpace(n))
	if err != nil || tokens <= 0 {
		return 0, 0, false
	}
	w = strings.TrimSpace(w)
	if w == "" {
		return 0, 0, false
	}
	unit := map[byte]time.Duration{'s': time.Second, 'm': time.Minute, 'h': time.Hour}[w[len(w)-1]]
	count, err := strconv.Atoi(w[:len(w)-1])
	if unit == 0 || err != nil || count <= 0 {
		return 0, 0, false
	}
	return tokens, time.Duration(count) * unit, true
}

// routeOf is a request path as a route: no query, numbers as {id}, so
// every character's wallet is one route.
func routeOf(method, path string) string {
	path, _, _ = strings.Cut(path, "?")
	parts := strings.Split(path, "/")
	for i, part := range parts {
		if part != "" && strings.Trim(part, "0123456789") == "" {
			parts[i] = "{id}"
		}
	}
	return method + " " + strings.Join(parts, "/")
}

// learnBucket records a response's bucket and the route's group.
// Callers hold c.rates.mu.
func (c *Client) learnBucket(route string, characterID int64, group string, remaining int, header http.Header, now time.Time) {
	if group == "" {
		return
	}
	if c.rates.routeGroup == nil {
		c.rates.routeGroup = map[string]string{}
		c.rates.buckets = map[bucketKey]bucketState{}
	}
	if _, ok := c.rates.routeGroup[route]; ok || len(c.rates.routeGroup) < 2000 {
		c.rates.routeGroup[route] = group
	}
	state := bucketState{remaining: remaining, at: now}
	if tokens, window, ok := parseRateLimit(header.Get("X-Ratelimit-Limit")); ok {
		state.limit, state.window = tokens, window
	}
	key := bucketKey{group, characterID}
	if _, ok := c.rates.buckets[key]; ok || len(c.rates.buckets) < 50000 {
		c.rates.buckets[key] = state
	}
}

// preflight refuses a request ESI would soon refuse: the error budget
// at its last few, or the route's bucket for this character down to its
// reserve. The errors are the ones ESI's own refusals carry, so every
// caller backs off the way it already does; nothing is sent.
func (c *Client) preflight(ctx context.Context, method, path string, now time.Time) error {
	if c.ErrorBudgetLow() {
		return fmt.Errorf("ESI %s %s: error budget nearly spent, holding until it resets: %w", method, path, ErrErrorLimit)
	}
	characterID := characterFrom(ctx)
	route := routeOf(method, path)
	c.rates.mu.Lock()
	defer c.rates.mu.Unlock()
	if until, held := c.rates.routeHold[bucketKey{route, characterID}]; held {
		if now.Before(until) {
			return fmt.Errorf("ESI %s %s: rate limited until %s: %w (%w)", method, path, until.UTC().Format("15:04:05"), ErrErrorLimit, ErrRateLimited)
		}
		delete(c.rates.routeHold, bucketKey{route, characterID})
	}
	group, known := c.rates.routeGroup[route]
	if !known {
		return nil
	}
	b, seen := c.rates.buckets[bucketKey{group, characterID}]
	if !seen || b.estimate(now) > b.reserve() {
		return nil
	}
	// Held for as long as the window takes to give the reserve back.
	wait := rateLowWait
	if b.limit > 0 && b.window > 0 {
		wait = time.Duration(float64(b.window) * float64(b.reserve()) / float64(b.limit))
		if wait < 5*time.Second {
			wait = 5 * time.Second
		}
	}
	if c.rates.until == nil {
		c.rates.until = map[int64]time.Time{}
	}
	if _, ok := c.rates.until[characterID]; !ok && len(c.rates.until) >= 50000 {
		for id, u := range c.rates.until {
			if !u.After(now) {
				delete(c.rates.until, id)
			}
		}
	}
	if until := now.Add(wait); until.After(c.rates.until[characterID]) {
		if _, ok := c.rates.until[characterID]; ok || len(c.rates.until) < 50000 {
			c.rates.until[characterID] = until
		}
	}
	return fmt.Errorf("ESI %s %s: %s budget at its reserve: %w (%w)", method, path, group, ErrErrorLimit, ErrRateLimited)
}

// noteRateLimited records a 429 about a character.
func (c *Client) noteRateLimited(route string, characterID int64, header http.Header, now time.Time) {
	wait := rateLimitDefaultWait
	if w, ok := parseRetryAfter(header.Get("Retry-After")); ok && w > 0 {
		wait = w
	}
	if wait > rateLimitMaxWait {
		wait = rateLimitMaxWait
	}
	c.rates.mu.Lock()
	defer c.rates.mu.Unlock()
	if c.rates.until == nil {
		c.rates.until = map[int64]time.Time{}
	}
	if _, ok := c.rates.until[characterID]; !ok && len(c.rates.until) >= 50000 {
		for id, u := range c.rates.until {
			if !u.After(now) {
				delete(c.rates.until, id)
			}
		}
	}
	if until := now.Add(wait); until.After(c.rates.until[characterID]) {
		if _, ok := c.rates.until[characterID]; ok || len(c.rates.until) < 50000 {
			c.rates.until[characterID] = until
		}
	}
	// The route itself waits out Retry-After too, for this character:
	// asking again sooner only spends another refusal.
	if c.rates.routeHold == nil {
		c.rates.routeHold = map[bucketKey]time.Time{}
	}
	key := bucketKey{route, characterID}
	if _, ok := c.rates.routeHold[key]; !ok && len(c.rates.routeHold) >= 50000 {
		for k, u := range c.rates.routeHold {
			if !u.After(now) {
				delete(c.rates.routeHold, k)
			}
		}
	}
	if _, ok := c.rates.routeHold[key]; ok || len(c.rates.routeHold) < 50000 {
		c.rates.routeHold[key] = now.Add(wait)
	}
}

// RateLimitedUntil reports until when a character is to be left alone
// because ESI answered 429 about it; the zero time when it is not.
func (c *Client) RateLimitedUntil(characterID int64, now time.Time) time.Time {
	c.rates.mu.Lock()
	defer c.rates.mu.Unlock()
	until, held := c.rates.until[characterID]
	if !held {
		return time.Time{}
	}
	if !until.After(now) {
		delete(c.rates.until, characterID)
		return time.Time{}
	}
	return until
}

// trackRateHeaders reads the rate-limit headers ESI sends with a
// response (X-Ratelimit-Group, -Limit, -Remaining) and keeps the
// tightest budget seen. Headers that are missing or not as expected
// are ignored: this is for seeing the limit coming, not for deciding
// anything.
func (c *Client) trackRateHeaders(route string, characterID int64, header http.Header, now time.Time) {
	raw := strings.TrimSpace(header.Get("X-Ratelimit-Remaining"))
	if raw == "" {
		return
	}
	remaining, err := strconv.Atoi(raw)
	if err != nil || remaining < 0 {
		return
	}
	c.rates.mu.Lock()
	defer c.rates.mu.Unlock()
	// Nearly out: ease off this character before ESI refuses it.
	if characterID != 0 && remaining <= rateLowWater {
		if c.rates.until == nil {
			c.rates.until = map[int64]time.Time{}
		}
		if until := now.Add(rateLowWait); until.After(c.rates.until[characterID]) {
			c.rates.until[characterID] = until
		}
	}
	group := clipHeader(header.Get("X-Ratelimit-Group"))
	c.learnBucket(route, characterID, group, remaining, header, now)
	if seen, known := c.rates.groups[group]; known && remaining >= seen.Remaining {
		return
	}
	if c.rates.groups == nil {
		c.rates.groups = map[string]RateHeadroom{}
	}
	if len(c.rates.groups) >= 200 {
		return // far more groups than ESI has: something is making names up
	}
	c.rates.groups[group] = RateHeadroom{
		Group:       group,
		CharacterID: characterID,
		Remaining:   remaining,
		Limit:       clipHeader(header.Get("X-Ratelimit-Limit")),
		At:          now,
	}
}

// TakeRateBudgets returns, for each group of routes ESI reported a
// budget for since the last call, the lowest it was seen at and for
// which character, the tightest first (least left as a share of the
// group's limit), and forgets them. Empty when ESI sent no such
// headers.
func (c *Client) TakeRateBudgets() []RateHeadroom {
	c.rates.mu.Lock()
	defer c.rates.mu.Unlock()
	out := make([]RateHeadroom, 0, len(c.rates.groups))
	for _, b := range c.rates.groups {
		out = append(out, b)
	}
	c.rates.groups = nil
	sort.Slice(out, func(i, j int) bool {
		fi, fj := out[i].share(), out[j].share()
		if fi != fj {
			return fi < fj
		}
		return out[i].Group < out[j].Group
	})
	return out
}

// share is how much of the group's limit was left, from 0 to 1. A
// limit that is not stated the way ESI states it ("150/15m") counts as
// a large one, so that such a group sorts by what was left.
func (h RateHeadroom) share() float64 {
	total := 1000000
	if n, _, found := strings.Cut(h.Limit, "/"); found {
		if v, err := strconv.Atoi(strings.TrimSpace(n)); err == nil && v > 0 {
			total = v
		}
	}
	return float64(h.Remaining) / float64(total)
}

// clipHeader keeps a header value short and plain: it is shown on a
// page.
func clipHeader(v string) string {
	v = strings.TrimSpace(v)
	if len(v) > 40 {
		v = v[:40]
	}
	return v
}
