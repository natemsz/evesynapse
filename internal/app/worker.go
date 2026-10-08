package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Panic containment. The HTTP recoverer only covers request
// handlers; a panic on any worker goroutine — a payload shape ESI
// never sent before, a nil the code didn't expect — would end the
// whole process and take the site down with it. Every worker
// goroutine and pass therefore runs under one of the guards
// below: the panic is logged with its stack and the work is
// simply retried on its next tick. Worker code releases its locks
// and transactions with defer, so a recovered pass leaves nothing
// held.
// ---------------------------------------------------------------------------

// recoverWorkerPanic is the deferred guard itself; what names the
// work for the log line. It must be deferred directly (recover
// only sees a panic from the deferred function's own frame).
func recoverWorkerPanic(what string) {
	if r := recover(); r != nil {
		logging.Errorf("worker: PANIC in %s (recovered; it retries on its next tick): %v\n%s", what, r, debug.Stack())
	}
}

// runGuarded runs one worker pass under recoverWorkerPanic.
func runGuarded(what string, pass func()) {
	defer recoverWorkerPanic(what)
	pass()
}

// guardedCycle runs one minute-cycle pass under the guard and,
// when the pass panicked, clears the "warming" flag it left set so
// the Sync and Admin pages say what happened instead of showing a
// cycle that never ends.
func (app *Application) guardedCycle(ctx context.Context, cycle func(context.Context)) {
	defer func() {
		if r := recover(); r != nil {
			logging.Errorf("worker: PANIC in refresh cycle (recovered; the next cycle retries): %v\n%s", r, debug.Stack())
			app.updateWorkerStatus(func(s *workerStatus) {
				s.Warming = false
				s.Summary = "cycle stopped by an internal error — see the server log"
			})
		}
	}()
	cycle(ctx)
}

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
	logging.Infof("worker: started")

	// The two loops below run beside the minute cycle. runWorker
	// waits for them on the way out, so when it returns the whole
	// worker has stopped and Close may close the database handles
	// without any pass still querying them.
	var children sync.WaitGroup
	children.Add(2)

	// SDE maintenance runs alongside the ESI cycle: a first import
	// when the static-data tables are empty, then a weekly update
	// check (patch-day cadence) — see sdeMaintenance in sde.go. It
	// only ever starts background operations, so it never delays
	// snapshot refreshes.
	go func() {
		defer children.Done()
		runGuarded("SDE maintenance", func() { app.sdeMaintenance(ctx) })
	}()

	// The urgent want drain polls the want queues every few
	// seconds so a click that outruns proactive coverage fills in
	// within seconds instead of waiting for the minute cycle.
	go func() {
		defer children.Done()
		app.runUrgentDrain(ctx)
	}()
	defer func() {
		children.Wait()
		logging.Infof("worker: stopped")
	}()

	cycle := time.NewTicker(time.Minute)
	heartbeat := time.NewTicker(10 * time.Minute)
	sdeTick := time.NewTicker(time.Hour)
	defer cycle.Stop()
	defer heartbeat.Stop()
	defer sdeTick.Stop()

	// First pass right away so a cold start doesn't wait a minute for
	// fresh data.
	app.guardedCycle(ctx, app.refreshCycle)

	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			logging.Infof("worker: alive")
		case <-sdeTick.C:
			runGuarded("SDE maintenance", func() { app.sdeMaintenance(ctx) })
		case <-cycle.C:
			app.guardedCycle(ctx, app.refreshCycle)
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
		logging.Errorf("worker: list characters: %v", err)
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

	c := &cycleState{app: app, allowance: &fetchBudget{left: maxFetchesPerCycle}}

	// The market guide (v0.3.04): one public call mirrors into
	// the stored table on ESI's cache window, ahead of the
	// character pass so asset valuation — the net-worth card
	// and the daily sampler below — always has prices to work
	// with. Fresh tables cost nothing here.
	if stored, gLimited := app.refreshGuidePrices(ctx); stored {
		c.refreshed++
	} else if gLimited {
		logging.Warnf("worker: ESI error limit hit refreshing guide prices; backing off until next cycle")
		c.limited = true
	}

	for i, ch := range eligible {
		if ctx.Err() != nil {
			app.updateWorkerStatus(func(s *workerStatus) { s.Warming = false })
			return
		}
		if c.allowance.exhausted() {
			// The cycle's work budget is spent; the rest keep
			// their place in the due order for the next cycle
			// instead of one giant pass over every character.
			c.deferred = len(eligible) - i
			break
		}
		if !c.refreshCharacter(ctx, ch) {
			continue // its token is unusable: on to the next one
		}
		if c.limited {
			break
		}
	}

	c.refreshPublicData(ctx, characters)
	c.warmNames(ctx, characters)

	// Last, with everything this cycle fetched already stored: turn
	// what is newly true into notifications (notify.go). Local data
	// only, so it runs whether or not ESI was reachable.
	runGuarded("notifications", func() {
		if n := app.notifyPass(ctx, characters, time.Now()); n > 0 {
			logging.Infof("worker: %d new notification(s)", n)
		}
	})

	summary := cycleSummary(c.refreshed, c.namesResolved, c.failed, c.limited, parked, c.deferred)
	app.updateWorkerStatus(func(s *workerStatus) {
		s.Warming = false
		s.Summary = summary
		s.NamesResolvedTotal += c.namesResolved
	})

	if c.refreshed > 0 || c.failed > 0 || c.namesResolved > 0 {
		logging.Infof("worker: cycle done: %s", summary)
	}
}

