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
	"strconv"
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

func headerCookies(header http.Header) []*http.Cookie {
	return (&http.Response{Header: header}).Cookies()
}

func getWithCookies(t *testing.T, app *Application, cookies []*http.Cookie, path string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Result().Body)
	return rec.Code, string(body)
}

// postAnonAppraisal submits an anonymous paste and follows
// its redirect to the stored share page, carrying the
// history cookie the submit set.
func postAnonAppraisal(t *testing.T, app *Application, form url.Values) (string, []*http.Cookie, string) {
	t.Helper()
	code, _, header := postAppraise(t, app, nil, form)
	if code != http.StatusSeeOther {
		t.Fatalf("anonymous appraisal: status %d, want 303 to the stored result", code)
	}
	loc := header.Get("Location")
	if !strings.HasPrefix(loc, "/market/appraise/s/") {
		t.Fatalf("anonymous appraisal redirect = %q, want the share page", loc)
	}
	cookies := headerCookies(header)
	code, body := getWithCookies(t, app, cookies, loc)
	if code != http.StatusOK {
		t.Fatalf("anonymous appraisal result: status %d, want 200", code)
	}
	return body, cookies, loc
}

func TestAppraisalToolAnonymousPaste(t *testing.T) {
	tr := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, tr)
	seedAppraisalTool(t, conn, q)

	body, _, _ := postAnonAppraisal(t, app, url.Values{"items": {`Tritanium x 1,000
Pyerite 100
Mexallon x 5
Cap Booster 3200 x 2
Damage Control II
Tritanium 500
Unknown Item XYZ x 3`}})
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

	body, _, _ := postAnonAppraisal(t, app, url.Values{"items": {`Rifter Blueprint (BPC, ME 10, TE 20, 5 runs)
Rifter Blueprint
Abyssal Damage Control II`}})
	mustContain(t, "blueprint and abyssal lines", body,
		"BPC · ME 10 · TE 20 · 5 runs",
		"Blueprint — priced as an original",
		"Base item price — this mutated item was pasted without its link, so its own rolls were not priced",
		"150,000.00")
	if strings.Contains(body, "prices from its rolls in Contract Search") {
		t.Error("mutated line still implies it cannot be appraised here")
	}
}

// seedAppraisalAbyssal adds the mutated type and two corpus
// rows for it: item 900001 with a stored estimate (the same
// figure the contract pages show for it), and item 900003
// recorded with its rolls but no estimate yet.
func seedAppraisalAbyssal(t *testing.T, conn *sql.DB) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("seed %q: %v", query, err)
		}
	}
	exec(`INSERT INTO sde_types (type_id, name) VALUES (60001, 'Abyssal Damage Control II')`)
	exec(`INSERT INTO abyssal_items (item_id, type_id, source_type_id, mutator_type_id, created_by,
		attributes, fetched_at, first_seen_at, last_seen_at,
		family, score, percentiles, derived_stats, est_value, est_low, est_high,
		est_basis, est_scope, est_comps, premium_mult, scored_at)
		VALUES (900001, 60001, 30001, 47735, 9001,
		'[]', $1, $1, $1, 'test_family', 0.9, '{}', '[]',
		205000000, 180000000, 240000000, 'corpus', 'module', 8, 1, $1)`, now)
	exec(`INSERT INTO abyssal_items (item_id, type_id, source_type_id, mutator_type_id, created_by,
		attributes, fetched_at, first_seen_at, last_seen_at,
		family, score, percentiles, derived_stats, est_basis, est_scope, est_comps, premium_mult)
		VALUES (900003, 60001, 30001, 47735, 9001,
		'[]', $1, $1, $1, 'test_family', 0.5, '{}', '[]', '', '', 0, 1)`, now)
}

