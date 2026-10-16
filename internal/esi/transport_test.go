package esi

import (
	"net/http"
	"testing"
	"time"
)

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
	if tr.MaxIdleConns != 100 {
		t.Errorf("MaxIdleConns = %d, want 100", tr.MaxIdleConns)
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
