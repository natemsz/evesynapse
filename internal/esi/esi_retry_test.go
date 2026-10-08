package esi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// scriptedTransport answers requests from a script: the nth call
// gets the nth answer (the last repeats). Failures are transport
// errors; a nil error with a status is an ESI answer.
type scriptedTransport struct {
	calls atomic.Int64
	next  atomic.Int64 // next answer index, claimed atomically
	steps []scriptedStep
	seen  *[]string // request URLs, in order (optional)
}

type scriptedStep struct {
	status int
	header http.Header
	body   string
	err    error
}

func (s *scriptedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.calls.Add(1)
	i := int(s.next.Add(1)) - 1
	if i >= len(s.steps) {
		i = len(s.steps) - 1
	}
	step := s.steps[i]
	if s.seen != nil {
		*s.seen = append(*s.seen, req.URL.String())
	}
	if step.err != nil {
		return nil, step.err
	}
	header := step.header
	if header == nil {
		header = make(http.Header)
	}
	return &http.Response{
		StatusCode: step.status,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(step.body)),
	}, nil
}

// fastRetries runs retries without sleeping: the production
// backoffs would stall the suite 1.5s per failing read.
func fastRetries(t *testing.T) {
	t.Helper()
	old := sendRetryBackoffs
	sendRetryBackoffs = []time.Duration{0, 0}
	t.Cleanup(func() { sendRetryBackoffs = old })
}

// TestSendRetriesReads: a GET that 503s twice then answers
// succeeds after three attempts.
func TestSendRetriesReads(t *testing.T) {
	fastRetries(t)
	transport := &scriptedTransport{steps: []scriptedStep{
		{status: http.StatusServiceUnavailable, body: `{"error":"wobble"}`},
		{status: http.StatusServiceUnavailable, body: `{"error":"wobble"}`},
		{status: http.StatusOK, body: `{"ok":true}`},
	}}
	c := New(&http.Client{Transport: transport}, nil, nil)

	body, _, status, err := c.send(context.Background(), http.MethodGet, "", "/x/", nil, "", http.StatusOK)
	if err != nil {
		t.Fatalf("GET after two 503s: %v", err)
	}
	if status != http.StatusOK || string(body) != `{"ok":true}` {
		t.Fatalf("GET after two 503s = %d %q", status, body)
	}
	if got := transport.calls.Load(); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
}

// TestSendRetryLimit: three 503s in a row give up with a
// StatusError, and a 4xx is never retried at all.
func TestSendRetryLimit(t *testing.T) {
	fastRetries(t)
	transport := &scriptedTransport{steps: []scriptedStep{
		{status: http.StatusServiceUnavailable, body: `{}`},
	}}
	c := New(&http.Client{Transport: transport}, nil, nil)
	_, _, status, err := c.send(context.Background(), http.MethodGet, "", "/x/", nil, "", http.StatusOK)
	var se *StatusError
	if !errors.As(err, &se) || status != http.StatusServiceUnavailable {
		t.Fatalf("persistent 503 = %d, %v; want 503 StatusError", status, err)
	}
	if got := transport.calls.Load(); got != 3 {
		t.Fatalf("attempts = %d, want 3 (initial plus two retries)", got)
	}

	transport404 := &scriptedTransport{steps: []scriptedStep{
		{status: http.StatusNotFound, body: `{}`},
	}}
	c404 := New(&http.Client{Transport: transport404}, nil, nil)
	if _, _, _, err := c404.send(context.Background(), http.MethodGet, "", "/x/", nil, "", http.StatusOK); err == nil {
		t.Fatal("404 unexpectedly succeeded")
	}
	if got := transport404.calls.Load(); got != 1 {
		t.Fatalf("404 attempts = %d, want 1 (never retried)", got)
	}
}

// TestSendNeverRetriesWrites: a POST that 500s tries once. A
// repeated POST could send a mail twice.
func TestSendNeverRetriesWrites(t *testing.T) {
	fastRetries(t)
	transport := &scriptedTransport{steps: []scriptedStep{
		{status: http.StatusInternalServerError, body: `{}`},
		{status: http.StatusOK, body: `{"ok":true}`},
	}}
	c := New(&http.Client{Transport: transport}, nil, nil)
	if _, _, _, err := c.send(context.Background(), http.MethodPost, "", "/x/", map[string]string{"a": "b"}, "", http.StatusOK); err == nil {
		t.Fatal("POST 500 unexpectedly succeeded")
	}
	if got := transport.calls.Load(); got != 1 {
		t.Fatalf("POST attempts = %d, want 1 (never retried)", got)
	}
}

