package esi

import (
	"context"
	"errors"
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
}

// noteRateLimited records a 429 about a character.
func (c *Client) noteRateLimited(characterID int64, header http.Header, now time.Time) {
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
	if until := now.Add(wait); until.After(c.rates.until[characterID]) {
		c.rates.until[characterID] = until
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
func (c *Client) trackRateHeaders(characterID int64, header http.Header, now time.Time) {
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
