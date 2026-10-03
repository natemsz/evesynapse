package app

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// syncKindOrder fixes the snapshot-kind display order on the Sync
// page (ListSnapshotsByCharacter orders alphabetically instead).
var syncKindOrder = []string{esi.SnapSkills, esi.SnapSkillqueue, esi.SnapWallet, esi.SnapAssets}

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
	}
	data.Sync = view

	userID := int64(app.sessions.GetInt(ctx, sessionUserID))
	if userID == 0 {
		// Dev-login sessions carry no user; nothing to show.
		app.render(w, http.StatusOK, "sync.html", data)
		return
	}

	characters, err := app.queries.ListCharactersByUser(ctx, userID)
	if err != nil {
		log.Printf("sync: list characters for user %d: %v", userID, err)
		data.Error = "Could not load sync data; check the server log."
		app.render(w, http.StatusOK, "sync.html", data)
		return
	}

	// The whole type_names table as a membership set for coverage
	// math (one query; the table is small and local).
	known := make(map[int64]bool)
	if rows, err := app.queries.ListAllTypeNames(ctx); err != nil {
		log.Printf("sync: list type names: %v", err)
	} else {
		for _, row := range rows {
			known[row.TypeID] = true
		}
	}

	for _, ch := range characters {
		cv := syncCharacterView{ID: ch.CharacterID, Name: ch.Name}

		byKind := make(map[string]db.CharacterSnapshot)
		if snaps, err := app.queries.ListSnapshotsByCharacter(ctx, ch.CharacterID); err != nil {
			log.Printf("sync: list snapshots for character %d: %v", ch.CharacterID, err)
		} else {
			for _, snap := range snaps {
				byKind[snap.Kind] = snap
			}
		}

		for _, kind := range syncKindOrder {
			row := syncSnapshotRow{Kind: kind, State: "Missing", FetchedAt: "—", CachedUntil: "—"}
			if snap, ok := byKind[kind]; ok {
				row.FetchedAt = snap.FetchedAt
				if snap.CachedUntil.Valid && snap.CachedUntil.String != "" {
					row.CachedUntil = snap.CachedUntil.String
				}
				if esi.SnapshotFresh(snap) {
					row.State = "Fresh"
				} else {
					row.State = "Stale"
				}
			}
			cv.Snapshots = append(cv.Snapshots, row)
		}

		needed := snapshotTypeIDs(byKind)
		cv.NamesTotal = len(needed)
		for id := range needed {
			if known[id] {
				cv.NamesResolved++
			}
		}
		if cv.NamesTotal > 0 {
			cv.NamesPercent = cv.NamesResolved * 100 / cv.NamesTotal
		}

		view.Characters = append(view.Characters, cv)
	}

	app.render(w, http.StatusOK, "sync.html", data)
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
			log.Printf("sync: warm: list characters for user %d: %v", userID, err)
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
