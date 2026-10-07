package app

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

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

// syncCharacterView is one character block on the Sync page:
// snapshot states plus type-name coverage (resolved vs needed).
type syncCharacterView struct {
	ID            int64
	Name          string
	Snapshots     []syncSnapshotRow
	NamesResolved int
	NamesTotal    int
	NamesPercent  int // 0-100, for the coverage bar
}

// syncView is the Sync page body.
type syncView struct {
	WorkerLine string
	Warming    bool // a worker cycle is running right now
	Characters []syncCharacterView
	SDE        *sdeView
	Global     []syncSnapshotRow // public-data store (intel cluster)
	WarDetails string            // stored war detail count, formatted
}

// handleSync renders the Sync page: live worker status, per-character
// snapshot freshness, and type-name coverage with a re-warm button.
// The page auto-refreshes (pageData.AutoRefresh) so an import can be
// watched as it lands.
func (app *Application) handleSync(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := pageData{
		LoggedIn:      true,
		CharacterName: app.sessions.GetString(ctx, sessionCharacterName),
		SSOConfigured: app.cfg.SSOConfigured(),
		AutoRefresh:   true,
	}

	status := app.snapshotWorkerStatus()
	view := &syncView{
		WorkerLine: app.workerStatusText(),
		Warming:    status.Warming,
		SDE:        app.loadSDEView(ctx),
		Global:     app.loadGlobalView(ctx),
	}
	if n, err := app.queries.CountWarDetails(ctx); err != nil {
		logging.Errorf("sync: count war details: %v", err)
	} else {
		view.WarDetails = esi.FormatInt(n)
	}
	data.Sync = view

	userID := int64(app.sessions.GetInt(ctx, sessionUserID))
	if userID == 0 {
		// Dev-login sessions carry no user; nothing to show.
		app.render(ctx, w, http.StatusOK, "sync.html", data)
		return
	}

	characters, err := app.queries.ListCharactersByUser(ctx, userID)
	if err != nil {
		logging.Errorf("sync: list characters for user %d: %v", userID, err)
		data.Error = "Could not load sync data; check the server log."
		app.render(ctx, w, http.StatusOK, "sync.html", data)
		return
	}

	// First pass: per-character snapshots and the type IDs whose
	// names the coverage figure judges. The needed-ID set is
	// bounded by what this user's own snapshots reference.
	type syncCharWork struct {
		cv     syncCharacterView
		needed map[int64]bool
	}
	var work []syncCharWork
	allNeeded := make(map[int64]bool)
	for _, ch := range characters {
		cv := syncCharacterView{ID: ch.CharacterID, Name: ch.Name}

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
				} else {
					row.State = "Stale"
				}
			} else if fs, ok := fetchStates[kind]; ok && fs.State == fetchStateRoleMissing {
				row.State = "Role missing"
				row.FetchedAt = "—"
				row.CachedUntil = "needs the " + fs.Detail + " role"
				if fs.Detail == "" {
					row.CachedUntil = "refused by ESI (403)"
				}
			} else if fs, ok := fetchStates[kind]; ok && fs.State == fetchStateError {
				row.State = "Error"
				row.CachedUntil = fs.Detail
			}
			cv.Snapshots = append(cv.Snapshots, row)
		}

		needed := snapshotTypeIDs(byKind)
		for id := range needed {
			allNeeded[id] = true
		}
		work = append(work, syncCharWork{cv: cv, needed: needed})
	}

	// The name-coverage denominator resolves against the SDE type
	// table plus the type_names fallback cache, in two batched
	// lookups over the referenced IDs only -- never the whole
	// tables.
	known := make(map[int64]bool)
	if len(allNeeded) > 0 {
		ids := make([]int64, 0, len(allNeeded))
		for id := range allNeeded {
			ids = append(ids, id)
		}
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
	}

	for _, w := range work {
		cv := w.cv
		cv.NamesTotal = len(w.needed)
		for id := range w.needed {
			if known[id] {
				cv.NamesResolved++
			}
		}
		if cv.NamesTotal > 0 {
			cv.NamesPercent = cv.NamesResolved * 100 / cv.NamesTotal
		}
		view.Characters = append(view.Characters, cv)
	}

	app.render(ctx, w, http.StatusOK, "sync.html", data)
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

// handleSyncWarm enqueues priority warm-up for one of the user's
// characters (?character=) or all of them (no parameter), then
// bounces back to the Sync page to watch it happen.
func (app *Application) handleSyncWarm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userID := int64(app.sessions.GetInt(ctx, sessionUserID))
	if userID != 0 {
		characters, err := app.queries.ListCharactersByUser(ctx, userID)
		if err != nil {
			logging.Errorf("sync: warm: list characters for user %d: %v", userID, err)
		} else {
			want, _ := strconv.ParseInt(r.URL.Query().Get("character"), 10, 64)
			for _, ch := range characters {
				if want == 0 || ch.CharacterID == want {
					app.markCharacterPriority(ch.CharacterID)
				}
			}
		}
	}

	http.Redirect(w, r, "/sync/", http.StatusSeeOther)
}

// handleSyncSDE kicks off an SDE update check (which imports only
// when the remote dump changed), then bounces back to the Sync
// page to watch it happen. The same check runs weekly in the
// worker; this is its manual trigger.
func (app *Application) handleSyncSDE(w http.ResponseWriter, r *http.Request) {
	app.startSDECheck("manual")
	http.Redirect(w, r, "/sync/", http.StatusSeeOther)
}
