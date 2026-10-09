package esi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// headerTransport answers every request with one status and one set
// of headers.
type headerTransport struct {
	status int
	header http.Header
}

func (h *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: h.status, Header: h.header.Clone(), Body: io.NopCloser(strings.NewReader(`{}`)), Request: req}, nil
}

// TestRateLimitIsOneCharacters: a 429 is the rate limit of the
// character the request was for, held for as long as ESI says; a 420
// is the application's error limit and is nobody's in particular. Both
// still read as "a limit" to code that stops on one.
func TestRateLimitIsOneCharacters(t *testing.T) {
	fastRetries(t)
	transport := &headerTransport{status: http.StatusTooManyRequests, header: http.Header{"Retry-After": {"90"}}}
	c := New(&http.Client{Transport: transport}, nil, nil)
	now := time.Now()

	ctx := WithCharacter(context.Background(), 90000001)
	_, _, status, err := c.send(ctx, http.MethodGet, "", "/characters/90000001/wallet/", nil, "", http.StatusOK)
	if status != http.StatusTooManyRequests || !errors.Is(err, ErrRateLimited) || !errors.Is(err, ErrErrorLimit) {
		t.Fatalf("429: status %d, err %v; want it to be both a rate limit and a limit", status, err)
	}
	until := c.RateLimitedUntil(90000001, now)
	if wait := until.Sub(now); wait < 85*time.Second || wait > 95*time.Second {
		t.Fatalf("held for %v, want the 90 seconds ESI asked for", wait)
	}
	if !c.RateLimitedUntil(90000002, now).IsZero() {
		t.Fatal("one character's rate limit holds another")
	}
	// It ends when ESI said it would.
	if !c.RateLimitedUntil(90000001, now.Add(2*time.Minute)).IsZero() {
		t.Fatal("still held after the wait was over")
	}

	// No Retry-After: a minute. A silly one: no more than the window.
	transport.header = http.Header{}
	_, _, _, _ = c.send(WithCharacter(context.Background(), 7), http.MethodGet, "", "/x/", nil, "", http.StatusOK)
	if wait := c.RateLimitedUntil(7, now).Sub(now); wait < 55*time.Second || wait > 65*time.Second {
		t.Fatalf("with no Retry-After: held for %v, want a minute", wait)
	}
	transport.header = http.Header{"Retry-After": {"86400"}}
	_, _, _, _ = c.send(WithCharacter(context.Background(), 8), http.MethodGet, "", "/x/", nil, "", http.StatusOK)
	if wait := c.RateLimitedUntil(8, now).Sub(now); wait > 16*time.Minute {
		t.Fatalf("a day-long Retry-After was honoured: %v", wait)
	}

	// 420 is the error limit: a limit, but not a character's.
	transport.status, transport.header = 420, http.Header{}
	_, _, _, err = c.send(WithCharacter(context.Background(), 9), http.MethodGet, "", "/x/", nil, "", http.StatusOK)
	if !errors.Is(err, ErrErrorLimit) || errors.Is(err, ErrRateLimited) {
		t.Fatalf("420: %v; want the error limit and not a rate limit", err)
	}
	if !c.RateLimitedUntil(9, now).IsZero() {
		t.Fatal("a 420 was recorded against a character")
	}
}

// TestRateBudgets: the lowest each group's budget was seen at is kept
// until it is read, with the character it was for, and the groups come
// back tightest first by share of their limit; headers that are
// missing or not numbers are ignored.
func TestRateBudgets(t *testing.T) {
	transport := &headerTransport{status: http.StatusOK, header: http.Header{}}
	c := New(&http.Client{Transport: transport}, nil, nil)
	limit := "150/15m"
	get := func(character int64, remaining, group string) {
		transport.header = http.Header{}
		if remaining != "" {
			transport.header.Set("X-Ratelimit-Remaining", remaining)
			transport.header.Set("X-Ratelimit-Limit", limit)
			transport.header.Set("X-Ratelimit-Group", group)
		}
		if _, _, _, err := c.send(WithCharacter(context.Background(), character), http.MethodGet, "", "/x/", nil, "", http.StatusOK); err != nil {
			t.Fatal(err)
		}
	}
	if got := c.TakeRateBudgets(); len(got) != 0 {
		t.Fatal("budgets reported before any response")
	}
	get(1, "", "")
	get(1, "plenty", "char-wallet")
	if got := c.TakeRateBudgets(); len(got) != 0 {
		t.Fatal("budgets reported from responses that stated none")
	}
	get(1, "120", "char-wallet")
	get(2, "37", "char-location")
	get(3, "90", "char-wallet")
	// A small group with most of its budget left, and one nearly spent:
	// what counts is the share left, not the number.
	limit = "30/15m"
	get(2, "27", "corp-killmail")
	get(3, "6", "char-notification")
	limit = "150/15m"
	got := c.TakeRateBudgets()
	var order []string
	for _, b := range got {
		order = append(order, b.Group)
	}
	if strings.Join(order, ",") != "char-notification,char-location,char-wallet,corp-killmail" {
		t.Fatalf("groups in order %v; want tightest first by share of limit", order)
	}
	if got[1].Remaining != 37 || got[1].CharacterID != 2 || got[1].Limit != "150/15m" {
		t.Fatalf("char-location: %+v; want 37 left for character 2", got[1])
	}
	if got[2].Remaining != 90 || got[2].CharacterID != 3 {
		t.Fatalf("char-wallet: %+v; want the lowest it was seen at, 90 for character 3", got[2])
	}
	if len(c.TakeRateBudgets()) != 0 {
		t.Fatal("budgets were not forgotten once read")
	}
}

// TestLowBudgetEasesOff: a character whose budget in some group is
// nearly gone is left alone for a minute before ESI has to refuse it;
// with tokens to spare nobody is held, and a public request running
// low holds no character.
func TestLowBudgetEasesOff(t *testing.T) {
	transport := &headerTransport{status: http.StatusOK, header: http.Header{}}
	c := New(&http.Client{Transport: transport}, nil, nil)
	get := func(character int64, remaining string) {
		transport.header = http.Header{"X-Ratelimit-Remaining": {remaining}, "X-Ratelimit-Limit": {"30/15m"}, "X-Ratelimit-Group": {"corp-killmail"}}
		if _, _, _, err := c.send(WithCharacter(context.Background(), character), http.MethodGet, "", "/x/", nil, "", http.StatusOK); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	get(1, "27")
	if !c.RateLimitedUntil(1, now).IsZero() {
		t.Fatal("a character with most of its budget left is held")
	}
	get(2, "4")
	if wait := c.RateLimitedUntil(2, now).Sub(now); wait < 50*time.Second || wait > 70*time.Second {
		t.Fatalf("with 4 tokens left: held for %v, want about a minute", wait)
	}
	if !c.RateLimitedUntil(1, now).IsZero() {
		t.Fatal("one character running low held another")
	}
	get(0, "2")
	if !c.RateLimitedUntil(0, now).IsZero() {
		t.Fatal("a public request running low was held as if it were a character")
	}
}
