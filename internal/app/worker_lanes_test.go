package app

import (
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// laneESI holds each request about a character's own data for a moment,
// or until another one is in flight beside it, and remembers the most
// it ever saw at once and which characters were asked about. Every
// request is then answered "not found", which ESI's client does not
// try again.
type laneESI struct {
	mu       sync.Mutex
	inFlight int
	most     int
	asked    map[string]int
	together chan struct{}
}

var laneCharacterPath = regexp.MustCompile(`/characters/(\d+)/.+`)

func (s *laneESI) RoundTrip(req *http.Request) (*http.Response, error) {
	m := laneCharacterPath.FindStringSubmatch(req.URL.Path)
	if m == nil {
		return laneNotFound(req), nil
	}
	s.mu.Lock()
	if s.asked == nil {
		s.asked = map[string]int{}
		s.together = make(chan struct{})
	}
	s.asked[m[1]]++
	s.inFlight++
	if s.inFlight > s.most {
		s.most = s.inFlight
	}
	if s.inFlight == 2 {
		select {
		case <-s.together:
		default:
			close(s.together)
		}
	}
	together := s.together
	s.mu.Unlock()

	select {
	case <-together:
	case <-time.After(10 * time.Millisecond):
	}

	s.mu.Lock()
	s.inFlight--
	s.mu.Unlock()
	return laneNotFound(req), nil
}

func laneNotFound(req *http.Request) *http.Response {
	return &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":"not found"}`)), Request: req}
}

// TestWorkerRefreshesCharactersSideBySide: with more than one lane the
// worker has requests for different characters in flight together, and
// every character is still served; with one lane it is one at a time.
func TestWorkerRefreshesCharactersSideBySide(t *testing.T) {
	for _, tc := range []struct {
		lanes    int
		together bool
	}{{4, true}, {1, false}} {
		transport := &laneESI{}
		app, _, q := buildCorpTestApp(t, transport)
		app.cfg.workerLanes = tc.lanes
		ctx := t.Context()
		user, err := q.CreateUser(ctx)
		if err != nil {
			t.Fatal(err)
		}
		ids := []int64{fixtureCharA, fixtureCharB, 90000003, 90000004}
		for _, id := range ids {
			seedCharacter(t, q, user.ID, id, "Lane Fixture")
		}

		app.refreshCycle(ctx)

		transport.mu.Lock()
		most, asked := transport.most, len(transport.asked)
		transport.mu.Unlock()
		if asked != len(ids) {
			t.Fatalf("%d lane(s): %d of %d characters were asked about", tc.lanes, asked, len(ids))
		}
		if tc.together && most < 2 {
			t.Fatalf("%d lanes: never more than %d request in flight", tc.lanes, most)
		}
		if !tc.together && most != 1 {
			t.Fatalf("one lane: %d requests in flight at once", most)
		}
		if timing := app.snapshotWorkerStatus().Timing; timing.Characters != len(ids) || timing.Fetches < len(ids) {
			t.Fatalf("%d lane(s): the cycle counted %d characters and %d fetches", tc.lanes, timing.Characters, timing.Fetches)
		}
	}
}

// TestKeyedLocks: one id's lock does not hold another's, and does hold
// its own.
func TestKeyedLocks(t *testing.T) {
	var k keyedLocks
	unlockOne := k.lock(1)
	other := make(chan struct{})
	go func() {
		k.lock(2)()
		close(other)
	}()
	select {
	case <-other:
	case <-time.After(5 * time.Second):
		t.Fatal("locking one id held up another")
	}

	same := make(chan struct{})
	go func() {
		k.lock(1)()
		close(same)
	}()
	select {
	case <-same:
		t.Fatal("an id was locked twice at once")
	case <-time.After(50 * time.Millisecond):
	}
	unlockOne()
	select {
	case <-same:
	case <-time.After(5 * time.Second):
		t.Fatal("the lock was not released")
	}
}

func TestParseWorkerLanes(t *testing.T) {
	for raw, want := range map[string]int{"": defaultWorkerLanes, " ": defaultWorkerLanes, "1": 1, " 8 ": 8, "999": mostWorkerLanes, "many": defaultWorkerLanes, "0": defaultWorkerLanes, "-2": defaultWorkerLanes} {
		if got := parseWorkerLanes(raw); got != want {
			t.Errorf("parseWorkerLanes(%q) = %d, want %d", raw, got, want)
		}
	}
	// The default allowance follows the lanes; a set one does not.
	app := &Application{}
	if app.workerLanes() != 1 || app.fetchesPerCycle() != maxFetchesPerCycle {
		t.Fatalf("a bare application: %d lane(s), %d fetches", app.workerLanes(), app.fetchesPerCycle())
	}
	app.cfg.workerLanes = 4
	if got := app.fetchesPerCycle(); got != 4*fetchesPerLane {
		t.Fatalf("four lanes: default allowance %d, want %d", got, 4*fetchesPerLane)
	}
	app.cfg.workerFetches = 250
	if got := app.fetchesPerCycle(); got != 250 {
		t.Fatalf("a set allowance was changed to %d", got)
	}
}
