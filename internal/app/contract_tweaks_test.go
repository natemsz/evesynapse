package app

// The contract search tweaks: the matched item pinned to
// the head of a row's drawer, the sortable column headers
// (and the sort select they replaced), the exclusion and
// security-floor filters, and the subtab sweep telemetry.
// All seeded against stored rows, the way the pages read.

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"evesynapse/internal/esi"
)

// seedContractTweaks stores a corpus aimed at the tweaks:
// contracts at an NPC station, a public structure and a
// private one; single- and multi-type offers; a courier
// carrying two item rows; systems at 0.9, 0.5, a
// rounds-to-0.4 security and -0.2; and one 144-line
// contract whose Epithal sits deep in the list.
func seedContractTweaks(t *testing.T, app *Application, conn *sql.DB) {
	t.Helper()
	app.esi.PrimeName(esi.NameCharacter, 9001, "Issuer One")
	app.esi.PrimeName(esi.NameCorporation, 9801, "Issuer Corp")
	ctx := context.Background()
	now := time.Now().UTC()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("tweaks seed %q: %v", query, err)
		}
	}
	exec(`INSERT INTO sde_regions (region_id, name) VALUES (10000002, 'The Forge')`)
	exec(`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES
		(30000142, 'Jita', 10000002, 0.9459),
		(30000201, 'Halfsec', 10000002, 0.5),
		(30000202, 'Lowish', 10000002, 0.4499),
		(30000203, 'Nullsec', 10000002, -0.2)`)
	exec(`INSERT INTO sde_stations (station_id, name, system_id) VALUES
		(60003760, 'Jita IV - Moon 4 - Caldari Navy Assembly Plant', 30000142),
		(60000201, 'Halfsec - Test Station', 30000201),
		(60000202, 'Lowish - Test Station', 30000202),
		(60000203, 'Nullsec - Test Station', 30000203)`)
	exec(`INSERT INTO sde_types (type_id, name) VALUES (34, 'Tritanium'), (35, 'Pyerite'), (9998, 'Epithal')`)
	exec(`INSERT INTO entity_names (kind, id, name) VALUES ('character', 9001, 'Issuer One'), ('corporation', 9801, 'Issuer Corp')`)
	// One public structure (mirrored in public_structures,
	// as the worker's refresh leaves it) and one private.
	exec(`INSERT INTO structure_context (structure_id, owner_corporation_id, system_id, type_id, updated_at) VALUES
		(1000000000001, 9801, 30000201, 35832, $1),
		(1000000000002, 9801, 30000142, 35832, $1)`, now)
	exec(`INSERT INTO structure_names (structure_id, name, state, resolved_at) VALUES
		(1000000000001, 'Halfsec - Public Keepstar', 'resolved', $1),
		(1000000000002, 'Jita - Private Dock', 'resolved', $1)`, now)
	exec(`INSERT INTO public_structures (structure_id, refreshed_at) VALUES (1000000000001, $1)`, now)

	contract := func(id int64, typ, title string, price, reward float64, startLoc int64) {
		t.Helper()
		exec(`INSERT INTO pub_contracts (contract_id, region_id, type, issuer_id, issuer_corporation_id,
			title, price, buyout, reward, collateral, volume, start_location_id, end_location_id,
			date_issued, date_expired, days_to_complete, items_state, last_cycle, first_seen, last_seen)
			VALUES ($1, 10000002, $2, 9001, 9801, $3, $4, 0, $5, 0, 10, $6, 0, $7, $8, 1, 'stored', 1, $7, $7)`,
			id, typ, title, price, reward, startLoc, now.Add(-time.Hour), now.Add(7*24*time.Hour))
	}
	item := func(rec, contractID, typeID, qty int64) {
		t.Helper()
		exec(`INSERT INTO pub_contract_items (record_id, contract_id, type_id, quantity, is_included)
			VALUES ($1, $2, $3, $4, true)`, rec, contractID, typeID, qty)
	}

	contract(9001, "item_exchange", "NPC Single", 100000, 0, 60003760)
	item(1, 9001, 34, 10)

	contract(9002, "item_exchange", "Public Structure Single", 900000, 0, 1000000000001)
	item(2, 9002, 34, 10)

	contract(9003, "item_exchange", "Private Structure Single", 300000, 0, 1000000000002)
	item(3, 9003, 34, 10)

	// Two distinct types, Pyerite first: a Tritanium
	// search must visibly reorder this drawer's lines.
	contract(9004, "item_exchange", "NPC Multi", 200000, 0, 60003760)
	item(4, 9004, 35, 5)
	item(5, 9004, 34, 7)

	// A courier carrying two item rows: multi-item
	// exclusion must never touch couriers.
	contract(9005, "courier", "Courier Two Crates", 0, 5000000, 60003760)
	item(6, 9005, 34, 3)
	item(7, 9005, 35, 3)

	contract(9006, "item_exchange", "Half Sec Haul", 150000, 0, 60000201)
	item(8, 9006, 34, 10)

	contract(9007, "item_exchange", "Lowish Haul", 150000, 0, 60000202)
	item(9, 9007, 34, 10)

	contract(9008, "item_exchange", "Null Haul", 150000, 0, 60000203)
	item(10, 9008, 34, 10)

	// The 144-line haul: the Epithal rides at line 101.
	contract(9009, "item_exchange", "Big Haul", 500000, 0, 60003760)
	for i := int64(0); i < 144; i++ {
		typeID := int64(34)
		if i == 100 {
			typeID = 9998
		}
		item(1000+i, 9009, typeID, 1)
	}
}