// TestSendHonorsRetryAfter: a 503 carrying Retry-After is retried
// (the header's wait, not the backoff), and the error limit still
// surfaces as ErrErrorLimit without a retry.
func TestSendHonorsRetryAfter(t *testing.T) {
	fastRetries(t)
	after := http.Header{"Retry-After": []string{"0"}}
	transport := &scriptedTransport{steps: []scriptedStep{
		{status: http.StatusServiceUnavailable, header: after, body: `{}`},
		{status: http.StatusOK, body: `{"ok":true}`},
	}}
	c := New(&http.Client{Transport: transport}, nil, nil)
	if _, _, _, err := c.send(context.Background(), http.MethodGet, "", "/x/", nil, "", http.StatusOK); err != nil {
		t.Fatalf("GET with Retry-After: %v", err)
	}
	if got := transport.calls.Load(); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}

	limited := &scriptedTransport{steps: []scriptedStep{
		{status: http.StatusTooManyRequests, body: `{}`},
		{status: http.StatusOK, body: `{"ok":true}`},
	}}
	cl := New(&http.Client{Transport: limited}, nil, nil)
	if _, _, _, err := cl.send(context.Background(), http.MethodGet, "", "/x/", nil, "", http.StatusOK); !errors.Is(err, ErrErrorLimit) {
		t.Fatalf("429 = %v, want ErrErrorLimit", err)
	}
	if got := limited.calls.Load(); got != 1 {
		t.Fatalf("429 attempts = %d, want 1 (the worker backs off by contract)", got)
	}
}

func TestParseRetryAfter(t *testing.T) {
	if wait, ok := parseRetryAfter("3"); !ok || wait != 3*time.Second {
		t.Fatalf(`parseRetryAfter("3") = %v, %v`, wait, ok)
	}
	if _, ok := parseRetryAfter(""); ok {
		t.Fatal("empty Retry-After parsed, want false")
	}
	if _, ok := parseRetryAfter("soon"); ok {
		t.Fatal("junk Retry-After parsed, want false")
	}
	when := time.Now().Add(90 * time.Second).UTC().Format(http.TimeFormat)
	if wait, ok := parseRetryAfter(when); !ok || wait <= 0 || wait > 95*time.Second {
		t.Fatalf("HTTP-date Retry-After = %v, %v", wait, ok)
	}
}

// TestRetryWaitCapsRetryAfter: ESI asking for a long wait is cut to
// maxRetryAfter, so two retries cannot stall one dataset for minutes.
func TestRetryWaitCapsRetryAfter(t *testing.T) {
	header := http.Header{"Retry-After": []string{"3600"}}
	err := &StatusError{Code: http.StatusServiceUnavailable}
	wait, ok := retryWait(err, header, time.Second)
	if !ok || wait != maxRetryAfter {
		t.Fatalf("retryWait with Retry-After 3600 = %v, %v; want %v, true", wait, ok, maxRetryAfter)
	}
}

// TestStatusErrorCarriesESIText: an error answer's "error" text rides
// on the StatusError (shown to readers on refused writes and kept in
// the log line), while an answer with no JSON error text leaves the
// message in its plain form.
func TestStatusErrorCarriesESIText(t *testing.T) {
	fastRetries(t)
	transport := &scriptedTransport{steps: []scriptedStep{
		{status: http.StatusBadRequest, body: `{"error":"recipient is not a valid mail target"}`},
	}}
	c := New(&http.Client{Transport: transport}, nil, nil)
	_, _, _, err := c.send(context.Background(), http.MethodPost, "tok", "/characters/1/mail/", map[string]int{"x": 1}, "", http.StatusCreated)
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusBadRequest || se.Detail != "recipient is not a valid mail target" {
		t.Fatalf("err = %v (%#v), want a 400 StatusError carrying ESI's text", err, se)
	}
	if want := "ESI POST /characters/1/mail/: status 400: recipient is not a valid mail target"; err.Error() != want {
		t.Fatalf("Error() = %q, want %q", err.Error(), want)
	}

	plain := &scriptedTransport{steps: []scriptedStep{{status: http.StatusNotFound, body: `<html>nope</html>`}}}
	c2 := New(&http.Client{Transport: plain}, nil, nil)
	_, _, _, err = c2.send(context.Background(), http.MethodGet, "", "/x/", nil, "", http.StatusOK)
	if want := "ESI GET /x/: status 404"; err == nil || err.Error() != want {
		t.Fatalf("non-JSON error answer: %v, want %q", err, want)
	}
}
