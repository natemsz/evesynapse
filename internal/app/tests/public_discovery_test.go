package tests

// Public discovery: SEO files, the anonymous fit browser, fit
// pages, ratings, share, the anonymous simulator, and the public
// item and ship pages. Every route here answers with no session;
// the counting transport fails the suite on any outbound call.

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"evesynapse/internal/app"
	"evesynapse/internal/apptest"
	db "evesynapse/internal/db/sqlc"
)

func seedPublicSDE(t *testing.T, conn *sql.DB) {
	t.Helper()
	ctx := context.Background()
	for _, stmt := range []string{
		`INSERT INTO sde_categories (category_id, name) VALUES (6, 'Ship'), (7, 'Module')`,
		`INSERT INTO sde_groups (group_id, name, category_id) VALUES (25, 'Frigate', 6), (18, 'Mining', 7), (99, 'Shield Extender', 7)`,
		`INSERT INTO sde_types (type_id, name, group_id, market_group_id, published, description) VALUES
			(587, 'Rifter', 25, 1, 1, 'The Rifter is a frigate.'),
			(1234, 'Republic Fleet Medium Shield Extender', 99, 1, 1, 'Faction extender.'),
			(1236, 'Medium Shield Extender II', 99, 1, 1, 'Tech II extender.'),
			(1238, 'Medium Shield Extender I', 99, 1, 1, 'Basic extender.')`,
		`INSERT INTO sde_type_attributes (type_id, attribute_id, value) VALUES (1234, 1692, 4), (1236, 1692, 2)`,
		`INSERT INTO type_names (type_id, name) VALUES (587, 'Rifter'), (1234, 'Republic Fleet Medium Shield Extender'), (1236, 'Medium Shield Extender II'), (1238, 'Medium Shield Extender I')`,
		`INSERT INTO sde_regions (region_id, name) VALUES (10000002, 'The Forge'), (10000043, 'Domain')`,
	} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed sde: %v", err)
		}
	}
}

func seedPublicFit(t *testing.T, q *db.Queries, userID int64, name string, ship int64, items, tags string, updated time.Time) int64 {
	t.Helper()
	doc := fmt.Sprintf(`{"name":%q,"shipTypeId":%d,"items":[%s],"tags":[%s]}`, name, ship, items, tags)
	now := time.Now().UTC()
	row, err := q.CreateLocalFitting(context.Background(), db.CreateLocalFittingParams{
		UserID: userID, Name: name, ShipTypeID: ship, ItemsJson: doc,
		IsPublic: true, CreatedAt: now, UpdatedAt: updated,
	})
	if err != nil {
		t.Fatalf("seed fit %s: %v", name, err)
	}
	return row.ID
}

// rateFit seeds a rating through the real rate endpoint, so the
// helper exercises the handler's delta math instead of
// reimplementing it.
func rateFit(t *testing.T, rig *app.TestRig, cookie *http.Cookie, fitID int64, stars string) {
	t.Helper()
	path := fmt.Sprintf("/fittings/public/fit/%d/rate", fitID)
	if code, _ := postPublicForm(t, rig, cookie, path, url.Values{"stars": {stars}}); code != http.StatusSeeOther {
		t.Fatalf("rate fit %d: status = %d, want 303", fitID, code)
	}
}

