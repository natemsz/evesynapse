package app

// The system page's enrichment: its most recent kills with the
// gate camp flag, and the intel reports about it that the reader
// may see. The merge test pins how kill sources fold together
// before any first-party source exists: dedupe by killmail, the
// first-party row winning a duplicate.

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
)

func TestMergeSystemKills(t *testing.T) {
	at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	stream := []systemKill{
		{kill: db.Kill{KillmailID: 3, KillmailTime: at.Add(-3 * time.Minute), ShipTypeID: 587}},
		{kill: db.Kill{KillmailID: 2, KillmailTime: at.Add(-2 * time.Minute), ShipTypeID: 587}},
		{kill: db.Kill{KillmailID: 1, KillmailTime: at.Add(-time.Minute), ShipTypeID: 587}},
	}
	own := []systemKill{
		{kill: db.Kill{KillmailID: 2, KillmailTime: at.Add(-2 * time.Minute), ShipTypeID: 22456}, firstParty: true},
		{kill: db.Kill{KillmailID: 4, KillmailTime: at.Add(-4 * time.Minute), ShipTypeID: 22456}, firstParty: true},
	}
	got := mergeSystemKills(5, stream, own)
	if len(got) != 4 {
		t.Fatalf("merged = %d kills, want 4 (one duplicate folded)", len(got))
	}
	wantIDs := []int64{1, 2, 3, 4} // newest first
	for i, id := range wantIDs {
		if got[i].KillmailID != id {
			t.Fatalf("merged[%d] = kill %d, want %d (newest first)", i, got[i].KillmailID, id)
		}
	}
	if got[1].ShipTypeID != 22456 {
		t.Errorf("the duplicate kept ship type %d, want the first-party row's 22456", got[1].ShipTypeID)
	}
	// Between rows of the same standing the first source keeps
	// the kill, and the limit trims the oldest.
	got = mergeSystemKills(2, stream, []systemKill{{kill: db.Kill{KillmailID: 1, KillmailTime: at.Add(-time.Minute), ShipTypeID: 999}}})
	if len(got) != 2 || got[0].KillmailID != 1 || got[0].ShipTypeID != 587 || got[1].KillmailID != 2 {
		t.Errorf("same-standing merge = %+v, want the stream's rows for kills 1 and 2", got)
	}
	if got := mergeSystemKills(5); len(got) != 0 {
		t.Errorf("no sources merged to %d kills, want none", len(got))
	}
}

// TestSystemPageKillsAndCamp renders the page for a system with
// a camp running: the flag in the alert style with its detail on
// the mouseover, the five most recent public kills linked to
// their kill pages, and nothing private.
func TestSystemPageKillsAndCamp(t *testing.T) {
	app, conn, _ := buildCorpTestApp(t, nil)
	ctx := context.Background()
	exec := func(s string, args ...any) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, s, args...); err != nil {
			t.Fatal(err)
		}
	}
	const rancer = int64(30003504)
	now := time.Now().UTC()
	exec(`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30003504, 'Rancer', 10000048, 0.4) ON CONFLICT DO NOTHING`)
	exec(`INSERT INTO sde_types (type_id, name) VALUES (22456, 'Sabre'), (587, 'Rifter'), (17738, 'Machariel') ON CONFLICT DO NOTHING`)
	exec(`INSERT INTO sde_celestials (item_id, system_id, name) VALUES (50000001, 30003504, 'Stargate (Otou)') ON CONFLICT DO NOTHING`)
	kill := func(id int64, ago time.Duration, victimCorp int64, public bool, alliance int64, loc int64, attackers ...int64) {
		t.Helper()
		exec(`INSERT INTO kills (killmail_id, killmail_time, system_id, victim_character_id, victim_corporation_id, ship_type_id, location_id, source, public, ingested_at, value_zkill)
			VALUES ($1, $2, $3, $4, $5, 587, $7, 'stream', $6, now(), 10000000)`, id, now.Add(-ago), rancer, 92000000+id, victimCorp, public, loc)
		for i, a := range attackers {
			ship := int64(22456)
			if i == 1 {
				ship = 17738
			}
			exec(`INSERT INTO kill_attackers (killmail_id, seq, character_id, corporation_id, alliance_id, ship_type_id, final_blow) VALUES ($1, $2, $3, 98000001, $4, $5, $6)`,
				id, i, a, alliance, ship, i == 0)
		}
	}
	// The TestMapSystem camp, plus two older public kills the
	// five-row list must trim, and one private kill. The camp's
	// kills sit on the Otou gate.
	kill(10, 5*time.Minute, 1, true, 99000001, 50000001, 91000001, 91000002)
	kill(11, 15*time.Minute, 2, true, 99000001, 50000001, 91000001)
	kill(12, 25*time.Minute, 3, true, 99000001, 50000001, 91000003)
	kill(13, 35*time.Minute, 4, true, 99000002, 50000002, 91000009)
	kill(14, 40*time.Minute, 5, true, 99000001, 50000001, 91000001)
	kill(16, 2*time.Hour, 7, true, 99000003, 0, 91000020)
	kill(17, 3*time.Hour, 8, true, 99000003, 0, 91000021)
	kill(15, 2*time.Minute, 6, false, 99000001, 50000001, 91000001)
	// The camp's group has a name, and so does its recurring pilot.
	app.esi.StoreAllianceName(99000001, "Gatekeepers")
	app.esi.StoreCharacterName(91000001, "Camp Regular")

	anon := &http.Cookie{Name: "nobody", Value: "x"}
	code, body := getPage(t, app, anon, "/system/?system=30003504")
	if code != http.StatusOK {
		t.Fatalf("GET /system/: status %d", code)
	}
	mustContain(t, "/system/ camp", body,
		"Recent kills",
		`<p class="err" title="Gate camp by Gatekeepers on the Stargate (Otou):`,
		"4 kills in the last hour",
		"Flying Sabre ×",
		"Recurring crew: Camp Regular.",
		"Gate camp",
		" · on the Stargate (Otou)</p>",
		`<a href="/intel/kill/10/">Rifter</a>`,
		`<a href="/intel/kill/14/">Rifter</a>`,
		`<a href="/intel/kills/?system=30003504">All kills in Rancer</a>`)
	// Five rows only: the two oldest public kills are trimmed,
	// and the private kill never shows.
	for _, trimmed := range []string{`/intel/kill/16/`, `/intel/kill/17/`, `/intel/kill/15/`} {
		if strings.Contains(body, trimmed) {
			t.Errorf("the page links %s, which its five recent kills must not include", trimmed)
		}
	}
	// The map link: the header's action, with its glyph, deep
	// linking to the map focused on this system.
	mustContain(t, "/system/ map link", body,
		`<a class="h-action" href="/map/?system=30003504" title="Open Rancer on the universe map"><svg`)

	// A quiet system carries no kills section at all.
	exec(`INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30000142, 'Jita', 10000002, 0.946) ON CONFLICT DO NOTHING`)
	_, body = getPage(t, app, anon, "/system/?system=30000142")
	if strings.Contains(body, "Recent kills") {
		t.Error("a system without kills grew a kills section")
	}
}

