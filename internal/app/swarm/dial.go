package swarm

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

// MaxSwarmRedirects bounds same-host redirects (a trailing slash,
// a canonical path); nothing legitimate needs more.
const MaxSwarmRedirects = 3

// SwarmLoopbackKey marks a request whose URL names loopback
// itself, the one case where a loopback connection is allowed.
type SwarmLoopbackKey struct{}

// SwarmCGNAT is carrier-grade NAT space (RFC 6598): not private
// by the standard library's definition, and not public either.
var SwarmCGNAT = netip.MustParsePrefix("100.64.0.0/10")

// SwarmPublicIP reports whether ip is an address on the public
// internet: global unicast, and none of the private, link-local,
// loopback, shared or unspecified ranges.
func SwarmPublicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || !ip.IsGlobalUnicast() {
		return false
	}
	return !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !SwarmCGNAT.Contains(ip)
}

// SwarmDialControl runs on every connection the swarm client
// opens, after the name has resolved and before anything is
// sent, and refuses an address the rules above do not allow.
func SwarmDialControl(ctx context.Context, _, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("swarm: refusing to dial %q: %w", address, err)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("swarm: refusing to dial %q: not an IP address", address)
	}
	if ip.Unmap().IsLoopback() {
		if allowed, _ := ctx.Value(SwarmLoopbackKey{}).(bool); allowed {
			return nil
		}
		return fmt.Errorf("swarm: refusing to dial %s: a peer address resolved to this machine", ip)
	}
	if !SwarmPublicIP(ip) {
		return fmt.Errorf("swarm: refusing to dial %s: not a public address", ip)
	}
	return nil
}

// SwarmCheckRedirect follows a redirect only within the scheme
// and host that were asked, a few times at most.
func SwarmCheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return nil
	}
	if len(via) > MaxSwarmRedirects {
		return errors.New("swarm: too many redirects")
	}
	asked := via[0].URL
	if req.URL.Scheme != asked.Scheme || req.URL.Host != asked.Host {
		return fmt.Errorf("swarm: refusing a redirect from %s to %s", asked.Host, req.URL.Host)
	}
	return nil
}

// SwarmTransport marks loopback-URL requests for the dial check
// and hands everything to the guarded transport.
type SwarmTransport struct {
	Base *http.Transport
}

func (t SwarmTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if SwarmHostIsLoopback(req.URL.Hostname()) {
		req = req.WithContext(context.WithValue(req.Context(), SwarmLoopbackKey{}, true))
	}
	return t.Base.RoundTrip(req)
}

// NewSwarmClient builds the peer client: bearer secret, the
// guarded dialer, the same-host redirect rule, no proxy.
func NewSwarmClient(secret string) *SwarmClient {
	dialer := &net.Dialer{Timeout: 10 * time.Second, ControlContext: SwarmDialControl}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: SwarmHTTPTimeout,
		MaxIdleConns:          16,
		IdleConnTimeout:       90 * time.Second,
	}
	return &SwarmClient{Secret: secret, Http: &http.Client{
		Timeout:       SwarmHTTPTimeout,
		Transport:     SwarmTransport{Base: transport},
		CheckRedirect: SwarmCheckRedirect,
	}}
}
