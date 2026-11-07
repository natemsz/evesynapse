package app

// The shared demo account (demo.go, demo_seed.go): seeded from
// the SDE like everything else, so these tests first plant a
// small SDE fixture with the names the seed resolves, then pin
// the account's shape: the seed is complete and idempotent, the
// worker never queues its pilots, its writes are turned away
// while a normal account's go through, and the daily reset puts
// tampered data back.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// seedDemoSDEFixture plants the SDE rows the demo seed resolves
// by name: the stations and systems of the seeded story, the
// item types its payloads mention, and a skill set (two groups
// of the skill category) for the all-V pilot and her alt.
func seedDemoSDEFixture(t *testing.T, conn *sql.DB) {
	t.Helper()
	ctx := context.Background()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("sde fixture %q: %v", query, err)
		}
	}
	exec(`INSERT INTO sde_categories (category_id, name) VALUES (16, 'Skill')`)
	exec(`INSERT INTO sde_groups (group_id, name, category_id) VALUES (255, 'Gunnery', 16), (256, 'Industry', 16), (25, 'Frigate', 6)`)
	exec(`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES
		(30000142, 'Jita', 10000002, 0.9),
		(30002187, 'Amarr', 10000043, 0.9),
		(30002659, 'Dodixie', 10000032, 0.9),
		(30003068, 'Tama', 10000020, 0.3)`)
	exec(`INSERT INTO sde_stations (station_id, name, system_id) VALUES
		(60003760, 'Jita IV - Moon 4 - Caldari Navy Assembly Plant', 30000142),
		(60008494, 'Amarr VIII (Oris) - Emperor Family Academy', 30002187),
		(60011866, 'Dodixie IX - Moon 20 - Federation Navy Assembly Plant', 30002659)`)

	skills := []string{
		"Mining", "Astrogeology", "Industry", "Mass Production", "Advanced Mass Production",
		"Reprocessing", "Reprocessing Efficiency", "Metallurgy", "Science",
		"Laboratory Operation", "Advanced Laboratory Operation", "Research",
		"Mining Foreman", "Mining Director", "Leadership", "Spaceship Command",
		"Mining Barge", "Exhumers", "ORE Industrial", "Veldspar Processing",
		"Scordite Processing", "Pyroxeres Processing", "Drones", "Drone Interfacing",
		"Shield Operation", "Energy Grid Upgrades", "CPU Management",
		"Power Grid Management", "Navigation", "Afterburner", "Trade", "Retail",
		"Broker Relations", "Accounting", "Cybernetics", "Biology",
		"Capital Industrial Ships", "Gunnery", "Small Projectile Turret",
	}
	for i, name := range skills {
		id := int64(910001 + i)
		group := int64(256)
		if name == "Gunnery" || name == "Small Projectile Turret" {
			group = 255
		}
		exec(`INSERT INTO sde_types (type_id, name, group_id, published) VALUES ($1, $2, $3, 1)`, id, name, group)
		exec(`INSERT INTO sde_skill_meta (type_id, rank) VALUES ($1, 2)`, id)
	}
	items := []string{
		"Rifter", "Thrasher", "Merlin", "Punisher", "Drake", "Vexor", "Covetor",
		"Damage Control II", "Gyrostabilizer II", "Ballistic Control System II",
		"1MN Afterburner II", "Medium Shield Extender II", "200mm AutoCannon II",
		"125mm Gatling AutoCannon II", "Heavy Missile Launcher II",
		"Warp Scrambler II", "Stasis Webifier II", "Small Armor Repairer II",
		"Energized Adaptive Nano Membrane II", "Strip Miner I", "Mining Laser Upgrade II",
		"Hobgoblin II", "Warrior II", "Republic Fleet EMP S", "Fusion S", "Scourge Heavy Missile",
		"Tritanium", "Pyerite", "Mexallon", "Isogen", "Nocxium", "Zydrine", "Megacyte",
		"Veldspar", "Scordite", "Pyroxeres", "PLEX",
		"Rifter Blueprint", "Thrasher Blueprint", "Damage Control II Blueprint",
		"Vexor Blueprint", "Covetor Blueprint", "Antimatter Charge S Blueprint",
		"Ocular Filter - Basic", "Memory Augmentation - Basic", "Neural Boost - Basic",
		"Cybernetic Subprocessor - Basic", "Social Adaptation Chip - Basic",
		"Oxygen", "Water", "Bacteria", "Proteins", "Reactive Gas", "Precious Metals",
		"Toxic Metals", "Electrolytes", "Micro Organisms", "Carbon Compounds",
		"Aqueous Liquids", "Ionic Solutions", "Base Metals", "Suspended Plasma",
		"Noble Gas", "Reactive Metals", "Plasmoids",
		"Temperate Command Center", "Barren Command Center", "Gas Command Center",
		"Lava Command Center", "Extractor Control Unit", "Storage Facility",
		"Launchpad", "Basic Industry Facility", "Advanced Industry Facility",
	}
	itemIDs := map[string]int64{}
	for i, name := range items {
		id := int64(920001 + i)
		itemIDs[name] = id
		exec(`INSERT INTO sde_types (type_id, name, group_id, published) VALUES ($1, $2, 25, 1)`, id, name)
	}
	// The blueprints the seeded jobs run, with simple mineral
	// recipes: without these the fold cannot say what a job
	// built, and Industry by blueprint reads zero built.
	blueprints := []struct {
		name, product string
		materials     map[string]int64
	}{
		{"Damage Control II Blueprint", "Damage Control II", map[string]int64{"Tritanium": 9000, "Pyerite": 2600, "Mexallon": 850, "Isogen": 300, "Nocxium": 90}},
		{"Vexor Blueprint", "Vexor", map[string]int64{"Tritanium": 3200000, "Pyerite": 900000, "Mexallon": 160000, "Isogen": 12000}},
		{"Rifter Blueprint", "Rifter", map[string]int64{"Tritanium": 26000, "Pyerite": 7000, "Mexallon": 950}},
		{"Thrasher Blueprint", "Thrasher", map[string]int64{"Tritanium": 82000, "Pyerite": 24000, "Mexallon": 2600, "Isogen": 180}},
		{"Covetor Blueprint", "Covetor", map[string]int64{"Tritanium": 1900000, "Pyerite": 380000, "Mexallon": 30000, "Isogen": 1600, "Nocxium": 300}},
	}
	for _, bp := range blueprints {
		exec(`INSERT INTO sde_blueprints (blueprint_type_id, product_type_id, product_quantity, max_production_limit, manufacturing_time_seconds) VALUES ($1, $2, 1, 100, 600)`,
			itemIDs[bp.name], itemIDs[bp.product])
		for mat, qty := range bp.materials {
			exec(`INSERT INTO sde_blueprint_materials (blueprint_type_id, material_type_id, quantity) VALUES ($1, $2, $3)`,
				itemIDs[bp.name], itemIDs[mat], qty)
		}
	}
	// The Forge's swept prices for what the pilots trade and hold:
	// an install's own market data, which the seed reads and never
	// writes, standing in here for a real sweep.
	prices := []struct {
		name      string
		sell, buy float64
		dailyVol  float64
	}{
		{"Tritanium", 6.32, 5.94, 900000000}, {"Pyerite", 12.42, 11.66, 260000000},
		{"Mexallon", 52.10, 48.90, 64000000}, {"Isogen", 548, 512, 21000000},
		{"Nocxium", 1290, 1205, 2600000}, {"Damage Control II", 768000, 721000, 2600},
		{"Republic Fleet EMP S", 1046, 982, 1900000}, {"Scourge Heavy Missile", 471, 442, 1500000},
		{"Warrior II", 688, 640, 320000}, {"Gyrostabilizer II", 1185000, 1110000, 900},
		{"Ballistic Control System II", 906000, 850000, 2100}, {"Stasis Webifier II", 2250000, 2110000, 700},
		{"Vexor", 12400000, 11600000, 480}, {"Thrasher", 1460000, 1370000, 5200},
		{"Rifter", 428000, 402000, 9600}, {"Covetor", 26400000, 24800000, 350},
		{"Drake", 68500000, 64400000, 620}, {"Hobgoblin II", 512000, 481000, 14000},
		{"PLEX", 4310000, 4050000, 26000},
	}
	for _, p := range prices {
		exec(`INSERT INTO market_region_stats (region_id, type_id, best_sell, typical_sell, sell_band, best_buy, typical_buy, buy_band,
			sell_orders, buy_orders, sell_volume, buy_volume, avg_daily_volume, updated_at)
			VALUES (10000002, $1, $2, $3, $4, $5, $6, $7, 24, 19, $8, $9, $10, now())`,
			itemIDs[p.name], p.sell*0.99, p.sell, p.sell*0.06, p.buy*1.01, p.buy, p.buy*0.06,
			int64(p.dailyVol*0.6), int64(p.dailyVol*0.4), p.dailyVol)
	}
}

