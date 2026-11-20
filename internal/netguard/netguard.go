// Package netguard keeps long-lived outbound connections from
// hanging on a dead network path. A network blip can drop the state a
// firewall or NAT holds for a connection; after that the far side
// never answers and never resets, and without a guard the connection
// stays open for many minutes while every request sent on it times
// out. Two guards close such a connection within seconds, so the
// next request dials a fresh one:
//
//   - HTTP/2 health checks (HTTP2): a connection that has read
//     nothing for a while is pinged, and one that misses the ping or
//     stops taking writes is closed.
//   - A kernel limit on unacknowledged data (Control, on Linux): a
//     socket whose sent bytes go unacknowledged for UserTimeout is
//     closed by the kernel. This covers HTTP/1.1 connections too.
package netguard

import (
	"context"
	"net"
	"net/http"
	"syscall"
	"time"
)

// UserTimeout is how long sent data may go unacknowledged before the
// kernel closes the socket. Well above any real round trip, well
// under the many minutes the kernel's own retransmission takes.
const UserTimeout = 20 * time.Second

// HTTP/2 health check timing: ping after pingAfter with nothing
// read, close if the ping is not answered within pingTimeout, and
// close if a pending write makes no progress for writeTimeout.
const (
	pingAfter    = 15 * time.Second
	pingTimeout  = 5 * time.Second
	writeTimeout = 15 * time.Second
)

// HTTP2 is the HTTP/2 configuration with health checks on.
func HTTP2() *http.HTTP2Config {
	return &http.HTTP2Config{
		SendPingTimeout:  pingAfter,
		PingTimeout:      pingTimeout,
		WriteByteTimeout: writeTimeout,
	}
}

// Control is a net.Dialer Control function that sets the kernel's
// unacknowledged-data limit on each new TCP socket.
func Control(network, address string, c syscall.RawConn) error {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil
	}
	var serr error
	if err := c.Control(func(fd uintptr) { serr = setUserTimeout(fd, UserTimeout) }); err != nil {
		return err
	}
	return serr
}

// ControlContext chains Control after another dialer check, for a
// dialer that already carries one (a net.Dialer ignores Control when
// ControlContext is set).
func ControlContext(next func(ctx context.Context, network, address string, c syscall.RawConn) error) func(ctx context.Context, network, address string, c syscall.RawConn) error {
	return func(ctx context.Context, network, address string, c syscall.RawConn) error {
		if next != nil {
			if err := next(ctx, network, address, c); err != nil {
				return err
			}
		}
		return Control(network, address, c)
	}
}

// Dialer is a net.Dialer with the given dial timeout, TCP keep-alive
// and the unacknowledged-data limit.
func Dialer(timeout time.Duration) *net.Dialer {
	return &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second, Control: Control}
}

// DefaultTransport is Go's default transport with both guards:
// the guarded dialer and health-checked HTTP/2.
func DefaultTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = Dialer(30 * time.Second).DialContext
	t.HTTP2 = HTTP2()
	return t
}
