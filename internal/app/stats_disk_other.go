//go:build !(linux || darwin)

package app

// diskUsage has no disk figure to report off Linux and macOS (the
// host sample reads /proc anyway, so it is a production-box figure)
// and returns zeros, as a failed read does on Linux.
func diskUsage(string) (used, total int64) { return 0, 0 }