// demoTestApp builds the fixture application with the SDE
// planted and the demo account ensured.
func demoTestApp(t *testing.T) (*Application, *sql.DB, *db.Queries) {
	t.Helper()
	transport := &countingTransport{}
	app, conn, queries := buildCorpTestApp(t, transport)
	seedDemoSDEFixture(t, conn)
	if err := app.ensureDemoAccount(context.Background()); err != nil {
		t.Fatalf("ensureDemoAccount: %v", err)
	}
	if transport.calls.Load() != 0 {
		t.Fatalf("seeding reached the network %d times; it must only read stored data", transport.calls.Load())
	}
	return app, conn, queries
}

func TestDemoSeedIsCompleteAndIdempotent(t *testing.T) {
	app, conn, queries := demoTestApp(t)
	ctx := context.Background()

	userID := app.demoUserID(ctx)
	if userID == 0 {
		t.Fatal("demo user id is 0 after ensure")
	}
	user, err := queries.GetUser(ctx, userID)
	if err != nil || user.IsDemo == 0 {
		t.Fatalf("demo user row: %+v, err %v", user, err)
	}
	chars, err := queries.ListCharactersByUser(ctx, userID)
	if err != nil || len(chars) != 2 {
		t.Fatalf("demo characters: %d, err %v", len(chars), err)
	}
	for _, ch := range chars {
		if ch.AccessToken != "" || ch.RefreshToken != "" {
			t.Errorf("demo character %d carries a token; it must have none", ch.CharacterID)
		}
		if got := len(strings.Fields(ch.Scopes)); got != len(eveScopes) {
			t.Errorf("demo character %d scopes = %d, want the full %d", ch.CharacterID, got, len(eveScopes))
		}
	}

	counts := func() (int, int) {
		var snaps, fits int
		if err := conn.QueryRowContext(ctx,
			`SELECT count(*) FROM character_snapshots WHERE character_id IN ($1, $2)`,
			demoCharacterID, demoCharacter2ID).Scan(&snaps); err != nil {
			t.Fatal(err)
		}
		if err := conn.QueryRowContext(ctx,
			`SELECT count(*) FROM local_fittings WHERE user_id = $1`, userID).Scan(&fits); err != nil {
			t.Fatal(err)
		}
		return snaps, fits
	}
	snapsBefore, fitsBefore := counts()
	if snapsBefore < 60 {
		t.Errorf("seed wrote %d snapshots, want a full account's worth (60+)", snapsBefore)
	}
	if fitsBefore != 3 {
		t.Errorf("seed wrote %d saved fits, want 3", fitsBefore)
	}

	// The all-V pilot: every fixture skill present at level 5,
	// and the total is their sum.
	snap, err := queries.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: demoCharacterID, Kind: esi.SnapSkills})
	if err != nil {
		t.Fatalf("skills snapshot: %v", err)
	}
	var skills esi.Skills
	if err := json.Unmarshal([]byte(snap.Payload), &skills); err != nil {
		t.Fatalf("decode skills: %v", err)
	}
	if len(skills.Skills) != 39 {
		t.Errorf("seeded %d skills, want all 39 fixture skills", len(skills.Skills))
	}
	var total int64
	for _, sk := range skills.Skills {
		if sk.TrainedSkillLevel != 5 || sk.ActiveSkillLevel != 5 {
			t.Errorf("skill %d at level %d, want V", sk.SkillID, sk.TrainedSkillLevel)
		}
		total += sk.SkillpointsInSkill
	}
	if total != skills.TotalSP {
		t.Errorf("total_sp = %d, sum of skills = %d", skills.TotalSP, total)
	}

	// The seed folds the pilots' trades into the profit ledger
	// itself (the worker never warms the demo), so the account
	// pool has daily profit rows with real income to test the
	// Profit pages against.
	profitDays := func() (int, float64) {
		var days int
		var income float64
		if err := conn.QueryRowContext(ctx,
			`SELECT count(DISTINCT day), COALESCE(sum(income), 0) FROM profit_daily WHERE owner_kind = 'char' AND owner_id IN ($1, $2)`,
			demoCharacterID, demoCharacter2ID).Scan(&days, &income); err != nil {
			t.Fatal(err)
		}
		return days, income
	}
	daysBefore, incomeBefore := profitDays()
	if daysBefore < 3 || incomeBefore <= 0 {
		t.Errorf("seeded profit: %d days, %.0f income; want several days with income", daysBefore, incomeBefore)
	}

	// The seeded trading history carries covered sales: the
	// fold matches sells to buys and pairs their tax, so the
	// cost-of-goods data is populated across the demo window,
	// with some days only partly covered.
	now := time.Now().UTC()
	fromDay := now.AddDate(0, 0, -29).Format("2006-01-02")
	owners := []int64{demoCharacterID, demoCharacter2ID}
	buckets, err := queries.ListTradeCOGSBuckets(ctx, db.ListTradeCOGSBucketsParams{
		Unit: "day", OwnerKind: "char", OwnerIds: owners, FromDay: fromDay,
	})
	if err != nil {
		t.Fatalf("ListTradeCOGSBuckets: %v", err)
	}
	var covRev float64
	for _, b := range buckets {
		covRev += b.CovRevenue
	}
	if covRev <= 0 {
		t.Errorf("seeded covered sales revenue over the 30-day window = %.2f, want above zero", covRev)
	}
	coverage, err := queries.ListTradeCoverageDaily(ctx, db.ListTradeCoverageDailyParams{
		OwnerKind: "char", OwnerIds: owners, FromDay: fromDay,
	})
	if err != nil {
		t.Fatalf("ListTradeCoverageDaily: %v", err)
	}
	coveredDays := 0
	partly := false
	for _, c := range coverage {
		if c.CovRevenue > 0 {
			coveredDays++
		}
		if c.Units > c.CovUnits {
			partly = true
		}
	}
	if coveredDays < 10 {
		t.Errorf("days with covered sales = %d, want at least 10", coveredDays)
	}
	if !partly {
		t.Error("no partly covered day in the demo window: every sale matched, want some uncovered sales too")
	}

	// The demo profit view builds its full chart set.
	names := map[int64]string{demoCharacterID: demoCharName, demoCharacter2ID: demoChar2Name}
	charts, err := app.buildProfitCharts(ctx, &profitView{Days: 30}, "char", owners, names, fromDay, false)
	if err != nil {
		t.Fatalf("buildProfitCharts: %v", err)
	}
	if charts == nil {
		t.Fatal("buildProfitCharts returned no charts for the demo window")
	}
	if charts.Hero == nil || charts.Waterfall == nil || charts.Efficiency == nil {
		t.Errorf("demo chart set incomplete: hero %v, waterfall %v, efficiency %v",
			charts.Hero != nil, charts.Waterfall != nil, charts.Efficiency != nil)
	}

	// Seeding again lands on exactly the same state. The two
	// seeds compared here run off one captured time, so the
	// comparison covers the seed's own determinism: the first
	// seed above ran inside ensureDemoAccount on its own clock,
	// and a seed anchored to a different time is a different
	// fixture, not a replay.
	seedNow := time.Now()
	if err := app.seedDemoData(ctx, seedNow); err != nil {
		t.Fatalf("second seed: %v", err)
	}
	snapsSeeded, fitsSeeded := counts()
	daysSeeded, incomeSeeded := profitDays()
	if err := app.seedDemoData(ctx, seedNow); err != nil {
		t.Fatalf("third seed: %v", err)
	}
	snapsAfter, fitsAfter := counts()
	if snapsAfter != snapsSeeded || fitsAfter != fitsSeeded {
		t.Errorf("second seed changed the state: snapshots %d -> %d, fits %d -> %d",
			snapsSeeded, snapsAfter, fitsSeeded, fitsAfter)
	}
	daysAfter, incomeAfter := profitDays()
	if daysAfter != daysSeeded || incomeAfter != incomeSeeded {
		t.Errorf("second seed changed the profit ledger: days %d -> %d, income %v -> %v",
			daysSeeded, daysAfter, incomeSeeded, incomeAfter)
	}
}