// cycleState is one refresh cycle while it runs: what it has done so far
// and what it may still spend. Its methods are the cycle's passes.
type cycleState struct {
	app *Application

	refreshed     int // datasets fetched and stored
	namesResolved int // names added to the local caches
	failed        int // fetches that failed, and characters with no usable token
	deferred      int // characters left for the next cycle

	// limited is set once ESI says to back off (its error limit).
	// Nothing more is fetched in this cycle after that.
	limited bool
	// allowance is the cycle's shared fetch budget.
	allowance *fetchBudget
}

// pass runs one piece of the cycle, unless ESI has already said to
// back off. What it stored is counted, and if it is where the
// back-off came from that is logged (what names the pass in the log
// line) and the cycle stops fetching.
func (c *cycleState) pass(what string, run func() (stored int, limited bool)) {
	if c.limited {
		return
	}
	stored, limited := run()
	c.refreshed += stored
	if limited {
		logging.Warnf("worker: ESI error limit hit %s; backing off until next cycle", what)
		c.limited = true
	}
}

// characterPass is pass for one character's further datasets. Those
// keep their own per-character caps and freshness gates; on top of
// that they hold back once the cycle's fetch allowance is spent, so
// a spent budget stops new work rather than cutting a pass short.
func (c *cycleState) characterPass(what string, ch db.Character, run func() (stored int, limited bool)) {
	if c.allowance.exhausted() {
		return
	}
	c.pass(fmt.Sprintf("%s for character %d", what, ch.CharacterID), run)
}

