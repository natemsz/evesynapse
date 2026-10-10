package app

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// TestCorporationsOverviewRendersStoredDataWithoutFetching: the
// overview renders from stored data and never fetches at render
// (cluster rule, see TestCorpPagesRenderFromSnapshots: pages render
// cache-only). It replaces the stale concurrency test, which
// asserted render-time ESI lookups — a design deliberately removed
// when the overview moved to stored records filled by the worker.
// Cold, the block shows the pending placeholder and leaves a
// corporation want behind; once the record lands, the stored name
// renders. The transport stays silent throughout.
func TestCorporationsOverviewRendersStoredDataWithoutFetching(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	const charID = int64(90000100)
	const corpID = int64(98000100)
	seedCharacter(t, q, user.ID, charID, "Pilot 0")
	seedSnapshot(t, q, charID, esi.SnapProfile, `{"name":"Pilot 0","corporation_id":98000100}`)
	cookie := sessionCookie(t, app, user.ID, charID, "Pilot 0")

	// Cold: nothing stored. The placeholder shows, the worker gets
	// its want, and nothing goes out.
	code, body := getPage(t, app, cookie, "/corporations/")
	if code != http.StatusOK {
		t.Fatalf("/corporations/: status %d", code)
	}
	mustContain(t, "/corporations/", body, "Corporation #98000100", "still syncing")
	if strings.Contains(body, "Corp 98000100") {
		t.Error("/corporations/: cold page shows a name it has not stored")
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("cold render made %d outbound calls, want 0", got)
	}
	drains, err := q.ListCorporationDrains(ctx, db.ListCorporationDrainsParams{
		TypingPriority: wantTyping,
		StaleCutoff:    time.Now().UTC(),
		TypingLimit:    12,
		DrainLimit:     100,
	})
	if err != nil {
		t.Fatalf("list corporation drains: %v", err)
	}
	queued := false
	for _, id := range drains {
		if id == corpID {
			queued = true
		}
	}
	if !queued {
		t.Errorf("cold render left no corporation want for %d (drains: %v)", corpID, drains)
	}

	// Warm: the stored record renders, still with no outbound calls.
	if err := q.SetCorporationRecord(ctx, db.SetCorporationRecordParams{
		CorporationID: corpID,
		Payload:       `{"corp":{"name":"Stored Corp","ticker":"STC","member_count":5,"tax_rate":0.1},"alliance":{}}`,
		State:         orgStateReady,
		FetchedAt:     mustNullTime("2026-01-01T00:00:00Z"),
	}); err != nil {
		t.Fatalf("seed corporation record: %v", err)
	}
	code, body = getPage(t, app, cookie, "/corporations/")
	if code != http.StatusOK {
		t.Fatalf("/corporations/ after record: status %d", code)
	}
	mustContain(t, "/corporations/ after record", body, "Stored Corp")
	if strings.Contains(body, "still syncing") {
		t.Error("/corporations/: stored record still renders as pending")
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("warm render made %d outbound calls, want 0", got)
	}
}
