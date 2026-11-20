package netguard

import (
	"context"
	"net"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// TestDialerSetsUserTimeout: a connection from the guarded dialer
// carries the kernel's unacknowledged-data limit, so a dead path
// closes it in UserTimeout instead of minutes of retransmission.
func TestDialerSetsUserTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	conn, err := Dialer(UserTimeout).DialContext(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	raw, err := conn.(*net.TCPConn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var got int
	var gerr error
	if err := raw.Control(func(fd uintptr) {
		got, gerr = unix.GetsockoptInt(int(fd), syscall.IPPROTO_TCP, unix.TCP_USER_TIMEOUT)
	}); err != nil {
		t.Fatal(err)
	}
	if gerr != nil {
		t.Fatal(gerr)
	}
	if want := int(UserTimeout.Milliseconds()); got != want {
		t.Errorf("TCP_USER_TIMEOUT = %d ms, want %d", got, want)
	}
}
