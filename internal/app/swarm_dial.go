package app

// ---------------------------------------------------------------------------
// Where the swarm client may connect. Peer addresses arrive from
// other instances, so the client that dials them is held to
// three rules, all enforced at the point of connection rather
// than by looking at the URL:
//
//  1. Only public addresses. The check runs on the IP actually
//     being connected to, after name resolution, so a hostname
//     that resolves inward (or is re-pointed between lookups) is
//     refused like a literal private address.
//  2. Loopback only for a loopback URL: http://127.0.0.1 and
//     localhost are for local development and tests, where the
//     operator typed the address. Any other name that resolves
//     to loopback is refused.
//  3. No redirect leaves the host that was asked. A peer cannot
//     answer with a Location that sends the next request, and
//     the bearer with it, somewhere else.
//
// The client uses no proxy: a proxy would make the connection
// itself, and the address check would only ever see the proxy.
// ---------------------------------------------------------------------------

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

// maxSwarmRedirects bounds same-host redirects (a trailing slash,
// a canonical path); nothing legitimate needs more.
const maxSwarmRedirects = 3

// swarmLoopbackKey marks a request whose URL names loopback
// itself, the one case where a loopback connection is allowed.
type swarmLoopbackKey struct{}

// swarmCGNAT is carrier-grade NAT space (RFC 6598): not private
// by the standard library's definition, and not public either.
var swarmCGNAT = netip.MustParsePrefix("100.64.0.0/10")

// swarmPublicIP reports whether ip is an address on the public
// internet: global unicast, and none of the private, link-local,
// loopback, shared or unspecified ranges.
func swarmPublicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || !ip.IsGlobalUnicast() {
		return false
	}
	return !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !swarmCGNAT.Contains(ip)
}

// swarmDialControl runs on every connection the swarm client
// opens, after the name has resolved and before anything is
// sent, and refuses an address the rules above do not allow.
func swarmDialControl(ctx context.Context, _, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("swarm: refusing to dial %q: %w", address, err)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("swarm: refusing to dial %q: not an IP address", address)
	}
	if ip.Unmap().IsLoopback() {
		if allowed, _ := ctx.Value(swarmLoopbackKey{}).(bool); allowed {
			return nil
		}
		return fmt.Errorf("swarm: refusing to dial %s: a peer address resolved to this machine", ip)
	}
	if !swarmPublicIP(ip) {
		return fmt.Errorf("swarm: refusing to dial %s: not a public address", ip)
	}
	return nil
}

// swarmCheckRedirect follows a redirect only within the scheme
// and host that were asked, a few times at most.
func swarmCheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return nil
	}
	if len(via) > maxSwarmRedirects {
		return errors.New("swarm: too many redirects")
	}
	asked := via[0].URL
	if req.URL.Scheme != asked.Scheme || req.URL.Host != asked.Host {
		return fmt.Errorf("swarm: refusing a redirect from %s to %s", asked.Host, req.URL.Host)
	}
	return nil
}

// swarmTransport marks loopback-URL requests for the dial check
// and hands everything to the guarded transport.
type swarmTransport struct {
	base *http.Transport
}

func (t swarmTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if swarmHostIsLoopback(req.URL.Hostname()) {
		req = req.WithContext(context.WithValue(req.Context(), swarmLoopbackKey{}, true))
	}
	return t.base.RoundTrip(req)
}

// newSwarmClient builds the peer client: bearer secret, the
// guarded dialer, the same-host redirect rule, no proxy.
func newSwarmClient(secret string) *swarmClient {
	dialer := &net.Dialer{Timeout: 10 * time.Second, ControlContext: swarmDialControl}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: swarmHTTPTimeout,
		MaxIdleConns:          16,
		IdleConnTimeout:       90 * time.Second,
	}
	return &swarmClient{secret: secret, http: &http.Client{
		Timeout:       swarmHTTPTimeout,
		Transport:     swarmTransport{base: transport},
		CheckRedirect: swarmCheckRedirect,
	}}
}
