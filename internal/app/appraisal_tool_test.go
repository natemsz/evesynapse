package app

// The appraisal tool (/market/appraise/, appraise module):
// anonymous pastes price through the appraisal core with the
// tier and as-of on every line and coverage beside the total;
// a signed-in user's appraisal is saved as a snapshot that
// renders verbatim (including through its share link) and
// re-prices as a new snapshot beside the old one.

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
)

func seedAppraisalTool(t *testing.T, conn *sql.DB, q *db.Queries) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("seed %q: %v", query, err)
		}
	}
	exec(`INSERT INTO sde_types (type_id, name) VALUES
		(34, 'Tritanium'), (35, 'Pyerite'), (36, 'Mexallon'),
		(587, 'Rifter'), (7001, 'Rifter Blueprint'),
		(30001, 'Damage Control II'), (32001, 'Cap Booster 3200'),
		(40001, '1MN Afterburner II'), (23001, 'Antimatter Charge S')`)
	// Tritanium: a fresh typical with a buy side. Damage
	// Control II: a stale typical and an expired fallback, so
	// the floor rule speaks with its age. Rifter and its
	// blueprint and the afterburner: fresh typicals.
	exec(`INSERT INTO market_region_stats (region_id, type_id, typical_sell, typical_buy, updated_at) VALUES
		(10000002, 34, 6.5, 5.0, $1), (10000002, 587, 250000, 0, $1),
		(10000002, 7001, 1000000, 0, $1), (10000002, 40001, 40000, 0, $1)`, now.Add(-time.Hour))
	exec(`INSERT INTO market_region_stats (region_id, type_id, typical_sell, typical_buy, updated_at)
		VALUES (10000002, 30001, 150000, 0, $1)`, now.Add(-10*24*time.Hour))
	// Pyerite: a stored estimate only.
	exec(`INSERT INTO true_values (type_id, build_cost, est_sell, computed_at) VALUES (35, 0, 9.25, $1)`, now)
	// Cap Booster 3200: a live fallback median; Damage
	// Control II: the same source, long expired.
	fb := func(typeID int64, median float64, asof time.Time) {
		t.Helper()
		if err := q.InsertMarketStatsFallback(ctx, db.InsertMarketStatsFallbackParams{
			RegionID: 10000002, TypeID: typeID, MedianSell: median, FivepctSell: median,
			Source: "fuzzwork", SourceAsof: asof, FetchedAt: asof,
		}); err != nil {
			t.Fatalf("seed fallback %d: %v", typeID, err)
		}
	}
	fb(32001, 1200, now.Add(-time.Hour))
	fb(30001, 999999, now.Add(-72*time.Hour))
}

func postAppraise(t *testing.T, app *Application, cookie *http.Cookie, form url.Values) (int, string, http.Header) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/market/appraise/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Result().Body)
	return rec.Code, string(body), rec.Header()
}

func getWithCookie(t *testing.T, app *Application, cookie *http.Cookie, path string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Result().Body)
	return rec.Code, string(body)
}

