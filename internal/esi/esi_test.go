package esi

// Tests for the client's HTTP layer: every helper goes through one
// request path, so each must identify itself, carry its token and
// payload, read ESI's statuses the same way, and feed the error
// budget. A stub transport stands in for ESI; nothing here touches
// the network or a database.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubESI answers every request with a fixed status, body and
// headers, and keeps the last request it saw.
type stubESI struct {
	status  int
	body    string
	headers map[string]string

	last     *http.Request
	lastBody string
	calls    int
}

func (s *stubESI) RoundTrip(req *http.Request) (*http.Response, error) {
	s.calls++
	s.last = req
	s.lastBody = ""
	if req.Body != nil {
		raw, _ := io.ReadAll(req.Body)
		s.lastBody = string(raw)
	}
	h := make(http.Header)
	for k, v := range s.headers {
		h.Set(k, v)
	}
	return &http.Response{
		StatusCode: s.status,
		Header:     h,
		Body:       io.NopCloser(strings.NewReader(s.body)),
		Request:    req,
	}, nil
}

func newStubClient(stub *stubESI) *Client {
	return New(&http.Client{Transport: stub}, nil, nil)
}

func TestRequestsIdentifyThemselvesAndCarryTheirToken(t *testing.T) {
	stub := &stubESI{status: http.StatusOK, body: `{"ok":true}`}
	client := newStubClient(stub)
	ctx := context.Background()

	// No user agent set: the default still names the project.
	if _, _, err := client.FetchRaw(ctx, "", "/status/"); err != nil {
		t.Fatalf("FetchRaw: %v", err)
	}
	if got := stub.last.Header.Get("User-Agent"); !strings.Contains(got, "EveSynapse") {
		t.Errorf("default User-Agent = %q, want it to name EveSynapse", got)
	}
	if got := stub.last.Header.Get("Authorization"); got != "" {
		t.Errorf("a public request carried Authorization %q", got)
	}
	if got := stub.last.Header.Get("X-Compatibility-Date"); got != "2026-11-01" {
		t.Errorf("X-Compatibility-Date = %q, want the pinned 2026-11-01", got)
	}
	if got := stub.last.URL.String(); got != "https://esi.evetech.net/status/" {
		t.Errorf("request went to %q", got)
	}

	// Once set, every kind of request carries it.
	const ua = "EveSynapse/1.2.3 (+https://example.org; ops@example.org)"
	client.SetUserAgent(ua)
	client.SetUserAgent("   ") // an empty value never clears it
	var out map[string]bool
	calls := map[string]func() error{
		"GET": func() error { return client.Get(ctx, "token-a", "/characters/1/", &out) },
		"POST (public)": func() error {
			return client.PostJSON(ctx, "/universe/ids/", []string{"x"}, &out)
		},
	}
	for name, call := range calls {
		if err := call(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := stub.last.Header.Get("User-Agent"); got != ua {
			t.Errorf("%s: User-Agent = %q, want %q", name, got, ua)
		}
		if got := stub.last.Header.Get("X-Compatibility-Date"); got != compatibilityDate {
			t.Errorf("%s: X-Compatibility-Date = %q, want %q", name, got, compatibilityDate)
		}
	}

	if err := client.Get(ctx, "token-a", "/characters/1/", &out); err != nil {
		t.Fatalf("authenticated GET: %v", err)
	}
	if got := stub.last.Header.Get("Authorization"); got != "Bearer token-a" {
		t.Errorf("authenticated GET Authorization = %q", got)
	}
	if !out["ok"] {
		t.Errorf("GET decoded %v, want ok=true", out)
	}
}

func TestWritesSendJSONAndAcceptTheirStatuses(t *testing.T) {
	ctx := context.Background()

	// A new fitting answers 201 with an object.
	stub := &stubESI{status: http.StatusCreated, body: `{"fitting_id":77}`}
	client := newStubClient(stub)
	var created struct {
		FittingID int64 `json:"fitting_id"`
	}
	if err := client.PostJSONAuthed(ctx, "token-b", "/characters/1/fittings/", map[string]string{"name": "fit"}, &created); err != nil {
		t.Fatalf("PostJSONAuthed: %v", err)
	}
	if created.FittingID != 77 {
		t.Errorf("decoded fitting id %d, want 77", created.FittingID)
	}
	if stub.last.Method != http.MethodPost || stub.lastBody != `{"name":"fit"}` {
		t.Errorf("sent %s %q, want POST of the JSON payload", stub.last.Method, stub.lastBody)
	}
	if got := stub.last.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := stub.last.Header.Get("Authorization"); got != "Bearer token-b" {
		t.Errorf("Authorization = %q", got)
	}

	// A sent mail answers 201 with a bare number. A caller that
	// has no use for the answer passes nil and must not fail on its
	// shape.
	stub.body = `424242`
	if err := client.PostJSONAuthed(ctx, "token-b", "/characters/1/mail/", map[string]string{"subject": "hi"}, nil); err != nil {
		t.Fatalf("PostJSONAuthed with a bare-number answer and nil out: %v", err)
	}

	// 200 is not what an authenticated write answers.
	stub.status = http.StatusOK
	if err := client.PostJSONAuthed(ctx, "token-b", "/characters/1/mail/", map[string]string{}, nil); err == nil {
		t.Error("PostJSONAuthed accepted a 200")
	}

	// Marking a mail read answers 204 with nothing.
	stub.status, stub.body = http.StatusNoContent, ""
	if err := client.PutJSONAuthed(ctx, "token-b", "/characters/1/mail/5/", map[string]bool{"read": true}); err != nil {
		t.Fatalf("PutJSONAuthed: %v", err)
	}
	if stub.last.Method != http.MethodPut || stub.lastBody != `{"read":true}` {
		t.Errorf("sent %s %q, want PUT of the JSON payload", stub.last.Method, stub.lastBody)
	}
}

func TestStatusesAreReadTheSameWayEverywhere(t *testing.T) {
	ctx := context.Background()
	stub := &stubESI{}
	var out any

	// Each answer goes to a client of its own: a 429 holds its route
	// for that client until Retry-After, so a shared client would
	// refuse what follows on the same route without asking ESI.
	calls := map[string]func(client *Client) error{
		"FetchRaw": func(client *Client) error { _, _, err := client.FetchRaw(ctx, "t", "/x/"); return err },
		"Get":      func(client *Client) error { return client.Get(ctx, "t", "/x/", &out) },
		"PostJSON": func(client *Client) error { return client.PostJSON(ctx, "/x/", []int{1}, &out) },
		"PostJSONAuthed": func(client *Client) error {
			return client.PostJSONAuthed(ctx, "t", "/x/", []int{1}, &out)
		},
		"PutJSONAuthed": func(client *Client) error { return client.PutJSONAuthed(ctx, "t", "/x/", []int{1}) },
	}

	for name, run := range calls {
		call := func() error { return run(newStubClient(stub)) }
		// ESI's error-limit answers stop the caller.
		for _, code := range []int{420, http.StatusTooManyRequests} {
			stub.status, stub.body = code, `{"error":"slow down"}`
			if err := call(); !errors.Is(err, ErrErrorLimit) {
				t.Errorf("%s on %d: err = %v, want ErrErrorLimit", name, code, err)
			}
		}
		// A refusal carries its status, so callers can tell a
		// missing scope or role from anything else.
		stub.status, stub.body = http.StatusForbidden, `{"error":"forbidden"}`
		err := call()
		if code, ok := StatusCode(err); !ok || code != http.StatusForbidden {
			t.Errorf("%s on 403: err = %v, want a StatusError carrying 403", name, err)
		}
		if errors.Is(err, ErrErrorLimit) {
			t.Errorf("%s on 403 was read as the error limit", name)
		}
	}
}

func TestEveryResponseFeedsTheErrorBudget(t *testing.T) {
	ctx := context.Background()
	stub := &stubESI{status: http.StatusOK, body: `{}`}
	client := newStubClient(stub)
	var out any

	if client.ErrorBudgetLow() {
		t.Fatal("budget reads low before any response was seen")
	}

	// A healthy budget, reported on a public POST: that helper
	// keeps the bookkeeping too.
	stub.headers = map[string]string{"X-Esi-Error-Limit-Remain": "80", "X-Esi-Error-Limit-Reset": "30"}
	if err := client.PostJSON(ctx, "/universe/ids/", []string{"x"}, &out); err != nil {
		t.Fatalf("PostJSON: %v", err)
	}
	if remain, reset := client.ErrorBudgetStatus(); remain != 80 || reset == 0 {
		t.Fatalf("budget after a public POST: remain %d, reset %d; want 80 and a reset time", remain, reset)
	}
	if client.ErrorBudgetLow() {
		t.Error("budget reads low with 80 errors left")
	}

	// Nearly spent, reported on a failing response: still counted.
	stub.status = http.StatusNotFound
	stub.headers["X-Esi-Error-Limit-Remain"] = "3"
	_ = client.Get(ctx, "", "/characters/0/", &out)
	if !client.ErrorBudgetLow() {
		t.Error("budget does not read low with 3 errors left")
	}

	// While it reads low, requests are held here instead of sent.
	stub.status = http.StatusOK
	if err := client.Get(ctx, "", "/status/", &out); !errors.Is(err, ErrErrorLimit) {
		t.Errorf("a request with the budget nearly spent: %v; want it held as the error limit", err)
	}

	// A reset time that has passed means a fresh budget.
	client.errBudgetMu.Lock()
	client.errBudgetReset = time.Now().Unix() - 1
	client.errBudgetMu.Unlock()
	if client.ErrorBudgetLow() {
		t.Error("budget still reads low after its reset time passed")
	}
	if err := client.Get(ctx, "", "/status/", &out); err != nil {
		t.Errorf("a request after the reset: %v", err)
	}
}

func TestUndecodableAnswerIsAnError(t *testing.T) {
	stub := &stubESI{status: http.StatusOK, body: `not json`}
	client := newStubClient(stub)
	var out map[string]any
	err := client.Get(context.Background(), "", "/status/", &out)
	if err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("err = %v, want a decode error", err)
	}
}