func TestAppraisalToolAbyssalItemLink(t *testing.T) {
	tr := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, tr)
	seedAppraisalTool(t, conn, q)
	seedAppraisalAbyssal(t, conn)

	body, _, _ := postAnonAppraisal(t, app, url.Values{"items": {`<url=showinfo:60001//900001>Abyssal Damage Control II</url>
Tritanium x 100
Rifter Blueprint (BPC, ME 10, TE 20, 5 runs)`}})
	// The linked item prices from its stored abyssal
	// estimate (205,000,000), the tritanium at its typical
	// (100 x 6.50): 205,000,650.00 in all, and the line
	// carries the estimate's range and basis in the contract
	// pages' words. The blueprint copy prices NOWHERE: no
	// copy medians are stored in this fixture, and a copy
	// never takes its original's price (the old behavior
	// priced this line at the original's 1,000,000 typical).
	mustContain(t, "abyssal item link", body,
		"205,000,650.00", "Priced 2 of 3 lines",
		"Our abyssal estimate", "205,000,000.00",
		"likely 180,000,000.00 to 240,000,000.00", "from 8 listings of this module",
		"BPC · ME 10 · TE 20 · 5 runs")
	if n := strings.Count(body, ">Abyssal Damage Control II</a>"); n != 1 {
		t.Errorf("abyssal rows = %d, want 1", n)
	}
	if tr.calls.Load() != 0 {
		t.Errorf("the appraisal page reached the network %d times; stored reads only", tr.calls.Load())
	}
}

func TestAppraisalToolAbyssalItemLinkUnknown(t *testing.T) {
	tr := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, tr)
	seedAppraisalTool(t, conn, q)
	seedAppraisalAbyssal(t, conn)

	// Item 900002 is not in the corpus: the line prices its
	// base item (the damage control's stale typical under
	// the floor rule) and says its rolls are not on record.
	body, _, _ := postAnonAppraisal(t, app, url.Values{"items": {`<url=showinfo:60001//900002>Abyssal Damage Control II</url>
Tritanium x 100`}})
	mustContain(t, "unknown abyssal item link", body,
		"150,650.00", "150,000.00",
		"Base item price", "rolls are not in our records")
	if strings.Contains(body, "Our abyssal estimate") {
		t.Error("an unrecorded item was priced from an abyssal estimate")
	}
}

func TestAppraisalToolAbyssalItemLinkNoEstimate(t *testing.T) {
	tr := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, tr)
	seedAppraisalTool(t, conn, q)
	seedAppraisalAbyssal(t, conn)

	// Item 900003 is on record with its rolls but no
	// estimate: the line prices its known base type and
	// says so.
	body, _, _ := postAnonAppraisal(t, app, url.Values{"items": {`<url=showinfo:60001//900003>Abyssal Damage Control II</url>`}})
	mustContain(t, "unestimated abyssal item link", body,
		"150,000.00", ">Damage Control II</a>",
		"Base item price", "we hold this item", "no estimate for it yet")
}

func TestAppraisalToolEFTPaste(t *testing.T) {
	tr := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, tr)
	seedAppraisalTool(t, conn, q)

	body, _, _ := postAnonAppraisal(t, app, url.Values{"items": {`[Rifter, Test Fit]
Damage Control II
1MN Afterburner II

Antimatter Charge S x 100
Nonexistent Module X`}})
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
	mustContain(t, "appraisal page", body, "Appraisal", "Items to appraise",
		"A pasted EFT fit appraises the ship, its fitted modules and charges, and the drones and cargo the fit lists",
		"Mutated (abyssal) items appraise too")
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

