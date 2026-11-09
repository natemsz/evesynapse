package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"

	// json is goccy/go-json under the standard library's name: it
	// decodes the stored killmail details faster than
	// encoding/json with identical results (TestAppDecoderParity
	// in decode_parity_test.go).
	json "github.com/goccy/go-json"
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
	BattleHref string // the viewer's battle report this mail belongs to, if any
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
	Older         string // the next page's URL, "" at the end of the board's history
	olderCursor   string // the next page's board cursor ("<unix>.<killmail id>"), "" at the end; shapeKillmails builds Older from it with the page's query state

	// The summary, panels and filters (killmails_shape.go).
	CharView        bool // the character view: hulls, wingmen and final blows read its pilot
	Tiles           []jobTile
	TilesScope      string // the tile band's scope label: "All time" | "Last 30 days" | "Shown window"
	TilesScopeHref  string // the tile scope swap link, "" when the tiles read the shown window
	TilesScopeOther string // the scope the swap link switches to
	WindowNote      string // the listed window's kill and loss counts, for the showing note
	Ranges          []jobFilter
	Filters         []jobFilter
	Pulse           *kmPulse
	Hotspots        []kmSpot
	SystemClear     string
	Hulls           []kmHull
	HullClear       string
	Wing            []kmWing
	DestroyedAll    string
	LostAll         string
	Total           int // rows in the recent list before any filter
	ShownDestroyed  string
	ShownLost       string
	Filtered        bool
	ClearHref       string
	ListHref        string // the list with the inspector closed
	Insp            *killmailInspector
	Depth           int // console level: 1 the list, 2 one kill
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
		Title:         "Killmails",
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

	// Viewing the page offers this character and their
	// corporation to the history backfill, so the board's
	// coverage of both completes in the background.
	app.notePilotsEncountered([]int64{active.CharacterID})
	if crow, err := app.queries.GetCharacterCorporation(ctx, active.CharacterID); err == nil && crow.CorporationID > 0 {
		app.noteCorpsEncountered([]int64{crow.CorporationID})
	}

	beforeTime, beforeID, cursored := parseKillmailCursor(r.URL.Query())
	if cursored {
		// A cursored page reads the board alone: the recent
		// list is a newest-window snapshot and has nothing
		// older to offer.
		refs = nil
	} else if len(refs) > maxKillmailsShown {
		refs = refs[:maxKillmailsShown]
	}

	viewer := killmailViewer{characterID: active.CharacterID}
	view.Rows, view.olderCursor = app.killmailUnionRows(ctx, viewer, refs, beforeTime, beforeID)
	ids := make([]int64, 0, len(view.Rows))
	for _, row := range view.Rows {
		ids = append(ids, row.KillmailID)
	}
	// Mails that fall inside one of the account's recorded fights
	// open its battle report (battle_pages.go).
	battles := app.battleLinks(ctx, app.userID(ctx), ids)
	for i := range view.Rows {
		view.Rows[i].BattleHref = battles[view.Rows[i].KillmailID]
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

// warmingKillmailRow is the placeholder list line for a kill
// whose detail has not landed in the store yet.
func warmingKillmailRow(killmailID int64) killmailRow {
	return killmailRow{
		KillmailID: killmailID,
		Time:       "—",
		System:     placeRef{Name: "—"},
		Victim:     "—",
		Ship:       "—",
		FinalBlow:  "—",
		Involved:   "—",
		Value:      "—",
		Warming:    true,
	}
}

// killmailRow builds one list line from the stored detail. A
// missing detail (worker has not warmed it yet) yields the
// warming placeholder row.
func (app *Application) killmailRow(ctx context.Context, viewer killmailViewer, ref esi.KillmailRef, values map[int64]float64, placeMemo map[int64]placeRef) killmailRow {
	stored, err := app.queries.GetKillmailDetail(ctx, ref.KillmailID)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			logging.Errorf("killmails: read detail %d: %v", ref.KillmailID, err)
		}
		return warmingKillmailRow(ref.KillmailID)
	}
	var km esi.Killmail
	if err := json.Unmarshal([]byte(stored.Payload), &km); err != nil {
		logging.Errorf("killmails: decode detail %d: %v", ref.KillmailID, err)
		return warmingKillmailRow(ref.KillmailID)
	}
	value, priced := values[ref.KillmailID]
	return app.killmailRowFor(ctx, viewer, km, value, priced, placeMemo)
}

