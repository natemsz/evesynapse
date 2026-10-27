package app

import (
	"context"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// TestAxonPreviewAndOverrides: the preview says what the posting
// path would make of a paste and stores nothing; a send's overrides
// file the reading where the player said, read it only as the kind
// they said, and leave a label beside what was detected.
func TestAxonPreviewAndOverrides(t *testing.T) {
	app, conn, q := buildCorpTestApp(t, nil)
	ctx := context.Background()
	exec := func(s string, args ...any) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, s, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES
		(30002385, 'Teonusude', 10000060, -0.38), (30002386, 'Mozzidit', 10000060, -0.40) ON CONFLICT DO NOTHING`)
	const (
		teoID  = int64(30002385)
		mozID  = int64(30002386)
		poster = int64(519225918)
	)
	u, _ := q.CreateUser(ctx)
	seedCharacter(t, q, u.ID, poster, "Burzrujat")
	seedSnapshot(t, q, poster, esi.SnapLocation, esi.Location{SolarSystemID: teoID})
	cid, err := q.CreateIntelChannel(ctx, db.CreateIntelChannelParams{OwnerUserID: u.ID, Name: "Home", CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	count := func(table string) int {
		t.Helper()
		var n int
		if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// The preview: a line naming its system, and one naming none.
	if pv := app.previewSubmission(ctx, "Mozzidit Hostile One +2", "", poster); pv.Kind != submitKindChat || pv.SystemID != mozID || pv.SystemFrom != "line" || pv.SystemName != "Mozzidit" {
		t.Errorf("a line naming its system: %+v", pv)
	}
	if pv := app.previewSubmission(ctx, "Hostile One +2", "", poster); pv.SystemID != teoID || pv.SystemFrom != "poster" {
		t.Errorf("a line naming none: %+v, want the poster's system", pv)
	}
	if n := count("intel_reports") + count("scans"); n != 0 {
		t.Fatalf("the preview stored %d rows", n)
	}

	now := time.Now().UTC()
	base := db.InsertIntelReportParams{ChannelID: cid, AuthorUserID: u.ID,
		AuthorCharacterID: poster, Reporter: "Burzrujat", CreatedAt: now, SeenAt: now}

	// A system override files a line that named none there.
	ov := submitOverride{SystemID: mozID}
	if res := app.postChatLinesWith(ctx, base, "Hostile One +2", "axon", "", ov); res.Posted != 1 {
		t.Fatalf("a line under a system override: %+v, want one posted", res)
	}
	rows, err := q.ListIntelReports(ctx, db.ListIntelReportsParams{ChannelID: cid, BeforeAt: intelFeedTop(), BeforeID: 1 << 62, MaxRows: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].SystemID != mozID {
		t.Fatalf("reports: %+v, want one on Mozzidit", rows)
	}

	// A scan kind the paste does not read as is skipped, never
	// read as chat; "chat" never stores a scan.
	if res := app.postChatLinesWith(ctx, base, "Teonusude Hostile Two", "axon", "", submitOverride{Kind: "dscan"}); res.Posted != 0 || res.Skipped != 1 {
		t.Errorf("chat under a d-scan override: %+v, want it skipped", res)
	}
	scans := count("scans")
	app.postChatLinesWith(ctx, base, "Hostile Three\nHostile Four\nHostile Five", "axon", "", submitOverride{Kind: submitKindChat})
	if count("scans") != scans {
		t.Errorf("a chat override stored a scan")
	}

	// The label: the player's word beside the detection.
	app.logSubmitLabel(ctx, cid, u.ID, "Hostile One +2", app.previewSubmission(ctx, "Hostile One +2", "", poster), ov, 1)
	var dk, ok string
	var ds, os int64
	if err := conn.QueryRowContext(ctx, `SELECT detected_kind, detected_system_id, override_kind, override_system_id FROM intel_submit_labels`).Scan(&dk, &ds, &ok, &os); err != nil {
		t.Fatal(err)
	}
	if dk != submitKindChat || ds != teoID || ok != "" || os != mozID {
		t.Errorf("label: detected %s/%d, override %q/%d", dk, ds, ok, os)
	}
	if validSubmitKind("bogus") || !validSubmitKind("probe") || !validSubmitKind("") {
		t.Error("validSubmitKind")
	}
}
