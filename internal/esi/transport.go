package esi

import (
	"crypto/tls"
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
// A bare http.Client with no Transport gives esi.evetech.net Go's
// default of two idle connections per host, and the worker's
// parallel fetches queue behind each other. This client is ESI's
// own (not the SSO/JWKS client), tuned the way the
// swarm peer client is: a real idle pool, HTTP/2 where offered, and
// short handshake and header waits under the same 10s overall
// request timeout the worker's pacing assumes. The login/SSO client
// is untouched, and nothing here changes how responses are read
// (the error budget and 420/429 handling live in the caller).
func NewHTTPClient() *http.Client {
	return &http.Client{Timeout: 10 * time.Second, Transport: newESITransport()}
}

// bulkRequestTimeout is the overall deadline for a bulk read
// (FetchRawLong): a whole-region market page is a few hundred KB
// before compression, and a slow route can stretch one well past the
// shared client's 10s fast failure. Small calls keep the 10s
// deadline; only the bulk read gets this one.
const bulkRequestTimeout = 45 * time.Second

// bulkMaxConns caps the bulk client's connections to ESI: the
// region sweep's page windows across the hub regions (5 regions at
// up to sweepPageWindow pages) plus the guide-price read, with room
// to spare and no more.
const bulkMaxConns = 32

// NewBulkHTTPClient builds the HTTP client bulk ESI reads ride: the
// tuned transport of NewHTTPClient on HTTP/1.1 with a pool of up to
// bulkMaxConns connections, and the long bulk deadline
// (bulkRequestTimeout) in place of the 10s one. ESI paces each
// connection on its own (measured from the app host: 30 whole-book
// pages over one HTTP/2 connection took 17s together, 29 of them
// past 10s; over a pool, 2 to 3s with none past 10s), so parallel
// pages need parallel connections. Bulk reads on their own
// connections also stop starving the shared client's small calls.
func NewBulkHTTPClient() *http.Client {
	transport := newESITransport()
	// HTTP/1.1, one connection per request in flight: ESI paces
	// each connection on its own, and over HTTP/2 every bulk page
	// shares one connection's pace. An empty TLSNextProto keeps
	// the transport off HTTP/2 even where the server offers it.
	transport.ForceAttemptHTTP2 = false
	transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	transport.MaxConnsPerHost = bulkMaxConns
	transport.MaxIdleConnsPerHost = bulkMaxConns
	return &http.Client{Timeout: bulkRequestTimeout, Transport: transport}
}
