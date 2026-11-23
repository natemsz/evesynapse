package app

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"evesynapse/internal/gamelog"
)

// TestFleetExplorerDrawsABattleFromItsKillmails: with no log, the
// explorer still shows the battle: the ISK destroyed and lost as it went,
// each pilot's damage on kills and on their loss, the enemy from the
// mails (kill victims, and whoever was on the fleet's losses, a ship with
// no pilot by its type), the weapons the mails list, and every hand on a
// mail with its weapon and the final blow.
func TestFleetExplorerDrawsABattleFromItsKillmails(t *testing.T) {
	const pilotA, pilotB, rival, other = int64(1), int64(2), int64(900), int64(901)
	start := time.Unix(1_800_000_000, 0).UTC()
	on := map[int64]fleetPresence{
		pilotA: {From: start.Add(-time.Hour), To: start.Add(time.Hour)},
		pilotB: {From: start.Add(-time.Hour), To: start.Add(time.Hour)},
	}
	kill := &fleetMail{
		ID: 7, At: start, Victim: rival, Ship: 587, Damage: 1000, VictimCorp: 98000001, VictimAlliance: 99000001,
		Value: sql.NullFloat64{Float64: 100_000_000, Valid: true},
		Hands: []fleetMailHandRow{
			{Character: pilotA, Ship: 11, Weapon: 2410, Damage: 700, FinalBlow: true},
			{Character: pilotB, Ship: 12, Weapon: 2410, Damage: 300},
		},
	}
	loss := &fleetMail{
		ID: 8, At: start.Add(90 * time.Second), Victim: pilotB, Ship: 12, Damage: 2000, Loss: true,
		Value: sql.NullFloat64{Float64: 40_000_000, Valid: true},
		Hands: []fleetMailHandRow{
			{Character: other, Corp: 98000002, Alliance: 99000001, Ship: 13, Weapon: 3001, Damage: 1500, FinalBlow: true},
			{Ship: 14, Damage: 500},
		},
	}
	names := map[int64]string{pilotA: "Pilot A", pilotB: "Pilot B", rival: "Rival", other: "Other"}
	types := map[int64]string{587: "Rifter", 11: "Tengu", 12: "Loki", 13: "Legion", 14: "Sentry Gun", 2410: "Heavy Missile Launcher II", 3001: "Heavy Pulse Laser II"}
	d := buildFleetDetail(fleetDetailInput{
		Cluster: &fleetCluster{From: start, To: loss.At, Mails: []*fleetMail{kill, loss}},
		On:      on, Ships: map[int64]int64{},
		Name: func(id int64) string { return names[id] }, Type: func(id int64) string { return types[id] },
	})

	if d.Logged != 0 || len(d.IskKill) == 0 {
		t.Fatalf("logged %d, ISK series %d steps: want a killmail-only battle with its ISK series", d.Logged, len(d.IskKill))
	}
	if last := len(d.IskKill) - 1; d.IskKill[last] != 100_000_000 || d.IskLoss[last] != 40_000_000 {
		t.Errorf("ISK so far at the end: destroyed %d, lost %d", d.IskKill[last], d.IskLoss[last])
	}
	pilots := map[int64]fleetDetailPilot{}
	for _, p := range d.Pilots {
		pilots[p.ID] = p
	}
	if a := pilots[pilotA]; a.Out != 700 || a.OnMails != 1 || !a.FromMails {
		t.Errorf("pilot A from the mails: %+v, want 700 on 1 kill", a)
	}
	if b := pilots[pilotB]; b.Out != 300 || b.In != 2000 || !b.Died || b.LossMail != 8 {
		t.Errorf("pilot B from the mails: %+v, want 300 dealt, 2000 on its loss, died on mail 8", b)
	}
	enemies := map[string]fleetDetailParty{}
	for _, e := range d.Enemies {
		enemies[e.Name] = e
	}
	if e := enemies["Rival"]; !e.Died || e.KillMail != 7 || e.FromUs != 1000 || !e.FromMails || e.AllianceID != 99000001 {
		t.Errorf("the kill's victim: %+v, want died on mail 7, 1000 from the fleet, its alliance", e)
	}
	if e := enemies["Other"]; e.ToUs != 1500 || !e.FromMails || e.CorpID != 98000002 || e.HullTypeID != 13 {
		t.Errorf("an attacker on the loss: %+v, want 1500 on the fleet, its corporation and hull", e)
	}
	if e := enemies["Sentry Gun"]; e.ToUs != 500 || e.Pilot {
		t.Errorf("a ship with no pilot on the loss: %+v, want named by its type, 500 on the fleet", e)
	}
	if !d.WeaponsFromMails || len(d.WeaponsOut) != 1 || d.WeaponsOut[0].TypeID != 2410 || d.WeaponsOut[0].Damage != 1000 || d.WeaponsOut[0].Users != 2 {
		t.Errorf("the fleet's weapons from the mails: %+v", d.WeaponsOut)
	}
	if len(d.WeaponsIn) != 1 || d.WeaponsIn[0].TypeID != 3001 || d.WeaponsIn[0].Damage != 1500 {
		t.Errorf("the enemy's weapons from the mails: %+v", d.WeaponsIn)
	}
	var sawKill, sawLoss bool
	for _, m := range d.Moments {
		switch m.Kind {
		case momentKill:
			sawKill = true
			if m.Detail.OnMail != 2 || !strings.Contains(m.Detail.Summary, "2 of the fleet's pilots are on it") {
				t.Errorf("the kill's moment: %d on the mail, %q", m.Detail.OnMail, m.Detail.Summary)
			}
		case momentLoss:
			sawLoss = true
			h := m.Detail.Attackers
			if len(h) != 2 || h[0].Character != other || h[0].Ours || h[0].Weapon != 3001 || !h[0].FinalBlow || h[0].Pct != 75 {
				t.Errorf("the loss's hands: %+v, want Other first with its weapon, the final blow and 75%%", h)
			}
			if !m.Detail.FromMails || !strings.Contains(m.Detail.Summary, "did the most damage, 75%") {
				t.Errorf("the loss's window: from mails %v, %q", m.Detail.FromMails, m.Detail.Summary)
			}
		}
	}
	if !sawKill || !sawLoss {
		t.Errorf("moments %+v, want the kill and the loss", d.Moments)
	}
}