// etagStub answers GETs with an ETag and honors If-None-Match: a
// call offering the current ETag gets a 304 with no body, any other
// a 200 with the body. Each answer's cache window ends window from
// now. The first call waits firstCallDelay, so concurrent readers
// have time to join the one in-flight call before it answers.
type etagStub struct {
	mu             sync.Mutex
	calls          int
	etag           string
	body           string
	window         time.Duration
	firstCallDelay time.Duration
	seen           []string // If-None-Match values received, in order
}

func (s *etagStub) RoundTrip(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	s.calls++
	first := s.calls == 1
	inm := req.Header.Get("If-None-Match")
	s.seen = append(s.seen, inm)
	h := make(http.Header)
	h.Set("ETag", s.etag)
	h.Set("Expires", time.Now().Add(s.window).UTC().Format(http.TimeFormat))
	status, body := http.StatusOK, s.body
	if inm != "" && inm == s.etag {
		status, body = http.StatusNotModified, ""
	}
	s.mu.Unlock()
	if first && s.firstCallDelay > 0 {
		select {
		case <-time.After(s.firstCallDelay):
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
}

func (s *etagStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// TestConcurrentGETsCollapseAndETagServesCached pins what the ESI
// call collapse and ETag cache exist for: concurrent identical GETs
// fire one ESI call, and the first caller leaving does not fail the
// others; inside ESI's cache window a repeat GET is served without a
// call; past the window it revalidates with its ETag and a 304
// serves the held body.
func TestConcurrentGETsCollapseAndETagServesCached(t *testing.T) {
	stub := &etagStub{etag: `"v1"`, body: `{"n":1}`, window: time.Hour, firstCallDelay: 200 * time.Millisecond}
	client := New(&http.Client{Transport: stub}, nil, nil)
	ctx := context.Background()

	// The first caller gives up while nine others wait on its call.
	leaderCtx, cancelLeader := context.WithCancel(ctx)
	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		_, _, _ = client.FetchRaw(leaderCtx, "", "/alliances/1/")
	}()
	time.Sleep(20 * time.Millisecond)
	const readers = 9
	var wg sync.WaitGroup
	bodies := make([][]byte, readers)
	errs := make([]error, readers)
	for i := range readers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body, _, err := client.FetchRaw(ctx, "", "/alliances/1/")
			bodies[i], errs[i] = body, err
		}(i)
	}
	time.Sleep(20 * time.Millisecond)
	cancelLeader()
	<-leaderDone
	wg.Wait()
	if n := stub.count(); n != 1 {
		t.Fatalf("ten concurrent identical GETs made %d ESI calls, want 1", n)
	}
	for i := range readers {
		if errs[i] != nil {
			t.Fatalf("reader %d failed when the first caller left: %v", i, errs[i])
		}
		if string(bodies[i]) != `{"n":1}` {
			t.Fatalf("reader %d got %q, want the shared body", i, bodies[i])
		}
	}

	// Inside the window: served from memory, no call.
	if body, _, err := client.FetchRaw(ctx, "", "/alliances/1/"); err != nil || string(body) != `{"n":1}` {
		t.Fatalf("GET inside the window = %q, %v; want the held body", body, err)
	}
	if n := stub.count(); n != 1 {
		t.Fatalf("GET inside the window made %d ESI calls in total, want 1", n)
	}

	// Past the window: one conditional call, 304, the held body.
	client.getCache.mu.Lock()
	for _, e := range client.getCache.entries {
		e.expires = time.Now().Add(-time.Second)
	}
	client.getCache.mu.Unlock()
	if body, _, err := client.FetchRaw(ctx, "", "/alliances/1/"); err != nil || string(body) != `{"n":1}` {
		t.Fatalf("GET past the window = %q, %v; want the held body", body, err)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.calls != 2 || stub.seen[1] != `"v1"` {
		t.Fatalf("GET past the window made %d calls with If-None-Match %q, want 2 with the held ETag", stub.calls, stub.seen)
	}
}