// refreshCharacter brings one character up to date: the core
// snapshots first, then the datasets behind the other pages. It
// reports false when the character's token is unusable and nothing
// was attempted.
func (c *cycleState) refreshCharacter(ctx context.Context, ch db.Character) bool {
	app := c.app

	// Ensure the token is usable before touching snapshots; a
	// revoked refresh token means this character needs a fresh
	// login, and fetching would only fail three more times.
	// (A definitive rejection parks the character inside
	// validAccessToken — see links.go.)
	if _, err := app.validAccessToken(ctx, ch); err != nil {
		logging.Warnf("worker: token for character %d unusable: %v", ch.CharacterID, err)
		c.failed++
		return false
	}

	c.refreshCoreSnapshots(ctx, ch)

	// Killmail details behind the recent list: immutable once
	// posted, so each missing detail is fetched once and kept.
	// Bounded per character per cycle (warmKillmailDetails).
	c.characterPass("warming killmail details", ch, func() (int, bool) {
		return app.warmKillmailDetails(ctx, ch)
	})

	// Corporation datasets: the corp_* snapshots behind the
	// corporation subpages, plus the details behind the corp's
	// recent killmail list (corp_worker.go). 403 role refusals
	// are recorded state there, not failures.
	c.characterPass("warming corp killmail details", ch, func() (int, bool) {
		stored := app.refreshCorpSnapshots(ctx, ch)
		warmed, limited := app.warmCorpKillmailDetails(ctx, ch)
		return stored + warmed, limited
	})

	// Economy datasets (cluster 3): the wallet/orders/
	// contracts/industry snapshots, plus the contract item
	// lists behind the contracts snapshot (economy_worker.go).
	c.characterPass("warming contract items", ch, func() (int, bool) {
		stored := app.refreshEconomySnapshots(ctx, ch)
		warmed, limited := app.warmContractItems(ctx, ch)
		return stored + warmed, limited
	})

	// Phase 2 datasets: planetary industry (colonies +
	// layouts, planets_worker.go) and mail/calendar/contacts
	// (list kinds + bodies + event details, comms_worker.go).
	// Both passes spend from the cycle's shared fetch
	// allowance, so they compose with the core pass's budget
	// instead of adding an unbounded tail.
	c.characterPass("refreshing planetary industry", ch, func() (int, bool) {
		return app.refreshPlanetarySnapshots(ctx, ch, c.allowance)
	})
	c.characterPass("refreshing mail/calendar/contacts", ch, func() (int, bool) {
		return app.refreshCommsSnapshots(ctx, ch, c.allowance)
	})

	// The names players gave their ships and containers, for the
	// Assets page (assets_names_worker.go). At most one call per
	// character per cycle, and usually none.
	c.characterPass("warming asset names", ch, func() (int, bool) {
		return app.warmCharacterAssetNames(ctx, ch)
	})

	// Daily wallet history (schema 019): record today from
	// the snapshots just stored. Pure local reads — no fetch
	// budget spent, no extra ESI calls.
	app.sampleWalletHistory(ctx, ch, time.Now())
	return true
}

// refreshCoreSnapshots fetches the character's core datasets whose
// ESI cache window has closed, stopping at the first failure.
func (c *cycleState) refreshCoreSnapshots(ctx context.Context, ch db.Character) {
	app := c.app
	// One meta read (no payloads) for the whole freshness pass:
	// the payloads are the bulk of the table, and freshness only
	// needs cached_until.
	meta, merr := app.queries.ListSnapshotMetaByCharacter(ctx, ch.CharacterID)
	if merr != nil {
		logging.Errorf("worker: read snapshot freshness for character %d: %v", ch.CharacterID, merr)
	}
	fresh := make(map[string]bool, len(meta))
	for _, snap := range meta {
		fresh[snap.Kind] = esi.CacheWindowOpen(snap.CachedUntil)
	}
	granted := scopeSet(ch.Scopes)
	for _, kind := range coreSnapshotKinds {
		if fresh[kind] {
			continue // still inside ESI's cache window
		}
		// A character that granted none of the owning module's scopes
		// can only be refused; skipping spares the fetch and keeps
		// one locked kind from ending the pass for the rest. A row with no
		// recorded scopes is unknown, not locked, and fetches as before.
		if len(granted) > 0 && kindLockedOut(granted, kind) {
			continue
		}

		if !c.allowance.take() {
			break
		}
		if err := app.esi.FetchAndStoreSnapshot(ctx, ch, kind); err != nil {
			c.failed++
			if errors.Is(err, esi.ErrErrorLimit) {
				logging.Warnf("worker: ESI error limit hit refreshing %s for character %d; backing off until next cycle", kind, ch.CharacterID)
				c.limited = true
			} else {
				if isDefinitiveTokenFailure(err) {
					// The access token itself was rejected:
					// park the character rather than failing
					// the same way every cycle.
					app.markCharacterTokenDead(ctx, ch.CharacterID)
					logging.Warnf("worker: character %d token rejected refreshing %s; parked until re-login", ch.CharacterID, kind)
				} else {
					logging.Errorf("worker: refresh %s for character %d: %v", kind, ch.CharacterID, err)
				}
			}
			break // don't keep pushing this character this cycle
		}
		c.refreshed++
	}
}