func TestContractPanelPinsMatchedItem(t *testing.T) {
	tr := &openWindowTransport{}
	app, conn, _ := buildCorpTestApp(t, tr)
	seedContractTweaks(t, app, conn)

	// The search that matched the Epithal deep inside the
	// 144-line contract pins it to the drawer's head,
	// highlighted, exactly one line marked.
	code, body := getAnon(t, app, "/contracts/search/9009/panel?q_item=Epithal")
	if code != 200 {
		t.Fatalf("pinned panel: status %d, want 200", code)
	}
	if n := strings.Count(body, `class="cs-aline matched"`); n != 1 {
		t.Errorf("matched lines: got %d, want 1", n)
	}
	epi := strings.Index(body, "Epithal")
	tri := strings.Index(body, "Tritanium")
	if epi < 0 || tri < 0 || epi > tri {
		t.Error("the matched Epithal does not lead the drawer")
	}

	// The exact-type deep link pins the same way.
	_, body = getAnon(t, app, "/contracts/search/9009/panel?item_type=9998")
	if n := strings.Count(body, `class="cs-aline matched"`); n != 1 {
		t.Errorf("type-pinned lines: got %d, want 1", n)
	}

	// No item context: the drawer keeps its stored order
	// and nothing is marked.
	_, body = getAnon(t, app, "/contracts/search/9009/panel")
	if strings.Contains(body, "matched") {
		t.Error("an unpinned drawer carries a matched line")
	}
	if epi, tri := strings.Index(body, "Epithal"), strings.Index(body, "Tritanium"); epi < 0 || tri < 0 || epi < tri {
		t.Error("the unpinned drawer reordered its lines")
	}

	// A two-line drawer shows the reorder directly: stored
	// order is Pyerite, Tritanium; the Tritanium search
	// leads with Tritanium.
	_, body = getAnon(t, app, "/contracts/search/9004/panel?q_item=Tritanium")
	mi := strings.Index(body, `class="cs-aline matched"`)
	ti := strings.Index(body, "Tritanium")
	pi := strings.Index(body, "Pyerite")
	if mi < 0 || ti < 0 || pi < 0 || ti > pi {
		t.Error("the matched line does not lead the two-line drawer")
	}
	if mi > ti {
		t.Error("the leading line is not the marked one")
	}
}

