package netguard

import (
	"time"

	"golang.org/x/sys/unix"
)

func setUserTimeout(fd uintptr, d time.Duration) error {
	return unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_USER_TIMEOUT, int(d.Milliseconds()))
}