// TestSystemPageIntelReports: a system page lists the reports
// about it from the channels its reader can access -- the owner
// and a granted corp mate see them; a stranger and a signed-out
// reader do not.
func TestSystemPageIntelReports(t *testing.T) {
	app, conn, q := buildCorpTestApp(t, nil)
	ctx := context.Background()
	if _, err := conn.ExecContext(ctx, `INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30000142, 'Jita', 10000002, 0.946) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	const corp = int64(98000101)
	member := func(charID int64, name string, corpID int64) (int64, *http.Cookie) {
		u, err := q.CreateUser(ctx)
		if err != nil {
			t.Fatal(err)
		}
		seedCharacter(t, q, u.ID, charID, name)
		if err := q.UpsertCharacterCorporation(ctx, db.UpsertCharacterCorporationParams{CharacterID: charID, CorporationID: corpID, UpdatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		return u.ID, sessionCookie(t, app, u.ID, charID, name)
	}
	ownerID, owner := member(90000201, "Channel Owner", corp)
	_, mate := member(90000202, "Corp Mate", corp)
	_, stranger := member(90000203, "Other Pilot", 98000102)

	chID, err := q.CreateIntelChannel(ctx, db.CreateIntelChannelParams{
		OwnerUserID: ownerID, Name: "Home intel", CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	if _, err := q.InsertIntelReport(ctx, db.InsertIntelReportParams{
		ChannelID: chID, SystemID: 30000142, AuthorUserID: ownerID, AuthorCharacterID: 90000201,
		Reporter: "Channel Owner", Text: "Bad Guy", Hostiles: 3, Source: "form",
		SeenAt: time.Now().UTC(), CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("insert report: %v", err)
	}

	path := "/system/?system=30000142"
	if _, body := getPage(t, app, owner, path); !strings.Contains(body, "Intel reports") || !strings.Contains(body, "Bad Guy") ||
		!strings.Contains(body, `<a href="/intel/channels/`+strconv.FormatInt(chID, 10)+`/">Home intel</a>`) {
		t.Error("the channel's owner does not see its report on the system page")
	}
	anon := &http.Cookie{Name: "nobody", Value: "x"}
	if _, body := getPage(t, app, anon, path); strings.Contains(body, "Bad Guy") {
		t.Error("a signed-out reader sees restricted intel")
	}
	if _, body := getPage(t, app, stranger, path); strings.Contains(body, "Bad Guy") {
		t.Error("a reader without a grant sees restricted intel")
	}
	if err := q.UpsertIntelChannelGrant(ctx, db.UpsertIntelChannelGrantParams{
		ChannelID: chID, Kind: "corporation", TargetID: corp, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, body := getPage(t, app, mate, path); !strings.Contains(body, "Bad Guy") {
		t.Error("a granted corp mate does not see the report on the system page")
	}
}
