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
	"testing"
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
	client := newStubClient(stub)
	var out any

	calls := map[string]func() error{
		"FetchRaw": func() error { _, _, err := client.FetchRaw(ctx, "t", "/x/"); return err },
		"Get":      func() error { return client.Get(ctx, "t", "/x/", &out) },
		"PostJSON": func() error { return client.PostJSON(ctx, "/x/", []int{1}, &out) },
		"PostJSONAuthed": func() error {
			return client.PostJSONAuthed(ctx, "t", "/x/", []int{1}, &out)
		},
		"PutJSONAuthed": func() error { return client.PutJSONAuthed(ctx, "t", "/x/", []int{1}) },
	}

	for name, call := range calls {
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

	// A healthy budget, reported on a public POST — the one helper
	// that used to skip the bookkeeping.
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

	// A reset time that has already passed means a fresh budget.
	stub.status = http.StatusOK
	stub.headers["X-Esi-Error-Limit-Reset"] = "0"
	_ = client.Get(ctx, "", "/status/", &out)
	if client.ErrorBudgetLow() {
		t.Error("budget still reads low after its reset time passed")
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
