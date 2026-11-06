package app

// Battle reports for the demo account (demo_seed.go has the
// seed itself). The seed plants Sable Iversen's killmails as
// data; this step gives three of those fights their other half:
// a game log each, fabricated from the mails themselves, read
// back through the real gamelog parser and filed through the
// real ingest path, so the demo's Battles page shows reports
// the normal pipeline built, kills joined to mails by the
// normal correlation.
//
// The fights: her solo kill on Ilya Sorin and her Thrasher
// loss, both already planted by writePilotOneKillmails, and
// one Tama gang fight whose three mails that step plants for
// this. Every pilot named here is in the seed's cast already
// (writeNames covers them); no new names are invented.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"evesynapse/internal/esi"
	"evesynapse/internal/gamelog"
)

// The killmails the demo's battle reports are built from: the
// two writePilotOneKillmails has always planted, and the gang
// fight's three. The wipe clears these IDs from the shared
// kill tables (see queries_demo.sql) because a stored kill
// keeps its first copy while the seed replants its details
// with fresh times.
const (
	demoBattleLossMail = int64(2900007001)
	demoBattleKillMail = int64(2900007002)
)

var demoGangMailIDs = []int64{2900007011, 2900007012, 2900007013}

var demoBattleMailIDs = []int64{
	demoBattleLossMail, demoBattleKillMail,
	2900007011, 2900007012, 2900007013,
}

// demoLogWeapons is the module a game log line names for a
// hull: what that hull shoots with in the demo's stories.
var demoLogWeapons = map[string]string{
	"Rifter":   "200mm AutoCannon II",
	"Thrasher": "200mm AutoCannon II",
	"Merlin":   "Light Neutron Blaster II",
	"Punisher": "Dual Light Pulse Laser II",
	"Tristan":  "Hobgoblin II",
	"Vexor":    "Hammerhead II",
	"Catalyst": "Light Neutron Blaster II",
	"Atron":    "Light Ion Blaster II",
	"Drake":    "Heavy Missile Launcher II",
}

// demoQualities is the hit quality words a log cycles through,
// best saved for a killing shot.
var demoQualities = []string{"Hits", "Penetrates", "Hits", "Smashes", "Hits", "Grazes", "Glances off", "Hits"}

// writePilotOneBattles files the demo's battle reports. It
// runs after writePilotOneKillmails, whose details it reads
// back: the story a log tells is the mails' own, never a
// second telling that could drift from them.
func (s *demoSeed) writePilotOneBattles(ctx context.Context) error {
	q := s.app.queries
	rows, err := q.ListKillmailDetailsByCharacter(ctx, demoCharacterID)
	if err != nil {
		return fmt.Errorf("demo seed: list killmail details for the battle reports: %w", err)
	}
	// Correlation joins a fight to the shared kills table. A
	// real pilot's details reach it through the worker's
	// killboard merge; the demo pilots never warm, so the seed
	// merges Sable's itself, through the same ingest.
	mails := map[int64]esi.Killmail{}
	for _, r := range rows {
		if _, err := s.app.ingestKill(ctx, json.RawMessage(r.Payload), r.Hash, nil, "esi", false, false); err != nil {
			return fmt.Errorf("demo seed: merge killmail %d for the battle reports: %w", r.KillmailID, err)
		}
		var km esi.Killmail
		if err := json.Unmarshal([]byte(r.Payload), &km); err != nil {
			return fmt.Errorf("demo seed: decode killmail %d for the battle reports: %w", r.KillmailID, err)
		}
		mails[km.KillmailID] = km
	}

	smith := newDemoLogSmith(s)
	var logs []demoLogText
	if km, ok := mails[demoBattleKillMail]; ok {
		if log, ok := smith.winLog(km); ok {
			logs = append(logs, log)
		}
	}
	if km, ok := mails[demoBattleLossMail]; ok {
		if log, ok := smith.lossLog(km); ok {
			logs = append(logs, log)
		}
	}
	gang := make([]esi.Killmail, 0, len(demoGangMailIDs))
	for _, id := range demoGangMailIDs {
		if km, ok := mails[id]; ok {
			gang = append(gang, km)
		}
	}
	if len(gang) == len(demoGangMailIDs) {
		if log, ok := smith.gangLog(gang); ok {
			logs = append(logs, log)
		}
	}
	for _, log := range logs {
		l, err := gamelog.Read(strings.NewReader(log.text), log.session)
		if err != nil {
			return fmt.Errorf("demo seed: read a fabricated game log: %w", err)
		}
		got, err := s.app.ingestGameLog(ctx, s.userID, demoCharacterID, demoCharName, l)
		if err != nil {
			return fmt.Errorf("demo seed: file a fabricated game log: %w", err)
		}
		if len(got.IDs) == 0 {
			return fmt.Errorf("demo seed: a fabricated game log filed no fights: %s", strings.Join(got.Skipped, " "))
		}
	}
	return nil
}