func TestDemoSeedWaitsForTheSDE(t *testing.T) {
	app, conn, _ := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	// No SDE fixture: the account is created, the seed cannot
	// land, and nothing pretends otherwise.
	if err := app.ensureDemoAccount(ctx); err == nil {
		t.Fatal("ensureDemoAccount with an empty SDE: want an error")
	}
	if app.demoUserID(ctx) == 0 {
		t.Fatal("the demo account itself should still be created")
	}
	if app.demoSeedPresent(ctx) {
		t.Fatal("seed reported present with an empty SDE")
	}
	// Once the SDE exists, the same call completes the account.
	seedDemoSDEFixture(t, conn)
	if err := app.ensureDemoAccount(ctx); err != nil {
		t.Fatalf("ensureDemoAccount after the SDE landed: %v", err)
	}
	if !app.demoSeedPresent(ctx) {
		t.Fatal("seed still absent after the SDE landed")
	}
}

func TestDemoPilotsNeverSync(t *testing.T) {
	app, _, queries := demoTestApp(t)
	ctx := context.Background()

	demo, err := queries.GetCharacter(ctx, demoCharacterID)
	if err != nil {
		t.Fatal(err)
	}
	if !characterSyncs(demo) {
		t.Fatal("the demo pilot's link state should read healthy; the demo gate is separate")
	}
	if app.characterMaySync(ctx, demo) {
		t.Error("the worker may queue the demo pilot; it must never")
	}
	if _, err := app.validAccessToken(ctx, demo); err == nil {
		t.Error("validAccessToken handed out a token for the demo pilot; it must refuse")
	}

	// A control pilot on a normal account syncs as ever.
	user, err := queries.CreateUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	control, err := queries.UpsertCharacter(ctx, db.UpsertCharacterParams{
		CharacterID: 90000007, UserID: user.ID, Name: "Control Pilot",
		AccessToken: "fixture", RefreshToken: "fixture", LinkState: "ok",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !app.characterMaySync(ctx, control) {
		t.Error("a normal linked pilot no longer syncs")
	}
}

func TestDemoSignInAndPagesRender(t *testing.T) {
	app, _, _ := demoTestApp(t)

	// The landing page offers the button, signed out.
	landing := get(app, nil, "/")
	if landing.Code != http.StatusOK {
		t.Fatalf("GET / signed out = %d", landing.Code)
	}
	mustContain(t, "landing", landing.Body.String(), `href="/auth/demo"`, "Try the demo")

	// The button opens a session on the demo account.
	rec := get(app, nil, "/auth/demo")
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("GET /auth/demo = %d to %q, want 303 to /", rec.Code, rec.Header().Get("Location"))
	}
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "evesynapse_session" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("demo sign-in set no session cookie")
	}

	home := get(app, cookie, "/")
	if home.Code != http.StatusOK {
		t.Fatalf("demo GET / = %d", home.Code)
	}
	mustContain(t, "demo home", home.Body.String(),
		"Sable Iversen", "Test Pilot Collective", `badge demo`, "Demo")

	pages := []struct {
		path string
		want []string
	}{
		{"/character/", []string{"Sable Iversen", "Test Pilot Collective", "Gunnery", "Capital Industrial Ships"}},
		{"/wallet/", []string{"Bounty prizes"}},
		{"/mail/", []string{"Your Rifter is ready in Jita", "Saturday op: Tama roam"}},
		{"/industry/", []string{"Rifter", "Damage Control II"}},
		{"/contracts/", []string{"Fitted Rifter, ready to undock"}},
		{"/corporations/", []string{"Test Pilot Collective"}},
		{"/corporations/members/", []string{"Sera Voss", "Juno Park"}},
		{"/planets/", []string{"Jita IV"}},
		{"/killmails/", []string{"Thrasher"}},
		{"/fittings/saved/", []string{"Tama Rifter", "Mission Drake"}},
		// A seeded stranger's public record: the stored
		// envelope renders his name, corporation and security
		// status, and the seeded activity profile backs the
		// activity figures.
		{"/pilot/?character=2900000102", []string{"Ilya Sorin", "Hale Freight Services [HALE]", "1.20",
			"every recorded kill and loss", "Merlin", "1 final blows"}},
	}
	for _, page := range pages {
		rec := get(app, cookie, page.path)
		if rec.Code != http.StatusOK {
			t.Errorf("demo GET %s = %d, want 200", page.path, rec.Code)
			continue
		}
		mustContain(t, page.path, rec.Body.String(), page.want...)
		// A query string built ahead and dropped into a link is
		// escaped whole by html/template: its & and = arrive as
		// %26 and %3d, and the server reads one garbled parameter.
		if m := escapedQueryLink.FindString(rec.Body.String()); m != "" {
			t.Errorf("demo GET %s: link carries an escaped query string: %s", page.path, m)
		}
	}
}