func TestContractSearchSortHeaders(t *testing.T) {
	tr := &openWindowTransport{}
	app, conn, _ := buildCorpTestApp(t, tr)
	seedContractTweaks(t, app, conn)

	_, body := getAnon(t, app, "/contracts/search/")
	// The sort select is gone; the form still carries the
	// sort as a hidden field so filter submits keep a
	// header-chosen sort.
	if strings.Contains(body, "Sort results by") || strings.Contains(body, `<select name="sort">`) {
		t.Error("the sort dropdown still renders")
	}
	mustContain(t, "hidden sort", body, `<input type="hidden" name="sort" value="newest">`)
	// Idle headers point at their first sort, chevrons at
	// rest, no live state.
	mustContain(t, "idle headers", body,
		`<a class="cs-sorthead" href="/contracts/search/?sort=price_desc"`,
		`<a class="cs-sorthead" href="/contracts/search/?sort=margin_desc"`,
		`<span class="cs-sortchev" aria-hidden="true">↕</span>`)

	// Price descending: the price header flips to
	// ascending and shows the down arrow, live.
	_, body = getAnon(t, app, "/contracts/search/?sort=price_desc")
	mustContain(t, "price desc header", body,
		`<a class="cs-sorthead on" href="/contracts/search/?sort=price_asc"`,
		`<span class="cs-sortchev" aria-hidden="true">↓</span>`)
	// The two priced singles order by the raw figure.
	hi := strings.Index(body, "Public Structure Single")
	lo := strings.Index(body, "NPC Single")
	if hi < 0 || lo < 0 || hi > lo {
		t.Error("price_desc should rank the 900k contract above the 100k one")
	}

	// Price ascending flips both the arrow and the order.
	_, body = getAnon(t, app, "/contracts/search/?sort=price_asc")
	mustContain(t, "price asc header", body,
		`<a class="cs-sorthead on" href="/contracts/search/?sort=price_desc"`,
		`<span class="cs-sortchev" aria-hidden="true">↑</span>`)
	if hi, lo := strings.Index(body, "Public Structure Single"), strings.Index(body, "NPC Single"); hi < lo {
		t.Error("price_asc should rank the 100k contract above the 900k one")
	}

	// The value header alternates metric: ISK spread live
	// means its next click is the discount percent.
	_, body = getAnon(t, app, "/contracts/search/?sort=margin_desc")
	mustContain(t, "margin header", body,
		`<a class="cs-sorthead on" href="/contracts/search/?sort=deal_asc"`)
	_, body = getAnon(t, app, "/contracts/search/?sort=deal_asc")
	mustContain(t, "deal header", body,
		`<a class="cs-sorthead on" href="/contracts/search/?sort=margin_desc"`)
	// And a header link keeps the rest of the search:
	// the active filters ride along. (Probed with the
	// multi-item filter rather than deals: no seeded
	// contract is appraised, so a deals page has no rows
	// and renders no table to hold the headers.)
	_, body = getAnon(t, app, "/contracts/search/?exclude_multi=1&sort=margin_desc")
	if !strings.Contains(body, `href="/contracts/search/?exclude_multi=1&amp;sort=deal_asc"`) {
		t.Error("the value header dropped the active multi-item filter")
	}
}

