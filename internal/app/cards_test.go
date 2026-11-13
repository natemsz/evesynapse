package app

import (
	"bytes"
	"context"
	"database/sql"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"evesynapse/internal/card"
	db "evesynapse/internal/db/sqlc"
)

// TestSummaryCards: the pilot and kill pages point og:image at their
// card routes; a route draws a 1200x630 PNG from stored data once and
// serves it from cache until the data version moves; a pilot with no
// activity record gets a card without the stat row; an entity that
// does not resolve gets the global fallback card.
func TestSummaryCards(t *testing.T) {
	app, conn, _ := buildCorpTestApp(t, nil)
	ctx := context.Background()
	exec := func(s string, args ...any) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, s, args...); err != nil {
			t.Fatal(err)
		}
	}
	var pic bytes.Buffer
	src := image.NewRGBA(image.Rect(0, 0, 64, 64))
	src.Set(1, 1, color.RGBA{200, 100, 50, 255})
	_ = png.Encode(&pic, src)
	app.cardFetch = func(context.Context, string) ([]byte, error) { return pic.Bytes(), nil }

	fetch := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}
	visitor := &http.Cookie{Name: "nobody", Value: "x"}
	draws := func() int64 { return app.cardsState().draws.Load() }

	const pilot = int64(93300001)
	now := time.Now().UTC()
	if err := app.setPilotRecord(ctx, db.SetPilotRecordParams{CharacterID: pilot, State: pilotStateReady,
		Payload:   `{"profile":{"name":"Card Pilot","corporation_id":98000001},"corp":{"name":"Card Corp","ticker":"CARD"}}`,
		FetchedAt: sql.NullTime{Time: now, Valid: true}}); err != nil {
		t.Fatal(err)
	}
	_, body := getPage(t, app, visitor, "/pilot/?character=93300001")
	mustContain(t, "/pilot/", body, `<meta property="og:image" content="https://evesynapse.app/pilot/card.png?character=93300001">`)

	// No activity record: name and portrait, no stat row.
	if job, ok := app.pilotCardJob(ctx, pilot); !ok || job.card.Title != "Card Pilot" || len(job.card.Stats) != 0 {
		t.Fatalf("pilot without activity: ok=%v card=%+v, want a name card with no stats", ok, job.card)
	}
	rec := fetch("/pilot/card.png?character=93300001")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("pilot card: status %d type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(rec.Body.Bytes()))
	if err != nil || cfg.Width != card.W || cfg.Height != card.H || card.W != 1200 || card.H != 630 {
		t.Fatalf("pilot card: %dx%d err=%v, want 1200x630", cfg.Width, cfg.Height, err)
	}
	if fetch("/pilot/card.png?character=93300001"); draws() != 1 {
		t.Fatalf("a repeat request drew again: %d draws", draws())
	}

	// The activity record adds the stat row, and moving its
	// updated_at redraws the card.
	exec(`INSERT INTO pilot_activity_profiles (character_id, backfilled_at, updated_at, kills, losses) VALUES ($1, $2, $2, 2431, 318)`, pilot, now)
	if job, _ := app.pilotCardJob(ctx, pilot); len(job.card.Stats) < 2 || job.card.Stats[0].Value[0].Text != "2,431" {
		t.Fatalf("pilot with activity: stats %+v, want kills 2,431 first", job.card.Stats)
	}
	fetch("/pilot/card.png?character=93300001")
	fetch("/pilot/card.png?character=93300001")
	if draws() != 2 {
		t.Fatalf("after the profile appeared: %d draws, want 2", draws())
	}
	exec(`UPDATE pilot_activity_profiles SET updated_at = $2 WHERE character_id = $1`, pilot, now.Add(time.Minute))
	if fetch("/pilot/card.png?character=93300001"); draws() != 3 {
		t.Fatalf("a bumped updated_at did not redraw: %d draws", draws())
	}

	// A public kill: the page names its card and the card draws.
	exec(`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30002537, 'Amamake', 10000042, 0.4) ON CONFLICT DO NOTHING`)
	exec(`INSERT INTO sde_types (type_id, name) VALUES (22456, 'Sabre') ON CONFLICT DO NOTHING`)
	exec(`INSERT INTO kills (killmail_id, killmail_time, system_id, victim_character_id, fb_character_id, ship_type_id, source, public, ingested_at, value_ours)
		VALUES (77, $1, 30002537, $2, 93300002, 22456, 'stream', true, now(), 82400000)`, now, pilot)
	if job, ok := app.killCardJob(ctx, 77); !ok || job.card.Detail == nil || job.card.Stats[0].Value[0].Text != "82.40M" {
		t.Fatalf("kill card: ok=%v card=%+v, want the stored value and a final blow line", ok, job.card)
	}
	_, body = getPage(t, app, visitor, "/intel/kill/77/")
	mustContain(t, "/intel/kill/77/", body, `<meta property="og:image" content="https://evesynapse.app/intel/kill/77/card.png">`)
	if rec := fetch("/intel/kill/77/card.png"); rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("kill card: status %d type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if fetch("/intel/kill/77/card.png"); draws() != 4 {
		t.Fatalf("a repeat kill card request drew again: %d draws, want 4", draws())
	}

	// Unknown entities get the fallback card's bytes.
	for _, path := range []string{"/pilot/card.png?character=93399999", "/intel/kill/999999/card.png"} {
		rec := fetch(path)
		if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), fallbackCard()) || len(fallbackCard()) == 0 {
			t.Errorf("%s: status %d, %d bytes, want the fallback card", path, rec.Code, rec.Body.Len())
		}
	}
}

