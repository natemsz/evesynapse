package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// syncKindOrder fixes the snapshot-kind display order on the Sync
// page (ListSnapshotsByCharacter orders alphabetically instead).
// The corporation kinds (corpSnapshotKinds, including the
// per-division wallet kinds) follow the character kinds.
var syncKindOrder = []string{
	esi.SnapSkills, esi.SnapSkillqueue, esi.SnapWallet, esi.SnapAssets,
	esi.SnapLocation, esi.SnapShip, esi.SnapOnline, esi.SnapClones,
	esi.SnapImplants, esi.SnapFittings, esi.SnapFatigue, esi.SnapKillmails,
	esi.SnapWalletJournal, esi.SnapWalletTxns,
	esi.SnapOrders, esi.SnapOrdersHistory, esi.SnapContracts,
	esi.SnapIndustryJobs, esi.SnapBlueprints, esi.SnapMining,
}

// syncDisplayKinds is the full Sync-page kind list: character
// kinds, then corporation kinds.
func syncDisplayKinds() []string {
	kinds := append([]string(nil), syncKindOrder...)
	return append(kinds, corpSnapshotKinds()...)
}

// syncSnapshotRow is one snapshot kind's cache state for a character.
type syncSnapshotRow struct {
	Kind        string
	State       string // "Fresh" | "Stale" | "Missing"
	FetchedAt   string // "—" when missing
	CachedUntil string // "—" when missing/unset
}

// syncCharacterView is the one character the Sync page is showing:
// what the worker is doing for it, its snapshot states, and type-name
// coverage (resolved vs needed).
type syncCharacterView struct {
	ID            int64
	Name          string
	UserID        int64
	Tier          string // how often the worker refreshes it right now
	Parked        string // why it is not being synced at all, when it is not
	Snapshots     []syncSnapshotRow
	Fresh         int // of Snapshots: how many are inside their cache window
	Stale         int
	Missing       int // never fetched, refused for want of a role, or failed
	NamesResolved int
	NamesTotal    int
	NamesPercent  int // 0-100, for the coverage bar
}

// syncView is the Sync page body: the worker, the data everyone
// shares, and one character's data found through the search box. It
// is about data and nothing else; accounts are the Admin page's.
type syncView struct {
	WorkerLine  string
	ErrorBudget string // ESI's app-wide error budget, in words
	Warming     bool   // a worker cycle is running right now
	// Timing: how long the worker's cycles take and how far behind
	// it is (worker_timing.go); nil before the first has finished.
	Timing     *workerTimingView
	Lookup     characterLookupView
	Character  *syncCharacterView // nil until one is found
	SDE        *sdeView
	Global     []syncSnapshotRow // public-data store (intel cluster)
	WarDetails string            // stored war detail count, formatted
}

// handleSync renders the Sync page: live worker status, the shared
// data, and the snapshot freshness and type-name coverage of the one
// character asked for (?character= a name or an id; the reader's own
// selected character when nothing is asked). The page auto-refreshes
// (pageData.AutoRefresh) so an import can be watched as it lands.
func (app *Application) handleSync(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.page(ctx)
	data.AutoRefresh = true

	status := app.snapshotWorkerStatus()
	view := &syncView{
		WorkerLine:  app.workerStatusText(),
		ErrorBudget: errorBudgetWords(app.esi.ErrorBudgetStatus()),
		Warming:     status.Warming,
		Timing:      workerTimingViewFor(status),
		SDE:         app.loadSDEView(ctx),
		Global:      app.loadGlobalView(ctx),
	}
	if n, err := app.queries.CountWarDetails(ctx); err != nil {
		logging.Errorf("sync: count war details: %v", err)
	} else {
		view.WarDetails = esi.FormatInt(n)
	}
	data.Sync = view

	asked := r.URL.Query().Get("character")
	ch, lookup := app.lookupCharacter(ctx, asked)
	view.Lookup = lookup
	if ch == nil && strings.TrimSpace(asked) == "" {
		// Nothing asked for: the reader's own selected character, so
		// the page opens on something.
		if id := sessionCharID(app.sessions, ctx); id != 0 {
			if own, err := app.queries.GetCharacter(ctx, id); err == nil {
				ch = &own
			}
		}
	}
	if ch != nil {
		view.Character = app.syncCharacterDetail(ctx, *ch)
	}
	app.render(ctx, w, http.StatusOK, "sync.html", data)
}

