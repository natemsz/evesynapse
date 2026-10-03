package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// workerStatus is the worker's user-visible heartbeat: when the last
// cycle ran, what it did, and how many names it has resolved since
// boot. The Sync and Admin pages read it via snapshotWorkerStatus.
type workerStatus struct {
	LastRunAt          time.Time
	Summary            string
	Warming            bool // a cycle is running right now
	NamesResolvedTotal int  // cumulative since boot
}

// markCharacterPriority flags a character for first-in-line warm-up
// on the next worker cycle (fresh SSO logins, Sync-page re-warm
// requests).
func (app *Application) markCharacterPriority(characterID int64) {
	app.priorityMu.Lock()
	defer app.priorityMu.Unlock()
	app.priorityChars[characterID] = true
}

// takePriorityCharacters drains the priority set: the IDs are
// consumed (removed) and returned for this cycle's ordering.
func (app *Application) takePriorityCharacters() []int64 {
	app.priorityMu.Lock()
	defer app.priorityMu.Unlock()
	ids := make([]int64, 0, len(app.priorityChars))
	for id := range app.priorityChars {
		ids = append(ids, id)
	}
	app.priorityChars = make(map[int64]bool)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func (app *Application) snapshotWorkerStatus() workerStatus {
	app.workerMu.Lock()
	defer app.workerMu.Unlock()
	return app.worker
}

func (app *Application) updateWorkerStatus(fn func(*workerStatus)) {
	app.workerMu.Lock()
	defer app.workerMu.Unlock()
	fn(&app.worker)
}

// workerStatusText renders the status for the Sync/Admin pages.
func (app *Application) workerStatusText() string {
	s := app.snapshotWorkerStatus()
	if s.LastRunAt.IsZero() {
		return "Worker starting…"
	}
	summary := s.Summary
	if summary == "" {
		summary = "idle"
	}
	if s.Warming {
		summary += " (warming now)"
	}
	return fmt.Sprintf("%s · last run %s · %s names resolved since boot",
		summary, s.LastRunAt.UTC().Format("2006-01-02 15:04 UTC"), esi.FormatInt(int64(s.NamesResolvedTotal)))
}

// runWorker is the background ESI refresh scheduler. Every minute it
// walks every linked character, makes sure their access token is
// usable (refreshing near-expiry tokens), and re-fetches any cached
// snapshot whose cached_until has passed. It then warms the local
// name caches (types, groups, places) from the fresh snapshots so
// page renders never wait on ESI. If ESI answers with its
// error-limit status (420/429) the cycle stops and the next tick
// retries — CCP asks clients to back off instead of hammering.
//
// Logging is deliberately quiet: one summary line per cycle only when
// something was refreshed or failed, plus a 10-minute heartbeat so
// liveness is visible without spamming the console.
//
// The same goroutine also hosts the hourly SDE maintenance tick
// (first import + weekly update check; sde.go).
func (app *Application) runWorker(ctx context.Context) {
	log.Printf("worker: started")

	// SDE maintenance runs alongside the ESI cycle: a first import
	// when the static-data tables are empty, then a weekly update
	// check (patch-day cadence) — see sdeMaintenance in sde.go. It
	// only ever starts background operations, so it never delays
	// snapshot refreshes.
	go app.sdeMaintenance(ctx)

	cycle := time.NewTicker(time.Minute)
	heartbeat := time.NewTicker(10 * time.Minute)
	sdeTick := time.NewTicker(time.Hour)
	defer cycle.Stop()
	defer heartbeat.Stop()
	defer sdeTick.Stop()

	// First pass right away so a cold start doesn't wait a minute for
	// fresh data.
	app.refreshCycle(ctx)

	for {
		select {
		case <-ctx.Done():
			log.Printf("worker: stopped")
			return
		case <-heartbeat.C:
			log.Printf("worker: alive")
		case <-sdeTick.C:
			app.sdeMaintenance(ctx)
		case <-cycle.C:
			app.refreshCycle(ctx)
		}
	}
}

// refreshCycle performs one pass over all characters. See runWorker.
func (app *Application) refreshCycle(ctx context.Context) {
	app.updateWorkerStatus(func(s *workerStatus) {
		s.LastRunAt = time.Now()
		s.Warming = true
	})

	characters, err := app.queries.ListAllCharacters(ctx)
	if err != nil {
		log.Printf("worker: list characters: %v", err)
		app.updateWorkerStatus(func(s *workerStatus) {
			s.Warming = false
			s.Summary = "could not list characters"
		})
		return
	}

	// Characters flagged since the last cycle (fresh logins, Sync
	// page requests) warm first; the rest follow in list order.
	characters = orderByPriority(characters, app.takePriorityCharacters())

	var refreshed, failed int
	limited := false

	for _, ch := range characters {
		if ctx.Err() != nil {
			app.updateWorkerStatus(func(s *workerStatus) { s.Warming = false })
			return
		}

		// Ensure the token is usable before touching snapshots; a
		// revoked refresh token means this character needs a fresh
		// login, and fetching would only fail three more times.
		if _, err := app.validAccessToken(ctx, ch); err != nil {
			log.Printf("worker: token for character %d unusable: %v", ch.CharacterID, err)
			failed++
			continue
		}

		for _, kind := range []string{
			esi.SnapSkills, esi.SnapSkillqueue, esi.SnapWallet, esi.SnapAssets,
			esi.SnapLocation, esi.SnapShip, esi.SnapOnline, esi.SnapClones,
			esi.SnapImplants, esi.SnapFittings, esi.SnapFatigue, esi.SnapKillmails,
		} {
			snap, serr := app.queries.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: ch.CharacterID, Kind: kind})
			switch {
			case serr == nil && esi.SnapshotFresh(snap):
				continue // still inside ESI's cache window
			case serr != nil && !errors.Is(serr, sql.ErrNoRows):
				log.Printf("worker: read %s snapshot for character %d: %v", kind, ch.CharacterID, serr)
			}

			if _, err := app.esi.FetchAndStoreSnapshot(ctx, ch, kind); err != nil {
				failed++
				if errors.Is(err, esi.ErrErrorLimit) {
					log.Printf("worker: ESI error limit hit refreshing %s for character %d; backing off until next cycle", kind, ch.CharacterID)
					limited = true
				} else {
					log.Printf("worker: refresh %s for character %d: %v", kind, ch.CharacterID, err)
				}
				break // don't keep pushing this character this cycle
			}
			refreshed++
		}

		// Killmail details behind the recent list: immutable once
		// posted, so each missing detail is fetched once and kept.
		// Bounded per character per cycle (warmKillmailDetails).
		if !limited {
			warmed, ltd := app.warmKillmailDetails(ctx, ch)
			refreshed += warmed
			if ltd {
				log.Printf("worker: ESI error limit hit warming killmail details for character %d; backing off until next cycle", ch.CharacterID)
				limited = true
			}
		}

		// Corporation datasets: the corp_* snapshots behind the
		// corporation subpages, plus the details behind the corp's
		// recent killmail list (corp_worker.go). 403 role refusals
		// are recorded state there, not failures.
		if !limited {
			refreshed += app.refreshCorpSnapshots(ctx, ch)
			warmed, ltd := app.warmCorpKillmailDetails(ctx, ch)
			refreshed += warmed
			if ltd {
				log.Printf("worker: ESI error limit hit warming corp killmail details for character %d; backing off until next cycle", ch.CharacterID)
				limited = true
			}
		}

		// Economy datasets (cluster 3): the wallet/orders/
		// contracts/industry snapshots, plus the contract item
		// lists behind the contracts snapshot (economy_worker.go).
		if !limited {
			refreshed += app.refreshEconomySnapshots(ctx, ch)
			warmed, ltd := app.warmContractItems(ctx, ch)
			refreshed += warmed
			if ltd {
				log.Printf("worker: ESI error limit hit warming contract items for character %d; backing off until next cycle", ch.CharacterID)
				limited = true
			}
		}
		if limited {
			break
		}
	}

	// Name warm-up: resolve whatever the local caches still lack —
	// type names (persisted in type_names), type→group links, group
	// names, station/system names — from the characters' latest
	// snapshots, so renders resolve from memory/DB only. Bounded
	// per cycle; whatever doesn't fit converges over later cycles.
	namesResolved := 0
	if !limited {
		budget := &warmBudget{left: maxWarmLookupsPerCycle}
		for _, ch := range characters {
			if ctx.Err() != nil {
				break
			}
			namesResolved += app.warmCharacterNames(ctx, ch, budget)
			if budget.errorLimited() {
				log.Printf("worker: ESI error limit hit during name warm-up; resuming next cycle")
				limited = true
				break
			}
			if budget.exhausted() {
				break
			}
		}
	}

	summary := cycleSummary(refreshed, namesResolved, failed, limited)
	app.updateWorkerStatus(func(s *workerStatus) {
		s.Warming = false
		s.Summary = summary
		s.NamesResolvedTotal += namesResolved
	})

	if refreshed > 0 || failed > 0 || namesResolved > 0 {
		log.Printf("worker: cycle done: %s", summary)
	}
}