// escapedQueryLink matches a link whose query carries an escaped
// "&name=" pair, the mark of a prebuilt query string the template
// escaped as one value.
var escapedQueryLink = regexp.MustCompile(`(?i)(?:href|action|src)="[^"]*%26[a-z_]+%3d[^"]*"`)

func TestDemoWritesAreTurnedAway(t *testing.T) {
	app, _, queries := demoTestApp(t)
	ctx := context.Background()
	userID := app.demoUserID(ctx)
	cookie := app.SessionCookie(t, userID, demoCharacterID, demoCharName)

	mailBefore, err := queries.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: demoCharacterID, Kind: esi.SnapMail})
	if err != nil {
		t.Fatal(err)
	}

	blocked := []struct {
		name string
		path string
		form url.Values
	}{
		{"mail send", "/mail/send/", url.Values{"to": {"90000007"}, "subject": {"hi"}, "body": {"hello"}}},
		{"mail mark read", "/mail/read/", url.Values{"mail": {"3900000001"}}},
		{"home layout", "/home/layout", url.Values{"action": {"toggle"}, "widget": {"fleet"}}},
		{"character unlink", fmt.Sprintf("/characters/%d/unlink", demoCharacterID), url.Values{}},
		{"calendar respond", fmt.Sprintf("/calendar/%d/respond", demoEventMining), url.Values{"response": {"declined"}}},
		{"settings tags", fmt.Sprintf("/characters/%d/tags", demoCharacterID), url.Values{"tags": {"main"}}},
	}
	for _, b := range blocked {
		code, _ := postForm(t, app, cookie, b.path, b.form)
		if code != http.StatusSeeOther {
			t.Errorf("demo POST %s (%s) = %d, want 303 turned away", b.name, b.path, code)
		}
	}
	mailAfter, err := queries.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: demoCharacterID, Kind: esi.SnapMail})
	if err != nil {
		t.Fatal(err)
	}
	if mailAfter.Payload != mailBefore.Payload {
		t.Error("the mail snapshot changed under blocked writes")
	}
	if _, err := queries.GetCharacter(ctx, demoCharacterID); err != nil {
		t.Errorf("the demo pilot vanished under a blocked unlink: %v", err)
	}
	user, err := queries.GetUser(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if user.HomeLayout != "" {
		t.Errorf("home layout persisted for the demo: %q", user.HomeLayout)
	}

	// Publishing a saved fit is blocked, and the corpus stays clean.
	fits, err := queries.ListLocalFittings(ctx, userID)
	if err != nil || len(fits) == 0 {
		t.Fatalf("list demo fits: %v (%d)", err, len(fits))
	}
	fit := fits[0]
	code, _ := postForm(t, app, cookie, fmt.Sprintf("/fittings/%d/publish", fit.ID), url.Values{})
	if code != http.StatusSeeOther {
		t.Errorf("demo POST publish fit = %d, want 303", code)
	}
	row, err := queries.GetLocalFitting(ctx, db.GetLocalFittingParams{ID: fit.ID, UserID: userID})
	if err != nil {
		t.Fatal(err)
	}
	if row.IsPublic {
		t.Error("a demo fit reached the public corpus")
	}

	// The route send answers in JSON and sends nothing.
	code, body := postForm(t, app, cookie, "/map/route", url.Values{"route": {"30000142"}})
	if code != http.StatusForbidden || !strings.Contains(body, "demo") {
		t.Errorf("demo POST /map/route = %d %q, want 403 with the demo message", code, body)
	}

	// The read-only tools stay open in the demo.
	code, _ = postForm(t, app, cookie, "/auth/logout", url.Values{})
	if code == http.StatusForbidden {
		t.Error("demo sign-out was blocked")
	}
}