// TestPlaceSummaryCards: the system, region and map pages point
// og:image at their cards (the map's carrying the link's focus and
// layer); the region card splits its systems by the display bands;
// an unknown system gets the fallback card.
func TestPlaceSummaryCards(t *testing.T) {
	app, conn, _ := buildCorpTestApp(t, nil)
	ctx := context.Background()
	for _, s := range []string{
		`INSERT INTO sde_regions (region_id, name) VALUES (10000042, 'Heimatar') ON CONFLICT DO NOTHING`,
		`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES
			(30002537, 'Amamake', 10000042, 0.4), (30002510, 'Rens', 10000042, 0.9),
			(30002538, 'Vard', 10000042, 0.03), (30002539, 'Null One', 10000042, -0.2) ON CONFLICT DO NOTHING`,
	} {
		if _, err := conn.ExecContext(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	visitor := &http.Cookie{Name: "nobody", Value: "x"}
	for page, want := range map[string]string{
		"/system/?system=30002537":                 "https://evesynapse.app/system/card.png?system=30002537",
		"/region/?region=10000042":                 "https://evesynapse.app/region/card.png?region=10000042",
		"/map/?region=10000042&layer=sov&view=1,2": "https://evesynapse.app/map/card.png?layer=sov&amp;region=10000042",
	} {
		_, body := getPage(t, app, visitor, page)
		mustContain(t, page, body, `<meta property="og:image" content="`+want+`">`)
	}
	c, ok := app.regionCard(ctx, 10000042)
	if !ok || len(c.Bars) != 3 || c.Bars[0].Count != "1" || c.Bars[1].Count != "2" || c.Bars[2].Count != "1" {
		t.Fatalf("region card bars: ok=%v %+v, want 1 high, 2 low, 1 null", ok, c.Bars)
	}
	for _, path := range []string{"/system/card.png?system=30002537", "/region/card.png?region=10000042", "/map/card.png?layer=sov"} {
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/png" {
			t.Errorf("%s: status %d type %q, want a PNG", path, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/system/card.png?system=1", nil))
	if !bytes.Equal(rec.Body.Bytes(), fallbackCard()) {
		t.Errorf("an unknown system did not get the fallback card")
	}
}

// TestCardBrandMatchesWordmark: the card package draws the brand from
// copies of the template's outlines; every one must still be in
// templates/wordmark.html, so the cards never drift from the site.
func TestCardBrandMatchesWordmark(t *testing.T) {
	raw, err := templatesFS.ReadFile("templates/wordmark.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range append(append([]string{}, card.WordmarkPaths...), card.MarkPaths...) {
		if !strings.Contains(string(raw), `d="`+d+`"`) {
			t.Errorf("card outline not in wordmark.html: %s", d)
		}
	}
}