// cycleSummary builds the one-line status/log summary of a cycle.
func cycleSummary(refreshed, namesResolved, failed int, limited bool) string {
	var parts []string
	if refreshed > 0 {
		parts = append(parts, fmt.Sprintf("refreshed %d snapshot(s)", refreshed))
	}
	if namesResolved > 0 {
		parts = append(parts, fmt.Sprintf("resolved %d name(s)", namesResolved))
	}
	if failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failure(s)", failed))
	}
	if len(parts) == 0 {
		parts = append(parts, "idle")
	}
	if limited {
		parts = append(parts, "ESI error limit — backing off")
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += ", " + p
	}
	return out
}

// orderByPriority stably reorders characters so flagged IDs come
// first (in flag order); unflagged characters keep their order, and
// flags for characters that no longer exist are ignored.
func orderByPriority(characters []db.Character, priority []int64) []db.Character {
	if len(priority) == 0 {
		return characters
	}
	rank := make(map[int64]int, len(priority))
	for i, id := range priority {
		rank[id] = i
	}
	out := append([]db.Character(nil), characters...)
	sort.SliceStable(out, func(i, j int) bool {
		ri, iok := rank[out[i].CharacterID]
		rj, jok := rank[out[j].CharacterID]
		if iok != jok {
			return iok
		}
		return ri < rj
	})
	return out
}