// TestFleetExplorerKeepsLogFiguresOverKillmails: an enemy or pilot a log
// saw keeps the log's figures, whatever the killmails credit; the mails
// add who died, and fill in only what no log saw.
func TestFleetExplorerKeepsLogFiguresOverKillmails(t *testing.T) {
	const pilotA, pilotB, rival, other = int64(1), int64(2), int64(999), int64(901)
	start := time.Unix(1_800_000_000, 0).UTC()
	step := gamelog.ClockStep(start.Unix())
	end := start.Add(9 * time.Second)
	enemy := fleetLogParty{Name: "Rival", Hull: "Rifter", HullTypeID: 587, CharacterID: rival, Pilot: true}
	reads := []*fleetLogRead{{
		ID: 10, Character: pilotA, Start: start, End: end, Received: start.Add(time.Hour), Usable: true,
		Fleetmates: map[int64]int64{}, Parties: map[int64]fleetLogParty{0: enemy},
		Steps: []gamelog.AlignedSlice{{At: step, Rows: [][7]int64{{0, 100, 40, 0, 0, 0, 0}}}},
	}}
	on := map[int64]fleetPresence{
		pilotA: {From: start.Add(-time.Minute), To: end.Add(time.Minute)},
		pilotB: {From: start.Add(-time.Minute), To: end.Add(time.Minute)},
	}
	kill := &fleetMail{ID: 7, At: start, Victim: rival, Ship: 587, Damage: 5000,
		Hands: []fleetMailHandRow{{Character: pilotA, Damage: 5000}}}
	loss := &fleetMail{ID: 8, At: start.Add(5 * time.Second), Victim: pilotB, Ship: 12, Damage: 800, Loss: true,
		Hands: []fleetMailHandRow{{Character: other, Ship: 13, Damage: 800}}}
	counted := countedByRead(reads)
	c := clusterFleetBattles([]*fleetMail{kill, loss}, reads, on)
	if len(c) != 1 {
		t.Fatalf("%d battles, want 1", len(c))
	}
	d := buildFleetDetail(fleetDetailInput{
		Cluster: c[0], Counted: counted, On: on, Ships: map[int64]int64{},
		Merge: mergeFleetLogs(c[0].Reads, counted, on),
		Name:  func(id int64) string { return fmt.Sprintf("Pilot %d", id) },
	})
	if d.Logged != 1 || d.IskKill != nil {
		t.Errorf("logged %d, ISK series %v: a battle with a log charts the log", d.Logged, d.IskKill)
	}
	for _, e := range d.Enemies {
		switch e.CharacterID {
		case rival:
			if e.FromUs != 100 || e.ToUs != 40 || e.FromMails || !e.Died || e.KillMail != 7 {
				t.Errorf("the enemy the log saw: %+v, want the log's 100 and 40, killed on mail 7", e)
			}
		case other:
			if e.ToUs != 800 || !e.FromMails {
				t.Errorf("the enemy only a mail saw: %+v, want 800 from the mail", e)
			}
		}
	}
	for _, p := range d.Pilots {
		switch p.ID {
		case pilotA:
			if p.Out != 100 || !p.HasLog || p.FromMails {
				t.Errorf("the pilot with a log: %+v, want the log's 100", p)
			}
		case pilotB:
			if p.In != 800 || !p.FromMails || !p.Died {
				t.Errorf("the pilot with no log: %+v, want 800 on its loss from the mail", p)
			}
		}
	}
}

