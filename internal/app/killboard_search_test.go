package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// TestKillboardSearchResolvesNames: the killboard's merged name
// lookup answers the suggestion feed with the right scope and id
// for a pilot, a corporation, an alliance, a system, a region and
// a ship from seeded records and SDE, and the search form's path
// redirects a known name to its scoped feed and names an unknown
// one on the unscoped board.
func TestKillboardSearchResolvesNames(t *testing.T) {
	app, conn, q := buildCorpTestApp(t, nil)
	ctx := context.Background()
	fetched := mustNullTime("2026-10-01T00:00:00Z")

	for _, stmt := range []string{
		`INSERT INTO sde_regions (region_id, name) VALUES (10000002, 'The Forge')`,
		`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30000142, 'Jita', 10000002, 0.946)`,
		`INSERT INTO sde_categories (category_id, name) VALUES (6, 'Ship')`,
		`INSERT INTO sde_groups (group_id, name, category_id) VALUES (25, 'Frigate', 6)`,
		`INSERT INTO sde_types (type_id, name, group_id, market_group_id, published) VALUES (587, 'Rifter', 25, 0, 1)`,
	} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed SDE: %v", err)
		}
	}
	pilot, err := json.Marshal(pilotPayload{
		Profile: esi.Character{Name: "Kbsearch Pilot"},
		Corp:    esi.Corporation{Name: "Pilot Home Corp"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.SetPilotRecord(ctx, db.SetPilotRecordParams{
		CharacterID: 96000001, Payload: string(pilot), State: pilotStateReady, FetchedAt: fetched,
	}); err != nil {
		t.Fatalf("seed pilot record: %v", err)
	}
	corp, err := json.Marshal(corporationRecordPayload{Corp: esi.Corporation{Name: "Kbsearch Industries", Ticker: "KBSI"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.SetCorporationRecord(ctx, db.SetCorporationRecordParams{
		CorporationID: 98000101, Payload: string(corp), State: orgStateReady, FetchedAt: fetched,
	}); err != nil {
		t.Fatalf("seed corporation record: %v", err)
	}
	alliance, err := json.Marshal(allianceRecordPayload{Alliance: esi.Alliance{Name: "Kbsearch Alliance", Ticker: "KBSA"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.SetAllianceRecord(ctx, db.SetAllianceRecordParams{
		AllianceID: 99000101, Payload: string(alliance), State: orgStateReady, FetchedAt: fetched,
	}); err != nil {
		t.Fatalf("seed alliance record: %v", err)
	}

	get := func(path string) (int, http.Header, string) {
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Code, rec.Header(), rec.Body.String()
	}
	suggest := func(query string) []kbSearchItem {
		code, _, body := get("/intel/kills/suggest?q=" + query)
		if code != http.StatusOK {
			t.Fatalf("suggest %q: status %d", query, code)
		}
		var items []kbSearchItem
		if err := json.Unmarshal([]byte(body), &items); err != nil {
			t.Fatalf("suggest %q: decode: %v (%q)", query, err, body)
		}
		return items
	}
	find := func(items []kbSearchItem, kind string, id int64) (kbSearchItem, bool) {
		for _, it := range items {
			if it.Kind == kind && it.ID == id {
				return it, true
			}
		}
		return kbSearchItem{}, false
	}

	if it, ok := find(suggest("kbsearch%20pilot"), "character", 96000001); !ok || it.Sub != "Pilot Home Corp" {
		t.Errorf("pilot suggestion = %+v, %v; want character 96000001 with its corporation as the sub", it, ok)
	}
	if it, ok := find(suggest("kbsearch%20industries"), "corporation", 98000101); !ok || it.Sub != "[KBSI]" {
		t.Errorf("corporation suggestion = %+v, %v; want corporation 98000101 with its ticker as the sub", it, ok)
	}
	if _, ok := find(suggest("kbsearch%20alliance"), "alliance", 99000101); !ok {
		t.Errorf("alliance suggestion missing; want alliance 99000101")
	}
	if it, ok := find(suggest("jita"), "system", 30000142); !ok || it.Sub != "The Forge" || it.Sec != "0.9" {
		t.Errorf("system suggestion = %+v, %v; want system 30000142 in The Forge at 0.9", it, ok)
	}
	if _, ok := find(suggest("the%20forge"), "region", 10000002); !ok {
		t.Errorf("region suggestion missing; want region 10000002")
	}
	if _, ok := find(suggest("rifter"), "ship", 587); !ok {
		t.Errorf("ship suggestion missing; want ship 587")
	}
	if items := suggest("j"); len(items) != 0 {
		t.Errorf("one-character query = %+v, want an empty list", items)
	}

	code, header, _ := get("/intel/kills/?q=jita")
	if code != http.StatusFound || header.Get("Location") != "/intel/kills/?system=30000142" {
		t.Errorf("form search for a system: %d to %q, want 302 to the system feed", code, header.Get("Location"))
	}
	code, header, _ = get("/intel/kills/?q=kbsearch%20pilot")
	if code != http.StatusFound || header.Get("Location") != "/intel/kills/?character=96000001" {
		t.Errorf("form search for a pilot: %d to %q, want 302 to the pilot feed", code, header.Get("Location"))
	}
	code, _, body := get("/intel/kills/?q=zzznomatch")
	if code != http.StatusOK || !strings.Contains(body, "No pilot, corporation, alliance, system, region or ship matches “zzznomatch”.") {
		t.Errorf("form search for an unknown name: %d, the miss is not named on the board", code)
	}
	if code, _, body := get("/intel/kills/"); code != http.StatusOK || !strings.Contains(body, `id="kb-q"`) {
		t.Errorf("the board renders without its search box: %d", code)
	}
}
