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

// TestRateHeadroom: the tightest budget ESI reports is kept until it
// is read, with the group and character it was for; headers that are
// missing or not numbers are ignored.
func TestRateHeadroom(t *testing.T) {
	transport := &headerTransport{status: http.StatusOK, header: http.Header{}}
	c := New(&http.Client{Transport: transport}, nil, nil)
	get := func(character int64, remaining, group string) {
		transport.header = http.Header{}
		if remaining != "" {
			transport.header.Set("X-Ratelimit-Remaining", remaining)
			transport.header.Set("X-Ratelimit-Limit", "150/15m")
			transport.header.Set("X-Ratelimit-Group", group)
		}
		if _, _, _, err := c.send(WithCharacter(context.Background(), character), http.MethodGet, "", "/x/", nil, "", http.StatusOK); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := c.TakeRateHeadroom(); ok {
		t.Fatal("headroom reported before any response")
	}
	get(1, "", "")
	get(1, "plenty", "char-wallet")
	if _, ok := c.TakeRateHeadroom(); ok {
		t.Fatal("headroom reported from responses that stated none")
	}
	get(1, "120", "char-wallet")
	get(2, "37", "char-location")
	get(3, "90", "char-wallet")
	tight, ok := c.TakeRateHeadroom()
	if !ok || tight.Remaining != 37 || tight.CharacterID != 2 || tight.Group != "char-location" || tight.Limit != "150/15m" {
		t.Fatalf("tightest %+v, ok=%v; want 37 left for character 2 in char-location", tight, ok)
	}
	if _, ok := c.TakeRateHeadroom(); ok {
		t.Fatal("headroom was not forgotten once read")
	}
}
