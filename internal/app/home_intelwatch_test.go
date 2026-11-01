package app

// Intel watch widget: the home widget reads the account's
// watched systems (notifyPrefs.CampSystems, stored order) and
// shows the filed intel for them through the shared intel read
// path, the gate states at the default sizes, and the public
// kills involving the account's corporations at the largest.
// Unwatched systems never appear, and an account watching
// nothing gets the plain empty state with the manage button.

import (
	"context"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
)

const (
	iwSysAlpha = int64(30001001)
	iwSysBeta  = int64(30001002)
	iwSysGamma = int64(30001003)
	iwSysDelta = int64(30001004)
	iwCorpMine = int64(98000111)
)

// The fixture: four SDE systems, one owned intel channel, five
// filed reports (one in an unwatched system, one a gate-linked
// clear filed under the far system) and three kills (the
// account's corporation public and private, and a stranger's).
func TestIntelWatchWidget(t *testing.T) {
	transport := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now().UTC()
	exec := func(s string, args ...any) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, s, args...); err != nil {
			t.Fatal(err)
		}
	}
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	if err := q.UpsertCharacterCorporation(ctx, db.UpsertCharacterCorporationParams{
		CharacterID: fixtureCharA, CorporationID: iwCorpMine, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	for _, s := range []struct {
		id   int64
		name string
	}{
		{iwSysAlpha, "Alpha Watch"}, {iwSysBeta, "Beta Watch"}, {iwSysGamma, "Gamma Gate"}, {iwSysDelta, "Delta Far"},
	} {
		exec(`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES ($1, $2, 10000001, 0.4) ON CONFLICT DO NOTHING`, s.id, s.name)
	}
	exec(`INSERT INTO sde_types (type_id, name) VALUES (587, 'Rifter'), (17738, 'Machariel'), (22456, 'Sabre') ON CONFLICT DO NOTHING`)
	for id, name := range map[int64]string{587: "Rifter", 17738: "Machariel", 22456: "Sabre"} {
		if err := q.UpsertTypeName(ctx, db.UpsertTypeNameParams{TypeID: id, Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	cid, err := q.CreateIntelChannel(ctx, db.CreateIntelChannelParams{OwnerUserID: user.ID, Name: "Watch Chan", CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	report := func(systemID int64, text, kind, analysis string, hostiles int32, ago time.Duration) {
		t.Helper()
		if _, err := q.InsertIntelReport(ctx, db.InsertIntelReportParams{
			ChannelID: cid, SystemID: systemID, AuthorUserID: user.ID, AuthorCharacterID: fixtureCharA,
			Reporter: "Fixture Ceo", Text: text, Hostiles: hostiles, Clear: kind == "clear", Source: "local",
			SeenAt: now.Add(-ago), CreatedAt: now, Kind: kind, Analysis: analysis,
		}); err != nil {
			t.Fatal(err)
		}
	}
	report(iwSysBeta, "5 hostiles on the Beta Watch gate", "sighting", `{"kind":"sighting","hostile":5}`, 5, 5*time.Minute)
	report(iwSysBeta, "hostiles at the Alpha Watch gate", "sighting", `{"kind":"sighting","hostile":4,"gate":30001001}`, 4, 8*time.Minute)
	report(iwSysAlpha, "2 hostiles in Alpha Watch", "sighting", `{"kind":"sighting","hostile":2,"gate":30001003}`, 2, 10*time.Minute)
	report(iwSysGamma, "Alpha Watch gate clear", "clear", `{"kind":"clear","gate":30001001}`, 0, 30*time.Minute)
	report(iwSysAlpha, "old sighting in Alpha Watch", "sighting", `{"kind":"sighting","hostile":1}`, 1, 50*time.Minute)
	report(iwSysDelta, "hostiles in Delta Far", "sighting", `{"kind":"sighting","hostile":9}`, 9, time.Minute)

	kill := func(id int64, ago time.Duration, systemID, victimCorp, ship int64, public bool) {
		t.Helper()
		exec(`INSERT INTO kills (killmail_id, killmail_time, system_id, victim_character_id, victim_corporation_id, ship_type_id, location_id, source, public, ingested_at, value_zkill)
			VALUES ($1, $2, $3, $4, $5, $6, 0, 'stream', $7, now(), 10000000)`,
			id, now.Add(-ago), systemID, 92000000+id, victimCorp, ship, public)
		exec(`INSERT INTO kill_attackers (killmail_id, seq, character_id, corporation_id, alliance_id, ship_type_id, final_blow)
			VALUES ($1, 0, 91000001, 98000777, 0, 587, true)`, id)
	}
	kill(101, 20*time.Minute, iwSysAlpha, iwCorpMine, 587, true)
	kill(102, 15*time.Minute, iwSysBeta, iwCorpMine, 17738, false)
	kill(103, 12*time.Minute, iwSysDelta, 98000999, 22456, true)

	// Stored order is Beta first, then Alpha.
	if err := app.storeNotifyPrefs(ctx, user.ID, notifyPrefs{CampSystems: []int64{iwSysBeta, iwSysAlpha}}); err != nil {
		t.Fatal(err)
	}
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")
	home := func(layout string) string {
		t.Helper()
		setHomeLayout(t, q, user.ID, layout)
		code, body := getPage(t, app, cookie, "/")
		if code != 200 {
			t.Fatalf("home: status %d", code)
		}
		return body
	}

	// Default size: newest reports first across the watched
	// systems, the far-side clear phrased from this side, the
	// gate states, the modal in stored order, no kills yet.
	body := home(`[{"id":"intelwatch","w":2,"h":2}]`)
	for _, want := range []string{
		"5 hostiles on the Beta Watch gate",
		"hostiles at the Alpha Watch gate",
		"2 hostiles in Alpha Watch",
		"The Gamma Gate side is clear",
		"old sighting in Alpha Watch",
		"gates: Beta Watch Hostiles",
		"Gamma Gate Hostiles",
		`id="watched-systems-modal"`,
		"Watched systems</button>",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("default size: home missing %q", want)
		}
	}
	// The Beta report about Alpha's gate stands in both
	// systems' lists; the merged feed shows it once.
	if n := strings.Count(body, "hostiles at the Alpha Watch gate"); n != 1 {
		t.Fatalf("default size: the cross-gate report showed %d times, want 1", n)
	}
	if strings.Contains(body, "hostiles in Delta Far") {
		t.Fatal("default size: an unwatched system's report showed")
	}
	if strings.Contains(body, "Recent kills, your corporations") {
		t.Fatal("default size: kills showed below the largest band")
	}
	if i, j := strings.Index(body, "5 hostiles on the Beta Watch gate"), strings.Index(body, "2 hostiles in Alpha Watch"); i < 0 || j < 0 || i > j {
		t.Fatal("default size: reports are not newest first")
	}
	if i, j := strings.Index(body, `name="remove" value="30001002"`), strings.Index(body, `name="remove" value="30001001"`); i < 0 || j < 0 || i > j {
		t.Fatal("default size: the modal does not list the systems in stored order")
	}

	// Smallest size: the newest two reports only, no gate
	// states.
	body = home(`[{"id":"intelwatch","w":1,"h":1}]`)
	if !strings.Contains(body, "5 hostiles on the Beta Watch gate") || !strings.Contains(body, "hostiles at the Alpha Watch gate") {
		t.Fatal("small size: the newest two reports should show")
	}
	for _, unwanted := range []string{"2 hostiles in Alpha Watch", "The Gamma Gate side is clear", "old sighting in Alpha Watch", "gates:"} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("small size: %q showed below its band", unwanted)
		}
	}

	// Largest size: the public kill involving the account's
	// corporation; the private kill and the stranger's stay out.
	body = home(`[{"id":"intelwatch","w":3,"h":3}]`)
	for _, want := range []string{"Recent kills, your corporations", "lost a Rifter in Alpha Watch"} {
		if !strings.Contains(body, want) {
			t.Fatalf("largest size: home missing %q", want)
		}
	}
	for _, unwanted := range []string{"Machariel", "Sabre"} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("largest size: %q showed but its kill is not a public kill of the account's corporations", unwanted)
		}
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("home renders made %d outbound calls, want 0", got)
	}
}

// TestIntelWatchWidgetEmpty: an account watching nothing gets
// the plain empty state and the manage button, and no content.
func TestIntelWatchWidgetEmpty(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	setHomeLayout(t, q, user.ID, `[{"id":"intelwatch","w":2,"h":2}]`)
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")
	code, body := getPage(t, app, cookie, "/")
	if code != 200 {
		t.Fatalf("home: status %d", code)
	}
	for _, want := range []string{"No watched systems yet", "Watched systems</button>", `id="watched-systems-modal"`, "No systems watched yet"} {
		if !strings.Contains(body, want) {
			t.Fatalf("empty state: home missing %q", want)
		}
	}
}
