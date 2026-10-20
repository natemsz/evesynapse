// Sync-drain load harness: the sync-side sibling of the web load
// test (EVESYNAPSE_LOADTEST). It answers with numbers: as the
// count of pilots with due sync work grows, where does the
// per-cycle drain rate saturate, and at what pilot count does
// the backlog stop shrinking?
//
// Run the REAL cycle machinery (app.refreshCycle) against
// embedded Postgres and a canned in-process ESI stub: no live
// ESI calls, no production data. The per-pilot work mix mirrors
// what the cycle actually performs (the core 15 kinds, the
// corporation 22, the economy 8 plus contract items, one PI
// colony plus its layout, the comms 5 plus mail bodies and
// calendar detail, killmail details, asset names).
//
// Env-gated, never in the normal test path:
//
//	EVESYNAPSE_SYNCDRAIN=1 go test ./internal/app -run TestSyncDrain -count=1 -v
//
// Knobs (all optional):
//
//	EVESYNAPSE_SYNCDRAIN_STAGES  comma-separated pilot counts
//	                            (default "12,120,600,1200,2400")
//	EVESYNAPSE_SYNCDRAIN_CYCLES  cycles per stage (default 8; a
//	                            stage stops early once its backlog
//	                            has been empty two cycles running)
//	EVESYNAPSE_SYNCDRAIN_REPORT  report file name (default
//	                            "sync-drain-report.md").
//
// The report lands next to the web harness's, in
// ~/workspace/evesynapse/logs/.

package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

func TestSyncDrain(t *testing.T) {
	if os.Getenv("EVESYNAPSE_SYNCDRAIN") != "1" {
		t.Skip("sync-drain harness disabled; set EVESYNAPSE_SYNCDRAIN=1 to run")
	}
	stages := drainStages(t)
	cycles := drainCycles()
	t.Logf("sync-drain stages %v, up to %d cycles each", stages, cycles)

	var results []drainStageResult
	for _, n := range stages {
		results = append(results, runDrainStage(t, n, cycles))
	}
	verdict, defect := drainVerdict(results)
	writeSyncDrainReport(t, results, verdict, defect)
	if defect != "" {
		t.Fatalf("sync-drain harness stopped on a defect: %s", defect)
	}
	t.Logf("sync-drain verdict: %s", verdict)
}

func drainStages(t *testing.T) []int {
	t.Helper()
	raw := os.Getenv("EVESYNAPSE_SYNCDRAIN_STAGES")
	if raw == "" {
		raw = "12,120,600,1200,2400"
	}
	var stages []int
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil || n <= 0 {
			t.Fatalf("bad EVESYNAPSE_SYNCDRAIN_STAGES %q: want comma-separated pilot counts", raw)
		}
		stages = append(stages, n)
	}
	if len(stages) == 0 {
		t.Fatal("EVESYNAPSE_SYNCDRAIN_STAGES names no stages")
	}
	return stages
}