// killmailRowFor shapes one list line from a decoded killmail:
// the KILL/LOSS badge from the viewer's seat, the parties, the
// viewer's own part in it. value and priced carry the killboard's
// stored price for the kill, absent while the board has not
// priced it.
func (app *Application) killmailRowFor(ctx context.Context, viewer killmailViewer, km esi.Killmail, value float64, priced bool, placeMemo map[int64]placeRef) killmailRow {
	row := warmingKillmailRow(km.KillmailID)
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
	if priced {
		row.Value = esi.FormatISK(value)
		row.ISK = value
		row.Priced = true
	}
	return row
}

// boardKillmailRow builds the list line for a kill only the
// board holds: its full killmail from kill_details, shaped by
// the same fill the ESI rows use. A kill whose detail has been
// pruned renders from its board summary row instead.
func (app *Application) boardKillmailRow(ctx context.Context, viewer killmailViewer, k db.Kill, values map[int64]float64, placeMemo map[int64]placeRef) killmailRow {
	if payload, err := app.queries.GetKillDetail(ctx, k.KillmailID); err == nil {
		if km, ok := decodeBoardKillmail(payload); ok {
			value, priced := values[k.KillmailID]
			return app.killmailRowFor(ctx, viewer, km, value, priced, placeMemo)
		}
	}
	return app.summaryKillmailRow(ctx, viewer, k, values, placeMemo)
}

// decodeBoardKillmail reads a board-held killmail (kill_details'
// compressed payload) as the ESI shape the list rows are built from.
func decodeBoardKillmail(payload []byte) (esi.Killmail, bool) {
	var km esi.Killmail
	raw, err := killDetailRaw(payload)
	if err != nil {
		return km, false
	}
	return km, json.Unmarshal(raw, &km) == nil
}

// summaryKillmailRow renders a board kill from its summary row
// alone: the fallback for a kill whose full killmail is gone.
// The parties and the value are the summary's own; the viewer's
// attacking ship is known only when they landed the final blow,
// and the wing list stays empty.
func (app *Application) summaryKillmailRow(ctx context.Context, viewer killmailViewer, k db.Kill, values map[int64]float64, placeMemo map[int64]placeRef) killmailRow {
	row := warmingKillmailRow(k.KillmailID)
	row.Warming = false
	row.Time = formatFinish(k.KillmailTime.UTC().Format(time.RFC3339))
	row.System = app.linkPlaceMemo(ctx, placeMemo, k.SystemID, app.locationTitle(ctx, k.SystemID, "solar_system"))
	row.At = k.KillmailTime
	row.SystemID = k.SystemID
	isLoss := k.VictimCharacterID == viewer.characterID
	if viewer.corporationID > 0 {
		isLoss = k.VictimCorporationID == viewer.corporationID
	}
	if isLoss {
		row.Loss = true
	} else {
		row.Kill = true
	}
	row.Victim = app.displayCharacter(ctx, k.VictimCharacterID)
	row.VictimID = k.VictimCharacterID
	row.Ship = app.typeNameOrID(ctx, k.ShipTypeID)
	row.ShipTypeID = k.ShipTypeID
	row.Involved = fmt.Sprintf("%d", k.AttackerCount)
	row.Pod = k.ShipTypeID == 670 || k.ShipTypeID == 33328
	row.Solo = k.Solo
	if row.Loss {
		row.YourShipID = k.ShipTypeID
	}
	if k.FbCharacterID > 0 {
		row.FinalBlow = app.displayCharacter(ctx, k.FbCharacterID)
		row.FinalBlowID = k.FbCharacterID
	}
	viewerBlow := viewer.characterID > 0 && k.FbCharacterID == viewer.characterID ||
		viewer.corporationID > 0 && k.FbCorporationID == viewer.corporationID
	row.YourBlow = viewerBlow && !row.Loss
	if row.Kill && viewerBlow {
		row.YourShipID = k.FbShipTypeID
	}
	if v, ok := values[k.KillmailID]; ok {
		row.Value = esi.FormatISK(v)
		row.ISK = v
		row.Priced = true
	}
	return row
}

// parseKillmailCursor reads the ?before=<unixSeconds>.<killmailID>
// paging cursor, the killboard's convention. An absent or
// malformed cursor is the first page: the board window starts
// at the newest rows and the ESI recent list joins the union.
func parseKillmailCursor(q url.Values) (beforeTime time.Time, beforeID int64, cursored bool) {
	beforeTime, beforeID = time.Now().Add(time.Hour).UTC(), int64(1<<62)
	b := q.Get("before")
	if b == "" {
		return beforeTime, beforeID, false
	}
	if dot := strings.IndexByte(b, '.'); dot > 0 {
		if sec, err := strconv.ParseInt(b[:dot], 10, 64); err == nil {
			if id, err := strconv.ParseInt(b[dot+1:], 10, 64); err == nil {
				return time.Unix(sec, 0).UTC(), id, true
			}
		}
	}
	return beforeTime, beforeID, false
}

