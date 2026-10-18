package app

// The public contract search + detail pages and the View in
// game handler (contracts module, search.go): anonymous
// stored reads, both-direction contents search, blueprint and
// deal filters, courier privacy, auction bids, and the
// scope present/absent paths for openwindow.

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// openWindowTransport records the requests the app makes and
// answers openwindow the way ESI does (204, no body).
type openWindowTransport struct {
	mu    sync.Mutex
	paths []string
}

func (tr *openWindowTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tr.mu.Lock()
	tr.paths = append(tr.paths, req.Method+" "+req.URL.Path+"?"+req.URL.RawQuery)
	tr.mu.Unlock()
	if strings.HasPrefix(req.URL.Path, "/ui/openwindow/") {
		return &http.Response{StatusCode: 204, Body: http.NoBody, Header: make(http.Header)}, nil
	}
	return &http.Response{StatusCode: 404, Body: http.NoBody, Header: make(http.Header)}, nil
}

func (tr *openWindowTransport) sawOpenWindow() bool {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	for _, p := range tr.paths {
		if strings.Contains(p, "/ui/openwindow/contract/") {
			return true
		}
	}
	return false
}

// seedContractCorpus stores a small public corpus directly:
// the pages read stored rows, so the sweep is not under test
// here (PR 1 covers it). Issuer names are primed into the ESI
// name cache the way the startup entity_names load does it.
func seedContractCorpus(t *testing.T, app *Application, conn *sql.DB) {
	app.esi.PrimeName(esi.NameCharacter, 9001, "Issuer One")
	app.esi.PrimeName(esi.NameCorporation, 9801, "Issuer Corp")
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("seed %q: %v", query, err)
		}
	}
	exec(`INSERT INTO sde_regions (region_id, name) VALUES (10000002, 'The Forge'), (10000001, 'Domain')`)
	exec(`INSERT INTO sde_systems (system_id, name, region_id) VALUES (30000142, 'Jita', 10000002), (30000144, 'Urlen', 10000002)`)
	exec(`INSERT INTO sde_stations (station_id, name, system_id) VALUES (60003760, 'Jita IV - Moon 4 - Caldari Navy Assembly Plant', 30000142), (60003761, 'Urlen - Test Station', 30000144)`)
	exec(`INSERT INTO sde_types (type_id, name) VALUES (34, 'Tritanium'), (35, 'Pyerite'), (9999, 'Test Blueprint')`)
	exec(`INSERT INTO entity_names (kind, id, name) VALUES ('character', 9001, 'Issuer One'), ('corporation', 9801, 'Issuer Corp')`)

	contract := func(id int64, typ, title string, price, buyout, reward, collateral, volume float64,
		startLoc, endLoc int64, state string) {
		t.Helper()
		exec(`INSERT INTO pub_contracts (contract_id, region_id, type, issuer_id, issuer_corporation_id,
			title, price, buyout, reward, collateral, volume, start_location_id, end_location_id,
			date_issued, date_expired, days_to_complete, items_state, last_cycle, first_seen, last_seen)
			VALUES ($1, 10000002, $2, 9001, 9801, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, 1, $13, 1, $11, $11)`,
			id, typ, title, price, buyout, reward, collateral, volume, startLoc, endLoc,
			now.Add(-time.Hour), now.Add(7*24*time.Hour), state)
	}
	item := func(rec, contractID, typeID, qty int64, included bool) {
		t.Helper()
		exec(`INSERT INTO pub_contract_items (record_id, contract_id, type_id, quantity, is_included)
			VALUES ($1, $2, $3, $4, $5)`, rec, contractID, typeID, qty, included)
	}
	appraise := func(contractID int64, offered, want float64, priced, total int64, ratio sql.NullFloat64, mix string) {
		t.Helper()
		exec(`UPDATE pub_contracts SET offered_value=$2, want_value=$3, priced_lines=$4, total_lines=$5,
			deal_ratio=$6, tier_mix=$7, priced_at=$8 WHERE contract_id=$1`,
			contractID, offered, want, priced, total, ratio, mix, now)
	}
	priceLine := func(rec int64, unit, line float64, tier string) {
		t.Helper()
		exec(`UPDATE pub_contract_items SET unit_price=$2, line_value=$3, price_tier=$4, price_asof=$5 WHERE record_id=$1`,
			rec, unit, line, tier, now)
	}

	// A: a fully priced exchange at exactly 85% of value.
	contract(7001, "item_exchange", "Cheap Tritanium", 850, 0, 0, 0, 0, 60003760, 0, "stored")
	item(1, 7001, 34, 10, true)
	priceLine(1, 100, 1000, "typical")
	appraise(7001, 1000, 0, 1, 1, sql.NullFloat64{Float64: 0.85, Valid: true}, "typical:1")

	// B: wants Tritanium, offers Pyerite.
	contract(7002, "item_exchange", "Swap Meet", 0, 0, 0, 0, 0, 60003760, 0, "stored")
	item(2, 7002, 35, 5, true)
	item(3, 7002, 34, 100, false)
	priceLine(3, 100, 10000, "typical")
	appraise(7002, 0, 10000, 0, 1, sql.NullFloat64{}, "typical:1")

	// C: a courier; cargo private by design.
	contract(7003, "courier", "Move My Stuff", 0, 0, 5000000, 100000, 1000, 60003760, 60003761, "private")

	// D: an auction with two bids.
	contract(7004, "auction", "Rare Relic", 100, 2000, 0, 0, 0, 60003760, 0, "stored")
	item(4, 7004, 35, 1, true)
	exec(`INSERT INTO pub_contract_bids (bid_id, contract_id, amount, date_bid) VALUES (1, 7004, 500, $1), (2, 7004, 700, $2)`,
		now.Add(-2*time.Hour), now.Add(-time.Hour))

	// E1/E2: a BPO listing and a BPC listing.
	contract(7005, "item_exchange", "Original For Sale", 5000, 0, 0, 0, 0, 60003760, 0, "stored")
	exec(`INSERT INTO pub_contract_items (record_id, contract_id, type_id, quantity, is_included,
		is_blueprint_copy, material_efficiency, time_efficiency) VALUES (5, 7005, 9999, 1, true, false, 10, 20)`)
	contract(7006, "item_exchange", "Copy For Sale", 500, 0, 0, 0, 0, 60003760, 0, "stored")
	exec(`INSERT INTO pub_contract_items (record_id, contract_id, type_id, quantity, is_included,
		is_blueprint_copy, runs, material_efficiency, time_efficiency) VALUES (6, 7006, 9999, 1, true, true, 5, 5, 10)`)

	// G: a partially priced exchange: cheap, but no deal flag
	// may ever hang off half an appraisal.
	contract(7007, "item_exchange", "Half Known", 10, 0, 0, 0, 0, 60003760, 0, "stored")
	item(7, 7007, 34, 1, true)
	item(8, 7007, 35, 1, true)
	priceLine(7, 100, 100, "typical")
	appraise(7007, 100, 0, 1, 2, sql.NullFloat64{}, "typical:1")
}