// refreshPublicData runs the passes that are not about one
// character: market data, the name queues, and the public records
// pages have asked for.
func (c *cycleState) refreshPublicData(ctx context.Context, characters []db.Character) {
	app := c.app

	// Market pass (Phase 5): price-history warming for
	// watchlists/wants/order types, and per-order health from
	// regional books. Public data, spending from its own lane
	// (refreshMarketData owns the market allowance).
	c.pass("refreshing market data", func() (int, bool) {
		return app.refreshMarketData(ctx, characters)
	})

	// Structure names: resolve the due slice of the structure
	// queue across every scoped character (structures.go).
	c.pass("resolving structure names", func() (int, bool) {
		return app.resolveStructureNames(ctx, characters, c.allowance)
	})

	// Planet names: resolve the due slice of the planet queue
	// (public endpoint, no token — planet_names.go).
	c.pass("resolving planet names", func() (int, bool) {
		return app.resolvePlanetNames(ctx, c.allowance)
	})

	// Public records: resolve any pilot names the topbar search
	// is waiting on, note the counterparty orbit (everyone the
	// deployment's data mentions) ahead of the pilot drain, then
	// fill the pilot queue (strangers viewed on /pilot/) and the
	// item-description wants the item details page notes. Public
	// endpoints, same cycle allowance.
	c.pass("resolving pilot names", func() (int, bool) {
		return app.refreshPilotNameWants(ctx, c.allowance)
	})
	app.notePilotOrbit(ctx)
	c.pass("draining pilot records", func() (int, bool) {
		return app.refreshPilotRecords(ctx, c.allowance)
	})
	// Public organization records (v0.3.12): corporations and
	// alliances someone followed a name to. Public endpoints,
	// same cycle allowance.
	c.pass("draining corporation records", func() (int, bool) {
		return app.refreshCorporationRecords(ctx, c.allowance)
	})
	c.pass("draining alliance records", func() (int, bool) {
		return app.refreshAllianceRecords(ctx, c.allowance)
	})
	c.pass("draining type details", func() (int, bool) {
		return app.refreshTypeDetails(ctx, c.allowance)
	})
}

// warmNames resolves whatever names the local caches still lack,
// then spends what is left of the lookup budget on public intel.
func (c *cycleState) warmNames(ctx context.Context, characters []db.Character) {
	app := c.app

	// Name warm-up: resolve whatever the local caches still lack —
	// type names (persisted in type_names), type→group links, group
	// names, station/system names — from the characters' latest
	// snapshots, so renders resolve from memory/DB only. Bounded
	// per cycle; whatever doesn't fit converges over later cycles.
	budget := &warmBudget{left: maxWarmLookupsPerCycle}
	if !c.limited {
		for _, ch := range characters {
			if ctx.Err() != nil {
				break
			}
			c.namesResolved += app.warmCharacterNames(ctx, ch, budget)
			if budget.errorLimited() {
				logging.Warnf("worker: ESI error limit hit during name warm-up; resuming next cycle")
				c.limited = true
				break
			}
			if c.allowance.exhausted() {
				break
			}
		}
	}

	// Intel (public data): global snapshots, war details and the
	// public name caches, spending whatever of the cycle's lookup
	// budget the character pass left. Runs with zero characters
	// linked too — none of it needs a token.
	if !c.limited {
		iStored, iNames, iLimited := app.refreshIntel(ctx, budget)
		c.refreshed += iStored
		c.namesResolved += iNames
		if iLimited {
			logging.Warnf("worker: ESI error limit hit refreshing intel; backing off until next cycle")
			c.limited = true
		}
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

// fetchBudget is one pass's fetch allowance for one cycle: the
// character pass and the market pass each hold one, so public
// market data never queues behind character warming. Safe for
// concurrent use: the region sweep pass advances every hub
// region in parallel from the market lane's one allowance.
type fetchBudget struct {
	mu   sync.Mutex
	left int
}

// take spends one fetch, reporting whether it was available.
func (b *fetchBudget) take() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.left <= 0 {
		return false
	}
	b.left--
	return true
}

func (b *fetchBudget) exhausted() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.left <= 0
}

