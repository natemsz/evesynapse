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
//	active   the character somebody is looking at: the one selected
//	         in an account that has the site open (a request in the
//	         last ten minutes; an open, visible page checks in every
//	         half minute), or one a page was opened for. Everything
//	         as often as ESI allows, as before.
//	watched  the other characters of an account that has the site
//	         open. Where they are: every 2 minutes, which keeps the
//	         fleet overview current. The rest: every 5.
//	recent   seen in the last day. Where the character is and what it
//	         flies: every 15 minutes. The rest: every 5.
//	dormant  not seen for a day. Position: every 6 hours. The rest:
//	         every 30 minutes, which is what notifications need.
//	asleep   not seen for a week, and with nowhere for a notification
//	         to go but the site itself: no browser subscribed to
//	         push and no Discord account linked. Position: every 6
//	         hours. The rest: every 2 hours. An account that is
//	         being told things elsewhere, or has Discord roles to
//	         keep right, stays dormant however long it is away.
//
// An account that comes back is active from its first request, and its
// characters go to the front of the next cycle, so the stalest it sees
// is a minute of the old data. When an account was last seen is kept
// in memory and written to its row at most once an hour (worker_activity.go),
// so a restart remembers it to within the hour.
//
// A character with nothing due is skipped before its token is looked
// at, so a dormant character's token is renewed when its data is, not
// every twenty minutes.
// ---------------------------------------------------------------------------

type warmTier int

const (
	tierActive warmTier = iota
	tierWatched
	tierRecent
	tierDormant
	tierAsleep
)

func (t warmTier) String() string {
	switch t {
	case tierActive:
		return "active"
	case tierWatched:
		return "watched"
	case tierRecent:
		return "recent"
	case tierAsleep:
		return "asleep"
	}
	return "dormant"
}

const (
	tierActiveWindow = 10 * time.Minute
	tierRecentWindow = 24 * time.Hour
	tierAsleepAfter  = 7 * 24 * time.Hour
)

// positionKinds are the datasets that say where a character is right
// now. They change by the second and matter only to someone looking.
var positionKinds = map[string]bool{esi.SnapLocation: true, esi.SnapShip: true, esi.SnapOnline: true}

// tierHold is the least time between two fetches of a core dataset
// for a character in tier. Zero means as often as ESI allows.
func tierHold(tier warmTier, kind string) time.Duration {
	switch tier {
	case tierWatched:
		if positionKinds[kind] {
			return 2 * time.Minute
		}
		return 5 * time.Minute
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
	case tierAsleep:
		if positionKinds[kind] {
			return 6 * time.Hour
		}
		return 2 * time.Hour
	}
	return 0
}

// tierFurtherHold is the least time between two rounds of a
// character's further datasets (mail, calendar, industry, contracts,
// planets, corporation data).
func tierFurtherHold(tier warmTier) time.Duration {
	switch tier {
	case tierWatched, tierRecent:
		return 5 * time.Minute
	case tierDormant:
		return 30 * time.Minute
	case tierAsleep:
		return 2 * time.Hour
	}
	return 0
}

// activityLog remembers when each account was last seen, and when each
// character's further datasets were last gone through.
type activityLog struct {
	mu      sync.Mutex
	seen    map[int64]time.Time // account -> last request
	viewed  map[int64]time.Time // character -> last time a page was about it
	further map[int64]time.Time // character -> last round of further datasets
	saved   map[int64]time.Time // account -> the last-seen time its row holds (worker_activity.go)
	active  map[int64]bool      // character -> was in the active tier at the worker's last cycle

	// listening is the accounts whose notifications go somewhere other
	// than the site, as of the last cycle; unknown until it has been
	// read, and then everyone counts as listening (worker_activity.go).
	listening      map[int64]bool
	listeningKnown bool
}

// noteTier records whether a character is in the active tier at this
// cycle, and reports whether it was at the one before.
func (a *activityLog) noteTier(characterID int64, active bool) (was bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	was = a.active[characterID]
	switch {
	case active && a.active == nil:
		a.active = map[int64]bool{characterID: true}
	case active:
		a.active[characterID] = true
	default:
		delete(a.active, characterID)
	}
	return was
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
	case known && now.Sub(last) > tierAsleepAfter && a.listeningKnown && !a.listening[userID]:
		return tierAsleep
	}
	return tierDormant
}

// noteViewed records that a page was about a character. It reports
// whether the character was not being looked at until now.
func (a *activityLog) noteViewed(characterID int64, now time.Time) (fresh bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.viewed == nil {
		a.viewed = map[int64]time.Time{}
	}
	last, known := a.viewed[characterID]
	a.viewed[characterID] = now
	return !known || now.Sub(last) > tierActiveWindow
}

// isViewed reports whether a page was about the character lately.
func (a *activityLog) isViewed(characterID int64, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	last, known := a.viewed[characterID]
	return known && now.Sub(last) <= tierActiveWindow
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
	tier := app.activity.tier(ch.UserID, now)
	// An account with the site open: only the character being looked
	// at is kept to the second. An account that is open is the
	// condition; a character id alone, which anybody can put in an
	// address, raises nothing.
	if tier == tierActive && !app.activity.isViewed(ch.CharacterID, now) {
		return tierWatched
	}
	return tier
}

// trackActivity notes each signed-in request, so that the worker knows
// which accounts are being looked at. An account that has just come
// back has its characters put at the front of the next cycle.
func (app *Application) trackActivity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if userID := int64(app.sessions.GetInt(ctx, sessionUserID)); userID != 0 {
			now := time.Now()
			if app.activity.noteActivity(userID, now) && !app.cfg.workerTiersOff {
				app.prioritizeAccount(ctx, userID)
			}
			app.saveSeen(ctx, userID, now)
			// The character being looked at: the one selected in the
			// session, and the one a page is asked for by address.
			viewed := []int64{sessionCharID(app.sessions, ctx)}
			if id, err := strconv.ParseInt(r.URL.Query().Get("character"), 10, 64); err == nil {
				viewed = append(viewed, id)
			}
			for _, id := range viewed {
				if id > 0 && app.activity.noteViewed(id, now) && !app.cfg.workerTiersOff {
					app.markCharacterPriority(id)
				}
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

// fetchesPerCycle is the character pass's fetch allowance for a cycle:
// what was set, or else what the worker's lanes get through in a
// minute (never less than the one-lane default).
func (app *Application) fetchesPerCycle() int {
	if app.cfg.workerFetches > 0 {
		return app.cfg.workerFetches
	}
	if n := app.workerLanes() * fetchesPerLane; n > maxFetchesPerCycle {
		return n
	}
	return maxFetchesPerCycle
}