func TestDemoGateLeavesOtherAccountsAlone(t *testing.T) {
	app, _, queries := demoTestApp(t)
	ctx := context.Background()

	user, err := queries.CreateUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.UpsertCharacter(ctx, db.UpsertCharacterParams{
		CharacterID: 90000007, UserID: user.ID, Name: "Control Pilot",
		AccessToken: "fixture", RefreshToken: "fixture", LinkState: "ok",
	}); err != nil {
		t.Fatal(err)
	}
	cookie := app.SessionCookie(t, user.ID, 90000007, "Control Pilot")

	code, _ := postForm(t, app, cookie, "/home/layout", url.Values{"action": {"toggle"}, "widget": {"fleet"}})
	if code != http.StatusSeeOther {
		t.Fatalf("control POST /home/layout = %d, want 303", code)
	}
	got, err := queries.GetUser(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.HomeLayout == "" {
		t.Error("the control account's layout did not persist; the gate is not demo-scoped")
	}
}

func TestDemoResetRestoresSeedState(t *testing.T) {
	app, conn, queries := demoTestApp(t)
	ctx := context.Background()
	userID := app.demoUserID(ctx)

	// Tamper: a snapshot deleted, a stray fit added, a wallet
	// figure changed.
	if _, err := conn.ExecContext(ctx,
		`DELETE FROM character_snapshots WHERE character_id = $1 AND kind = 'mail'`, demoCharacterID); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CreateLocalFitting(ctx, db.CreateLocalFittingParams{
		UserID: userID, Name: "Visitor scribble", ShipTypeID: 920001,
		ItemsJson: "{}", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	// Not due yet: a fresh reset stamp leaves the tampering be.
	app.maybeResetDemo(ctx, time.Now())
	if _, err := queries.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: demoCharacterID, Kind: esi.SnapMail}); err == nil {
		t.Fatal("reset ran before its day was up")
	}

	// A day later the worker's pass restores the seed state.
	if err := queries.StampDemoReset(ctx, time.Now().Add(-25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	app.maybeResetDemo(ctx, time.Now())
	if _, err := queries.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: demoCharacterID, Kind: esi.SnapMail}); err != nil {
		t.Errorf("mail snapshot not restored: %v", err)
	}
	fits, err := queries.ListLocalFittings(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	for _, fit := range fits {
		if fit.Name == "Visitor scribble" {
			t.Error("the reset left a visitor's fit behind")
		}
	}
	if len(fits) != 3 {
		t.Errorf("fits after reset = %d, want the seeded 3", len(fits))
	}
}

// TestDemoPilotNameRefreshReachesSeededInstalls pins the
// bootstrap's name refresh: on an install whose seed already
// landed, every stored copy of the lead pilot's name follows
// the constant — the character row, the entity name, the pilot
// record and the profile snapshot — with the rest of each
// payload preserved and no other pilot's rows touched.
func TestDemoPilotNameRefreshReachesSeededInstalls(t *testing.T) {
	app, conn, queries := demoTestApp(t)
	ctx := context.Background()
	const stale = "Stale Name"

	setPayloadName := func(payload, name string) string {
		t.Helper()
		var pilot esi.Character
		if err := json.Unmarshal([]byte(payload), &pilot); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		pilot.Name = name
		raw, err := json.Marshal(pilot)
		if err != nil {
			t.Fatalf("encode payload: %v", err)
		}
		return string(raw)
	}

	// The pilot record is the pilotPayload envelope; its name
	// sits on the profile inside it.
	setRecordName := func(payload, name string) string {
		t.Helper()
		var rec pilotPayload
		if err := json.Unmarshal([]byte(payload), &rec); err != nil {
			t.Fatalf("decode pilot record payload: %v", err)
		}
		rec.Profile.Name = name
		raw, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("encode pilot record payload: %v", err)
		}
		return string(raw)
	}

	// Put a stale name in every store the bootstrap refreshes.
	if _, err := conn.ExecContext(ctx,
		`UPDATE characters SET name = $1 WHERE character_id = $2`, stale, demoCharacterID); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx,
		`UPDATE entity_names SET name = $1 WHERE kind = $2 AND id = $3`,
		stale, esi.NameCharacter, demoCharacterID); err != nil {
		t.Fatal(err)
	}
	rec, err := queries.GetPilotRecord(ctx, demoCharacterID)
	if err != nil {
		t.Fatalf("pilot record: %v", err)
	}
	if err := queries.SetPilotRecord(ctx, db.SetPilotRecordParams{
		CharacterID: demoCharacterID, Payload: setRecordName(rec.Payload, stale),
		State: rec.State, FetchedAt: rec.FetchedAt,
	}); err != nil {
		t.Fatal(err)
	}
	snap, err := queries.GetSnapshot(ctx, db.GetSnapshotParams{
		CharacterID: demoCharacterID, Kind: esi.SnapProfile,
	})
	if err != nil {
		t.Fatalf("profile snapshot: %v", err)
	}
	if err := queries.UpsertSnapshot(ctx, db.UpsertSnapshotParams{
		CharacterID: demoCharacterID, Kind: esi.SnapProfile,
		Payload:   setPayloadName(snap.Payload, stale),
		FetchedAt: snap.FetchedAt, CachedUntil: snap.CachedUntil, Etag: snap.Etag,
	}); err != nil {
		t.Fatal(err)
	}

	// The bootstrap pass puts the constant's name back in all
	// four stores.
	if err := app.ensureDemoAccount(ctx); err != nil {
		t.Fatalf("ensureDemoAccount: %v", err)
	}
	var charName string
	if err := conn.QueryRowContext(ctx,
		`SELECT name FROM characters WHERE character_id = $1`, demoCharacterID).Scan(&charName); err != nil {
		t.Fatal(err)
	}
	if charName != demoCharName {
		t.Errorf("character row name = %q, want %q", charName, demoCharName)
	}
	var entityName string
	if err := conn.QueryRowContext(ctx,
		`SELECT name FROM entity_names WHERE kind = $1 AND id = $2`,
		esi.NameCharacter, demoCharacterID).Scan(&entityName); err != nil {
		t.Fatal(err)
	}
	if entityName != demoCharName {
		t.Errorf("entity name = %q, want %q", entityName, demoCharName)
	}
	rec, err = queries.GetPilotRecord(ctx, demoCharacterID)
	if err != nil {
		t.Fatalf("pilot record: %v", err)
	}
	var record pilotPayload
	if err := json.Unmarshal([]byte(rec.Payload), &record); err != nil {
		t.Fatalf("decode pilot record: %v", err)
	}
	if record.Profile.Name != demoCharName {
		t.Errorf("pilot record name = %q, want %q", record.Profile.Name, demoCharName)
	}
	if record.Profile.Birthday != "2016-03-14T09:24:00Z" || record.Profile.SecurityStatus != 4.8 {
		t.Errorf("pilot record payload changed beyond the name: %+v", record.Profile)
	}
	if record.Corp.Name != demoCorpName || len(record.History) == 0 {
		t.Errorf("pilot record lost its corporation or history: %+v", record)
	}
	snap, err = queries.GetSnapshot(ctx, db.GetSnapshotParams{
		CharacterID: demoCharacterID, Kind: esi.SnapProfile,
	})
	if err != nil {
		t.Fatalf("profile snapshot: %v", err)
	}
	var profile esi.Character
	if err := json.Unmarshal([]byte(snap.Payload), &profile); err != nil {
		t.Fatalf("decode profile snapshot: %v", err)
	}
	if profile.Name != demoCharName {
		t.Errorf("profile snapshot name = %q, want %q", profile.Name, demoCharName)
	}
	if !strings.Contains(profile.Description, "sample data") {
		t.Errorf("profile snapshot payload changed beyond the name: %q", profile.Description)
	}

	// The other pilot's name is not the refresh's business.
	var altName string
	if err := conn.QueryRowContext(ctx,
		`SELECT name FROM entity_names WHERE kind = $1 AND id = $2`,
		esi.NameCharacter, demoCharacter2ID).Scan(&altName); err != nil {
		t.Fatal(err)
	}
	if altName != demoChar2Name {
		t.Errorf("alt pilot entity name = %q, want %q", altName, demoChar2Name)
	}

	// A second pass over current data is a no-op that succeeds.
	if err := app.ensureDemoAccount(ctx); err != nil {
		t.Fatalf("second ensureDemoAccount: %v", err)
	}
}