func getAnon(t *testing.T, app *Application, path string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Result().Body)
	return rec.Code, string(body)
}

func TestContractSearchPages(t *testing.T) {
	tr := &openWindowTransport{}
	app, conn, _ := buildCorpTestApp(t, tr)
	seedContractCorpus(t, app, conn)

	code, body := getAnon(t, app, "/contracts/search/")
	if code != 200 {
		t.Fatalf("anonymous search: status %d, want 200", code)
	}
	mustContain(t, "/contracts/search/", body,
		"Cheap Tritanium", "Swap Meet", "Move My Stuff", "Rare Relic", "Half Known")

	// Contents search runs in both directions.
	_, body = getAnon(t, app, "/contracts/search/?q_item=Tritanium&item_dir=wanted")
	mustContain(t, "wanted search", body, "Swap Meet")
	if strings.Contains(body, "Cheap Tritanium") {
		t.Error("wanted-side search must not match contracts merely offering the item")
	}
	_, body = getAnon(t, app, "/contracts/search/?q_item=Tritanium&item_dir=offered")
	mustContain(t, "offered search", body, "Cheap Tritanium")
	if strings.Contains(body, "Swap Meet") {
		t.Error("offered-side search must not match contracts merely wanting the item")
	}

	// Deals only: the full-coverage 85% contract, never the
	// partially priced one.
	_, body = getAnon(t, app, "/contracts/search/?deals=1")
	mustContain(t, "deals search", body, "Cheap Tritanium")
	if strings.Contains(body, "Half Known") {
		t.Error("a partially priced contract must never appear under deals only")
	}

	// Blueprint filters.
	_, body = getAnon(t, app, "/contracts/search/?bp=bpo&me_min=10")
	mustContain(t, "bpo search", body, "Original For Sale")
	if strings.Contains(body, "Copy For Sale") {
		t.Error("BPO filter matched a copy")
	}
	_, body = getAnon(t, app, "/contracts/search/?bp=bpc&runs_min=3")
	mustContain(t, "bpc search", body, "Copy For Sale")
	if strings.Contains(body, "Original For Sale") {
		t.Error("BPC filter matched an original")
	}

	// Courier type filter.
	_, body = getAnon(t, app, "/contracts/search/?type=courier")
	mustContain(t, "courier search", body, "Move My Stuff")
	if strings.Contains(body, "Cheap Tritanium") {
		t.Error("courier filter matched an exchange")
	}
}

