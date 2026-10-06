package app

// Quick jump (v0.3.15): the Ctrl+K palette and the search pool
// widening behind it. The pool now answers corporations and
// alliances alongside characters, items and pilots; every item
// hit notes the market-history prefetch wants; and the
// palette's static page group must mirror the sidebar exactly,
// so a destination joins both or neither.

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"testing"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

func TestTopbarSearchIncludesOrganizations(t *testing.T) {
	transport := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")
	seedNext1SDE(t, conn)

	corpPayload, err := json.Marshal(corporationRecordPayload{
		Corp: esi.Corporation{Name: "Fixture Corp", Ticker: "FXC"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.SetCorporationRecord(ctx, db.SetCorporationRecordParams{
		CorporationID: fixtureCorpA, Payload: string(corpPayload),
		State: orgStateReady, FetchedAt: "2026-10-01T00:00:00Z",
	}); err != nil {
		t.Fatalf("seed corporation record: %v", err)
	}
	alliancePayload, err := json.Marshal(allianceRecordPayload{
		Alliance: esi.Alliance{Name: "Fixture Alliance", Ticker: "FXA"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.SetAllianceRecord(ctx, db.SetAllianceRecordParams{
		AllianceID: 99000001, Payload: string(alliancePayload),
		State: orgStateReady, FetchedAt: "2026-10-01T00:00:00Z",
	}); err != nil {
		t.Fatalf("seed alliance record: %v", err)
	}

	code, body := getPage(t, app, cookie, "/search.json?q=fixture")
	if code != http.StatusOK {
		t.Fatalf("GET /search.json: status %d", code)
	}
	var hits []searchHit
	if err := json.Unmarshal([]byte(body), &hits); err != nil {
		t.Fatalf("decode hits: %v (%q)", err, body)
	}
	byKind := map[string][]searchHit{}
	for _, h := range hits {
		byKind[h.Kind] = append(byKind[h.Kind], h)
	}
	if len(byKind["corporation"]) != 1 || byKind["corporation"][0].ID != fixtureCorpA ||
		byKind["corporation"][0].Name != "Fixture Corp" {
		t.Errorf("corporation hits = %+v, want the warmed Fixture Corp", byKind["corporation"])
	}
	if len(byKind["alliance"]) != 1 || byKind["alliance"][0].ID != 99000001 ||
		byKind["alliance"][0].Name != "Fixture Alliance" {
		t.Errorf("alliance hits = %+v, want the warmed Fixture Alliance", byKind["alliance"])
	}

	// An item hit notes the market-history prefetch wants the
	// market page's search notes, so the jump lands warm.
	if _, err := conn.ExecContext(ctx, `DELETE FROM market_history_wants`); err != nil {
		t.Fatalf("clear history wants: %v", err)
	}
	code, body = getPage(t, app, cookie, "/search.json?q=tritanium")
	if code != http.StatusOK {
		t.Fatalf("GET /search.json (items): status %d", code)
	}
	hits = nil
	if err := json.Unmarshal([]byte(body), &hits); err != nil {
		t.Fatalf("decode item hits: %v (%q)", err, body)
	}
	itemFound := false
	for _, h := range hits {
		if h.Kind == "item" && h.ID == 34 {
			itemFound = true
		}
	}
	if !itemFound {
		t.Fatalf("item hits = %+v, want Tritanium among them", hits)
	}
	var wantCount int
	if err := conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM market_history_wants WHERE region_id = $1 AND type_id = 34`,
		defaultMarketRegion).Scan(&wantCount); err != nil {
		t.Fatalf("count history wants: %v", err)
	}
	if wantCount != 1 {
		t.Errorf("history wants for Tritanium = %d, want 1 noted", wantCount)
	}

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handlers made %d outbound calls, want 0", got)
	}
}

func TestQuickJumpPaletteMarkupAndScript(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")

	code, body := getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET /: status %d", code)
	}
	mustContain(t, "/", body,
		`id="quickjump-open"`,
		`aria-controls="quickjump"`,
		`<div class="quickjump" id="quickjump" hidden>`,
		`id="quickjump-q"`,
		`id="quickjump-results"`,
		`Powered by EveSynapse v0.3.38.001 🏓`)

	code, js := getPage(t, app, cookie, "/static/app.js")
	if code != http.StatusOK {
		t.Fatalf("GET /static/app.js: status %d", code)
	}
	mustContain(t, "/static/app.js", js,
		"var quickJumpPages = [",
		"/search.json?q=",
		"ctrlKey",
		"metaKey",
		"ArrowDown",
		"ArrowUp",
		`"Enter"`,
		`"Escape"`,
		"/market/?type=",
		"/corporation/?corporation=",
		"/alliance/?alliance=",
		"/pilot/?character=",
		"/character/?character=")

	code, css := getPage(t, app, cookie, "/static/style.css")
	if code != http.StatusOK {
		t.Fatalf("GET /static/style.css: status %d", code)
	}
	mustContain(t, "/static/style.css", css,
		".quickjump {",
		".quickjump-panel {",
		".quickjump-results li.sel",
		"prefers-reduced-motion")

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handlers made %d outbound calls, want 0", got)
	}
}

// TestQuickJumpPagesMatchNav pins the palette's static page
// group against the destinations the sidebar actually offers:
// every sidenav link (account switcher actions aside) is in
// the palette, and the palette invents none.
func TestQuickJumpPagesMatchNav(t *testing.T) {
	base, err := templatesFS.ReadFile("templates/base.html")
	if err != nil {
		t.Fatalf("read base.html: %v", err)
	}
	navStart := strings.Index(string(base), `<nav class="sidenav"`)
	navEnd := strings.Index(string(base), `</nav>`)
	if navStart < 0 || navEnd < navStart {
		t.Fatal("sidenav block not found in base.html")
	}
	hrefRe := regexp.MustCompile(`href="([^"]+)"`)
	navURLs := map[string]bool{}
	for _, m := range hrefRe.FindAllStringSubmatch(string(base)[navStart:navEnd], -1) {
		href := m[1]
		if strings.Contains(href, "switch") || strings.HasPrefix(href, "/auth/") {
			continue
		}
		navURLs[href] = true
	}
	if len(navURLs) == 0 {
		t.Fatal("no nav links extracted from base.html")
	}

	jsBytes, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	js := string(jsBytes)
	blockStart := strings.Index(js, "var quickJumpPages = [")
	blockEnd := strings.Index(js[blockStart:], "];")
	if blockStart < 0 || blockEnd < 0 {
		t.Fatal("quickJumpPages block not found in app.js")
	}
	entryRe := regexp.MustCompile(`\{\s*name: "([^"]+)", url: "([^"]+)"\s*\}`)
	pageURLs := map[string]bool{}
	for _, m := range entryRe.FindAllStringSubmatch(js[blockStart:blockStart+blockEnd], -1) {
		pageURLs[m[2]] = true
	}
	if len(pageURLs) == 0 {
		t.Fatal("no palette pages extracted from app.js")
	}

	var missing, extra []string
	for u := range navURLs {
		if !pageURLs[u] {
			missing = append(missing, u)
		}
	}
	for u := range pageURLs {
		if !navURLs[u] {
			extra = append(extra, u)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("palette misses nav destinations: %v", missing)
	}
	if len(extra) > 0 {
		t.Errorf("palette lists non-nav destinations: %v", extra)
	}
}
