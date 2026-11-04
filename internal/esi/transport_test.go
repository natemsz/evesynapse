package esi

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// deadlineTransport answers 200 and records the deadline each
// request's context carried, so a test can see which client's
// overall timeout a read rode.
type deadlineTransport struct {
	calls       int
	hasDeadline bool
	deadline    time.Time
}

func (d *deadlineTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	d.calls++
	d.deadline, d.hasDeadline = req.Context().Deadline()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`[]`)),
		Request:    req,
	}, nil
}

// TestFetchRawLongCarriesTheBulkDeadline: the sweep's bulk page
// read goes through FetchRawLong and carries the long bulk
// deadline, while an ordinary FetchRaw keeps the shared client's
// 10s deadline.
func TestFetchRawLongCarriesTheBulkDeadline(t *testing.T) {
	if got := NewBulkHTTPClient().Timeout; got != 45*time.Second {
		t.Errorf("bulk client timeout = %v, want 45s", got)
	}
	if got := NewHTTPClient().Timeout; got != 10*time.Second {
		t.Errorf("shared client timeout = %v, want 10s", got)
	}

	shared, bulk := &deadlineTransport{}, &deadlineTransport{}
	client := New(&http.Client{Timeout: 10 * time.Second, Transport: shared}, nil, nil)
	client.SetBulkHTTPClient(&http.Client{Timeout: bulkRequestTimeout, Transport: bulk})
	ctx := context.Background()

	if _, _, err := client.FetchRaw(ctx, "", "/markets/10000002/orders/"); err != nil {
		t.Fatalf("FetchRaw: %v", err)
	}
	if shared.calls != 1 || bulk.calls != 0 {
		t.Fatalf("FetchRaw rode shared %d times and bulk %d times, want 1 and 0", shared.calls, bulk.calls)
	}
	if !shared.hasDeadline {
		t.Fatal("FetchRaw carried no deadline")
	}
	if until := time.Until(shared.deadline); until <= 5*time.Second || until > 11*time.Second {
		t.Errorf("FetchRaw deadline is %v out, want about 10s", until)
	}

	if _, _, err := client.FetchRawLong(ctx, "", "/markets/10000002/orders/"); err != nil {
		t.Fatalf("FetchRawLong: %v", err)
	}
	if bulk.calls != 1 || shared.calls != 1 {
		t.Fatalf("FetchRawLong rode bulk %d times and shared %d times, want 1 and 1", bulk.calls, shared.calls)
	}
	if !bulk.hasDeadline {
		t.Fatal("FetchRawLong carried no deadline")
	}
	if until := time.Until(bulk.deadline); until <= 40*time.Second || until > 46*time.Second {
		t.Errorf("FetchRawLong deadline is %v out, want about 45s", until)
	}
}

// TestNewHTTPClientIsTuned: the production ESI client gets its own
// tuned transport (the bare login client left it at Go's default of
// two idle connections per host). Every pool value is pinned so a
// refactor cannot silently hand ESI back the defaults.
func TestNewHTTPClientIsTuned(t *testing.T) {
	d := esiDialer()
	if d.Timeout != 5*time.Second || d.KeepAlive != 30*time.Second {
		t.Errorf("dialer = %+v, want a 5s dial timeout and a 30s keep-alive", d)
	}

	client := NewHTTPClient()
	if client.Timeout != 10*time.Second {
		t.Errorf("client timeout = %v, want the 10s discipline the worker pacing assumes", client.Timeout)
	}
	tr, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("client transport is %T, want the tuned *http.Transport", client.Transport)
	}
	if tr.MaxIdleConnsPerHost != 128 {
		t.Errorf("MaxIdleConnsPerHost = %d, want 128 (not Go's default 2)", tr.MaxIdleConnsPerHost)
	}
	if tr.MaxIdleConns != 128 {
		t.Errorf("MaxIdleConns = %d, want 128 (at least the per-host pool)", tr.MaxIdleConns)
	}
	if tr.IdleConnTimeout != 90*time.Second {
		t.Errorf("IdleConnTimeout = %v, want 90s", tr.IdleConnTimeout)
	}
	if !tr.ForceAttemptHTTP2 {
		t.Error("ForceAttemptHTTP2 is off")
	}
	if tr.TLSHandshakeTimeout != 5*time.Second {
		t.Errorf("TLSHandshakeTimeout = %v, want 5s", tr.TLSHandshakeTimeout)
	}
	if tr.ResponseHeaderTimeout != 8*time.Second {
		t.Errorf("ResponseHeaderTimeout = %v, want 8s", tr.ResponseHeaderTimeout)
	}
	if tr.ExpectContinueTimeout != time.Second {
		t.Errorf("ExpectContinueTimeout = %v, want 1s", tr.ExpectContinueTimeout)
	}
	if tr.DialContext == nil {
		t.Error("the transport has no dialer; the keep-alive dialer is missing")
	}

	// The client New ends up with is the tuned one.
	c := New(client, nil, nil)
	if c.http != client {
		t.Error("New did not keep the HTTP client it was given")
	}
}