func TestAppraisalToolAnonymousPaste(t *testing.T) {
	tr := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, tr)
	seedAppraisalTool(t, conn, q)

	code, body, _ := postAppraise(t, app, nil, url.Values{"items": {`Tritanium x 1,000
Pyerite 100
Mexallon x 5
Cap Booster 3200 x 2
Damage Control II
Tritanium 500
Unknown Item XYZ x 3`}})
	if code != 200 {
		t.Fatalf("anonymous appraisal: status %d, want 200", code)
	}
	// Tritanium aggregates (1,500 x 6.50), Pyerite prices off
	// its estimate (100 x 9.25), the booster off the fallback
	// (2 x 1,200), the damage control off its stale typical
	// under the floor rule (150,000): 163,075.00 in all, 4 of
	// the 6 displayed lines priced (the unknown name is the
	// sixth), and the typical buy total beside it.
	mustContain(t, "anonymous appraisal", body,
		"163,075.00", "Priced 4 of 6 lines", "7,500.00",
		"Our Forge typical", "Estimated sell", "Fuzzwork aggregate", "Not priced",
		"(10 days ago)", "Unknown Item XYZ", "not in the item database")
	if n := strings.Count(body, ">Tritanium</a>"); n != 1 {
		t.Errorf("Tritanium rows = %d, want 1 aggregated row", n)
	}
	// The total is a receipt-style block at the foot of the
	// result: after the lines table, never on the coverage line.
	totalAt := strings.Index(body, `<div class="ap-total">`)
	tableAt := strings.Index(body, "</table>")
	coverageAt := strings.Index(body, "Priced 4 of 6 lines")
	if totalAt < 0 || tableAt < 0 || totalAt < tableAt {
		t.Errorf("grand total block missing or not below the lines table (total=%d table=%d)", totalAt, tableAt)
	}
	if coverageAt < 0 || coverageAt > totalAt {
		t.Errorf("coverage line should precede the total block (coverage=%d total=%d)", coverageAt, totalAt)
	}
	mustContain(t, "anonymous appraisal total", body,
		`<span class="ap-total-label">Total value</span><span class="ap-total-fig">163,075.00 ISK</span>`)
	if strings.Contains(body, "<strong>Total value:") {
		t.Error("total still rendered on the header line")
	}
	// The form rides the shared box kit with its label above the
	// textarea.
	mustContain(t, "anonymous appraisal form", body,
		`<section class="box ap-form">`, `<label class="ap-label" for="ap-items">Items to appraise</label>`,
		`<section class="box ap-result">`)
	if tr.calls.Load() != 0 {
		t.Errorf("the appraisal page reached the network %d times; stored reads only", tr.calls.Load())
	}
}

func TestAppraisalToolBlueprintAndAbyssalLines(t *testing.T) {
	tr := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, tr)
	seedAppraisalTool(t, conn, q)

	code, body, _ := postAppraise(t, app, nil, url.Values{"items": {`Rifter Blueprint (BPC, ME 10, TE 20, 5 runs)
Rifter Blueprint
Abyssal Damage Control II`}})
	if code != 200 {
		t.Fatalf("blueprint/abyssal appraisal: status %d, want 200", code)
	}
	mustContain(t, "blueprint and abyssal lines", body,
		"BPC · ME 10 · TE 20 · 5 runs",
		"Blueprint — priced as an original",
		"Base item price — the mutated item itself prices from its rolls in Contract Search",
		"150,000.00")
}

func TestAppraisalToolEFTPaste(t *testing.T) {
	tr := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, tr)
	seedAppraisalTool(t, conn, q)

	code, body, _ := postAppraise(t, app, nil, url.Values{"items": {`[Rifter, Test Fit]
Damage Control II
1MN Afterburner II

Antimatter Charge S x 100
Nonexistent Module X`}})
	if code != 200 {
		t.Fatalf("eft appraisal: status %d, want 200", code)
	}
	// Ship 250,000 + damage control 150,000 + afterburner
	// 40,000; the charges have no price anywhere, and the
	// unknown module is reported, never dropped.
	mustContain(t, "eft appraisal", body,
		"440,000.00", "Priced 3 of 5 lines", ">Rifter</a>", "Nonexistent Module X")
}