// ---------------------------------------------------------------------------
// Name warm-up. A per-cycle lookup budget keeps a huge account from
// turning one cycle into an ESI marathon; a 420/429 anywhere stops
// the whole pass until the next cycle.
// ---------------------------------------------------------------------------

const (
	// maxWarmLookupsPerCycle caps total ESI lookups across all
	// characters and name kinds in one worker cycle.
	maxWarmLookupsPerCycle = 400
	// warmPoolSize is the warm-up worker-pool width.
	warmPoolSize = 6
)

// warmBudget is the shared lookup allowance for one warm-up pass.
type warmBudget struct {
	mu      sync.Mutex
	left    int
	limited bool // ESI error limit hit: stop everything
}

// take spends one lookup, reporting whether it was available.
func (b *warmBudget) take() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.limited || b.left <= 0 {
		return false
	}
	b.left--
	return true
}

func (b *warmBudget) hitLimit() {
	b.mu.Lock()
	b.limited = true
	b.mu.Unlock()
}

func (b *warmBudget) stopped() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.limited || b.left <= 0
}

func (b *warmBudget) errorLimited() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.limited
}

func (b *warmBudget) exhausted() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.left <= 0
}

// runWarmPool runs work over ids with a small goroutine pool,
// stopping early when the budget runs out or ESI answers with its
// error-limit status. It returns how many ids work resolved.
func (app *Application) runWarmPool(ctx context.Context, ids []int64, budget *warmBudget, work func(context.Context, int64) bool) int {
	if len(ids) == 0 {
		return 0
	}
	jobs := make(chan int64)
	var resolved atomic.Int64
	var wg sync.WaitGroup
	workers := warmPoolSize
	if len(ids) < workers {
		workers = len(ids)
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				if ctx.Err() != nil || budget.stopped() {
					continue // drain without spending lookups
				}
				if !budget.take() {
					continue
				}
				if work(ctx, id) {
					resolved.Add(1)
				}
			}
		}()
	}