// orderByDue stably orders characters most-overdue first: a
// character's due key is the earliest cached_until among the core
// snapshot kinds (a kind with no snapshot yet is due immediately,
// key zero). The sort is stable, so callers can layer the
// priority-flag ordering on top. Freshness for every character
// comes from one batched query, not one per character.
func (app *Application) orderByDue(ctx context.Context, characters []db.Character) []db.Character {
	out := append([]db.Character(nil), characters...)
	if len(out) == 0 {
		return out
	}
	ids := make([]int64, 0, len(out))
	for _, ch := range out {
		ids = append(ids, ch.CharacterID)
	}
	metas, err := app.queries.ListSnapshotMetaForCharacters(ctx, ids)
	if err != nil {
		// Unreadable state: every character is equally due, so
		// the given order stands.
		logging.Errorf("worker: order characters by overdue: %v", err)
		return out
	}
	byChar := make(map[int64][]db.ListSnapshotMetaForCharactersRow, len(out))
	for _, meta := range metas {
		byChar[meta.CharacterID] = append(byChar[meta.CharacterID], meta)
	}
	due := make(map[int64]time.Time, len(out))
	for _, ch := range out {
		due[ch.CharacterID] = dueKeyFromMeta(byChar[ch.CharacterID])
	}
	sort.SliceStable(out, func(i, j int) bool {
		return due[out[i].CharacterID].Before(due[out[j].CharacterID])
	})
	return out
}