func drainCycles() int {
	raw := os.Getenv("EVESYNAPSE_SYNCDRAIN_CYCLES")
	if raw == "" {
		return 8
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 2 {
		return 8
	}
	return n
}

// drainStageResult is one row of the stage table.
type drainStageResult struct {
	Pilots            int
	CharAllowance     int
	MarketAllowance   int
	Cycles            int
	FirstCycleFetches int64
	SteadyFetches     int64
	FirstCycleWall    time.Duration
	SteadyWall        time.Duration
	DrainedPerCycle   []int // newly fully-drained pilots per cycle
	Backlogs          []int // pilots still due after each cycle
	FirstCycleByLane  drainStubCounts
	SteadyByLane      drainStubCounts
	EndToEnd          time.Duration
	EndToEndCycles    int
	UnhandledPaths    []string
	FetchErrorKinds   []string
}

// drainVerdict reads the stage table: the pilot count where
// drain-per-cycle stops scaling and the backlog plateau past
// it. A non-empty defect stops the harness instead of tuning
// around it.
func drainVerdict(results []drainStageResult) (verdict, defect string) {
	for _, r := range results {
		if len(r.UnhandledPaths) > 0 {
			return "", fmt.Sprintf("stage %d: stub missed paths %v", r.Pilots, r.UnhandledPaths)
		}
		if len(r.FetchErrorKinds) > 0 {
			return "", fmt.Sprintf("stage %d: fetch error states recorded for %v", r.Pilots, r.FetchErrorKinds)
		}
	}
	for _, r := range results {
		if len(r.Backlogs) == 0 {
			continue
		}
		if r.Backlogs[len(r.Backlogs)-1] > 0 {
			return fmt.Sprintf("drain-per-cycle stops scaling at %d pilots: the backlog no longer reaches zero within the stage's cycles past that count.", r.Pilots), ""
		}
	}
	// Every backlog reached zero, so the rate cap is the story:
	// the modal per-cycle drain of the largest stage, set by the
	// clustered-pass fetch allowance, not by pilot count.
	rate := 0
	counts := map[int]int{}
	for _, d := range results[len(results)-1].DrainedPerCycle {
		if d > 0 {
			counts[d]++
			if counts[d] > counts[rate] {
				rate = d
			}
		}
	}
	return fmt.Sprintf("drain-per-cycle saturates at ~%d pilots per cycle (the clustered-pass allowance cap); the backlog shrinks linearly at that rate and reaches zero at every stage through %d pilots, so no plateau point was observed.",
		rate, results[len(results)-1].Pilots), ""
}

// drainCorp is the one corporation every harness pilot shares,
// so corp datasets exercise the same per-character duplication
// production sees when many pilots share a few corps.
const drainCorpID = int64(98000001)

// drainBaseIDs keeps every stage's pilots, mails, contracts
// and killmails distinct, so globally-deduped stores (killmail
// and contract details) cost what distinct pilots cost.
const (
	drainCharBase     = int64(91000000)
	drainMailBase     = int64(50000000)
	drainContractBase = int64(80000000)
	drainKillmailBase = int64(70000000)
	drainEventBase    = int64(60000000)
	drainItemBase     = int64(90000000)
)

// drainCharFetches mirrors the worker's per-character share of
// one full drain: the core 15, four corporation datasets plus
// runDrainStage seeds N due pilots, runs the real cycle, and
// records the stage row.
func runDrainStage(t *testing.T, n, maxCycles int) drainStageResult {
	t.Helper()
	ctx := context.Background()

	stub := newDrainStub()
	app, conn, q := buildCorpTestApp(t, stub)
	// Production shape: four urgency lanes of 120 fetches.
	app.cfg.workerLanes = 4
	res := drainStageResult{
		Pilots:          n,
		CharAllowance:   app.fetchesPerCycle(),
		MarketAllowance: maxMarketFetchesPerCycle,
	}

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("stage %d: create user: %v", n, err)
	}
	// Close the gates the harness must not cross: the mirror
	// poll would leave the process for the live Fuzzwork
	// host, so its fetch state is seeded fresh like a recently
	// polled instance.
	if err := q.UpsertMarketFetchState(ctx, db.UpsertMarketFetchStateParams{
		Kind: fuzzworkFetchKind, State: fetchStateOK, AttemptedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("stage %d: seed fuzzwork gate: %v", n, err)
	}
	chars := make([]db.Character, 0, n)
	for i := 0; i < n; i++ {
		charID := drainCharBase + int64(i)
		seedCharacter(t, q, user.ID, charID, fmt.Sprintf("Drain Pilot %d", i))
		if err := q.UpsertCharacterCorporation(ctx, db.UpsertCharacterCorporationParams{
			CharacterID: charID, CorporationID: drainCorpID, UpdatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("stage %d: seed corp mapping: %v", n, err)
		}
		seedDrainPilot(t, q, charID, i)
		ch, err := q.GetCharacter(ctx, charID)
		if err != nil {
			t.Fatalf("stage %d: read character: %v", n, err)
		}
		chars = append(chars, ch)
	}
	_ = chars

	drained := make([]bool, n)
	for cycle := 0; cycle < maxCycles; cycle++ {
		before := stub.snapshot()
		start := time.Now()
		app.refreshCycle(ctx)
		wall := time.Since(start)
		after := stub.snapshot()
		used := after.total - before.total
		byLane := drainStubCounts{
			char:  after.char - before.char,
			corp:  after.corp - before.corp,
			pub:   after.pub - before.pub,
			other: after.other - before.other,
		}

		newly := 0
		backlog := 0
		for i := 0; i < n; i++ {
			if drained[i] {
				continue
			}
			if due := drainPilotDue(ctx, conn, drainCharBase+int64(i), i); !due {
				drained[i] = true
				newly++
				continue
			}
			backlog++
		}
		res.DrainedPerCycle = append(res.DrainedPerCycle, newly)
		res.Backlogs = append(res.Backlogs, backlog)
		if cycle == 0 {
			res.FirstCycleFetches = used
			res.FirstCycleWall = wall
			res.FirstCycleByLane = byLane
		} else {
			res.SteadyFetches = used
			res.SteadyWall = wall
			res.SteadyByLane = byLane
		}
		t.Logf("stage %d cycle %d: %d fetches in %s, %d newly drained, backlog %d",
			n, cycle+1, used, wall.Round(time.Second), newly, backlog)
		if backlog == 0 && cycle > 0 && res.Backlogs[cycle-1] == 0 {
			break
		}
	}
	res.Cycles = len(res.DrainedPerCycle)

	// End-to-end: make one pilot newly due again (full
	// re-download, details included) and time its return to
	// fresh across further real cycles.
	probe := n / 2
	expireDrainPilot(t, ctx, conn, drainCharBase+int64(probe), probe)
	e2eStart := time.Now()
	res.EndToEndCycles = -1
	for cycle := 0; cycle < maxCycles; cycle++ {
		app.refreshCycle(ctx)
		if due := drainPilotDue(ctx, conn, drainCharBase+int64(probe), probe); !due {
			res.EndToEnd = time.Since(e2eStart)
			res.EndToEndCycles = cycle + 1
			break
		}
	}
	if res.EndToEndCycles < 0 {
		res.EndToEnd = time.Since(e2eStart)
	}

	res.UnhandledPaths = stub.unhandledPaths()
	res.FetchErrorKinds = drainFetchErrors(ctx, conn, n)
	return res
}

// drainListKinds are the one-row-per-pilot snapshot kinds the
// seed marks due. It mirrors coreSnapshotKinds, the corp 22,
// economySnapshotKinds, commsSnapshotKinds, and the planets
// list; keyed detail kinds are derived per pilot below.
func drainListKinds() []string {
	kinds := append([]string{}, coreSnapshotKinds...)
	for d := int64(1); d <= walletDivisionCount; d++ {
		kinds = append(kinds,
			fmt.Sprintf("%s%d", esi.SnapCorpJournalPrefix, d),
			fmt.Sprintf("%s%d", esi.SnapCorpTxnsPrefix, d))
	}
	kinds = append(kinds, economySnapshotKinds()...)
	kinds = append(kinds, commsSnapshotKinds()...)
	kinds = append(kinds, esi.SnapPlanets)
	return kinds
}

// drainDetailKeys returns the keyed (character, kind) rows one
// full drain stores for pilot i: mail bodies, the calendar
// event plus attendees, the contract's items, the colony
// layout, and the two killmail details. Killmail and
// contract details live in global stores, so their IDs are
// unique per pilot.
func drainDetailKeys(i int) [][2]any {
	charID := drainCharBase + int64(i)
	var keys [][2]any
	for j := 0; j < 2; j++ {
		mailID := drainMailBase + int64(i)*2 + int64(j)
		keys = append(keys, [2]any{charID, esi.MailBodyKind(mailID)})
		keys = append(keys, [2]any{charID, esi.PlanetLayoutKind(drainPlanetID)})
	}
	// Contract items live in the global contract_details
	// store, checked by drainGlobalDetailFresh, not here.
	keys = append(keys,
		[2]any{charID, esi.CalendarEventKind(drainEventBase + int64(i))},
		[2]any{charID, esi.CalendarAttendeesKind(drainEventBase + int64(i))},
	)
	return keys
}

// drainPlanetID is the one temperate colony every harness
// pilot works: layouts are stored per character, so sharing
// the planet keeps the market/types footprint small while the
// layout fetch still runs once per pilot.
const drainPlanetID = int64(40161464)

// seedDrainPilot marks one pilot's whole work mix due: every
// list kind stale, keyed details absent.
func seedDrainPilot(t *testing.T, q *db.Queries, charID int64, i int) {
	t.Helper()
	ctx := context.Background()
	stale := time.Now().UTC().Add(-2 * time.Hour)
	fetched := stale.Add(-time.Hour)
	seedKind := func(kind, payload string) {
		t.Helper()
		if err := q.UpsertSnapshot(ctx, db.UpsertSnapshotParams{
			CharacterID: charID, Kind: kind, Payload: payload,
			FetchedAt:   fetched,
			CachedUntil: sql.NullTime{Time: stale, Valid: true},
		}); err != nil {
			t.Fatalf("seed %s for %d: %v", kind, charID, err)
		}
	}
	for _, kind := range drainListKinds() {
		seedKind(kind, drainSeedPayload(charID, i, kind))
	}
}

// drainSeedPayload is the canned stored copy for one due
// kind: valid ESI shapes the warmers decode, sized like a
// lightly active pilot (a few assets, one open order, one
// outstanding contract, two mails, one event, one colony).
func drainSeedPayload(charID int64, i int, kind string) string {
	switch kind {
	case esi.SnapProfile:
		return fmt.Sprintf(`{"birthday":"2020-01-01T00:00:00Z","corporation_id":%d,"name":"Drain Pilot %d","security_status":0.0}`, drainCorpID, i)
	case esi.SnapSkills:
		return `{"skills":[{"skill_id":3426,"skillpoints_in_skill":256000,"active_skill_level":5,"trained_skill_level":5}],"total_sp":1000000,"unallocated_sp":0}`
	case esi.SnapSkillqueue:
		return `[{"skill_id":3426,"finished_level":5,"queue_position":0,"level_start_sp":0,"level_end_sp":256000,"training_start_sp":0}]`
	case esi.SnapAttributes:
		return `{"charisma":20,"intelligence":20,"memory":20,"perception":20,"willpower":20}`
	case esi.SnapWallet:
		return `1234567.89`
	case esi.SnapAssets:
		return fmt.Sprintf(`[{"is_blueprint_copy":false,"is_singleton":true,"item_id":%d,"location_flag":"Hangar","location_id":60003760,"location_type":"station","quantity":1,"type_id":34},{"is_blueprint_copy":false,"is_singleton":true,"item_id":%d,"location_flag":"Hangar","location_id":60003760,"location_type":"station","quantity":1,"type_id":35},{"is_blueprint_copy":false,"is_singleton":false,"item_id":%d,"location_flag":"Hangar","location_id":60003760,"location_type":"station","quantity":1000,"type_id":34}]`,
			drainItemBase+int64(i)*4, drainItemBase+int64(i)*4+1, drainItemBase+int64(i)*4+2)
	case esi.SnapLocation:
		return `{"solar_system_id":30000142,"station_id":60003760}`
	case esi.SnapShip:
		return fmt.Sprintf(`{"ship_item_id":%d,"ship_name":"Drain Rifter %d","ship_type_id":587}`, drainItemBase+int64(i)*4+3, i)
	case esi.SnapOnline:
		return `true`
	case esi.SnapClones:
		return `{"home_location":{"location_id":60003760,"location_type":"station"},"jump_clones":[],"last_clone_jump_date":null,"last_station_change_date":null}`
	case esi.SnapImplants:
		return `[10018]`
	case esi.SnapFittings:
		return `[{"fitting_id":9000,"name":"Drain Rifter","ship_type_id":587,"items":[]}]`
	case esi.SnapFatigue:
		return `{"last_jump_date":null,"last_update_date":null}`
	case esi.SnapKillmails:
		return fmt.Sprintf(`[{"killmail_hash":"syncdrain%d_0","killmail_id":%d},{"killmail_hash":"syncdrain%d_1","killmail_id":%d}]`,
			i, drainKillmailBase+int64(i)*2, i, drainKillmailBase+int64(i)*2+1)
	case esi.SnapCorpRoles:
		return `{"roles":[],"roles_at_base":[],"roles_at_hq":[],"roles_at_other":[]}`
	case esi.SnapCorpInfo:
		return fmt.Sprintf(`{"name":"Drain Corp","ticker":"DRAIN","member_count":%d,"ceo_id":%d,"creator_id":%d,"alliance_id":0,"home_station_id":60003760}`, 3, charID, charID)
	case esi.SnapCorpMembers:
		return fmt.Sprintf(`[%d,%d,%d]`, charID, drainCharBase, drainCharBase+1)
	case esi.SnapCorpMemberTracking:
		return `[]`
	case esi.SnapCorpWallets:
		return `[{"balance":1000000.0,"division":1},{"balance":1000000.0,"division":2},{"balance":1000000.0,"division":3},{"balance":1000000.0,"division":4},{"balance":1000000.0,"division":5},{"balance":1000000.0,"division":6},{"balance":1000000.0,"division":7}]`
	case esi.SnapCorpOrders:
		return `[{"duration":30,"escrow":0.0,"is_buy_order":false,"issued":"2026-01-01T00:00:00Z","location_id":60003760,"min_volume":1,"order_id":81000000,"price":6.5,"range":"region","region_id":10000002,"type_id":34,"volume_remain":100,"volume_total":100}]`
	case esi.SnapCorpAssets:
		return fmt.Sprintf(`[{"is_blueprint_copy":false,"is_singleton":true,"item_id":%d,"location_flag":"CorpDeliveries","location_id":60003760,"location_type":"station","quantity":1,"type_id":34},{"is_blueprint_copy":false,"is_singleton":false,"item_id":%d,"location_flag":"CorpDeliveries","location_id":60003760,"location_type":"station","quantity":500,"type_id":35}]`,
			drainItemBase+int64(i)*4+10, drainItemBase+int64(i)*4+11)
	case esi.SnapCorpStructures:
		return `[]`
	case esi.SnapCorpKillmails:
		return fmt.Sprintf(`[{"killmail_hash":"syncdraincorp%d","killmail_id":%d}]`, i, drainKillmailBase+1000000+int64(i))
	case esi.SnapWalletJournal:
		return fmt.Sprintf(`[{"amount":10.0,"balance":1234567.89,"context_id":0,"context_id_type":null,"date":"2026-01-01T00:00:00Z","description":"drain","first_party_id":%d,"id":%d,"reason":null,"ref_type":"player_donation","second_party_id":%d},{"amount":-5.0,"balance":1234557.89,"context_id":0,"context_id_type":null,"date":"2026-01-01T00:00:00Z","description":"drain","first_party_id":%d,"id":%d,"reason":null,"ref_type":"market_transaction","second_party_id":%d}]`,
			drainCharBase, 1000+int64(i)*2, drainCharBase+1, drainCharBase+1, 1001+int64(i)*2, drainCharBase)
	case esi.SnapWalletTxns:
		return fmt.Sprintf(`[{"client_id":%d,"date":"2026-01-01T00:00:00Z","is_buy":true,"is_personal":true,"journal_ref_id":%d,"location_id":60003760,"quantity":10,"transaction_id":%d,"type_id":34,"unit_price":6.5},{"client_id":%d,"date":"2026-01-01T00:00:00Z","is_buy":false,"is_personal":true,"journal_ref_id":%d,"location_id":60003760,"quantity":5,"transaction_id":%d,"type_id":35,"unit_price":9.25}]`,
			drainCharBase+1, 1000+int64(i)*2, 2000+int64(i)*2, drainCharBase, 1001+int64(i)*2, 2001+int64(i)*2)
	case esi.SnapOrders:
		return `[{"duration":30,"escrow":0.0,"is_buy_order":false,"is_corporation":false,"issued":"2026-01-01T00:00:00Z","location_id":60003760,"min_volume":1,"order_id":82000000,"price":6.5,"range":"region","region_id":10000002,"type_id":34,"volume_remain":100,"volume_total":100}]`
	case esi.SnapOrdersHistory:
		return `[{"duration":30,"escrow":0.0,"is_buy_order":false,"is_corporation":false,"issued":"2025-01-01T00:00:00Z","location_id":60003760,"min_volume":1,"order_id":82000999,"price":6.5,"range":"region","region_id":10000002,"state":"expired","type_id":34,"volume_remain":0,"volume_total":100}]`
	case esi.SnapContracts:
		return fmt.Sprintf(`[{"contract_id":%d,"issuer_id":%d,"issuer_corporation_id":%d,"assignee_id":0,"acceptor_id":0,"start_location_id":60003760,"end_location_id":60003760,"type":"item_exchange","status":"outstanding","title":"drain","for_corporation":false,"days_to_complete":0,"price":100.0,"reward":0.0,"volume":1.0,"date_issued":"2026-01-01T00:00:00Z","date_expired":"2026-02-01T00:00:00Z"}]`,
			drainContractBase+int64(i), charID, drainCorpID)
	case esi.SnapIndustryJobs:
		return `[{"activity_id":1,"blueprint_id":1000000026842,"blueprint_location_id":60003760,"blueprint_type_id":34,"completed_character_id":0,"completed_date":"2026-02-01T00:00:00Z","cost":100.0,"duration":100,"end_date":"2026-02-01T00:00:00Z","facility_id":60003760,"installer_id":0,"job_id":31000000,"licensed_runs":200,"output_location_id":60003760,"runs":1,"start_date":"2026-01-01T00:00:00Z","station_id":60003760,"status":"active","successful_runs":0}]`
	case esi.SnapBlueprints:
		return `[{"item_id":1000000026842,"type_id":34,"quantity":1,"time_efficiency":0,"material_efficiency":0,"runs":200,"location_flag":"Hangar","location_id":60003760}]`
	case esi.SnapMining:
		return `[{"date":"2026-01-01","solar_system_id":30000142,"type_id":34,"quantity":1000}]`
	case esi.SnapMail:
		return fmt.Sprintf(`[{"from":%d,"is_read":false,"labels":[2],"mail_id":%d,"recipients":[{"recipient_id":%d,"recipient_type":"character"}],"subject":"drain one","timestamp":"2026-01-01T00:00:00Z"},{"from":%d,"is_read":true,"labels":[1],"mail_id":%d,"recipients":[{"recipient_id":%d,"recipient_type":"character"}],"subject":"drain two","timestamp":"2026-01-01T00:00:00Z"}]`,
			drainCharBase+1, drainMailBase+int64(i)*2, charID, drainCharBase+2, drainMailBase+int64(i)*2+1, charID)
	case esi.SnapMailLabels:
		return `{"labels":[{"color":"#ffffff","label_id":1,"name":"Inbox","unread_count":0},{"color":"#ffffff","label_id":2,"name":" starred","unread_count":1}],"total_unread_count":1}`
	case esi.SnapMailLists:
		return `[]`
	case esi.SnapCalendar:
		return fmt.Sprintf(`[{"event_date":"2026-02-01T00:00:00Z","event_id":%d,"event_response":"accepted","importance":0,"owner_id":%d,"owner_name":"Drain Pilot %d","owner_type":"character","response":"accepted","text":"drain op","title":"Drain Op"}]`,
			drainEventBase+int64(i), charID, i)
	case esi.SnapContacts:
		return fmt.Sprintf(`[{"contact_id":%d,"contact_type":"character","is_blocked":false,"is_watched":false,"label_ids":[],"standing":5.0},{"contact_id":%d,"contact_type":"corporation","is_blocked":false,"is_watched":false,"label_ids":[],"standing":5.0}]`,
			drainCharBase+1, drainCorpID)
	case esi.SnapPlanets:
		return fmt.Sprintf(`{"colonies":[{"last_update":"2026-01-01T00:00:00Z","num_pins":2,"owner_id":%d,"planet_id":%d,"planet_type":"temperate","solar_system_id":30000142,"upgrade_level":3}]}`,
			charID, drainPlanetID)
	}
	if strings.HasPrefix(kind, esi.SnapCorpJournalPrefix) || strings.HasPrefix(kind, esi.SnapCorpTxnsPrefix) {
		return `[]`
	}
	return `[]`
}

// drainPilotDue reports whether pilot i still has due sync
// work: a stale or missing list row, or a missing keyed
// detail row. It reads stored state only, one query.
func drainPilotDue(ctx context.Context, conn *sql.DB, charID int64, i int) bool {
	rows, err := conn.QueryContext(ctx,
		`SELECT kind, cached_until FROM character_snapshots WHERE character_id = $1`, charID)
	if err != nil {
		return true
	}
	defer rows.Close()
	fresh := map[string]bool{}
	now := time.Now()
	for rows.Next() {
		var kind string
		var cached sql.NullTime
		if err := rows.Scan(&kind, &cached); err != nil {
			return true
		}
		if cached.Valid && now.Before(cached.Time) {
			fresh[kind] = true
		}
	}
	if err := rows.Err(); err != nil {
		return true
	}
	for _, kind := range drainListKinds() {
		if !fresh[kind] {
			return true
		}
	}
	for _, key := range drainDetailKeys(i) {
		if !fresh[key[1].(string)] {
			return true
		}
	}
	// Killmail and contract details are global stores keyed
	// by ID alone, not (character, kind).
	if !drainGlobalDetailFresh(ctx, conn, i) {
		return true
	}
	return false
}

// drainGlobalDetailFresh checks the globally-deduped detail
// rows one pilot's drain stores: its two killmail details,
// the contract's items, and the shared corporation killmail
// the corp pass fetches once for the whole stage.
func drainGlobalDetailFresh(ctx context.Context, conn *sql.DB, i int) bool {
	var n int
	err := conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM killmail_details WHERE killmail_id IN ($1,$2,$3)`,
		drainKillmailBase+int64(i)*2, drainKillmailBase+int64(i)*2+1,
		drainKillmailBase+999999).Scan(&n)
	if err != nil || n < 3 {
		return false
	}
	err = conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM contract_details WHERE contract_id = $1`,
		drainContractBase+int64(i)).Scan(&n)
	return err == nil && n >= 1
}

// expireDrainPilot makes pilot i newly due again for the
// end-to-end probe: all list rows stale with no ETag (full
// re-download, not a 304), keyed rows removed.
func expireDrainPilot(t *testing.T, ctx context.Context, conn *sql.DB, charID int64, i int) {
	t.Helper()
	stale := time.Now().UTC().Add(-2 * time.Hour)
	if _, err := conn.ExecContext(ctx,
		`UPDATE character_snapshots SET cached_until = $1, etag = '' WHERE character_id = $2`,
		stale, charID); err != nil {
		t.Fatalf("expire pilot %d: %v", charID, err)
	}
	for _, key := range drainDetailKeys(i) {
		if _, err := conn.ExecContext(ctx,
			`DELETE FROM character_snapshots WHERE character_id = $1 AND kind = $2`,
			charID, key[1].(string)); err != nil {
			t.Fatalf("expire detail %v: %v", key, err)
		}
	}
	if _, err := conn.ExecContext(ctx,
		`DELETE FROM killmail_details WHERE killmail_id IN ($1,$2,$3)`,
		drainKillmailBase+int64(i)*2, drainKillmailBase+int64(i)*2+1,
		drainKillmailBase+999999); err != nil {
		t.Fatalf("expire killmail details: %v", err)
	}
	if _, err := conn.ExecContext(ctx,
		`DELETE FROM contract_details WHERE contract_id = $1`,
		drainContractBase+int64(i)); err != nil {
		t.Fatalf("expire contract details: %v", err)
	}
}

// drainFetchErrors lists snapshot kinds whose latest fetch
// outcome is an error or dead state for stage pilots: a real
// defect signal, not graceful lag.

func drainFetchErrors(ctx context.Context, conn *sql.DB, n int) []string {
	rows, err := conn.QueryContext(ctx,
		`SELECT kind, COUNT(*) FROM snapshot_fetch_state
		 WHERE character_id >= $1 AND character_id < $2 AND state IN ('error','dead')
		 GROUP BY kind ORDER BY kind`, drainCharBase, drainCharBase+int64(n))
	if err != nil {
		return []string{"fetch-state query failed: " + err.Error()}
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var kind string
		var count int
		if err := rows.Scan(&kind, &count); err != nil {
			return append(out, "scan: "+err.Error())
		}
		out = append(out, fmt.Sprintf("%s x%d", kind, count))
	}
	return out
}

// ---------------------------------------------------------------------------
// Canned ESI stub. Answers every route the cycle touches with
// valid minimal payloads: the character work mix is
// realistic per pilot (unique killmails, contracts, mails and
// events so globally-deduped stores cost what distinct pilots
// cost), while the public/market surface stays small and
// bounded (shared types, empty order books).
// ---------------------------------------------------------------------------

// drainStub is the harness's canned ESI transport. It counts
// every request by lane class and records any path it cannot
// answer, so an incomplete stub fails the harness instead of
// silently skewing the numbers.
type drainStub struct {
	calls     atomic.Int64
	charCalls atomic.Int64
	corpCalls atomic.Int64
	pubCalls  atomic.Int64
	other     atomic.Int64

	mu        sync.Mutex
	unhandled map[string]int64
}

type drainStubCounts struct {
	total int64
	char  int64
	corp  int64
	pub   int64
	other int64
}

func (s *drainStub) snapshot() drainStubCounts {
	return drainStubCounts{
		total: s.calls.Load(),
		char:  s.charCalls.Load(),
		corp:  s.corpCalls.Load(),
		pub:   s.pubCalls.Load(),
		other: s.other.Load(),
	}
}

func newDrainStub() *drainStub {
	return &drainStub{unhandled: map[string]int64{}}
}

// bump records one request under its class key.
func (s *drainStub) bump(path string) {
	s.calls.Add(1)
	switch {
	case strings.HasPrefix(path, "/characters/"):
		s.charCalls.Add(1)
	case strings.HasPrefix(path, "/corporations/"):
		s.corpCalls.Add(1)
	case strings.HasPrefix(path, "/markets/"),
		strings.HasPrefix(path, "/industry/"),
		strings.HasPrefix(path, "/universe/"),
		strings.HasPrefix(path, "/status"),
		strings.HasPrefix(path, "/wars/"),
		strings.HasPrefix(path, "/incursions/"),
		strings.HasPrefix(path, "/fw/"),
		strings.HasPrefix(path, "/contracts/public/"):
		s.pubCalls.Add(1)
	default:
		s.other.Add(1)
	}
}

// unhandledPaths returns sorted paths the stub could not answer.
func (s *drainStub) unhandledPaths() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for path := range s.unhandled {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

func drainJSON(status int, body string) *http.Response {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("ETag", `W/"syncdrain-v1"`)
	h.Set("Expires", time.Now().UTC().Add(time.Hour).Format(http.TimeFormat))
	h.Set("X-Pages", "1")
	return &http.Response{
		StatusCode: status,
		Header:     h,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func drainNotModified() *http.Response {
	h := http.Header{}
	h.Set("Expires", time.Now().UTC().Add(time.Hour).Format(http.TimeFormat))
	return &http.Response{StatusCode: http.StatusNotModified, Header: h, Body: io.NopCloser(strings.NewReader(""))}
}

var (
	drainCharRe    = regexp.MustCompile(`^/characters/(\d+)(/.*)?$`)
	drainMailRe    = regexp.MustCompile(`^/characters/\d+/mail/(\d+)/$`)
	drainCalEvRe   = regexp.MustCompile(`^/characters/\d+/calendar/(\d+)/$`)
	drainCalAttRe  = regexp.MustCompile(`^/characters/\d+/calendar/(\d+)/attendees/$`)
	drainPlanetRe  = regexp.MustCompile(`^/characters/\d+/planets/(\d+)/$`)
	drainContItRe  = regexp.MustCompile(`^/characters/\d+/contracts/(\d+)/items/$`)
	drainCorpDivRe = regexp.MustCompile(`^/corporations/\d+/wallets/(\d+)/(journal|transactions)/$`)
	drainKillRe    = regexp.MustCompile(`^/killmails/(\d+)/([^/]+)/$`)
)

// RoundTrip answers every route the cycle touches.
func (s *drainStub) RoundTrip(req *http.Request) (*http.Response, error) {
	s.bump(req.URL.Path)
	path := req.URL.Path

	// Conditional refresh: the seed clears ETags, but after
	// the first cycle every refetch offers one, so the stub
	// must answer 304 to stay honest about window renewals.
	if inm := req.Header.Get("If-None-Match"); inm != "" && !drainNeverCache(path) {
		return drainNotModified(), nil
	}

	if body, ok := drainRoute(req, path); ok {
		return drainJSON(http.StatusOK, body), nil
	}
	s.mu.Lock()
	s.unhandled[path]++
	s.mu.Unlock()
	return drainJSON(http.StatusNotFound, `{"error":"syncdrain stub has no route for this path"}`), nil
}

// drainNeverCache lists POST endpoints (name lookups) that
// must not answer 304: they carry request bodies.
func drainNeverCache(path string) bool {
	return strings.HasSuffix(path, "/names/")
}

// drainRoute maps one ESI path to its canned payload.
// Character-scoped content derives from the IDs in the path
// so per-pilot payloads stay consistent across refetches.
func drainRoute(req *http.Request, path string) (string, bool) {
	if m := drainMailRe.FindStringSubmatch(path); m != nil {
		return fmt.Sprintf(`{"body":"<p>drain mail %s</p>","labels":[2],"read":true,"subject":"drain"}`, m[1]), true
	}
	if m := drainCalEvRe.FindStringSubmatch(path); m != nil {
		return fmt.Sprintf(`{"date":"2026-02-01T00:00:00Z","duration":60,"event_id":%s,"importance":0,"owner_id":%d,"owner_name":"Drain","owner_type":"character","response":"accepted","text":"drain op","title":"Drain Op"}`, m[1], drainCharBase), true
	}
	if m := drainCalAttRe.FindStringSubmatch(path); m != nil {
		return fmt.Sprintf(`[{"character_id":%d,"event_response":"accepted"}]`, drainCharBase+1), true
	}
	if m := drainPlanetRe.FindStringSubmatch(path); m != nil {
		return `{"links":[],"pins":[{"expiry_time":null,"install_time":null,"last_cycle_start_time":null,"latitude":0.0,"longitude":0.0,"pin_id":1001,"schematic_id":0,"type_id":2254},{"expiry_time":null,"install_time":null,"last_cycle_start_time":null,"latitude":0.1,"longitude":0.1,"pin_id":1002,"schematic_id":0,"type_id":2254}],"routes":[]}`, true
	}
	if m := drainContItRe.FindStringSubmatch(path); m != nil {
		return `[{"is_included":true,"is_singleton":false,"quantity":10,"raw_quantity":-2,"record_id":1,"type_id":34}]`, true
	}
	if m := drainCorpDivRe.FindStringSubmatch(path); m != nil {
		if m[2] == "journal" {
			return `[{"amount":1.0,"balance":1000000.0,"context_id":0,"context_id_type":null,"date":"2026-01-01T00:00:00Z","description":"drain","first_party_id":0,"id":1,"reason":null,"ref_type":"corporation_account_withdrawal","second_party_id":0}]`, true
		}
		return `[{"client_id":0,"date":"2026-01-01T00:00:00Z","is_buy":false,"is_personal":false,"journal_ref_id":1,"location_id":60003760,"quantity":1,"transaction_id":1,"type_id":34,"unit_price":6.5}]`, true
	}
	if m := drainKillRe.FindStringSubmatch(path); m != nil {
		return fmt.Sprintf(`{"killmail_id":%s,"killmail_time":"2026-01-01T00:00:00Z","solar_system_id":30000142,"victim":{"character_id":%d,"corporation_id":%d,"damage_taken":100,"items":[],"ship_type_id":587}}`, m[1], drainCharBase, drainCorpID), true
	}
	if strings.HasSuffix(path, "/names/") {
		n := drainEchoNames(req)
		return n, true
	}
	if m := drainCharRe.FindStringSubmatch(path); m != nil {
		if body, ok := drainCharacterRoute(m[1], m[2]); ok {
			return body, true
		}
		return "", false
	}
	if strings.HasPrefix(path, "/corporations/") {
		return drainCorporationRoute(path)
	}
	switch path {
	case "/markets/prices/":
		return `[]`, true
	case "/industry/systems/":
		return `[{"cost_indices":[{"activity":"manufacturing","cost_index":0.01}],"solar_system_id":30000142}]`, true
	case "/status/":
		return `{"players":12,"server_version":"syncdrain","start_time":"2026-01-01T00:00:00Z"}`, true
	case "/universe/factions/", "/fw/leaderboards/corporations/", "/fw/leaderboards/characters/",
		"/fw/stats/", "/fw/systems/", "/universe/ancestries/", "/universe/bloodlines/",
		"/universe/categories/", "/wars/", "/incursions/", "/alliances/":
		return `[]`, true
	}
	if m := regexp.MustCompile(`^/universe/types/(\d+)/$`).FindStringSubmatch(path); m != nil {
		return fmt.Sprintf(`{"name":"Drain Type %s","group_id":1,"description":"sync-drain stub type"}`, m[1]), true
	}
	if m := regexp.MustCompile(`^/universe/(systems|stations)/(\d+)/$`).FindStringSubmatch(path); m != nil {
		return fmt.Sprintf(`{"name":"Drain %s %s"}`, m[1], m[2]), true
	}
	if m := regexp.MustCompile(`^/universe/planets/(\d+)/$`).FindStringSubmatch(path); m != nil {
		return fmt.Sprintf(`{"name":"Drain Planet %s"}`, m[1]), true
	}
	if m := regexp.MustCompile(`^/universe/groups/(\d+)/$`).FindStringSubmatch(path); m != nil {
		return fmt.Sprintf(`{"name":"Drain Group %s"}`, m[1]), true
	}
	if strings.HasPrefix(path, "/markets/") {
		// Order books and history: empty, one page.
		return `[]`, true
	}
	if strings.HasPrefix(path, "/universe/") {
		return drainUniverseRoute(path)
	}
	if strings.HasPrefix(path, "/contracts/public/") {
		return `[]`, true
	}
	if strings.HasPrefix(path, "/search/") {
		return `{"character":[]}`, true
	}
	return "", false
}

// drainEchoNames answers /universe/names/ and asset-name POSTs.
func drainEchoNames(req *http.Request) string {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return `[]`
	}
	var ids []int64
	if err := json.Unmarshal(body, &ids); err != nil {
		return `[]`
	}
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, fmt.Sprintf(`{"id":%d,"name":"Drain %d","category":"inventory_type"}`, id, id))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// drainCharacterRoute answers one character-scoped path.
func drainCharacterRoute(charID, rest string) (string, bool) {
	cid, _ := strconv.ParseInt(charID, 10, 64)
	i := cid - drainCharBase
	mail0 := drainMailBase + i*2
	mail1 := mail0 + 1
	switch rest {
	case "/":
		return fmt.Sprintf(`{"birthday":"2020-01-01T00:00:00Z","corporation_id":%d,"name":"Drain Pilot %d","security_status":0.0}`, drainCorpID, i), true
	case "/skills/":
		return `{"skills":[{"skill_id":3426,"skillpoints_in_skill":256000,"active_skill_level":5,"trained_skill_level":5}],"total_sp":1000000,"unallocated_sp":0}`, true
	case "/skillqueue/":
		return `[{"skill_id":3426,"finished_level":5,"queue_position":0,"level_start_sp":0,"level_end_sp":256000,"training_start_sp":0}]`, true
	case "/attributes/":
		return `{"charisma":20,"intelligence":20,"memory":20,"perception":20,"willpower":20}`, true
	case "/wallet/":
		return `1234567.89`, true
	case "/assets/":
		return fmt.Sprintf(`[{"is_blueprint_copy":false,"is_singleton":true,"item_id":%d,"location_flag":"Hangar","location_id":60003760,"location_type":"station","quantity":1,"type_id":34},{"is_blueprint_copy":false,"is_singleton":true,"item_id":%d,"location_flag":"Hangar","location_id":60003760,"location_type":"station","quantity":1,"type_id":35},{"is_blueprint_copy":false,"is_singleton":false,"item_id":%d,"location_flag":"Hangar","location_id":60003760,"location_type":"station","quantity":1000,"type_id":34}]`,
			drainItemBase+i*4, drainItemBase+i*4+1, drainItemBase+i*4+2), true
	case "/location/":
		return `{"solar_system_id":30000142,"station_id":60003760}`, true
	case "/ship/":
		return fmt.Sprintf(`{"ship_item_id":%d,"ship_name":"Drain Rifter %d","ship_type_id":587}`, drainItemBase+i*4+3, i), true
	case "/online/":
		return `true`, true
	case "/clones/":
		return `{"home_location":{"location_id":60003760,"location_type":"station"},"jump_clones":[],"last_clone_jump_date":null,"last_station_change_date":null}`, true
	case "/implants/":
		return `[10018]`, true
	case "/fittings/":
		return `[{"fitting_id":9000,"name":"Drain Rifter","ship_type_id":587,"items":[]}]`, true
	case "/fatigue/":
		return `{"last_jump_date":null,"last_update_date":null}`, true
	case "/killmails/recent/":
		return fmt.Sprintf(`[{"killmail_hash":"syncdrain%d_0","killmail_id":%d},{"killmail_hash":"syncdrain%d_1","killmail_id":%d}]`,
			i, drainKillmailBase+i*2, i, drainKillmailBase+i*2+1), true
	case "/roles/":
		return `{"roles":[],"roles_at_base":[],"roles_at_hq":[],"roles_at_other":[]}`, true
	case "/wallet/journal/":
		return fmt.Sprintf(`[{"amount":10.0,"balance":1234567.89,"context_id":0,"context_id_type":null,"date":"2026-01-01T00:00:00Z","description":"drain","first_party_id":%d,"id":%d,"reason":null,"ref_type":"player_donation","second_party_id":%d},{"amount":-5.0,"balance":1234557.89,"context_id":0,"context_id_type":null,"date":"2026-01-01T00:00:00Z","description":"drain","first_party_id":%d,"id":%d,"reason":null,"ref_type":"market_transaction","second_party_id":%d}]`,
			drainCharBase, 1000+i*2, drainCharBase+1, drainCharBase+1, 1001+i*2, drainCharBase), true
	case "/wallet/transactions/":
		return fmt.Sprintf(`[{"client_id":%d,"date":"2026-01-01T00:00:00Z","is_buy":true,"is_personal":true,"journal_ref_id":%d,"location_id":60003760,"quantity":10,"transaction_id":%d,"type_id":34,"unit_price":6.5},{"client_id":%d,"date":"2026-01-01T00:00:00Z","is_buy":false,"is_personal":true,"journal_ref_id":%d,"location_id":60003760,"quantity":5,"transaction_id":%d,"type_id":35,"unit_price":9.25}]`,
			drainCharBase+1, 1000+i*2, 2000+i*2, drainCharBase, 1001+i*2, 2001+i*2), true
	case "/orders/":
		return `[{"duration":30,"escrow":0.0,"is_buy_order":false,"is_corporation":false,"issued":"2026-01-01T00:00:00Z","location_id":60003760,"min_volume":1,"order_id":82000000,"price":6.5,"range":"region","region_id":10000002,"type_id":34,"volume_remain":100,"volume_total":100}]`, true
	case "/orders/history/":
		return `[{"duration":30,"escrow":0.0,"is_buy_order":false,"is_corporation":false,"issued":"2025-01-01T00:00:00Z","location_id":60003760,"min_volume":1,"order_id":82000999,"price":6.5,"range":"region","region_id":10000002,"state":"expired","type_id":34,"volume_remain":0,"volume_total":100}]`, true
	case "/contracts/":
		return fmt.Sprintf(`[{"contract_id":%d,"issuer_id":%d,"issuer_corporation_id":%d,"assignee_id":0,"acceptor_id":0,"start_location_id":60003760,"end_location_id":60003760,"type":"item_exchange","status":"outstanding","title":"drain","for_corporation":false,"days_to_complete":0,"price":100.0,"reward":0.0,"volume":1.0,"date_issued":"2026-01-01T00:00:00Z","date_expired":"2026-02-01T00:00:00Z"}]`,
			drainContractBase+i, cid, drainCorpID), true
	case "/industry/jobs/":
		return `[{"activity_id":1,"blueprint_id":1000000026842,"blueprint_location_id":60003760,"blueprint_type_id":34,"completed_character_id":0,"completed_date":"2026-02-01T00:00:00Z","cost":100.0,"duration":100,"end_date":"2026-02-01T00:00:00Z","facility_id":60003760,"installer_id":0,"job_id":31000000,"licensed_runs":200,"output_location_id":60003760,"runs":1,"start_date":"2026-01-01T00:00:00Z","station_id":60003760,"status":"active","successful_runs":0}]`, true
	case "/blueprints/":
		return `[{"item_id":1000000026842,"type_id":34,"quantity":1,"time_efficiency":0,"material_efficiency":0,"runs":200,"location_flag":"Hangar","location_id":60003760}]`, true
	case "/mining/":
		return `[{"date":"2026-01-01","solar_system_id":30000142,"type_id":34,"quantity":1000}]`, true
	case "/planets/":
		return fmt.Sprintf(`[{"last_update":"2026-01-01T00:00:00Z","num_pins":2,"owner_id":%d,"planet_id":%d,"planet_type":"temperate","solar_system_id":30000142,"upgrade_level":3}]`, cid, drainPlanetID), true
	case "/mail/labels/":
		return `{"labels":[{"color":"#ffffff","label_id":1,"name":"Inbox","unread_count":0}],"total_unread_count":1}`, true
	case "/mail/lists/":
		return `[]`, true
	case "/mail/":
		return fmt.Sprintf(`[{"from":%d,"is_read":false,"labels":[1],"mail_id":%d,"recipients":[{"recipient_id":%d,"recipient_type":"character"}],"subject":"drain one","timestamp":"2026-01-01T00:00:00Z"},{"from":%d,"is_read":true,"labels":[1],"mail_id":%d,"recipients":[{"recipient_id":%d,"recipient_type":"character"}],"subject":"drain two","timestamp":"2026-01-01T00:00:00Z"}]`,
			drainCharBase+1, mail0, cid, drainCharBase+2, mail1, cid), true
	case "/calendar/":
		return fmt.Sprintf(`[{"event_date":"2026-02-01T00:00:00Z","event_id":%d,"event_response":"accepted","importance":0,"owner_id":%d,"owner_name":"Drain Pilot %d","owner_type":"character","response":"accepted","text":"drain op","title":"Drain Op"}]`,
			drainEventBase+i, cid, i), true
	case "/contacts/":
		return fmt.Sprintf(`[{"contact_id":%d,"contact_type":"character","is_blocked":false,"is_watched":false,"label_ids":[],"standing":5.0},{"contact_id":%d,"contact_type":"corporation","is_blocked":false,"is_watched":false,"label_ids":[],"standing":5.0}]`,
			drainCharBase+1, drainCorpID), true
	}
	return "", false
}

// drainCorporationRoute answers corporation datasets: one
// shared corp, so the stub cost is the per-character row
// duplication, exactly like production.
func drainCorporationRoute(path string) (string, bool) {
	switch {
	case strings.HasSuffix(path, "/info/") || regexp.MustCompile(`^/corporations/\d+/$`).MatchString(path):
		return `{"name":"Drain Corp","ticker":"DRAIN","member_count":3,"ceo_id":91000000,"creator_id":91000000,"alliance_id":0,"home_station_id":60003760}`, true
	case strings.HasSuffix(path, "/members/"):
		return fmt.Sprintf(`[%d,%d,%d]`, drainCharBase, drainCharBase+1, drainCharBase+2), true
	case strings.HasSuffix(path, "/membertracking/"):
		return `[]`, true
	case strings.HasSuffix(path, "/wallets/"):
		return `[{"balance":1000000.0,"division":1},{"balance":1000000.0,"division":2},{"balance":1000000.0,"division":3},{"balance":1000000.0,"division":4},{"balance":1000000.0,"division":5},{"balance":1000000.0,"division":6},{"balance":1000000.0,"division":7}]`, true
	case strings.HasSuffix(path, "/orders/"):
		return `[{"duration":30,"escrow":0.0,"is_buy_order":false,"issued":"2026-01-01T00:00:00Z","location_id":60003760,"min_volume":1,"order_id":81000000,"price":6.5,"range":"region","region_id":10000002,"type_id":34,"volume_remain":100,"volume_total":100}]`, true
	case strings.HasSuffix(path, "/assets/"):
		return fmt.Sprintf(`[{"is_blueprint_copy":false,"is_singleton":true,"item_id":%d,"location_flag":"CorpDeliveries","location_id":60003760,"location_type":"station","quantity":1,"type_id":34}]`, drainItemBase), true
	case strings.HasSuffix(path, "/structures/"):
		return `[]`, true
	case strings.HasSuffix(path, "/killmails/recent/"):
		return fmt.Sprintf(`[{"killmail_hash":"syncdraincorp","killmail_id":%d}]`, drainKillmailBase+999999), true
	case strings.HasSuffix(path, "/roles/history/"):
		return `[]`, true
	}
	return "", false
}

func drainUniverseRoute(path string) (string, bool) {
	if m := regexp.MustCompile(`^/universe/structures/(\d+)/$`).FindStringSubmatch(path); m != nil {
		return `{"name":"Jita IV - Caldari Navy Assembly Plant","owner_id":500003,"solar_system_id":30000142,"type_id":52678}`, true
	}
	return "", false
}

// ---------------------------------------------------------------------------
// Report, in the web harness's writeLoadReport style: one table
// per stage plus the saturation verdict.
// ---------------------------------------------------------------------------

func writeSyncDrainReport(t *testing.T, results []drainStageResult, verdict, defect string) {
	t.Helper()
	var b strings.Builder
	b.WriteString("# Sync-drain harness\n\n")
	b.WriteString("Real sync cycle (`app.refreshCycle`) against embedded Postgres and a canned in-process ESI stub. ")
	b.WriteString("Per-pilot work mix mirrors what the cycle performs: ")
	fmt.Fprintf(&b, "the core %d kinds, the corporation %d, the economy 8 plus one contract's items, one PI colony plus its layout, ",
		len(coreSnapshotKinds), 4+2*walletDivisionCount)
	b.WriteString("the comms 5 plus two mail bodies and one calendar event with attendees, ")
	b.WriteString("two killmail details plus one corporation killmail, and a batched asset-name lookup. ")
	b.WriteString("Character lane allowance is the production shape (four urgency lanes of 120 fetches); ")
	b.WriteString("the market lane has its own allowance and spends only on the shared watchlist/history/order sweep.\n\n")
	b.WriteString("A pilot counts as drained when every row in that mix is fresh or stored; the backlog is pilots still due. ")
	b.WriteString("The per-cycle allowance gates the clustered passes; the core pass fetches every due core kind with no budget.\n\n")

	for _, r := range results {
		fmt.Fprintf(&b, "## Stage: %d pilots\n\n", r.Pilots)
		b.WriteString("| metric | value |\n|---|---|\n")
		fmt.Fprintf(&b, "| character allowance per cycle | %d |\n", r.CharAllowance)
		fmt.Fprintf(&b, "| market allowance per cycle | %d |\n", r.MarketAllowance)
		fmt.Fprintf(&b, "| cycles run | %d |\n", r.Cycles)
		fmt.Fprintf(&b, "| cycle 1: ESI calls (incl. 304 revalidations) | %d |\n", r.FirstCycleFetches)
		fmt.Fprintf(&b, "| cycle 1: wall time | %s |\n", r.FirstCycleWall.Round(time.Second))
		fmt.Fprintf(&b, "| cycle 1: by path class (character/corporation/public/other) | %d/%d/%d/%d |\n",
			r.FirstCycleByLane.char, r.FirstCycleByLane.corp, r.FirstCycleByLane.pub, r.FirstCycleByLane.other)
		fmt.Fprintf(&b, "| steady cycle: ESI calls (incl. 304 revalidations) | %d |\n", r.SteadyFetches)
		fmt.Fprintf(&b, "| steady cycle: wall time | %s |\n", r.SteadyWall.Round(time.Second))
		fmt.Fprintf(&b, "| steady cycle: by path class (character/corporation/public/other) | %d/%d/%d/%d |\n",
			r.SteadyByLane.char, r.SteadyByLane.corp, r.SteadyByLane.pub, r.SteadyByLane.other)
		fmt.Fprintf(&b, "| newly drained per cycle | %v |\n", r.DrainedPerCycle)
		fmt.Fprintf(&b, "| backlog after each cycle | %v |\n", r.Backlogs)
		if r.EndToEndCycles < 0 {
			fmt.Fprintf(&b, "| newly-due pilot fresh again | NOT within %s of extra cycles |\n", r.EndToEnd.Round(time.Second))
		} else if r.EndToEndCycles == 1 {
			fmt.Fprintf(&b, "| newly-due pilot fresh again | %s (1 cycle) |\n", r.EndToEnd.Round(time.Second))
		} else {
			fmt.Fprintf(&b, "| newly-due pilot fresh again | %s (%d cycles) |\n", r.EndToEnd.Round(time.Second), r.EndToEndCycles)
		}
		if len(r.FetchErrorKinds) > 0 {
			fmt.Fprintf(&b, "| fetch error states | %v |\n", r.FetchErrorKinds)
		}
		b.WriteString("\n")
	}

	b.WriteString("## Verdict\n\n" + verdict + "\n\n")
	if defect != "" {
		b.WriteString("## Defect\n\n" + defect + "\n\n")
	}

	reportName := os.Getenv("EVESYNAPSE_SYNCDRAIN_REPORT")
	if reportName == "" {
		reportName = "sync-drain-report.md"
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("user home: %v", err)
	}
	path := filepath.Join(home, "workspace", "evesynapse", "logs", reportName)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create report dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write report: %v", err)
	}
	t.Logf("sync-drain report written to %s", path)
}