// The demo book hangs together on the Profit page: over 30 days the
// part of the net worth change the ledger cannot explain stays a
// small share of the change, the open positions carry a value, and
// the headline blueprint's jobs deliver units.
func TestDemoProfitCoherence(t *testing.T) {
	app, _, _ := demoTestApp(t)
	ctx := context.Background()
	view, err := app.buildProfitView(ctx, app.demoUserID(ctx), "", "30", "", profitModes{})
	if err != nil {
		t.Fatal(err)
	}
	parse := func(s string) float64 {
		v, err := strconv.ParseFloat(strings.ReplaceAll(s, ",", ""), 64)
		if err != nil {
			t.Fatalf("view figure %q does not parse: %v", s, err)
		}
		return v
	}
	change, remainder := parse(view.Change), parse(view.Remainder)
	if change == 0 {
		t.Fatal("demo net worth change over 30 days is zero; the coherence check has nothing to measure")
	}
	if math.Abs(remainder) > 0.25*math.Abs(change) {
		t.Errorf("not explained = %s, over 25%% of the net worth change %s", view.Remainder, view.Change)
	}
	if v := parse(view.OpenValue); v <= 0 {
		t.Errorf("open positions value = %s, want above zero", view.OpenValue)
	}
	built := int64(-1)
	for _, bp := range view.Blueprints {
		if bp.Name == "Damage Control II" {
			built = int64(parse(bp.Built))
		}
	}
	if built <= 0 {
		t.Errorf("headline blueprint built = %d, want above zero", built)
	}
}

