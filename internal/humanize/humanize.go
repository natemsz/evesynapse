// Package humanize holds the small wording helpers every page and
// module shares: pure text, no store, no config.
package humanize

import (
	"fmt"
	"time"
)

// Duration (mirrored by plan.js dur: change both) renders a duration
// compactly for "time remaining" labels: "2d 3h", "3h 12m", "45m",
// "<1m". Non-positive durations render "<1m"; callers that care check
// the sign themselves.
func Duration(d time.Duration) string {
	if d < time.Minute {
		return "<1m"
	}
	days := int(d / (24 * time.Hour))
	hours := int((d % (24 * time.Hour)) / time.Hour)
	mins := int((d % time.Hour) / time.Minute)
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, mins)
	default:
		return fmt.Sprintf("%dm", mins)
	}
}

// PluralSuffix is "" for one and "s" otherwise.
func PluralSuffix(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
