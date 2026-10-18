package app

// The abyssal surface on the contract pages (contracts
// module, search.go): the family / score / estimate filters,
// the search row's estimate with its basis, and the detail
// panel's percentile bars, spread, basis and cap note -- all
// read from the stored scoring columns (schema 057), never
// recomputed at render.

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

func seedAbyssalPage(t *testing.T, conn *sql.DB) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("seed %q: %v", query, err)
		}
	}
	exec(`INSERT INTO sde_regions (region_id, name) VALUES (10000002, 'The Forge')`)
	exec(`INSERT INTO sde_systems (system_id, name, region_id) VALUES (30000142, 'Jita', 10000002)`)
	exec(`INSERT INTO sde_stations (station_id, name, system_id) VALUES (60003760, 'Jita IV - Moon 4 - Caldari Navy Assembly Plant', 30000142)`)
	exec(`INSERT INTO sde_types (type_id, name) VALUES
		(60001, 'Abyssal Test Damage'), (30001, 'Test Damage II'), (47735, 'Test Mutaplasmid')`)
	exec(`INSERT INTO sde_attribute_types (attribute_id, name, stackable, high_is_good, unit_id, default_value) VALUES
		(64, 'damageMultiplier', 1, 1, 0, 0), (6, 'capacitorNeed', 1, 0, 0, 0), (50, 'cpu', 1, 0, 0, 0)`)
	exec(`INSERT INTO sde_type_attributes (type_id, attribute_id, value) VALUES
		(30001, 64, 2.0), (30001, 6, 10), (30001, 50, 20)`)
	exec(`INSERT INTO abyssal_family_weights (family, attribute_id, weight, penalty_only) VALUES
		('test_damage_thing', 64, 1.0, false), ('test_damage_thing', 6, 0.25, false),
		('test_damage_thing', 50, 0.5, true)`)

	exec(`INSERT INTO pub_contracts (contract_id, region_id, type, issuer_id, issuer_corporation_id,
		title, price, buyout, reward, collateral, volume, start_location_id, end_location_id,
		date_issued, date_expired, days_to_complete, items_state, last_cycle, first_seen, last_seen,
		offered_value, want_value, priced_lines, total_lines, tier_mix, priced_at)
		VALUES (8501, 10000002, 'item_exchange', 9001, 9801, 'Bricked But Pretty', 300000000, 0, 0, 0, 0,
		60003760, 0, $1, $2, 1, 'stored', 1, $1, $1, 205000000, 0, 1, 1, 'abyssal:1', $1)`,
		now.Add(-time.Hour), now.Add(7*24*time.Hour))
	exec(`INSERT INTO pub_contract_items (record_id, contract_id, type_id, item_id, quantity, is_included,
		unit_price, line_value, price_tier, price_asof)
		VALUES (1, 8501, 60001, 900001, 1, true, 205000000, 205000000, 'abyssal', $1)`, now)
	// A strong damage roll dragged down by a bricked CPU:
	// score 0.45, capped by the fitting roll, corpus-priced.
	exec(`INSERT INTO abyssal_items (item_id, type_id, source_type_id, mutator_type_id, created_by,
		attributes, fetched_at, first_seen_at, last_seen_at,
		family, score, percentiles, derived_stats, est_value, est_low, est_high,
		est_basis, est_scope, est_comps, premium_mult, cap_attribute_id, cap_kind, scored_at)
		VALUES (900001, 60001, 30001, 47735, 9001,
		'[{"attribute_id":64,"value":2.36},{"attribute_id":6,"value":10.2},{"attribute_id":50,"value":27.9}]',
		$1, $1, $1, 'test_damage_thing', 0.45, '{"64":0.8,"6":0.6,"50":0.02}', '[]',
		205000000, 180000000, 240000000, 'corpus', 'module', 8, 1, 50, 'fitting', $1)`, now)
}

func TestAbyssalSearchAndDetailPages(t *testing.T) {
	tr := &openWindowTransport{}
	app, conn, _ := buildCorpTestApp(t, tr)
	seedAbyssalPage(t, conn)

	// The row carries the estimate with spread and basis,
	// the cap in plain words, and the primary bar.
	_, body := getAnon(t, app, "/contracts/search/?family=test_damage_thing")
	mustContain(t, "family search", body,
		"Bricked But Pretty", "Mutated from", "Test Damage II", "Test Mutaplasmid",
		"Our estimate", "205,000,000", "from 8 listings of this module",
		"Held back by: CPU fitting cost", "Roll score 45 / 100", `data-w="80"`)

	// A family the item is not in excludes it.
	_, body = getAnon(t, app, "/contracts/search/?family=warp_scrambler")
	if got := body; strings.Contains(got, "Bricked But Pretty") {
		t.Error("family filter matched another family's item")
	}
	// Score band: 45 passes a 40 floor and fails a 90 floor.
	_, body = getAnon(t, app, "/contracts/search/?score_min=40")
	mustContain(t, "score floor 40", body, "Bricked But Pretty")
	_, body = getAnon(t, app, "/contracts/search/?score_min=90")
	if strings.Contains(body, "Bricked But Pretty") {
		t.Error("score floor 90 matched a 45-score item")
	}
	// Estimate band.
	_, body = getAnon(t, app, "/contracts/search/?est_min=100m&est_max=300m")
	mustContain(t, "estimate band", body, "Bricked But Pretty")
	_, body = getAnon(t, app, "/contracts/search/?est_min=1b")
	if strings.Contains(body, "Bricked But Pretty") {
		t.Error("estimate floor 1b matched a 205m estimate")
	}
	// Per-attribute percentile floor: the damage roll sits
	// at 80; the CPU roll at 2.
	_, body = getAnon(t, app, "/contracts/search/?attr=64&attr_min=70")
	mustContain(t, "attr floor pass", body, "Bricked But Pretty")
	_, body = getAnon(t, app, "/contracts/search/?attr=50&attr_min=70")
	if strings.Contains(body, "Bricked But Pretty") {
		t.Error("attr floor on the bricked CPU roll matched")
	}
	// Source module name search.
	_, body = getAnon(t, app, "/contracts/search/?source_q=Damage")
	mustContain(t, "source search", body, "Bricked But Pretty")

	// Detail: rolls with percentile bars and signed deltas,
	// the estimate with spread and basis, the cap sentence,
	// and the abyssal tier label on the line.
	code, body := getAnon(t, app, "/contracts/search/8501/")
	if code != 200 {
		t.Fatalf("detail: status %d", code)
	}
	mustContain(t, "abyssal detail", body,
		"damageMultiplier", "&#43;18.0%", `data-w="80"`, `data-w="2"`,
		"Roll score: <strong>45 / 100</strong>",
		"Our estimate: <strong>205,000,000.00 ISK</strong>",
		"likely range 180,000,000.00 to 240,000,000.00 ISK",
		"from 8 current and recent listings of this module",
		"Held back by: CPU fitting cost",
		"Our abyssal estimate")
}