// syncCharacterDetail builds one character's block: every snapshot
// kind's state, and how many of the item names its data refers to are
// known. Stored data only; nothing is fetched.
func (app *Application) syncCharacterDetail(ctx context.Context, ch db.Character) *syncCharacterView {
	cv := &syncCharacterView{ID: ch.CharacterID, Name: ch.Name, UserID: ch.UserID, Tier: app.tierOf(ch, time.Now()).String()}
	if !characterSyncs(ch) {
		cv.Parked, cv.Tier = linkStateWords(ch.LinkState), ""
	}

	byKind := make(map[string]db.CharacterSnapshot)
	if snaps, err := app.queries.ListSnapshotsByCharacter(ctx, ch.CharacterID); err != nil {
		logging.Errorf("sync: list snapshots for character %d: %v", ch.CharacterID, err)
	} else {
		for _, snap := range snaps {
			byKind[snap.Kind] = snap
		}
	}

	// Recorded fetch outcomes (corporation role refusals and
	// errors) so a missing snapshot explains itself instead of
	// looking like an endless warm-up.
	fetchStates := make(map[string]db.SnapshotFetchState)
	if rows, err := app.queries.ListSnapshotFetchStatesByCharacter(ctx, ch.CharacterID); err != nil {
		logging.Errorf("sync: list fetch states for character %d: %v", ch.CharacterID, err)
	} else {
		for _, fs := range rows {
			fetchStates[fs.Kind] = fs
		}
	}

	for _, kind := range syncDisplayKinds() {
		row := syncSnapshotRow{Kind: kind, State: "Missing", FetchedAt: "—", CachedUntil: "—"}
		if snap, ok := byKind[kind]; ok {
			row.FetchedAt = rfc3339(snap.FetchedAt)
			row.CachedUntil = rfc3339Or(snap.CachedUntil, "—")
			if esi.SnapshotFresh(snap) {
				row.State = "Fresh"
				cv.Fresh++
			} else {
				row.State = "Stale"
				cv.Stale++
			}
		} else {
			cv.Missing++
			if fs, ok := fetchStates[kind]; ok && fs.State == fetchStateRoleMissing {
				row.State = "Role missing"
				row.CachedUntil = "needs the " + fs.Detail + " role"
				if fs.Detail == "" {
					row.CachedUntil = "refused by ESI (403)"
				}
			} else if ok && fs.State == fetchStateError {
				row.State = "Error"
				row.CachedUntil = fs.Detail
			}
		}
		cv.Snapshots = append(cv.Snapshots, row)
	}

	// Name coverage: the item types this character's data refers to,
	// against the SDE type table plus the type_names fallback cache,
	// in two batched lookups over those ids only.
	needed := snapshotTypeIDs(byKind)
	cv.NamesTotal = len(needed)
	if len(needed) > 0 {
		ids := make([]int64, 0, len(needed))
		for id := range needed {
			ids = append(ids, id)
		}
		known := make(map[int64]bool)
		if rows, err := app.queries.ListTypeNameIDsByIDs(ctx, ids); err != nil {
			logging.Errorf("sync: list type names: %v", err)
		} else {
			for _, id := range rows {
				known[id] = true
			}
		}
		if rows, err := app.queries.ListSDETypeIDsByIDs(ctx, ids); err != nil {
			logging.Errorf("sync: list SDE type ids: %v", err)
		} else {
			for _, id := range rows {
				known[id] = true
			}
		}
		cv.NamesResolved = len(known)
		cv.NamesPercent = cv.NamesResolved * 100 / cv.NamesTotal
	}
	return cv
}

// linkStateWords says why a character is not being synced.
func linkStateWords(state string) string {
	switch state {
	case linkStateTokenDead:
		return "its EVE sign-in has expired or was revoked; it has to sign in again"
	case linkStateOwnerChanged:
		return "the character changed hands; it has to sign in again"
	}
	return "it has to sign in again (" + state + ")"
}