feed:
	for _, id := range ids {
		select {
		case jobs <- id:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	return int(resolved.Load())
}

// warmCharacterNames resolves every name the local caches still lack
// for one character, derived from its latest skills + assets
// snapshots: type names, type→group links, group names, and
// station/system names — plus character names for the victims and
// final-blow attackers in its stored killmail details. It returns
// how many entries were resolved.
// (Structure locations are skipped: their names need an auth scope
// this app does not hold, so they render as "Structure #<id>".)
func (app *Application) warmCharacterNames(ctx context.Context, ch db.Character, budget *warmBudget) int {
	snaps, err := app.queries.ListSnapshotsByCharacter(ctx, ch.CharacterID)
	if err != nil {
		log.Printf("worker: warm names for character %d: list snapshots: %v", ch.CharacterID, err)
		return 0
	}

	typeIDs := make(map[int64]bool)
	placeKinds := make(map[int64]string) // location id -> "station"|"solar_system"
	charIDs := make(map[int64]bool)      // character names (killmail people + corp rosters)
	for _, snap := range snaps {
		switch snap.Kind {
		case esi.SnapSkills:
			var skills esi.Skills
			if err := json.Unmarshal([]byte(snap.Payload), &skills); err != nil {
				log.Printf("worker: warm names for character %d: decode skills snapshot: %v", ch.CharacterID, err)
				continue
			}
			for _, s := range skills.Skills {
				typeIDs[s.SkillID] = true
			}
		case esi.SnapAssets:
			var items []esi.Asset
			if err := json.Unmarshal([]byte(snap.Payload), &items); err != nil {
				log.Printf("worker: warm names for character %d: decode assets snapshot: %v", ch.CharacterID, err)
				continue
			}
			for _, it := range items {
				typeIDs[it.TypeID] = true
				if it.LocationType == "station" || it.LocationType == "solar_system" {
					placeKinds[it.LocationID] = it.LocationType
				}
			}
		case esi.SnapCorpMembers:
			// The roster's names resolve through the same cache.
			var members esi.CorpMembers
			if err := json.Unmarshal([]byte(snap.Payload), &members); err != nil {
				log.Printf("worker: warm names for character %d: decode corp members snapshot: %v", ch.CharacterID, err)
				continue
			}
			for _, id := range members {
				if id > 0 {
					charIDs[id] = true
				}
			}
		case esi.SnapCorpMemberTracking:
			var tracking esi.CorpMemberTrackings
			if err := json.Unmarshal([]byte(snap.Payload), &tracking); err != nil {
				log.Printf("worker: warm names for character %d: decode corp membertracking snapshot: %v", ch.CharacterID, err)
				continue
			}
			for _, t := range tracking {
				if t.CharacterID > 0 {
					charIDs[t.CharacterID] = true
				}
				if t.ShipTypeID > 0 {
					typeIDs[t.ShipTypeID] = true
				}
			}
		case esi.SnapCorpAssets:
			// Same payload shape as character assets.
			var items []esi.Asset
			if err := json.Unmarshal([]byte(snap.Payload), &items); err != nil {
				log.Printf("worker: warm names for character %d: decode corp assets snapshot: %v", ch.CharacterID, err)
				continue
			}
			for _, it := range items {
				typeIDs[it.TypeID] = true
				if it.LocationType == "station" || it.LocationType == "solar_system" {
					placeKinds[it.LocationID] = it.LocationType
				}
			}
		case esi.SnapCorpOrders:
			var orders esi.CorpOrders
			if err := json.Unmarshal([]byte(snap.Payload), &orders); err != nil {
				log.Printf("worker: warm names for character %d: decode corp orders snapshot: %v", ch.CharacterID, err)
				continue
			}
			for _, o := range orders {
				typeIDs[o.TypeID] = true
			}
		case esi.SnapCorpStructures:
			var structures esi.CorpStructures
			if err := json.Unmarshal([]byte(snap.Payload), &structures); err != nil {
				log.Printf("worker: warm names for character %d: decode corp structures snapshot: %v", ch.CharacterID, err)
				continue
			}
			for _, s := range structures {
				if s.TypeID > 0 {
					typeIDs[s.TypeID] = true
				}
			}
		case esi.SnapWalletJournal:
			// Journal parties resolve through the character-name
			// cache; only plausible character IDs are harvested
			// (corporations/alliances share the numeric space and
			// would just 404 the character endpoint every cycle).
			var journal esi.WalletJournal
			if err := json.Unmarshal([]byte(snap.Payload), &journal); err == nil {
				for _, e := range journal {
					if e.FirstPartyID >= 90_000_000 {
						charIDs[e.FirstPartyID] = true
					}
					if e.SecondPartyID >= 90_000_000 {
						charIDs[e.SecondPartyID] = true
					}
				}
			}
		case esi.SnapWalletTxns:
			var txns esi.WalletTransactions
			if err := json.Unmarshal([]byte(snap.Payload), &txns); err == nil {
				for _, t := range txns {
					if t.TypeID > 0 {
						typeIDs[t.TypeID] = true
					}
					if t.ClientID >= 90_000_000 {
						charIDs[t.ClientID] = true
					}
				}
			}
		case esi.SnapOrders:
			var orders esi.CharOrders
			if err := json.Unmarshal([]byte(snap.Payload), &orders); err == nil {
				for _, o := range orders {
					if o.TypeID > 0 {
						typeIDs[o.TypeID] = true
					}
				}
			}
		case esi.SnapOrdersHistory:
			var history esi.CharOrderHistory
			if err := json.Unmarshal([]byte(snap.Payload), &history); err == nil {
				for _, o := range history {
					if o.TypeID > 0 {
						typeIDs[o.TypeID] = true
					}
				}
			}
		case esi.SnapContracts:
			var contracts esi.Contracts
			if err := json.Unmarshal([]byte(snap.Payload), &contracts); err == nil {
				for _, c := range contracts {
					for _, id := range []int64{c.IssuerID, c.AssigneeID, c.AcceptorID} {
						if id >= 90_000_000 {
							charIDs[id] = true
						}
					}
				}
			}
		case esi.SnapIndustryJobs:
			var jobs esi.IndustryJobs
			if err := json.Unmarshal([]byte(snap.Payload), &jobs); err == nil {
				for _, j := range jobs {
					if j.BlueprintTypeID > 0 {
						typeIDs[j.BlueprintTypeID] = true
					}
					if j.ProductTypeID > 0 {
						typeIDs[j.ProductTypeID] = true
					}
				}
			}
		case esi.SnapBlueprints:
			var blueprints esi.Blueprints
			if err := json.Unmarshal([]byte(snap.Payload), &blueprints); err == nil {
				for _, bp := range blueprints {
					if bp.TypeID > 0 {
						typeIDs[bp.TypeID] = true
					}
				}
			}
		case esi.SnapMining:
			var ledger esi.MiningLedger
			if err := json.Unmarshal([]byte(snap.Payload), &ledger); err == nil {
				for _, m := range ledger {
					if m.TypeID > 0 {
						typeIDs[m.TypeID] = true
					}
				}
			}
		default:
			// Per-division wallet ledgers: counterparties and
			// journal parties resolve through the same cache.
			if strings.HasPrefix(snap.Kind, esi.SnapCorpTxnsPrefix) {
				var txns esi.CorpWalletTransactions
				if err := json.Unmarshal([]byte(snap.Payload), &txns); err == nil {
					for _, t := range txns {
						if t.ClientID > 0 {
							charIDs[t.ClientID] = true
						}
					}
				}
			}
			if strings.HasPrefix(snap.Kind, esi.SnapCorpJournalPrefix) {
				var journal esi.CorpJournal
				if err := json.Unmarshal([]byte(snap.Payload), &journal); err == nil {
					for _, e := range journal {
						if e.FirstPartyID > 0 {
							charIDs[e.FirstPartyID] = true
						}
						if e.SecondPartyID > 0 {
							charIDs[e.SecondPartyID] = true
						}
					}
				}
			}
		}
	}
	// With no skills/assets yet, the type/group/place passes below
	// simply no-op on empty ID sets; the killmail character-name
	// pass at the end may still have work to do.
	ids := sortedInt64Keys(typeIDs)

	resolved := 0

	// Type names (persisted in type_names).
	haveNames := app.esi.CachedTypeNames(ctx, ids)
	var missing []int64
	for _, id := range ids {
		if _, ok := haveNames[id]; !ok {
			missing = append(missing, id)
		}
	}
	resolved += app.runWarmPool(ctx, missing, budget, func(ctx context.Context, id int64) bool {
		return app.warmTypeName(ctx, budget, id)
	})
	if budget.stopped() {
		return resolved
	}

	// Type → group links (skill-sheet grouping). Fetching a type to
	// learn its group also refreshes its name when one is present.
	haveGroups := app.esi.CachedTypeGroups(ctx, ids)
	var missingGroups []int64
	for _, id := range ids {
		if _, ok := haveGroups[id]; !ok {
			missingGroups = append(missingGroups, id)
		}
	}
	resolved += app.runWarmPool(ctx, missingGroups, budget, func(ctx context.Context, id int64) bool {
		return app.warmTypeGroup(ctx, budget, id)
	})
	if budget.stopped() {
		return resolved
	}

	// Group names for every group those types belong to.
	groupIDs := make(map[int64]bool)
	for _, gid := range app.esi.CachedTypeGroups(ctx, ids) {
		if gid > 0 {
			groupIDs[gid] = true
		}
	}
	gids := sortedInt64Keys(groupIDs)
	haveGroupNames := app.esi.CachedGroupNames(ctx, gids)
	var missingGroupNames []int64
	for _, gid := range gids {
		if _, ok := haveGroupNames[gid]; !ok {
			missingGroupNames = append(missingGroupNames, gid)
		}
	}
	resolved += app.runWarmPool(ctx, missingGroupNames, budget, func(ctx context.Context, id int64) bool {
		return app.warmGroupName(ctx, budget, id)
	})
	if budget.stopped() {
		return resolved
	}

	// Station / solar-system names for asset locations.
	pathByID := make(map[int64]string, len(placeKinds))
	var missingPlaces []int64
	for id, kind := range placeKinds {
		if _, ok := app.esi.CachedPlaceName(ctx, id); ok {
			continue
		}
		dir := "stations"
		if kind == "solar_system" {
			dir = "systems"
		}
		pathByID[id] = fmt.Sprintf("/universe/%s/%d/", dir, id)
		missingPlaces = append(missingPlaces, id)
	}
	sort.Slice(missingPlaces, func(i, j int) bool { return missingPlaces[i] < missingPlaces[j] })
	resolved += app.runWarmPool(ctx, missingPlaces, budget, func(ctx context.Context, id int64) bool {
		return app.warmPlaceName(ctx, budget, pathByID[id], id)
	})
	if budget.stopped() {
		return resolved
	}

	// Character names from this character's stored killmail
	// details: victims and final-blow attackers, so the killmail
	// list can label people instead of raw IDs. (Corp rosters and
	// tracking rows harvested above feed the same set.)
	if rows, err := app.queries.ListKillmailDetailsByCharacter(ctx, ch.CharacterID); err != nil {
		log.Printf("worker: warm names for character %d: list killmail details: %v", ch.CharacterID, err)
	} else {
		for _, row := range rows {
			var km esi.Killmail
			if err := json.Unmarshal([]byte(row.Payload), &km); err != nil {
				continue // undecodable payload: nothing to derive
			}
			if km.Victim.CharacterID > 0 {
				charIDs[km.Victim.CharacterID] = true
			}
			for _, a := range km.Attackers {
				if a.FinalBlow && a.CharacterID > 0 {
					charIDs[a.CharacterID] = true
				}
			}
		}
	}
	var missingChars []int64
	for _, id := range sortedInt64Keys(charIDs) {
		if _, ok := app.esi.CachedCharacterName(id); !ok {
			missingChars = append(missingChars, id)
		}
	}
	resolved += app.runWarmPool(ctx, missingChars, budget, func(ctx context.Context, id int64) bool {
		return app.warmCharacterName(ctx, budget, id)
	})

	return resolved
}

// fetchTypeForWarm GETs one type for the warm-up pass, translating
// an ESI error-limit response into a budget stop.
func (app *Application) fetchTypeForWarm(ctx context.Context, budget *warmBudget, id int64) (esi.Type, bool) {
	var t esi.Type
	if err := app.esi.Get(ctx, "", fmt.Sprintf("/universe/types/%d/", id), &t); err != nil {
		if errors.Is(err, esi.ErrErrorLimit) {
			budget.hitLimit()
		} else if ctx.Err() == nil {
			log.Printf("worker: warm type %d: %v", id, err)
		}
		return esi.Type{}, false
	}
	return t, true
}

func (app *Application) warmTypeName(ctx context.Context, budget *warmBudget, id int64) bool {
	t, ok := app.fetchTypeForWarm(ctx, budget, id)
	if !ok || t.Name == "" {
		return false
	}
	app.esi.StoreTypeName(ctx, id, t)
	return true
}

func (app *Application) warmTypeGroup(ctx context.Context, budget *warmBudget, id int64) bool {
	t, ok := app.fetchTypeForWarm(ctx, budget, id)
	if !ok || t.GroupID <= 0 {
		return false
	}
	app.esi.StoreTypeName(ctx, id, t)
	return true
}

func (app *Application) warmGroupName(ctx context.Context, budget *warmBudget, id int64) bool {
	var g esi.Group
	if err := app.esi.Get(ctx, "", fmt.Sprintf("/universe/groups/%d/", id), &g); err != nil {
		if errors.Is(err, esi.ErrErrorLimit) {
			budget.hitLimit()
		} else if ctx.Err() == nil {
			log.Printf("worker: warm group %d: %v", id, err)
		}
		return false
	}
	if g.Name == "" {
		return false
	}
	app.esi.StoreGroupName(id, g.Name)
	return true
}

func (app *Application) warmPlaceName(ctx context.Context, budget *warmBudget, path string, id int64) bool {
	var place esi.Station
	if err := app.esi.Get(ctx, "", path, &place); err != nil {
		if errors.Is(err, esi.ErrErrorLimit) {
			budget.hitLimit()
		} else if ctx.Err() == nil {
			log.Printf("worker: warm place %s: %v", path, err)
		}
		return false
	}
	if place.Name == "" {
		return false
	}
	app.esi.StorePlaceName(id, place.Name)
	return true
}

func (app *Application) warmCharacterName(ctx context.Context, budget *warmBudget, id int64) bool {
	name, err := app.esi.CharacterName(ctx, id)
	if err != nil {
		if errors.Is(err, esi.ErrErrorLimit) {
			budget.hitLimit()
		} else if ctx.Err() == nil {
			log.Printf("worker: warm character %d: %v", id, err)
		}
		return false
	}
	return name != ""
}

// ---------------------------------------------------------------------------
// Killmail detail warming. The recent-killmails list is a snapshot;
// the detail payloads behind it are immutable, so the worker fills
// the killmail_details store once per killmail (bounded per cycle)
// and pages render from the store only — never from the network.
// ---------------------------------------------------------------------------

// maxKillmailDetailsPerCycle bounds detail fetches per character
// per worker cycle; a busy character's backlog converges over a
// few cycles instead of turning one cycle into an ESI marathon.
const maxKillmailDetailsPerCycle = 10

// warmKillmailDetails fetches detail payloads for the entries of
// the character's recent-killmails snapshot that are not stored
// yet, at most maxKillmailDetailsPerCycle of them. It reports how
// many were stored and whether ESI's error limit stopped the pass.
// The detail endpoint is public (the hash authorizes it), so no
// character token is involved. The corporation pass shares this
// machinery via warmKillmailDetailsFor.
func (app *Application) warmKillmailDetails(ctx context.Context, ch db.Character) (fetched int, limited bool) {
	return app.warmKillmailDetailsFor(ctx, ch, esi.SnapKillmails)
}

// warmKillmailDetailsFor is warmKillmailDetails against an
// arbitrary recent-list snapshot kind (the character's killmails,
// or the corporation's).
func (app *Application) warmKillmailDetailsFor(ctx context.Context, ch db.Character, kind string) (fetched int, limited bool) {
	snap, err := app.queries.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: ch.CharacterID, Kind: kind})
	if err != nil {
		return 0, false // no recent list yet; nothing to warm
	}
	var refs []esi.KillmailRef
	if err := json.Unmarshal([]byte(snap.Payload), &refs); err != nil {
		log.Printf("worker: warm killmail details for character %d: decode recent list: %v", ch.CharacterID, err)
		return 0, false
	}

	for _, ref := range refs {
		if fetched >= maxKillmailDetailsPerCycle {
			break
		}
		if ref.KillmailID <= 0 || ref.KillmailHash == "" {
			continue
		}
		if _, err := app.queries.GetKillmailDetail(ctx, ref.KillmailID); err == nil {
			continue // already stored (by any character's list)
		} else if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("worker: warm killmail details for character %d: read detail %d: %v", ch.CharacterID, ref.KillmailID, err)
			continue
		}

		body, _, err := app.esi.FetchRaw(ctx, "", fmt.Sprintf("/killmails/%d/%s/", ref.KillmailID, ref.KillmailHash))
		if err != nil {
			if errors.Is(err, esi.ErrErrorLimit) {
				return fetched, true
			}
			if ctx.Err() == nil {
				log.Printf("worker: killmail detail %d for character %d: %v", ref.KillmailID, ch.CharacterID, err)
			}
			continue
		}
		if err := app.queries.UpsertKillmailDetail(ctx, db.UpsertKillmailDetailParams{
			KillmailID:  ref.KillmailID,
			CharacterID: ch.CharacterID,
			Hash:        ref.KillmailHash,
			Payload:     string(body),
			FetchedAt:   time.Now().UTC().Format(time.RFC3339),
		}); err != nil {
			log.Printf("worker: store killmail detail %d for character %d: %v", ref.KillmailID, ch.CharacterID, err)
			continue
		}
		fetched++
	}
	return fetched, false
}

func sortedInt64Keys(set map[int64]bool) []int64 {
	keys := make([]int64, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}
