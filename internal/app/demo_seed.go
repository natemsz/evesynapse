package app

// The demo account's seed data (demo.go has the account itself).
// seedDemoData writes one complete, self-consistent snapshot of
// a lived-in account: two pilots in a small corporation, every
// dataset the signed-in pages read present and populated, all of
// it invented. It runs when the account is first created, again
// from the -seed-demo maintenance command, and daily from the
// worker, so the demo always shows recent dates and undamaged
// data no matter what a visitor managed to change.
//
// Everything is resolved from the install's own data at seed
// time: skills are every published skill in the SDE (the main
// pilot has them all at level V), and items, ships, stations and
// systems are looked up by name, so the seed carries no item ID
// that could drift away from the SDE it renders against. A name
// the SDE does not know is skipped, never faked; only the skill
// list is load-bearing, because an all-V pilot with no skills is
// not a demo.
//
// Dates are computed from the moment of seeding, which is what
// keeps a daily-restored demo from ever looking abandoned.

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/fitview"
	"evesynapse/internal/logging"
)

// The invented people around the demo pilots: correspondents,
// counterparties, corp members. IDs share the demo band.
const (
	demoCharMarcus = int64(2900000101) // Marcus Hale, freighter pilot
	demoCharIlya   = int64(2900000102) // Ilya Sorin, a rival in Tama
	demoCharPetra  = int64(2900000103) // Petra Lindqvist, PI neighbour
	demoCharSera   = int64(2900000104) // Sera Voss, corp member
	demoCharDario  = int64(2900000105) // Dario Fenn, corp member
	demoCharMina   = int64(2900000106) // Mina Okafor, corp member
	demoCharKato   = int64(2900000107) // Kato Vire, Kedama Watch
	demoCharRhen   = int64(2900000108) // Rhen Absalom, Kedama Watch
	demoCharLio    = int64(2900000109) // Lio Marsh, Kedama Watch

	demoCorpHaleID  = int64(2900001002) // Hale Freight Services
	demoCorpWatchID = int64(2900001003) // Kedama Watch, the local pirates
)

// demoFictionalChars is every invented character the seed data
// mentions, for the name and record rows they need.
var demoFictionalChars = []struct {
	ID     int64
	Name   string
	Corp   int64
	Sec    float64 // security status; 0 reads as a quiet 1.2
	Joined string  // RFC3339 start of the current corporation stint
}{
	{demoCharMarcus, "Marcus Hale", demoCorpHaleID, 0, "2019-04-18T12:00:00Z"},
	{demoCharIlya, "Ilya Sorin", demoCorpHaleID, 0, "2021-03-08T20:14:00Z"},
	{demoCharPetra, "Petra Lindqvist", demoCorpID, 0, "2020-02-14T08:45:00Z"},
	{demoCharSera, "Sera Voss", demoCorpID, 0, "2021-09-01T17:20:00Z"},
	{demoCharDario, "Dario Fenn", demoCorpID, 0, "2018-06-19T05:40:00Z"},
	{demoCharMina, "Mina Okafor", demoCorpID, 0, "2023-01-23T13:05:00Z"},
	{demoCharKato, "Kato Vire", demoCorpWatchID, -4.6, "2020-09-03T21:10:00Z"},
	{demoCharRhen, "Rhen Absalom", demoCorpWatchID, -2.9, "2021-05-17T22:52:00Z"},
	{demoCharLio, "Lio Marsh", demoCorpWatchID, -6.1, "2020-11-08T04:37:00Z"},
}

// demoItemBase is where the seed's invented item IDs start:
// asset, blueprint and job-adjacent item rows the demo owns.
// Real item IDs are player-issued and far smaller in practice;
// the band is chosen to never meet one.
const demoItemBase = int64(900000000000)

// skillSP is the cumulative skillpoints a skill of the given
// rank holds at each trained level, level 0 first.
var skillSP = []int64{0, 250, 1414, 8000, 45255, 256000}

func spAtLevel(rank float64, level int64) int64 {
	if level < 0 {
		level = 0
	}
	if level > 5 {
		level = 5
	}
	return int64(math.Round(float64(skillSP[level]) * rank))
}

// demoSeed is one seeding run: the resolved SDE references every
// payload builder below works from.
type demoSeed struct {
	app    *Application
	now    time.Time
	userID int64

	types  map[string]int64 // type name -> type id, unresolved names absent
	ranks  map[int64]float64
	skills []demoSkillType

	jitaStationID, amarrStationID, dodixieStationID, hekStationID int64
	jitaSystemID, amarrSystemID, tamaSystemID                     int64
	kedamaSystemID, sujaSystemID                                  int64 // lowsec beside Tama; Tama when unknown
	forgeRegionID, domainRegionID, sinqRegionID                   int64

	// pendingColonies gathers each pilot's colonies while her
	// writeColony calls run, for writeColonies to store.
	pendingColonies map[int64][]esi.Colony

	// corpKillmails is every seeded fight a corp member was in,
	// for the corporation killboard (writeCorpSnapshots).
	corpKillmails []esi.KillmailRef
}

type demoSkillType struct {
	ID   int64
	Rank float64
}

// typeID resolves a type name to its SDE id, or 0 when this
// install's SDE does not know it; builders skip what is 0.
func (s *demoSeed) typeID(name string) int64 { return s.types[name] }

func (s *demoSeed) rfc(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// snap stores one snapshot payload for a demo pilot, fresh now
// and cached far past the daily reset.
func (s *demoSeed) snap(ctx context.Context, characterID int64, kind string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("demo seed: marshal %s: %w", kind, err)
	}
	return s.app.queries.UpsertSnapshot(ctx, db.UpsertSnapshotParams{
		CharacterID: characterID,
		Kind:        kind,
		Payload:     string(raw),
		FetchedAt:   s.now,
		CachedUntil: sql.NullTime{Time: s.now.Add(30 * 24 * time.Hour), Valid: true},
	})
}

