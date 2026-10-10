package esi

import (
	"testing"
	"time"
)

func TestCallMeterDayWindow(t *testing.T) {
	c := &Client{}
	base := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	c.noteCallAt(base.Add(-25 * time.Hour))
	c.noteCallAt(base.Add(-23 * time.Hour))
	c.noteCallAt(base.Add(-time.Hour))
	c.noteCallAt(base)
	if got := c.CallsTotal(); got != 4 {
		t.Fatalf("total = %d, want 4", got)
	}
	if got := c.callsLastDayAt(base); got != 3 {
		t.Fatalf("last day = %d, want 3 (the 25h-old call falls out)", got)
	}
	// An hour later the window slides again without new calls.
	if got := c.callsLastDayAt(base.Add(time.Hour)); got != 2 {
		t.Fatalf("last day an hour later = %d, want 2", got)
	}
}
