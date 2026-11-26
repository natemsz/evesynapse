package calendar

import (
	"testing"
	"time"
)

// TestMonthGridAndAddress: the month grid is whole weeks from Monday,
// and an unreadable month address reads as the current month.
func TestMonthGridAndAddress(t *testing.T) {
	from, to := calGridRange(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	if from.Format("Mon 2006-01-02") != "Mon 2026-09-28" || to.Format("Mon 2006-01-02") != "Mon 2026-11-02" {
		t.Fatalf("October 2026 grid runs %s to %s", from.Format("Mon 2006-01-02"), to.Format("Mon 2006-01-02"))
	}
	if got := parseCalMonth("nonsense", time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)).Format(calMonthLayout); got != "2026-10" {
		t.Fatalf("an unreadable month gave %s, want the current one", got)
	}
}
