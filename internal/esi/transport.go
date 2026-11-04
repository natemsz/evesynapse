package esi

import (
	"net"
	"net/http"
	"time"
)

// esiDialer is the connection dialer under every production ESI
// request: a short dial timeout so a dead route fails fast, and a
// 30s keep-alive so pooled connections stay warm between cycles.
func esiDialer() *net.Dialer {
	return &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
}

// newESITransport builds the tuned transport under every production
// ESI client: a real idle pool, HTTP/2 where offered, and short
// handshake and header waits. The overall request deadline is the
// client's, not the transport's, so the shared client and the bulk
// client ride the same transport tuning at different deadlines.
func newESITransport() *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           esiDialer().DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          128,
		MaxIdleConnsPerHost:   128,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 8 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

// NewHTTPClient builds the HTTP client production ESI requests ride.
// ESI used to share the SSO/JWKS client — a bare http.Client with no
// Transport, so esi.evetech.net got Go's default of two idle
// connections per host and the worker's parallel fetches queued
// behind each other. This client is ESI's own, tuned the way the
// swarm peer client is: a real idle pool, HTTP/2 where offered, and
// short handshake and header waits under the same 10s overall
// request timeout the worker's pacing assumes. The login/SSO client
// is untouched, and nothing here changes how responses are read
// (the error budget and 420/429 handling live in the caller).
func NewHTTPClient() *http.Client {
	return &http.Client{Timeout: 10 * time.Second, Transport: newESITransport()}
}

// bulkRequestTimeout is the overall deadline for a bulk read
// (FetchRawLong): a whole-region market page is a few hundred KB,
// and on a slow route several parallel pages share the link, so a
// page legitimately needs far longer than the shared client's 10s
// fast failure. Small calls keep the 10s deadline; only the bulk
// read gets this one.
const bulkRequestTimeout = 45 * time.Second

// NewBulkHTTPClient builds the HTTP client bulk ESI reads ride: the
// same tuned transport as NewHTTPClient, with the long bulk
// deadline (bulkRequestTimeout) in place of the 10s one.
func NewBulkHTTPClient() *http.Client {
	return &http.Client{Timeout: bulkRequestTimeout, Transport: newESITransport()}
}