func TestContractDetailPages(t *testing.T) {
	tr := &openWindowTransport{}
	app, conn, _ := buildCorpTestApp(t, tr)
	seedContractCorpus(t, app, conn)

	code, body := getAnon(t, app, "/contracts/search/7001/")
	if code != 200 {
		t.Fatalf("anonymous detail: status %d, want 200", code)
	}
	mustContain(t, "detail 7001", body,
		"Cheap Tritanium", "Tritanium", "Our Forge typical",
		"Asking 85% of our appraised value", "Issuer One", "Issuer Corp")

	// The courier page shows the route and the economics, and
	// says plainly why there is no contents list.
	code, body = getAnon(t, app, "/contracts/search/7003/")
	if code != 200 {
		t.Fatalf("courier detail: status %d, want 200", code)
	}
	mustContain(t, "detail 7003", body,
		"Move My Stuff", "Urlen - Test Station", "private")
	if strings.Contains(body, "<h3>Offered</h3>") {
		t.Error("courier detail must not render a contents section")
	}

	// The auction page lists its bids, top first.
	_, body = getAnon(t, app, "/contracts/search/7004/")
	mustContain(t, "detail 7004", body, "Top bid", "700.00 ISK")
	if strings.Index(body, "700.00 ISK") > strings.Index(body, "500.00 ISK") {
		t.Error("bids should list the top bid before the lower one")
	}

	// A contract outside the corpus is a clean 404.
	if code, _ := getAnon(t, app, "/contracts/search/999999/"); code != 404 {
		t.Errorf("unknown contract: status %d, want 404", code)
	}

	// Over-asking phrasing: past 100% of value the notes name
	// the excess instead of a share over the whole — the
	// search row note and the detail note alike. (The 85%
	// "of value" wording is pinned above.)
	if _, err := conn.ExecContext(context.Background(),
		`UPDATE pub_contracts SET deal_ratio=1.54 WHERE contract_id=7001`); err != nil {
		t.Fatalf("re-ratio 7001: %v", err)
	}
	_, body = getAnon(t, app, "/contracts/search/")
	mustContain(t, "search above value", body, "asking 54% above value")
	if strings.Contains(body, "asking 154% of value") {
		t.Error("row note still frames over-asking as a share of value")
	}
	_, body = getAnon(t, app, "/contracts/search/7001/")
	mustContain(t, "detail above value", body, "Asking 54% above our appraised value")
	if strings.Contains(body, "Asking 154% of our appraised value") {
		t.Error("detail note still frames over-asking as a share of value")
	}
}

// TestOpenContractInGame covers the View in game handler:
// anonymous readers bounce off auth, a character whose grant
// predates the scope gets the sign-in-again path with no ESI
// call, and a scoped character's click reaches ESI.
func TestOpenContractInGame(t *testing.T) {
	tr := &openWindowTransport{}
	app, conn, q := buildCorpTestApp(t, tr)
	seedContractCorpus(t, app, conn)
	ctx := context.Background()

	// Anonymous: RequireAuth stops the POST before any ESI call.
	req := httptest.NewRequest(http.MethodPost, "/ui/openwindow/contract/7001", nil)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code == 200 {
		t.Error("anonymous openwindow POST must not succeed")
	}
	if tr.sawOpenWindow() {
		t.Fatal("anonymous POST reached ESI")
	}

	mkUser := func(characterID int64, scopes string) (*http.Cookie, string) {
		t.Helper()
		user, err := q.CreateUser(ctx)
		if err != nil {
			t.Fatalf("create user: %v", err)
		}
		name := "Pilot " + strings.TrimSpace(strings.Split(scopes, " ")[0])
		if _, err := q.UpsertCharacter(ctx, db.UpsertCharacterParams{
			CharacterID: characterID, UserID: user.ID, Name: name,
			AccessToken: "fixture", RefreshToken: "fixture",
			TokenExpiry: mustNullTime("2999-01-01T00:00:00Z"),
			Scopes:      scopes, LinkState: "ok",
		}); err != nil {
			t.Fatalf("seed character: %v", err)
		}
		return sessionCookie(t, app, user.ID, characterID, name), name
	}
	post := func(cookie *http.Cookie) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/ui/openwindow/contract/7001", nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec.Code
	}

	// Grant predates the scope: redirect back, no ESI call, and
	// the detail page says to sign in again.
	oldCookie, _ := mkUser(9101, "esi-contracts.read_character_contracts.v1")
	if code := post(oldCookie); code != http.StatusSeeOther {
		t.Errorf("pre-scope POST: status %d, want 303 back to the contract", code)
	}
	if tr.sawOpenWindow() {
		t.Fatal("pre-scope character reached ESI")
	}
	code, body := getPage(t, app, oldCookie, "/contracts/search/7001/")
	if code != 200 {
		t.Fatalf("detail for pre-scope user: status %d", code)
	}
	mustContain(t, "pre-scope detail", body,
		"were linked before EveSynapse could open contracts in the game", "Sign in again")

	// Scoped character: the click reaches ESI's openwindow.
	newCookie, _ := mkUser(9102, "esi-contracts.read_character_contracts.v1 esi-ui.open_window.v1")
	if code := post(newCookie); code != http.StatusSeeOther {
		t.Errorf("scoped POST: status %d, want 303 back to the contract", code)
	}
	if !tr.sawOpenWindow() {
		t.Error("scoped character's POST never reached ESI openwindow")
	}
}
