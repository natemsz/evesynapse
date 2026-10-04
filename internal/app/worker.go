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

	// The urgent want drain polls the want queues every few
	// seconds so a click that outruns proactive coverage fills in
	// within seconds instead of waiting for the minute cycle.
	go app.runUrgentDrain(ctx)

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

	// Parked characters (token_dead, owner_changed) are never
	// synced until the user signs them in again; they are counted
	// for the status line but cost no fetches.
	var eligible []db.Character
	parked := 0
	for _, ch := range characters {
		if characterSyncs(ch) {
			eligible = append(eligible, ch)
		} else {
			parked++
		}
	}

	// Due order: characters flagged since the last cycle (fresh
	// logins, Sync page requests) first, then most-overdue first
	// (earliest cached_until across the core kinds; a kind with no
	// snapshot at all counts as due immediately). With dozens of
	// linked characters the stalest work always goes first.
	eligible = app.orderByDue(ctx, eligible)
	eligible = orderByPriority(eligible, app.takePriorityCharacters())

	var refreshed, failed, deferred int
	limited := false
	allowance := &fetchBudget{left: maxFetchesPerCycle}

	// The market guide (v0.3.04): one public call mirrors into
	// the stored table on ESI's cache window, ahead of the
	// character pass so asset valuation — the net-worth card
	// and the daily sampler below — always has prices to work
	// with. Fresh tables cost nothing here.
	if stored, gLimited := app.refreshGuidePrices(ctx); stored {
		refreshed++
	} else if gLimited {
		log.Printf("worker: ESI error limit hit refreshing guide prices; backing off until next cycle")
		limited = true
	}

	for i, ch := range eligible {
		if ctx.Err() != nil {
			app.updateWorkerStatus(func(s *workerStatus) { s.Warming = false })
			return
		}
		if allowance.exhausted() {
			// The cycle's work budget is spent; the rest keep
			// their place in the due order for the next cycle
			// instead of one giant pass over every character.
			deferred = len(eligible) - i
			break
		}

		// Ensure the token is usable before touching snapshots; a
		// revoked refresh token means this character needs a fresh
		// login, and fetching would only fail three more times.
		// (A definitive rejection parks the character inside
		// validAccessToken — see links.go.)
		if _, err := app.validAccessToken(ctx, ch); err != nil {
			log.Printf("worker: token for character %d unusable: %v", ch.CharacterID, err)
			failed++
			continue
		}

		for _, kind := range coreSnapshotKinds {
			snap, serr := app.queries.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: ch.CharacterID, Kind: kind})
			switch {
			case serr == nil && esi.SnapshotFresh(snap):
				continue // still inside ESI's cache window
			case serr != nil && !errors.Is(serr, sql.ErrNoRows):
				log.Printf("worker: read %s snapshot for character %d: %v", kind, ch.CharacterID, serr)
			}

			if !allowance.take() {
				break
			}
			if _, err := app.esi.FetchAndStoreSnapshot(ctx, ch, kind); err != nil {
				failed++
				if errors.Is(err, esi.ErrErrorLimit) {
					log.Printf("worker: ESI error limit hit refreshing %s for character %d; backing off until next cycle", kind, ch.CharacterID)
					limited = true
				} else {
					if isDefinitiveTokenFailure(err) {
						// The access token itself was rejected:
						// park the character rather than failing
						// the same way every cycle.
						app.markCharacterTokenDead(ctx, ch.CharacterID)
						log.Printf("worker: character %d token rejected refreshing %s; parked until re-login", ch.CharacterID, kind)
					} else {
						log.Printf("worker: refresh %s for character %d: %v", kind, ch.CharacterID, err)
					}
				}
				break // don't keep pushing this character this cycle
			}
			refreshed++
		}

		// Killmail details behind the recent list: immutable once
		// posted, so each missing detail is fetched once and kept.
		// Bounded per character per cycle (warmKillmailDetails).
		// The sub-passes below keep their own per-character caps
		// and freshness gates; the cycle budget only stops new
		// characters from starting once it is spent.
		if !limited && !allowance.exhausted() {
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
		if !limited && !allowance.exhausted() {
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
		if !limited && !allowance.exhausted() {
			refreshed += app.refreshEconomySnapshots(ctx, ch)
			warmed, ltd := app.warmContractItems(ctx, ch)
			refreshed += warmed
			if ltd {
				log.Printf("worker: ESI error limit hit warming contract items for character %d; backing off until next cycle", ch.CharacterID)
				limited = true
			}
		}

		// Phase 2 datasets: planetary industry (colonies +
		// layouts, planets_worker.go) and mail/calendar/contacts
		// (list kinds + bodies + event details, comms_worker.go).
		// Both passes spend from the cycle's shared fetch
		// allowance, so they compose with the core pass's budget
		// instead of adding an unbounded tail.
		if !limited && !allowance.exhausted() {
			warmed, ltd := app.refreshPlanetarySnapshots(ctx, ch, allowance)
			refreshed += warmed
			if ltd {
				log.Printf("worker: ESI error limit hit refreshing planetary industry for character %d; backing off until next cycle", ch.CharacterID)
				limited = true
			}
		}
		if !limited && !allowance.exhausted() {
			warmed, ltd := app.refreshCommsSnapshots(ctx, ch, allowance)
			refreshed += warmed
			if ltd {
				log.Printf("worker: ESI error limit hit refreshing mail/calendar/contacts for character %d; backing off until next cycle", ch.CharacterID)
				limited = true
			}
		}

		// Daily wallet history (schema 019): record today from
		// the snapshots just stored. Pure local reads — no fetch
		// budget spent, no extra ESI calls.
		app.sampleWalletHistory(ctx, ch, time.Now())

		if limited {
			break
		}
	}

	// Market pass (Phase 5): price-history warming for
	// watchlists/wants/order types, and per-order health from
	// regional books. Public data, but it spends from the same
	// cycle allowance; with the budget gone it only prunes.
	if !limited {
		mStored, mLimited := app.refreshMarketData(ctx, characters, allowance)
		refreshed += mStored
		if mLimited {
			log.Printf("worker: ESI error limit hit refreshing market data; backing off until next cycle")
			limited = true
		}
	}

	// Structure names: resolve the due slice of the structure
	// queue across every scoped character (structures.go).
	if !limited {
		sResolved, sLimited := app.resolveStructureNames(ctx, characters, allowance)
		refreshed += sResolved
		if sLimited {
			log.Printf("worker: ESI error limit hit resolving structure names; backing off until next cycle")
			limited = true
		}
	}

	// Planet names: resolve the due slice of the planet queue
	// (public endpoint, no token — planet_names.go).
	if !limited {
		plResolved, plLimited := app.resolvePlanetNames(ctx, allowance)
		refreshed += plResolved
		if plLimited {
			log.Printf("worker: ESI error limit hit resolving planet names; backing off until next cycle")
			limited = true
		}
	}

	// Public records: resolve any pilot names the topbar search
	// is waiting on, note the counterparty orbit (everyone the
	// deployment's data mentions) ahead of the pilot drain, then
	// fill the pilot queue (strangers viewed on /pilot/) and the
	// item-description wants the item details page notes. Public
	// endpoints, same cycle allowance.
	if !limited {
		nResolved, nLimited := app.refreshPilotNameWants(ctx, allowance)
		refreshed += nResolved
		if nLimited {
			log.Printf("worker: ESI error limit hit resolving pilot names; backing off until next cycle")
			limited = true
		}
	}
	app.notePilotOrbit(ctx)
	if !limited {
		pDrained, pLimited := app.refreshPilotRecords(ctx, allowance)
		refreshed += pDrained
		if pLimited {
			log.Printf("worker: ESI error limit hit draining pilot records; backing off until next cycle")
			limited = true
		}
	}
	// Public organization records (v0.3.12): corporations and
	// alliances someone followed a name to. Public endpoints,
	// same cycle allowance.
	if !limited {
		cDrained, cLimited := app.refreshCorporationRecords(ctx, allowance)
		refreshed += cDrained
		if cLimited {
			log.Printf("worker: ESI error limit hit draining corporation records; backing off until next cycle")
			limited = true
		}
	}
	if !limited {
		aDrained, aLimited := app.refreshAllianceRecords(ctx, allowance)
		refreshed += aDrained
		if aLimited {
			log.Printf("worker: ESI error limit hit draining alliance records; backing off until next cycle")
			limited = true
		}
	}
	if !limited {
		tDrained, tLimited := app.refreshTypeDetails(ctx, allowance)
		refreshed += tDrained
		if tLimited {
			log.Printf("worker: ESI error limit hit draining type details; backing off until next cycle")
			limited = true
		}
	}

	// Name warm-up: resolve whatever the local caches still lack —
	// type names (persisted in type_names), type→group links, group
	// names, station/system names — from the characters' latest
	// snapshots, so renders resolve from memory/DB only. Bounded
	// per cycle; whatever doesn't fit converges over later cycles.
	namesResolved := 0
	budget := &warmBudget{left: maxWarmLookupsPerCycle}
	if !limited {
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
			if allowance.exhausted() {
				break
			}
		}
	}

	// Intel (public data): global snapshots, war details and the
	// public name caches, spending whatever of the cycle's lookup
	// budget the character pass left. Runs with zero characters
	// linked too — none of it needs a token.
	if !limited {
		iStored, iNames, iLimited := app.refreshIntel(ctx, budget)
		refreshed += iStored
		namesResolved += iNames
		if iLimited {
			log.Printf("worker: ESI error limit hit refreshing intel; backing off until next cycle")
			limited = true
		}
	}

	summary := cycleSummary(refreshed, namesResolved, failed, limited, parked, deferred)
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
func cycleSummary(refreshed, namesResolved, failed int, limited bool, parked, deferred int) string {
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
	if parked > 0 {
		parts = append(parts, fmt.Sprintf("%d awaiting re-login", parked))
	}
	if deferred > 0 {
		parts = append(parts, fmt.Sprintf("%d deferred (cycle budget)", deferred))
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

// coreSnapshotKinds are the per-character snapshot kinds the main
// worker pass keeps warm (the cluster 1 set).
var coreSnapshotKinds = []string{
	esi.SnapProfile, esi.SnapSkills, esi.SnapSkillqueue, esi.SnapAttributes, esi.SnapWallet, esi.SnapAssets,
	esi.SnapLocation, esi.SnapShip, esi.SnapOnline, esi.SnapClones,
	esi.SnapImplants, esi.SnapFittings, esi.SnapFatigue, esi.SnapKillmails,
}

// maxFetchesPerCycle bounds snapshot fetches in the main character
// pass of one worker cycle. With dozens of linked characters the
// stalest work goes first (due order) and the rest waits for the
// next one-minute cycle instead of one giant pass; the killmail /
// corp / economy sub-passes keep their own per-character caps.
const maxFetchesPerCycle = 120

// fetchBudget is the main pass's fetch allowance for one cycle.
// Sequential use only (the character pass is single-goroutine).
type fetchBudget struct{ left int }

// take spends one fetch, reporting whether it was available.
func (b *fetchBudget) take() bool {
	if b.left <= 0 {
		return false
	}
	b.left--
	return true
}

func (b *fetchBudget) exhausted() bool { return b.left <= 0 }

// orderByDue stably orders characters most-overdue first: a
// character's due key is the earliest cached_until among the core
// snapshot kinds (a kind with no snapshot yet is due immediately,
// key zero). The sort is stable, so callers can layer the
// priority-flag ordering on top.
func (app *Application) orderByDue(ctx context.Context, characters []db.Character) []db.Character {
	due := make(map[int64]time.Time, len(characters))
	for _, ch := range characters {
		due[ch.CharacterID] = app.characterDueKey(ctx, ch)
	}
	out := append([]db.Character(nil), characters...)
	sort.SliceStable(out, func(i, j int) bool {
		return due[out[i].CharacterID].Before(due[out[j].CharacterID])
	})
	return out
}

// characterDueKey computes a character's most-overdue moment: the
// earliest cached_until across every stored snapshot, pulled to
// the zero time when any core kind has never been fetched.
func (app *Application) characterDueKey(ctx context.Context, ch db.Character) time.Time {
	snaps, err := app.queries.ListSnapshotsByCharacter(ctx, ch.CharacterID)
	if err != nil {
		return time.Time{} // unreadable state: treat as due now
	}
	seen := make(map[string]bool, len(snaps))
	var earliest time.Time
	for _, snap := range snaps {
		seen[snap.Kind] = true
		if !snap.CachedUntil.Valid || snap.CachedUntil.String == "" {
			continue
		}
		if until, err := time.Parse(time.RFC3339, snap.CachedUntil.String); err == nil {
			if earliest.IsZero() || until.Before(earliest) {
				earliest = until
			}
		}
	}
	for _, kind := range coreSnapshotKinds {
		if !seen[kind] {
			return time.Time{} // never fetched: most due
		}
	}
	return earliest
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
// (Structure ids met in snapshots are noted for the background
// structure-name queue — see structures.go — never fetched here;
// pages render "Structure #<id>" until the queue lands a name.)
func (app *Application) warmCharacterNames(ctx context.Context, ch db.Character, budget *warmBudget) int {
	snaps, err := app.queries.ListSnapshotsByCharacter(ctx, ch.CharacterID)
	if err != nil {
		log.Printf("worker: warm names for character %d: list snapshots: %v", ch.CharacterID, err)
		return 0
	}

	typeIDs := make(map[int64]bool)
	placeKinds := make(map[int64]string) // location id -> "station"|"solar_system"
	charIDs := make(map[int64]bool)      // character names (killmail people + corp rosters)
	planetIDs := make(map[int64]bool)    // planet names (colony planets)
	schematicIDs := make(map[int64]bool) // PI schematic names + cycle times
	structureIDs := make(map[int64]bool) // structure ids for the name queue
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
				if it.LocationType == "structure" {
					structureIDs[it.LocationID] = true
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
				if it.LocationType == "structure" {
					structureIDs[it.LocationID] = true
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
			// Owner/system/type facts ride along into the
			// structure_context store behind the structure page.
			app.persistStructureContexts(ctx, structures)
			for _, s := range structures {
				if s.TypeID > 0 {
					typeIDs[s.TypeID] = true
				}
				// The corp's own structures arrive already named:
				// seed the structure-name cache for free (tier 2,
				// provenance 'corp' — ESI truth, below only the
				// per-structure lookup itself).
				if s.Name != "" {
					if _, ok := app.esi.CachedStructureName(ctx, s.StructureID); !ok {
						if app.storeStructureName(ctx, s.StructureID, s.Name,
							esi.StructureResolved, esi.StructureSourceCorp, time.Now().UTC().Format(time.RFC3339)) {
							app.esi.StoreStructureName(s.StructureID, s.Name)
						}
					}
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
					if isStructureID(o.LocationID) {
						structureIDs[o.LocationID] = true
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
					if isStructureID(j.FacilityID) {
						structureIDs[j.FacilityID] = true
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
		case esi.SnapPlanets:
			// Colony planets resolve through the place-name
			// cache (their names come from /universe/planets/);
			// their systems ride the station/system pass.
			var colonies esi.Colonies
			if err := json.Unmarshal([]byte(snap.Payload), &colonies); err == nil {
				for _, c := range colonies {
					if c.PlanetID > 0 {
						planetIDs[c.PlanetID] = true
					}
					if c.SolarSystemID > 0 {
						placeKinds[c.SolarSystemID] = "solar_system"
					}
				}
			}
		case esi.SnapMail:
			// Senders and character recipients resolve through
			// the character-name cache (same >= 90M harvest rule
			// as the ledger payloads).
			var headers esi.MailHeaders
			if err := json.Unmarshal([]byte(snap.Payload), &headers); err == nil {
				for _, h := range headers {
					if h.From >= 90_000_000 {
						charIDs[h.From] = true
					}
					for _, rcpt := range h.Recipients {
						if rcpt.RecipientType == "character" && rcpt.RecipientID >= 90_000_000 {
							charIDs[rcpt.RecipientID] = true
						}
					}
				}
			}
		case esi.SnapContacts:
			// Contact kind is explicit in the payload, so the
			// >= 90M harvest rule does not apply: pre-90M
			// character contacts (the oldest pilots) warm their
			// names here too.
			var contacts esi.Contacts
			if err := json.Unmarshal([]byte(snap.Payload), &contacts); err == nil {
				for _, c := range contacts {
					if c.ContactType == "character" && c.ContactID > 0 {
						charIDs[c.ContactID] = true
					}
				}
			}
		default:
			// Per-division wallet ledgers: counterparties and
			// journal parties resolve through the same cache. The
			// >= 90M harvest threshold matches the character-side
			// ledgers (corporation/alliance IDs below it would
			// just fail the character endpoint); IDs above it
			// that still aren't characters are remembered by the
			// client's negative cache after one definitive answer.
			if strings.HasPrefix(snap.Kind, esi.SnapCorpTxnsPrefix) {
				var txns esi.CorpWalletTransactions
				if err := json.Unmarshal([]byte(snap.Payload), &txns); err == nil {
					for _, t := range txns {
						if t.ClientID >= 90_000_000 {
							charIDs[t.ClientID] = true
						}
					}
				}
			}
			if strings.HasPrefix(snap.Kind, esi.SnapCorpJournalPrefix) {
				var journal esi.CorpJournal
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
			}
			// Colony layouts (suffix-keyed snapshots): pin and
			// product types resolve through the type caches,
			// factory schematics through the schematic cache.
			if strings.HasPrefix(snap.Kind, esi.SnapPlanetLayoutPrefix) {
				var layout esi.PlanetLayout
				if err := json.Unmarshal([]byte(snap.Payload), &layout); err == nil {
					for _, pin := range layout.Pins {
						if pin.TypeID > 0 {
							typeIDs[pin.TypeID] = true
						}
						if pin.ExtractorDetails != nil && pin.ExtractorDetails.ProductTypeID > 0 {
							typeIDs[pin.ExtractorDetails.ProductTypeID] = true
						}
						if pin.FactoryDetails != nil && pin.FactoryDetails.SchematicID > 0 {
							schematicIDs[pin.FactoryDetails.SchematicID] = true
						}
						if pin.SchematicID > 0 {
							schematicIDs[pin.SchematicID] = true
						}
					}
				}
			}
			// Calendar event details: a character owner resolves
			// through the character-name cache (attendees resolve
			// the same way from their own snapshots below — the
			// attendee list payloads carry character ids only).
			if strings.HasPrefix(snap.Kind, esi.SnapCalendarAttPrefix) {
				var attendees esi.CalendarAttendees
				if err := json.Unmarshal([]byte(snap.Payload), &attendees); err == nil {
					for _, a := range attendees {
						if a.CharacterID >= 90_000_000 {
							charIDs[a.CharacterID] = true
						}
					}
				}
			}
		}
	}
	// Structure ids met in this character's snapshots join the
	// background resolution queue (structures.go). A plain queue
	// note, no fetches — resolution runs once per cycle below.
	if len(structureIDs) > 0 {
		ids := make([]int64, 0, len(structureIDs))
		for id := range structureIDs {
			ids = append(ids, id)
		}
		app.noteStructureIDs(ctx, ids...)
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

	// Planet names for the character's colonies (the place cache
	// carries them; the network tier is /universe/planets/).
	var missingPlanets []int64
	for _, id := range sortedInt64Keys(planetIDs) {
		if _, ok := app.esi.CachedPlaceName(ctx, id); ok {
			continue
		}
		missingPlanets = append(missingPlanets, id)
	}
	resolved += app.runWarmPool(ctx, missingPlanets, budget, func(ctx context.Context, id int64) bool {
		return app.warmPlanetName(ctx, budget, id)
	})
	if budget.stopped() {
		return resolved
	}

	// PI schematics for the colony layouts' factory pins.
	var missingSchematics []int64
	for _, id := range sortedInt64Keys(schematicIDs) {
		if _, ok := app.esi.CachedSchematic(id); ok {
			continue
		}
		missingSchematics = append(missingSchematics, id)
	}
	resolved += app.runWarmPool(ctx, missingSchematics, budget, func(ctx context.Context, id int64) bool {
		return app.warmSchematic(ctx, budget, id)
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
		if _, ok := app.esi.CachedCharacterName(id); ok {
			continue
		}
		if app.esi.CharacterNameMissed(id) {
			continue // ESI already said this ID is not a character
		}
		missingChars = append(missingChars, id)
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
	if app.esi.CharacterNameMissed(id) {
		return false // not a character; ESI answered definitively once already
	}
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

// warmPlanetName resolves one planet's name from the public
// /universe/planets/ endpoint into the place-name cache, and
// persists it in planet_names (schema 025) so the name survives
// restarts instead of re-warming every cold start.
func (app *Application) warmPlanetName(ctx context.Context, budget *warmBudget, id int64) bool {
	var planet esi.Station // the payload's name field is all we need
	if err := app.esi.Get(ctx, "", fmt.Sprintf("/universe/planets/%d/", id), &planet); err != nil {
		if errors.Is(err, esi.ErrErrorLimit) {
			budget.hitLimit()
		} else if ctx.Err() == nil {
			log.Printf("worker: warm planet %d: %v", id, err)
		}
		return false
	}
	if planet.Name == "" {
		return false
	}
	app.esi.StorePlaceName(id, planet.Name)
	if err := app.queries.SetPlanetName(ctx, db.SetPlanetNameParams{
		PlanetID: id, Name: planet.Name, State: esi.PlanetResolved,
		ResolvedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		log.Printf("worker: persist planet name %d: %v", id, err)
	}
	return true
}

// warmSchematic resolves one PI schematic (name + cycle time)
// into the client's schematic cache.
func (app *Application) warmSchematic(ctx context.Context, budget *warmBudget, id int64) bool {
	s, err := app.esi.FetchSchematic(ctx, id)
	if err != nil {
		if errors.Is(err, esi.ErrErrorLimit) {
			budget.hitLimit()
		} else if ctx.Err() == nil {
			log.Printf("worker: warm schematic %d: %v", id, err)
		}
		return false
	}
	return s.SchematicName != ""
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
