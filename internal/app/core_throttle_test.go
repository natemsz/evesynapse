package app

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// TestAnonThrottleBucket: a burst, then the steady rate; addresses
// do not share; a full bucket is forgotten.
func TestAnonThrottleBucket(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	th := newAnonThrottle(60, 3) // one a second, burst three
	th.now = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		if ok, _ := th.allow("198.51.100.7"); !ok {
			t.Fatalf("request %d inside the burst refused", i+1)
		}
	}
	ok, wait := th.allow("198.51.100.7")
	if ok {
		t.Fatal("fourth request in the same instant allowed")
	}
	if wait <= 0 || wait > time.Second {
		t.Errorf("wait = %v, want up to a second", wait)
	}
	if ok, _ := th.allow("203.0.113.9"); !ok {
		t.Error("another address refused: buckets are per address")
	}
	now = now.Add(time.Second)
	if ok, _ := th.allow("198.51.100.7"); !ok {
		t.Error("a second later one more request is due")
	}
	// Idle long enough to fill up: forgotten on the next sweep.
	now = now.Add(2 * time.Minute)
	th.allow("192.0.2.200")
	if _, kept := th.clients["198.51.100.7"]; kept {
		t.Error("a refilled address was not forgotten")
	}
}

// TestClientAddr: the proxy's X-Forwarded-For counts only when the
// proxy is on this machine.
func TestClientAddr(t *testing.T) {
	cases := []struct {
		remote, fwd, cf, want string
	}{
		{"198.51.100.7:4242", "", "", "198.51.100.7"},
		// A stranger cannot pick its own address.
		{"198.51.100.7:4242", "203.0.113.9", "", "198.51.100.7"},
		// Caddy on loopback appends the client it saw: the last
		// entry, not one the client sent ahead of it.
		{"127.0.0.1:50000", "10.9.9.9, 203.0.113.9", "", "203.0.113.9"},
		{"[::1]:50000", "2001:db8::5", "", "2001:db8::5"},
		{"127.0.0.1:50000", "", "", "127.0.0.1"},
		{"127.0.0.1:50000", "not-an-address", "", "127.0.0.1"},
		// cloudflared on loopback names the visitor in CF-Connecting-IP.
		{"127.0.0.1:50000", "172.70.1.1", "203.0.113.9", "203.0.113.9"},
		// From anywhere else it is ignored like X-Forwarded-For.
		{"198.51.100.7:4242", "", "203.0.113.9", "198.51.100.7"},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(http.MethodGet, "/fittings/public/", nil)
		r.RemoteAddr = tc.remote
		if tc.fwd != "" {
			r.Header.Set("X-Forwarded-For", tc.fwd)
		}
		if tc.cf != "" {
			r.Header.Set("CF-Connecting-IP", tc.cf)
		}
		if got := clientAddr(r); got != tc.want {
			t.Errorf("clientAddr(%s, XFF %q, CF %q) = %q, want %q", tc.remote, tc.fwd, tc.cf, got, tc.want)
		}
	}
}

// TestAnonThrottleRoutes: an anonymous address that spends its
// allowance on the public pages gets 429 with Retry-After; another
// address and a signed-in visitor are untouched; the simulator has
// its own allowance.
func TestAnonThrottleRoutes(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	user, err := q.CreateUser(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")
	// A frozen clock: under the race detector 60 requests take long enough
	// for real time to refill a token and the burst would not run out.
	pages, sim := app.anonThrottles()
	frozen := time.Now()
	pages.now = func() time.Time { return frozen }
	sim.now = func() time.Time { return frozen }
	handler := app.Handler()
	get := func(path, addr string, c *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.RemoteAddr = addr + ":1234"
		if c != nil {
			r.AddCookie(c)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)
		return rec
	}

	for i := 0; i < anonPagesBurst; i++ {
		if rec := get("/fittings/public/", "198.51.100.7", nil); rec.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d of the burst throttled", i+1)
		}
	}
	rec := get("/fittings/public/", "198.51.100.7", nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("past the burst: status %d, want 429", rec.Code)
	}
	if secs, err := strconv.Atoi(rec.Header().Get("Retry-After")); err != nil || secs < 1 {
		t.Errorf("Retry-After = %q, want whole seconds", rec.Header().Get("Retry-After"))
	}
	// The same allowance covers the other public pages.
	if rec := get("/ships/587/", "198.51.100.7", nil); rec.Code != http.StatusTooManyRequests {
		t.Errorf("ship page from the spent address: status %d, want 429", rec.Code)
	}
	if rec := get("/fittings/public/", "203.0.113.9", nil); rec.Code == http.StatusTooManyRequests {
		t.Error("another address throttled")
	}
	if rec := get("/fittings/public/", "198.51.100.7", cookie); rec.Code == http.StatusTooManyRequests {
		t.Error("a signed-in visitor throttled")
	}

	// The simulator spends from its own bucket.
	post := func(addr string) int {
		r := httptest.NewRequest(http.MethodPost, "/fittings/simulate/", nil)
		r.RemoteAddr = addr + ":1234"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)
		return rec.Code
	}
	for i := 0; i < anonSimBurst; i++ {
		if code := post("192.0.2.33"); code == http.StatusTooManyRequests {
			t.Fatalf("simulate %d of the burst throttled", i+1)
		}
	}
	if code := post("192.0.2.33"); code != http.StatusTooManyRequests {
		t.Errorf("simulate past the burst: status %d, want 429", code)
	}
}