func TestContractSearchLocationFilters(t *testing.T) {
	tr := &openWindowTransport{}
	app, conn, _ := buildCorpTestApp(t, tr)
	seedContractTweaks(t, app, conn)

	all := []string{"NPC Single", "Public Structure Single", "Private Structure Single",
		"NPC Multi", "Courier Two Crates", "Half Sec Haul", "Lowish Haul", "Null Haul", "Big Haul"}

	// The panel carries the three new controls.
	_, body := getAnon(t, app, "/contracts/search/")
	mustContain(t, "filter panel", body,
		`name="exclude_undockable"`, "Exclude Un-Dockable Citadels",
		`name="exclude_multi"`, "Exclude Multi-Item Contracts",
		`name="sec_min"`, `class="cs-secslider"`, "Minimum sec status",
		`<span class="cs-secval" data-sec-val>-1.0</span>`,
		"<strong>9</strong> contracts match")
	mustContain(t, "unfiltered", body, all...)

	// Un-dockable: only the private-structure contract
	// leaves; the NPC station and the public structure
	// stay, and the count tells the truth.
	_, body = getAnon(t, app, "/contracts/search/?exclude_undockable=1")
	if strings.Contains(body, "Private Structure Single") {
		t.Error("a non-public structure's contract survived the un-dockable filter")
	}
	mustContain(t, "dockable kept", body, "NPC Single", "Public Structure Single",
		"<strong>8</strong> contracts match", "Exclude: un-dockable citadels")

	// Multi-item: the two-type offer and the 144-liner
	// leave; the courier with two item rows stays.
	_, body = getAnon(t, app, "/contracts/search/?exclude_multi=1")
	for _, gone := range []string{"NPC Multi", "Big Haul"} {
		if strings.Contains(body, gone) {
			t.Errorf("multi-item contract %q survived the multi-item filter", gone)
		}
	}
	mustContain(t, "multi kept", body, "NPC Single", "Courier Two Crates",
		"<strong>7</strong> contracts match", "Exclude: multi-item contracts")

	// The security floor: 0.5 keeps the 0.5 system (and
	// the public structure standing in it) and drops the
	// rounds-to-0.4 system and the null one.
	_, body = getAnon(t, app, "/contracts/search/?sec_min=0.5")
	mustContain(t, "sec 0.5 kept", body, "Half Sec Haul", "Public Structure Single",
		"<strong>7</strong> contracts match", "Sec ≥ 0.5")
	for _, gone := range []string{"Lowish Haul", "Null Haul"} {
		if strings.Contains(body, gone) {
			t.Errorf("below-floor contract %q survived sec_min=0.5", gone)
		}
	}
	// 0.4499 rounds to the tier the site displays, 0.4 --
	// a 0.4 floor admits it.
	_, body = getAnon(t, app, "/contracts/search/?sec_min=0.4")
	mustContain(t, "sec 0.4 kept", body, "Lowish Haul", "<strong>8</strong> contracts match")
	if strings.Contains(body, "Null Haul") {
		t.Error("the -0.2 system survived a 0.4 floor")
	}
	// A floor under the null system's own status admits
	// it; the bottomed-out default admits everything.
	_, body = getAnon(t, app, "/contracts/search/?sec_min=-0.5")
	mustContain(t, "sec -0.5", body, "Null Haul", "<strong>9</strong> contracts match")
	_, body = getAnon(t, app, "/contracts/search/?sec_min=0.0")
	if strings.Contains(body, "Null Haul") {
		t.Error("the -0.2 system survived a 0.0 floor")
	}
}

func TestContractSearchSweepTelemetry(t *testing.T) {
	tr := &openWindowTransport{}
	app, conn, _ := buildCorpTestApp(t, tr)
	seedContractTweaks(t, app, conn)
	ctx := context.Background()
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO pub_contract_sweep_state (region_id, next_page, total_pages, cycle, updated_at)
		 VALUES (10000002, 0, 3, 7, $1)`, time.Now().Add(-4*time.Minute-30*time.Second).UTC()); err != nil {
		t.Fatalf("seed sweep state: %v", err)
	}

	// The subtab row carries the readout: the freshest
	// sweep stamp in the house age words and the stored
	// corpus size.
	_, body := getAnon(t, app, "/contracts/search/")
	mustContain(t, "telemetry", body,
		`class="subtab-note"`,
		"ESI Sweep: 4 minutes ago • 9 Public Contracts Indexed")

	// No other page grows the note: the detail page's
	// subtab row stays bare.
	_, body = getAnon(t, app, "/contracts/search/9001/")
	if strings.Contains(body, "subtab-note") {
		t.Error("the sweep note leaked onto the detail page")
	}
}
