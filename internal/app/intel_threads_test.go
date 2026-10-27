package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
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

// TestIntelDashboardRows: the dashboard reads one row per thread,
// by its newest report, with the systems it spans, and the page
// renders (rows or none) with the compact map.
func TestIntelDashboardRows(t *testing.T) {
	f := seedGroupFixture(t)
	ctx := context.Background()
	if _, err := f.app.db.ExecContext(ctx, `INSERT INTO sde_system_jumps (from_system_id, to_system_id) VALUES (30004759, 30004760), (30004760, 30004759) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	sabre := grpDScan([]scanCount{{ID: 22456, Name: "Sabre", Class: "Interdictor", N: 1, Grid: 1}})
	raw := "22456\tAlpha Sabre\tSabre\t-\n"
	f.postScanReport(t, f.chanA, grpSystem, "dash00000001", sabre, raw, "Sabre in 1DQ1-A", now.Add(-20*time.Minute))
	r2 := f.postScanReport(t, f.chanA, grpSystem2, "dash00000002", sabre, raw, "Sabre in GE-8JZ", now.Add(-10*time.Minute))
	f.postScanReport(t, f.chanB, grpSystem, "dash00000003", sabre, raw, "Sabre in 1DQ1-A", now.Add(-5*time.Minute))

	chans := []accessChannel{{ListAccessibleChannelsRow: db.ListAccessibleChannelsRow{ID: f.chanA, Name: "Group A"}}}
	rows := f.app.dashboardThreads(ctx, chans, time.Now())
	if len(rows) != 1 {
		t.Fatalf("rows: %+v, want one thread (channel B is not the reader's)", rows)
	}
	if rows[0].ReportID != r2 || rows[0].SystemName != "GE-8JZ" || rows[0].Systems != 2 || rows[0].ChName != "Group A" {
		t.Errorf("row: %+v, want the newest report on GE-8JZ, spanning 2 systems", rows[0])
	}

	req := httptest.NewRequest(http.MethodGet, "/intel/workbench/", nil)
	req.AddCookie(sessionCookie(t, f.app, f.ownerID, f.charID, "Group Owner"))
	rec := httptest.NewRecorder()
	f.app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard: status %d", rec.Code)
	}
	body := rec.Body.String()
	// The bare workbench opens the newest thread: its row marked,
	// its report broken down below with the verdict strip.
	for _, want := range []string{"data-wbd", "data-map-compact", "data-ch-feed", "/static/dashboard.js", "wbd-row is-open", "wb-verdict", "/static/workbench.js"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard page lacks %q", want)
		}
	}
}
