package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// slowESITransport answers the public lookups the Corporation page
// makes, each after a fixed delay, and records how many were in flight
// at once. A page that goes one lookup after another peaks at 1.
type slowESITransport struct {
	delay    time.Duration
	inFlight atomic.Int64
	peak     atomic.Int64
	calls    atomic.Int64
}

func (s *slowESITransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.calls.Add(1)
	now := s.inFlight.Add(1)
	defer s.inFlight.Add(-1)
	for {
		peak := s.peak.Load()
		if now <= peak || s.peak.CompareAndSwap(peak, now) {
			break
		}
	}
	time.Sleep(s.delay)
	body := `{}`
	path := req.URL.Path
	switch {
	case strings.HasPrefix(path, "/corporations/"):
		var id int64
		fmt.Sscanf(path, "/corporations/%d/", &id)
		body = fmt.Sprintf(`{"name":"Corp %d","ticker":"C%d","member_count":5,"tax_rate":0.1,"ceo_id":%d,"alliance_id":%d,"home_station_id":60000001}`, id, id%100, id+1, id+2)
	case strings.HasPrefix(path, "/characters/"):
		body = `{"name":"A CEO"}`
	case strings.HasPrefix(path, "/alliances/"):
		body = `{"name":"An Alliance","ticker":"ALLY"}`
	case strings.HasPrefix(path, "/universe/stations/"):
		body = `{"name":"A Station"}`
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}, nil
}

// TestCorporationsPageBuildsCorporationsConcurrently: a user whose
// characters sit in several corporations used to wait for every public
// lookup of every corporation, one after another (four per corporation
// on a cold cache). They now run together, so the page takes about as
// long as one corporation's slowest lookup chain, not the sum.
func TestCorporationsPageBuildsCorporationsConcurrently(t *testing.T) {
	transport := &slowESITransport{delay: 80 * time.Millisecond}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	const corps = 6
	for i := 0; i < corps; i++ {
		charID := int64(90000100 + i)
		seedCharacter(t, q, user.ID, charID, fmt.Sprintf("Pilot %d", i))
		seedSnapshot(t, q, charID, "profile", fmt.Sprintf(`{"name":"Pilot %d","corporation_id":%d}`, i, 98000100+i))
	}
	cookie := sessionCookie(t, app, user.ID, 90000100, "Pilot 0")

	start := time.Now()
	code, body := getPage(t, app, cookie, "/corporations/")
	elapsed := time.Since(start)
	if code != http.StatusOK {
		t.Fatalf("/corporations/: status %d", code)
	}
	for i := 0; i < corps; i++ {
		mustContain(t, "/corporations/", body, fmt.Sprintf("Corp %d", 98000100+i))
	}
	// The CEO, alliance and station lookups of one corporation also
	// run together, and different corporations overlap: with 6
	// corporations x 4 lookups and a cap of 4 corporations at a time,
	// well over 4 requests are in flight at the peak.
	if peak := transport.peak.Load(); peak < 6 {
		t.Errorf("peak in-flight ESI requests = %d, want >= 6 (lookups run together)", peak)
	}
	// Sequentially this would be 6 x 4 x 80ms = ~1.9s.
	if sequential := time.Duration(corps*4) * transport.delay; elapsed > sequential/2 {
		t.Errorf("page took %v; one lookup after another would take about %v", elapsed, sequential)
	}
}