// resolve loads the SDE references the seed needs. Only the
// skill list is mandatory: with no skills published there is no
// all-V pilot to show, and the caller treats that as an
// incomplete seed to retry once the SDE import has landed.
func (s *demoSeed) resolve(ctx context.Context) error {
	q := s.app.queries

	rows, err := s.app.db.QueryContext(ctx, `
		SELECT t.type_id, COALESCE(m.rank, 1)
		FROM sde_types t
		JOIN sde_groups g ON g.group_id = t.group_id
		LEFT JOIN sde_skill_meta m ON m.type_id = t.type_id
		WHERE g.category_id = 16 AND t.published <> 0
		ORDER BY t.type_id`)
	if err != nil {
		return fmt.Errorf("demo seed: list skills: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sk demoSkillType
		if err := rows.Scan(&sk.ID, &sk.Rank); err != nil {
			return fmt.Errorf("demo seed: scan skill: %w", err)
		}
		s.skills = append(s.skills, sk)
		s.ranks[sk.ID] = sk.Rank
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("demo seed: list skills: %w", err)
	}
	if len(s.skills) == 0 {
		return fmt.Errorf("demo seed: the SDE has no published skills yet")
	}

	// Every type name the payloads mention, resolved in one pass.
	wanted := []string{
		// Ships.
		"Rifter", "Thrasher", "Merlin", "Punisher", "Drake", "Vexor", "Covetor",
		"Tristan", "Catalyst", "Atron",
		// Station containers the asset seeds fill.
		"Station Container", "Small Standard Container", "Station Warehouse Container",
		// Modules and charges.
		"Damage Control II", "Gyrostabilizer II", "Ballistic Control System II",
		"1MN Afterburner II", "Medium Shield Extender II", "200mm AutoCannon II",
		"125mm Gatling AutoCannon II", "Heavy Missile Launcher II",
		"Warp Scrambler II", "Stasis Webifier II", "Small Armor Repairer II",
		"Energized Adaptive Nano Membrane II", "Strip Miner I", "Mining Laser Upgrade II",
		"Hobgoblin II", "Warrior II", "Republic Fleet EMP S", "Fusion S", "Scourge Heavy Missile",
		// Materials and trade goods.
		"Tritanium", "Pyerite", "Mexallon", "Isogen", "Nocxium", "Zydrine", "Megacyte",
		"Veldspar", "Scordite", "Pyroxeres", "PLEX",
		// Blueprints.
		"Rifter Blueprint", "Thrasher Blueprint", "Damage Control II Blueprint",
		"Vexor Blueprint", "Covetor Blueprint", "Antimatter Charge S Blueprint",
		"Drake Blueprint", "Hobgoblin I Blueprint", "Hobgoblin II Blueprint",
		"Gyrostabilizer II Blueprint", "Ballistic Control System II Blueprint",
		"Scourge Heavy Missile Blueprint", "Fusion S Blueprint",
		"Small Armor Repairer II Blueprint", "Medium Shield Extender II Blueprint",
		// Attribute implants, slots 1-5.
		"Ocular Filter - Basic", "Memory Augmentation - Basic", "Neural Boost - Basic",
		"Cybernetic Subprocessor - Basic", "Social Adaptation Chip - Basic",
		// Skill hardwirings, slots 6-9 (the seeded Sable clone set).
		"Zainou 'Gnome' Shield Upgrades SU-603", "Zainou 'Deadeye' Missile Bombardment MB-705",
		"Zainou 'Deadeye' Guided Missile Precision GP-805", "Zainou 'Deadeye' Target Navigation Prediction TN-903",
		// Planetary commodities and structures.
		"Oxygen", "Water", "Bacteria", "Proteins", "Reactive Gas", "Precious Metals",
		"Toxic Metals", "Electrolytes", "Micro Organisms", "Carbon Compounds",
		"Aqueous Liquids", "Ionic Solutions", "Base Metals", "Suspended Plasma",
		"Noble Gas", "Reactive Metals", "Plasmoids",
		"Temperate Command Center", "Barren Command Center", "Gas Command Center",
		"Lava Command Center", "Plasma Command Center", "Extractor Control Unit", "Storage Facility",
		"Launchpad", "Basic Industry Facility", "Advanced Industry Facility",
		// The industry alt's trained skills and plan targets, resolved
		// by name like everything else.
		"Mining", "Astrogeology", "Industry", "Mass Production", "Advanced Mass Production",
		"Refining", "Reprocessing", "Reprocessing Efficiency", "Metallurgy", "Science",
		"Laboratory Operation", "Advanced Laboratory Operation", "Research",
		"Mining Foreman", "Mining Director", "Leadership", "Spaceship Command",
		"Mining Barge", "Exhumers", "ORE Industrial", "Veldspar Processing",
		"Scordite Processing", "Pyroxeres Processing", "Drones", "Drone Interfacing",
		"Shield Operation", "Energy Grid Upgrades", "CPU Management",
		"Power Grid Management", "Navigation", "Afterburner", "Trade", "Retail",
		"Broker Relations", "Accounting", "Cybernetics", "Biology",
		"Capital Industrial Ships",
	}
	// Every wanted type in two queries (the SDE table, then
	// the name cache for what it lacks) instead of up to two
	// queries per name.
	if rows, err := s.app.queries.ListSDETypesByNames(ctx, wanted); err == nil {
		for _, r := range rows {
			if cur, ok := s.types[r.Name]; !ok || r.TypeID < cur {
				s.types[r.Name] = r.TypeID
			}
		}
	}
	var missing []string
	for _, name := range wanted {
		if s.types[name] == 0 {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		if rows, err := s.app.queries.ListTypeNamesByNames(ctx, missing); err == nil {
			for _, r := range rows {
				if cur, ok := s.types[r.Name]; !ok || r.TypeID < cur {
					s.types[r.Name] = r.TypeID
				}
			}
		}
	}

	// The hub stations by ID: CCP's station IDs are fixed, while
	// their names change spelling between SDE releases (Jita IV
	// became Jita 4), and a missed name filed every asset, order
	// and job under station 0. A station this SDE lacks stays 0.
	station := func(id int64) int64 {
		ok, err := stationKnown(ctx, s.app.db, id)
		if err != nil {
			logging.Warnf("demo seed: resolve station %d: %v", id, err)
			return 0
		}
		if !ok {
			return 0
		}
		return id
	}
	s.jitaStationID = station(60003760)    // Jita 4 - Moon 4 - Caldari Navy Assembly Plant
	s.amarrStationID = station(60008494)   // Amarr VIII (Oris) - Emperor Family Academy
	s.dodixieStationID = station(60011866) // Dodixie 9 - Moon 20 - Federation Navy Assembly Plant
	s.hekStationID = station(60005686)     // Hek 8 - Moon 12 - Boundless Creation Factory

	system := func(name string) (int64, int64) {
		row, err := q.GetSDESystemByName(ctx, name)
		if err != nil {
			logging.Warnf("demo seed: resolve system %q: %v", name, err)
			return 0, 0
		}
		return row.SystemID, row.RegionID
	}
	s.jitaSystemID, s.forgeRegionID = system("Jita")
	s.amarrSystemID, s.domainRegionID = system("Amarr")
	s.tamaSystemID, _ = system("Tama")
	s.kedamaSystemID, _ = system("Kedama")
	s.sujaSystemID, _ = system("Sujarento")
	if s.kedamaSystemID == 0 {
		s.kedamaSystemID = s.tamaSystemID
	}
	if s.sujaSystemID == 0 {
		s.sujaSystemID = s.tamaSystemID
	}
	if row, err := q.GetSDESystemByName(ctx, "Dodixie"); err == nil {
		s.sinqRegionID = row.RegionID
	}
	return nil
}

// stationKnown reports whether this install's SDE has the station.
func stationKnown(ctx context.Context, conn *sql.DB, id int64) (bool, error) {
	var found int64
	err := conn.QueryRowContext(ctx,
		`SELECT station_id FROM sde_stations WHERE station_id = $1`, id).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// wipe clears every row the demo account owns, so writing the
// seed state over it is exact: nothing a visitor (or an older
// seed) left behind survives into the new state.
func (s *demoSeed) wipe(ctx context.Context) error {
	q := s.app.queries
	for _, charID := range []int64{demoCharacterID, demoCharacter2ID} {
		steps := []func() error{
			func() error { return q.DeleteDemoSnapshots(ctx, charID) },
			func() error { return q.DeleteDemoCharacterCorporation(ctx, charID) },
			func() error { return q.DeleteDemoWalletHistory(ctx, charID) },
			func() error { return q.DeleteDemoOrderHealth(ctx, charID) },
			func() error { return q.DeleteDemoOrderLifecycle(ctx, charID) },
			func() error { return q.DeleteDemoContractDetails(ctx, charID) },
			func() error { return q.DeleteDemoKillmailDetails(ctx, charID) },
		}
		for _, step := range steps {
			if err := step(); err != nil {
				return fmt.Errorf("demo seed: wipe character %d: %w", charID, err)
			}
		}
	}
	userSteps := []func() error{
		func() error { return q.DeleteDemoFittings(ctx, s.userID) },
		func() error { return q.DeleteDemoSkillPlans(ctx, s.userID) },
		func() error { return q.DeleteDemoNotifications(ctx, s.userID) },
		func() error { return q.DeleteDemoNotificationSeen(ctx, s.userID) },
		func() error { return q.DeleteDemoWidgetConfigs(ctx, s.userID) },
		func() error { return q.DeleteDemoAppraisals(ctx, s.userID) },
		func() error { return q.DeleteDemoWatchlist(ctx, s.userID) },
		func() error { return q.DeleteDemoLinkChoices(ctx, s.userID) },
		func() error { return q.DeleteDemoPushSubscriptions(ctx, s.userID) },
		func() error { return q.ResetDemoHomeLayout(ctx, s.userID) },
		func() error { return q.DeleteDemoBlueprintPins(ctx, s.userID) },
		func() error { return q.DeleteDemoRestockTargets(ctx, s.userID) },
		func() error { return q.DeleteDemoBaskets(ctx, sql.NullInt64{Int64: s.userID, Valid: true}) },
		// The demo corporation is invented (demoCorpID sits in the
		// demo ID band), so everything it owns is the seed's.
		func() error { return q.DeleteDemoDoctrines(ctx, demoCorpID) },
		func() error { return q.DeleteDemoOps(ctx, demoCorpID) },
		func() error { return q.DeleteDemoOrgGroups(ctx, demoCorpID) },
		// The battle reports and the shared kill rows their
		// fights join to (demo_battles.go): the reports' times
		// move with every seed, and a stored kill keeps its
		// first copy, so both go for the battles step to file
		// the fresh ones.
		func() error { return q.DeleteDemoBattleFights(ctx, s.userID) },
		func() error { return q.DeleteDemoBattleKills(ctx, demoBattleMailIDs) },
		func() error { return q.DeleteDemoBattleKillAttackers(ctx, demoBattleMailIDs) },
		func() error { return q.DeleteDemoBattleKillInvolvement(ctx, demoBattleMailIDs) },
		func() error { return q.DeleteDemoBattleKillDetails(ctx, demoBattleMailIDs) },
		func() error { return q.DeleteDemoBattleKillPrices(ctx, demoBattleMailIDs) },
	}
	for _, step := range userSteps {
		if err := step(); err != nil {
			return fmt.Errorf("demo seed: wipe user rows: %w", err)
		}
	}
	return nil
}

// writeNames seeds the stored name and public-record rows the
// demo's fictional people need, so every name in the seeded
// payloads resolves through the same machinery as a real one and
// the worker's record drains find them already settled.
func (s *demoSeed) writeNames(ctx context.Context) error {
	names := []entityName{
		{Kind: esi.NameCharacter, ID: demoCharacterID, Name: demoCharName},
		{Kind: esi.NameCharacter, ID: demoCharacter2ID, Name: demoChar2Name},
		{Kind: esi.NameCorporation, ID: demoCorpID, Name: demoCorpName},
		{Kind: esi.NameCorporation, ID: demoCorpHaleID, Name: "Hale Freight Services"},
		{Kind: esi.NameCorporation, ID: demoCorpWatchID, Name: "Kedama Watch"},
	}
	for _, c := range demoFictionalChars {
		names = append(names, entityName{Kind: esi.NameCharacter, ID: c.ID, Name: c.Name})
	}
	if err := upsertEntityNames(ctx, s.app.db, names); err != nil {
		return fmt.Errorf("demo seed: entity names: %w", err)
	}
	if s.app.esi != nil {
		for _, n := range names {
			s.app.esi.PrimeName(n.Kind, n.ID, n.Name)
		}
	}

	fetchedAt := sql.NullTime{Time: s.now, Valid: true}
	// The pilot page reads the record as the envelope the
	// worker's drain assembles (pilotPayload in intel_pilot.go):
	// the profile, its corporation, and the employment history
	// with names baked in, oldest stint first.
	corps := map[int64]esi.Corporation{
		demoCorpID:      {Name: demoCorpName, Ticker: "TPC"},
		demoCorpHaleID:  {Name: "Hale Freight Services", Ticker: "HALE"},
		demoCorpWatchID: {Name: "Kedama Watch", Ticker: "KWTCH"},
	}
	stint := func(corpID int64, start, end string) pilotHistoryRow {
		return pilotHistoryRow{CorpID: corpID, CorpName: corps[corpID].Name, Start: start, End: end}
	}
	record := func(id int64, corpID int64, name, birthday string, sec float64, title, description string, history []pilotHistoryRow) error {
		payload, err := json.Marshal(pilotPayload{
			Profile: esi.Character{
				Name: name, CorporationID: corpID, Birthday: birthday, SecurityStatus: sec,
				Title: title, Description: description,
			},
			Corp:    corps[corpID],
			History: history,
		})
		if err != nil {
			return err
		}
		return s.app.setPilotRecord(ctx, db.SetPilotRecordParams{
			CharacterID: id, Payload: string(payload), State: "ready", FetchedAt: fetchedAt,
		})
	}
	if err := record(demoCharacterID, demoCorpID, demoCharName, "2016-03-14T09:24:00Z", 4.8,
		"Chief Executive Officer",
		"Test pilot for the Test Pilot Collective. Everything you see on this account is sample data, refreshed daily.",
		[]pilotHistoryRow{
			stint(demoCorpHaleID, "2016-03-14T09:24:00Z", "2019-05-20T10:00:00Z"),
			stint(demoCorpID, "2019-05-20T10:00:00Z", ""),
		}); err != nil {
		return fmt.Errorf("demo seed: pilot record: %w", err)
	}
	if err := record(demoCharacter2ID, demoCorpID, demoChar2Name, "2019-06-02T14:03:00Z", 3.1,
		"Factory Manager",
		"Industry alt of the Test Pilot Collective's CEO. Builds the ships the rest of the corp loses.",
		[]pilotHistoryRow{
			stint(demoCorpHaleID, "2019-06-02T14:03:00Z", "2021-08-15T09:30:00Z"),
			stint(demoCorpID, "2021-08-15T09:30:00Z", ""),
		}); err != nil {
		return fmt.Errorf("demo seed: pilot record: %w", err)
	}
	birthdays := []string{"2018-01-19T08:00:00Z", "2020-11-30T19:45:00Z", "2017-04-11T06:12:00Z",
		"2021-02-25T22:31:00Z", "2015-09-09T03:17:00Z", "2022-08-16T11:58:00Z"}
	for i, c := range demoFictionalChars {
		sec := c.Sec
		if sec == 0 {
			sec = 1.2
		}
		if err := record(c.ID, c.Corp, c.Name, birthdays[i%len(birthdays)], sec, "", "",
			[]pilotHistoryRow{stint(c.Corp, c.Joined, "")}); err != nil {
			return fmt.Errorf("demo seed: pilot record %d: %w", c.ID, err)
		}
	}
	corpPayload, err := json.Marshal(esi.Corporation{
		Name: "Hale Freight Services", Ticker: "HALE", MemberCount: 3,
		CEOID: demoCharMarcus, CreatorID: demoCharMarcus, TaxRate: 0.05,
		DateFounded: "2019-04-18T12:00:00Z", HomeStationID: s.dodixieStationID,
	})
	if err != nil {
		return err
	}
	if err := s.app.setCorporationRecord(ctx, db.SetCorporationRecordParams{
		CorporationID: demoCorpHaleID, Payload: string(corpPayload), State: "ready", FetchedAt: fetchedAt,
	}); err != nil {
		return fmt.Errorf("demo seed: corporation record: %w", err)
	}
	watchPayload, err := json.Marshal(esi.Corporation{
		Name: "Kedama Watch", Ticker: "KWTCH", MemberCount: 14,
		CEOID: demoCharKato, CreatorID: demoCharKato, TaxRate: 0.1,
		DateFounded: "2020-09-03T21:10:00Z",
		Description: "Lowsec small gang out of Kedama. Frigates welcome, tears expected.",
	})
	if err != nil {
		return err
	}
	if err := s.app.setCorporationRecord(ctx, db.SetCorporationRecordParams{
		CorporationID: demoCorpWatchID, Payload: string(watchPayload), State: "ready", FetchedAt: fetchedAt,
	}); err != nil {
		return fmt.Errorf("demo seed: corporation record: %w", err)
	}
	return nil
}

// seedDemoData writes the complete seed state for the demo
// account: names and records, both pilots' datasets, the
// corporation's datasets under each pilot, and the account-level
// rows (saved fits, a skill plan, notifications, wallet history,
// the watch list, planet and item names). It wipes first, so
// running it any number of times lands on the same state.
func (app *Application) seedDemoData(ctx context.Context, now time.Time) error {
	userID := app.demoUserID(ctx)
	if userID == 0 {
		return fmt.Errorf("demo seed: the demo account does not exist")
	}
	s := &demoSeed{
		app: app, now: now.UTC(), userID: userID,
		types: map[string]int64{}, ranks: map[int64]float64{},
		pendingColonies: map[int64][]esi.Colony{},
	}
	if err := s.resolve(ctx); err != nil {
		return err
	}
	if err := s.wipe(ctx); err != nil {
		return err
	}
	if err := s.writeNames(ctx); err != nil {
		return err
	}
	if err := s.writePilotOne(ctx); err != nil {
		return err
	}
	for _, write := range []func(context.Context) error{
		s.writePilotOneMarkets, s.writePilotOneIndustry,
		s.writePilotOneAssets, s.writePilotOneKillmails, s.writePilotOneBattles,
		s.writePilotActivityProfiles,
		s.writePilotOneComms,
	} {
		if err := write(ctx); err != nil {
			return err
		}
	}
	if err := s.writePilotTwo(ctx); err != nil {
		return err
	}
	for _, pilot := range demoPilots {
		if err := s.writeCorpSnapshots(ctx, pilot.ID); err != nil {
			return err
		}
	}
	if err := s.writeAccountRows(ctx); err != nil {
		return err
	}
	if err := s.writeOrgRows(ctx); err != nil {
		return err
	}
	// The worker folds a real pilot's snapshots into the profit
	// ledger after each warm; the demo pilots never warm, so the
	// seed folds theirs itself. Without this the Profit pages
	// have no ledger to read and render empty. The fold runs
	// before any wallet history exists, so it attributes the
	// year and nothing more; the history is then derived from
	// that attribution (writeDemoWalletHistory) and each pilot's
	// remainder is folded against it, which is what keeps the
	// page's Not explained small enough to believe.
	for _, pilot := range demoPilots {
		ch, err := app.queries.GetCharacter(ctx, pilot.ID)
		if err != nil {
			return fmt.Errorf("demo seed: load pilot %d for the profit fold: %w", pilot.ID, err)
		}
		app.refreshProfitLedger(ctx, ch, now)
	}
	// The two folds above land each pilot's rows in turn, so a
	// type one pilot's jobs consume can checkpoint past history
	// the other pilot's book would have filled. Clear the pool's
	// member key to make the next pass refold the pool from
	// scratch, the same remedy the LP path uses, now that both
	// books are in.
	if err := app.queries.ClearLedgerPoolMembers(ctx, db.ClearLedgerPoolMembersParams{PoolKind: poolAcct, PoolID: userID}); err != nil {
		return fmt.Errorf("demo seed: clear the pool member key: %w", err)
	}
	for _, pilot := range demoPilots {
		ch, err := app.queries.GetCharacter(ctx, pilot.ID)
		if err != nil {
			return fmt.Errorf("demo seed: load pilot %d for the pool refold: %w", pilot.ID, err)
		}
		app.refreshProfitLedger(ctx, ch, now)
	}
	for _, pilot := range demoPilots {
		ch, err := app.queries.GetCharacter(ctx, pilot.ID)
		if err != nil {
			return fmt.Errorf("demo seed: load pilot %d for the wallet history: %w", pilot.ID, err)
		}
		if err := s.writeDemoWalletHistory(ctx, ch); err != nil {
			return err
		}
		if err := app.foldRemainder(ctx, ch.CharacterID, s.now.AddDate(0, 0, -364).Format("2006-01-02")); err != nil {
			return fmt.Errorf("demo seed: fold remainder for pilot %d: %w", pilot.ID, err)
		}
	}
	logging.Infof("demo: seeded the demo account (%d skills at V for %s)", len(s.skills), demoCharName)
	return nil
}

// journalBalance walks newest-first journal entries back from
// the wallet's current balance, filling each entry's Balance
// with the balance just after it landed.
func journalBalances(entries []esi.WalletJournalEntry, current float64) {
	balance := current
	for i := range entries {
		entries[i].Balance = balance
		balance -= entries[i].Amount
	}
}

func corpJournalBalances(entries []esi.CorpJournalEntry, current float64) {
	balance := current
	for i := range entries {
		entries[i].Balance = balance
		balance -= entries[i].Amount
	}
}

// demoSalesTaxRate is the sales tax rate every seeded sale pays:
// the hand-written journal's tax lines sit at this rate, and the
// generated history's do too, so the fold's tax pairing (which
// checks a second's rate against its neighbours) pairs them all.
const demoSalesTaxRate = 0.05

// demoTradeLane is one repeated trade of a pilot's generated
// history: a type she buys below her sell price on a cycle of
// days, the way a working market alt actually trades.
type demoTradeLane struct {
	typeName string
	base     float64 // the price the lane trades around
	lot      int64   // the typical buy quantity
	round    int64   // quantities round to this
	step     int     // days between one cycle's buy and the next
	phase    int     // days back the most recent buy sits
}

// demoLootSale is one sale of goods no buy in the pool covers
// (loot, in the story): its tax pairs, its cost stays unknown,
// and the charts hatch its days as partly covered.
type demoLootSale struct {
	typeName string
	daysAgo  int
	qty      int64
	price    float64
}

// demoJitter spreads the generated history's times, prices and
// quantities deterministically: the same seed always writes the
// same history relative to the seeding moment, never a frozen
// one, because every date hangs off that moment.
func demoJitter(seed int64) float64 {
	x := uint64(seed)*2654435761 + 2246822519
	x ^= x >> 13
	x *= 3266489917
	x ^= x >> 16
	return float64(x%10000) / 10000
}

func demoRoundQty(v float64, step int64) int64 {
	if step <= 1 {
		return int64(math.Round(v))
	}
	return int64(math.Round(v/float64(step))) * step
}

// demoPrice rounds a generated price the way the market shows
// them: whole hundreds for module money, two decimals below.
func demoPrice(p float64) float64 {
	if p >= 10000 {
		return math.Round(p/100) * 100
	}
	return math.Round(p*100) / 100
}

// demoBookBuilder accumulates one pilot's generated journal and
// transaction rows: raw records in the shapes the ingest path
// produces (a journal line for every transaction, a tax line in
// the same second as every sale), for the seed's own fold to
// consume exactly as it consumes a real pilot's.
type demoBookBuilder struct {
	s       *demoSeed
	charID  int64
	station int64
	clients []int64
	txnID   int64
	jrnlID  int64
	seq     int64
	journal []esi.WalletJournalEntry
	txns    []esi.WalletTransaction
}

func (b *demoBookBuilder) jitter() float64 {
	b.seq++
	return demoJitter(b.seq*7919 + b.charID)
}

// at is a clock time on a past day: the day's small hours are for
// market lines, the afternoon for everything else.
func (b *demoBookBuilder) at(daysAgo, hour int) time.Time {
	return b.s.now.Truncate(24*time.Hour).AddDate(0, 0, -daysAgo).
		Add(time.Duration(hour) * time.Hour).
		Add(time.Duration(b.jitter()*59) * time.Minute)
}

func (b *demoBookBuilder) add(at time.Time, e esi.WalletJournalEntry) int64 {
	b.jrnlID++
	e.ID = b.jrnlID
	e.Date = b.s.rfc(at)
	b.journal = append(b.journal, e)
	return e.ID
}

// market writes the journal line of one wallet transaction: the
// counterparty pays on a sale, the pilot pays on a buy.
func (b *demoBookBuilder) market(at time.Time, client int64, amount float64, sell bool) int64 {
	e := esi.WalletJournalEntry{RefType: "market_transaction", Amount: amount,
		Description: "Market transaction", FirstPartyType: "character", SecondPartyType: "character"}
	if sell {
		e.FirstPartyID, e.SecondPartyID = client, b.charID
	} else {
		e.FirstPartyID, e.SecondPartyID = b.charID, client
	}
	return b.add(at, e)
}

// tax writes a sale's tax line in the sale's own second, which
// is what the fold pairs on.
func (b *demoBookBuilder) tax(at time.Time, revenue float64) {
	b.add(at, esi.WalletJournalEntry{RefType: "transaction_tax",
		Amount:       -math.Round(revenue * demoSalesTaxRate),
		FirstPartyID: b.charID, FirstPartyType: "character",
		SecondPartyID: demoCorpHaleID, SecondPartyType: "corporation",
		Description: "Transaction tax"})
}

func (b *demoBookBuilder) trade(at time.Time, typeID int64, price float64, qty, client int64, buy bool, jrnlID int64) {
	b.txnID++
	b.txns = append(b.txns, esi.WalletTransaction{
		TransactionID: b.txnID, Date: b.s.rfc(at), TypeID: typeID, LocationID: b.station,
		UnitPrice: price, Quantity: qty, ClientID: client, IsBuy: buy, IsPersonal: true,
		JournalRefID: jrnlID,
	})
}

// lane runs one trade lane back through the last year: buy,
// sell most of it a day or two later at a higher price, repeat.
// Part of every buy stays in stock, the way a real book
// accumulates. The recent ninety days trade at full cadence;
// older cycles space out to a weekly scale in smaller lots, so
// the long span has shape without a year of daily rows. Weekends
// run quiet, and one week a month back runs hot.
func (b *demoBookBuilder) lane(l demoTradeLane, laneIdx int) {
	typeID := b.s.typeID(l.typeName)
	if typeID == 0 {
		return
	}
	for c := 0; ; c++ {
		daysAgo := l.phase + c*l.step
		if daysAgo > 364 {
			return
		}
		if daysAgo > 90 && c%3 != 0 {
			continue // the sparse years: about one cycle in three
		}
		client := b.clients[(laneIdx+c)%len(b.clients)]
		scale := 1.0
		if daysAgo > 90 {
			scale = 0.55
		}
		if daysAgo >= 16 && daysAgo <= 22 {
			scale *= 2.2 // the strong week
		}
		switch wd := b.s.now.Truncate(24*time.Hour).AddDate(0, 0, -daysAgo).Weekday(); wd {
		case time.Saturday, time.Sunday:
			scale *= 0.45
		case time.Friday:
			scale *= 0.8
		}
		buyQty := demoRoundQty(float64(l.lot)*scale*(0.8+0.5*b.jitter()), l.round)
		sellQty := demoRoundQty(float64(buyQty)*(0.6+0.35*b.jitter()), l.round)
		buyPrice := demoPrice(l.base * (0.95 + 0.05*b.jitter()))
		sellPrice := demoPrice(l.base * (1.05 + 0.08*b.jitter()))
		buyAt := b.at(daysAgo, 7+int(b.jitter()*4))
		sellAt := buyAt.Add(time.Duration(1500+b.jitter()*1200) * time.Minute)
		if sellAt.After(b.s.now.Add(-time.Hour)) {
			continue
		}
		jrnlID := b.market(buyAt, client, -float64(buyQty)*buyPrice, false)
		b.trade(buyAt, typeID, buyPrice, buyQty, client, true, jrnlID)
		if c%3 == 2 {
			b.add(buyAt.Add(3*time.Minute), esi.WalletJournalEntry{RefType: "brokers_fee",
				Amount:       -math.Round(float64(buyQty) * buyPrice * 0.008),
				FirstPartyID: b.charID, FirstPartyType: "character",
				SecondPartyID: demoCorpHaleID, SecondPartyType: "corporation",
				Description: "Broker fee"})
		}
		jrnlID = b.market(sellAt, client, float64(sellQty)*sellPrice, true)
		b.trade(sellAt, typeID, sellPrice, sellQty, client, false, jrnlID)
		b.tax(sellAt, float64(sellQty)*sellPrice)
	}
}

// lootSale writes one uncovered sale and its tax line.
func (b *demoBookBuilder) lootSale(l demoLootSale, client int64) {
	typeID := b.s.typeID(l.typeName)
	if typeID == 0 {
		return
	}
	at := b.at(l.daysAgo, 15)
	rev := float64(l.qty) * l.price
	jrnlID := b.market(at, client, rev, true)
	b.trade(at, typeID, l.price, l.qty, client, false, jrnlID)
	b.tax(at, rev)
}

// journalLine places one extra journal entry on a past day.
func (b *demoBookBuilder) journalLine(daysAgo int, e esi.WalletJournalEntry) {
	b.add(b.at(daysAgo, 13), e)
}

func intPtr(v int) *int { return &v }

// writePilotOne seeds Sable Iversen: the all-skills-at-V pilot.
func (s *demoSeed) writePilotOne(ctx context.Context) error {
	id := demoCharacterID
	now := s.now
	hour := time.Hour
	day := 24 * hour

	if err := s.snap(ctx, id, esi.SnapProfile, esi.Character{
		Name: demoCharName, CorporationID: demoCorpID,
		Birthday: "2016-03-14T09:24:00Z", SecurityStatus: 4.8,
		Title:       "Chief Executive Officer",
		Description: "Test pilot for the Test Pilot Collective. Everything you see on this account is sample data, refreshed daily.",
	}); err != nil {
		return err
	}

	// Every published skill at level V.
	skills := esi.Skills{UnallocatedSP: 250000}
	for _, sk := range s.skills {
		sp := spAtLevel(sk.Rank, 5)
		skills.Skills = append(skills.Skills, esi.Skill{
			SkillID: sk.ID, SkillpointsInSkill: sp,
			ActiveSkillLevel: 5, TrainedSkillLevel: 5,
		})
		skills.TotalSP += sp
	}
	if err := s.snap(ctx, id, esi.SnapSkills, skills); err != nil {
		return err
	}
	// An all-V pilot has nothing left to train; the queue is
	// honestly empty. Her alt carries the training story.
	if err := s.snap(ctx, id, esi.SnapSkillqueue, esi.Skillqueue{}); err != nil {
		return err
	}
	if err := s.snap(ctx, id, esi.SnapAttributes, esi.Attributes{
		Intelligence: 32, Memory: 30, Perception: 31, Willpower: 29, Charisma: 27,
		BonusRemaps: intPtr(1), LastRemapDate: s.rfc(now.Add(-47 * day)),
	}); err != nil {
		return err
	}

	// Wallet and its journal.
	const wallet = 4281553120.44
	if err := s.snap(ctx, id, esi.SnapWallet, json.RawMessage(fmt.Sprintf("%.2f", wallet))); err != nil {
		return err
	}
	journal := []esi.WalletJournalEntry{
		{ID: 8800001021, Date: s.rfc(now.Add(-2 * hour)), RefType: "transaction_tax", Amount: -922500, FirstPartyID: id, FirstPartyType: "character", SecondPartyID: demoCorpHaleID, SecondPartyType: "corporation", Description: "Transaction tax"},
		{ID: 8800001012, Date: s.rfc(now.Add(-2 * hour)), RefType: "market_transaction", Amount: 18450000, FirstPartyID: demoCharSera, FirstPartyType: "character", SecondPartyID: id, SecondPartyType: "character", Description: "Market transaction"},
		{ID: 8800001011, Date: s.rfc(now.Add(-7 * hour)), RefType: "bounty_prizes", Amount: 6240000, FirstPartyID: demoCorpHaleID, FirstPartyType: "corporation", SecondPartyID: id, SecondPartyType: "character", Description: "Bounty prizes for Tama"},
		{ID: 8800001019, Date: s.rfc(now.Add(-12 * hour)), RefType: "transaction_tax", Amount: -260000, FirstPartyID: id, FirstPartyType: "character", SecondPartyID: demoCorpHaleID, SecondPartyType: "corporation", Description: "Transaction tax"},
		{ID: 8800001016, Date: s.rfc(now.Add(-12 * hour)), RefType: "market_transaction", Amount: 5200000, FirstPartyID: demoCharSera, FirstPartyType: "character", SecondPartyID: id, SecondPartyType: "character", Description: "Market transaction"},
		{ID: 8800001010, Date: s.rfc(now.Add(-20 * hour)), RefType: "player_donation", Amount: 5000000, FirstPartyID: demoCharMarcus, FirstPartyType: "character", SecondPartyID: id, SecondPartyType: "character", Description: "Player donation", Reason: "for the op fund"},
		{ID: 8800001018, Date: s.rfc(now.Add(-1 * day)), RefType: "transaction_tax", Amount: -93600, FirstPartyID: id, FirstPartyType: "character", SecondPartyID: demoCorpHaleID, SecondPartyType: "corporation", Description: "Transaction tax"},
		{ID: 8800001015, Date: s.rfc(now.Add(-1 * day)), RefType: "market_transaction", Amount: 1872000, FirstPartyID: demoCharMina, FirstPartyType: "character", SecondPartyID: id, SecondPartyType: "character", Description: "Market transaction"},
		{ID: 8800001009, Date: s.rfc(now.Add(-26 * hour)), RefType: "contract_reward", Amount: 8500000, FirstPartyID: demoCharMarcus, FirstPartyType: "character", SecondPartyID: id, SecondPartyType: "character", Description: "Courier contract reward"},
		{ID: 8800001008, Date: s.rfc(now.Add(-30 * hour)), RefType: "market_transaction", Amount: -46240000, FirstPartyID: id, FirstPartyType: "character", SecondPartyID: demoCharMarcus, SecondPartyType: "character", Description: "Market transaction"},
		{ID: 8800001007, Date: s.rfc(now.Add(-2 * day)), RefType: "market_escrow", Amount: -212499972, FirstPartyID: id, FirstPartyType: "character", SecondPartyID: id, SecondPartyType: "character", Description: "Market escrow for a buy order"},
		{ID: 8800001006, Date: s.rfc(now.Add(-2 * day)), RefType: "broker_fee", Amount: -1240300, FirstPartyID: id, FirstPartyType: "character", SecondPartyID: demoCorpHaleID, SecondPartyType: "corporation", Description: "Broker fee"},
		{ID: 8800001017, Date: s.rfc(now.Add(-3 * day)), RefType: "transaction_tax", Amount: -1117500, FirstPartyID: id, FirstPartyType: "character", SecondPartyID: demoCorpHaleID, SecondPartyType: "corporation", Description: "Transaction tax"},
		{ID: 8800001005, Date: s.rfc(now.Add(-3 * day)), RefType: "manufacturing", Amount: -3850000, FirstPartyID: id, FirstPartyType: "character", SecondPartyID: demoCorpHaleID, SecondPartyType: "corporation", Description: "Manufacturing job", ContextID: 8800009001, ContextType: "industry_job_id"},
		{ID: 8800001004, Date: s.rfc(now.Add(-4 * day)), RefType: "corporation_account_withdrawal", Amount: 7250000, FirstPartyID: demoCorpID, FirstPartyType: "corporation", SecondPartyID: id, SecondPartyType: "character", Description: "Corporation account withdrawal"},
		{ID: 8800001003, Date: s.rfc(now.Add(-5 * day)), RefType: "research", Amount: -1150000, FirstPartyID: id, FirstPartyType: "character", SecondPartyID: demoCorpHaleID, SecondPartyType: "corporation", Description: "Research"},
		{ID: 8800001020, Date: s.rfc(now.Add(-6 * day)), RefType: "transaction_tax", Amount: -535500, FirstPartyID: id, FirstPartyType: "character", SecondPartyID: demoCorpHaleID, SecondPartyType: "corporation", Description: "Transaction tax"},
		{ID: 8800001002, Date: s.rfc(now.Add(-6 * day)), RefType: "jump_clone_fee", Amount: -900000, FirstPartyID: id, FirstPartyType: "character", SecondPartyID: demoCorpHaleID, SecondPartyType: "corporation", Description: "Jump clone activation fee"},
		{ID: 8800001014, Date: s.rfc(now.Add(-7 * day)), RefType: "market_transaction", Amount: -13800000, FirstPartyID: id, FirstPartyType: "character", SecondPartyID: demoCharPetra, SecondPartyType: "character", Description: "Market transaction"},
		{ID: 8800001001, Date: s.rfc(now.Add(-8 * day)), RefType: "transaction_tax", Amount: -923400, FirstPartyID: id, FirstPartyType: "character", SecondPartyID: demoCorpHaleID, SecondPartyType: "corporation", Description: "Transaction tax"},
		{ID: 8800001013, Date: s.rfc(now.Add(-9 * day)), RefType: "market_transaction", Amount: -13020000, FirstPartyID: id, FirstPartyType: "character", SecondPartyID: demoCharDario, SecondPartyType: "character", Description: "Market transaction"},
	}
	// The generated trading history behind the hand-written
	// entries: a year of working lanes in the minerals,
	// ammunition and modules she trades, dense in the recent
	// ninety days and sparse before that, so the Profit page's
	// charts have a book to draw, not a vignette.
	book := &demoBookBuilder{s: s, charID: id, station: s.jitaStationID,
		clients: []int64{demoCharSera, demoCharMina, demoCharDario, demoCharPetra, demoCharMarcus},
		txnID:   7700010000, jrnlID: 8800010000}
	for i, lane := range []demoTradeLane{
		{typeName: "Tritanium", base: 6.05, lot: 3500000, round: 1000, step: 3, phase: 4},
		{typeName: "Pyerite", base: 11.90, lot: 900000, round: 1000, step: 3, phase: 5},
		{typeName: "Mexallon", base: 49.50, lot: 260000, round: 100, step: 4, phase: 6},
		{typeName: "Ballistic Control System II", base: 865000, lot: 14, round: 1, step: 4, phase: 7},
		{typeName: "Republic Fleet EMP S", base: 990, lot: 6000, round: 100, step: 5, phase: 8},
		{typeName: "Scourge Heavy Missile", base: 445, lot: 5000, round: 100, step: 5, phase: 3},
	} {
		book.lane(lane, i)
	}
	book.lootSale(demoLootSale{typeName: "Warrior II", daysAgo: 19, qty: 300, price: 620}, demoCharDario)
	// The bad trade: a stack of Gyrostabilizers chased up one
	// day and dumped well below cost the next, the month's one
	// clear mistake.
	if gyro := s.typeID("Gyrostabilizer II"); gyro != 0 {
		buyAt := book.at(14, 8)
		jrnlID := book.market(buyAt, demoCharPetra, -30*1150000, false)
		book.trade(buyAt, gyro, 1150000, 30, demoCharPetra, true, jrnlID)
		sellAt := book.at(13, 9)
		jrnlID = book.market(sellAt, demoCharMina, 30*822000, true)
		book.trade(sellAt, gyro, 822000, 30, demoCharMina, false, jrnlID)
		book.tax(sellAt, 30*822000)
	}
	for _, line := range []struct {
		daysAgo int
		entry   esi.WalletJournalEntry
	}{
		{16, esi.WalletJournalEntry{RefType: "bounty_prizes", Amount: 5100000, FirstPartyID: demoCorpHaleID, FirstPartyType: "corporation", SecondPartyID: id, SecondPartyType: "character", Description: "Bounty prizes for Tama"}},
		{23, esi.WalletJournalEntry{RefType: "bounty_prizes", Amount: 7300000, FirstPartyID: demoCorpHaleID, FirstPartyType: "corporation", SecondPartyID: id, SecondPartyType: "character", Description: "Bounty prizes for Tama"}},
		{31, esi.WalletJournalEntry{RefType: "bounty_prizes", Amount: 4600000, FirstPartyID: demoCorpHaleID, FirstPartyType: "corporation", SecondPartyID: id, SecondPartyType: "character", Description: "Bounty prizes for Tama"}},
		{18, esi.WalletJournalEntry{RefType: "contract_reward", Amount: 6500000, FirstPartyID: demoCharMarcus, FirstPartyType: "character", SecondPartyID: id, SecondPartyType: "character", Description: "Courier contract reward"}},
		{15, esi.WalletJournalEntry{RefType: "manufacturing", Amount: -1900000, FirstPartyID: id, FirstPartyType: "character", SecondPartyID: demoCorpHaleID, SecondPartyType: "corporation", Description: "Manufacturing job", ContextID: 8800009205, ContextType: "industry_job_id"}},
		{22, esi.WalletJournalEntry{RefType: "research", Amount: -840000, FirstPartyID: id, FirstPartyType: "character", SecondPartyID: demoCorpHaleID, SecondPartyType: "corporation", Description: "Research"}},
		{16, esi.WalletJournalEntry{RefType: "corporation_account_withdrawal", Amount: 18500000, FirstPartyID: demoCorpID, FirstPartyType: "corporation", SecondPartyID: id, SecondPartyType: "character", Description: "Corporation account withdrawal"}},
		{9, esi.WalletJournalEntry{RefType: "player_donation", Amount: -2750000, FirstPartyID: id, FirstPartyType: "character", SecondPartyID: demoCharMina, SecondPartyType: "character", Description: "Player donation", Reason: "for the replacement Drake"}},
		{6, esi.WalletJournalEntry{RefType: "player_donation", Amount: -3400000, FirstPartyID: id, FirstPartyType: "character", SecondPartyID: demoCharacter2ID, SecondPartyType: "character", Description: "Player donation", Reason: "for materials"}},
	} {
		book.journalLine(line.daysAgo, line.entry)
	}
	journal = append(journal, book.journal...)
	sort.SliceStable(journal, func(i, j int) bool { return journal[i].Date > journal[j].Date })
	journalBalances(journal, wallet)
	if err := s.snap(ctx, id, esi.SnapWalletJournal, esi.WalletJournal(journal)); err != nil {
		return err
	}
	txns := []esi.WalletTransaction{
		{TransactionID: 7700001006, Date: s.rfc(now.Add(-2 * hour)), TypeID: s.typeID("Tritanium"), LocationID: s.jitaStationID, UnitPrice: 6.15, Quantity: 3000000, ClientID: demoCharSera, IsBuy: false, IsPersonal: true, JournalRefID: 8800001012},
		{TransactionID: 7700001010, Date: s.rfc(now.Add(-12 * hour)), TypeID: s.typeID("Republic Fleet EMP S"), LocationID: s.jitaStationID, UnitPrice: 1040, Quantity: 5000, ClientID: demoCharSera, IsBuy: false, IsPersonal: true, JournalRefID: 8800001016},
		{TransactionID: 7700001005, Date: s.rfc(now.Add(-30 * hour)), TypeID: s.typeID("Tritanium"), LocationID: s.jitaStationID, UnitPrice: 5.78, Quantity: 8000000, ClientID: demoCharMarcus, IsBuy: true, IsPersonal: true, JournalRefID: 8800001008},
		{TransactionID: 7700001009, Date: s.rfc(now.Add(-1 * day)), TypeID: s.typeID("Scourge Heavy Missile"), LocationID: s.jitaStationID, UnitPrice: 468.00, Quantity: 4000, ClientID: demoCharMina, IsBuy: false, IsPersonal: true, JournalRefID: 8800001015},
		{TransactionID: 7700001004, Date: s.rfc(now.Add(-2 * day)), TypeID: s.typeID("Scourge Heavy Missile"), LocationID: s.jitaStationID, UnitPrice: 412.50, Quantity: 4000, ClientID: demoCharPetra, IsBuy: true, IsPersonal: true, JournalRefID: 0},
		{TransactionID: 7700001003, Date: s.rfc(now.Add(-3 * day)), TypeID: s.typeID("Damage Control II"), LocationID: s.jitaStationID, UnitPrice: 745000, Quantity: 30, ClientID: demoCharMina, IsBuy: false, IsPersonal: true, JournalRefID: 0},
		{TransactionID: 7700001002, Date: s.rfc(now.Add(-4 * day)), TypeID: s.typeID("Republic Fleet EMP S"), LocationID: s.jitaStationID, UnitPrice: 890, Quantity: 5000, ClientID: demoCharDario, IsBuy: true, IsPersonal: true, JournalRefID: 0},
		{TransactionID: 7700001001, Date: s.rfc(now.Add(-6 * day)), TypeID: s.typeID("Pyerite"), LocationID: s.jitaStationID, UnitPrice: 11.90, Quantity: 900000, ClientID: demoCharSera, IsBuy: false, IsPersonal: true, JournalRefID: 0},
		{TransactionID: 7700001008, Date: s.rfc(now.Add(-7 * day)), TypeID: s.typeID("Damage Control II"), LocationID: s.jitaStationID, UnitPrice: 690000, Quantity: 20, ClientID: demoCharPetra, IsBuy: true, IsPersonal: true, JournalRefID: 8800001014},
		{TransactionID: 7700001007, Date: s.rfc(now.Add(-9 * day)), TypeID: s.typeID("Pyerite"), LocationID: s.jitaStationID, UnitPrice: 10.85, Quantity: 1200000, ClientID: demoCharDario, IsBuy: true, IsPersonal: true, JournalRefID: 8800001013},
	}
	txns = append(txns, book.txns...)
	sort.SliceStable(txns, func(i, j int) bool { return txns[i].Date > txns[j].Date })
	if err := s.snap(ctx, id, esi.SnapWalletTxns, esi.WalletTransactions(txns)); err != nil {
		return err
	}
	return nil
}

// writePilotOneMarkets seeds Sable's orders, contracts and their
// stored detail rows.
func (s *demoSeed) writePilotOneMarkets(ctx context.Context) error {
	id := demoCharacterID
	now := s.now
	hour := time.Hour
	day := 24 * hour
	q := s.app.queries

	orders := []esi.CharOrder{
		{OrderID: 7900000001, TypeID: s.typeID("Tritanium"), LocationID: s.jitaStationID, RegionID: s.forgeRegionID, Price: 6.12, VolumeTotal: 2500000, VolumeRemain: 1450000, MinVolume: 1, Range: "station", Issued: s.rfc(now.Add(-3 * day)), Duration: 90},
		{OrderID: 7900000002, TypeID: s.typeID("Damage Control II"), LocationID: s.jitaStationID, RegionID: s.forgeRegionID, Price: 771000, VolumeTotal: 40, VolumeRemain: 26, MinVolume: 1, Range: "station", Issued: s.rfc(now.Add(-1 * day)), Duration: 90},
		{OrderID: 7900000003, TypeID: s.typeID("PLEX"), LocationID: s.jitaStationID, RegionID: s.forgeRegionID, IsBuyOrder: true, Price: 5902777, VolumeTotal: 36, VolumeRemain: 36, MinVolume: 1, Range: "station", Issued: s.rfc(now.Add(-5 * hour)), Duration: 30, Escrow: 212499972},
		{OrderID: 7900000004, TypeID: s.typeID("Gyrostabilizer II"), LocationID: s.jitaStationID, RegionID: s.forgeRegionID, Price: 1189000, VolumeTotal: 24, VolumeRemain: 17, MinVolume: 1, Range: "station", Issued: s.rfc(now.Add(-2 * day)), Duration: 90},
		{OrderID: 7900000005, TypeID: s.typeID("Hobgoblin II"), LocationID: s.jitaStationID, RegionID: s.forgeRegionID, Price: 509900, VolumeTotal: 150, VolumeRemain: 94, MinVolume: 1, Range: "station", Issued: s.rfc(now.Add(-4 * day)), Duration: 90},
		{OrderID: 7900000006, TypeID: s.typeID("Nocxium"), LocationID: s.jitaStationID, RegionID: s.forgeRegionID, IsBuyOrder: true, Price: 1201, VolumeTotal: 60000, VolumeRemain: 41250, MinVolume: 1, Range: "region", Issued: s.rfc(now.Add(-30 * hour)), Duration: 30, Escrow: 49541250},
	}
	if err := s.snap(ctx, id, esi.SnapOrders, esi.CharOrders(orders)); err != nil {
		return err
	}
	health := []struct {
		order           esi.CharOrder
		status          string
		station, region float64
	}{
		{orders[0], "best", 6.12, 6.10},
		{orders[1], "undercut_station", 768750, 768750},
		{orders[2], "best", 5902777, 5902777},
		{orders[3], "undercut_station", 1184400, 1184400},
		{orders[4], "best", 509900, 509800},
		{orders[5], "best", 1201, 1203},
	}
	for _, hRow := range health {
		if err := q.UpsertOrderHealth(ctx, db.UpsertOrderHealthParams{
			CharacterID: id, OrderID: hRow.order.OrderID, TypeID: hRow.order.TypeID,
			RegionID: hRow.order.RegionID, LocationID: hRow.order.LocationID,
			MyPrice: hRow.order.Price, StationBest: hRow.station, RegionBest: hRow.region,
			Status: hRow.status, ComputedAt: now,
		}); err != nil {
			return err
		}
		if err := q.UpsertOrderLifecycle(ctx, db.UpsertOrderLifecycleParams{
			CharacterID: id, OrderID: hRow.order.OrderID, TypeID: hRow.order.TypeID,
			LocationID: hRow.order.LocationID, RegionID: hRow.order.RegionID,
			IsBuyOrder: boolToInt(hRow.order.IsBuyOrder), ListedPrice: hRow.order.Price,
			VolumeTotal: hRow.order.VolumeTotal, VolumeRemainLast: hRow.order.VolumeRemain,
			FirstSeenAt: now.Add(-3 * day), LastSeenAt: now,
		}); err != nil {
			return err
		}
	}
	// Two sells beaten at Jita: the verdicts the order-health
	// pass stores, so My Orders shows what beat them, the next
	// price past it and the margin left there.
	for _, b := range []struct {
		order               esi.CharOrder
		beatPrice, suggest  float64
		beatVolume, outbids int64
		unitCost, margin    float64
		beatenAgo           time.Duration
	}{
		{orders[1], 768750, 768700, 6, 3, 612400, 23.9, 3 * hour},
		{orders[3], 1184400, 1184300, 4, 1, 948000, 21.4, 50 * time.Minute},
	} {
		if err := q.UpdateOrderLifecycleBook(ctx, db.UpdateOrderLifecycleBookParams{
			Beaten: 1, CheckedAt: now.Add(-b.beatenAgo), OutbidEvents: b.outbids,
			BeatingPrice: b.beatPrice, BeatingVolume: b.beatVolume, SuggestedPrice: b.suggest,
			UnitCost: b.unitCost, MarginPct: sql.NullFloat64{Float64: b.margin, Valid: true},
			TaxRate: demoSalesTaxRate, CharacterID: id, OrderID: b.order.OrderID,
		}); err != nil {
			return err
		}
		if err := q.TouchOrderLifecycleChecked(ctx, db.TouchOrderLifecycleCheckedParams{
			CheckedAt: now.Add(-10 * time.Minute), CharacterID: id, OrderID: b.order.OrderID,
		}); err != nil {
			return err
		}
	}
	history := []esi.CharOrderHistoryEntry{
		{CharOrder: esi.CharOrder{OrderID: 7900000011, TypeID: s.typeID("Vexor"), LocationID: s.jitaStationID, RegionID: s.forgeRegionID, Price: 98500000, VolumeTotal: 1, VolumeRemain: 1, MinVolume: 1, Range: "station", Issued: s.rfc(now.Add(-32 * day)), Duration: 30}, State: "expired"},
		{CharOrder: esi.CharOrder{OrderID: 7900000012, TypeID: s.typeID("Mexallon"), LocationID: s.jitaStationID, RegionID: s.forgeRegionID, IsBuyOrder: true, Price: 44.20, VolumeTotal: 500000, VolumeRemain: 500000, MinVolume: 1, Range: "station", Issued: s.rfc(now.Add(-8 * day)), Duration: 30, Escrow: 22100000}, State: "cancelled"},
		// Most of a trader's history fills: these sold or bought out.
		{CharOrder: esi.CharOrder{OrderID: 7900000013, TypeID: s.typeID("Hobgoblin II"), LocationID: s.jitaStationID, RegionID: s.forgeRegionID, Price: 512000, VolumeTotal: 100, VolumeRemain: 0, MinVolume: 1, Range: "station", Issued: s.rfc(now.Add(-12 * day)), Duration: 90}, State: "expired"},
		{CharOrder: esi.CharOrder{OrderID: 7900000014, TypeID: s.typeID("Damage Control II"), LocationID: s.jitaStationID, RegionID: s.forgeRegionID, Price: 774500, VolumeTotal: 40, VolumeRemain: 0, MinVolume: 1, Range: "station", Issued: s.rfc(now.Add(-10 * day)), Duration: 90}, State: "expired"},
		{CharOrder: esi.CharOrder{OrderID: 7900000015, TypeID: s.typeID("Tritanium"), LocationID: s.jitaStationID, RegionID: s.forgeRegionID, IsBuyOrder: true, Price: 5.88, VolumeTotal: 3000000, VolumeRemain: 0, MinVolume: 1, Range: "station", Issued: s.rfc(now.Add(-9 * day)), Duration: 30, Escrow: 0}, State: "expired"},
	}
	if err := s.snap(ctx, id, esi.SnapOrdersHistory, esi.CharOrderHistory(history)); err != nil {
		return err
	}
	for _, hRow := range history {
		if err := q.UpsertOrderLifecycle(ctx, db.UpsertOrderLifecycleParams{
			CharacterID: id, OrderID: hRow.OrderID, TypeID: hRow.TypeID,
			LocationID: hRow.LocationID, RegionID: hRow.RegionID,
			IsBuyOrder: boolToInt(hRow.IsBuyOrder), ListedPrice: hRow.Price,
			VolumeTotal: hRow.VolumeTotal, VolumeRemainLast: hRow.VolumeRemain,
			FirstSeenAt: now.Add(-8 * day), LastSeenAt: now.Add(-2 * day),
		}); err != nil {
			return err
		}
		kind := "ended"
		if hRow.VolumeRemain <= 0 {
			kind = "filled"
		}
		if err := q.CloseOrderLifecycle(ctx, db.CloseOrderLifecycleParams{
			ClosedAt: now.Add(-2 * day), CloseKind: kind, CharacterID: id, OrderID: hRow.OrderID,
		}); err != nil {
			return err
		}
	}

	contracts := []esi.Contract{
		{ContractID: 2900005001, IssuerID: demoCharMarcus, IssuerCorporationID: demoCorpHaleID, AssigneeID: id, AcceptorID: id, Type: "item_exchange", Status: "finished", Availability: "personal", Title: "Fitted Rifter, ready to undock", Price: 14500000, StartLocationID: s.jitaStationID, EndLocationID: s.jitaStationID, DateIssued: s.rfc(now.Add(-6 * day)), DateExpired: s.rfc(now.Add(24 * day)), DateAccepted: s.rfc(now.Add(-5 * day)), DateCompleted: s.rfc(now.Add(-5 * day))},
		{ContractID: 2900005002, IssuerID: id, IssuerCorporationID: demoCorpID, AcceptorID: demoCharMarcus, Type: "courier", Status: "in_progress", Availability: "public", Title: "Jita to Amarr, 12,500 m3", Reward: 8500000, Collateral: 250000000, Volume: 12500, StartLocationID: s.jitaStationID, EndLocationID: s.amarrStationID, DateIssued: s.rfc(now.Add(-1 * day)), DateExpired: s.rfc(now.Add(13 * day)), DateAccepted: s.rfc(now.Add(-20 * hour)), DaysToComplete: 3},
		{ContractID: 2900005003, IssuerID: id, IssuerCorporationID: demoCorpID, Type: "item_exchange", Status: "outstanding", Availability: "public", Title: "Damage Control II x40", Price: 41500000, StartLocationID: s.jitaStationID, EndLocationID: s.jitaStationID, DateIssued: s.rfc(now.Add(-8 * hour)), DateExpired: s.rfc(now.Add(7 * day))},
	}
	if err := s.snap(ctx, id, esi.SnapContracts, esi.Contracts(contracts)); err != nil {
		return err
	}
	contractItems := map[int64][]esi.ContractItem{
		2900005001: {
			{RecordID: 1, TypeID: s.typeID("Rifter"), Quantity: 1, RawQuantity: 1, IsIncluded: true, IsSingleton: true},
			{RecordID: 2, TypeID: s.typeID("Damage Control II"), Quantity: 1, RawQuantity: 1, IsIncluded: true, IsSingleton: true},
			{RecordID: 3, TypeID: s.typeID("200mm AutoCannon II"), Quantity: 2, RawQuantity: 2, IsIncluded: true, IsSingleton: true},
		},
		2900005002: {
			{RecordID: 1, TypeID: s.typeID("Tritanium"), Quantity: 2000000, RawQuantity: 2000000, IsIncluded: true},
		},
		2900005003: {
			{RecordID: 1, TypeID: s.typeID("Damage Control II"), Quantity: 40, RawQuantity: 40, IsIncluded: true},
		},
	}
	for contractID, items := range contractItems {
		raw, err := json.Marshal(esi.ContractItems(items))
		if err != nil {
			return err
		}
		if err := q.UpsertContractDetail(ctx, db.UpsertContractDetailParams{
			ContractID: contractID, CharacterID: id, Payload: string(raw), FetchedAt: now,
		}); err != nil {
			return err
		}
	}
	return nil
}

func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// writePilotOneIndustry seeds Sable's jobs, blueprints and
// mining ledger.
func (s *demoSeed) writePilotOneIndustry(ctx context.Context) error {
	id := demoCharacterID
	now := s.now
	hour := time.Hour
	day := 24 * hour

	jobs := []esi.IndustryJob{
		{JobID: 8800009001, ActivityID: 1, BlueprintTypeID: s.typeID("Rifter Blueprint"), BlueprintID: demoItemBase + 501, BlueprintLocationID: s.jitaStationID, OutputLocationID: s.jitaStationID, FacilityID: s.jitaStationID, StationID: s.jitaStationID, InstallerID: id, Runs: 10, LicensedRuns: 10, Status: "active", StartDate: s.rfc(now.Add(-1 * day)), EndDate: s.rfc(now.Add(14 * hour)), Duration: 129600, Cost: 1240000, ProductTypeID: s.typeID("Rifter")},
		{JobID: 8800009002, ActivityID: 1, BlueprintTypeID: s.typeID("Damage Control II Blueprint"), BlueprintID: demoItemBase + 502, BlueprintLocationID: s.jitaStationID, OutputLocationID: s.jitaStationID, FacilityID: s.jitaStationID, StationID: s.jitaStationID, InstallerID: id, Runs: 20, LicensedRuns: 20, SuccessfulRuns: 20, Status: "delivered", StartDate: s.rfc(now.Add(-9 * day)), EndDate: s.rfc(now.Add(-8 * day)), CompletedDate: s.rfc(now.Add(-8 * day)), Duration: 86400, Cost: 860000, ProductTypeID: s.typeID("Damage Control II"), CompletedCharacterID: id},
		// The rest of a working week: hulls and modules building,
		// a research and an invention job running, a batch waiting
		// to be delivered, and two delivered earlier.
		{JobID: 8800009003, ActivityID: 1, BlueprintTypeID: s.typeID("Thrasher Blueprint"), BlueprintID: demoItemBase + 504, BlueprintLocationID: s.jitaStationID, OutputLocationID: s.jitaStationID, FacilityID: s.jitaStationID, StationID: s.jitaStationID, InstallerID: id, Runs: 5, LicensedRuns: 5, Status: "active", StartDate: s.rfc(now.Add(-9 * hour)), EndDate: s.rfc(now.Add(6 * hour)), Duration: 54000, Cost: 1690000, ProductTypeID: s.typeID("Thrasher")},
		{JobID: 8800009004, ActivityID: 4, BlueprintTypeID: s.typeID("Drake Blueprint"), BlueprintID: demoItemBase + 505, BlueprintLocationID: s.jitaStationID, OutputLocationID: s.jitaStationID, FacilityID: s.jitaStationID, StationID: s.jitaStationID, InstallerID: id, Runs: 1, LicensedRuns: 1, Status: "active", StartDate: s.rfc(now.Add(-1 * day)), EndDate: s.rfc(now.Add(2*day + 4*hour)), Duration: 273600, Cost: 3120000, ProductTypeID: s.typeID("Drake Blueprint")},
		{JobID: 8800009005, ActivityID: 8, BlueprintTypeID: s.typeID("Hobgoblin I Blueprint"), BlueprintID: demoItemBase + 506, BlueprintLocationID: s.jitaStationID, OutputLocationID: s.jitaStationID, FacilityID: s.jitaStationID, StationID: s.jitaStationID, InstallerID: id, Runs: 4, LicensedRuns: 4, Probability: 0.425, Status: "active", StartDate: s.rfc(now.Add(-7 * hour)), EndDate: s.rfc(now.Add(19 * hour)), Duration: 93600, Cost: 412000, ProductTypeID: s.typeID("Hobgoblin II Blueprint")},
		{JobID: 8800009006, ActivityID: 1, BlueprintTypeID: s.typeID("Gyrostabilizer II Blueprint"), BlueprintID: demoItemBase + 507, BlueprintLocationID: s.jitaStationID, OutputLocationID: s.jitaStationID, FacilityID: s.jitaStationID, StationID: s.jitaStationID, InstallerID: id, Runs: 10, LicensedRuns: 10, Status: "ready", StartDate: s.rfc(now.Add(-26 * hour)), EndDate: s.rfc(now.Add(-3 * hour)), Duration: 82800, Cost: 1140000, ProductTypeID: s.typeID("Gyrostabilizer II")},
		{JobID: 8800009007, ActivityID: 1, BlueprintTypeID: s.typeID("Scourge Heavy Missile Blueprint"), BlueprintID: demoItemBase + 508, BlueprintLocationID: s.jitaStationID, OutputLocationID: s.jitaStationID, FacilityID: s.jitaStationID, StationID: s.jitaStationID, InstallerID: id, Runs: 50, LicensedRuns: 50, SuccessfulRuns: 50, Status: "delivered", StartDate: s.rfc(now.Add(-4 * day)), EndDate: s.rfc(now.Add(-3 * day)), CompletedDate: s.rfc(now.Add(-3 * day)), Duration: 90000, Cost: 286000, ProductTypeID: s.typeID("Scourge Heavy Missile"), CompletedCharacterID: id},
		{JobID: 8800009008, ActivityID: 1, BlueprintTypeID: s.typeID("Hobgoblin II Blueprint"), BlueprintID: demoItemBase + 509, BlueprintLocationID: s.jitaStationID, OutputLocationID: s.jitaStationID, FacilityID: s.jitaStationID, StationID: s.jitaStationID, InstallerID: id, Runs: 10, LicensedRuns: 10, SuccessfulRuns: 10, Status: "delivered", StartDate: s.rfc(now.Add(-6 * day)), EndDate: s.rfc(now.Add(-5 * day)), CompletedDate: s.rfc(now.Add(-5 * day)), Duration: 64800, Cost: 512000, ProductTypeID: s.typeID("Hobgoblin II"), CompletedCharacterID: id},
	}
	// A job whose blueprint this SDE does not know is left out.
	known := jobs[:0]
	for _, j := range jobs {
		if j.BlueprintTypeID != 0 && j.ProductTypeID != 0 {
			known = append(known, j)
		}
	}
	jobs = known
	if err := s.snap(ctx, id, esi.SnapIndustryJobs, esi.IndustryJobs(jobs)); err != nil {
		return err
	}
	blueprints := []esi.Blueprint{
		{ItemID: demoItemBase + 501, TypeID: s.typeID("Rifter Blueprint"), LocationID: s.jitaStationID, LocationFlag: "Hangar", MaterialEfficiency: 10, TimeEfficiency: 20, Quantity: -1, Runs: -1},
		{ItemID: demoItemBase + 502, TypeID: s.typeID("Damage Control II Blueprint"), LocationID: s.jitaStationID, LocationFlag: "Hangar", MaterialEfficiency: 8, TimeEfficiency: 14, Quantity: -2, Runs: 40},
		{ItemID: demoItemBase + 503, TypeID: s.typeID("Antimatter Charge S Blueprint"), LocationID: s.dodixieStationID, LocationFlag: "Hangar", MaterialEfficiency: 10, TimeEfficiency: 20, Quantity: -1, Runs: -1},
		{ItemID: demoItemBase + 504, TypeID: s.typeID("Thrasher Blueprint"), LocationID: s.jitaStationID, LocationFlag: "Hangar", MaterialEfficiency: 10, TimeEfficiency: 20, Quantity: -1, Runs: -1},
		{ItemID: demoItemBase + 505, TypeID: s.typeID("Drake Blueprint"), LocationID: s.jitaStationID, LocationFlag: "Hangar", MaterialEfficiency: 6, TimeEfficiency: 12, Quantity: -1, Runs: -1},
		{ItemID: demoItemBase + 506, TypeID: s.typeID("Hobgoblin I Blueprint"), LocationID: s.jitaStationID, LocationFlag: "Hangar", MaterialEfficiency: 10, TimeEfficiency: 20, Quantity: -1, Runs: -1},
		{ItemID: demoItemBase + 507, TypeID: s.typeID("Gyrostabilizer II Blueprint"), LocationID: s.jitaStationID, LocationFlag: "Hangar", MaterialEfficiency: 2, TimeEfficiency: 4, Quantity: -2, Runs: 4},
		{ItemID: demoItemBase + 508, TypeID: s.typeID("Scourge Heavy Missile Blueprint"), LocationID: s.jitaStationID, LocationFlag: "Hangar", MaterialEfficiency: 10, TimeEfficiency: 20, Quantity: -1, Runs: -1},
		{ItemID: demoItemBase + 509, TypeID: s.typeID("Hobgoblin II Blueprint"), LocationID: s.jitaStationID, LocationFlag: "Hangar", MaterialEfficiency: 2, TimeEfficiency: 4, Quantity: -2, Runs: 7},
		{ItemID: demoItemBase + 510, TypeID: s.typeID("Ballistic Control System II Blueprint"), LocationID: s.jitaStationID, LocationFlag: "Hangar", MaterialEfficiency: 2, TimeEfficiency: 4, Quantity: -2, Runs: 10},
		{ItemID: demoItemBase + 511, TypeID: s.typeID("Fusion S Blueprint"), LocationID: s.dodixieStationID, LocationFlag: "Hangar", MaterialEfficiency: 10, TimeEfficiency: 20, Quantity: -1, Runs: -1},
		{ItemID: demoItemBase + 512, TypeID: s.typeID("Small Armor Repairer II Blueprint"), LocationID: s.amarrStationID, LocationFlag: "Hangar", MaterialEfficiency: 2, TimeEfficiency: 4, Quantity: -2, Runs: 10},
		{ItemID: demoItemBase + 513, TypeID: s.typeID("Medium Shield Extender II Blueprint"), LocationID: s.jitaStationID, LocationFlag: "Hangar", MaterialEfficiency: 3, TimeEfficiency: 2, Quantity: -2, Runs: 8},
	}
	knownBPs := blueprints[:0]
	for _, b := range blueprints {
		if b.TypeID != 0 {
			knownBPs = append(knownBPs, b)
		}
	}
	blueprints = knownBPs
	if err := s.snap(ctx, id, esi.SnapBlueprints, esi.Blueprints(blueprints)); err != nil {
		return err
	}
	// The two lines Sable runs every week, pinned so the home
	// page's production lines show them.
	for _, name := range []string{"Damage Control II Blueprint", "Rifter Blueprint"} {
		if typeID := s.typeID(name); typeID != 0 {
			if err := s.app.queries.PinBlueprint(ctx, db.PinBlueprintParams{UserID: s.userID, BlueprintTypeID: typeID}); err != nil {
				return err
			}
		}
	}
	mining := []esi.MiningEntry{
		{Date: now.Add(-1 * day).Format("2006-01-02"), TypeID: s.typeID("Veldspar"), SolarSystemID: s.jitaSystemID, Quantity: 18400},
		{Date: now.Add(-2 * day).Format("2006-01-02"), TypeID: s.typeID("Scordite"), SolarSystemID: s.jitaSystemID, Quantity: 12900},
		{Date: now.Add(-4 * day).Format("2006-01-02"), TypeID: s.typeID("Veldspar"), SolarSystemID: s.jitaSystemID, Quantity: 22100},
	}
	if err := s.snap(ctx, id, esi.SnapMining, esi.MiningLedger(mining)); err != nil {
		return err
	}
	// LP from mission running, for the LP Store's balances.
	if err := s.snap(ctx, id, esi.SnapLoyalty, esi.LoyaltyBalances{
		{CorporationID: 1000035, LoyaltyPoints: 412850}, // Caldari Navy
		{CorporationID: 1000120, LoyaltyPoints: 96200},  // Federation Navy
	}); err != nil {
		return err
	}
	return nil
}

// writePilotOneAssets seeds Sable's assets, live state, clones,
// implants, saved-in-game fittings and fatigue.
func (s *demoSeed) writePilotOneAssets(ctx context.Context) error {
	id := demoCharacterID
	now := s.now
	hour := time.Hour
	day := 24 * hour
	q := s.app.queries

	shipItem := demoItemBase + 1
	assets := []esi.Asset{
		{ItemID: shipItem, TypeID: s.typeID("Rifter"), Quantity: 1, LocationID: s.jitaStationID, LocationType: "station", LocationFlag: "Hangar", IsSingleton: true},
		{ItemID: demoItemBase + 11, TypeID: s.typeID("200mm AutoCannon II"), Quantity: 1, LocationID: shipItem, LocationType: "item", LocationFlag: "HiSlot0", IsSingleton: true},
		{ItemID: demoItemBase + 12, TypeID: s.typeID("200mm AutoCannon II"), Quantity: 1, LocationID: shipItem, LocationType: "item", LocationFlag: "HiSlot1", IsSingleton: true},
		{ItemID: demoItemBase + 13, TypeID: s.typeID("125mm Gatling AutoCannon II"), Quantity: 1, LocationID: shipItem, LocationType: "item", LocationFlag: "HiSlot2", IsSingleton: true},
		{ItemID: demoItemBase + 14, TypeID: s.typeID("1MN Afterburner II"), Quantity: 1, LocationID: shipItem, LocationType: "item", LocationFlag: "MedSlot0", IsSingleton: true},
		{ItemID: demoItemBase + 15, TypeID: s.typeID("Medium Shield Extender II"), Quantity: 1, LocationID: shipItem, LocationType: "item", LocationFlag: "MedSlot1", IsSingleton: true},
		{ItemID: demoItemBase + 16, TypeID: s.typeID("Warp Scrambler II"), Quantity: 1, LocationID: shipItem, LocationType: "item", LocationFlag: "MedSlot2", IsSingleton: true},
		{ItemID: demoItemBase + 17, TypeID: s.typeID("Damage Control II"), Quantity: 1, LocationID: shipItem, LocationType: "item", LocationFlag: "LowSlot0", IsSingleton: true},
		{ItemID: demoItemBase + 18, TypeID: s.typeID("Gyrostabilizer II"), Quantity: 1, LocationID: shipItem, LocationType: "item", LocationFlag: "LowSlot1", IsSingleton: true},
		{ItemID: demoItemBase + 19, TypeID: s.typeID("Gyrostabilizer II"), Quantity: 1, LocationID: shipItem, LocationType: "item", LocationFlag: "LowSlot2", IsSingleton: true},
		{ItemID: demoItemBase + 20, TypeID: s.typeID("Hobgoblin II"), Quantity: 4, LocationID: shipItem, LocationType: "item", LocationFlag: "DroneBay"},
		{ItemID: demoItemBase + 21, TypeID: s.typeID("Republic Fleet EMP S"), Quantity: 1200, LocationID: shipItem, LocationType: "item", LocationFlag: "Cargo"},
		{ItemID: demoItemBase + 2, TypeID: s.typeID("Drake"), Quantity: 1, LocationID: s.jitaStationID, LocationType: "station", LocationFlag: "Hangar"},
		{ItemID: demoItemBase + 3, TypeID: s.typeID("Tritanium"), Quantity: 4250000, LocationID: s.jitaStationID, LocationType: "station", LocationFlag: "Hangar"},
		{ItemID: demoItemBase + 4, TypeID: s.typeID("Pyerite"), Quantity: 1120000, LocationID: s.jitaStationID, LocationType: "station", LocationFlag: "Hangar"},
		{ItemID: demoItemBase + 5, TypeID: s.typeID("Damage Control II"), Quantity: 14, LocationID: s.jitaStationID, LocationType: "station", LocationFlag: "Hangar"},
		{ItemID: demoItemBase + 6, TypeID: s.typeID("PLEX"), Quantity: 35, LocationID: s.jitaStationID, LocationType: "station", LocationFlag: "Hangar"},
		{ItemID: demoItemBase + 7, TypeID: s.typeID("Vexor"), Quantity: 1, LocationID: s.dodixieStationID, LocationType: "station", LocationFlag: "Hangar"},
		{ItemID: demoItemBase + 8, TypeID: s.typeID("Scourge Heavy Missile"), Quantity: 4000, LocationID: s.dodixieStationID, LocationType: "station", LocationFlag: "Hangar"},
	}
	// Two station cans in Jita with their contents inside, so the
	// Assets matrix shows its container strip and drill-in. A can
	// whose type this install's SDE does not know is skipped whole,
	// contents and all.
	canSpares := demoItemBase + 31
	if t := s.typeID("Station Container"); t != 0 {
		assets = append(assets,
			esi.Asset{ItemID: canSpares, TypeID: t, Quantity: 1, LocationID: s.jitaStationID, LocationType: "station", LocationFlag: "Hangar", IsSingleton: true},
			esi.Asset{ItemID: demoItemBase + 32, TypeID: s.typeID("Damage Control II"), Quantity: 6, LocationID: canSpares, LocationType: "item", LocationFlag: "Unlocked"},
			esi.Asset{ItemID: demoItemBase + 33, TypeID: s.typeID("Gyrostabilizer II"), Quantity: 4, LocationID: canSpares, LocationType: "item", LocationFlag: "Unlocked"},
			esi.Asset{ItemID: demoItemBase + 34, TypeID: s.typeID("Hobgoblin II"), Quantity: 10, LocationID: canSpares, LocationType: "item", LocationFlag: "Unlocked"},
		)
	}
	canAmmo := demoItemBase + 35
	if t := s.typeID("Small Standard Container"); t != 0 {
		assets = append(assets,
			esi.Asset{ItemID: canAmmo, TypeID: t, Quantity: 1, LocationID: s.jitaStationID, LocationType: "station", LocationFlag: "Hangar", IsSingleton: true},
			esi.Asset{ItemID: demoItemBase + 36, TypeID: s.typeID("Republic Fleet EMP S"), Quantity: 2400, LocationID: canAmmo, LocationType: "item", LocationFlag: "Unlocked"},
			esi.Asset{ItemID: demoItemBase + 37, TypeID: s.typeID("Scourge Heavy Missile"), Quantity: 1000, LocationID: canAmmo, LocationType: "item", LocationFlag: "Unlocked"},
		)
	}
	// Sable's working archive at Jita: one can per commodity and
	// purpose, the way a long-time organizer's hangar actually
	// looks. The container strip has to stay usable at this
	// count, so the demo carries the scale on purpose. Can types
	// and fillers this install's SDE does not know are skipped,
	// like the two cans above.
	archiveNames := []string{
		"Ammo - EMP S", "Ammo - Fusion S", "Ammo - Phased Plasma", "Ammo - Missiles Heavy", "Ammo - Missiles Light", "Ammo - Torpedoes",
		"Blueprints - Ships", "Blueprints - Modules", "Blueprints - Ammo", "Blueprints - Copies To Run", "Blueprints - Research Queue",
		"Components - T2 Build", "Components - Sleeper Loot", "Components - Salvaged Parts",
		"Datacores - Mechanical", "Datacores - Starship", "Decryptors - All Types",
		"Drones - Light Scouts", "Drones - Medium", "Drones - Sentries", "Drones - Mining",
		"Fuel - Isotopes", "Fuel - Strontium", "Fuel - POS Leftovers",
		"Implants - Loose Sets", "Loot - Mission Drops", "Loot - Rat Bounties",
		"Minerals - Tritanium", "Minerals - Pyerite", "Minerals - Mexallon", "Minerals - Isogen", "Minerals - Nocxium", "Minerals - Zydrine", "Minerals - Megacyte", "Minerals - Morphite",
		"Modules - Guns", "Modules - Launchers", "Modules - Tank Armor", "Modules - Tank Shield", "Modules - Propulsion", "Modules - EWAR",
		"Ore - Veldspar", "Ore - Scordite", "Ore - Pyroxeres", "Ore - Plagioclase", "Ore - Ice Products",
		"Personal - Keepsakes", "Personal - Sable Alt Gear",
		"PI - Tier One", "PI - Tier Two", "PI - Coolant Stock",
		"Salvage - Intact", "Salvage - Melted", "Salvage - Burned",
		"Sell - Jita Listings", "Sell - Overflow", "Sell - Reprocess?",
		"Ships - Spare Frigates", "Ships - Spare Cruisers",
		"SKINs - Permanent", "Staging - Doctrine Fits", "Staging - Cap Charges",
		"Trash - Sort Later", "Z - Archive 2024", "Z - Archive 2025",
	}
	var archiveTypes []int64
	for _, n := range []string{"Station Container", "Small Standard Container", "Medium Standard Container", "Large Standard Container", "Station Warehouse Container"} {
		if t := s.typeID(n); t != 0 {
			archiveTypes = append(archiveTypes, t)
		}
	}
	archiveFillers := []string{"Tritanium", "Pyerite", "Mexallon", "Isogen", "Republic Fleet EMP S", "Scourge Heavy Missile", "Hobgoblin II", "Damage Control II"}
	type archiveCan struct {
		id   int64
		name string
	}
	var archive []archiveCan
	if len(archiveTypes) > 0 {
		for i, name := range archiveNames {
			canID := demoItemBase + 1000 + int64(i)*4
			assets = append(assets, esi.Asset{ItemID: canID, TypeID: archiveTypes[i%len(archiveTypes)], Quantity: 1, LocationID: s.jitaStationID, LocationType: "station", LocationFlag: "Hangar", IsSingleton: true})
			for j := 0; j < 1+i%3; j++ {
				ft := s.typeID(archiveFillers[(i+j)%len(archiveFillers)])
				if ft == 0 {
					continue
				}
				assets = append(assets, esi.Asset{ItemID: canID + 1 + int64(j), TypeID: ft, Quantity: int64(250*(i%7+1) + j*37 + 10), LocationID: canID, LocationType: "item", LocationFlag: "Unlocked"})
			}
			archive = append(archive, archiveCan{id: canID, name: name})
		}
	}
	if err := s.snap(ctx, id, esi.SnapAssets, assets); err != nil {
		return err
	}
	if err := q.UpsertItemName(ctx, db.UpsertItemNameParams{ItemID: shipItem, Name: "Vale's Rifter"}); err != nil {
		return err
	}
	if s.typeID("Station Container") != 0 {
		if err := q.UpsertItemName(ctx, db.UpsertItemNameParams{ItemID: canSpares, Name: "Doctrine Spares"}); err != nil {
			return err
		}
	}
	if s.typeID("Small Standard Container") != 0 {
		if err := q.UpsertItemName(ctx, db.UpsertItemNameParams{ItemID: canAmmo, Name: "Ammo Crate"}); err != nil {
			return err
		}
	}
	for _, can := range archive {
		if err := q.UpsertItemName(ctx, db.UpsertItemNameParams{ItemID: can.id, Name: can.name}); err != nil {
			return err
		}
	}
	if err := s.snap(ctx, id, esi.SnapLocation, esi.Location{
		SolarSystemID: s.jitaSystemID, StationID: s.jitaStationID,
	}); err != nil {
		return err
	}
	if err := s.snap(ctx, id, esi.SnapShip, esi.Ship{
		ShipTypeID: s.typeID("Rifter"), ShipItemID: shipItem, ShipName: "Vale's Rifter",
	}); err != nil {
		return err
	}
	if err := s.snap(ctx, id, esi.SnapOnline, esi.Online{
		Online: false, LastLogin: s.rfc(now.Add(-3 * hour)), LastLogout: s.rfc(now.Add(-2 * hour)), Logins: 412,
	}); err != nil {
		return err
	}

	implantIDs := func(names ...string) []int64 {
		out := []int64{}
		for _, name := range names {
			if t := s.typeID(name); t != 0 {
				out = append(out, t)
			}
		}
		return out
	}
	basics := implantIDs(
		"Ocular Filter - Basic", "Memory Augmentation - Basic", "Neural Boost - Basic",
		"Cybernetic Subprocessor - Basic", "Social Adaptation Chip - Basic",
	)
	// The active pod: the five attribute basics, then missile
	// hardwirings in slots 6 to 9; slot 10 stays empty.
	implants := append(append([]int64{}, basics...), implantIDs(
		"Zainou 'Gnome' Shield Upgrades SU-603", "Zainou 'Deadeye' Missile Bombardment MB-705",
		"Zainou 'Deadeye' Guided Missile Precision GP-805", "Zainou 'Deadeye' Target Navigation Prediction TN-903",
	)...)
	if err := s.snap(ctx, id, esi.SnapImplants, esi.Implants(implants)); err != nil {
		return err
	}
	jumpClones := []esi.JumpClone{
		{JumpCloneID: 1, LocationID: s.amarrStationID, LocationType: "station", Name: "Amarr Trade & Learning", Implants: basics},
		{JumpCloneID: 2, LocationID: s.dodixieStationID, LocationType: "station", Name: "Clean Roam / Dictor Clone", Implants: implantIDs("Zainou 'Gnome' Shield Upgrades SU-603")},
	}
	if s.hekStationID != 0 {
		jumpClones = append(jumpClones, esi.JumpClone{
			JumpCloneID: 3, LocationID: s.hekStationID, LocationType: "station", Name: "",
			Implants: implantIDs("Ocular Filter - Basic", "Memory Augmentation - Basic"),
		})
	}
	clones := esi.Clones{
		HomeLocation: esi.CloneHome{LocationID: s.jitaStationID, LocationType: "station"},
		JumpClones:   jumpClones,
		// Five hours ago: the clone jump cooldown is still
		// running on the sheet (24h less her Infomorph
		// Synchronizing level), which is the timers card's
		// live state.
		LastCloneJumpDate: s.rfc(now.Add(-5 * hour)),
	}
	if err := s.snap(ctx, id, esi.SnapClones, clones); err != nil {
		return err
	}
	if err := s.snap(ctx, id, esi.SnapFatigue, esi.Fatigue{
		LastJumpDate:          s.rfc(now.Add(-3 * day)),
		JumpFatigueExpireDate: s.rfc(now.Add(-3*day + 30*time.Minute)),
		LastUpdateDate:        s.rfc(now.Add(-3 * day)),
	}); err != nil {
		return err
	}

	rifterItems := []esi.FittingItem{
		{TypeID: s.typeID("200mm AutoCannon II"), Quantity: 1, Flag: "HiSlot0"},
		{TypeID: s.typeID("200mm AutoCannon II"), Quantity: 1, Flag: "HiSlot1"},
		{TypeID: s.typeID("125mm Gatling AutoCannon II"), Quantity: 1, Flag: "HiSlot2"},
		{TypeID: s.typeID("1MN Afterburner II"), Quantity: 1, Flag: "MedSlot0"},
		{TypeID: s.typeID("Medium Shield Extender II"), Quantity: 1, Flag: "MedSlot1"},
		{TypeID: s.typeID("Warp Scrambler II"), Quantity: 1, Flag: "MedSlot2"},
		{TypeID: s.typeID("Damage Control II"), Quantity: 1, Flag: "LowSlot0"},
		{TypeID: s.typeID("Gyrostabilizer II"), Quantity: 1, Flag: "LowSlot1"},
		{TypeID: s.typeID("Gyrostabilizer II"), Quantity: 1, Flag: "LowSlot2"},
		{TypeID: s.typeID("Hobgoblin II"), Quantity: 4, Flag: "DroneBay"},
		{TypeID: s.typeID("Republic Fleet EMP S"), Quantity: 1200, Flag: "Cargo"},
	}
	drakeItems := []esi.FittingItem{
		{TypeID: s.typeID("Heavy Missile Launcher II"), Quantity: 1, Flag: "HiSlot0"},
		{TypeID: s.typeID("Heavy Missile Launcher II"), Quantity: 1, Flag: "HiSlot1"},
		{TypeID: s.typeID("Heavy Missile Launcher II"), Quantity: 1, Flag: "HiSlot2"},
		{TypeID: s.typeID("Heavy Missile Launcher II"), Quantity: 1, Flag: "HiSlot3"},
		{TypeID: s.typeID("Heavy Missile Launcher II"), Quantity: 1, Flag: "HiSlot4"},
		{TypeID: s.typeID("Heavy Missile Launcher II"), Quantity: 1, Flag: "HiSlot5"},
		{TypeID: s.typeID("Medium Shield Extender II"), Quantity: 1, Flag: "MedSlot0"},
		{TypeID: s.typeID("Damage Control II"), Quantity: 1, Flag: "LowSlot0"},
		{TypeID: s.typeID("Ballistic Control System II"), Quantity: 1, Flag: "LowSlot1"},
		{TypeID: s.typeID("Ballistic Control System II"), Quantity: 1, Flag: "LowSlot2"},
		{TypeID: s.typeID("Scourge Heavy Missile"), Quantity: 3000, Flag: "Cargo"},
	}
	fittings := esi.Fittings{
		{FittingID: 1, Name: "Tama Rifter", ShipTypeID: s.typeID("Rifter"), Items: rifterItems},
		{FittingID: 2, Name: "Mission Drake", ShipTypeID: s.typeID("Drake"), Items: drakeItems},
	}
	if err := s.snap(ctx, id, esi.SnapFittings, fittings); err != nil {
		return err
	}
	if err := s.snap(ctx, id, esi.SnapCorpRoles, esi.CharacterRoles{Roles: []string{"Director"}}); err != nil {
		return err
	}
	return nil
}

// demoKillmailHashes are the invented hashes for the demo's two
// killmail details; they unlock nothing, they only have to look
// like the real thing next to the IDs.
const (
	demoKillmailHashLoss = "3f9a1c4e5b7d2098a6f1e3c5b7092d4a6f8e1b3c"
	demoKillmailHashKill = "b7d2098a6f1e3c5f9a1c4e7092d4a6f8e1b3c5d7"
)

// writePilotOneKillmails seeds Sable's recent killmails: a loss
// to her Tama rival and the revenge kill, with detail rows.
func (s *demoSeed) writePilotOneKillmails(ctx context.Context) error {
	id := demoCharacterID
	now := s.now
	hour := time.Hour
	day := 24 * hour
	q := s.app.queries
	t := s.typeID

	// Two and a half weeks of lowsec frigate and destroyer work
	// around Tama, Kedama and Sujarento with the corp: solo kills, a gang kill on a
	// cruiser, and the two ships Sable lost (both claimed on SRP).
	type fight struct {
		id        int64
		hash      string
		ago       time.Duration
		system    int64
		victim    esi.KillmailVictim
		attackers []esi.KillmailAttacker
	}
	fights := []fight{
		{2900007001, demoKillmailHashLoss, 2 * day, s.tamaSystemID,
			esi.KillmailVictim{CharacterID: id, CorporationID: demoCorpID, ShipTypeID: t("Thrasher"), DamageTaken: 8941, Items: []esi.KillmailVictimItem{
				{ItemTypeID: t("Damage Control II"), QuantityDestroyed: 1},
				{ItemTypeID: t("Gyrostabilizer II"), QuantityDropped: 1},
				{ItemTypeID: t("1MN Afterburner II"), QuantityDestroyed: 1},
				{ItemTypeID: t("200mm AutoCannon II"), QuantityDropped: 3},
			}},
			[]esi.KillmailAttacker{
				{CharacterID: demoCharIlya, CorporationID: demoCorpHaleID, ShipTypeID: t("Merlin"), FinalBlow: true, DamageDone: 5120},
				{CharacterID: demoCharKato, CorporationID: demoCorpWatchID, ShipTypeID: t("Tristan"), DamageDone: 3821},
			}},
		{2900007002, demoKillmailHashKill, 26 * hour, s.tamaSystemID,
			esi.KillmailVictim{CharacterID: demoCharIlya, CorporationID: demoCorpHaleID, ShipTypeID: t("Punisher"), DamageTaken: 6210, Items: []esi.KillmailVictimItem{
				{ItemTypeID: t("Small Armor Repairer II"), QuantityDestroyed: 1},
				{ItemTypeID: t("Energized Adaptive Nano Membrane II"), QuantityDropped: 1},
			}},
			[]esi.KillmailAttacker{
				{CharacterID: id, CorporationID: demoCorpID, ShipTypeID: t("Rifter"), FinalBlow: true, DamageDone: 4105},
				{CharacterID: demoCharDario, CorporationID: demoCorpID, ShipTypeID: t("Merlin"), DamageDone: 2105},
			}},
		{2900007003, "", 4 * day, s.kedamaSystemID,
			esi.KillmailVictim{CharacterID: demoCharKato, CorporationID: demoCorpWatchID, ShipTypeID: t("Tristan"), DamageTaken: 3404, Items: []esi.KillmailVictimItem{
				{ItemTypeID: t("Hobgoblin II"), QuantityDestroyed: 2, QuantityDropped: 1},
				{ItemTypeID: t("Warp Scrambler II"), QuantityDropped: 1},
				{ItemTypeID: t("Small Armor Repairer II"), QuantityDestroyed: 1},
			}},
			[]esi.KillmailAttacker{
				{CharacterID: id, CorporationID: demoCorpID, ShipTypeID: t("Rifter"), FinalBlow: true, DamageDone: 2180},
				{CharacterID: demoCharSera, CorporationID: demoCorpID, ShipTypeID: t("Thrasher"), DamageDone: 1224},
			}},
		{2900007004, "", 6 * day, s.tamaSystemID,
			esi.KillmailVictim{CharacterID: demoCharRhen, CorporationID: demoCorpWatchID, ShipTypeID: t("Catalyst"), DamageTaken: 4870, Items: []esi.KillmailVictimItem{
				{ItemTypeID: t("Fusion S"), QuantityDropped: 800},
				{ItemTypeID: t("Damage Control II"), QuantityDestroyed: 1},
			}},
			[]esi.KillmailAttacker{
				{CharacterID: id, CorporationID: demoCorpID, ShipTypeID: t("Thrasher"), FinalBlow: true, DamageDone: 4870},
			}},
		{2900007005, "", 9 * day, s.sujaSystemID,
			esi.KillmailVictim{CharacterID: id, CorporationID: demoCorpID, ShipTypeID: t("Rifter"), DamageTaken: 3150, Items: []esi.KillmailVictimItem{
				{ItemTypeID: t("200mm AutoCannon II"), QuantityDestroyed: 2},
				{ItemTypeID: t("Warp Scrambler II"), QuantityDropped: 1},
				{ItemTypeID: t("Republic Fleet EMP S"), QuantityDestroyed: 400},
			}},
			[]esi.KillmailAttacker{
				{CharacterID: demoCharLio, CorporationID: demoCorpWatchID, ShipTypeID: t("Vexor"), FinalBlow: true, DamageDone: 2410},
				{CharacterID: demoCharKato, CorporationID: demoCorpWatchID, ShipTypeID: t("Tristan"), DamageDone: 740},
			}},
		{2900007006, "", 13 * day, s.kedamaSystemID,
			esi.KillmailVictim{CharacterID: demoCharLio, CorporationID: demoCorpWatchID, ShipTypeID: t("Vexor"), DamageTaken: 21460, Items: []esi.KillmailVictimItem{
				{ItemTypeID: t("Hobgoblin II"), QuantityDestroyed: 3, QuantityDropped: 2},
				{ItemTypeID: t("Medium Shield Extender II"), QuantityDropped: 1},
				{ItemTypeID: t("Stasis Webifier II"), QuantityDestroyed: 1},
				{ItemTypeID: t("Damage Control II"), QuantityDropped: 1},
			}},
			[]esi.KillmailAttacker{
				{CharacterID: demoCharSera, CorporationID: demoCorpID, ShipTypeID: t("Drake"), FinalBlow: true, DamageDone: 7920},
				{CharacterID: id, CorporationID: demoCorpID, ShipTypeID: t("Thrasher"), DamageDone: 6310},
				{CharacterID: demoCharMina, CorporationID: demoCorpID, ShipTypeID: t("Vexor"), DamageDone: 4480},
				{CharacterID: demoCharDario, CorporationID: demoCorpID, ShipTypeID: t("Merlin"), DamageDone: 2750},
			}},
		{2900007007, "", 17 * day, s.tamaSystemID,
			esi.KillmailVictim{CharacterID: demoCharKato, CorporationID: demoCorpWatchID, ShipTypeID: t("Atron"), DamageTaken: 2280, Items: []esi.KillmailVictimItem{
				{ItemTypeID: t("1MN Afterburner II"), QuantityDropped: 1},
				{ItemTypeID: t("Warp Scrambler II"), QuantityDestroyed: 1},
			}},
			[]esi.KillmailAttacker{
				{CharacterID: id, CorporationID: demoCorpID, ShipTypeID: t("Rifter"), FinalBlow: true, DamageDone: 2280},
			}},
		// One Tama gang fight three days back, the corp against
		// the Kedama Watch: three kills inside six minutes. The
		// battle reports step (demo_battles.go) writes the game
		// log these mails are the other half of.
		{2900007011, "", 3*day - 100*time.Second, s.tamaSystemID,
			esi.KillmailVictim{CharacterID: demoCharRhen, CorporationID: demoCorpWatchID, ShipTypeID: t("Merlin"), DamageTaken: 5310, Items: []esi.KillmailVictimItem{
				{ItemTypeID: t("Damage Control II"), QuantityDestroyed: 1},
				{ItemTypeID: t("Medium Shield Extender II"), QuantityDropped: 1},
			}},
			[]esi.KillmailAttacker{
				{CharacterID: demoCharSera, CorporationID: demoCorpID, ShipTypeID: t("Drake"), FinalBlow: true, DamageDone: 2920},
				{CharacterID: id, CorporationID: demoCorpID, ShipTypeID: t("Thrasher"), DamageDone: 1710},
				{CharacterID: demoCharDario, CorporationID: demoCorpID, ShipTypeID: t("Merlin"), DamageDone: 680},
			}},
		{2900007012, "", 3*day - 235*time.Second, s.tamaSystemID,
			esi.KillmailVictim{CharacterID: demoCharKato, CorporationID: demoCorpWatchID, ShipTypeID: t("Rifter"), DamageTaken: 3980, Items: []esi.KillmailVictimItem{
				{ItemTypeID: t("200mm AutoCannon II"), QuantityDropped: 2},
				{ItemTypeID: t("Warp Scrambler II"), QuantityDestroyed: 1},
			}},
			[]esi.KillmailAttacker{
				{CharacterID: id, CorporationID: demoCorpID, ShipTypeID: t("Thrasher"), FinalBlow: true, DamageDone: 2140},
				{CharacterID: demoCharMina, CorporationID: demoCorpID, ShipTypeID: t("Vexor"), DamageDone: 1260},
				{CharacterID: demoCharDario, CorporationID: demoCorpID, ShipTypeID: t("Merlin"), DamageDone: 580},
			}},
		{2900007013, "", 3*day - 370*time.Second, s.tamaSystemID,
			esi.KillmailVictim{CharacterID: demoCharLio, CorporationID: demoCorpWatchID, ShipTypeID: t("Vexor"), DamageTaken: 18360, Items: []esi.KillmailVictimItem{
				{ItemTypeID: t("Hobgoblin II"), QuantityDestroyed: 3, QuantityDropped: 2},
				{ItemTypeID: t("Damage Control II"), QuantityDropped: 1},
				{ItemTypeID: t("Stasis Webifier II"), QuantityDestroyed: 1},
			}},
			[]esi.KillmailAttacker{
				{CharacterID: demoCharSera, CorporationID: demoCorpID, ShipTypeID: t("Drake"), FinalBlow: true, DamageDone: 7540},
				{CharacterID: id, CorporationID: demoCorpID, ShipTypeID: t("Thrasher"), DamageDone: 5420},
				{CharacterID: demoCharMina, CorporationID: demoCorpID, ShipTypeID: t("Vexor"), DamageDone: 3310},
				{CharacterID: demoCharDario, CorporationID: demoCorpID, ShipTypeID: t("Merlin"), DamageDone: 2090},
			}},
	}

	var refs, corpRefs []esi.KillmailRef
	for _, f := range fights {
		if f.victim.ShipTypeID == 0 || f.system == 0 {
			continue // a hull this SDE does not know, or no Tama
		}
		hash := f.hash
		if hash == "" {
			hash = demoKillmailHash(f.id)
		}
		km := esi.Killmail{
			KillmailID: f.id, KillmailTime: s.rfc(now.Add(-f.ago)), SolarSystemID: f.system,
			Victim: f.victim, Attackers: f.attackers,
		}
		raw, err := json.Marshal(km)
		if err != nil {
			return err
		}
		if err := q.UpsertKillmailDetail(ctx, db.UpsertKillmailDetailParams{
			KillmailID: km.KillmailID, CharacterID: id, Hash: hash,
			Payload: string(raw), FetchedAt: now,
		}); err != nil {
			return err
		}
		refs = append(refs, esi.KillmailRef{KillmailID: f.id, KillmailHash: hash})
		corpRefs = append(corpRefs, esi.KillmailRef{KillmailID: f.id, KillmailHash: hash})
	}
	s.corpKillmails = corpRefs
	return s.snap(ctx, id, esi.SnapKillmails, refs)
}

// demoKillmailHash is a stable, hash-shaped stand-in for a seeded
// killmail's hash: the same mail always gets the same one.
func demoKillmailHash(killmailID int64) string {
	sum := sha1.Sum([]byte(fmt.Sprintf("evesynapse-demo-killmail-%d", killmailID)))
	return hex.EncodeToString(sum[:])
}

// demoColony is one seeded colony: the summary row and the pin
// layout behind it.
type demoColony struct {
	PlanetID   int64
	PlanetName string
	Type       string
	SystemID   int64
	Upgrade    int64
	CCName     string // the command center type name for this planet type
	Extractor  string // the P0 commodity the extractor pulls
	Product    string // the P1 commodity the factory makes
	Schematic  int64
	// Expiry is when the extractor program ends, relative to
	// the seeding moment; zero keeps the default two days out.
	// A negative value seeds an already-expired extractor.
	Expiry time.Duration
}

func (s *demoSeed) writeColony(ctx context.Context, characterID int64, c demoColony) error {
	now := s.now
	hour := time.Hour
	day := 24 * hour
	expiry := now.Add(2 * day)
	if c.Expiry != 0 {
		expiry = now.Add(c.Expiry)
	}
	pins := []esi.PlanetPin{
		{PinID: 1, TypeID: s.typeID(c.CCName), InstallTime: s.rfc(now.Add(-90 * day))},
		{PinID: 2, TypeID: s.typeID("Extractor Control Unit"), InstallTime: s.rfc(now.Add(-2 * day)),
			LastCycleStart: s.rfc(now.Add(-30 * time.Minute)), ExpiryTime: s.rfc(expiry),
			ExtractorDetails: &esi.PlanetExtractor{
				ProductTypeID: s.typeID(c.Extractor), CycleTime: 1800, QtyPerCycle: 850,
				Heads: []esi.PlanetExtractorHead{
					{HeadID: 0, Latitude: 0.42, Longitude: 1.13},
					{HeadID: 1, Latitude: 0.44, Longitude: 1.16},
					{HeadID: 2, Latitude: 0.40, Longitude: 1.10},
					{HeadID: 3, Latitude: 0.43, Longitude: 1.19},
				},
			}},
		{PinID: 3, TypeID: s.typeID("Storage Facility"), InstallTime: s.rfc(now.Add(-90 * day)),
			Contents: []esi.PlanetPinContent{
				{TypeID: s.typeID(c.Extractor), Amount: 2400},
				{TypeID: s.typeID(c.Product), Amount: 620},
			}},
		{PinID: 4, TypeID: s.typeID("Basic Industry Facility"), SchematicID: c.Schematic,
			InstallTime:    s.rfc(now.Add(-60 * day)),
			FactoryDetails: &esi.PlanetFactory{SchematicID: c.Schematic},
			Contents:       []esi.PlanetPinContent{{TypeID: s.typeID(c.Product), Amount: 40}}},
	}
	layout := esi.PlanetLayout{
		Pins: pins,
		Links: []esi.PlanetLink{
			{SourcePinID: 1, DestinationPinID: 2, LinkLevel: 0},
			{SourcePinID: 2, DestinationPinID: 3, LinkLevel: 0},
			{SourcePinID: 3, DestinationPinID: 4, LinkLevel: 0},
		},
		Routes: []esi.PlanetRoute{
			{RouteID: 1, SourcePinID: 2, DestinationPinID: 4, ContentTypeID: s.typeID(c.Extractor), Quantity: 40, Waypoints: []int64{3}},
		},
	}
	if err := s.snap(ctx, characterID, esi.PlanetLayoutKind(c.PlanetID), layout); err != nil {
		return err
	}
	if err := s.app.queries.SetPlanetName(ctx, db.SetPlanetNameParams{
		PlanetID: c.PlanetID, Name: c.PlanetName, State: esi.PlanetResolved,
		ResolvedAt: sql.NullTime{Time: now, Valid: true},
	}); err != nil {
		return err
	}
	colony := esi.Colony{
		PlanetID: c.PlanetID, OwnerID: characterID, SolarSystemID: c.SystemID,
		PlanetType: c.Type, NumPins: 4, UpgradeLevel: c.Upgrade,
		LastUpdate: s.rfc(now.Add(-1 * hour)),
	}
	s.pendingColonies[characterID] = append(s.pendingColonies[characterID], colony)
	return nil
}

// writeColonies stores the colonies snapshot for a pilot from
// the colonies her writeColony calls gathered.
func (s *demoSeed) writeColonies(ctx context.Context, characterID int64) error {
	return s.snap(ctx, characterID, esi.SnapPlanets, esi.Colonies(s.pendingColonies[characterID]))
}

// writePilotOneComms seeds Sable's mail, calendar and contacts.
func (s *demoSeed) writePilotOneComms(ctx context.Context) error {
	id := demoCharacterID
	now := s.now
	hour := time.Hour
	day := 24 * hour

	if err := s.writeColony(ctx, id, demoColony{
		PlanetID: 40009076, PlanetName: "Jita IV", Type: "temperate", SystemID: s.jitaSystemID,
		Upgrade: 5, CCName: "Temperate Command Center",
		Extractor: "Micro Organisms", Product: "Bacteria", Schematic: 116,
	}); err != nil {
		return err
	}
	if err := s.writeColony(ctx, id, demoColony{
		PlanetID: 40009079, PlanetName: "Jita VII", Type: "barren", SystemID: s.jitaSystemID,
		Upgrade: 4, CCName: "Barren Command Center",
		Extractor: "Base Metals", Product: "Reactive Metals", Schematic: 117,
	}); err != nil {
		return err
	}
	// The neglected one: its extractor program ended a day
	// ago, which is the PI card's expired state.
	if err := s.writeColony(ctx, id, demoColony{
		PlanetID: 40009081, PlanetName: "Jita VI", Type: "plasma", SystemID: s.jitaSystemID,
		Upgrade: 3, CCName: "Plasma Command Center",
		Extractor: "Suspended Plasma", Product: "Plasmoids", Schematic: 119,
		Expiry: -24 * hour,
	}); err != nil {
		return err
	}
	if err := s.writeColonies(ctx, id); err != nil {
		return err
	}

	mails := []struct {
		header esi.MailHeader
		body   string
	}{
		{esi.MailHeader{MailID: 3900000001, From: demoCharMarcus, Subject: "Your Rifter is ready in Jita", Timestamp: s.rfc(now.Add(-5 * hour)), IsRead: false, Labels: []int64{1001}, Recipients: []esi.MailRecipient{{RecipientID: id, RecipientType: "character"}}},
			"<p>Sable,</p><p>The Rifter from the contract is fitted and sitting in your hangar at Jita IV-4, EMP loaded. Try not to feed this one to Ilya before Saturday.</p><p>Marcus</p>"},
		{esi.MailHeader{MailID: 3900000002, From: demoCharSera, Subject: "Saturday op: Tama roam, 19:00", Timestamp: s.rfc(now.Add(-1 * day)), IsRead: false, Labels: []int64{1002}, Recipients: []esi.MailRecipient{{RecipientID: demoCorpID, RecipientType: "corporation"}}},
			"<p>All,</p><p>Form-up is Jita IV-4 at 19:00 on Saturday. Rifters and Thrashers, SRP is on. Dario has the route, Mina is bringing links. New pilots welcome, loaner fits are in the corp hangar.</p><p>Sera</p>"},
		{esi.MailHeader{MailID: 3900000003, From: demoCharPetra, Subject: "Re: PI pickup on Sunday", Timestamp: s.rfc(now.Add(-2 * day)), IsRead: true, Labels: []int64{}, Recipients: []esi.MailRecipient{{RecipientID: id, RecipientType: "character"}}},
			"<p>Pickup moved to Sunday after downtime, same customs office. Your Bacteria run is looking healthy, by the way. The new extractor heads paid off.</p><p>Petra</p>"},
		{esi.MailHeader{MailID: 3900000004, From: demoCharDario, Subject: "Route for Saturday", Timestamp: s.rfc(now.Add(-3 * day)), IsRead: true, Labels: []int64{1002}, Recipients: []esi.MailRecipient{{RecipientID: demoCorpID, RecipientType: "corporation"}}},
			"<p>Tama first, then Kedama, back out through Sujarento. Kedama Watch has been sitting on the Tama gate in the evenings, so scouts go ahead and nobody jumps blind.</p><p>Dario</p>"},
		{esi.MailHeader{MailID: 3900000005, From: demoCharMarcus, Subject: "Picked up your courier", Timestamp: s.rfc(now.Add(-19 * hour)), IsRead: false, Labels: []int64{1001}, Recipients: []esi.MailRecipient{{RecipientID: id, RecipientType: "character"}}},
			"<p>Got the Jita to Amarr run, 12,500 m3. Freighter undocks after downtime, you should see it in Amarr tomorrow evening.</p><p>Marcus</p>"},
		{esi.MailHeader{MailID: 3900000006, From: demoCharKato, Subject: "gf", Timestamp: s.rfc(now.Add(-2*day + 40*time.Minute)), IsRead: true, Labels: []int64{1001}, Recipients: []esi.MailRecipient{{RecipientID: id, RecipientType: "character"}}},
			"<p>gf in the Thrasher. Ilya says hi. Kedama Watch is recruiting if you get tired of buying new ships.</p><p>Kato</p>"},
		{esi.MailHeader{MailID: 3900000007, From: demoCharSera, Subject: "SRP paid: Rifter, Kedama roam", Timestamp: s.rfc(now.Add(-7 * day)), IsRead: true, Labels: []int64{1002}, Recipients: []esi.MailRecipient{{RecipientID: id, RecipientType: "character"}}},
			"<p>Paid 11,850,000 ISK for the Rifter you lost holding Lio's Vexor on the Sujarento gate. That tackle got us the kill. Thanks for staying on it.</p><p>Sera</p>"},
	}
	headers := make([]esi.MailHeader, 0, len(mails))
	for _, m := range mails {
		headers = append(headers, m.header)
		if err := s.snap(ctx, id, esi.MailBodyKind(m.header.MailID), esi.Mail{
			From: m.header.From, Subject: m.header.Subject, Body: m.body,
			Timestamp: m.header.Timestamp, Read: m.header.IsRead,
			Labels: m.header.Labels, Recipients: m.header.Recipients,
		}); err != nil {
			return err
		}
	}
	if err := s.snap(ctx, id, esi.SnapMail, esi.MailHeaders(headers)); err != nil {
		return err
	}
	if err := s.snap(ctx, id, esi.SnapMailLabels, esi.MailLabels{
		Labels: []esi.MailLabel{
			{LabelID: 1001, Name: "Contracts", Color: "#2f81f7", UnreadCount: 1},
			{LabelID: 1002, Name: "Corp", Color: "#d29922", UnreadCount: 1},
		},
		TotalUnreadCount: 2,
	}); err != nil {
		return err
	}
	if err := s.snap(ctx, id, esi.SnapMailLists, esi.MailLists{
		{MailingListID: 290000555, Name: "Test Pilot Collective"},
	}); err != nil {
		return err
	}

	if err := s.writeCalendar(ctx, id, []int64{demoEventMining, demoEventRoam}); err != nil {
		return err
	}
	contacts := []esi.Contact{
		{ContactID: demoCharacter2ID, ContactType: "character", Standing: 10, IsWatched: true},
		{ContactID: demoCharMarcus, ContactType: "character", Standing: 7.5, IsWatched: true},
		{ContactID: demoCharPetra, ContactType: "character", Standing: 5},
		{ContactID: demoCorpHaleID, ContactType: "corporation", Standing: 3},
		{ContactID: demoCharIlya, ContactType: "character", Standing: -6.5},
	}
	if err := s.snap(ctx, id, esi.SnapContacts, esi.Contacts(contacts)); err != nil {
		return err
	}
	return nil
}

// The demo's two calendar events, shared by both pilots.
const (
	demoEventMining = int64(3900001001)
	demoEventRoam   = int64(3900001002)
)

// writeCalendar seeds one pilot's calendar: summaries for the
// given events, their detail snapshots and attendee lists.
func (s *demoSeed) writeCalendar(ctx context.Context, characterID int64, eventIDs []int64) error {
	now := s.now
	hour := time.Hour
	day := 24 * hour
	events := map[int64]struct {
		summary   esi.CalendarEventSummary
		detail    esi.CalendarEvent
		attendees []esi.CalendarAttendee
	}{
		demoEventMining: {
			summary: esi.CalendarEventSummary{EventID: demoEventMining, EventDate: s.rfc(now.Add(2 * day)), Title: "Corp mining op: Amarr belts", Importance: 0, EventResponse: "accepted"},
			detail:  esi.CalendarEvent{EventID: demoEventMining, Date: s.rfc(now.Add(2 * day)), Duration: 120, Importance: 0, OwnerID: demoCorpID, OwnerName: demoCorpName, OwnerType: "corporation", Response: "accepted", Title: "Corp mining op: Amarr belts", Text: "Boosts on grid from 18:00. Bring a barge or a Venture; the corp buys the ore at Jita split."},
			attendees: []esi.CalendarAttendee{
				{CharacterID: demoCharacterID, EventResponse: "accepted"},
				{CharacterID: demoCharacter2ID, EventResponse: "accepted"},
				{CharacterID: demoCharSera, EventResponse: "tentative"},
				{CharacterID: demoCharMina, EventResponse: "not_responded"},
			},
		},
		demoEventRoam: {
			summary: esi.CalendarEventSummary{EventID: demoEventRoam, EventDate: s.rfc(now.Add(5 * day)), Title: "Tama roam form-up", Importance: 1, EventResponse: "accepted"},
			detail:  esi.CalendarEvent{EventID: demoEventRoam, Date: s.rfc(now.Add(5 * day)), Duration: 90, Importance: 1, OwnerID: demoCharacterID, OwnerName: demoCharName, OwnerType: "character", Response: "accepted", Title: "Tama roam form-up", Text: "The Saturday roam from Sera's mail. Form-up Jita IV-4, 19:00. SRP covers T1 frigates and destroyers."},
			attendees: []esi.CalendarAttendee{
				{CharacterID: demoCharacterID, EventResponse: "accepted"},
				{CharacterID: demoCharDario, EventResponse: "accepted"},
				{CharacterID: demoCharSera, EventResponse: "accepted"},
			},
		},
	}
	summaries := make([]esi.CalendarEventSummary, 0, len(eventIDs))
	for _, eventID := range eventIDs {
		ev, ok := events[eventID]
		if !ok {
			continue
		}
		summaries = append(summaries, ev.summary)
		if err := s.snap(ctx, characterID, esi.CalendarEventKind(eventID), ev.detail); err != nil {
			return err
		}
		if err := s.snap(ctx, characterID, esi.CalendarAttendeesKind(eventID), esi.CalendarAttendees(ev.attendees)); err != nil {
			return err
		}
	}
	return s.snap(ctx, characterID, esi.SnapCalendar, esi.CalendarEventSummaries(summaries))
}

// writePilotTwo seeds Juno Park: the industry alt. Her skills
// are a working subset part-way up their plans, her queue is
// mid-flight, and her industry book is busier than her CEO's.
func (s *demoSeed) writePilotTwo(ctx context.Context) error {
	id := demoCharacter2ID
	now := s.now
	hour := time.Hour
	day := 24 * hour
	q := s.app.queries

	if err := s.snap(ctx, id, esi.SnapLoyalty, esi.LoyaltyBalances{
		{CorporationID: 1000035, LoyaltyPoints: 61400}, // Caldari Navy
	}); err != nil {
		return err
	}

	if err := s.snap(ctx, id, esi.SnapProfile, esi.Character{
		Name: demoChar2Name, CorporationID: demoCorpID,
		Birthday: "2019-06-02T14:03:00Z", SecurityStatus: 3.1,
		Title:       "Factory Manager",
		Description: "Industry alt of the Test Pilot Collective's CEO. Builds the ships the rest of the corp loses.",
	}); err != nil {
		return err
	}

	// A working subset of skills at working levels.
	trained := map[string]int64{
		"Mining": 5, "Astrogeology": 4, "Industry": 5, "Mass Production": 4,
		"Advanced Mass Production": 3, "Reprocessing": 5, "Reprocessing Efficiency": 4,
		"Metallurgy": 4, "Science": 5, "Laboratory Operation": 4,
		"Advanced Laboratory Operation": 3, "Research": 5, "Mining Foreman": 3,
		"Leadership": 3, "Spaceship Command": 5, "Mining Barge": 5, "Exhumers": 3,
		"ORE Industrial": 4, "Veldspar Processing": 4, "Scordite Processing": 4,
		"Pyroxeres Processing": 3, "Drones": 4, "Drone Interfacing": 3,
		"Shield Operation": 4, "Energy Grid Upgrades": 4, "CPU Management": 5,
		"Power Grid Management": 5, "Navigation": 4, "Afterburner": 4, "Trade": 4,
		"Retail": 3, "Broker Relations": 4, "Accounting": 4, "Cybernetics": 3,
		"Biology": 3,
	}
	skills := esi.Skills{}
	for name, level := range trained {
		typeID := s.typeID(name)
		if typeID == 0 {
			continue
		}
		sp := spAtLevel(s.ranks[typeID], level)
		skills.Skills = append(skills.Skills, esi.Skill{
			SkillID: typeID, SkillpointsInSkill: sp,
			ActiveSkillLevel: int(level), TrainedSkillLevel: int(level),
		})
		skills.TotalSP += sp
	}
	if err := s.snap(ctx, id, esi.SnapSkills, skills); err != nil {
		return err
	}
	queueFor := func(name string, level int64, start, finish time.Time, position int) (esi.SkillqueueEntry, bool) {
		typeID := s.typeID(name)
		if typeID == 0 {
			return esi.SkillqueueEntry{}, false
		}
		rank := s.ranks[typeID]
		startSP := spAtLevel(rank, level-1)
		return esi.SkillqueueEntry{
			SkillID: typeID, FinishedLevel: int(level), QueuePosition: position,
			StartDate: s.rfc(start), FinishDate: s.rfc(finish),
			TrainingStartSP: &startSP, LevelStartSP: startSP, LevelEndSP: spAtLevel(rank, level),
		}, true
	}
	queue := esi.Skillqueue{}
	for i, spec := range []struct {
		name  string
		level int64
		start time.Time
		end   time.Time
	}{
		{"Advanced Mass Production", 4, now.Add(-1 * day), now.Add(2 * day)},
		{"Astrogeology", 5, now.Add(2 * day), now.Add(9 * day)},
		{"Mining Foreman", 4, now.Add(9 * day), now.Add(12 * day)},
	} {
		if entry, ok := queueFor(spec.name, spec.level, spec.start, spec.end, i); ok {
			queue = append(queue, entry)
		}
	}
	if err := s.snap(ctx, id, esi.SnapSkillqueue, queue); err != nil {
		return err
	}
	if err := s.snap(ctx, id, esi.SnapAttributes, esi.Attributes{
		Intelligence: 30, Memory: 31, Perception: 24, Willpower: 24, Charisma: 22,
		BonusRemaps: intPtr(2), LastRemapDate: s.rfc(now.Add(-120 * day)),
	}); err != nil {
		return err
	}

	const wallet = 96450210.75
	if err := s.snap(ctx, id, esi.SnapWallet, json.RawMessage(fmt.Sprintf("%.2f", wallet))); err != nil {
		return err
	}
	journal := []esi.WalletJournalEntry{
		{ID: 8800002019, Date: s.rfc(now.Add(-4 * hour)), RefType: "transaction_tax", Amount: -615000, FirstPartyID: id, FirstPartyType: "character", SecondPartyID: demoCorpHaleID, SecondPartyType: "corporation", Description: "Transaction tax"},
		{ID: 8800002015, Date: s.rfc(now.Add(-4 * hour)), RefType: "market_transaction", Amount: 12300000, FirstPartyID: demoCharPetra, FirstPartyType: "character", SecondPartyID: id, SecondPartyType: "character", Description: "Market transaction"},
		{ID: 8800002014, Date: s.rfc(now.Add(-11 * day)), RefType: "corporation_account_withdrawal", Amount: 9800000, FirstPartyID: demoCorpID, FirstPartyType: "corporation", SecondPartyID: id, SecondPartyType: "character", Description: "Corporation account withdrawal"},
		{ID: 8800002018, Date: s.rfc(now.Add(-25 * hour)), RefType: "transaction_tax", Amount: -384000, FirstPartyID: id, FirstPartyType: "character", SecondPartyID: demoCorpHaleID, SecondPartyType: "corporation", Description: "Transaction tax"},
		{ID: 8800002017, Date: s.rfc(now.Add(-25 * hour)), RefType: "market_transaction", Amount: 7680000, FirstPartyID: demoCharSera, FirstPartyType: "character", SecondPartyID: id, SecondPartyType: "character", Description: "Market transaction"},
		{ID: 8800002013, Date: s.rfc(now.Add(-2 * day)), RefType: "manufacturing", Amount: -640000, FirstPartyID: id, FirstPartyType: "character", SecondPartyID: demoCorpHaleID, SecondPartyType: "corporation", Description: "Manufacturing job", ContextID: 8800009101, ContextType: "industry_job_id"},
		{ID: 8800002012, Date: s.rfc(now.Add(-3 * day)), RefType: "market_transaction", Amount: -8940000, FirstPartyID: id, FirstPartyType: "character", SecondPartyID: demoCharSera, SecondPartyType: "character", Description: "Market transaction"},
		{ID: 8800002011, Date: s.rfc(now.Add(-5 * day)), RefType: "broker_fee", Amount: -184200, FirstPartyID: id, FirstPartyType: "character", SecondPartyID: demoCorpHaleID, SecondPartyType: "corporation", Description: "Broker fee"},
		{ID: 8800002016, Date: s.rfc(now.Add(-6 * day)), RefType: "market_transaction", Amount: -2975000, FirstPartyID: id, FirstPartyType: "character", SecondPartyID: demoCharMarcus, SecondPartyType: "character", Description: "Market transaction"},
	}
	// Her generated trading history: the Amarr book of the same
	// account pool, weighted to minerals and Isogen and lighter
	// than Sable's Jita book; her ISK mostly comes from the
	// factory and from missions, not from the market.
	book := &demoBookBuilder{s: s, charID: id, station: s.amarrStationID,
		clients: []int64{demoCharPetra, demoCharSera, demoCharMarcus, demoCharMina},
		txnID:   7700020000, jrnlID: 8800020000}
	for i, lane := range []demoTradeLane{
		{typeName: "Tritanium", base: 6.15, lot: 1600000, round: 1000, step: 4, phase: 6},
		{typeName: "Pyerite", base: 12.10, lot: 600000, round: 1000, step: 3, phase: 4},
		{typeName: "Isogen", base: 520, lot: 30000, round: 100, step: 4, phase: 5},
		{typeName: "Mexallon", base: 50.50, lot: 140000, round: 100, step: 5, phase: 7},
		{typeName: "Stasis Webifier II", base: 2150000, lot: 5, round: 1, step: 6, phase: 10},
	} {
		book.lane(lane, i)
	}
	book.lootSale(demoLootSale{typeName: "Nocxium", daysAgo: 21, qty: 900, price: 1180}, demoCharPetra)
	book.lootSale(demoLootSale{typeName: "Nocxium", daysAgo: 9, qty: 1400, price: 1245}, demoCharSera)
	// One of her Vexors, sold a few days after it delivered.
	if vexor := s.typeID("Vexor"); vexor != 0 {
		sellAt := book.at(15, 14)
		jrnlID := book.market(sellAt, demoCharSera, 58500000, true)
		book.trade(sellAt, vexor, 58500000, 1, demoCharSera, false, jrnlID)
		book.tax(sellAt, 58500000)
	}
	for _, line := range []struct {
		daysAgo int
		entry   esi.WalletJournalEntry
	}{
		{12, esi.WalletJournalEntry{RefType: "manufacturing", Amount: -520000, FirstPartyID: id, FirstPartyType: "character", SecondPartyID: demoCorpHaleID, SecondPartyType: "corporation", Description: "Manufacturing job", ContextID: 8800009201, ContextType: "industry_job_id"}},
		{19, esi.WalletJournalEntry{RefType: "manufacturing", Amount: -780000, FirstPartyID: id, FirstPartyType: "character", SecondPartyID: demoCorpHaleID, SecondPartyType: "corporation", Description: "Manufacturing job", ContextID: 8800009202, ContextType: "industry_job_id"}},
		{19, esi.WalletJournalEntry{RefType: "industry_job_tax", Amount: -96000, FirstPartyID: id, FirstPartyType: "character", SecondPartyID: demoCorpHaleID, SecondPartyType: "corporation", Description: "Industry job tax"}},
		{27, esi.WalletJournalEntry{RefType: "manufacturing", Amount: -610000, FirstPartyID: id, FirstPartyType: "character", SecondPartyID: demoCorpHaleID, SecondPartyType: "corporation", Description: "Manufacturing job", ContextID: 8800009203, ContextType: "industry_job_id"}},
		{33, esi.WalletJournalEntry{RefType: "manufacturing", Amount: -455000, FirstPartyID: id, FirstPartyType: "character", SecondPartyID: demoCorpHaleID, SecondPartyType: "corporation", Description: "Manufacturing job", ContextID: 8800009204, ContextType: "industry_job_id"}},
		{14, esi.WalletJournalEntry{RefType: "agent_mission_reward", Amount: 1850000, FirstPartyID: demoCorpHaleID, FirstPartyType: "corporation", SecondPartyID: id, SecondPartyType: "character", Description: "Agent mission reward"}},
		{3, esi.WalletJournalEntry{RefType: "agent_mission_reward", Amount: 2740000, FirstPartyID: demoCorpHaleID, FirstPartyType: "corporation", SecondPartyID: id, SecondPartyType: "character", Description: "Agent mission reward"}},
		{3, esi.WalletJournalEntry{RefType: "agent_mission_time_bonus_reward", Amount: 860000, FirstPartyID: demoCorpHaleID, FirstPartyType: "corporation", SecondPartyID: id, SecondPartyType: "character", Description: "Agent mission time bonus"}},
		{6, esi.WalletJournalEntry{RefType: "agent_mission_reward", Amount: 3120000, FirstPartyID: demoCorpHaleID, FirstPartyType: "corporation", SecondPartyID: id, SecondPartyType: "character", Description: "Agent mission reward"}},
		{10, esi.WalletJournalEntry{RefType: "agent_mission_reward", Amount: 2380000, FirstPartyID: demoCorpHaleID, FirstPartyType: "corporation", SecondPartyID: id, SecondPartyType: "character", Description: "Agent mission reward"}},
		{17, esi.WalletJournalEntry{RefType: "agent_mission_reward", Amount: 3560000, FirstPartyID: demoCorpHaleID, FirstPartyType: "corporation", SecondPartyID: id, SecondPartyType: "character", Description: "Agent mission reward"}},
		{17, esi.WalletJournalEntry{RefType: "agent_mission_time_bonus_reward", Amount: 1040000, FirstPartyID: demoCorpHaleID, FirstPartyType: "corporation", SecondPartyID: id, SecondPartyType: "character", Description: "Agent mission time bonus"}},
		{20, esi.WalletJournalEntry{RefType: "agent_mission_reward", Amount: 2910000, FirstPartyID: demoCorpHaleID, FirstPartyType: "corporation", SecondPartyID: id, SecondPartyType: "character", Description: "Agent mission reward"}},
		{24, esi.WalletJournalEntry{RefType: "agent_mission_reward", Amount: 3340000, FirstPartyID: demoCorpHaleID, FirstPartyType: "corporation", SecondPartyID: id, SecondPartyType: "character", Description: "Agent mission reward"}},
		{28, esi.WalletJournalEntry{RefType: "agent_mission_reward", Amount: 2630000, FirstPartyID: demoCorpHaleID, FirstPartyType: "corporation", SecondPartyID: id, SecondPartyType: "character", Description: "Agent mission reward"}},
		{28, esi.WalletJournalEntry{RefType: "agent_mission_time_bonus_reward", Amount: 720000, FirstPartyID: demoCorpHaleID, FirstPartyType: "corporation", SecondPartyID: id, SecondPartyType: "character", Description: "Agent mission time bonus"}},
		{31, esi.WalletJournalEntry{RefType: "agent_mission_reward", Amount: 3080000, FirstPartyID: demoCorpHaleID, FirstPartyType: "corporation", SecondPartyID: id, SecondPartyType: "character", Description: "Agent mission reward"}},
		{6, esi.WalletJournalEntry{RefType: "player_donation", Amount: 3400000, FirstPartyID: demoCharacterID, FirstPartyType: "character", SecondPartyID: id, SecondPartyType: "character", Description: "Player donation", Reason: "for materials"}},
	} {
		book.journalLine(line.daysAgo, line.entry)
	}
	journal = append(journal, book.journal...)
	sort.SliceStable(journal, func(i, j int) bool { return journal[i].Date > journal[j].Date })
	journalBalances(journal, wallet)
	if err := s.snap(ctx, id, esi.SnapWalletJournal, esi.WalletJournal(journal)); err != nil {
		return err
	}
	txns := []esi.WalletTransaction{
		{TransactionID: 7700002003, Date: s.rfc(now.Add(-4 * hour)), TypeID: s.typeID("Pyerite"), LocationID: s.amarrStationID, UnitPrice: 12.30, Quantity: 1000000, ClientID: demoCharPetra, IsBuy: false, IsPersonal: true, JournalRefID: 8800002015},
		{TransactionID: 7700002004, Date: s.rfc(now.Add(-25 * hour)), TypeID: s.typeID("Mexallon"), LocationID: s.amarrStationID, UnitPrice: 51.20, Quantity: 150000, ClientID: demoCharSera, IsBuy: false, IsPersonal: true, JournalRefID: 8800002017},
		{TransactionID: 7700002002, Date: s.rfc(now.Add(-3 * day)), TypeID: s.typeID("Mexallon"), LocationID: s.amarrStationID, UnitPrice: 44.70, Quantity: 200000, ClientID: demoCharSera, IsBuy: true, IsPersonal: true, JournalRefID: 8800002012},
		{TransactionID: 7700002001, Date: s.rfc(now.Add(-6 * day)), TypeID: s.typeID("Tritanium"), LocationID: s.amarrStationID, UnitPrice: 5.95, Quantity: 500000, ClientID: demoCharMarcus, IsBuy: true, IsPersonal: true, JournalRefID: 8800002016},
	}
	txns = append(txns, book.txns...)
	sort.SliceStable(txns, func(i, j int) bool { return txns[i].Date > txns[j].Date })
	if err := s.snap(ctx, id, esi.SnapWalletTxns, esi.WalletTransactions(txns)); err != nil {
		return err
	}

	orders := []esi.CharOrder{
		{OrderID: 7900000101, TypeID: s.typeID("Pyerite"), LocationID: s.amarrStationID, RegionID: s.domainRegionID, Price: 12.05, VolumeTotal: 650000, VolumeRemain: 400000, MinVolume: 1, Range: "station", Issued: s.rfc(now.Add(-2 * day)), Duration: 90},
	}
	if err := s.snap(ctx, id, esi.SnapOrders, esi.CharOrders(orders)); err != nil {
		return err
	}
	if err := q.UpsertOrderHealth(ctx, db.UpsertOrderHealthParams{
		CharacterID: id, OrderID: 7900000101, TypeID: s.typeID("Pyerite"),
		RegionID: s.domainRegionID, LocationID: s.amarrStationID,
		MyPrice: 12.05, StationBest: 12.05, RegionBest: 11.80,
		Status: "best_region_cheaper", ComputedAt: now,
	}); err != nil {
		return err
	}
	if err := q.UpsertOrderLifecycle(ctx, db.UpsertOrderLifecycleParams{
		CharacterID: id, OrderID: 7900000101, TypeID: s.typeID("Pyerite"),
		LocationID: s.amarrStationID, RegionID: s.domainRegionID,
		ListedPrice: 12.05, VolumeTotal: 650000, VolumeRemainLast: 400000,
		FirstSeenAt: now.Add(-2 * day), LastSeenAt: now,
	}); err != nil {
		return err
	}
	history := []esi.CharOrderHistoryEntry{
		{CharOrder: esi.CharOrder{OrderID: 7900000111, TypeID: s.typeID("Isogen"), LocationID: s.amarrStationID, RegionID: s.domainRegionID, Price: 512.00, VolumeTotal: 80000, VolumeRemain: 21000, MinVolume: 1, Range: "station", Issued: s.rfc(now.Add(-40 * day)), Duration: 30}, State: "expired"},
	}
	if err := s.snap(ctx, id, esi.SnapOrdersHistory, esi.CharOrderHistory(history)); err != nil {
		return err
	}
	if err := s.snap(ctx, id, esi.SnapContracts, esi.Contracts{}); err != nil {
		return err
	}

	jobs := []esi.IndustryJob{
		{JobID: 8800009101, ActivityID: 1, BlueprintTypeID: s.typeID("Thrasher Blueprint"), BlueprintID: demoItemBase + 602, BlueprintLocationID: s.amarrStationID, OutputLocationID: s.amarrStationID, FacilityID: s.amarrStationID, StationID: s.amarrStationID, InstallerID: id, Runs: 5, LicensedRuns: 5, Status: "active", StartDate: s.rfc(now.Add(-2 * day)), EndDate: s.rfc(now.Add(1 * day)), Duration: 259200, Cost: 640000, ProductTypeID: s.typeID("Thrasher")},
		{JobID: 8800009102, ActivityID: 3, BlueprintTypeID: s.typeID("Covetor Blueprint"), BlueprintID: demoItemBase + 601, BlueprintLocationID: s.amarrStationID, OutputLocationID: s.amarrStationID, FacilityID: s.amarrStationID, StationID: s.amarrStationID, InstallerID: id, Runs: 1, LicensedRuns: 1, Status: "active", StartDate: s.rfc(now.Add(-3 * day)), EndDate: s.rfc(now.Add(4 * day)), Duration: 604800, Cost: 412000, ProductTypeID: s.typeID("Covetor Blueprint")},
		{JobID: 8800009103, ActivityID: 1, BlueprintTypeID: s.typeID("Vexor Blueprint"), BlueprintID: demoItemBase + 603, BlueprintLocationID: s.amarrStationID, OutputLocationID: s.amarrStationID, FacilityID: s.amarrStationID, StationID: s.amarrStationID, InstallerID: id, Runs: 2, LicensedRuns: 2, SuccessfulRuns: 2, Status: "delivered", StartDate: s.rfc(now.Add(-6 * day)), EndDate: s.rfc(now.Add(-5 * day)), CompletedDate: s.rfc(now.Add(-5 * day)), Duration: 86400, Cost: 980000, ProductTypeID: s.typeID("Vexor"), CompletedCharacterID: id},
		{JobID: 8800009104, ActivityID: 1, BlueprintTypeID: s.typeID("Vexor Blueprint"), BlueprintID: demoItemBase + 603, BlueprintLocationID: s.amarrStationID, OutputLocationID: s.amarrStationID, FacilityID: s.amarrStationID, StationID: s.amarrStationID, InstallerID: id, Runs: 3, LicensedRuns: 3, SuccessfulRuns: 3, Status: "delivered", StartDate: s.rfc(now.Add(-20 * day)), EndDate: s.rfc(now.Add(-19 * day)), CompletedDate: s.rfc(now.Add(-19 * day)), Duration: 86400, Cost: 1450000, ProductTypeID: s.typeID("Vexor"), CompletedCharacterID: id},
	}
	if err := s.snap(ctx, id, esi.SnapIndustryJobs, esi.IndustryJobs(jobs)); err != nil {
		return err
	}
	blueprints := []esi.Blueprint{
		{ItemID: demoItemBase + 601, TypeID: s.typeID("Covetor Blueprint"), LocationID: s.amarrStationID, LocationFlag: "Hangar", MaterialEfficiency: 10, TimeEfficiency: 16, Quantity: -1, Runs: -1},
		{ItemID: demoItemBase + 602, TypeID: s.typeID("Thrasher Blueprint"), LocationID: s.amarrStationID, LocationFlag: "Hangar", MaterialEfficiency: 9, TimeEfficiency: 12, Quantity: -2, Runs: 15},
		{ItemID: demoItemBase + 603, TypeID: s.typeID("Vexor Blueprint"), LocationID: s.amarrStationID, LocationFlag: "Hangar", MaterialEfficiency: 10, TimeEfficiency: 20, Quantity: -1, Runs: -1},
	}
	if err := s.snap(ctx, id, esi.SnapBlueprints, esi.Blueprints(blueprints)); err != nil {
		return err
	}
	mining := []esi.MiningEntry{
		{Date: now.Add(-1 * day).Format("2006-01-02"), TypeID: s.typeID("Veldspar"), SolarSystemID: s.amarrSystemID, Quantity: 26400},
		{Date: now.Add(-3 * day).Format("2006-01-02"), TypeID: s.typeID("Pyroxeres"), SolarSystemID: s.amarrSystemID, Quantity: 9800},
	}
	if err := s.snap(ctx, id, esi.SnapMining, esi.MiningLedger(mining)); err != nil {
		return err
	}

	bargeItem := demoItemBase + 51
	assets := []esi.Asset{
		{ItemID: bargeItem, TypeID: s.typeID("Covetor"), Quantity: 1, LocationID: s.amarrStationID, LocationType: "station", LocationFlag: "Hangar", IsSingleton: true},
		{ItemID: demoItemBase + 61, TypeID: s.typeID("Strip Miner I"), Quantity: 1, LocationID: bargeItem, LocationType: "item", LocationFlag: "HiSlot0", IsSingleton: true},
		{ItemID: demoItemBase + 62, TypeID: s.typeID("Strip Miner I"), Quantity: 1, LocationID: bargeItem, LocationType: "item", LocationFlag: "HiSlot1", IsSingleton: true},
		{ItemID: demoItemBase + 63, TypeID: s.typeID("Mining Laser Upgrade II"), Quantity: 1, LocationID: bargeItem, LocationType: "item", LocationFlag: "LowSlot0", IsSingleton: true},
		{ItemID: demoItemBase + 64, TypeID: s.typeID("Mining Laser Upgrade II"), Quantity: 1, LocationID: bargeItem, LocationType: "item", LocationFlag: "LowSlot1", IsSingleton: true},
		{ItemID: demoItemBase + 65, TypeID: s.typeID("Medium Shield Extender II"), Quantity: 1, LocationID: bargeItem, LocationType: "item", LocationFlag: "MedSlot0", IsSingleton: true},
		{ItemID: demoItemBase + 52, TypeID: s.typeID("Tritanium"), Quantity: 780000, LocationID: s.amarrStationID, LocationType: "station", LocationFlag: "Hangar"},
		{ItemID: demoItemBase + 53, TypeID: s.typeID("Pyerite"), Quantity: 260000, LocationID: s.amarrStationID, LocationType: "station", LocationFlag: "Hangar"},
		{ItemID: demoItemBase + 54, TypeID: s.typeID("Veldspar"), Quantity: 45000, LocationID: s.amarrStationID, LocationType: "station", LocationFlag: "Hangar"},
		{ItemID: demoItemBase + 55, TypeID: s.typeID("Isogen"), Quantity: 12400, LocationID: s.amarrStationID, LocationType: "station", LocationFlag: "Hangar"},
		{ItemID: demoItemBase + 56, TypeID: s.typeID("Vexor"), Quantity: 4, LocationID: s.amarrStationID, LocationType: "station", LocationFlag: "Hangar"},
	}
	// One station can in Amarr holding mining spares, so the
	// default view of the Assets page carries a container too.
	canMining := demoItemBase + 81
	if t := s.typeID("Station Warehouse Container"); t != 0 {
		assets = append(assets,
			esi.Asset{ItemID: canMining, TypeID: t, Quantity: 1, LocationID: s.amarrStationID, LocationType: "station", LocationFlag: "Hangar", IsSingleton: true},
			esi.Asset{ItemID: demoItemBase + 82, TypeID: s.typeID("Strip Miner I"), Quantity: 2, LocationID: canMining, LocationType: "item", LocationFlag: "Unlocked"},
			esi.Asset{ItemID: demoItemBase + 83, TypeID: s.typeID("Mining Laser Upgrade II"), Quantity: 3, LocationID: canMining, LocationType: "item", LocationFlag: "Unlocked"},
			esi.Asset{ItemID: demoItemBase + 84, TypeID: s.typeID("Isogen"), Quantity: 800, LocationID: canMining, LocationType: "item", LocationFlag: "Unlocked"},
		)
	}
	if err := s.snap(ctx, id, esi.SnapAssets, assets); err != nil {
		return err
	}
	if err := q.UpsertItemName(ctx, db.UpsertItemNameParams{ItemID: bargeItem, Name: "Juno's Digger"}); err != nil {
		return err
	}
	if s.typeID("Station Warehouse Container") != 0 {
		if err := q.UpsertItemName(ctx, db.UpsertItemNameParams{ItemID: canMining, Name: "Mining Spares"}); err != nil {
			return err
		}
	}
	if err := s.snap(ctx, id, esi.SnapLocation, esi.Location{
		SolarSystemID: s.amarrSystemID, StationID: s.amarrStationID,
	}); err != nil {
		return err
	}
	if err := s.snap(ctx, id, esi.SnapShip, esi.Ship{
		ShipTypeID: s.typeID("Covetor"), ShipItemID: bargeItem, ShipName: "Juno's Digger",
	}); err != nil {
		return err
	}
	if err := s.snap(ctx, id, esi.SnapOnline, esi.Online{
		Online: false, LastLogin: s.rfc(now.Add(-1 * hour)), LastLogout: s.rfc(now.Add(-20 * time.Minute)), Logins: 268,
	}); err != nil {
		return err
	}
	if err := s.snap(ctx, id, esi.SnapImplants, esi.Implants{}); err != nil {
		return err
	}
	if err := s.snap(ctx, id, esi.SnapClones, esi.Clones{
		HomeLocation: esi.CloneHome{LocationID: s.amarrStationID, LocationType: "station"},
		JumpClones:   []esi.JumpClone{},
	}); err != nil {
		return err
	}
	if err := s.snap(ctx, id, esi.SnapFatigue, esi.Fatigue{}); err != nil {
		return err
	}
	if err := s.snap(ctx, id, esi.SnapFittings, esi.Fittings{}); err != nil {
		return err
	}
	if err := s.snap(ctx, id, esi.SnapKillmails, []esi.KillmailRef{}); err != nil {
		return err
	}
	if err := s.snap(ctx, id, esi.SnapCorpRoles, esi.CharacterRoles{
		Roles: []string{"Factory_Manager", "Contract_Manager", "Junior_Accountant"},
	}); err != nil {
		return err
	}

	if err := s.writeColony(ctx, id, demoColony{
		PlanetID: 40026205, PlanetName: "Amarr V", Type: "gas", SystemID: s.amarrSystemID,
		Upgrade: 4, CCName: "Gas Command Center",
		Extractor: "Noble Gas", Product: "Oxygen", Schematic: 118,
	}); err != nil {
		return err
	}
	if err := s.writeColony(ctx, id, demoColony{
		PlanetID: 40026208, PlanetName: "Amarr VIII", Type: "lava", SystemID: s.amarrSystemID,
		Upgrade: 3, CCName: "Lava Command Center",
		Extractor: "Suspended Plasma", Product: "Plasmoids", Schematic: 119,
	}); err != nil {
		return err
	}
	if err := s.writeColonies(ctx, id); err != nil {
		return err
	}

	mail := esi.MailHeader{MailID: 3900000011, From: demoCharacterID, Subject: "Blueprints are in the corp hangar", Timestamp: s.rfc(now.Add(-8 * hour)), IsRead: false, Labels: []int64{}, Recipients: []esi.MailRecipient{{RecipientID: id, RecipientType: "character"}}}
	if err := s.snap(ctx, id, esi.MailBodyKind(mail.MailID), esi.Mail{
		From: mail.From, Subject: mail.Subject,
		Body:      "<p>Juno,</p><p>The Thrasher prints moved to the corp hangar in Jita, division 2. The Covetor original stays in Amarr with you. Queue is yours to run this week.</p><p>Sable</p>",
		Timestamp: mail.Timestamp, Read: false, Labels: mail.Labels, Recipients: mail.Recipients,
	}); err != nil {
		return err
	}
	if err := s.snap(ctx, id, esi.SnapMail, esi.MailHeaders{mail}); err != nil {
		return err
	}
	if err := s.snap(ctx, id, esi.SnapMailLabels, esi.MailLabels{Labels: []esi.MailLabel{}, TotalUnreadCount: 1}); err != nil {
		return err
	}
	if err := s.snap(ctx, id, esi.SnapMailLists, esi.MailLists{}); err != nil {
		return err
	}
	if err := s.writeCalendar(ctx, id, []int64{demoEventMining}); err != nil {
		return err
	}
	contacts := []esi.Contact{
		{ContactID: demoCharacterID, ContactType: "character", Standing: 10, IsWatched: true},
		{ContactID: demoCharMarcus, ContactType: "character", Standing: 5},
	}
	if err := s.snap(ctx, id, esi.SnapContacts, esi.Contacts(contacts)); err != nil {
		return err
	}
	return nil
}

// writeCorpSnapshots seeds the Test Pilot Collective's datasets
// under one viewing pilot; both demo pilots carry the same copy,
// as either of them opening the corporation pages would hold.
func (s *demoSeed) writeCorpSnapshots(ctx context.Context, viewerID int64) error {
	now := s.now
	hour := time.Hour
	day := 24 * hour

	if err := s.snap(ctx, viewerID, esi.SnapCorpInfo, esi.Corporation{
		Name: demoCorpName, Ticker: "TPC", MemberCount: 6,
		CEOID: demoCharacterID, CreatorID: demoCharacterID,
		HomeStationID: s.jitaStationID, TaxRate: 0.05,
		DateFounded: "2021-05-04T17:22:00Z",
		Description: "A small collective of test pilots. We build, we haul, we occasionally feed frigates to Tama. Everything on this demo account is sample data.",
	}); err != nil {
		return err
	}
	members := []int64{demoCharacterID, demoCharacter2ID, demoCharSera, demoCharDario, demoCharMina, demoCharPetra}
	if err := s.snap(ctx, viewerID, esi.SnapCorpMembers, esi.CorpMembers(members)); err != nil {
		return err
	}
	tracking := []esi.CorpMemberTracking{
		{CharacterID: demoCharacterID, BaseID: s.jitaStationID, LocationID: s.jitaStationID, LogonDate: s.rfc(now.Add(-3 * hour)), LogoffDate: s.rfc(now.Add(-2 * hour)), ShipTypeID: s.typeID("Rifter"), StartDate: "2021-05-04T17:22:00Z"},
		{CharacterID: demoCharacter2ID, BaseID: s.amarrStationID, LocationID: s.amarrStationID, LogonDate: s.rfc(now.Add(-1 * hour)), LogoffDate: s.rfc(now.Add(-20 * time.Minute)), ShipTypeID: s.typeID("Covetor"), StartDate: "2021-05-04T17:40:00Z"},
		{CharacterID: demoCharSera, BaseID: s.jitaStationID, LocationID: s.jitaStationID, LogonDate: s.rfc(now.Add(-5 * hour)), LogoffDate: s.rfc(now.Add(-4 * hour)), ShipTypeID: s.typeID("Drake"), StartDate: "2022-01-11T09:15:00Z"},
		{CharacterID: demoCharDario, BaseID: s.jitaStationID, LocationID: s.jitaStationID, LogonDate: s.rfc(now.Add(-2 * day)), LogoffDate: s.rfc(now.Add(-2 * day)), ShipTypeID: s.typeID("Merlin"), StartDate: "2021-08-23T20:41:00Z"},
		{CharacterID: demoCharMina, BaseID: s.jitaStationID, LocationID: s.dodixieStationID, LogonDate: s.rfc(now.Add(-3 * day)), LogoffDate: s.rfc(now.Add(-3 * day)), ShipTypeID: s.typeID("Vexor"), StartDate: "2023-03-02T12:05:00Z"},
		{CharacterID: demoCharPetra, BaseID: s.jitaStationID, LocationID: s.jitaStationID, LogonDate: s.rfc(now.Add(-6 * hour)), LogoffDate: s.rfc(now.Add(-5 * hour)), ShipTypeID: s.typeID("Punisher"), StartDate: "2022-06-19T08:52:00Z"},
	}
	if err := s.snap(ctx, viewerID, esi.SnapCorpMemberTracking, esi.CorpMemberTrackings(tracking)); err != nil {
		return err
	}
	wallets := esi.CorpWallets{
		{Division: 1, Balance: 812400553.10},
		{Division: 2, Balance: 45000000},
		{Division: 3, Balance: 0},
		{Division: 4, Balance: 0},
		{Division: 5, Balance: 0},
		{Division: 6, Balance: 0},
		{Division: 7, Balance: 0},
	}
	if err := s.snap(ctx, viewerID, esi.SnapCorpWallets, wallets); err != nil {
		return err
	}
	journal := []esi.CorpJournalEntry{
		{ID: 8800003005, Date: s.rfc(now.Add(-6 * hour)), RefType: "player_donation", Amount: 120000000, FirstPartyID: demoCharMarcus, FirstPartyType: "character", SecondPartyID: demoCorpID, SecondPartyType: "corporation", Description: "Player donation", Reason: "for the SRP fund"},
		{ID: 8800003004, Date: s.rfc(now.Add(-1 * day)), RefType: "industry_job_tax", Amount: 842000, FirstPartyID: demoCharacter2ID, FirstPartyType: "character", SecondPartyID: demoCorpID, SecondPartyType: "corporation", Description: "Industry job tax"},
		{ID: 8800003003, Date: s.rfc(now.Add(-2 * day)), RefType: "market_transaction", Amount: 6300000, FirstPartyID: demoCharSera, FirstPartyType: "character", SecondPartyID: demoCorpID, SecondPartyType: "corporation", Description: "Market transaction"},
		{ID: 8800003002, Date: s.rfc(now.Add(-4 * day)), RefType: "corporation_account_withdrawal", Amount: -7250000, FirstPartyID: demoCorpID, FirstPartyType: "corporation", SecondPartyID: demoCharacterID, SecondPartyType: "character", Description: "Corporation account withdrawal"},
		{ID: 8800003001, Date: s.rfc(now.Add(-6 * day)), RefType: "office_rental_fee", Amount: -38500000, FirstPartyID: demoCorpID, FirstPartyType: "corporation", SecondPartyID: demoCorpHaleID, SecondPartyType: "corporation", Description: "Office rental fee"},
		{ID: 8800003006, Date: s.rfc(now.Add(-11 * day)), RefType: "corporation_account_withdrawal", Amount: -9800000, FirstPartyID: demoCorpID, FirstPartyType: "corporation", SecondPartyID: demoCharacter2ID, SecondPartyType: "character", Description: "Corporation account withdrawal"},
		{ID: 8800003007, Date: s.rfc(now.Add(-16 * day)), RefType: "corporation_account_withdrawal", Amount: -18500000, FirstPartyID: demoCorpID, FirstPartyType: "corporation", SecondPartyID: demoCharacterID, SecondPartyType: "character", Description: "Corporation account withdrawal"},
	}
	corpJournalBalances(journal, 812400553.10)
	if err := s.snap(ctx, viewerID, esi.CorpJournalKind(1), esi.CorpJournal(journal)); err != nil {
		return err
	}
	if err := s.snap(ctx, viewerID, esi.CorpJournalKind(2), esi.CorpJournal{}); err != nil {
		return err
	}
	corpTxns := []esi.CorpWalletTransaction{
		{TransactionID: 7700003003, Date: s.rfc(now.Add(-1 * day)), TypeID: s.typeID("Tritanium"), LocationID: s.jitaStationID, UnitPrice: 5.91, Quantity: 1000000, ClientID: demoCharacter2ID, IsBuy: true, JournalRefID: 0},
		{TransactionID: 7700003002, Date: s.rfc(now.Add(-2 * day)), TypeID: s.typeID("Damage Control II"), LocationID: s.jitaStationID, UnitPrice: 758000, Quantity: 8, ClientID: demoCharSera, IsBuy: false, JournalRefID: 0},
		{TransactionID: 7700003001, Date: s.rfc(now.Add(-3 * day)), TypeID: s.typeID("PLEX"), LocationID: s.jitaStationID, UnitPrice: 5880000, Quantity: 2, ClientID: demoCharMarcus, IsBuy: true, JournalRefID: 0},
	}
	if err := s.snap(ctx, viewerID, esi.CorpTxnsKind(1), esi.CorpWalletTransactions(corpTxns)); err != nil {
		return err
	}
	corpOrders := []esi.CorpOrder{
		{OrderID: 7900000201, TypeID: s.typeID("Tritanium"), LocationID: s.jitaStationID, RegionID: s.forgeRegionID, Price: 6.05, VolumeTotal: 900000, VolumeRemain: 900000, MinVolume: 1, Range: "station", Issued: s.rfc(now.Add(-2 * day)), Duration: 90, WalletDiv: 2, IssuedBy: demoCharacter2ID},
		{OrderID: 7900000202, TypeID: s.typeID("Rifter"), LocationID: s.jitaStationID, RegionID: s.forgeRegionID, Price: 431000, VolumeTotal: 20, VolumeRemain: 13, MinVolume: 1, Range: "station", Issued: s.rfc(now.Add(-3 * day)), Duration: 90, WalletDiv: 1, IssuedBy: demoCharacterID},
		{OrderID: 7900000203, TypeID: s.typeID("Thrasher"), LocationID: s.jitaStationID, RegionID: s.forgeRegionID, Price: 1468000, VolumeTotal: 10, VolumeRemain: 8, MinVolume: 1, Range: "station", Issued: s.rfc(now.Add(-5 * day)), Duration: 90, WalletDiv: 1, IssuedBy: demoCharacterID},
		{OrderID: 7900000204, TypeID: s.typeID("Scourge Heavy Missile"), LocationID: s.jitaStationID, RegionID: s.forgeRegionID, Price: 474, VolumeTotal: 50000, VolumeRemain: 31600, MinVolume: 1, Range: "station", Issued: s.rfc(now.Add(-6 * day)), Duration: 90, WalletDiv: 1, IssuedBy: demoCharSera},
		{OrderID: 7900000205, TypeID: s.typeID("Pyerite"), LocationID: s.jitaStationID, RegionID: s.forgeRegionID, IsBuyOrder: true, Price: 11.52, VolumeTotal: 2000000, VolumeRemain: 1240000, MinVolume: 1, Range: "region", Issued: s.rfc(now.Add(-1 * day)), Duration: 30, Escrow: 14284800, WalletDiv: 2, IssuedBy: demoCharacter2ID},
		{OrderID: 7900000206, TypeID: s.typeID("Isogen"), LocationID: s.jitaStationID, RegionID: s.forgeRegionID, IsBuyOrder: true, Price: 509, VolumeTotal: 80000, VolumeRemain: 80000, MinVolume: 1, Range: "station", Issued: s.rfc(now.Add(-10 * hour)), Duration: 30, Escrow: 40720000, WalletDiv: 2, IssuedBy: demoCharacter2ID},
	}
	if err := s.snap(ctx, viewerID, esi.SnapCorpOrders, esi.CorpOrders(corpOrders)); err != nil {
		return err
	}
	// Corporation contracts: a courier the corporation issued, an
	// item exchange assigned to it, and one that expired unanswered.
	corpContracts := []esi.Contract{
		{ContractID: 2900006001, IssuerID: demoCharacter2ID, IssuerCorporationID: demoCorpID, AssigneeID: demoCorpID, Type: "courier", Status: "outstanding", Availability: "corporation", ForCorporation: true, Title: "Staging resupply: Jita to Amarr", Reward: 24000000, Collateral: 610000000, Volume: 58000, StartLocationID: s.jitaStationID, EndLocationID: s.amarrStationID, DateIssued: s.rfc(now.Add(-6 * hour)), DateExpired: s.rfc(now.Add(6 * day)), DaysToComplete: 5},
		{ContractID: 2900006002, IssuerID: demoCharSera, IssuerCorporationID: demoCorpID, AssigneeID: demoCorpID, AcceptorID: demoCorpID, Type: "item_exchange", Status: "finished", Availability: "corporation", Title: "Doctrine hulls for the hangar", Price: 0, StartLocationID: s.jitaStationID, EndLocationID: s.jitaStationID, DateIssued: s.rfc(now.Add(-3 * day)), DateExpired: s.rfc(now.Add(11 * day)), DateAccepted: s.rfc(now.Add(-2 * day)), DateCompleted: s.rfc(now.Add(-2 * day))},
		{ContractID: 2900006003, IssuerID: viewerID, IssuerCorporationID: demoCorpID, Type: "item_exchange", Status: "outstanding", Availability: "public", ForCorporation: true, Title: "Surplus Damage Control II", Price: 15400000, StartLocationID: s.jitaStationID, EndLocationID: s.jitaStationID, DateIssued: s.rfc(now.Add(-16 * day)), DateExpired: s.rfc(now.Add(-2 * day))},
	}
	if err := s.snap(ctx, viewerID, esi.SnapCorpContracts, esi.Contracts(corpContracts)); err != nil {
		return err
	}
	for contractID, items := range map[int64][]esi.ContractItem{
		2900006001: {{RecordID: 1, TypeID: s.typeID("Tritanium"), Quantity: 5000000, RawQuantity: 5000000, IsIncluded: true}},
		2900006002: {
			{RecordID: 1, TypeID: s.typeID("Vexor"), Quantity: 3, RawQuantity: 3, IsIncluded: true},
			{RecordID: 2, TypeID: s.typeID("Rifter"), Quantity: 6, RawQuantity: 6, IsIncluded: true},
			{RecordID: 3, TypeID: s.typeID("Damage Control II"), Quantity: 9, RawQuantity: 9, IsIncluded: true},
			{RecordID: 4, TypeID: s.typeID("200mm AutoCannon II"), Quantity: 12, RawQuantity: 12, IsIncluded: true},
		},
		2900006003: {{RecordID: 1, TypeID: s.typeID("Damage Control II"), Quantity: 20, RawQuantity: 20, IsIncluded: true}},
	} {
		raw, err := json.Marshal(esi.ContractItems(items))
		if err != nil {
			return err
		}
		if err := s.app.queries.UpsertContractDetail(ctx, db.UpsertContractDetailParams{
			ContractID: contractID, CharacterID: viewerID, Payload: string(raw), FetchedAt: now,
		}); err != nil {
			return err
		}
	}
	corpAssets := []esi.Asset{
		{ItemID: demoItemBase + 71, TypeID: s.typeID("Tritanium"), Quantity: 900000, LocationID: s.jitaStationID, LocationType: "station", LocationFlag: "CorpHangar1"},
		{ItemID: demoItemBase + 72, TypeID: s.typeID("Damage Control II"), Quantity: 20, LocationID: s.jitaStationID, LocationType: "station", LocationFlag: "CorpHangar1"},
		{ItemID: demoItemBase + 73, TypeID: s.typeID("Vexor"), Quantity: 1, LocationID: s.jitaStationID, LocationType: "station", LocationFlag: "CorpHangar2"},
		// The doctrine hangar: packaged hulls and the fittings,
		// ammo and drones that go in them.
		{ItemID: demoItemBase + 74, TypeID: s.typeID("Rifter"), Quantity: 9, LocationID: s.jitaStationID, LocationType: "station", LocationFlag: "CorpHangar2"},
		{ItemID: demoItemBase + 75, TypeID: s.typeID("Thrasher"), Quantity: 5, LocationID: s.jitaStationID, LocationType: "station", LocationFlag: "CorpHangar2"},
		{ItemID: demoItemBase + 76, TypeID: s.typeID("Drake"), Quantity: 2, LocationID: s.jitaStationID, LocationType: "station", LocationFlag: "CorpHangar2"},
		{ItemID: demoItemBase + 77, TypeID: s.typeID("200mm AutoCannon II"), Quantity: 36, LocationID: s.jitaStationID, LocationType: "station", LocationFlag: "CorpHangar3"},
		{ItemID: demoItemBase + 78, TypeID: s.typeID("Warp Scrambler II"), Quantity: 14, LocationID: s.jitaStationID, LocationType: "station", LocationFlag: "CorpHangar3"},
		{ItemID: demoItemBase + 79, TypeID: s.typeID("1MN Afterburner II"), Quantity: 12, LocationID: s.jitaStationID, LocationType: "station", LocationFlag: "CorpHangar3"},
		{ItemID: demoItemBase + 80, TypeID: s.typeID("Republic Fleet EMP S"), Quantity: 24000, LocationID: s.jitaStationID, LocationType: "station", LocationFlag: "CorpHangar3"},
		{ItemID: demoItemBase + 81, TypeID: s.typeID("Scourge Heavy Missile"), Quantity: 18400, LocationID: s.jitaStationID, LocationType: "station", LocationFlag: "CorpHangar3"},
		{ItemID: demoItemBase + 82, TypeID: s.typeID("Hobgoblin II"), Quantity: 60, LocationID: s.jitaStationID, LocationType: "station", LocationFlag: "CorpHangar3"},
		{ItemID: demoItemBase + 83, TypeID: s.typeID("Pyerite"), Quantity: 640000, LocationID: s.jitaStationID, LocationType: "station", LocationFlag: "CorpHangar1"},
		{ItemID: demoItemBase + 84, TypeID: s.typeID("Mexallon"), Quantity: 182000, LocationID: s.jitaStationID, LocationType: "station", LocationFlag: "CorpHangar1"},
		{ItemID: demoItemBase + 85, TypeID: s.typeID("Veldspar"), Quantity: 1250000, LocationID: s.amarrStationID, LocationType: "station", LocationFlag: "CorpHangar1"},
	}
	knownAssets := corpAssets[:0]
	for _, a := range corpAssets {
		if a.TypeID != 0 && a.LocationID != 0 {
			knownAssets = append(knownAssets, a)
		}
	}
	corpAssets = knownAssets
	if err := s.snap(ctx, viewerID, esi.SnapCorpAssets, corpAssets); err != nil {
		return err
	}
	// Two structures so the Structures page shows its states: a
	// healthy Astrahus, and a Raitaru low on fuel with a service off.
	reinforceHour := int64(19)
	corpStructures := esi.CorpStructures{
		{StructureID: 1035000000101, TypeID: s.typeID("Astrahus"), SystemID: s.tamaSystemID, CorporationID: demoCorpID,
			Name: "Tama - TPC Staging", State: "shield_vulnerable", FuelExpires: s.rfc(now.Add(23 * day)), ReinforceHour: &reinforceHour,
			Services: []esi.CorpStructureService{{Name: "Clone Bay", State: "online"}, {Name: "Market Hub", State: "online"}}},
		{StructureID: 1035000000102, TypeID: s.typeID("Raitaru"), SystemID: s.jitaSystemID, CorporationID: demoCorpID,
			Name: "Jita - TPC Workshop", State: "shield_vulnerable", FuelExpires: s.rfc(now.Add(4*day + 6*hour)), ReinforceHour: &reinforceHour,
			Services: []esi.CorpStructureService{{Name: "Manufacturing (Standard)", State: "online"}, {Name: "Invention", State: "offline"}}},
	}
	if err := s.snap(ctx, viewerID, esi.SnapCorpStructures, corpStructures); err != nil {
		return err
	}
	if err := s.snap(ctx, viewerID, esi.SnapCorpKillmails, s.corpKillmails); err != nil {
		return err
	}
	return nil
}

// writeAccountRows seeds what belongs to the account rather
// than a pilot: saved fits, Juno's skill plan, a few
// notifications and the watch list. The wallet history is
// written later, once the fold has attributed the year (see
// writeDemoWalletHistory).
func (s *demoSeed) writeAccountRows(ctx context.Context) error {
	now := s.now
	hour := time.Hour
	day := 24 * hour
	q := s.app.queries

	fitDoc := func(name string, shipName string, items []fitview.FitDocItem, charges map[int64]int64) (string, int64, error) {
		ship := s.typeID(shipName)
		doc := fitview.FitDoc{Name: name, ShipTypeID: ship, Items: items, Charges: charges}
		raw, err := json.Marshal(doc)
		return string(raw), ship, err
	}
	rifterDoc, rifterShip, err := fitDoc("Tama Rifter", "Rifter", []fitview.FitDocItem{
		{TypeID: s.typeID("200mm AutoCannon II"), Qty: 2},
		{TypeID: s.typeID("125mm Gatling AutoCannon II"), Qty: 1},
		{TypeID: s.typeID("1MN Afterburner II"), Qty: 1},
		{TypeID: s.typeID("Medium Shield Extender II"), Qty: 1},
		{TypeID: s.typeID("Warp Scrambler II"), Qty: 1},
		{TypeID: s.typeID("Damage Control II"), Qty: 1},
		{TypeID: s.typeID("Gyrostabilizer II"), Qty: 2},
		{TypeID: s.typeID("Hobgoblin II"), Qty: 4},
	}, map[int64]int64{})
	if err != nil {
		return err
	}
	drakeDoc, drakeShip, err := fitDoc("Mission Drake", "Drake", []fitview.FitDocItem{
		{TypeID: s.typeID("Heavy Missile Launcher II"), Qty: 6},
		{TypeID: s.typeID("Medium Shield Extender II"), Qty: 1},
		{TypeID: s.typeID("Damage Control II"), Qty: 1},
		{TypeID: s.typeID("Ballistic Control System II"), Qty: 2},
	}, map[int64]int64{})
	if err != nil {
		return err
	}
	if rifterShip != 0 {
		if _, err := q.CreateLocalFitting(ctx, db.CreateLocalFittingParams{
			UserID: s.userID, Name: "Tama Rifter", ShipTypeID: rifterShip,
			ItemsJson: rifterDoc, CreatedAt: now.Add(-12 * day), UpdatedAt: now.Add(-2 * day),
		}); err != nil {
			return err
		}
	}
	if drakeShip != 0 {
		if _, err := q.CreateLocalFitting(ctx, db.CreateLocalFittingParams{
			UserID: s.userID, Name: "Mission Drake", ShipTypeID: drakeShip,
			ItemsJson: drakeDoc, CreatedAt: now.Add(-20 * day), UpdatedAt: now.Add(-6 * day),
		}); err != nil {
			return err
		}
	}

	plan, err := q.CreateSkillPlan(ctx, db.CreateSkillPlanParams{
		UserID: s.userID, CharacterID: demoCharacter2ID,
		Name: "Orca track", CreatedAt: now.Add(-14 * day),
	})
	if err != nil {
		return err
	}
	position := int64(0)
	var skillIDs, levels, positions []int64
	for _, target := range []struct {
		name  string
		level int64
	}{
		{"Mining Foreman", 5},
		{"Mining Director", 5},
		{"Leadership", 5},
		{"Astrogeology", 5},
		{"Capital Industrial Ships", 1},
	} {
		typeID := s.typeID(target.name)
		if typeID == 0 {
			continue
		}
		position++
		skillIDs = append(skillIDs, typeID)
		levels = append(levels, target.level)
		positions = append(positions, position)
	}
	if len(skillIDs) > 0 {
		if err := q.UpsertSkillPlanItems(ctx, db.UpsertSkillPlanItemsParams{
			PlanID: plan.ID, SkillTypeIds: skillIDs, TargetLevels: levels, Positions: positions,
		}); err != nil {
			return err
		}
	}

	notes := []struct {
		kind  string
		title string
		url   string
		when  time.Time
	}{
		{notifyWatch, "Watch list: PLEX moved 6.2% in The Forge", "/market/", now.Add(-5 * hour)},
		{notifyKillmail, "New killmail: Ilya Sorin's Punisher", "/killmails/", now.Add(-26 * hour)},
		{notifyIndustry, "Industry job complete: Damage Control II (20 runs)", "/industry/", now.Add(-8 * day)},
	}
	for _, n := range notes {
		if _, err := q.InsertNotification(ctx, db.InsertNotificationParams{
			UserID: s.userID, CharacterID: demoCharacterID,
			Kind: n.kind, Title: n.title, Url: n.url, CreatedAt: n.when,
		}); err != nil {
			return err
		}
	}
	// Juno's finished training, in the skill notifier's own
	// title format: the character sheet's trained banner reads
	// these, and her queue (Advanced Mass Production IV is
	// training now) is what she moved on to after them.
	for _, n := range []struct {
		title string
		when  time.Time
	}{
		{fmt.Sprintf("%s finished training %s %s", demoChar2Name, "Astrogeology", "IV"), now.Add(-26 * hour)},
		{fmt.Sprintf("%s finished training %s %s", demoChar2Name, "Reprocessing Efficiency", "IV"), now.Add(-2 * hour)},
	} {
		if _, err := q.InsertNotification(ctx, db.InsertNotificationParams{
			UserID: s.userID, CharacterID: demoCharacter2ID,
			Kind: notifySkill, Title: n.title,
			Url:       fmt.Sprintf("/character/?character=%d&view=1", demoCharacter2ID),
			CreatedAt: n.when,
		}); err != nil {
			return err
		}
	}

	for _, watch := range []struct {
		name      string
		threshold float64
	}{
		{"Tritanium", 5}, {"PLEX", 7.5},
	} {
		if typeID := s.typeID(watch.name); typeID != 0 && s.forgeRegionID != 0 {
			if err := q.UpsertWatchlistEntry(ctx, db.UpsertWatchlistEntryParams{
				UserID: s.userID, TypeID: typeID, RegionID: s.forgeRegionID,
				ThresholdPct: watch.threshold, CreatedAt: now.Add(-9 * day),
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

// writeOrgRows writes what the corporation and the account keep in
// EveSynapse itself rather than in ESI: two doctrines and their fits,
// ops (two flown, two coming), SRP claims for both of Sable's losses
// (one paid, one waiting), two groups, and the account's restock
// targets, a basket of roam consumables and a public fit.
func (s *demoSeed) writeOrgRows(ctx context.Context) error {
	q := s.app.queries
	now := s.now
	hour := time.Hour
	day := 24 * hour
	t := s.typeID

	fit := func(name, ship string, items []fitview.FitDocItem) (string, int64, bool) {
		shipID := t(ship)
		var kept []fitview.FitDocItem
		for _, it := range items {
			if it.TypeID != 0 {
				kept = append(kept, it)
			}
		}
		raw, err := json.Marshal(fitview.FitDoc{Name: name, ShipTypeID: shipID, Items: kept, Charges: map[int64]int64{}})
		return string(raw), shipID, err == nil && shipID != 0
	}
	type docFit struct {
		name, ship, role, note string
		items                  []fitview.FitDocItem
	}
	tamaRifter := docFit{"Tama Rifter", "Rifter", "Tackle", "Scram first, then web if you fit one. Burn to the gate ahead of the gang.", []fitview.FitDocItem{
		{TypeID: t("200mm AutoCannon II"), Qty: 2}, {TypeID: t("125mm Gatling AutoCannon II"), Qty: 1},
		{TypeID: t("1MN Afterburner II"), Qty: 1}, {TypeID: t("Medium Shield Extender II"), Qty: 1}, {TypeID: t("Warp Scrambler II"), Qty: 1},
		{TypeID: t("Damage Control II"), Qty: 1}, {TypeID: t("Gyrostabilizer II"), Qty: 2},
	}}
	brawlThrasher := docFit{"Brawl Thrasher", "Thrasher", "DPS", "Stay inside scram range and keep your transversal low. Load EMP for shields, Fusion for armor.", []fitview.FitDocItem{
		{TypeID: t("200mm AutoCannon II"), Qty: 7},
		{TypeID: t("1MN Afterburner II"), Qty: 1}, {TypeID: t("Warp Scrambler II"), Qty: 1}, {TypeID: t("Stasis Webifier II"), Qty: 1},
		{TypeID: t("Damage Control II"), Qty: 1}, {TypeID: t("Gyrostabilizer II"), Qty: 2},
	}}
	fleetDrake := docFit{"Fleet Drake", "Drake", "DPS", "Shield brick. Primary what the FC calls, reload between targets.", []fitview.FitDocItem{
		{TypeID: t("Heavy Missile Launcher II"), Qty: 6},
		{TypeID: t("Medium Shield Extender II"), Qty: 2},
		{TypeID: t("Damage Control II"), Qty: 1}, {TypeID: t("Ballistic Control System II"), Qty: 2},
	}}
	webVexor := docFit{"Web Vexor", "Vexor", "Tackle", "Drones on the tackled target, web whatever burns for the gate.", []fitview.FitDocItem{
		{TypeID: t("Stasis Webifier II"), Qty: 1}, {TypeID: t("Warp Scrambler II"), Qty: 1},
		{TypeID: t("Damage Control II"), Qty: 1}, {TypeID: t("Energized Adaptive Nano Membrane II"), Qty: 1},
		{TypeID: t("Hobgoblin II"), Qty: 5},
	}}

	doctrines := []struct {
		name, category, tags, description string
		created                           time.Duration
		fits                              []docFit
	}{
		{"Tama Frigates", "Small gang", "lowsec,frigates,roam",
			"Cheap, fast and replaceable. Rifters hold tackle, Thrashers bring the damage. SRP covers both hulls and their fits.",
			60 * day, []docFit{tamaRifter, brawlThrasher}},
		{"Home Defense", "Medium gang", "highsec,defense",
			"What we undock when someone camps the Jita workshop. Drakes tank and shoot, Vexors bring drones and webs.",
			120 * day, []docFit{fleetDrake, webVexor}},
	}
	doctrineIDs := map[string]int64{}
	for _, d := range doctrines {
		docID, err := q.CreateDoctrine(ctx, db.CreateDoctrineParams{
			CorporationID: demoCorpID, Name: d.name, Category: d.category, Description: d.description,
			Tags: d.tags, CreatedBy: demoCharacterID, CreatedAt: now.Add(-d.created),
		})
		if err != nil {
			return fmt.Errorf("demo seed: doctrine %q: %w", d.name, err)
		}
		doctrineIDs[d.name] = docID
		for _, f := range d.fits {
			raw, ship, ok := fit(f.name, f.ship, f.items)
			if !ok {
				continue
			}
			if _, err := q.AddDoctrineFit(ctx, db.AddDoctrineFitParams{
				DoctrineID: docID, Name: f.name, ShipTypeID: ship, ItemsJson: raw,
				FleetRole: f.role, Note: f.note, AddedBy: demoCharacterID, AddedAt: now.Add(-d.created + day),
			}); err != nil {
				return fmt.Errorf("demo seed: doctrine fit %q: %w", f.name, err)
			}
		}
	}

	ops := []struct {
		title, description, doctrine, formUp string
		at                                   time.Duration // from now
		minutes                              int64
		fc                                   int64
	}{
		{"Kedama roam", "Frigate roam through Kedama and Sujarento. Bring a spare Rifter.", "Tama Frigates", "Jita 4-4 undock", -9*day - 2*hour, 90, demoCharSera},
		{"Tama roam", "The usual Tama loop. Form up on time, we undock at :15.", "Tama Frigates", "Jita 4-4 undock", -2*day - 1*hour, 90, demoCharacterID},
		{"Saturday Tama roam", "Saturday roam. SRP covers T1 frigates and destroyers in the doctrine.", "Tama Frigates", "Jita 4-4 undock", 5 * day, 90, demoCharacterID},
		{"Home defense drill", "Practice undocking the Home Defense fleet together. Twenty minutes, then we go shoot red crosses.", "Home Defense", "Jita 4-4", 9*day + 3*hour, 45, demoCharSera},
	}
	opIDs := map[string]int64{}
	for _, o := range ops {
		opID, err := q.CreateOp(ctx, db.CreateOpParams{
			CorporationID: demoCorpID, Title: o.title, Description: o.description,
			StartsAt: now.Add(o.at), DurationMinutes: o.minutes,
			Doctrine: o.doctrine, DoctrineID: doctrineIDs[o.doctrine], FormUp: o.formUp,
			FcCharacterID: o.fc, CreatedByCharacter: o.fc, CreatedAt: now.Add(o.at - 4*day),
		})
		if err != nil {
			return fmt.Errorf("demo seed: op %q: %w", o.title, err)
		}
		opIDs[o.title] = opID
	}

	if err := q.SetSRPSettings(ctx, db.SetSRPSettingsParams{
		CorporationID: demoCorpID,
		Policy:        "T1 frigates and destroyers flown in the Tama Frigates doctrine on a corp op are covered at hull plus fit. Claim within seven days with the killmail; a director pays out on Sundays.",
		UpdatedAt:     now.Add(-40 * day),
	}); err != nil {
		return err
	}
	claims := []struct {
		killmail int64
		hash     string
		ship     string
		system   int64
		lostAgo  time.Duration
		op       string
		value    float64
		note     string
		paid     float64
	}{
		{2900007005, demoKillmailHash(2900007005), "Rifter", s.sujaSystemID, 9 * day, "Kedama roam", 11850000,
			"Held the Vexor on the gate long enough for the gang to land.", 11850000},
		{2900007001, demoKillmailHashLoss, "Thrasher", s.tamaSystemID, 2 * day, "Tama roam", 18420000,
			"Caught on the Tama gate by Ilya's Merlin, Kato landed right after.", 0},
	}
	for _, c := range claims {
		if t(c.ship) == 0 || opIDs[c.op] == 0 {
			continue
		}
		reqID, err := q.CreateSRPRequest(ctx, db.CreateSRPRequestParams{
			CorporationID: demoCorpID, OpID: opIDs[c.op], KillmailID: c.killmail, KillmailHash: c.hash,
			CharacterID: demoCharacterID, UserID: s.userID, ShipTypeID: t(c.ship), SolarSystemID: c.system,
			LostAt: now.Add(-c.lostAgo), LossValue: c.value, Note: c.note, CreatedAt: now.Add(-c.lostAgo + 3*hour),
		})
		if err != nil {
			return fmt.Errorf("demo seed: SRP claim for %d: %w", c.killmail, err)
		}
		if c.paid > 0 {
			if _, err := q.DecideSRPRequest(ctx, db.DecideSRPRequestParams{
				ID: reqID, Status: "paid", Payout: c.paid, HandledBy: demoCharSera,
				HandlerNote: "Paid hull plus fit. Good tackle.",
				HandledAt:   sql.NullTime{Time: now.Add(-c.lostAgo + 2*day), Valid: true},
			}); err != nil {
				return err
			}
		}
	}

	groups := []struct {
		name, description string
		members           []int64
	}{
		{"Roam regulars", "Pinged for every Tama roam and the Saturday fleet.", []int64{demoCharacterID, demoCharSera, demoCharDario, demoCharMina}},
		{"Industry SIG", "Builders and miners: shared job planning, the ore buyback and the workshop's fuel.", []int64{demoCharacter2ID, demoCharPetra, demoCharMina}},
	}
	for i, g := range groups {
		groupID, err := q.CreateOrgGroup(ctx, db.CreateOrgGroupParams{
			OwnerKind: "corporation", OwnerID: demoCorpID, Name: g.name, Description: g.description,
			CreatedBy: demoCharacterID, CreatedAt: now.Add(-time.Duration(90-30*i) * day),
		})
		if err != nil {
			return fmt.Errorf("demo seed: group %q: %w", g.name, err)
		}
		for j, member := range g.members {
			if err := q.AddOrgGroupMember(ctx, db.AddOrgGroupMemberParams{
				GroupID: groupID, CharacterID: member, AddedBy: demoCharacterID,
				AddedAt: now.Add(-time.Duration(80-30*i-j) * day),
			}); err != nil {
				return err
			}
		}
	}

	// The account's own trading kit: restock targets for what Sable
	// builds and sells, a basket of roam consumables, and the brawl
	// Thrasher shared in the public fit browser.
	for _, r := range []struct {
		name   string
		target int64
		margin float64
	}{
		{"Damage Control II", 40, 15}, {"Gyrostabilizer II", 24, 12}, {"Hobgoblin II", 150, 10},
		{"Scourge Heavy Missile", 50000, 8}, {"Rifter", 20, 6},
	} {
		if typeID := t(r.name); typeID != 0 {
			if err := q.UpsertRestockTarget(ctx, db.UpsertRestockTargetParams{
				UserID: s.userID, TypeID: typeID, TargetQty: r.target, MinMarginPct: r.margin,
			}); err != nil {
				return err
			}
		}
	}
	if s.forgeRegionID != 0 {
		basket, err := q.CreateMarketBasket(ctx, db.CreateMarketBasketParams{
			UserID: sql.NullInt64{Int64: s.userID, Valid: true}, Name: "Roam consumables", RegionID: s.forgeRegionID,
		})
		if err != nil {
			return err
		}
		for _, it := range []struct {
			name string
			qty  int64
		}{
			{"Republic Fleet EMP S", 5000}, {"Fusion S", 5000}, {"Scourge Heavy Missile", 4000},
			{"Hobgoblin II", 10}, {"Damage Control II", 2}, {"Warp Scrambler II", 2},
		} {
			if typeID := t(it.name); typeID != 0 {
				if err := q.InsertMarketBasketItem(ctx, db.InsertMarketBasketItemParams{BasketID: basket, TypeID: typeID, Quantity: it.qty}); err != nil {
					return err
				}
			}
		}
		// Its history comes from the region's stored daily rows,
		// the same pass the worker runs for every basket.
		if err := s.app.rebuildBasketValues(ctx, basket); err != nil {
			logging.Warnf("demo seed: basket history: %v", err)
		}
	}
	if raw, ship, ok := fit(brawlThrasher.name, brawlThrasher.ship, brawlThrasher.items); ok {
		if _, err := q.CreateLocalFitting(ctx, db.CreateLocalFittingParams{
			UserID: s.userID, Name: brawlThrasher.name, ShipTypeID: ship, ItemsJson: raw,
			IsPublic: true, CreatedAt: now.Add(-30 * day), UpdatedAt: now.Add(-4 * day),
		}); err != nil {
			return err
		}
	}
	return nil
}

// writeDemoWalletHistory writes one pilot's year of daily
// wallet history. Today's row is the worker sampler's own (the
// wallet, the assets at the demo's prices, the escrow); every
// earlier day is walked back from it: the day's worth is the
// next day's, less what the fold attributed to that day, less a
// small drift for price moves on what the pilot holds. The
// remainder the Profit page then derives is that drift and
// nothing else, which is what makes it small and plausible.
// Balances walk back through the pilot's own journal, so the
// history and the journal tell the same story.
func (s *demoSeed) writeDemoWalletHistory(ctx context.Context, ch db.Character) error {
	q := s.app.queries
	s.app.sampleWalletHistory(ctx, ch, s.now)
	// The Assets directory's stored values, folded from the
	// seeded snapshots at the seeded prices, like the worker
	// would on its next cycle.
	s.app.refreshAssetLocationValues(ctx, ch)
	s.app.refreshBlueprintBuildable(ctx, ch)
	today := s.now.UTC().Truncate(24 * time.Hour)
	sample, err := q.GetWalletHistorySample(ctx, db.GetWalletHistorySampleParams{
		UserID: s.userID, CharacterID: ch.CharacterID, Day: today.Format("2006-01-02"),
	})
	if err != nil {
		return fmt.Errorf("demo seed: read wallet sample for pilot %d: %w", ch.CharacterID, err)
	}
	if !sample.NetWorth.Valid {
		// No house price can value the pilot's assets yet (an
		// install whose market sweep has not run). The history
		// waits for prices rather than inventing them; the next
		// daily reset after a sweep writes it.
		logging.Warnf("demo seed: no prices yet to value pilot %d's assets; wallet history waits for the market sweep", ch.CharacterID)
		return nil
	}
	firstDay := today.AddDate(0, 0, -364)
	daily, err := q.ListProfitDaily(ctx, db.ListProfitDailyParams{
		OwnerKind: ownerChar, OwnerIds: []int64{ch.CharacterID}, FromDay: firstDay.Format("2006-01-02"),
	})
	if err != nil {
		return fmt.Errorf("demo seed: read attribution for pilot %d: %w", ch.CharacterID, err)
	}
	attr := map[string]float64{}
	for _, r := range daily {
		attr[r.Day] = r.Attributed
	}
	var journal esi.WalletJournal
	if snap, err := q.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: ch.CharacterID, Kind: esi.SnapWalletJournal}); err == nil {
		_ = json.Unmarshal([]byte(snap.Payload), &journal)
	}
	var wallet float64
	if snap, err := q.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: ch.CharacterID, Kind: esi.SnapWallet}); err == nil {
		_ = json.Unmarshal([]byte(snap.Payload), &wallet)
	}
	balanceOn := func(day time.Time) float64 {
		bal := wallet
		end := day.Add(24 * time.Hour)
		for _, e := range journal {
			if at, perr := time.Parse(time.RFC3339, e.Date); perr == nil && !at.Before(end) {
				bal -= e.Amount
			}
		}
		return bal
	}
	worth := sample.NetWorth.Float64
	var charIDs []int64
	var days []string
	var balances, worths []float64
	var sampled []time.Time
	for d := 1; d <= 364; d++ {
		day := today.AddDate(0, 0, -d)
		next := day.AddDate(0, 0, 1).Format("2006-01-02")
		// Prices move what the pilot holds, not the ISK sitting
		// in the wallet, so the day's drift is on the next day's
		// holdings: its worth less its balance.
		holdings := worth - balanceOn(day.AddDate(0, 0, 1))
		if holdings < 0 {
			holdings = 0
		}
		worth -= attr[next] + demoDrift(ch.CharacterID, next, holdings)
		charIDs = append(charIDs, ch.CharacterID)
		days = append(days, day.Format("2006-01-02"))
		balances = append(balances, balanceOn(day))
		worths = append(worths, worth)
		sampled = append(sampled, day.Add(23*time.Hour))
	}
	if err := q.UpsertWalletHistorySamples(ctx, db.UpsertWalletHistorySamplesParams{
		UserID: s.userID, CharacterIds: charIDs, Days: days,
		Balances: balances, NetWorths: worths, SampledAts: sampled,
	}); err != nil {
		return fmt.Errorf("demo seed: write wallet history for pilot %d: %w", ch.CharacterID, err)
	}
	return nil
}

// demoDrift is one day's price move on what a pilot holds: a
// small fraction of the holdings' value, jittered by day,
// mostly up and occasionally down. It is the only part of a
// day's net worth change the journals never record.
func demoDrift(charID int64, day string, holdings float64) float64 {
	t, err := time.Parse("2006-01-02", day)
	if err != nil {
		return 0
	}
	j := demoJitter(charID*131 + t.Unix()/86400)
	rate := 0.000045 * (0.35 + 1.3*j)
	if j > 0.93 {
		rate = -0.00003 * j
	}
	return holdings * rate
}
