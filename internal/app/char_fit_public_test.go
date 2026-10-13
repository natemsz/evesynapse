package app

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
)

// publicFitURL pins the browser addresses the filter bar builds:
// defaults omitted (sort=rank, faction=any), empty values dropped,
// page resets unless kept.
func TestPublicFitURL(t *testing.T) {
	blank := publicFitFilters{Sort: "rank", Faction: "any", Page: 1}
	if got := blank.publicFitURL(map[string]string{}, false); got != publicFitsPath {
		t.Errorf("blank filters = %q, want %q", got, publicFitsPath)
	}

	full := publicFitFilters{
		Q: "rift", Role: "Logistics", Hull: "Frigate", HullQ: "rif",
		IncludeName: "Medium Shield Extender I", ExcludeName: "Warp Disruptor",
		Faction: "none", Tag: "pvp", Sort: "new",
		MinStars: 4.5, MaxPrice: 1000000, Alpha: true, Page: 3,
	}
	kept := publicFitsPath + "?alpha=1&exclude=Warp+Disruptor&faction=none&hull=Frigate" +
		"&hullq=rif&include=Medium+Shield+Extender+I&maxprice=1000000&page=3" +
		"&q=rift&role=Logistics&sort=new&stars=4.5&tag=pvp"
	if got := full.publicFitURL(map[string]string{}, true); got != kept {
		t.Errorf("full filters keepPage:\n got %q\nwant %q", got, kept)
	}

	// Page resets unless kept or explicitly changed.
	reset := publicFitsPath + "?alpha=1&exclude=Warp+Disruptor&faction=none&hull=Frigate" +
		"&hullq=rif&include=Medium+Shield+Extender+I&maxprice=1000000" +
		"&q=rift&role=Logistics&sort=new&stars=4.5&tag=pvp"
	if got := full.publicFitURL(map[string]string{}, false); got != reset {
		t.Errorf("page reset:\n got %q\nwant %q", got, reset)
	}
	if got := full.publicFitURL(map[string]string{"page": "2"}, true); !strings.Contains(got, "page=2") {
		t.Errorf("page change = %q, want page=2", got)
	}

	// Changes override; empty changes drop the choice and
	// defaults stay omitted even when named.
	changed := full.publicFitURL(map[string]string{
		"sort": "rank", "faction": "any", "q": "", "tag": "solo",
	}, false)
	for _, dropped := range []string{"sort=", "faction=", "q=rift", "tag=pvp"} {
		if strings.Contains(changed, dropped) {
			t.Errorf("changed URL %q keeps %q", changed, dropped)
		}
	}
	if !strings.Contains(changed, "tag=solo") {
		t.Errorf("changed URL %q misses tag=solo", changed)
	}
}

// TestPublicFitPageMirrorsTool: the public fit page renders the
// fitting tool's own layout read-only — ship centerpiece, slot
// groups, headline stats at All-V — with the page's extras (EFT,
// share links) intact and none of the editor's controls.
func TestPublicFitPageMirrorsTool(t *testing.T) {
	transport := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
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
	row, err := q.CreateLocalFitting(ctx, db.CreateLocalFittingParams{
		UserID: user.ID, Name: "Public Fixture", ShipTypeID: 1001, IsPublic: true,
		CreatedAt: now, UpdatedAt: now,
		ItemsJson: `{"name":"Public Fixture","shipTypeId":1001,` +
			`"items":[{"typeId":3001,"qty":1},{"typeId":3002,"qty":1}],"charges":{"3001":4001}}`,
	})
	if err != nil {
		t.Fatalf("create fitting: %v", err)
	}
	t.Cleanup(func() {
		if transport.calls.Load() != 0 {
			t.Errorf("public fit page made %d outbound calls, want 0", transport.calls.Load())
		}
	})

	code, body := getSwapPage(t, app, nil, publicFitsPath+"fit/"+strconv.FormatInt(row.ID, 10)+"/")
	if code != http.StatusOK {
		t.Fatalf("public fit page (anonymous): status %d, want 200", code)
	}
	mustContain(t, "public fit page (anonymous)", body,
		"Public Fixture",
		// Ship centerpiece, slot groups, headline stats: the
		// tool's structure and classes.
		`class="fit-visual"`,
		`types/1001/render?size=512`,
		"High slots",
		"Fixture Blaster",
		`class="fit-headline-num"`,
		`>DPS</span>`,
		`class="fit-table"`,
		"Effective hit points",
		"with All V's skills",
		// The page's own extras stay.
		`id="fit-eft"`,
		"Copy EFT",
		"Share to X",
	)
	for _, editable := range []string{
		"data-fit-dec", "data-fit-inc", "data-fit-add", "data-fit-qty",
		"data-fit-charge", "fit-slot-empty", `id="fit-mod-search"`,
		`id="fit-pilot"`, `id="fit-save"`, `id="fit-yourfits-search"`,
	} {
		if strings.Contains(body, editable) {
			t.Errorf("public fit page exposes editor control %q", editable)
		}
	}
}

// TestPublicFitPageTooltips: the public page's modules carry
// the tooltip's computed rows — the same figures the fitting
// tool shows — and none of the editor's state plumbing:
// viewing a fit is information-only.
func TestPublicFitPageTooltips(t *testing.T) {
	transport := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
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
	row, err := q.CreateLocalFitting(ctx, db.CreateLocalFittingParams{
		UserID: user.ID, Name: "Tooltip Fixture", ShipTypeID: 1001, IsPublic: true,
		CreatedAt: now, UpdatedAt: now,
		ItemsJson: `{"name":"Tooltip Fixture","shipTypeId":1001,` +
			`"items":[{"typeId":3001,"qty":1},{"typeId":3002,"qty":1}],"charges":{"3001":4001}}`,
	})
	if err != nil {
		t.Fatalf("create fitting: %v", err)
	}
	code, body := getSwapPage(t, app, nil, publicFitsPath+"fit/"+strconv.FormatInt(row.ID, 10)+"/")
	if code != http.StatusOK {
		t.Fatalf("public fit page (anonymous): status %d, want 200", code)
	}
	// The blaster's rows, by hand: charge damage 12+8 = 20,
	// multiplier 5, cycle 5 s -> volley 100, DPS 20; CPU
	// 15 tf, PG 10 MW.
	mustContain(t, "public fit tooltips", body,
		`data-tip-lines="DPS|20|;Volley|100|;CPU|15 tf|;PG|10 MW|"`)
	for _, editorOnly := range []string{"data-tip-states", "data-tip-key"} {
		if strings.Contains(body, editorOnly) {
			t.Errorf("public fit page carries editor-only %q", editorOnly)
		}
	}
}