func appraisalRowCount(t *testing.T, conn *sql.DB) int64 {
	t.Helper()
	var n int64
	if err := conn.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM user_appraisals`).Scan(&n); err != nil {
		t.Fatalf("count appraisals: %v", err)
	}
	return n
}

func postFormCookies(t *testing.T, app *Application, cookies []*http.Cookie, path string) (int, http.Header) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	return rec.Code, rec.Header()
}

func TestAppraisalToolRejectsZeroLinePaste(t *testing.T) {
	tr := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, tr)
	seedAppraisalTool(t, conn, q)
	ctx := context.Background()

	for _, paste := range []string{
		"just some prose about my day",
		"https://example.com/some/link",
		"# only a comment\n// another comment",
	} {
		code, body, _ := postAppraise(t, app, nil, url.Values{"items": {paste}})
		if code != http.StatusOK {
			t.Fatalf("zero-line paste %q: status %d, want 200 with an error", paste, code)
		}
		mustContain(t, "zero-line paste", body, "could be read as an item list", "no appraisal was saved")
	}
	if n := appraisalRowCount(t, conn); n != 0 {
		t.Fatalf("zero-line pastes stored %d rows, want none", n)
	}

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture")
	code, body, _ := postAppraise(t, app, cookie, url.Values{"items": {"prose, no items here"}})
	if code != http.StatusOK {
		t.Fatalf("signed zero-line paste: status %d, want 200 with an error", code)
	}
	mustContain(t, "signed zero-line paste", body, "no appraisal was saved")
	if n := appraisalRowCount(t, conn); n != 0 {
		t.Fatalf("signed zero-line paste stored %d rows, want none", n)
	}

	// One resolved line is enough: the rest are reported.
	body, _, _ = postAnonAppraisal(t, app, url.Values{"items": {"Tritanium x 10\nprose, no items here"}})
	mustContain(t, "one good line", body, "65.00")
	if n := appraisalRowCount(t, conn); n != 1 {
		t.Fatalf("one-good-line paste stored %d rows, want 1", n)
	}
}

func TestAppraisalToolAnonymousStoredHistoryAndDelete(t *testing.T) {
	tr := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, tr)
	seedAppraisalTool(t, conn, q)

	code, _, header := postAppraise(t, app, nil, url.Values{"items": {"Tritanium x 10"}})
	if code != http.StatusSeeOther {
		t.Fatalf("anonymous appraisal: status %d, want 303", code)
	}
	share := header.Get("Location")
	cookies := headerCookies(header)
	var history *http.Cookie
	for _, c := range cookies {
		if c.Name == "evesynapse-appraisals" {
			history = c
		}
	}
	if history == nil || !history.HttpOnly {
		t.Fatalf("history cookie = %v, want an HttpOnly evesynapse-appraisals cookie", history)
	}

	// The creator sees the result with its delete control;
	// a stranger with the link does not.
	code, body := getWithCookies(t, app, cookies, share)
	if code != http.StatusOK {
		t.Fatalf("creator result: status %d, want 200", code)
	}
	mustContain(t, "creator result", body, "65.00", "Delete this appraisal")
	_, body = getAnon(t, app, share)
	if strings.Contains(body, "Delete this appraisal") {
		t.Error("a stranger's share view offered the delete control")
	}

	// The appraise page lists it from the cookie, with the
	// expiry policy in one sentence.
	_, body = getWithCookies(t, app, cookies, "/market/appraise/")
	mustContain(t, "anonymous history", body, "Your recent appraisals", "65.00",
		"Appraisals you make while signed out are kept for 180 days after they were last viewed, then deleted.")

	// A forged cookie (the real token, no valid signature)
	// shows no history and cannot delete.
	token := strings.TrimSuffix(strings.TrimPrefix(share, "/market/appraise/s/"), "/")
	forged := []*http.Cookie{{Name: "evesynapse-appraisals", Value: token}}
	_, body = getWithCookies(t, app, forged, "/market/appraise/")
	if strings.Contains(body, "Your recent appraisals") {
		t.Error("a forged history cookie listed appraisals")
	}
	if code, _ := postFormCookies(t, app, forged, share+"delete"); code != http.StatusSeeOther {
		t.Fatalf("forged delete: status %d, want 303", code)
	}
	if code, _ := getAnon(t, app, share); code != http.StatusOK {
		t.Fatalf("share after forged delete: status %d, want 200 (still there)", code)
	}

	// The creator deletes it; the link 404s and the history
	// forgets it.
	if code, _ := postFormCookies(t, app, cookies, share+"delete"); code != http.StatusSeeOther {
		t.Fatalf("creator delete: status %d, want 303", code)
	}
	if code, _ := getAnon(t, app, share); code != http.StatusNotFound {
		t.Fatalf("deleted share: status %d, want 404", code)
	}
	if n := appraisalRowCount(t, conn); n != 0 {
		t.Fatalf("rows after delete = %d, want 0", n)
	}
}

func TestAppraisalToolSignedDeleteAndExpirySweep(t *testing.T) {
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
		t.Fatalf("signed appraisal: status %d, want 303", code)
	}
	loc := header.Get("Location")
	rows, err := q.ListUserAppraisals(ctx, db.ListUserAppraisalsParams{UserID: user.ID, Limit: 20})
	if err != nil || len(rows) != 1 {
		t.Fatalf("saved rows = %v, %v; want one", rows, err)
	}
	share := "/market/appraise/s/" + rows[0].ShareToken + "/"
	del := "/market/appraise/" + strconv.FormatInt(rows[0].ID, 10) + "/delete"

	// Another user cannot delete it.
	other, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create other user: %v", err)
	}
	otherCookie := sessionCookie(t, app, other.ID, fixtureCharB, "Other")
	postFormCookies(t, app, []*http.Cookie{otherCookie}, del)
	if code, _ := getAnon(t, app, share); code != http.StatusOK {
		t.Fatalf("share after foreign delete: status %d, want 200", code)
	}

	// The owner's result view carries the delete control.
	_, body := getWithCookie(t, app, cookie, loc)
	mustContain(t, "owner result", body, "Delete this appraisal")

	// The owner deletes it; the link 404s.
	postFormCookies(t, app, []*http.Cookie{cookie}, del)
	if code, _ := getAnon(t, app, share); code != http.StatusNotFound {
		t.Fatalf("deleted signed share: status %d, want 404", code)
	}

	// The expiry sweep removes only stale anonymous rows.
	ownerID, err := func() (int64, error) {
		u, err := q.CreateUser(ctx)
		return u.ID, err
	}()
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	old := time.Now().UTC().Add(-200 * 24 * time.Hour)
	recent := time.Now().UTC().Add(-10 * 24 * time.Hour)
	insert := func(anon bool, viewed time.Time, token string) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, `INSERT INTO user_appraisals
			(user_id, input_text, input_hash, share_token, snapshot, total_value, priced_lines, total_lines, is_anonymous, last_viewed_at)
			VALUES ($1, 'x', 'h', $2, '{}', 1, 1, 1, $3, $4)`, ownerID, token, anon, viewed); err != nil {
			t.Fatalf("insert appraisal: %v", err)
		}
	}
	insert(true, old, strings.Repeat("a", 48))
	insert(true, recent, strings.Repeat("b", 48))
	insert(false, old, strings.Repeat("c", 48))
	n, err := q.DeleteExpiredAnonymousAppraisals(ctx, time.Now().UTC().Add(-180*24*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("sweep deleted %d rows (%v), want exactly the stale anonymous one", n, err)
	}
	if got := appraisalRowCount(t, conn); got != 2 {
		t.Fatalf("rows after sweep = %d, want the recent anonymous and the signed one", got)
	}
}

func TestAppraisalToolEscapesPastedPayloads(t *testing.T) {
	tr := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, tr)
	seedAppraisalTool(t, conn, q)

	body, _, _ := postAnonAppraisal(t, app, url.Values{"items": {
		"Tritanium x 5\n<script>alert(1)</script> x 2\n<img src=x onerror=alert(2)>",
	}})
	for _, raw := range []string{"<script>alert(1)</script>", "<img src=x onerror=alert(2)>"} {
		if strings.Contains(body, raw) {
			t.Errorf("appraisal page rendered pasted payload %q unescaped", raw)
		}
	}
	mustContain(t, "escaped paste", body, "&lt;script&gt;alert(1)&lt;/script&gt;", "32.50")
}

func postAppraiseCookies(t *testing.T, app *Application, cookies []*http.Cookie, form url.Values) (int, http.Header) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/market/appraise/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	return rec.Code, rec.Header()
}

func TestAppraisalToolPricingBasis(t *testing.T) {
	tr := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, tr)
	seedAppraisalTool(t, conn, q)
	ctx := context.Background()
	now := time.Now().UTC()
	// Domain: Tritanium typical sell 10, buy 8, fresh.
	if _, err := conn.ExecContext(ctx, `INSERT INTO market_region_stats (region_id, type_id, typical_sell, typical_buy, updated_at)
		VALUES (10000043, 34, 10, 8, $1)`, now.Add(-time.Hour)); err != nil {
		t.Fatalf("seed domain stats: %v", err)
	}

	// Split at Domain is the 9.00 midpoint; at 90% the unit
	// is 8.10, so 100 Tritanium appraise to 810.00 under the
	// Domain split label, with the basis stated on the page.
	body, cookies, share := postAnonAppraisal(t, app, url.Values{
		"items": {"Tritanium x 100"}, "hub": {"10000043"}, "mode": {"split"}, "pct": {"90"},
	})
	mustContain(t, "domain split appraisal", body,
		"810.00", "Our Domain typical (split)", "Priced from Domain typical split prices at 90%.")

	// The remembered basis (basis cookie) drives a second
	// appraisal that names no basis at all.
	code, header := postAppraiseCookies(t, app, cookies, url.Values{"items": {"Tritanium x 100"}})
	if code != http.StatusSeeOther {
		t.Fatalf("remembered-basis appraisal: status %d, want 303", code)
	}
	_, body = getWithCookies(t, app, headerCookies(header), header.Get("Location"))
	mustContain(t, "remembered basis", body, "810.00", "Our Domain typical (split)")

	// A hub with no stored typical for the type does not
	// borrow The Forge's: Heimatar has no Tritanium row, no
	// estimate and no fallback, so the line is unpriced there.
	body, _, _ = postAnonAppraisal(t, app, url.Values{
		"items": {"Tritanium x 100"}, "hub": {"10000030"}, "mode": {"sell"}, "pct": {"100"},
	})
	mustContain(t, "unpriced at hub", body, "Not priced", "Priced 0 of 1 lines")
	if strings.Contains(body, "650.00") {
		t.Error("a Heimatar appraisal silently used The Forge's price")
	}

	// URL parameters pre-select the form for a linked preset.
	code, body = getAnon(t, app, "/market/appraise/?hub=Domain&mode=buy&pct=110")
	if code != http.StatusOK {
		t.Fatalf("preset page: status %d, want 200", code)
	}
	mustContain(t, "preset form", body,
		`<option value="10000043" selected>Domain</option>`,
		`<option value="buy" selected>Typical buy</option>`, `value="110"`)

	// Repricing the Domain appraisal at The Forge sell 100%
	// makes a new appraisal beside the original, which keeps
	// its own basis and total.
	req := httptest.NewRequest(http.MethodPost, share+"reprice", strings.NewReader(url.Values{
		"hub": {"10000002"}, "mode": {"sell"}, "pct": {"100"},
	}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("reprice: status %d, want 303", rec.Code)
	}
	newShare := rec.Header().Get("Location")
	if newShare == share || !strings.HasPrefix(newShare, "/market/appraise/s/") {
		t.Fatalf("reprice redirect = %q, want a new share page", newShare)
	}
	_, body = getAnon(t, app, newShare)
	mustContain(t, "repriced at forge", body, "650.00", "Our Forge typical (sell)")
	_, body = getAnon(t, app, share)
	mustContain(t, "original after reprice", body, "810.00", "Our Domain typical (split)")
	if n := appraisalRowCount(t, conn); n < 3 {
		t.Fatalf("rows = %d, want the originals and the reprice stored separately", n)
	}
}

func TestAppraisalToolReprocessAndVolume(t *testing.T) {
	tr := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, tr)
	seedAppraisalTool(t, conn, q)
	ctx := context.Background()
	now := time.Now().UTC()
	// A fake ore: type 12345 "Test Ore", portion 100, volume
	// 0.1 m3, reprocessing into 700 Tritanium + 300 Pyerite
	// per portion. Direct price 10 sell at The Forge.
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("seed %q: %v", query, err)
		}
	}
	exec(`INSERT INTO sde_types (type_id, name) VALUES (12345, 'Test Ore')`)
	exec(`INSERT INTO sde_type_physics (type_id, mass, volume, capacity, portion_size) VALUES (12345, 0, 0.1, 0, 100), (34, 0, 0.01, 0, 0)`)
	exec(`INSERT INTO sde_reprocessing (type_id, material_type_id, quantity) VALUES (12345, 34, 700), (12345, 35, 300)`)
	exec(`INSERT INTO market_region_stats (region_id, type_id, typical_sell, typical_buy, updated_at) VALUES (10000002, 12345, 10, 0, $1)`, now.Add(-time.Hour))

	// 250 ore direct: 2,500. Volume 25 m3, 100 ISK/m3.
	body, _, share := postAnonAppraisal(t, app, url.Values{"items": {"Test Ore x 250"}})
	mustContain(t, "ore appraisal", body,
		"2,500.00", "25 m³", "0.1 m³ each", "Total volume 25 m³", "100.00 ISK/m³", "Reprocessing view")

	// At perfect refine: 2 full batches -> 1,400 Tritanium
	// (x6.50) and 600 Pyerite (x9.25 estimate) = 14,650.
	code, body := getAnon(t, app, share+"reprocess")
	if code != http.StatusOK {
		t.Fatalf("reprocess view: status %d, want 200", code)
	}
	mustContain(t, "reprocess view", body,
		"Reprocessing", "Tritanium × 1,400", "Pyerite × 600",
		"14,650.00", "Total reprocess value", "Perfect refine (max skills)",
		"Prune leftover ores below one refine batch", "Only full 100-unit batches reprocess")

	// At 50%: half the yield, 7,325.
	_, body = getAnon(t, app, share+"reprocess?refine=1&rate=50&prune=1")
	mustContain(t, "reprocess at 50%", body, "7,325.00")

	// Below one batch with prune on: pruned, no yield.
	body, _, share2 := postAnonAppraisal(t, app, url.Values{"items": {"Test Ore x 50"}})
	_ = body
	_, body = getAnon(t, app, share2+"reprocess")
	mustContain(t, "pruned ore", body, "Below one refine batch — pruned.", "Total reprocess value")
	if strings.Contains(body, "Tritanium × 350") {
		t.Error("a pruned stack still yielded")
	}
	// With prune off the partial batch counts in proportion:
	// 350 Tritanium x 6.50 + 150 Pyerite x 9.25 = 3,662.50.
	_, body = getAnon(t, app, share2+"reprocess?refine=1&rate=100")
	mustContain(t, "unpruned ore", body, "3,662.50")

	// A non-reprocessable line is listed as such, not dropped.
	body, _, share3 := postAnonAppraisal(t, app, url.Values{"items": {"Rifter x 1"}})
	_ = body
	_, body = getAnon(t, app, share3+"reprocess")
	mustContain(t, "non-reprocessable", body, "Does not reprocess.", "Not reprocessable: Rifter")
}

func TestPublicFitEscapesUserText(t *testing.T) {
	tr := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, tr)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedFitUniverse(t, func(stmt string) error {
		_, err := conn.ExecContext(ctx, stmt)
		return err
	})
	now := time.Now().UTC()
	payload := `<script>alert(1)</script>`
	row, err := q.CreateLocalFitting(ctx, db.CreateLocalFittingParams{
		UserID: user.ID, Name: payload, ShipTypeID: 1001, IsPublic: true,
		CreatedAt: now, UpdatedAt: now,
		ItemsJson: `{"name":"` + payload + `","shipTypeId":1001,"description":"<img src=x onerror=alert(2)>","tags":["<b>tag</b>"],` +
			`"items":[{"typeId":3001,"qty":1}]}`,
	})
	if err != nil {
		t.Fatalf("create fitting: %v", err)
	}
	app.refreshPublicFitPages(ctx)
	code, body := getSwapPage(t, app, nil, publicFitsPath+"fit/"+strconv.FormatInt(row.ID, 10)+"/")
	if code != http.StatusOK {
		t.Fatalf("public fit page: status %d, want 200", code)
	}
	for _, raw := range []string{"<script>alert(1)</script>", "<img src=x onerror=alert(2)>", "<b>tag</b>"} {
		if strings.Contains(body, raw) {
			t.Errorf("public fit page rendered user text %q unescaped", raw)
		}
	}
	mustContain(t, "escaped fit page", body, "&lt;script&gt;alert(1)&lt;/script&gt;")
}
