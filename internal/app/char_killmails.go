package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Killmails page: the character's most recent kills and losses.
// The recent list is a snapshot; the detail payloads behind it are
// warmed into the killmail_details store by the worker. This page
// reads the store only — a detail that has not landed yet renders
// as a "details warming" row instead of blocking on ESI.
// ---------------------------------------------------------------------------

// maxKillmailsShown caps the killmail list (the recent-list
// endpoint can return far more than a page wants).
const maxKillmailsShown = 50

// killmailRow is one list line. Rows without a stored detail yet
// carry Warming=true and placeholder fields.
type killmailRow struct {
	KillmailID  int64    // set even while warming — the zKillboard link never waits
	Time        string   // formatted kill time, "—" while warming
	System      placeRef // link-ified per the place policy; text when the SDE doesn't know it
	Kill        bool     // badge: victim is someone else
	Loss        bool     // badge: victim is the viewing character
	Victim      string
	VictimID    int64
	Ship        string // victim ship type name
	ShipTypeID  int64
	FinalBlow   string // final-blow attacker display
	FinalBlowID int64
	Involved    string // attacker count, "—" while warming
	Value       string // the killboard's stored value for the kill, bare number; "—" while the board has not priced it
	Warming     bool

	// Facts the summary, the panels and the filters read.
	At         time.Time
	SystemID   int64
	ISK        float64 // the stored value, 0 while unpriced
	Priced     bool
	Solo       bool  // one player on the attacking side
	YourBlow   bool  // the viewer landed the final blow
	Pod        bool  // the victim was in a capsule
	YourShipID int64 // the viewer's hull: the victim ship on a loss, the attacking ship on a kill
	Wing       []int64
	Href       string // opens this kill in the inspector
	On         bool
}

// killmailsView is the killmails page body; one shape serves both
// the character view and the corporation view, which share this
// page and the killmail_details
// store — only the kill/loss test differs (see killmailViewer).
type killmailsView struct {
	CharacterName string // viewing character (switcher + copy)
	Title         string // page heading
	BasePath      string // switcher link base: /killmails/ | /corporations/killmails/
	Subject       string // "this character" | "this corporation" (copy)
	CorpID        int64  // corporation view: the corporation's ID
	Loaded        bool
	Warming       bool   // no recent-list snapshot yet at all
	RoleMissing   string // in-game role label when ESI refused the list (corp view only)
	Rows          []killmailRow

	// The summary, panels and filters (killmails_shape.go).
	CharView       bool // the character view: hulls, wingmen and final blows read its pilot
	Tiles          []jobTile
	Ranges         []jobFilter
	Filters        []jobFilter
	Pulse          *kmPulse
	Hotspots       []kmSpot
	SystemClear    string
	Hulls          []kmHull
	HullClear      string
	Wing           []kmWing
	DestroyedAll   string
	LostAll        string
	Total          int // rows in the recent list before any filter
	ShownDestroyed string
	ShownLost      string
	Filtered       bool
	ClearHref      string
	ListHref       string // the list with the inspector closed
	Insp           *killmailInspector
	Depth          int // console level: 1 the list, 2 one kill
}

// handleKillmails renders the Killmails page for one of the
// signed-in user's characters (switchable via ?character=).
func (app *Application) handleKillmails(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.page(ctx)

	_, active, links, err := app.pickCharacter(ctx, r, "/killmails/")
	if err != nil {
		logging.Errorf("killmails: list characters: %v", err)
		data.Error = "Could not load killmail data; check the server log."
		app.render(ctx, w, http.StatusOK, "killmails.html", data)
		return
	}
	if links == nil {
		app.render(ctx, w, http.StatusOK, "killmails.html", data)
		return
	}
	data.KillmailChars = links

	view := &killmailsView{
		CharacterName: active.Name,
		Title:         active.Name + " — Killmails",
		BasePath:      "/killmails/",
		Subject:       "this character",
	}
	data.Killmails = view

	var refs []esi.KillmailRef
	if err := app.storedSnapshot(ctx, active, esi.SnapKillmails, &refs); err != nil {
		logging.Errorf("killmails: load recent list for character %d: %v", active.CharacterID, err)
		// No snapshot row at all = cold start: the worker is still
		// importing this character, which the Sync page shows live.
		if _, serr := app.queries.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: active.CharacterID, Kind: esi.SnapKillmails}); errors.Is(serr, sql.ErrNoRows) {
			view.Warming = true
		}
		app.render(ctx, w, http.StatusOK, "killmails.html", data)
		return
	}
	view.Loaded = true

	if len(refs) > maxKillmailsShown {
		refs = refs[:maxKillmailsShown]
	}

	// Values are the killboard's stored prices for these kills:
	// the board appraises every kill at ingest, and this page
	// reads that figure instead of pricing at render.
	values := app.killBoardValues(ctx, refs)

	viewer := killmailViewer{characterID: active.CharacterID}
	placeMemo := make(map[int64]placeRef)
	for _, ref := range refs {
		view.Rows = append(view.Rows, app.killmailRow(ctx, viewer, ref, values, placeMemo))
	}
	app.shapeKillmails(ctx, r, view, viewer)

	app.render(ctx, w, http.StatusOK, "killmails.html", data)
}

