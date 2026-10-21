package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestScanPasteAndRead: a pasted d-scan is sorted by ship class with
// what is on grid, finds its system from its celestials, lands at an
// unlisted link, and is listed back to its paster on the map card.
func TestScanPasteAndRead(t *testing.T) {
	app, conn, _ := buildCorpTestApp(t, nil)
	ctx := context.Background()
	exec := func(q string) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, q); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	exec(`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30000142, 'Jita', 10000002, 0.946) ON CONFLICT DO NOTHING`)
	exec(`INSERT INTO sde_groups (group_id, name, category_id) VALUES (831, 'Interceptor', 6), (8, 'Moon', 2), (1657, 'Citadel', 65) ON CONFLICT DO NOTHING`)
	exec(`INSERT INTO sde_types (type_id, name, group_id) VALUES (11176, 'Crow', 831), (14, 'Moon', 8), (35832, 'Astrahus', 1657) ON CONFLICT DO NOTHING`)

	h := app.Handler()
	paste := "11176\tCrow\tCrow\t1,234 km\n11176\tCrow\tCrow\t-\n14\tJita IV - Moon 4\tMoon\t3.2 AU\n35832\tPerimeter Keepstar\tAstrahus\t42,000 km\n"
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/intel/scan/", strings.NewReader(url.Values{"scan": {paste}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("paste: %d %s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/intel/scan/") || len(loc) != len("/intel/scan/")+13 {
		t.Fatalf("redirect to %q", loc)
	}
	cookies := rec.Result().Cookies()

	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		for _, c := range cookies {
			req.AddCookie(c)
		}
		h.ServeHTTP(rec, req)
		return rec
	}
	page := get(loc)
	if page.Code != http.StatusOK {
		t.Fatalf("reading: %d", page.Code)
	}
	body := page.Body.String()
	for _, want := range []string{"D-scan · 2 ships", "Interceptor", ">Jita<", "1 on grid", "Astrahus"} {
		if !strings.Contains(body, want) {
			t.Errorf("reading lacks %q", want)
		}
	}
	if page.Header().Get("X-Robots-Tag") != "noindex" {
		t.Errorf("a scan must not be indexed")
	}
	if !strings.Contains(get("/map/scans.json?system=30000142").Body.String(), strings.Trim(strings.TrimPrefix(loc, "/intel/scan/"), "/")) {
		t.Errorf("the paster's session lists its own scan on the map card")
	}
	// Another reader (no session) can open the link but lists nothing.
	cookies = nil
	if get(loc).Code != http.StatusOK {
		t.Errorf("an unlisted link opens for anyone")
	}
	if got := get("/map/scans.json?system=30000142").Body.String(); strings.TrimSpace(got) != "[]" {
		t.Errorf("another reader sees no scans: %s", got)
	}
	if get("/intel/scan/notarealid1/").Code != http.StatusNotFound {
		t.Errorf("an unknown scan is a 404")
	}

	// Junk is refused with the paste kept.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/intel/scan/", strings.NewReader(url.Values{"scan": {"a\tb\nc\td"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "doesn’t look like") {
		t.Errorf("junk paste: %d", rec.Code)
	}
}

// TestGroupLocal: pilots fold into alliances with their corporations,
// a corporation outside any alliance stands alone, biggest first.
func TestGroupLocal(t *testing.T) {
	pilots := []localPilot{
		{ID: 1, CorpID: 10, AllianceID: 100, Kills: 3},
		{ID: 2, CorpID: 11, AllianceID: 100},
		{ID: 3, CorpID: 10, AllianceID: 100},
		{ID: 4, CorpID: 20},
	}
	names := map[int64]string{10: "Corp A", 11: "Corp B", 20: "Loners", 100: "The Alliance"}
	g := groupLocal(pilots, names)
	if len(g) != 2 || g[0].Name != "The Alliance" || g[0].N != 3 || g[0].Kills != 3 {
		t.Fatalf("groups: %+v", g)
	}
	if len(g[0].Corps) != 2 || g[0].Corps[0].Name != "Corp A" || g[0].Corps[0].N != 2 {
		t.Errorf("corps: %+v", g[0].Corps)
	}
	if !g[1].Corp || g[1].ID != 20 || g[1].Name != "Loners" {
		t.Errorf("an alliance-less corporation stands alone: %+v", g[1])
	}
}