// dueKeyFromMeta computes a character's most-overdue moment from
// its snapshot freshness rows: the earliest cached_until across
// every stored snapshot, pulled to the zero time when any core
// kind has never been fetched.
func dueKeyFromMeta(snaps []db.ListSnapshotMetaForCharactersRow) time.Time {
	seen := make(map[string]bool, len(snaps))
	var earliest time.Time
	for _, snap := range snaps {
		seen[snap.Kind] = true
		if !snap.CachedUntil.Valid {
			continue
		}
		if until := snap.CachedUntil.Time; earliest.IsZero() || until.Before(earliest) {
			earliest = until
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
				if guardedWarm(ctx, id, work) {
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

// guardedWarm runs one pool item under the worker panic guard. The
// guard sits per item, not per goroutine: a pool goroutine that
// died would stop draining jobs and leave the feeder blocked. A
// panicking item counts as unresolved.
func guardedWarm(ctx context.Context, id int64, work func(context.Context, int64) bool) (resolved bool) {
	defer recoverWorkerPanic("name warm-up")
	return work(ctx, id)
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
		logging.Errorf("worker: warm names for character %d: list snapshots: %v", ch.CharacterID, err)
		return 0
	}

	// What the snapshots refer to (name_harvest.go).
	harvest := &nameHarvest{app: app, characterID: ch.CharacterID, wants: newNameWants()}
	for _, snap := range snaps {
		harvest.snapshot(ctx, snap)
	}
	wants := harvest.wants

	// Structure ids met in this character's snapshots join the
	// background resolution queue (structures.go). A plain queue
	// note, no fetches — resolution runs once per cycle.
	if len(wants.structures) > 0 {
		ids := make([]int64, 0, len(wants.structures))
		for id := range wants.structures {
			ids = append(ids, id)
		}
		app.noteStructureIDs(ctx, ids...)
	}

	// With no skills/assets yet, the type/group/place passes
	// simply no-op on empty ID sets; the character-name pass at
	// the end may still have work to do.
	typeIDs := sortedInt64Keys(wants.types)

	// One pass per kind of name, each resolving what the local
	// caches still lack. They run in this order and stop as soon
	// as the cycle's budget is spent or ESI says to back off.
	passes := []func() int{
		func() int { return app.warmMissingTypeNames(ctx, typeIDs, budget) },
		func() int { return app.warmMissingTypeGroups(ctx, typeIDs, budget) },
		func() int { return app.warmMissingGroupNames(ctx, typeIDs, budget) },
		func() int { return app.warmMissingPlaceNames(ctx, wants.places, budget) },
		func() int { return app.warmMissingPlanetNames(ctx, wants.planets, budget) },
		func() int { return app.warmMissingSchematics(ctx, wants.schematics, budget) },
		func() int {
			// The killmail details are only read once this pass is
			// reached: no point loading them for a pass that will
			// not run. (Corp rosters and the other snapshots
			// harvested above feed the same set.)
			harvest.killmailPeople(ctx)
			return app.warmMissingCharacterNames(ctx, wants.characters, budget)
		},
	}
	resolved := 0
	for _, pass := range passes {
		resolved += pass()
		if budget.stopped() {
			break
		}
	}
	return resolved
}

// warmMissingTypeNames resolves the type names not yet persisted in
// type_names.
func (app *Application) warmMissingTypeNames(ctx context.Context, typeIDs []int64, budget *warmBudget) int {
	have := app.esi.CachedTypeNames(ctx, typeIDs)
	var missing []int64
	for _, id := range typeIDs {
		if _, ok := have[id]; !ok {
			missing = append(missing, id)
		}
	}
	return app.runWarmPool(ctx, missing, budget, func(ctx context.Context, id int64) bool {
		return app.warmTypeName(ctx, budget, id)
	})
}

// warmMissingTypeGroups resolves type → group links (skill-sheet
// grouping). Fetching a type to learn its group also refreshes its
// name when one is present.
func (app *Application) warmMissingTypeGroups(ctx context.Context, typeIDs []int64, budget *warmBudget) int {
	have := app.esi.CachedTypeGroups(ctx, typeIDs)
	var missing []int64
	for _, id := range typeIDs {
		if _, ok := have[id]; !ok {
			missing = append(missing, id)
		}
	}
	return app.runWarmPool(ctx, missing, budget, func(ctx context.Context, id int64) bool {
		return app.warmTypeGroup(ctx, budget, id)
	})
}

// warmMissingGroupNames resolves the name of every group those
// types belong to.
func (app *Application) warmMissingGroupNames(ctx context.Context, typeIDs []int64, budget *warmBudget) int {
	groupIDs := make(map[int64]bool)
	for _, gid := range app.esi.CachedTypeGroups(ctx, typeIDs) {
		if gid > 0 {
			groupIDs[gid] = true
		}
	}
	gids := sortedInt64Keys(groupIDs)
	have := app.esi.CachedGroupNames(ctx, gids)
	var missing []int64
	for _, gid := range gids {
		if _, ok := have[gid]; !ok {
			missing = append(missing, gid)
		}
	}
	return app.runWarmPool(ctx, missing, budget, func(ctx context.Context, id int64) bool {
		return app.warmGroupName(ctx, budget, id)
	})
}

// warmMissingPlaceNames resolves station and solar-system names
// (asset locations, colony systems). places maps each id to
// "station" or "solar_system".
func (app *Application) warmMissingPlaceNames(ctx context.Context, places map[int64]string, budget *warmBudget) int {
	pathByID := make(map[int64]string, len(places))
	var missing []int64
	for id, kind := range places {
		if _, ok := app.esi.CachedPlaceName(ctx, id); ok {
			continue
		}
		dir := "stations"
		if kind == "solar_system" {
			dir = "systems"
		}
		pathByID[id] = fmt.Sprintf("/universe/%s/%d/", dir, id)
		missing = append(missing, id)
	}
	sort.Slice(missing, func(i, j int) bool { return missing[i] < missing[j] })
	return app.runWarmPool(ctx, missing, budget, func(ctx context.Context, id int64) bool {
		return app.warmPlaceName(ctx, budget, pathByID[id], id)
	})
}

// warmMissingPlanetNames resolves the names of the character's
// colony planets (the place cache carries them; the network tier
// is /universe/planets/).
func (app *Application) warmMissingPlanetNames(ctx context.Context, planetIDs map[int64]bool, budget *warmBudget) int {
	var missing []int64
	for _, id := range sortedInt64Keys(planetIDs) {
		if _, ok := app.esi.CachedPlaceName(ctx, id); ok {
			continue
		}
		missing = append(missing, id)
	}
	return app.runWarmPool(ctx, missing, budget, func(ctx context.Context, id int64) bool {
		return app.warmPlanetName(ctx, budget, id)
	})
}

// warmMissingSchematics resolves the PI schematics behind the
// colony layouts' factory pins.
func (app *Application) warmMissingSchematics(ctx context.Context, schematicIDs map[int64]bool, budget *warmBudget) int {
	var missing []int64
	for _, id := range sortedInt64Keys(schematicIDs) {
		if _, ok := app.esi.CachedSchematic(id); ok {
			continue
		}
		missing = append(missing, id)
	}
	return app.runWarmPool(ctx, missing, budget, func(ctx context.Context, id int64) bool {
		return app.warmSchematic(ctx, budget, id)
	})
}

// warmMissingCharacterNames resolves the names of the people the
// snapshots and killmails mention.
func (app *Application) warmMissingCharacterNames(ctx context.Context, characterIDs map[int64]bool, budget *warmBudget) int {
	var missing []int64
	for _, id := range sortedInt64Keys(characterIDs) {
		if _, ok := app.esi.CachedCharacterName(id); ok {
			continue
		}
		if app.esi.CharacterNameMissed(id) {
			continue // ESI already said this ID is not a character
		}
		missing = append(missing, id)
	}
	return app.runWarmPool(ctx, missing, budget, func(ctx context.Context, id int64) bool {
		return app.warmCharacterName(ctx, budget, id)
	})
}

// fetchTypeForWarm GETs one type for the warm-up pass, translating
// an ESI error-limit response into a budget stop.
func (app *Application) fetchTypeForWarm(ctx context.Context, budget *warmBudget, id int64) (esi.Type, bool) {
	var t esi.Type
	if err := app.esi.Get(ctx, "", fmt.Sprintf("/universe/types/%d/", id), &t); err != nil {
		if errors.Is(err, esi.ErrErrorLimit) {
			budget.hitLimit()
		} else if ctx.Err() == nil {
			logging.Errorf("worker: warm type %d: %v", id, err)
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
			logging.Errorf("worker: warm group %d: %v", id, err)
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
			logging.Errorf("worker: warm place %s: %v", path, err)
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
			logging.Errorf("worker: warm character %d: %v", id, err)
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
			logging.Errorf("worker: warm planet %d: %v", id, err)
		}
		return false
	}
	if planet.Name == "" {
		return false
	}
	app.esi.StorePlaceName(id, planet.Name)
	if err := app.queries.SetPlanetName(ctx, db.SetPlanetNameParams{
		PlanetID: id, Name: planet.Name, State: esi.PlanetResolved,
		ResolvedAt: timeSet(time.Now().UTC()),
	}); err != nil {
		logging.Errorf("worker: persist planet name %d: %v", id, err)
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
			logging.Errorf("worker: warm schematic %d: %v", id, err)
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
		logging.Errorf("worker: warm killmail details for character %d: decode recent list: %v", ch.CharacterID, err)
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
			logging.Errorf("worker: warm killmail details for character %d: read detail %d: %v", ch.CharacterID, ref.KillmailID, err)
			continue
		}

		body, _, err := app.esi.FetchRaw(ctx, "", fmt.Sprintf("/killmails/%d/%s/", ref.KillmailID, ref.KillmailHash))
		if err != nil {
			if errors.Is(err, esi.ErrErrorLimit) {
				return fetched, true
			}
			if ctx.Err() == nil {
				logging.Errorf("worker: killmail detail %d for character %d: %v", ref.KillmailID, ch.CharacterID, err)
			}
			continue
		}
		if err := app.queries.UpsertKillmailDetail(ctx, db.UpsertKillmailDetailParams{
			KillmailID:  ref.KillmailID,
			CharacterID: ch.CharacterID,
			Hash:        ref.KillmailHash,
			Payload:     string(body),
			FetchedAt:   time.Now().UTC(),
		}); err != nil {
			logging.Errorf("worker: store killmail detail %d for character %d: %v", ref.KillmailID, ch.CharacterID, err)
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
