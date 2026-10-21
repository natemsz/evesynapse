package app

// The abyssal roll sliders (backlog E1): per-attribute
// percentile bands in contract search. The fixture corpus
// holds one family (test_damage_thing) whose mutaplasmid
// rolls damage (64), capacitor (6) and CPU (50), plus a
// fixed attribute (999) whose range band has no width and
// so is never offered. Four contracts carry known stored
// percentiles; the fourth item never rolled damage at all.

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

func seedRollBandCorpus(t *testing.T, conn *sql.DB) {
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
		(64, 'damageMultiplier', 1, 1, 0, 0), (6, 'capacitorNeed', 1, 0, 0, 0),
		(50, 'cpu', 1, 0, 0, 0), (999, 'fixedThing', 1, 1, 0, 0)`)
	exec(`INSERT INTO sde_type_attributes (type_id, attribute_id, value) VALUES
		(30001, 64, 2.0), (30001, 6, 10), (30001, 50, 20)`)
	exec(`INSERT INTO abyssal_family_weights (family, attribute_id, weight, penalty_only) VALUES
		('test_damage_thing', 64, 1.0, false), ('test_damage_thing', 6, 0.25, false),
		('test_damage_thing', 50, 0.5, true)`)
	// The mutaplasmid's roll ranges: 999's band has no
	// width, so no percentile can ever exist for it.
	exec(`INSERT INTO abyssal_ranges (mutator_type_id, attribute_id, min_mult, max_mult, sde_build) VALUES
		(47735, 64, 0.85, 1.15, 'test'), (47735, 6, 0.8, 1.1, 'test'),
		(47735, 50, 1.0, 1.4, 'test'), (47735, 999, 1.1, 1.1, 'test')`)

	contracts := []struct {
		id    int64
		title string
		price float64
	}{
		{8501, "Bricked But Pretty", 300000000},
		{8502, "Mid Roll Special", 150000000},
		{8503, "Glass Cannon", 500000000},
		{8504, "No Damage Roll", 120000000},
	}
	for _, c := range contracts {
		exec(`INSERT INTO pub_contracts (contract_id, region_id, type, issuer_id, issuer_corporation_id,
			title, price, buyout, reward, collateral, volume, start_location_id, end_location_id,
			date_issued, date_expired, days_to_complete, items_state, last_cycle, first_seen, last_seen,
			offered_value, want_value, priced_lines, total_lines, tier_mix, priced_at)
			VALUES ($1, 10000002, 'item_exchange', 9001, 9801, $2, $3, 0, 0, 0, 0,
			60003760, 0, $4, $5, 1, 'stored', 1, $4, $4, 100000000, 0, 1, 1, 'abyssal:1', $4)`,
			c.id, c.title, c.price, now.Add(-time.Hour), now.Add(7*24*time.Hour))
	}
	items := []struct {
		record, contract, item int64
		score                  float64
		attrs, pcts            string
	}{
		{1, 8501, 900001, 0.45,
			`[{"attribute_id":64,"value":2.36},{"attribute_id":6,"value":10.2},{"attribute_id":50,"value":27.9}]`,
			`{"64":0.8,"6":0.6,"50":0.02}`},
		{2, 8502, 900002, 0.30,
			`[{"attribute_id":64,"value":1.88},{"attribute_id":6,"value":8.3},{"attribute_id":50,"value":24.0}]`,
			`{"64":0.3,"6":0.9,"50":0.5}`},
		{3, 8503, 900003, 0.92,
			`[{"attribute_id":64,"value":2.44},{"attribute_id":6,"value":10.6},{"attribute_id":50,"value":25.6}]`,
			`{"64":0.95,"6":0.2,"50":0.7}`},
		{4, 8504, 900004, 0.50,
			`[{"attribute_id":6,"value":9.5},{"attribute_id":50,"value":24.0}]`,
			`{"6":0.5,"50":0.5}`},
	}
	for _, it := range items {
		exec(`INSERT INTO pub_contract_items (record_id, contract_id, type_id, item_id, quantity, is_included,
			unit_price, line_value, price_tier, price_asof)
			VALUES ($1, $2, 60001, $3, 1, true, 100000000, 100000000, 'abyssal', $4)`,
			it.record, it.contract, it.item, now)
		exec(`INSERT INTO abyssal_items (item_id, type_id, source_type_id, mutator_type_id, created_by,
			attributes, fetched_at, first_seen_at, last_seen_at,
			family, score, percentiles, derived_stats, est_value, est_low, est_high,
			est_basis, est_scope, est_comps, premium_mult, cap_attribute_id, cap_kind, scored_at)
			VALUES ($1, 60001, 30001, 47735, 9001, $2, $3, $3, $3,
			'test_damage_thing', $4, $5, '[]', 100000000, 90000000, 110000000,
			'corpus', 'module', 4, 1, 0, '', $3)`,
			it.item, it.attrs, now, it.score, it.pcts)
	}
}

func TestContractSearchRollSlidersOffered(t *testing.T) {
	tr := &openWindowTransport{}
	app, conn, _ := buildCorpTestApp(t, tr)
	seedRollBandCorpus(t, conn)

	// With the family in scope, the drawer's sliders for
	// every attribute its mutaplasmids roll -- damage,
	// capacitor and the CPU penalty -- are visible and
	// enabled, and never the fixed one (it renders nowhere).
	_, body := getAnon(t, app, "/contracts/search/?family=test_damage_thing")
	mustContain(t, "family roll sliders", body,
		`name="rollmin_64"`, `name="rollmax_64"`,
		`name="rollmin_6"`, `name="rollmax_6"`,
		`name="rollmin_50"`, `name="rollmax_50"`,
		`data-roll-field="64">`, `data-roll-field="6">`, `data-roll-field="50">`)
	if strings.Contains(body, `name="rollmin_999"`) {
		t.Error("the fixed attribute (no range width) got a slider")
	}
	// The page carries the per-family slider catalog,
	// attributes heaviest weight first.
	mustContain(t, "roll catalog", body, `id="cs-roll-catalog"`,
		`"family":"test_damage_thing","attrs":[64,50,6]`)
	// No family and no band: the guidance shows, and every
	// slider renders hidden and disabled, so the submitted
	// form carries no band -- the picker's swap enables
	// them client-side when a family is chosen.
	_, body = getAnon(t, app, "/contracts/search/")
	mustContain(t, "no-family guidance", body,
		`data-roll-hint>Pick a module family to band any of its rolls.`,
		`data-roll-field="64" hidden`,
		`name="rollmin_64" min="0" max="100" value="" placeholder="0" disabled`)
	// A band alone makes its own slider visible and enabled
	// (and opens the abyssal drawer), so an active band is
	// never invisible; the unbanded attributes stay
	// disabled.
	_, body = getAnon(t, app, "/contracts/search/?rollmax_64=50")
	mustContain(t, "banded slider", body,
		`data-roll-field="64">`,
		`name="rollmax_64" min="0" max="100" value="50" placeholder="100">`,
		`<section class="cs-drawer" data-drawer-panel="abyssal">`,
		`name="rollmin_6" min="0" max="100" value="" placeholder="0" disabled`)
}

// seedSecondRollFamily adds a second abyssal family whose
// mutaplasmid rolls one attribute (77) the first family's
// never does, for the catalog and cross-family band pins.
func seedSecondRollFamily(t *testing.T, conn *sql.DB) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("seed %q: %v", query, err)
		}
	}
	exec(`INSERT INTO sde_attribute_types (attribute_id, name, stackable, high_is_good, unit_id, default_value) VALUES
		(77, 'testAttrB', 1, 1, 0, 0)`)
	exec(`INSERT INTO abyssal_ranges (mutator_type_id, attribute_id, min_mult, max_mult, sde_build) VALUES
		(47737, 77, 0.9, 1.2, 'test')`)
	exec(`INSERT INTO abyssal_items (item_id, type_id, source_type_id, mutator_type_id, created_by,
		attributes, fetched_at, first_seen_at, last_seen_at,
		family, score, percentiles, derived_stats, est_value, est_low, est_high,
		est_basis, est_scope, est_comps, premium_mult, cap_attribute_id, cap_kind, scored_at)
		VALUES (900005, 60001, 30001, 47737, 9001,
		'[{"attribute_id":77,"value":1.0}]', $1, $1, $1,
		'test_other_thing', 0.5, '{"77":0.5}', '[]', 100000000, 90000000, 110000000,
		'corpus', 'module', 4, 1, 0, '', $1)`, now)
}

func TestContractSearchRollSliderCatalog(t *testing.T) {
	tr := &openWindowTransport{}
	app, conn, _ := buildCorpTestApp(t, tr)
	seedRollBandCorpus(t, conn)
	seedSecondRollFamily(t, conn)

	// The catalog carries every family's rollable
	// attributes, so the drawer can swap slider sets as
	// families are picked without a submit.
	_, body := getAnon(t, app, "/contracts/search/")
	mustContain(t, "two-family catalog", body,
		`"family":"test_damage_thing","attrs":[64,50,6]`,
		`"family":"test_other_thing","attrs":[77]`)

	// A band on the other family's attribute stays visible
	// and enabled beside the picked family's sliders.
	_, body = getAnon(t, app, "/contracts/search/?family=test_damage_thing&rollmin_77=40")
	mustContain(t, "cross-family band", body,
		`data-roll-field="64">`,
		`data-roll-field="77">`,
		`name="rollmin_77" min="0" max="100" value="40" placeholder="0">`)

	// With the other family in scope, the first family's
	// unbanded sliders render hidden and disabled.
	_, body = getAnon(t, app, "/contracts/search/?family=test_other_thing")
	mustContain(t, "other family scope", body,
		`data-roll-field="77">`,
		`data-roll-field="64" hidden`,
		`name="rollmin_64" min="0" max="100" value="" placeholder="0" disabled`)
}

func TestContractSearchRollBands(t *testing.T) {
	tr := &openWindowTransport{}
	app, conn, _ := buildCorpTestApp(t, tr)
	seedRollBandCorpus(t, conn)
	all := []string{"Bricked But Pretty", "Mid Roll Special", "Glass Cannon", "No Damage Roll"}

	// Min band on damage: the 80 and 95 percentiles pass,
	// the 30 fails, and the item that never rolled damage
	// is out (a band requires the roll).
	_, body := getAnon(t, app, "/contracts/search/?family=test_damage_thing&rollmin_64=50")
	mustContain(t, "damage floor count", body, "<strong>2</strong> contracts match")
	for _, title := range []string{"Bricked But Pretty", "Glass Cannon"} {
		mustContain(t, "damage floor 50", body, title)
	}
	for _, title := range []string{"Mid Roll Special", "No Damage Roll"} {
		if strings.Contains(body, title) {
			t.Errorf("damage floor 50 matched %q", title)
		}
	}
	// Max band alone: only the 30-percentile roll survives.
	_, body = getAnon(t, app, "/contracts/search/?rollmax_64=50")
	mustContain(t, "damage cap 50", body, "Mid Roll Special")
	for _, title := range []string{"Bricked But Pretty", "Glass Cannon", "No Damage Roll"} {
		if strings.Contains(body, title) {
			t.Errorf("damage cap 50 matched %q", title)
		}
	}
	// Two bands compose (AND) across attributes: damage at
	// least 50 and capacitor at most 50 leaves only the
	// Glass Cannon (80/60 fails the cap, 30 fails damage).
	_, body = getAnon(t, app, "/contracts/search/?rollmin_64=50&rollmax_6=50")
	mustContain(t, "two bands", body, "Glass Cannon")
	for _, title := range []string{"Bricked But Pretty", "Mid Roll Special", "No Damage Roll"} {
		if strings.Contains(body, title) {
			t.Errorf("damage>=50 & capacitor<=50 matched %q", title)
		}
	}
	// Bands compose with the older filters: the score
	// floor drops the 45-score line the damage band kept.
	_, body = getAnon(t, app, "/contracts/search/?rollmin_64=50&score_min=50")
	mustContain(t, "band + score floor", body, "Glass Cannon")
	if strings.Contains(body, "Bricked But Pretty") {
		t.Error("score floor 50 should drop the 45-score line the damage band kept")
	}
	_, body = getAnon(t, app, "/contracts/search/?rollmin_64=50&score_min=40")
	mustContain(t, "band + lower score floor", body, "Bricked But Pretty", "Glass Cannon")
	// Crossed bounds swap into order: 90..50 bands 50..90,
	// which only the 80-percentile roll sits inside.
	_, body = getAnon(t, app, "/contracts/search/?rollmin_64=90&rollmax_64=50")
	mustContain(t, "crossed band", body, "Bricked But Pretty")
	for _, title := range []string{"Mid Roll Special", "Glass Cannon", "No Damage Roll"} {
		if strings.Contains(body, title) {
			t.Errorf("crossed band 90..50 matched %q", title)
		}
	}
	// An explicit full band still requires the roll: the
	// unrolled item stays out while every rolled one is in.
	_, body = getAnon(t, app, "/contracts/search/?rollmin_64=0&rollmax_64=100")
	for _, title := range []string{"Bricked But Pretty", "Mid Roll Special", "Glass Cannon"} {
		mustContain(t, "full band", body, title)
	}
	if strings.Contains(body, "No Damage Roll") {
		t.Error("a full damage band matched an item that never rolled damage")
	}
	// Out-of-range values clamp: a 250 cap is a 100 cap.
	_, body = getAnon(t, app, "/contracts/search/?rollmax_64=250")
	for _, title := range []string{"Bricked But Pretty", "Mid Roll Special", "Glass Cannon"} {
		mustContain(t, "clamped cap", body, title)
	}
	// Garbage never narrows and never errors: an
	// unparseable bound is ignored, and an attribute no
	// mutaplasmid rolls is pruned before it can filter.
	for _, suffix := range []string{"rollmin_64=banana", "rollmin_9999=50", "rollmin_999=50"} {
		code, body := getAnon(t, app, "/contracts/search/?"+suffix)
		if code != 200 {
			t.Errorf("%s: status %d, want 200", suffix, code)
		}
		for _, title := range all {
			mustContain(t, "garbage band "+suffix, body, title)
		}
	}
	// Unset sliders are no filter at all: empty bounds
	// return the family's whole set, exactly like the
	// plain family search.
	_, body = getAnon(t, app, "/contracts/search/?family=test_damage_thing&rollmin_64=&rollmax_64=")
	for _, title := range all {
		mustContain(t, "unset band equivalence", body, title)
	}
}

func TestContractSearchRollBandChips(t *testing.T) {
	tr := &openWindowTransport{}
	app, conn, _ := buildCorpTestApp(t, tr)
	seedRollBandCorpus(t, conn)

	// A two-sided band chips once, naming the attribute and
	// the band; the chip's URL drops both bounds and keeps
	// the family.
	_, body := getAnon(t, app, "/contracts/search/?family=test_damage_thing&rollmin_64=50&rollmax_64=90")
	mustContain(t, "band chip", body, "Roll: damageMultiplier 50–90%",
		`href="/contracts/search/?family=test_damage_thing"`)
	// One-sided chips name their single bound.
	_, body = getAnon(t, app, "/contracts/search/?rollmin_6=40")
	mustContain(t, "min chip", body, "Roll: capacitorNeed ≥ 40%")
	_, body = getAnon(t, app, "/contracts/search/?rollmax_50=25")
	mustContain(t, "max chip", body, "Roll: cpu ≤ 25%")
	// A pruned band (attribute nothing rolls) chips nothing.
	_, body = getAnon(t, app, "/contracts/search/?rollmin_9999=50")
	if strings.Contains(body, "Attribute #9999") {
		t.Error("a band on an unrollable attribute rendered a chip")
	}
}
