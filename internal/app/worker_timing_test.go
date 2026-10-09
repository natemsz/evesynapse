package app

import (
	"strings"
	"testing"
	"time"
)

func TestShortDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		-time.Second:                   "0.0s",
		400 * time.Millisecond:         "0.4s",
		9900 * time.Millisecond:        "9.9s",
		12 * time.Second:               "12s",
		3*time.Minute + 20*time.Second: "3m 20s",
		2*time.Hour + 5*time.Minute:    "2h 5m",
	} {
		if got := shortDuration(d); got != want {
			t.Errorf("shortDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestPhaseClock(t *testing.T) {
	start := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	c := newPhaseClock(start)
	c.mark("ordering", start.Add(200*time.Millisecond))
	c.mark("characters", start.Add(5*time.Second))
	if len(c.phases) != 2 || c.phases[0].Took != 200*time.Millisecond || c.phases[1].Took != 4800*time.Millisecond {
		t.Fatalf("phases %+v", c.phases)
	}
}

// TestWorkerTimingView: the Sync page's lines say what the last cycle
// did and where its time went, sum up the recent ones, and say so
// plainly when the worker is not keeping up.
func TestWorkerTimingView(t *testing.T) {
	if workerTimingViewFor(workerStatus{}) != nil {
		t.Fatal("something is shown before any cycle has finished")
	}
	at := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	easy := workerTiming{
		At: at, Took: 3200 * time.Millisecond, Characters: 14, Fetches: 37, Budget: 120, Overdue: 2 * time.Minute,
		Phases: []phaseTiming{{"ordering", 10 * time.Millisecond}, {"characters", 2500 * time.Millisecond}, {"notifications", 600 * time.Millisecond}},
	}
	v := workerTimingViewFor(workerStatus{Timing: easy, Recent: []workerTiming{easy}})
	if want := "took 3.2s · 14 characters · 37 of 120 fetches · stalest data 2m 0s past its refresh time"; v.Last != want {
		t.Errorf("last cycle: %q, want %q", v.Last, want)
	}
	// Longest first; a part too short to matter is left out.
	if got := strings.Join(v.Phases, ", "); got != "characters 2.5s, notifications 0.6s" {
		t.Errorf("phases: %q", got)
	}
	if v.Lately != "" || v.Behind != "" {
		t.Errorf("one cycle is not a trend: lately %q, behind %q", v.Lately, v.Behind)
	}

	// An hour of full allowances and characters put off.
	busy := workerTiming{At: at, Took: 48 * time.Second, Characters: 900, Fetches: 120, Budget: 120, Deferred: 700, Overdue: 3 * time.Hour}
	recent := []workerTiming{easy}
	for i := 0; i < 9; i++ {
		recent = append(recent, busy)
	}
	v = workerTimingViewFor(workerStatus{Timing: busy, Recent: recent})
	for _, want := range []string{"900 characters", "120 of 120 fetches", "700 put off to the next cycle", "3h 0m past its refresh time"} {
		if !strings.Contains(v.Last, want) {
			t.Errorf("last cycle %q is missing %q", v.Last, want)
		}
	}
	for _, want := range []string{"last 10 cycles", "longest 48s", "allowance used up in 9", "characters put off in 9", "up to 3h 0m late"} {
		if !strings.Contains(v.Lately, want) {
			t.Errorf("lately %q is missing %q", v.Lately, want)
		}
	}
	if !strings.Contains(v.Behind, "not keeping up") || !strings.Contains(v.Behind, "9 of the last 10") {
		t.Errorf("behind: %q", v.Behind)
	}

	// A cycle that outran the minute, without putting anyone off.
	slow := workerTiming{At: at, Took: 95 * time.Second, Characters: 14, Fetches: 30, Budget: 120}
	v = workerTimingViewFor(workerStatus{Timing: slow, Recent: []workerTiming{easy, slow}})
	if !strings.Contains(v.Behind, "longer than the minute") {
		t.Errorf("a cycle over a minute: behind %q", v.Behind)
	}
	// Data never fetched is said as that, not as a lateness.
	fresh := workerTiming{At: at, Took: time.Second, Characters: 1, Budget: 120, NeverFetched: true}
	if v = workerTimingViewFor(workerStatus{Timing: fresh}); !strings.Contains(v.Last, "some data not fetched yet") {
		t.Errorf("never fetched: %q", v.Last)
	}
}

// TestWorkerCycleIsTimed: a real cycle records its measurements, keeps
// only the recent ones, and the Sync page shows them.
func TestWorkerCycleIsTimed(t *testing.T) {
	f := newNotifyFixture(t)
	f.app.cfg.adminCharIDs = map[int64]bool{f.ch.CharacterID: true}
	cookie := sessionCookie(t, f.app, f.userID, f.ch.CharacterID, f.ch.Name)

	_, body := getPage(t, f.app, cookie, "/sync/")
	if strings.Contains(body, "Last cycle:") {
		t.Fatal("the Sync page shows a cycle before one has run")
	}

	f.app.refreshCycle(f.ctx)
	timing := f.app.snapshotWorkerStatus().Timing
	if timing.At.IsZero() || timing.Took <= 0 || timing.Characters != 1 || timing.Budget != maxFetchesPerCycle || !timing.NeverFetched {
		t.Fatalf("recorded %+v; want one character whose data had never been fetched", timing)
	}
	var names []string
	for _, p := range timing.Phases {
		names = append(names, p.Name)
	}
	if got := strings.Join(names, ","); got != "ordering,characters,op attendance,public data,names,notifications,discord" {
		t.Fatalf("phases %q", got)
	}
	if timing.Fetches < 0 || timing.Fetches > maxFetchesPerCycle {
		t.Fatalf("%d fetches counted against an allowance of %d", timing.Fetches, maxFetchesPerCycle)
	}
	_, body = getPage(t, f.app, cookie, "/sync/")
	mustContain(t, "/sync/ after a cycle", body, "Last cycle: took ", "1 character · ", " of 120 fetches")

	// Only the recent cycles are kept.
	for i := 0; i < workerRecentCycles+5; i++ {
		f.app.recordWorkerTiming(workerTiming{At: time.Now(), Took: time.Second, Budget: 120})
	}
	if n := len(f.app.snapshotWorkerStatus().Recent); n != workerRecentCycles {
		t.Fatalf("%d cycles kept, want %d", n, workerRecentCycles)
	}
}
