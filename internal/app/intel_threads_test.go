package app

import (
	"context"
	"testing"
	"time"
)

// TestIntelThreads pins the thread rule: a group on a neighbouring
// system inside the window joins the thread, and one past the
// window, two jumps away, or in another channel founds its own.
func TestIntelThreads(t *testing.T) {
	f := seedGroupFixture(t)
	ctx := context.Background()
	conn := f.app.db
	for _, s := range []string{
		`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30004761, 'Far Away', 10000060, -0.5) ON CONFLICT DO NOTHING`,
		`INSERT INTO sde_system_jumps (from_system_id, to_system_id) VALUES (30004759, 30004760), (30004760, 30004759), (30004760, 30004761), (30004761, 30004760) ON CONFLICT DO NOTHING`,
	} {
		if _, err := conn.ExecContext(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	thread := func(reportID int64) int64 {
		t.Helper()
		tid, err := f.q.GetIntelGroupThread(ctx, f.groupOf(t, reportID))
		if err != nil {
			t.Fatal(err)
		}
		return tid
	}
	sabre := grpDScan([]scanCount{{ID: 22456, Name: "Sabre", Class: "Interdictor", N: 1, Grid: 1}})
	raw := "22456\tAlpha Sabre\tSabre\t-\n"

	r1 := f.postScanReport(t, f.chanA, grpSystem, "thrd00000001", sabre, raw, "Sabre in 1DQ1-A", f.base)
	if tid := thread(r1); tid != f.groupOf(t, r1) {
		t.Fatalf("the founder's thread is its group: %d", tid)
	}
	// One jump, ten minutes later: the same thread.
	r2 := f.postScanReport(t, f.chanA, grpSystem2, "thrd00000002", sabre, raw, "Sabre in GE-8JZ", f.base.Add(10*time.Minute))
	if thread(r2) != thread(r1) {
		t.Errorf("a neighbour inside the window: thread %d, want %d", thread(r2), thread(r1))
	}
	// Another jump on, from the second group: still the thread.
	r3 := f.postScanReport(t, f.chanA, 30004761, "thrd00000003", sabre, raw, "Sabre in Far Away", f.base.Add(20*time.Minute))
	if thread(r3) != thread(r1) {
		t.Errorf("following the gang one more jump: thread %d, want %d", thread(r3), thread(r1))
	}
	// Two jumps from the first system and past the window of
	// the others: its own.
	r4 := f.postScanReport(t, f.chanA, 30004761, "thrd00000004", sabre, raw, "Sabre in Far Away", f.base.Add(3*time.Hour))
	if thread(r4) == thread(r1) {
		t.Error("a group past the window joined the thread")
	}
	// Another channel never joins.
	r5 := f.postScanReport(t, f.chanB, grpSystem2, "thrd00000005", sabre, raw, "Sabre in GE-8JZ", f.base.Add(12*time.Minute))
	if thread(r5) == thread(r1) {
		t.Error("another channel's group joined the thread")
	}
}