// TestKillmailLinksAreReadFromPastedText: external kill links (with the
// hash), zKillboard links, this board's links and bare ids are read,
// each kill once, at most ten; anything else counts as unread.
func TestKillmailLinksAreReadFromPastedText(t *testing.T) {
	hash := "0123456789abcdef0123456789ABCDEF01234567"
	refs, bad := parseKillmailLinks("https://esi.evetech.net/latest/killmails/123456789/" + hash + "/\n" +
		"https://zkillboard.com/kill/123456790/ junk 123456789\n<https://evesynapse.app/intel/kill/123456791/>")
	want := []killmailRef{{123456789, strings.ToLower(hash)}, {123456790, ""}, {123456791, ""}}
	if fmt.Sprint(refs) != fmt.Sprint(want) || bad != 1 {
		t.Errorf("read %v with %d unread, want %v with 1", refs, bad, want)
	}
	var many []string
	for i := 0; i < 14; i++ {
		many = append(many, fmt.Sprint(200000000+i))
	}
	if refs, _ := parseKillmailLinks(strings.Join(many, " ")); len(refs) != fleetAddMailsMax {
		t.Errorf("read %d of 14 ids, want %d", len(refs), fleetAddMailsMax)
	}
}

// TestFleetReportReadsTheBoardsOwnKillsAndTakesPastedLinks: a kill only
// our members' ESI brought in (private, not on zKillboard) counts on the
// fleet's report, with no killboard link until it is public; a member
// pasting a kill the board holds queues the report to rebuild; an
// outsider cannot post to it.
func TestFleetReportReadsTheBoardsOwnKillsAndTakesPastedLinks(t *testing.T) {
	f := newOpsFixture(t)
	id := f.create(time.Now().UTC().Add(48*time.Hour).Truncate(time.Minute), "Night roam")
	start := time.Now().UTC().Add(-4 * time.Hour).Truncate(time.Minute)
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := f.app.db.ExecContext(f.ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE ops SET starts_at = $1 WHERE id = $2`, start, id)
	for _, pilot := range []int64{fixtureCharA, opsCharC} {
		exec(`INSERT INTO op_attendance (op_id, character_id, ship_type_id, source, first_seen_at, last_seen_at)
		      VALUES ($1, $2, 587, 'fleet', $3, $4)`, id, pilot, start, start.Add(40*time.Minute))
	}
	exec(`INSERT INTO kills (killmail_id, hash, killmail_time, system_id, victim_character_id, victim_corporation_id, ship_type_id, damage_taken, source, public, ingested_at, value_ours)
	      VALUES (7001, 'h', $1, 30000142, 95000001, 98009999, 587, 1000, 'esi', FALSE, $1, 200000000)`, start.Add(10*time.Minute))
	exec(`INSERT INTO kill_attackers (killmail_id, seq, character_id, corporation_id, ship_type_id, weapon_type_id, damage_done, final_blow)
	      VALUES (7001, 0, $1, $2, 587, 2410, 1000, TRUE)`, opsCharC, opsCorp)
	f.app.refreshFleetReports(f.ctx)

	var held int
	if err := f.app.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM fleet_battle_kills k JOIN fleet_battles b ON b.id = k.fleet_battle_id WHERE b.op_id = $1 AND k.killmail_id = 7001`, id).Scan(&held); err != nil || held != 1 {
		t.Fatalf("the board's own private kill on the report: %d (%v), want 1", held, err)
	}
	report := fleetReportPath(id)
	if code, _, body := f.get(f.member, report); code != http.StatusOK || strings.Contains(body, "/intel/kill/7001/") {
		t.Errorf("a private kill links to the killboard: %d", code)
	}
	exec(`UPDATE kills SET public = TRUE WHERE killmail_id = 7001`)
	if _, _, body := f.get(f.member, report); !strings.Contains(body, "/intel/kill/7001/") {
		t.Error("a kill zKillboard has posted does not link to the killboard")
	}

	exec(`UPDATE fleet_report_state SET dirty_seq = NULL WHERE op_id = $1`, id)
	links := url.Values{"links": {"https://zkillboard.com/kill/7001/"}}
	if code, where := f.post(f.other, report+"/killmails", links); code == http.StatusSeeOther {
		t.Errorf("an outsider posted to the report: %d to %q", code, where)
	}
	var queued bool
	dirty := func() bool {
		t.Helper()
		if err := f.app.db.QueryRowContext(f.ctx, `SELECT dirty_seq IS NOT NULL FROM fleet_report_state WHERE op_id = $1`, id).Scan(&queued); err != nil {
			t.Fatal(err)
		}
		return queued
	}
	if dirty() {
		t.Fatal("an outsider's post queued the report")
	}
	if code, where := f.post(f.member, report+"/killmails", links); code != http.StatusSeeOther || !strings.HasPrefix(where, report) {
		t.Fatalf("a member's post: %d to %q", code, where)
	}
	if !dirty() {
		t.Error("pasting a kill the board holds did not queue the report")
	}
	if f.calls.calls.Load() != 0 {
		t.Errorf("a kill the board holds was fetched again: %d calls out", f.calls.calls.Load())
	}
}