// loadGlobalView builds the Sync page's public-data block: one
// row per global snapshot kind (fresh/stale/missing), the
// counterpart of the per-character snapshot tables. DB only.
func (app *Application) loadGlobalView(ctx context.Context) []syncSnapshotRow {
	byKind := make(map[string]db.GlobalSnapshot)
	if snaps, err := app.queries.ListGlobalSnapshots(ctx); err != nil {
		logging.Errorf("sync: list global snapshots: %v", err)
	} else {
		for _, snap := range snaps {
			byKind[snap.Kind] = snap
		}
	}

	rows := make([]syncSnapshotRow, 0, len(globalKindOrder))
	for _, kind := range globalKindOrder {
		row := syncSnapshotRow{Kind: kind, State: "Missing", FetchedAt: "—", CachedUntil: "—"}
		if snap, ok := byKind[kind]; ok {
			row.FetchedAt = rfc3339(snap.FetchedAt)
			row.CachedUntil = rfc3339(snap.CachedUntil)
			if esi.GlobalSnapshotFresh(snap) {
				row.State = "Fresh"
			} else {
				row.State = "Stale"
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// snapshotTypeIDs collects the distinct type IDs a character's
// latest skills + assets snapshots reference — the denominator of
// the Sync page's name-coverage figure. Undecodable payloads are
// skipped (the worker will refetch them in due course).
func snapshotTypeIDs(byKind map[string]db.CharacterSnapshot) map[int64]bool {
	needed := make(map[int64]bool)
	if snap, ok := byKind[esi.SnapSkills]; ok {
		var skills esi.Skills
		if err := json.Unmarshal([]byte(snap.Payload), &skills); err == nil {
			for _, s := range skills.Skills {
				needed[s.SkillID] = true
			}
		}
	}
	if snap, ok := byKind[esi.SnapAssets]; ok {
		var items []esi.Asset
		if err := json.Unmarshal([]byte(snap.Payload), &items); err == nil {
			for _, it := range items {
				needed[it.TypeID] = true
			}
		}
	}
	return needed
}

// handleSyncWarm puts characters at the front of the worker's next
// cycle, then bounces back to the Sync page to watch it happen. With
// ?character= it is that one character, whoever's it is (the page is
// an administrator's); with no parameter it is every character of the
// reader's own account.
func (app *Application) handleSyncWarm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	back := "/sync/"

	if raw := r.URL.Query().Get("character"); raw != "" {
		want, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || want <= 0 {
			// Garbage in the parameter warms nothing: it must never
			// fall through to "everything".
			logging.Warnf("sync: warm: ignoring invalid character parameter %q", raw)
			http.Redirect(w, r, back, http.StatusSeeOther)
			return
		}
		if ch, err := app.queries.GetCharacter(ctx, want); err == nil {
			app.markCharacterPriority(ch.CharacterID)
			back = "/sync/?character=" + strconv.FormatInt(ch.CharacterID, 10)
		}
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}

	if userID := app.userID(ctx); userID != 0 {
		characters, err := app.queries.ListCharactersByUser(ctx, userID)
		if err != nil {
			logging.Errorf("sync: warm: list characters for user %d: %v", userID, err)
		}
		for _, ch := range characters {
			app.markCharacterPriority(ch.CharacterID)
		}
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// handleSyncSDE kicks off an SDE update check (which imports only
// when the remote dump changed), then bounces back to the Sync
// page to watch it happen. The same check runs weekly in the
// worker; this is its manual trigger.
func (app *Application) handleSyncSDE(w http.ResponseWriter, r *http.Request) {
	app.startSDECheck("manual")
	http.Redirect(w, r, "/sync/", http.StatusSeeOther)
}

// errorBudgetWords says how much of ESI's error budget is left. The
// budget is for the whole application: once it is spent, ESI refuses
// every request until it resets.
func errorBudgetWords(remain int, resetUnix int64) string {
	left := time.Until(time.Unix(resetUnix, 0))
	switch {
	case resetUnix == 0:
		return "not reported yet"
	case left <= 0:
		return "full (the last window has reset)"
	}
	return fmt.Sprintf("%d errors left, resets in %ds", remain, int(left.Round(time.Second).Seconds()))
}