func postPublicForm(t *testing.T, rig *app.TestRig, cookie *http.Cookie, path string, values url.Values) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	rig.Handler().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func TestPublicSEOFiles(t *testing.T) {
	transport := &apptest.CountingTransport{}
	rig := apptest.Build(t, transport)

	code, body := apptest.GetPage(t, rig, nil, "/robots.txt")
	if code != 200 {
		t.Fatalf("robots status = %d", code)
	}
	apptest.MustContain(t, "/robots.txt", body,
		"User-agent: Googlebot",
		"User-agent: GPTBot",
		"Allow: /fittings/public/",
		"Allow: /items/",
		"Allow: /ships/",
		"Disallow: /fittings/saved/",
		"Sitemap:",
	)

	// Seed a public fit plus swept stats, so the sitemap has
	// fit, item and ship content beyond the core pages. URLs use
	// the canonical origin, never the request host.
	q := rig.Queries()
	ctx := context.Background()
	seedPublicSDE(t, rig.DB())
	ua, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fitID := seedPublicFit(t, q, ua.ID, "Sitemap Rifter", 587, `{"typeId":1238,"qty":1}`, `"pvp"`, time.Now().UTC())
	if err := q.UpsertMarketRegionStat(ctx, db.UpsertMarketRegionStatParams{
		RegionID: 10000002, TypeID: 587, BestSell: 120, BestBuy: 100, UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed stats: %v", err)
	}

	code, body = apptest.GetPage(t, rig, nil, "/sitemap.xml")
	if code != 200 {
		t.Fatalf("sitemap status = %d", code)
	}
	apptest.MustContain(t, "/sitemap.xml", body,
		"<urlset", "</loc>",
		"https://evesynapse.app/</loc>",
		"https://evesynapse.app/fittings/public/</loc>",
		"https://evesynapse.app/fittings/public/fit/"+fmt.Sprintf("%d", fitID)+"/",
		"https://evesynapse.app/items/type/587/",
		"https://evesynapse.app/ships/587/",
	)
	for _, authed := range []string{"/admin/", "/api/", "/fittings/saved/", "/auth/"} {
		if strings.Contains(body, authed) {
			t.Errorf("sitemap leaks authed path %s", authed)
		}
	}
	if transport.Calls.Load() != 0 {
		t.Errorf("seo pages made %d outbound calls", transport.Calls.Load())
	}
}

func seedPublicBrowser(t *testing.T, rig *app.TestRig) (q *db.Queries, userA, userB int64) {
	t.Helper()
	q = rig.Queries()
	ctx := context.Background()
	seedPublicSDE(t, rig.DB())
	ua, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ub, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	apptest.SeedCharacter(t, q, ua.ID, apptest.FixtureCharA, "Alpha Pilot")
	now := time.Now().UTC()
	items := func(ids ...int64) string {
		parts := make([]string, 0, len(ids))
		for _, id := range ids {
			parts = append(parts, fmt.Sprintf(`{"typeId":%d,"qty":1}`, id))
		}
		return strings.Join(parts, ",")
	}
	// Two 5-average Rifter fits: one 5-star vote vs three
	// 5-star votes, plus a 1-star fit dragging the global mean
	// down, so the Bayesian rank must prefer the three votes.
	cookieA := apptest.SessionCookie(t, rig, ua.ID, apptest.FixtureCharA, "Alpha Pilot")
	cookieB := apptest.SessionCookie(t, rig, ub.ID, 93300002, "Beta Pilot")
	one := seedPublicFit(t, q, ua.ID, "Solo Rifter", 587, items(1238), `"pvp"`, now)
	three := seedPublicFit(t, q, ub.ID, "Fleet Rifter", 587, items(1234), `"pvp","Logistics"`, now)
	bad := seedPublicFit(t, q, ub.ID, "Bad Rifter", 587, items(1238), `"meme"`, now)
	rateFit(t, rig, cookieB, one, "5")
	rateFit(t, rig, cookieA, three, "5")
	uc, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rateFit(t, rig, apptest.SessionCookie(t, rig, uc.ID, 93300003, "Gamma Pilot"), three, "5")
	ud, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rateFit(t, rig, apptest.SessionCookie(t, rig, ud.ID, 93300004, "Delta Pilot"), three, "5")
	rateFit(t, rig, cookieA, bad, "1")
	// T2 fit for the faction/alpha filters.
	seedPublicFit(t, q, ua.ID, "T2 Rifter", 587, items(1236), `"pvp"`, now)
	// A fit last touched twenty days ago: listed like any other,
	// and outside the optional Updated filter's 14 days.
	seedPublicFit(t, q, ua.ID, "Old Rifter", 587, items(1238), `"pvp"`, now.Add(-20*24*time.Hour))
	// Priced fit for the max-price filter.
	priced := seedPublicFit(t, q, ua.ID, "Rich Rifter", 587, items(1234, 1236), `"pvp"`, now)
	if err := q.SetLocalFittingPrice(ctx, db.SetLocalFittingPriceParams{TotalPrice: 900000000, ID: priced, UserID: ua.ID}); err != nil {
		t.Fatal(err)
	}
	return q, ua.ID, ub.ID
}

func TestPublicFitsBrowser(t *testing.T) {
	transport := &apptest.CountingTransport{}
	rig := apptest.Build(t, transport)
	_, userA, _ := seedPublicBrowser(t, rig)

	get := func(path string) (int, string) {
		return apptest.GetPage(t, rig, nil, path)
	}
	code, body := get("/fittings/public/")
	if code != 200 {
		t.Fatalf("browser status = %d", code)
	}
	// The default view is every public fit, newest first: nothing is
	// hidden for its age, and the twenty-day-old fit comes last.
	apptest.MustContain(t, "/fittings/public/", body, "Fleet Rifter", "Solo Rifter", "T2 Rifter", "Old Rifter", "Last 14 days")
	for _, name := range []string{"Fleet Rifter", "Solo Rifter", "T2 Rifter", "Rich Rifter", "Bad Rifter"} {
		if at, old := strings.Index(body, name), strings.Index(body, "Old Rifter"); at < 0 || at > old {
			t.Errorf("default view: %s is not listed before the oldest fit", name)
		}
	}

	// Rank is a sort like the others: Bayesian, and it hides nothing.
	_, body = get("/fittings/public/?sort=rank")
	apptest.MustContain(t, "sort=rank", body, "Old Rifter")
	first, second := strings.Index(body, "Fleet Rifter"), strings.Index(body, "Solo Rifter")
	if first < 0 || second < 0 || first > second {
		t.Errorf("Bayesian rank did not prefer three 5-star votes over one")
	}

	// Age is a filter the visitor picks, under any sort.
	for _, path := range []string{"/fittings/public/?updated=14", "/fittings/public/?updated=14&sort=rank", "/fittings/public/?updated=14&sort=old"} {
		_, body = get(path)
		apptest.MustContain(t, path, body, "Fleet Rifter")
		if strings.Contains(body, "Old Rifter") {
			t.Errorf("%s shows a fit updated twenty days ago", path)
		}
	}
	_, body = get("/fittings/public/?updated=30")
	apptest.MustContain(t, "updated=30", body, "Old Rifter")
	// An unlisted age is no filter at all.
	_, body = get("/fittings/public/?updated=3")
	apptest.MustContain(t, "updated=3", body, "Old Rifter")

	// Oldest sort spans all time.
	_, body = get("/fittings/public/?sort=old")
	apptest.MustContain(t, "sort=old", body, "Old Rifter")

	// Tag and role (roles match tags) filters.
	_, body = get("/fittings/public/?tag=pvp")
	apptest.MustContain(t, "tag=pvp", body, "Fleet Rifter", "Solo Rifter")
	_, body = get("/fittings/public/?role=Logistics")
	apptest.MustContain(t, "role=Logistics", body, "Fleet Rifter")
	if strings.Contains(body, "Solo Rifter") {
		t.Errorf("role filter leaked a fit without the tag")
	}

	// Ship class and hull search.
	_, body = get("/fittings/public/?hull=Frigate")
	apptest.MustContain(t, "hull=Frigate", body, "Fleet Rifter")
	_, body = get("/fittings/public/?hullq=rift")
	apptest.MustContain(t, "hullq=rift", body, "Fleet Rifter")

	// Stars, includes and excludes.
	_, body = get("/fittings/public/?stars=4.5")
	apptest.MustContain(t, "stars=4.5", body, "Fleet Rifter", "Solo Rifter")
	if strings.Contains(body, "T2 Rifter") {
		t.Errorf("stars filter leaked an unrated fit")
	}
	_, body = get("/fittings/public/?include=Medium+Shield+Extender+I")
	apptest.MustContain(t, "include=", body, "Solo Rifter")
	if strings.Contains(body, "T2 Rifter") {
		t.Errorf("include filter leaked a fit without the module")
	}
	_, body = get("/fittings/public/?exclude=Medium+Shield+Extender+I")
	if strings.Contains(body, "Solo Rifter") {
		t.Errorf("exclude filter leaked a fit with the module")
	}
	_, body = get("/fittings/public/?include=Nope+Module")
	apptest.MustContain(t, "include=Nope", body, "that filter was ignored")

	// Faction filter: Any shows everything, No faction modules
	// drops every fit with a faction-meta module — including the
	// Rich Rifter, which mixes a faction extender with a T2 one.
	_, body = get("/fittings/public/?faction=any")
	apptest.MustContain(t, "faction=any", body, "Fleet Rifter", "Rich Rifter")
	_, body = get("/fittings/public/?faction=none")
	apptest.MustContain(t, "faction=none", body, "Solo Rifter", "T2 Rifter")
	if strings.Contains(body, "Fleet Rifter") || strings.Contains(body, "Rich Rifter") {
		t.Errorf("faction=none leaked a faction fit")
	}

	// Max price excludes only priced fits above the cap: the
	// unpriced Solo Rifter stays visible under a tiny cap.
	_, body = get("/fittings/public/?maxprice=1000")
	apptest.MustContain(t, "maxprice=1000", body, "Solo Rifter")
	if strings.Contains(body, "Rich Rifter") {
		t.Errorf("maxprice filter leaked a 900M fit")
	}
	_, body = get("/fittings/public/?alpha=1")
	apptest.MustContain(t, "alpha=1", body, "Solo Rifter")
	if strings.Contains(body, "T2 Rifter") {
		t.Errorf("alpha filter leaked a Tech II fit")
	}

	// Pagination is 24 per page.
	for i := 0; i < 24; i++ {
		seedPublicFit(t, rig.Queries(), userA, fmt.Sprintf("Bulk %02d", i), 587, `{"typeId":1238,"qty":1}`, `"pvp"`, time.Now().UTC())
	}
	code, body = get("/fittings/public/?sort=new")
	if code != 200 {
		t.Fatalf("bulk page status = %d", code)
	}
	if got := strings.Count(body, "<article"); got != 24 {
		t.Errorf("page 1 holds %d cards, want 24", got)
	}
	_, body = get("/fittings/public/?sort=new&page=2")
	if got := strings.Count(body, "<article"); got < 1 {
		t.Errorf("page 2 holds %d cards, want the rest", got)
	}
	if transport.Calls.Load() != 0 {
		t.Errorf("browser made %d outbound calls", transport.Calls.Load())
	}
}

func TestPublicFitPage(t *testing.T) {
	transport := &apptest.CountingTransport{}
	rig := apptest.Build(t, transport)
	q, userA, _ := seedPublicBrowser(t, rig)
	ctx := context.Background()

	var fleetID int64
	rows, err := q.ListPublicFits(ctx, db.ListPublicFitsParams{Sort: "rank", Rlimit: 50})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Name == "Fleet Rifter" {
			fleetID = row.ID
		}
	}
	if fleetID == 0 {
		t.Fatal("fleet fit missing")
	}

	code, body := apptest.GetPage(t, rig, nil, fmt.Sprintf("/fittings/public/fit/%d/", fleetID))
	if code != 200 {
		t.Fatalf("fit page status = %d", code)
	}
	apptest.MustContain(t, "fit page", body,
		"Fleet Rifter", "Rifter",
		"Republic Fleet Medium Shield Extender",
		"og:title", "og:description", "og:url",
		"Copy link", "Copy EFT", "Share to X", "Share to Reddit",
		"x.com/intent/post", "reddit.com/submit",
		"Sign in with EVE SSO",
		"5.0", "3 ratings",
	)
	if !strings.Contains(body, "[Rifter, Fleet Rifter]") {
		t.Errorf("fit page missing the EFT block")
	}
	// Views count the load.
	_, body = apptest.GetPage(t, rig, nil, fmt.Sprintf("/fittings/public/fit/%d/", fleetID))
	apptest.MustContain(t, "second view", body, "2 views")

	// Unknown, private and draft fits 404.
	if code, _ := apptest.GetPage(t, rig, nil, "/fittings/public/fit/424242/"); code != 404 {
		t.Errorf("unknown fit status = %d, want 404", code)
	}
	priv, err := q.CreateLocalFitting(ctx, db.CreateLocalFittingParams{
		UserID: userA, Name: "Secret", ShipTypeID: 587,
		ItemsJson: `{"name":"Secret","shipTypeId":587,"items":[]}`,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := apptest.GetPage(t, rig, nil, fmt.Sprintf("/fittings/public/fit/%d/", priv.ID)); code != 404 {
		t.Errorf("private fit status = %d, want 404", code)
	}
	if transport.Calls.Load() != 0 {
		t.Errorf("fit page made %d outbound calls", transport.Calls.Load())
	}
}

func TestPublicFitRating(t *testing.T) {
	transport := &apptest.CountingTransport{}
	rig := apptest.Build(t, transport)
	q, userA, _ := seedPublicBrowser(t, rig)
	ctx := context.Background()

	var soloID int64
	rows, err := q.ListPublicFits(ctx, db.ListPublicFitsParams{Sort: "rank", Rlimit: 50})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Name == "Solo Rifter" {
			soloID = row.ID
		}
	}
	ratePath := fmt.Sprintf("/fittings/public/fit/%d/rate", soloID)
	cookieA := apptest.SessionCookie(t, rig, userA, apptest.FixtureCharA, "Alpha Pilot")

	// Logged out: the auth gate redirects to login.
	if code, _ := postPublicForm(t, rig, nil, ratePath, url.Values{"stars": {"5"}}); code != http.StatusSeeOther {
		t.Errorf("logged-out rate status = %d, want 303", code)
	}

	// Rating records one row per account and moves the aggregates.
	if code, _ := postPublicForm(t, rig, cookieA, ratePath, url.Values{"stars": {"4"}}); code != http.StatusSeeOther {
		t.Fatalf("rate status = %d", code)
	}
	got, err := q.GetPublicFitting(ctx, soloID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RatingCount != 2 || got.RatingSum != 9 {
		t.Errorf("after rate: count=%d sum=%d, want 2 and 9", got.RatingCount, got.RatingSum)
	}

	// Re-rating updates the same row: count holds, sum moves.
	if code, _ := postPublicForm(t, rig, cookieA, ratePath, url.Values{"stars": {"2"}}); code != http.StatusSeeOther {
		t.Fatalf("re-rate status = %d", code)
	}
	got, _ = q.GetPublicFitting(ctx, soloID)
	if got.RatingCount != 2 || got.RatingSum != 7 {
		t.Errorf("after re-rate: count=%d sum=%d, want 2 and 7", got.RatingCount, got.RatingSum)
	}

	// A second character on the same account shares the row.
	apptest.SeedCharacter(t, q, userA, apptest.FixtureCharB, "Beta Pilot")
	cookieA2 := apptest.SessionCookie(t, rig, userA, apptest.FixtureCharB, "Beta Pilot")
	if code, _ := postPublicForm(t, rig, cookieA2, ratePath, url.Values{"stars": {"5"}}); code != http.StatusSeeOther {
		t.Fatalf("same-account rate status = %d", code)
	}
	got, _ = q.GetPublicFitting(ctx, soloID)
	if got.RatingCount != 2 || got.RatingSum != 10 {
		t.Errorf("after same-account rate: count=%d sum=%d, want 2 and 10", got.RatingCount, got.RatingSum)
	}

	// A fresh account adds a row (userB rated this fit in the seed).
	uc, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cookieC := apptest.SessionCookie(t, rig, uc.ID, 93300004, "Gamma Pilot")
	if code, _ := postPublicForm(t, rig, cookieC, ratePath, url.Values{"stars": {"3"}}); code != http.StatusSeeOther {
		t.Fatalf("second account rate status = %d", code)
	}
	got, _ = q.GetPublicFitting(ctx, soloID)
	if got.RatingCount != 3 || got.RatingSum != 13 {
		t.Errorf("after second account: count=%d sum=%d, want 3 and 13", got.RatingCount, got.RatingSum)
	}

	// Bad input and unknown fits.
	if code, _ := postPublicForm(t, rig, cookieA, ratePath, url.Values{"stars": {"6"}}); code != http.StatusBadRequest {
		t.Errorf("stars=6 status = %d, want 400", code)
	}
	if code, _ := postPublicForm(t, rig, cookieA, "/fittings/public/fit/424242/rate", url.Values{"stars": {"5"}}); code != http.StatusNotFound {
		t.Errorf("unknown fit rate status = %d, want 404", code)
	}
}

func TestPublicSimulatorAnon(t *testing.T) {
	transport := &apptest.CountingTransport{}
	rig := apptest.Build(t, transport)
	seedPublicSDE(t, rig.DB())

	code, body := apptest.GetPage(t, rig, nil, "/fittings/")
	if code != 200 {
		t.Fatalf("simulator status = %d", code)
	}
	apptest.MustContain(t, "/fittings/", body,
		"Fitting with All-V skills",
		"/auth/eve",
		"All V",
	)
	for _, leaked := range []string{`id="fit-save"`, `id="fit-yourfits-search"`, "Alpha Pilot", "Beta Pilot"} {
		if strings.Contains(body, leaked) {
			t.Errorf("anonymous simulator leaks %q", leaked)
		}
	}

	// Pilot 0 simulates; a foreign pilot is rejected without a session.
	simBody := `{"fit":{"name":"x","shipTypeId":587,"items":[{"typeId":1238,"qty":1}]},"pilot":0}`
	req := httptest.NewRequest(http.MethodPost, "/fittings/simulate/", strings.NewReader(simBody))
	rec := httptest.NewRecorder()
	rig.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("anon simulate status = %d", rec.Code)
	}
	simBody = `{"fit":{"name":"x","shipTypeId":587,"items":[{"typeId":1238,"qty":1}]},"pilot":93300003}`
	req = httptest.NewRequest(http.MethodPost, "/fittings/simulate/", strings.NewReader(simBody))
	rec = httptest.NewRecorder()
	rig.Handler().ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Errorf("anon foreign-pilot simulate status = %d, want 400", rec.Code)
	}
	if transport.Calls.Load() != 0 {
		t.Errorf("simulator made %d outbound calls", transport.Calls.Load())
	}
}