// killmailUnionRows builds the killmail pages' row list: the
// ESI recent list's rows, in the list's own order, unioned with
// the killboard's public kills for the same entity, newest
// first, capped at the page's size. A kill both sources know
// appears once, as the ESI ref's row: the recent list's entry
// keeps its place and its stored detail. Board-only kills are
// kills the recent list cannot show: participations without the
// final blow, or mails past its window.
//
// The board side pages by the killboard's cursor convention:
// (beforeTime, beforeID) opens its window, and the returned
// cursor names the oldest board row on the page when the board
// filled its side, so the next page continues past it without
// skipping or repeating a board row. Callers pass no refs on a
// cursored request: the recent list is a newest-window snapshot,
// so later pages are board rows only.
func (app *Application) killmailUnionRows(ctx context.Context, viewer killmailViewer, refs []esi.KillmailRef, beforeTime time.Time, beforeID int64) ([]killmailRow, string) {
	var board []db.Kill
	var err error
	// The board's kills for the entity inside the cursor's
	// window, newest first.
	switch {
	case viewer.characterID > 0:
		board, err = app.queries.ListKillsByEntityFeed(ctx, db.ListKillsByEntityFeedParams{
			Kind: "character", EntityID: viewer.characterID, BeforeTime: beforeTime, BeforeID: beforeID, MaxRows: maxKillmailsShown,
		})
	case viewer.corporationID > 0:
		board, err = app.queries.ListKillsByEntityFeed(ctx, db.ListKillsByEntityFeedParams{
			Kind: "corporation", EntityID: viewer.corporationID, BeforeTime: beforeTime, BeforeID: beforeID, MaxRows: maxKillmailsShown,
		})
	}
	if err != nil {
		logging.Errorf("killmails: board kills for the union: %v", err)
	}

	esiIDs := make(map[int64]bool, len(refs))
	for _, ref := range refs {
		esiIDs[ref.KillmailID] = true
	}
	allRefs := make([]esi.KillmailRef, 0, len(refs)+len(board))
	allRefs = append(allRefs, refs...)
	for _, k := range board {
		if !esiIDs[k.KillmailID] {
			allRefs = append(allRefs, esi.KillmailRef{KillmailID: k.KillmailID, KillmailHash: k.Hash})
		}
	}
	// Values are the killboard's stored prices for these kills:
	// the board appraises every kill at ingest, and this page
	// reads that figure instead of pricing at render.
	values := app.killBoardValues(ctx, allRefs)
	placeMemo := make(map[int64]placeRef)

	rows := make([]killmailRow, 0, len(refs))
	for _, ref := range refs {
		rows = append(rows, app.killmailRow(ctx, viewer, ref, values, placeMemo))
	}
	// Effective times for the merge. A row whose detail has not
	// warmed carries no time; the recent list is newest first,
	// so it keeps the time of the row before it, and a warming
	// first row counts as the newest of all.
	eff := make([]time.Time, len(rows))
	last := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, row := range rows {
		if !row.At.IsZero() {
			last = row.At
		}
		eff[i] = last
	}
	for _, k := range board {
		if esiIDs[k.KillmailID] {
			continue
		}
		brow := app.boardKillmailRow(ctx, viewer, k, values, placeMemo)
		at := k.KillmailTime
		pos := len(rows)
		for i := range rows {
			if eff[i].Before(at) {
				pos = i
				break
			}
		}
		rows = append(rows, killmailRow{})
		copy(rows[pos+1:], rows[pos:])
		rows[pos] = brow
		eff = append(eff, time.Time{})
		copy(eff[pos+1:], eff[pos:])
		eff[pos] = at
	}
	if len(rows) > maxKillmailsShown {
		rows = rows[:maxKillmailsShown]
	}
	// The next page's cursor: the oldest board kill this page
	// shows. It exists only while the board filled its side; a
	// short board page is the end of the entity's history.
	next := ""
	if len(board) >= maxKillmailsShown {
		onPage := make(map[int64]bool, len(rows))
		for _, row := range rows {
			onPage[row.KillmailID] = true
		}
		for _, k := range board {
			if onPage[k.KillmailID] {
				next = fmt.Sprintf("%d.%d", k.KillmailTime.Unix(), k.KillmailID)
			}
		}
	}
	return rows, next
}
