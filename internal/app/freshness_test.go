package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// TestStatusFreshnessEndpoint: the sidebar status modal's feed.
// A signed-in reader gets ESI's state and one framed row per
// dataset (an age plus the cadence it is read against, never a
// bare age); a signed-out reader is sent away.
func TestStatusFreshnessEndpoint(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ch := seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")

	now := time.Now().UTC().Truncate(time.Second)
	if err := q.UpsertMarketFetchState(ctx, db.UpsertMarketFetchStateParams{
		Kind: regionSweepKind(10000002), State: "ok",
		AttemptedAt: now.Add(-12 * time.Minute),
	}); err != nil {
		t.Fatalf("seed fetch state: %v", err)
	}
	if err := q.UpsertGlobalSnapshot(ctx, db.UpsertGlobalSnapshotParams{
		Kind:      esi.GlobalStatus,
		Payload:   `{"players": 24312}`,
		FetchedAt: now,
	}); err != nil {
		t.Fatalf("seed status snapshot: %v", err)
	}

	get := func(cookie *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/status/freshness", nil)
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec
	}

	rec := get(sessionCookie(t, app, user.ID, ch.CharacterID, "Fixture Ceo"))
	if rec.Code != http.StatusOK {
		t.Fatalf("signed in: %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var body struct {
		ESI struct {
			State   string `json:"state"`
			Players string `json:"players"`
		} `json:"esi"`
		Rows []freshRow `json:"rows"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.ESI.State != "ok" || body.ESI.Players != "24,312" {
		t.Errorf("esi = %+v, want state ok and 24,312 players", body.ESI)
	}
	var forge *freshRow
	for i := range body.Rows {
		if body.Rows[i].Name == "Market orders · The Forge" {
			forge = &body.Rows[i]
		}
		if !strings.Contains(body.Rows[i].Text, "·") {
			t.Errorf("row %q text %q is a bare age, not framed against its cadence", body.Rows[i].Name, body.Rows[i].Text)
		}
	}
	if forge == nil {
		t.Fatalf("no Market orders · The Forge row in %+v", body.Rows)
	}
	if forge.State != "warn" || !strings.Contains(forge.Text, "ago · sweeps hourly") {
		t.Errorf("forge row = %+v, want the seeded age framed against the hourly sweep", forge)
	}

	anon := get(nil)
	if anon.Code != http.StatusSeeOther {
		t.Errorf("signed out: %d, want a redirect", anon.Code)
	}
}