// The demo's battle reports: the seed files fabricated game
// logs through the real parser and ingest, so the demo account
// holds three reports whose matched kills are the killmails
// the seed planted, and reseeding neither drops nor
// duplicates them.
func TestDemoSeedBattleReports(t *testing.T) {
	app, _, queries := demoTestApp(t)
	ctx := context.Background()
	userID := app.demoUserID(ctx)

	check := func(stage string) {
		t.Helper()
		fights, err := queries.ListBattleFightsByUser(ctx, userID)
		if err != nil {
			t.Fatalf("%s: list battle fights: %v", stage, err)
		}
		if len(fights) != 3 {
			t.Fatalf("%s: %d battle reports, want 3", stage, len(fights))
		}
		matched := map[int64]string{}
		var parties, weapons, tackles int
		for _, f := range fights {
			if f.SystemID != 30003068 {
				t.Errorf("%s: fight %d in system %d, want Tama (30003068)", stage, f.ID, f.SystemID)
			}
			kills, err := queries.ListBattleFightKills(ctx, f.ID)
			if err != nil {
				t.Fatalf("%s: list fight kills: %v", stage, err)
			}
			for _, k := range kills {
				matched[k.KillmailID] = f.Outcome
				if k.KillmailID == demoBattleLossMail && !k.Loss {
					t.Errorf("%s: the planted loss is not flagged as a loss on its report", stage)
				}
			}
			ps, err := queries.ListBattleFightParties(ctx, f.ID)
			if err != nil {
				t.Fatalf("%s: list fight parties: %v", stage, err)
			}
			parties += len(ps)
			ws, err := queries.ListBattleFightWeapons(ctx, f.ID)
			if err != nil {
				t.Fatalf("%s: list fight weapons: %v", stage, err)
			}
			weapons += len(ws)
			ts, err := queries.ListBattleFightTackles(ctx, f.ID)
			if err != nil {
				t.Fatalf("%s: list fight tackles: %v", stage, err)
			}
			tackles += len(ts)
		}
		for _, id := range demoBattleMailIDs {
			if _, ok := matched[id]; !ok {
				t.Errorf("%s: killmail %d is matched to no battle report", stage, id)
			}
		}
		if got := matched[demoBattleLossMail]; got != "loss" {
			t.Errorf("%s: the loss mail's report outcome = %q, want loss", stage, got)
		}
		if got := matched[demoBattleKillMail]; got != "kill" {
			t.Errorf("%s: the solo kill mail's report outcome = %q, want kill", stage, got)
		}
		if parties == 0 || weapons == 0 || tackles == 0 {
			t.Errorf("%s: reports carry %d parties, %d weapon rows, %d tackles; want all three kinds present",
				stage, parties, weapons, tackles)
		}
	}
	check("seeded")

	seedNow := time.Now()
	if err := app.seedDemoData(ctx, seedNow); err != nil {
		t.Fatalf("reseed one: %v", err)
	}
	if err := app.seedDemoData(ctx, seedNow); err != nil {
		t.Fatalf("reseed two: %v", err)
	}
	check("after two reseeds")
}