// demoLogText is one fabricated game log and the session file
// name it would have had.
type demoLogText struct {
	text    string
	session string
}

// demoLogSmith builds game log text: it knows the demo cast's
// names, corp tickers and hull names, all resolved from the
// seed's own data.
type demoLogSmith struct {
	typesByID map[int64]string
	chars     map[int64]string
	tickers   map[int64]string
}

func newDemoLogSmith(s *demoSeed) *demoLogSmith {
	sm := &demoLogSmith{
		typesByID: map[int64]string{},
		chars:     map[int64]string{demoCharacterID: demoCharName, demoCharacter2ID: demoChar2Name},
		tickers:   map[int64]string{demoCorpID: "TPC", demoCorpHaleID: "HALE", demoCorpWatchID: "KWTCH"},
	}
	for name, id := range s.types {
		if id > 0 {
			if _, taken := sm.typesByID[id]; !taken {
				sm.typesByID[id] = name
			}
		}
	}
	for _, c := range demoFictionalChars {
		sm.chars[c.ID] = c.Name
	}
	return sm
}

// demoParty is one pilot as a game log names them.
type demoParty struct {
	name   string // "Ilya Sorin"
	suffix string // "[HALE](Punisher)"
	hull   string // "Punisher"
}

func (p demoParty) full() string { return p.name + p.suffix }

// partyFor resolves a killmail side to how the log names it;
// false when this install's data cannot name the pilot or the
// hull, and the caller skips what depends on it.
func (sm *demoLogSmith) partyFor(charID, corpID, shipID int64) (demoParty, bool) {
	name, hull := sm.chars[charID], sm.typesByID[shipID]
	if name == "" || hull == "" {
		return demoParty{}, false
	}
	suffix := "(" + hull + ")"
	if tick := sm.tickers[corpID]; tick != "" {
		suffix = "[" + tick + "]" + suffix
	}
	return demoParty{name: name, suffix: suffix, hull: hull}, true
}

func (sm *demoLogSmith) weapon(hull string) string {
	if w := demoLogWeapons[hull]; w != "" {
		return w
	}
	return "200mm AutoCannon II"
}

// demoAttacker finds a character's entry on a mail.
func demoAttacker(km esi.Killmail, charID int64) (esi.KillmailAttacker, bool) {
	for _, a := range km.Attackers {
		if a.CharacterID == charID {
			return a, true
		}
	}
	return esi.KillmailAttacker{}, false
}

func demoMailTime(km esi.Killmail) (time.Time, bool) {
	at, err := time.Parse(time.RFC3339, km.KillmailTime)
	return at, err == nil
}

// demoLogLine is one log line at an offset from the fight's
// first line.
type demoLogLine struct {
	at   time.Duration
	text string
}

// demoShot is one dealt hit before it is rendered: when, and
// for how much.
type demoShot struct {
	at     time.Duration
	amount int64
}

// demoDeal splits total damage into shots between two offsets:
// the first at from, then one every step, the amounts jittered
// around the average the total and the count imply, and a last
// shot at to carrying what remains, so the shots sum to the
// total exactly.
func demoDeal(seed, total int64, from, to, step time.Duration) []demoShot {
	if total <= 0 || to <= from || step <= 0 {
		return nil
	}
	n := int((to-from)/step) + 1
	avg := float64(total) / float64(n)
	reserve := int64(avg * 0.35)
	var shots []demoShot
	left := total
	for i := 0; i < n-1; i++ {
		amt := int64(avg * (0.55 + 0.9*demoJitter(seed+int64(i))))
		if amt < 1 {
			amt = 1
		}
		if left-amt < reserve {
			amt = left - reserve
		}
		if amt < 1 {
			break
		}
		shots = append(shots, demoShot{at: from + time.Duration(i)*step, amount: amt})
		left -= amt
	}
	if left > 0 {
		shots = append(shots, demoShot{at: to, amount: left})
	}
	return shots
}

// demoDamage renders dealt shots as damage lines between the
// listener and one party: out to the party, or in from them.
// The last shot of a stream that ends in a kill is a wrecking
// one when lastQuality says so.
func demoDamage(shots []demoShot, out bool, party demoParty, weapon string, lastQuality string) []demoLogLine {
	lines := make([]demoLogLine, 0, len(shots))
	for i, sh := range shots {
		quality := demoQualities[i%len(demoQualities)]
		if i == len(shots)-1 && lastQuality != "" {
			quality = lastQuality
		}
		lines = append(lines, demoLogLine{at: sh.at, text: demoDamageText(out, sh.amount, party, weapon, quality)})
	}
	return lines
}

