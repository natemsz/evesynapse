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
	Older         string               // the next page's URL, "" at the end of the board's history
	olderCursor   string               // the next page's board cursor ("<unix>.<killmail id>"), "" at the end; shapeKillmails builds Older from it with the page's query state
	zkill         *zkillKillmailBundle // the live zKillboard read for this page's rows and tiles

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
	// The board side reads zKillboard live: the same bundle
	// feeds the rows and the tiles, so one view costs one
	// stats call and one bounded history walk.
	view.zkill = app.zkillKillmailBundle(ctx, viewer)
	view.Rows, view.olderCursor = app.killmailUnionRows(ctx, viewer, refs, view.zkill, beforeTime, beforeID)
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

// zkillKillmailBundle is one entity's board read live from
// zKillboard for the killmails page: the all-time counts from
// the stats endpoint and the recent kills and losses from the
// history endpoint, priced with our own appraisal. The rows and
// the tiles read the same bundle, so one page view costs one
// stats call and one bounded history walk.
type zkillKillmailBundle struct {
	Stats   *zkillStats
	Entries []zkillHistoryEntry
	Values  map[int64]float64
	// Complete is false when the history walk stopped at its
	// page bound with a full page, so more history may exist
	// past the bound.
	Complete bool
}

// zkillKillmailBundle reads the viewing entity's board live
// from zKillboard. A failed fetch returns a bundle with no
// stats and no entries; the page degrades to the ESI recent
// list and window tiles instead of failing.
func (app *Application) zkillKillmailBundle(ctx context.Context, viewer killmailViewer) *zkillKillmailBundle {
	ctx, cancel := context.WithTimeout(ctx, pilotLiveTimeout)
	defer cancel()
	var kind zkillHistoryKind
	var id int64
	if viewer.characterID > 0 {
		kind, id = zkillHistoryCharacter, viewer.characterID
	} else {
		kind, id = zkillHistoryCorporation, viewer.corporationID
	}
	stats, err := app.fetchZKillStats(ctx, kind, id)
	if err != nil {
		logging.Warnf("killmails: live zkill stats %d: %v", id, err)
	}
	entries, complete := app.fetchZKillHistory(ctx, kind, id, zkillHistoryMaxPages, time.Time{})
	values := app.priceZKillEntries(ctx, entries)
	return &zkillKillmailBundle{Stats: stats, Entries: entries, Values: values, Complete: complete}
}

// killmailUnionRows builds the killmail pages' row list: the
// ESI recent list's rows, in the list's own order, unioned with
// the entity's kills and losses read live from zKillboard,
// newest first, capped at the page's size. A kill both sources
// know appears once, as the ESI ref's row: the recent list's
// entry keeps its place and its stored detail. Board-only kills
// are kills the recent list cannot show: participations without
// the final blow, or mails past its window.
//
// The board side pages by the killboard's cursor convention:
// (beforeTime, beforeID) opens its window, and the returned
// cursor names the oldest board row on the page when the board
// side holds more, so the next page continues past it without
// skipping or repeating a board row. Callers pass no refs on a
// cursored request: the recent list is a newest-window snapshot,
// so later pages are board rows only.
func (app *Application) killmailUnionRows(ctx context.Context, viewer killmailViewer, refs []esi.KillmailRef, zb *zkillKillmailBundle, beforeTime time.Time, beforeID int64) ([]killmailRow, string) {
	// The board's kills for the entity inside the cursor's
	// window, newest first.
	var board []zkillHistoryEntry
	if zb != nil {
		for _, e := range zb.Entries {
			if e.KillmailID <= 0 || e.KillmailTime.IsZero() {
				continue
			}
			if !(e.KillmailTime.Before(beforeTime) || (e.KillmailTime.Equal(beforeTime) && e.KillmailID < beforeID)) {
				continue
			}
			board = append(board, e)
		}
	}

	esiIDs := make(map[int64]bool, len(refs))
	for _, ref := range refs {
		esiIDs[ref.KillmailID] = true
	}
	allRefs := make([]esi.KillmailRef, 0, len(refs)+len(board))
	allRefs = append(allRefs, refs...)
	for _, e := range board {
		if !esiIDs[e.KillmailID] {
			allRefs = append(allRefs, esi.KillmailRef{KillmailID: e.KillmailID})
		}
	}
	// Values are the killboard's stored prices for the ESI
	// rows, filled in from our own appraisal of the fetched
	// kills where the board has not priced one yet.
	values := app.killBoardValues(ctx, allRefs)
	if zb != nil {
		for id, v := range zb.Values {
			if _, ok := values[id]; !ok {
				values[id] = v
			}
		}
	}
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
	for _, e := range board {
		if esiIDs[e.KillmailID] {
			continue
		}
		km, ok := zkillEntryKillmail(e)
		if !ok {
			continue
		}
		value, priced := values[e.KillmailID]
		brow := app.killmailRowFor(ctx, viewer, km, value, priced, placeMemo)
		at := e.KillmailTime
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
	// shows. It exists only while the board side holds more
	// than this page shows.
	next := ""
	boardTotal := 0
	for _, e := range board {
		if !esiIDs[e.KillmailID] {
			boardTotal++
		}
	}
	if boardTotal > 0 {
		onPage := make(map[int64]bool, len(rows))
		for _, row := range rows {
			onPage[row.KillmailID] = true
		}
		shown := 0
		for _, e := range board {
			if !esiIDs[e.KillmailID] && onPage[e.KillmailID] {
				shown++
				next = fmt.Sprintf("%d.%d", e.KillmailTime.Unix(), e.KillmailID)
			}
		}
		if shown >= boardTotal {
			next = ""
		}
	}
	return rows, next
}
