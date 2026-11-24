package app

import (
	"testing"
	"time"
)

// TestSDECheckDueRunsAfterDowntimeUntilImported pins the SDE check
// schedule: checks start at downtime, repeat every sdeCheckEvery
// within sdeCheckWindow, stop for the day once an import lands, and a
// worker that missed the window catches up with one check.
func TestSDECheckDueRunsAfterDowntimeUntilImported(t *testing.T) {
	at := func(day, hour, min int) time.Time {
		return time.Date(2026, time.November, day, hour, min, 0, 0, time.UTC)
	}
	never := time.Time{}
	cases := []struct {
		name                   string
		now, check, lastImport time.Time
		want                   bool
	}{
		{"first tick after downtime checks", at(24, 11, 5), at(23, 16, 30), at(20, 11, 40), true},
		{"within the window a recent check waits", at(24, 11, 20), at(24, 11, 5), at(20, 11, 40), false},
		{"within the window checks repeat every interval", at(24, 11, 35), at(24, 11, 5), at(20, 11, 40), true},
		{"an import since downtime stops checks", at(24, 12, 30), at(24, 11, 35), at(24, 11, 36), false},
		{"an import since downtime stops checks before the next downtime", at(25, 9, 0), at(24, 11, 35), at(24, 11, 36), false},
		{"past the window with a check since downtime stays quiet", at(24, 17, 30), at(24, 16, 50), at(20, 11, 40), false},
		{"past the window without a check since downtime catches up once", at(24, 20, 0), at(23, 12, 0), at(20, 11, 40), true},
		{"before downtime the previous day's window applies", at(25, 3, 0), at(24, 16, 50), at(20, 11, 40), false},
		{"the next downtime opens a new window", at(25, 11, 5), at(24, 16, 50), at(24, 11, 36), true},
		{"never checked or imported checks", at(24, 20, 0), never, never, true},
	}
	for _, c := range cases {
		if got := sdeCheckDue(c.now, c.check, c.lastImport); got != c.want {
			t.Errorf("%s: sdeCheckDue = %v, want %v", c.name, got, c.want)
		}
	}
}
