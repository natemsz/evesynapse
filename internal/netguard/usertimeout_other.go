//go:build !linux

package netguard

import "time"

// setUserTimeout has no portable equivalent off Linux; the HTTP/2
// health checks still apply there.
func setUserTimeout(uintptr, time.Duration) error { return nil }