func TestAppraisalToolSavedShareAndReprice(t *testing.T) {
	tr := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, tr)
	seedAppraisalTool(t, conn, q)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture")

	code, _, header := postAppraise(t, app, cookie, url.Values{"items": {"Tritanium x 10"}})
	if code != http.StatusSeeOther {
		t.Fatalf("signed-in appraisal: status %d, want 303", code)
	}
	loc := header.Get("Location")
	if !strings.HasPrefix(loc, "/market/appraise/?a=") {
		t.Fatalf("redirect = %q, want the saved appraisal", loc)
	}
	code, body := getWithCookie(t, app, cookie, loc)
	if code != 200 {
		t.Fatalf("saved appraisal: status %d, want 200", code)
	}
	mustContain(t, "saved appraisal", body, "65.00", "Your appraisals")

	rows, err := q.ListUserAppraisals(ctx, db.ListUserAppraisalsParams{UserID: user.ID, Limit: 20})
	if err != nil || len(rows) != 1 {
		t.Fatalf("saved rows = %v, %v; want exactly one", rows, err)
	}
	share := "/market/appraise/s/" + rows[0].ShareToken + "/"

	// The share link renders the stored snapshot to anyone,
	// verbatim.
	code, body = getAnon(t, app, share)
	if code != 200 {
		t.Fatalf("shared appraisal: status %d, want 200", code)
	}
	mustContain(t, "shared appraisal", body, "Shared appraisal", "65.00",
		`<span class="ap-total-label">Total value</span><span class="ap-total-fig">65.00 ISK</span>`)

	// Another signed-in user cannot open it by id.
	other, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create other user: %v", err)
	}
	otherCookie := sessionCookie(t, app, other.ID, fixtureCharB, "Other")
	_, body = getWithCookie(t, app, otherCookie, loc)
	mustContain(t, "foreign appraisal id", body, "That appraisal could not be found.")
	if strings.Contains(body, "65.00") {
		t.Error("another user's appraisal total leaked by id")
	}

	// Prices move; the stored snapshot does not. Re-pricing
	// writes a new appraisal beside it at the new price.
	if _, err := conn.ExecContext(ctx, `UPDATE market_region_stats SET typical_sell = 10 WHERE type_id = 34`); err != nil {
		t.Fatalf("move price: %v", err)
	}
	_, body = getAnon(t, app, share)
	mustContain(t, "shared appraisal after price move", body, "65.00")

	repriceURL := "/market/appraise/" + strings.TrimPrefix(loc, "/market/appraise/?a=") + "/reprice"
	req := httptest.NewRequest(http.MethodPost, repriceURL, nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("reprice: status %d, want 303", rec.Code)
	}
	newLoc := rec.Header().Get("Location")
	if newLoc == loc || !strings.HasPrefix(newLoc, "/market/appraise/?a=") {
		t.Fatalf("reprice redirect = %q, want a new appraisal", newLoc)
	}
	_, body = getWithCookie(t, app, cookie, newLoc)
	mustContain(t, "repriced appraisal", body, "100.00")
	_, body = getWithCookie(t, app, cookie, loc)
	mustContain(t, "original appraisal after reprice", body, "65.00")
	rows, err = q.ListUserAppraisals(ctx, db.ListUserAppraisalsParams{UserID: user.ID, Limit: 20})
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows after reprice = %d, %v; want the old and the new", len(rows), err)
	}
}

func TestAppraisalToolPageAndBadShare(t *testing.T) {
	tr := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, tr)
	seedAppraisalTool(t, conn, q)

	code, body := getAnon(t, app, "/market/appraise/")
	if code != 200 {
		t.Fatalf("appraisal page: status %d, want 200", code)
	}
	mustContain(t, "appraisal page", body, "Appraisal", "Items to appraise", "Contract Search")
	// Appraisal lives under Tools now: its tab strip is the
	// Tools strip (Item Browser + Appraisal, itself active), and
	// the Market strip no longer carries it.
	tabs := sectionTabsBlock(t, body)
	mustContain(t, "appraisal tools tabs", tabs,
		`<a href="/market/appraise/" class="active" aria-current="page">Appraisal</a>`,
		`<a href="/items/">Item Browser</a>`)
	if strings.Contains(tabs, "Contract Search") {
		t.Error("appraisal page still carries the Market tab strip")
	}

	code, body = getAnon(t, app, "/market/appraise/s/deadbeef/")
	if code != 404 {
		t.Fatalf("bad share token: status %d, want 404", code)
	}
	mustContain(t, "bad share token", body, "That shared appraisal could not be found.")
}
