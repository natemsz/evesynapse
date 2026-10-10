package app

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
)

func TestAgeString(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	for ago, want := range map[time.Duration]string{
		40 * time.Second:   "40s ago",
		2 * time.Minute:    "2m ago",
		3 * time.Hour:      "3h ago",
		2 * 24 * time.Hour: "2d ago",
	} {
		if got := ageString(now, now.Add(-ago)); got != want {
			t.Errorf("ageString(%v) = %q, want %q", ago, got, want)
		}
	}
	if got := ageString(now, now.Add(time.Minute)); got != "0s ago" {
		t.Errorf("ageString(future) = %q, want 0s ago", got)
	}
}

func TestFetchReason(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	warmedTarget := func(entity int64, event, view string, warmed time.Time) {
		t.Helper()
		if _, err := q.AddForesightTarget(ctx, db.AddForesightTargetParams{
			UserID: user.ID, CharacterID: 90000100, EventKind: event, EventKey: view,
			TargetKind: targetCorporation, SubjectID: entity,
			ViewKey: view, CreatedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(time.Hour),
		}); err != nil {
			t.Fatalf("seed target: %v", err)
		}
		pending, err := q.ListForesightPending(ctx, db.ListForesightPendingParams{Now: now, RowLimit: 200})
		if err != nil {
			t.Fatalf("read pending: %v", err)
		}
		var id int64
		for _, tg := range pending {
			if tg.ViewKey == view {
				id = tg.ID
			}
		}
		if err := q.MarkForesightWarmed(ctx, db.MarkForesightWarmedParams{
			ID: id, WarmedAt: timeSet(warmed), Calls: 2,
		}); err != nil {
			t.Fatalf("mark warmed: %v", err)
		}
	}
	guess := func(entity int64, noted time.Time) {
		t.Helper()
		if err := q.InsertTypingGuess(ctx, db.InsertTypingGuessParams{
			Box: "7|topbar", Kind: string(pageWantCorporation), EntityID: entity,
			NotedAt: noted,
		}); err != nil {
			t.Fatalf("seed guess: %v", err)
		}
	}
	fetched := now

	// Warmed before the fetch: the event is the reason.
	warmedTarget(98000001, eventWar, "corporation:98000001", now.Add(-time.Hour))
	// Guessed before the fetch: the search is the reason.
	guess(98000002, now.Add(-5*time.Minute))
	// Both: the warming wins.
	warmedTarget(98000006, eventDock, "corporation:98000006", now.Add(-time.Hour))
	guess(98000006, now.Add(-5*time.Minute))
	// Warmed after the fetch: too late to have caused it.
	warmedTarget(98000003, eventWar, "corporation:98000003", now.Add(time.Hour))
	// Guessed after the fetch: likewise.
	guess(98000004, now.Add(time.Hour))

	for _, tc := range []struct {
		entity int64
		view   string
		want   string
	}{
		{98000001, "corporation:98000001", "a war was declared"},
		{98000002, "corporation:98000002", "your search"},
		{98000006, "corporation:98000006", "you docked"},
		{98000003, "corporation:98000003", ""},
		{98000004, "corporation:98000004", ""},
		{98000005, "corporation:98000005", ""},
	} {
		if got := app.fetchReason(ctx, tc.view, string(pageWantCorporation), tc.entity, 0, fetched); got != tc.want {
			t.Errorf("reason for %d = %q, want %q", tc.entity, got, tc.want)
		}
	}
}

func TestCorporationPageShowsFreshness(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, 90000100, "Pilot 0")
	cookie := sessionCookie(t, app, user.ID, 90000100, "Pilot 0")
	now := time.Now().UTC().Truncate(time.Second)

	if err := q.SetCorporationRecord(ctx, db.SetCorporationRecordParams{
		CorporationID: 98000001,
		Payload:       `{"corp":{"name":"Fresh Corp","ticker":"FR","member_count":5,"tax_rate":0.1},"alliance":{}}`,
		State:         orgStateReady, FetchedAt: timeSet(now.Add(-40 * time.Second)),
	}); err != nil {
		t.Fatalf("seed corporation record: %v", err)
	}
	if _, err := q.AddForesightTarget(ctx, db.AddForesightTargetParams{
		UserID: user.ID, CharacterID: 90000100, EventKind: eventWar, EventKey: "corporation:98000001",
		TargetKind: targetCorporation, SubjectID: 98000001,
		ViewKey: "corporation:98000001", CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	pending, err := q.ListForesightPending(ctx, db.ListForesightPendingParams{Now: now, RowLimit: 200})
	if err != nil || len(pending) != 1 {
		t.Fatalf("read pending: %v (%d rows)", err, len(pending))
	}
	if err := q.MarkForesightWarmed(ctx, db.MarkForesightWarmedParams{
		ID: pending[0].ID, WarmedAt: timeSet(now.Add(-50 * time.Second)), Calls: 1,
	}); err != nil {
		t.Fatalf("mark warmed: %v", err)
	}

	code, body := getPage(t, app, cookie, "/corporation/?corporation=98000001")
	if code != http.StatusOK {
		t.Fatalf("/corporation/: status %d", code)
	}
	// "Updated Ns ago" ticks with the clock; assert the shape and
	// the reason separately.
	mustContain(t, "/corporation/", body, "Updated ", "because a war was declared")
}

func TestCorporationPageHidesFreshnessWhileLoading(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, 90000100, "Pilot 0")
	cookie := sessionCookie(t, app, user.ID, 90000100, "Pilot 0")

	code, body := getPage(t, app, cookie, "/corporation/?corporation=98000001")
	if code != http.StatusOK {
		t.Fatalf("/corporation/: status %d", code)
	}
	if strings.Contains(body, "Updated ") {
		t.Error("loading page shows a freshness line it has no record for")
	}
}
