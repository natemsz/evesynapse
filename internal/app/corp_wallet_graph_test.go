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

// TestCorpWalletGraph: a division's journal plus its current balance
// make a line; the balance extends it to the snapshot's fetch time; one
// point or none draws nothing and says why.
func TestCorpWalletGraph(t *testing.T) {
	at := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	journal := esi.CorpJournal{
		{ID: 3, Date: "2026-10-03T12:00:00Z", Balance: 1500},
		{ID: 1, Date: "2026-10-01T12:00:00Z", Balance: 1000},
		{ID: 2, Date: "2026-10-02T12:00:00Z", Balance: 1250},
	}
	chart, note := corpWalletGraph(journal, 1600, true, at("2026-10-04T12:00:00Z"))
	if chart == nil || note != "" {
		t.Fatalf("chart %v, note %q; want a chart and no note", chart, note)
	}
	if len(chart.Dots) != 4 {
		t.Errorf("chart has %d points, want 3 journal entries plus the current balance", len(chart.Dots))
	}
	if chart.From != "2026-10-01" || chart.To != "2026-10-04" {
		t.Errorf("chart runs %s to %s, want 2026-10-01 to 2026-10-04", chart.From, chart.To)
	}

	// One journal entry and no balance: a single point is a fact, not a shape.
	chart, note = corpWalletGraph(journal[:1], 0, false, time.Time{})
	if chart != nil || !strings.Contains(note, "building") {
		t.Errorf("one point: chart %v, note %q; want no chart and a building note", chart, note)
	}
	// Nothing at all.
	chart, note = corpWalletGraph(nil, 0, false, time.Time{})
	if chart != nil || !strings.Contains(note, "No wallet activity") {
		t.Errorf("no points: chart %v, note %q; want no chart and a no-activity note", chart, note)
	}
}

// TestCorpWalletsPageShowsTheGraph: the corporation wallet page draws
// the selected division's balance graph above its journal, and a
// division whose ledger is refused explains why in the same place
// instead of leaving the graph out silently.
func TestCorpWalletsPageShowsTheGraph(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	seedCharacter(t, q, user.ID, fixtureCharB, "Fixture Other")
	now := time.Now().UTC()

	seedSnapshot(t, q, fixtureCharA, esi.SnapCorpInfo, `{"name":"Fixture Corp","ticker":"FXC","member_count":3}`)
	seedSnapshot(t, q, fixtureCharA, esi.SnapCorpWallets, esi.CorpWallets{{Division: 1, Balance: 2000}, {Division: 2, Balance: 50}})
	seedSnapshot(t, q, fixtureCharA, esi.CorpJournalKind(1), esi.CorpJournal{
		{ID: 1, Date: now.Add(-72 * time.Hour).Format(time.RFC3339), RefType: "player_trading", Amount: 500, Balance: 1500, Description: "a"},
		{ID: 2, Date: now.Add(-48 * time.Hour).Format(time.RFC3339), RefType: "player_trading", Amount: 250, Balance: 1750, Description: "b"},
		{ID: 3, Date: now.Add(-24 * time.Hour).Format(time.RFC3339), RefType: "player_trading", Amount: 250, Balance: 2000, Description: "c"},
	})
	seedSnapshot(t, q, fixtureCharA, esi.CorpJournalKind(2), esi.CorpJournal{
		{ID: 9, Date: now.Add(-24 * time.Hour).Format(time.RFC3339), RefType: "player_trading", Amount: 50, Balance: 50, Description: "only one"},
	})
	seedSnapshot(t, q, fixtureCharA, esi.CorpTxnsKind(1), esi.CorpWalletTransactions{})
	seedSnapshot(t, q, fixtureCharA, esi.CorpTxnsKind(2), esi.CorpWalletTransactions{})

	// Character B: wallet balances known, ledgers refused for a role.
	seedSnapshot(t, q, fixtureCharB, esi.SnapCorpInfo, `{"name":"Other Corp","ticker":"OTHR","member_count":2}`)
	seedSnapshot(t, q, fixtureCharB, esi.SnapCorpWallets, esi.CorpWallets{{Division: 1, Balance: 10}})
	if err := q.UpsertSnapshotFetchState(ctx, db.UpsertSnapshotFetchStateParams{
		CharacterID: fixtureCharB, Kind: esi.CorpJournalKind(1), State: fetchStateRoleMissing,
		Detail: "Accountant or Junior Accountant", AttemptedAt: now,
	}); err != nil {
		t.Fatalf("seed fetch state: %v", err)
	}
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")

	code, body := getPage(t, app, cookie, "/corporations/wallets/?character=90000001")
	if code != http.StatusOK {
		t.Fatalf("wallets page: status %d", code)
	}
	mustContain(t, "/corporations/wallets/", body,
		"Master Wallet — balance over time", `aria-label="Division balance over time"`, "<polyline",
		"ending at its current balance")
	if strings.Index(body, "balance over time") > strings.Index(body, "recent journal") {
		t.Error("the graph should sit above the journal")
	}

	// A division with a single point says it is building.
	_, body = getPage(t, app, cookie, "/corporations/wallets/?character=90000001&division=2")
	mustContain(t, "/corporations/wallets/ (division 2)", body, "Division 2 — balance over time", "Balance history is building")
	if strings.Contains(body, "<polyline") {
		t.Error("a one-point division drew a line")
	}

	// A refused ledger explains itself in the graph's place too.
	_, body = getPage(t, app, cookie, "/corporations/wallets/?character=90000002")
	mustContain(t, "/corporations/wallets/ (B)", body, "balance over time", "Wallet ledgers need the Accountant or Junior Accountant role")
	if strings.Contains(body, "<polyline") {
		t.Error("a refused ledger drew a line")
	}
}
