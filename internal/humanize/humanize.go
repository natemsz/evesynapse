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

// Rank renders an SDE skill rank (always integral in practice): "3x".
func Rank(rank float64) string {
	if rank == float64(int64(rank)) {
		return fmt.Sprintf("%dx", int64(rank))
	}
	return fmt.Sprintf("%.1fx", rank)
}

// Finish writes an ESI timestamp the way pages show times: "2006-01-02
// 15:04 UTC", the input unchanged when it is not a timestamp, "" when empty.
func Finish(raw string) string {
	if raw == "" {
		return ""
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.UTC().Format("2006-01-02 15:04 UTC")
	}
	return raw
}

// Clip cuts a string to at most max characters, adding an ellipsis.
func Clip(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// Plural is "1 order", "2 orders": the count and the word, pluralised.
func Plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}