func TestPublicItemAndShipPages(t *testing.T) {
	transport := &apptest.CountingTransport{}
	rig := apptest.Build(t, transport)
	q := rig.Queries()
	ctx := context.Background()
	seedPublicSDE(t, rig.DB())
	ua, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	apptest.SeedCharacter(t, q, ua.ID, apptest.FixtureCharA, "Alpha Pilot")
	now := time.Now().UTC()
	seedPublicFit(t, q, ua.ID, "Solo Rifter", 587, `{"typeId":1238,"qty":1}`, `"pvp"`, now)
	if _, err := rig.DB().ExecContext(ctx,
		`INSERT INTO market_history (region_id, type_id, date, average, highest, lowest, volume, order_count)
		 VALUES (10000002, 587, '2026-10-01', 100.0, 110.0, 90.0, 10, 2),
		        (10000002, 587, '2026-10-02', 120.0, 130.0, 110.0, 12, 3)`); err != nil {
		t.Fatalf("seed history: %v", err)
	}
	if err := q.UpsertMarketRegionStat(ctx, db.UpsertMarketRegionStatParams{
		RegionID: 10000002, TypeID: 587, BestSell: 120, BestBuy: 100, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed stats: %v", err)
	}
	if err := q.UpsertMarketRegionStat(ctx, db.UpsertMarketRegionStatParams{
		RegionID: 10000043, TypeID: 587, BestSell: 125, BestBuy: 99, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed stats: %v", err)
	}

	// The standalone public item page is retired outright:
	// nothing links to it, the sitemap and robots.txt point
	// at the Item Database instead, and the URL itself is
	// gone. The type page is public now and carries the hub
	// prices and price history itself.
	if code, _ := apptest.GetPage(t, rig, nil, "/item/587/"); code != http.StatusNotFound {
		t.Errorf("old item URL status = %d, want 404", code)
	}
	code, body := apptest.GetPage(t, rig, nil, "/items/type/587/")
	if code != 200 {
		t.Fatalf("item page status = %d", code)
	}
	apptest.MustContain(t, "/items/type/587/", body,
		"Rifter", "Frigate",
		"The Rifter is a frigate.",
		"Hub prices",
		"The Forge", "Domain",
		`<svg class="spark"`,
	)
	if code, _ := apptest.GetPage(t, rig, nil, "/items/type/999999/"); code != http.StatusSeeOther {
		t.Errorf("unknown item status = %d, want 303 to the browser", code)
	}

	// A type that is not traded on the market gets no market
	// sections, even when swept stats exist for it.
	if _, err := rig.DB().ExecContext(ctx,
		`INSERT INTO sde_types (type_id, name, group_id, market_group_id, published, description) VALUES (4242, 'Unpublished Relic', 99, 0, 1, 'Not sold anywhere.')`); err != nil {
		t.Fatalf("seed non-market type: %v", err)
	}
	if err := q.UpsertMarketRegionStat(ctx, db.UpsertMarketRegionStatParams{
		RegionID: 10000002, TypeID: 4242, BestSell: 55, BestBuy: 44, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed non-market stats: %v", err)
	}
	code, body = apptest.GetPage(t, rig, nil, "/items/type/4242/")
	if code != 200 {
		t.Fatalf("non-market item status = %d", code)
	}
	apptest.MustContain(t, "/items/type/4242/", body, "Unpublished Relic", "Not traded on the market")
	if strings.Contains(body, "Hub prices") {
		t.Errorf("non-market item page shows hub prices")
	}

	code, body = apptest.GetPage(t, rig, nil, "/ships/587/")
	if code != 200 {
		t.Fatalf("ship status = %d", code)
	}
	apptest.MustContain(t, "/ships/587/", body,
		"Rifter", "Solo Rifter",
		"/fittings/public/fit/", "og:title",
	)
	// Modules are not ships.
	if code, _ := apptest.GetPage(t, rig, nil, "/ships/1238/"); code != 404 {
		t.Errorf("module ship status = %d, want 404", code)
	}
	if code, _ := apptest.GetPage(t, rig, nil, "/ships/424242/"); code != 404 {
		t.Errorf("unknown ship status = %d, want 404", code)
	}
	if transport.Calls.Load() != 0 {
		t.Errorf("item/ship pages made %d outbound calls", transport.Calls.Load())
	}
}

func TestPublicBuildKeepsAuth(t *testing.T) {
	transport := &apptest.CountingTransport{}
	rig := apptest.Build(t, transport)

	for _, path := range []string{"/fittings/saved/", "/fittings/mine.json"} {
		if code, _ := apptest.GetPage(t, rig, nil, path); code != http.StatusSeeOther {
			t.Errorf("GET %s logged out = %d, want 303", path, code)
		}
	}
	if code, _ := postPublicForm(t, rig, nil, "/fittings/save/", url.Values{"fit": {`{}`}}); code != http.StatusSeeOther {
		t.Errorf("POST /fittings/save/ logged out = %d, want 303", code)
	}
}