func demoDamageText(out bool, amount int64, party demoParty, weapon, quality string) string {
	if out {
		return fmt.Sprintf("(combat) <color=0xff00ffff><b>%d</b> <color=0x77ffffff><font size=10>to</font> <b><color=0xffffffff>%s</b><font size=10><color=0x77ffffff> - %s - %s",
			amount, party.full(), weapon, quality)
	}
	return fmt.Sprintf("(combat) <color=0xffcc0000><b>%d</b> <color=0x77ffffff><font size=10>from</font> <b><color=0xffffffff>%s</b><font size=10><color=0x77ffffff> - %s - %s",
		amount, party.full(), weapon, quality)
}

func demoMissOut(at time.Duration, party demoParty, weapon string) demoLogLine {
	return demoLogLine{at: at, text: fmt.Sprintf("(combat) Your %s misses %s completely", weapon, party.full())}
}

func demoMissIn(at time.Duration, party demoParty, weapon string) demoLogLine {
	return demoLogLine{at: at, text: fmt.Sprintf("(combat) %s misses you completely - %s", party.full(), weapon)}
}

// demoTackleOnMe is a scramble or disruption attempt on the
// listener, as the client notifies it.
func demoTackleOnMe(at time.Duration, kind string, party demoParty) demoLogLine {
	return demoLogLine{at: at, text: fmt.Sprintf("(notify) Warp %s attempt from <b>%s</b>%s to <b>you!</b>", kind, party.name, party.suffix)}
}

// demoTackleByMe is the listener's own attempt on a party.
func demoTackleByMe(at time.Duration, kind string, party demoParty) demoLogLine {
	return demoLogLine{at: at, text: fmt.Sprintf("(notify) Warp %s attempt from <b>you</b> to <b>%s</b>%s!", kind, party.name, party.suffix)}
}

// render lays the lines out as a game log: the header the
// client writes, then the lines in time order.
func (sm *demoLogSmith) render(start time.Time, lines []demoLogLine) demoLogText {
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].at < lines[j].at })
	var b strings.Builder
	b.WriteString("------------------------------------------------------------\r\n")
	b.WriteString("  Gamelog\r\n")
	b.WriteString("  Listener: " + demoCharName + "\r\n")
	b.WriteString("  Session Started: " + start.Format("2006.01.02 15:04:05") + "\r\n")
	b.WriteString("------------------------------------------------------------\r\n")
	for _, ln := range lines {
		fmt.Fprintf(&b, "[ %s ] %s\r\n", start.Add(ln.at).Format("2006.01.02 15:04:05"), ln.text)
	}
	return demoLogText{
		text:    b.String(),
		session: start.Format("20060102_150405") + fmt.Sprintf("_%d.txt", demoCharacterID),
	}
}

// winLog tells the planted solo kill: Sable's Rifter against
// Ilya Sorin's Punisher in Tama, her damage the mail's own
// figure for her.
func (sm *demoLogSmith) winLog(km esi.Killmail) (demoLogText, bool) {
	killAt, ok := demoMailTime(km)
	if !ok {
		return demoLogText{}, false
	}
	victim, ok := sm.partyFor(km.Victim.CharacterID, km.Victim.CorporationID, km.Victim.ShipTypeID)
	if !ok {
		return demoLogText{}, false
	}
	mine, ok := demoAttacker(km, demoCharacterID)
	if !ok {
		return demoLogText{}, false
	}
	myWeapon := sm.weapon(sm.typesByID[mine.ShipTypeID])
	start := killAt.Add(-110 * time.Second)
	sec := time.Second
	var lines []demoLogLine
	lines = append(lines, demoTackleByMe(2*sec, "scramble", victim))
	lines = append(lines, demoDamage(
		demoDeal(km.KillmailID, mine.DamageDone, 4*sec, 110*sec, 2*sec),
		true, victim, myWeapon, "Wrecks")...)
	lines = append(lines, demoDamage(
		demoDeal(km.KillmailID+1, 1500, 7*sec, 100*sec, 3*sec),
		false, victim, sm.weapon(victim.hull), "")...)
	lines = append(lines,
		demoMissOut(21*sec, victim, myWeapon),
		demoMissIn(33*sec, victim, sm.weapon(victim.hull)),
		demoMissOut(57*sec, victim, myWeapon),
	)
	return sm.render(start, lines), true
}