// killBoardValues reads the killboard's stored value for each
// of a page's kills in one bounded query. The board prices every
// kill at ingest through the house price order; kills it has not
// priced yet are absent from the map and their rows show "—".
func (app *Application) killBoardValues(ctx context.Context, refs []esi.KillmailRef) map[int64]float64 {
	out := make(map[int64]float64, len(refs))
	if len(refs) == 0 || app.db == nil {
		return out
	}
	var b strings.Builder
	b.WriteString("SELECT killmail_id, value_ours FROM kills WHERE value_ours IS NOT NULL AND killmail_id IN (")
	args := make([]any, 0, len(refs))
	for i, ref := range refs {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "$%d", i+1)
		args = append(args, ref.KillmailID)
	}
	b.WriteString(")")
	rows, err := app.db.QueryContext(ctx, b.String(), args...)
	if err != nil {
		logging.Errorf("killmails: read board values: %v", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var value float64
		if rows.Scan(&id, &value) == nil {
			out[id] = value
		}
	}
	return out
}

// killmailViewer decides a row's KILL/LOSS badge: the character
// view marks mails whose victim is the viewing character; the
// corporation view marks mails whose victim belongs to the
// corporation. Memory says character and corporation IDs share
// EVE's numeric space in theory, so the two IDs travel in
// separate fields and are never compared cross-wise.
type killmailViewer struct {
	characterID   int64 // character view (0 in the corp view)
	corporationID int64 // corp view (0 in the character view)
}

// killmailRow builds one list line from the stored detail. A
// missing detail (worker has not warmed it yet) yields the
// warming placeholder row.
func (app *Application) killmailRow(ctx context.Context, viewer killmailViewer, ref esi.KillmailRef, values map[int64]float64, placeMemo map[int64]placeRef) killmailRow {
	row := killmailRow{
		KillmailID: ref.KillmailID,
		Time:       "—",
		System:     placeRef{Name: "—"},
		Victim:     "—",
		Ship:       "—",
		FinalBlow:  "—",
		Involved:   "—",
		Value:      "—",
		Warming:    true,
	}

	stored, err := app.queries.GetKillmailDetail(ctx, ref.KillmailID)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			logging.Errorf("killmails: read detail %d: %v", ref.KillmailID, err)
		}
		return row
	}
	var km esi.Killmail
	if err := json.Unmarshal([]byte(stored.Payload), &km); err != nil {
		logging.Errorf("killmails: decode detail %d: %v", ref.KillmailID, err)
		return row
	}
	row.Warming = false

	row.Time = formatFinish(km.KillmailTime)
	row.System = app.linkPlaceMemo(ctx, placeMemo, km.SolarSystemID, app.locationTitle(ctx, km.SolarSystemID, "solar_system"))
	isLoss := km.Victim.CharacterID == viewer.characterID
	if viewer.corporationID > 0 {
		isLoss = km.Victim.CorporationID == viewer.corporationID
	}
	if isLoss {
		row.Loss = true
	} else {
		row.Kill = true
	}
	row.Victim = app.displayCharacter(ctx, km.Victim.CharacterID)
	row.VictimID = km.Victim.CharacterID
	row.Ship = app.typeNameOrID(ctx, km.Victim.ShipTypeID)
	row.ShipTypeID = km.Victim.ShipTypeID
	row.Involved = fmt.Sprintf("%d", len(km.Attackers))
	if t, err := time.Parse(time.RFC3339, km.KillmailTime); err == nil {
		row.At = t
	}
	row.SystemID = km.SolarSystemID
	row.Pod = km.Victim.ShipTypeID == 670 || km.Victim.ShipTypeID == 33328
	if row.Loss {
		row.YourShipID = km.Victim.ShipTypeID
	}
	players := 0
	for _, a := range km.Attackers {
		if a.CharacterID > 0 {
			players++
		}
		mine := viewer.characterID > 0 && a.CharacterID == viewer.characterID ||
			viewer.corporationID > 0 && a.CorporationID == viewer.corporationID
		if a.FinalBlow {
			row.FinalBlow = app.displayCharacter(ctx, a.CharacterID)
			row.FinalBlowID = a.CharacterID
			row.YourBlow = mine && !row.Loss
		}
		if row.Kill && viewer.characterID > 0 {
			if a.CharacterID == viewer.characterID {
				row.YourShipID = a.ShipTypeID
			} else if a.CharacterID > 0 {
				row.Wing = append(row.Wing, a.CharacterID)
			}
		}
	}
	row.Solo = row.Kill && players == 1

	// The killboard's stored price for this kill, or "—" while
	// the board has not priced it.
	if v, ok := values[ref.KillmailID]; ok {
		row.Value = esi.FormatISK(v)
		row.ISK = v
		row.Priced = true
	}
	return row
}
