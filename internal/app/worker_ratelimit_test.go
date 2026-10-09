package app

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// rateESI answers 429, with a Retry-After, to every request about one
// character, and fails every other request the ordinary way. It
// remembers what was asked.
type rateESI struct {
	mu      sync.Mutex
	limited int64
	paths   []string
}

func (s *rateESI) RoundTrip(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	s.paths = append(s.paths, req.URL.Path)
	s.mu.Unlock()
	if strings.Contains(req.URL.Path, fmt.Sprintf("/characters/%d/", s.limited)) {
		return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {"120"}},
			Body: io.NopCloser(strings.NewReader(`{"error":"rate limited"}`)), Request: req}, nil
	}
	return (&countingTransport{}).RoundTrip(req)
}

// about counts the requests made about a character, and forgets all.
func (s *rateESI) about(ids ...int64) []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]int, len(ids))
	for _, p := range s.paths {
		for i, id := range ids {
			if strings.Contains(p, fmt.Sprintf("/characters/%d/", id)) {
				out[i]++
			}
		}
	}
	s.paths = nil
	return out
}

// TestRateLimitedCharacterDoesNotStopTheCycle: when ESI rate-limits
// one character, that character is asked once and then left alone for
// as long as ESI said, and the worker carries on with everybody else.
// (The application's error limit, a 420, still stops the cycle:
// TestRefreshCycleStopsAtTheErrorLimit.)
func TestRateLimitedCharacterDoesNotStopTheCycle(t *testing.T) {
	transport := &rateESI{limited: fixtureCharA}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := t.Context()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Alpha")
	seedCharacter(t, q, user.ID, fixtureCharB, "Beta")

	app.refreshCycle(ctx)
	asked := transport.about(fixtureCharA, fixtureCharB)
	if asked[0] != 1 {
		t.Fatalf("the rate-limited character was asked about %d time(s), want once", asked[0])
	}
	if asked[1] == 0 {
		t.Fatal("one character's rate limit stopped the worker serving the other")
	}
	timing := app.snapshotWorkerStatus().Timing
	if timing.RateHeld != 1 {
		t.Fatalf("%d character(s) counted as left alone, want 1", timing.RateHeld)
	}
	if until := app.esi.RateLimitedUntil(fixtureCharA, time.Now()); until.IsZero() {
		t.Fatal("the character is not being held")
	}
	if view := workerTimingViewFor(app.snapshotWorkerStatus()); !strings.Contains(view.Last, "1 character left alone at ESI's request") {
		t.Fatalf("the Sync page does not say so: %q", view.Last)
	}

	// The next cycle, inside the wait: not asked at all. The other still is.
	app.refreshCycle(ctx)
	asked = transport.about(fixtureCharA, fixtureCharB)
	if asked[0] != 0 || asked[1] == 0 {
		t.Fatalf("next cycle: %d request(s) about the held character, %d about the other", asked[0], asked[1])
	}
}

func TestParseWorkerFetches(t *testing.T) {
	for raw, want := range map[string]int{"": 0, "  ": 0, "120": 120, " 500 ": 500, "5": minFetchesPerCycle, "99999": mostFetchesPerCycle, "lots": 0, "-3": 0, "0": 0} {
		if got := parseWorkerFetches(raw); got != want {
			t.Errorf("parseWorkerFetches(%q) = %d, want %d", raw, got, want)
		}
	}
	app := &Application{}
	if app.fetchesPerCycle() != maxFetchesPerCycle {
		t.Fatal("with nothing set, the allowance is not the default")
	}
	app.cfg.workerFetches = 400
	if app.fetchesPerCycle() != 400 {
		t.Fatal("the configured allowance is not used")
	}
}