// lossLog tells the planted loss: Ilya Sorin's Merlin and
// Kato Vire's Tristan killing Sable's Thrasher in Tama, the
// incoming damage the mail's own figures for each of them,
// the killing shot Ilya's.
func (sm *demoLogSmith) lossLog(km esi.Killmail) (demoLogText, bool) {
	killAt, ok := demoMailTime(km)
	if !ok || km.Victim.CharacterID != demoCharacterID {
		return demoLogText{}, false
	}
	sec := time.Second
	start := killAt.Add(-175 * sec)
	myWeapon := sm.weapon(sm.typesByID[km.Victim.ShipTypeID])
	var lines []demoLogLine
	for _, a := range km.Attackers {
		party, ok := sm.partyFor(a.CharacterID, a.CorporationID, a.ShipTypeID)
		if !ok {
			continue
		}
		if a.FinalBlow {
			lines = append(lines, demoTackleOnMe(4*sec, "scramble", party))
			lines = append(lines, demoDamage(
				demoDeal(km.KillmailID, a.DamageDone, 2*sec, 175*sec, 3*sec),
				false, party, sm.weapon(party.hull), "Smashes")...)
			lines = append(lines, demoMissIn(66*sec, party, sm.weapon(party.hull)))
			continue
		}
		lines = append(lines, demoTackleOnMe(34*sec, "disruption", party))
		lines = append(lines, demoTackleOnMe(92*sec, "scramble", party))
		lines = append(lines, demoDamage(
			demoDeal(km.KillmailID+1, a.DamageDone, 30*sec, 168*sec, 4*sec),
			false, party, sm.weapon(party.hull), "")...)
		// Sable shoots back at the second attacker, not enough.
		lines = append(lines, demoDamage(
			demoDeal(km.KillmailID+2, 2350, 10*sec, 140*sec, 2*sec),
			true, party, myWeapon, "")...)
		lines = append(lines, demoMissOut(48*sec, party, myWeapon))
	}
	if len(lines) == 0 {
		return demoLogText{}, false
	}
	return sm.render(start, lines), true
}

// gangLog tells the Tama gang fight: Sable's corp against the
// Kedama Watch, three kills inside six minutes, her damage on
// each mail the mail's own figure for her, and each victim
// shooting back until they die.
func (sm *demoLogSmith) gangLog(kms []esi.Killmail) (demoLogText, bool) {
	sort.Slice(kms, func(i, j int) bool { return kms[i].KillmailTime < kms[j].KillmailTime })
	first, ok := demoMailTime(kms[0])
	if !ok {
		return demoLogText{}, false
	}
	sec := time.Second
	start := first.Add(-90 * sec)
	at := func(km esi.Killmail) time.Duration {
		t, _ := demoMailTime(km)
		return t.Sub(start)
	}
	var lines []demoLogLine
	prev := time.Duration(0)
	for i, km := range kms {
		victim, ok := sm.partyFor(km.Victim.CharacterID, km.Victim.CorporationID, km.Victim.ShipTypeID)
		if !ok {
			return demoLogText{}, false
		}
		mine, ok := demoAttacker(km, demoCharacterID)
		if !ok {
			return demoLogText{}, false
		}
		killAt := at(km)
		from := prev - 15*sec
		if i == 0 {
			from = 5 * sec
		}
		if from < 2*sec {
			from = 2 * sec
		}
		lines = append(lines, demoDamage(
			demoDeal(km.KillmailID, mine.DamageDone, from, killAt, 2*sec),
			true, victim, sm.weapon(sm.typesByID[mine.ShipTypeID]), "Wrecks")...)
		// The victim shoots back for most of their remaining
		// time, for a share of what a frigate or a cruiser
		// throws at one target in a gang fight.
		back := []int64{640, 890, 2650}[i]
		backFrom := from + 5*sec
		backTo := killAt - 12*sec
		if backTo > backFrom {
			step := 3 * sec
			if i == 2 {
				step = 4 * sec
			}
			lines = append(lines, demoDamage(
				demoDeal(km.KillmailID+7, back, backFrom, backTo, step),
				false, victim, sm.weapon(victim.hull), "")...)
		}
		if i == 0 {
			lines = append(lines, demoTackleOnMe(12*sec, "scramble", victim))
		}
		if i == 1 {
			lines = append(lines, demoMissOut(from+45*sec, victim, sm.weapon(sm.typesByID[mine.ShipTypeID])))
		}
		if i == 2 {
			lines = append(lines, demoTackleByMe(prev+7*sec, "scramble", victim))
		}
		prev = killAt
	}
	return sm.render(start, lines), true
}
