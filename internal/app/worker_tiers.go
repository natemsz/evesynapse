package app

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Warming by how much anyone is looking. ESI lets a character's
// location be asked for every five seconds and its wallet every two
// minutes, and the worker used to ask as often as ESI allowed, for
// every character, for ever. That is about seven fetches a minute per
// character, so the cycle's allowance was used up at around fifteen
// characters, with most of it spent keeping data to the second for
// accounts nobody had opened in days.
//
// Now how often a character's data is refreshed follows its account:
//
//	active   somebody has the site open (a request in the last ten
//	         minutes; an open, visible page checks in every half
//	         minute). Everything as often as ESI allows, as before.
//	recent   seen in the last day. Where the character is and what it
//	         flies: every 15 minutes. The rest: every 5.
//	dormant  not seen for a day. Position: every 6 hours. The rest:
//	         every 30 minutes, which is what notifications need.
//
// An account that comes back is active from its first request, and its
// characters go to the front of the next cycle, so the stalest it sees
// is a minute of the old data. Activity is kept in memory: after a
// restart every account counts as dormant until it is next seen.
//
// A character with nothing due is skipped before its token is looked
// at, so a dormant character's token is renewed when its data is, not
// every twenty minutes.
// ---------------------------------------------------------------------------

type warmTier int

const (
	tierActive warmTier = iota
	tierRecent
	tierDormant
)

func (t warmTier) String() string {
	switch t {
	case tierActive:
		return "active"
	case tierRecent:
		return "recent"
	}
	return "dormant"
}

const (
	tierActiveWindow = 10 * time.Minute
	tierRecentWindow = 24 * time.Hour
)

// positionKinds are the datasets that say where a character is right
// now. They change by the second and matter only to someone looking.
var positionKinds = map[string]bool{esi.SnapLocation: true, esi.SnapShip: true, esi.SnapOnline: true}

// tierHold is the least time between two fetches of a core dataset
// for a character in tier. Zero means as often as ESI allows.
func tierHold(tier warmTier, kind string) time.Duration {
	switch tier {
	case tierRecent:
		if positionKinds[kind] {
			return 15 * time.Minute
		}
		return 5 * time.Minute
	case tierDormant:
		if positionKinds[kind] {
			return 6 * time.Hour
		}
		return 30 * time.Minute
	}
	return 0
}

// tierFurtherHold is the least time between two rounds of a
// character's further datasets (mail, calendar, industry, contracts,
// planets, corporation data).
func tierFurtherHold(tier warmTier) time.Duration {
	switch tier {
	case tierRecent:
		return 5 * time.Minute
	case tierDormant:
		return 30 * time.Minute
	}
	return 0
}

// activityLog remembers when each account was last seen, and when each
// character's further datasets were last gone through.
type activityLog struct {
	mu      sync.Mutex
	seen    map[int64]time.Time // account -> last request
	further map[int64]time.Time // character -> last round of further datasets
}

// noteActivity records a request by an account. It reports whether the
// account was not active until now: it has just come back.
func (a *activityLog) noteActivity(userID int64, now time.Time) (returned bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.seen == nil {
		a.seen = map[int64]time.Time{}
	}
	last, known := a.seen[userID]
	a.seen[userID] = now
	return !known || now.Sub(last) > tierActiveWindow
}

func (a *activityLog) tier(userID int64, now time.Time) warmTier {
	a.mu.Lock()
	defer a.mu.Unlock()
	last, known := a.seen[userID]
	switch {
	case known && now.Sub(last) <= tierActiveWindow:
		return tierActive
	case known && now.Sub(last) <= tierRecentWindow:
		return tierRecent
	}
	return tierDormant
}

// furtherDue reports whether a character's further datasets are due a
// round under hold, and furtherDone records that one was made.
func (a *activityLog) furtherDue(characterID int64, hold time.Duration, now time.Time) bool {
	if hold <= 0 {
		return true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	last, known := a.further[characterID]
	return !known || now.Sub(last) >= hold
}

func (a *activityLog) furtherDone(characterID int64, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.further == nil {
		a.further = map[int64]time.Time{}
	}
	a.further[characterID] = now
}

// tierOf is the tier a character's account is in. With tiers switched
// off (WORKER_TIERS=off) every account is active.
func (app *Application) tierOf(ch db.Character, now time.Time) warmTier {
	if app.cfg.workerTiersOff {
		return tierActive
	}
	return app.activity.tier(ch.UserID, now)
}

// trackActivity notes each signed-in request, so that the worker knows
// which accounts are being looked at. An account that has just come
// back has its characters put at the front of the next cycle.
func (app *Application) trackActivity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if userID := int64(app.sessions.GetInt(ctx, sessionUserID)); userID != 0 {
			if app.activity.noteActivity(userID, time.Now()) && !app.cfg.workerTiersOff {
				app.prioritizeAccount(ctx, userID)
			}
		}
		next.ServeHTTP(w, r)
	})
}

// prioritizeAccount puts an account's characters at the front of the
// next worker cycle.
func (app *Application) prioritizeAccount(ctx context.Context, userID int64) {
	characters, err := app.queries.ListCharactersByUser(ctx, userID)
	if err != nil {
		logging.Errorf("worker: characters of returning user %d: %v", userID, err)
		return
	}
	for _, ch := range characters {
		app.markCharacterPriority(ch.CharacterID)
	}
}

// coreFreshness reads which of a character's core datasets need no
// fetch this cycle: still inside ESI's cache window, or fetched too
// recently for the character's tier. due reports whether any that the
// character has access to needs one.
func (c *cycleState) coreFreshness(ctx context.Context, ch db.Character, tier warmTier, now time.Time) (fresh map[string]bool, due bool) {
	app := c.app
	// One meta read (no payloads) for the whole freshness pass:
	// the payloads are the bulk of the table, and freshness only
	// needs cached_until and fetched_at.
	meta, merr := app.queries.ListSnapshotMetaByCharacter(ctx, ch.CharacterID)
	if merr != nil {
		logging.Errorf("worker: read snapshot freshness for character %d: %v", ch.CharacterID, merr)
	}
	fresh = make(map[string]bool, len(meta))
	for _, snap := range meta {
		held := false
		if hold := tierHold(tier, snap.Kind); hold > 0 {
			held = now.Sub(snap.FetchedAt) < hold
		}
		fresh[snap.Kind] = held || esi.CacheWindowOpen(snap.CachedUntil)
	}
	granted := scopeSet(ch.Scopes)
	for _, kind := range coreSnapshotKinds {
		if fresh[kind] || (len(granted) > 0 && kindLockedOut(granted, kind)) {
			continue
		}
		return fresh, true
	}
	return fresh, false
}

// Bounds on WORKER_FETCHES_PER_CYCLE.
const (
	minFetchesPerCycle  = 20
	mostFetchesPerCycle = 2000
)

// parseWorkerFetches reads WORKER_FETCHES_PER_CYCLE: 0 (use the
// default) when unset or unreadable, else the number kept in bounds.
func parseWorkerFetches(raw string) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		logging.Warnf("evesynapse: WORKER_FETCHES_PER_CYCLE=%q is not a whole number; using %d", raw, maxFetchesPerCycle)
		return 0
	}
	if n < minFetchesPerCycle {
		return minFetchesPerCycle
	}
	if n > mostFetchesPerCycle {
		return mostFetchesPerCycle
	}
	return n
}

// fetchesPerCycle is the character pass's fetch allowance for a cycle.
func (app *Application) fetchesPerCycle() int {
	if app.cfg.workerFetches > 0 {
		return app.cfg.workerFetches
	}
	return maxFetchesPerCycle
}
