package app

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------------------
// Killmails summary, panels, filters and inspector. Everything here
// reads the rows the page already built from the stored details and
// the board's stored values: no ESI call and no repricing at render.
// The panels cover the chosen time range of the recent list and the
// filters then narrow the table; the summary tiles headline the
// entity's record from the stored profile and the board's stats.
// ---------------------------------------------------------------------------

// killBigISK is the "high value" filter's floor.
const killBigISK = 100_000_000

// kmSpot is one system on the hotspots panel.
type kmSpot struct {
	Name    string
	SecText string
	SecTier int
	Kills   int
	Losses  int
	Href    string
	On      bool
}

// kmHull is one of the viewer's hulls on the hulls panel.
type kmHull struct {
	TypeID   int64
	Name     string
	Kills    int
	Losses   int
	Record   string // "14 kills, 0 losses"
	Eff      int    // ISK efficiency, percent
	EffClass string // pw-N width class for the bar
	Href     string
	On       bool
}

// kmWing is a pilot who shared the viewer's kills.
type kmWing struct {
	ID     int64
	Name   string
	Kills  int
	ISK    string
	Href   string
	Shared string // "18 shared kills"
}

// kmPulseDay is one day's bars on the ISK flow chart.
type kmPulseDay struct {
	X           int
	KillY       int // top of the destroyed bar
	KillH       int
	LossH       int
	Label       string // "Nov 2"
	Title       string // hover text
	Tick        bool   // label this day under the axis
	Destroyed   float64
	LostISK     float64
	HasActivity bool
}

// kmPulse is the ISK flow chart: destroyed above the line, lost
// below, one column a day for the last 14 days.
type kmPulse struct {
	Days      []kmPulseDay
	Destroyed string
	Lost      string
	Peak      string // "Nov 2", the day with the most ISK moved
	Width     int
}

const (
	kmPulseDays = 14
	kmPulseStep = 20
	kmPulseBar  = 12
	kmPulseHalf = 44 // px each side of the baseline
	kmPulseBase = 48
)

// killmailInspector is the open kill in the inspector pane.
type killmailInspector struct {
	Row       killmailRow
	Page      *killPageView // nil until the board has stored the kill
	CloseHref string
	PageHref  string
}

// shapeKillmails fills the summary, the panels, the filter chips and
// the inspector, and narrows view.Rows to the chosen filters.
func (app *Application) shapeKillmails(ctx context.Context, r *http.Request, view *killmailsView, viewer killmailViewer) {
	q := r.URL.Query()
	base := url.Values{}
	for _, k := range []string{"character", "corporation"} {
		if v := q.Get(k); v != "" {
			base.Set(k, v)
		}
	}
	when := q.Get("when")
	if when != "7d" && when != "30d" {
		when = ""
	}
	filter := q.Get("f")
	switch filter {
	case "kills", "losses", "solo", "blow", "big":
	default:
		filter = ""
	}
	// The tile band's own scope: all time, or the last 30 days
	// on ?scope=30d. It scopes the summary tiles only; the list
	// below keeps its own range and filters, and neither drives
	// the other.
	scope := q.Get("scope")
	if scope != "30d" {
		scope = ""
	}
	systemID, _ := strconv.ParseInt(q.Get("system"), 10, 64)
	hullID, _ := strconv.ParseInt(q.Get("hull"), 10, 64)
	killID, _ := strconv.ParseInt(q.Get("kill"), 10, 64)
	view.CharView = viewer.characterID > 0

	// href keeps the current choices and changes the ones given.
	href := func(set map[string]string) string {
		v := url.Values{}
		for k, vals := range base {
			v[k] = vals
		}
		cur := map[string]string{"when": when, "f": filter, "scope": scope}
		if systemID > 0 {
			cur["system"] = strconv.FormatInt(systemID, 10)
		}
		if hullID > 0 {
			cur["hull"] = strconv.FormatInt(hullID, 10)
		}
		for k, val := range set {
			cur[k] = val
		}
		for k, val := range cur {
			v.Set(k, val)
		}
		return withQuery(view.BasePath, v)
	}

	// The time range picks the rows everything else reads.
	now := time.Now().UTC()
	var cutoff time.Time
	switch when {
	case "7d":
		cutoff = now.Add(-7 * 24 * time.Hour)
	case "30d":
		cutoff = now.Add(-30 * 24 * time.Hour)
	}
	var ranged []killmailRow
	for _, row := range view.Rows {
		if !cutoff.IsZero() && (row.Warming || row.At.Before(cutoff)) {
			continue
		}
		ranged = append(ranged, row)
	}
	for _, rg := range []struct{ key, label string }{{"", "All recent"}, {"30d", "30 days"}, {"7d", "7 days"}} {
		view.Ranges = append(view.Ranges, jobFilter{Label: rg.label, Href: href(map[string]string{"when": rg.key}), On: when == rg.key})
	}

	// The listed window's own sums: the panels below read the
	// same rows, the showing note reports these counts, and the
	// tiles fall back to them when the entity has no stored
	// record to headline.
	var kills, losses, solo, blows, pods, attackers int
	var destroyed, lost float64
	for _, row := range ranged {
		switch {
		case row.Warming:
		case row.Loss:
			losses++
			lost += row.ISK
			if row.Pod {
				pods++
			}
		default:
			kills++
			destroyed += row.ISK
			if row.Solo {
				solo++
			}
			if row.YourBlow {
				blows++
			}
			n, _ := strconv.Atoi(row.Involved)
			attackers += n
		}
	}
	view.WindowNote = fmt.Sprintf("%s, %s in this window",
		pluralCount(int64(kills), "kill", "kills"), pluralCount(int64(losses), "loss", "losses"))

	// Summary tiles. The tiles headline the entity, not the
	// list: their figures are the entity's record at the tile
	// scope (all time, or the last 30 days on ?scope=30d), read
	// from the stored profile and the involvement stats. The
	// window sums stand only when no record exists to read,
	// and the tile band then says its figures are the window's.
	tKills, tLosses, tSolo := int64(kills), int64(losses), int64(solo)
	tDestroyed, tLost := destroyed, lost
	record := app.killmailTileRecord(ctx, viewer, scope == "30d", now)
	if record != nil {
		tKills, tLosses, tSolo = record.kills, record.losses, record.solo
		tDestroyed, tLost = record.destroyed, record.lost
		view.TilesScope = "All time"
		view.TilesScopeOther = "Last 30 days"
		view.TilesScopeHref = href(map[string]string{"scope": "30d"})
		if scope == "30d" {
			view.TilesScope = "Last 30 days"
			view.TilesScopeOther = "All time"
			view.TilesScopeHref = href(map[string]string{"scope": ""})
		}
	} else {
		view.TilesScope = "Shown window"
	}
	// Sub-lines stay inside the scope of the figures above
	// them: the window's subs keep its own facts (final blows,
	// pods, gang size); the record's read only record facts.
	var killSub, lossSub string
	if record != nil {
		killSub = fmt.Sprintf("%d solo", tSolo)
		lossSub = "no losses"
		if tLosses > 0 {
			lossSub = fmt.Sprintf("%.2f kills per loss", float64(tKills)/float64(tLosses))
		}
	} else {
		killSub = fmt.Sprintf("%d solo", solo)
		if view.CharView {
			killSub += ", " + plural(blows, "final blow")
		}
		if kills > 0 {
			killSub += fmt.Sprintf(", %.1f pilots on average", float64(attackers)/float64(kills))
		}
		lossSub = "no pods lost"
		switch {
		case pods == 1:
			lossSub = "1 pod lost"
		case pods > 1:
			lossSub = fmt.Sprintf("%d pods lost", pods)
		}
		if losses > 0 {
			lossSub += fmt.Sprintf(", %.2f kills per loss", float64(kills)/float64(losses))
		}
	}
	effSub := "nothing priced yet"
	if tDestroyed+tLost > 0 {
		effSub = fmt.Sprintf("%d%% ISK efficiency", int(math.Round(tDestroyed/(tDestroyed+tLost)*100)))
	}
	net := tDestroyed - tLost
	netSub := "net " + iskCompact(net) + " ISK"
	if net > 0 {
		netSub = "net +" + iskCompact(net) + " ISK"
	}
	view.Tiles = []jobTile{
		{Label: "Kills", Value: strconv.FormatInt(tKills, 10), Sub: killSub, Href: href(map[string]string{"f": "kills"}), On: filter == "kills", Tone: "ok"},
		{Label: "Losses", Value: strconv.FormatInt(tLosses, 10), Sub: lossSub, Href: href(map[string]string{"f": "losses"}), On: filter == "losses", Tone: "warn"},
		{Label: "ISK destroyed", Value: iskCompact(tDestroyed), Sub: effSub},
		{Label: "ISK lost", Value: iskCompact(tLost), Sub: netSub},
	}
	view.DestroyedAll = iskCompact(destroyed)
	view.LostAll = iskCompact(lost)

	view.Pulse = killPulse(ranged, now)

	// Hotspots: the systems with the most fights, kills and losses.
	names := app.newKillNames(ctx)
	spotBy := map[int64]*kmSpot{}
	var spotOrder []int64
	for _, row := range ranged {
		if row.Warming || row.SystemID == 0 {
			continue
		}
		sp, ok := spotBy[row.SystemID]
		if !ok {
			sys := names.system(row.SystemID)
			text, tier := secRound(sys.Security)
			name := sys.Name
			if name == "" {
				name = row.System.Name
			}
			sp = &kmSpot{Name: name, SecText: text, SecTier: tier, On: row.SystemID == systemID,
				Href: href(map[string]string{"system": strconv.FormatInt(row.SystemID, 10)})}
			if sp.On {
				sp.Href = href(map[string]string{"system": ""})
			}
			spotBy[row.SystemID] = sp
			spotOrder = append(spotOrder, row.SystemID)
		}
		if row.Loss {
			sp.Losses++
		} else {
			sp.Kills++
		}
	}
	sort.SliceStable(spotOrder, func(i, j int) bool {
		a, b := spotBy[spotOrder[i]], spotBy[spotOrder[j]]
		return a.Kills+a.Losses > b.Kills+b.Losses
	})
	for i, id := range spotOrder {
		if i >= 5 && !spotBy[id].On {
			continue
		}
		view.Hotspots = append(view.Hotspots, *spotBy[id])
	}
	if systemID > 0 {
		view.SystemClear = href(map[string]string{"system": ""})
	}

	// Hulls and wingmen read the viewing character's own seat; the
	// corporation view has no single pilot to read them for.
	if view.CharView {
		type hullAcc struct {
			kmHull
			destroyed, lost float64
		}
		hullBy := map[int64]*hullAcc{}
		var hullOrder []int64
		type wingAcc struct {
			kills int
			isk   float64
		}
		wingBy := map[int64]*wingAcc{}
		for _, row := range ranged {
			if row.Warming {
				continue
			}
			if row.YourShipID > 0 {
				h, ok := hullBy[row.YourShipID]
				if !ok {
					h = &hullAcc{kmHull: kmHull{TypeID: row.YourShipID, Name: names.typeName(row.YourShipID), On: row.YourShipID == hullID}}
					h.Href = href(map[string]string{"hull": strconv.FormatInt(row.YourShipID, 10)})
					if h.On {
						h.Href = href(map[string]string{"hull": ""})
					}
					hullBy[row.YourShipID] = h
					hullOrder = append(hullOrder, row.YourShipID)
				}
				if row.Loss {
					h.Losses++
					h.lost += row.ISK
				} else {
					h.Kills++
					h.destroyed += row.ISK
				}
			}
			if row.Kill {
				for _, id := range row.Wing {
					w, ok := wingBy[id]
					if !ok {
						w = &wingAcc{}
						wingBy[id] = w
					}
					w.kills++
					w.isk += row.ISK
				}
			}
		}
		sort.SliceStable(hullOrder, func(i, j int) bool {
			a, b := hullBy[hullOrder[i]], hullBy[hullOrder[j]]
			return a.Kills+a.Losses > b.Kills+b.Losses
		})
		for i, id := range hullOrder {
			h := hullBy[id]
			if i >= 5 && !h.On {
				continue
			}
			if h.destroyed+h.lost > 0 {
				f := h.destroyed / (h.destroyed + h.lost)
				h.Eff = int(math.Round(f * 100))
				h.EffClass = pctWidthClass(f)
			} else {
				h.EffClass = pctWidthClass(0)
			}
			h.Record = plural(h.Kills, "kill") + ", 1 loss"
			if h.Losses != 1 {
				h.Record = fmt.Sprintf("%s, %d losses", plural(h.Kills, "kill"), h.Losses)
			}
			view.Hulls = append(view.Hulls, h.kmHull)
		}
		if hullID > 0 {
			view.HullClear = href(map[string]string{"hull": ""})
		}
		wingIDs := make([]int64, 0, len(wingBy))
		for id := range wingBy {
			wingIDs = append(wingIDs, id)
		}
		sort.Slice(wingIDs, func(i, j int) bool {
			a, b := wingBy[wingIDs[i]], wingBy[wingIDs[j]]
			if a.kills != b.kills {
				return a.kills > b.kills
			}
			return wingIDs[i] < wingIDs[j]
		})
		for i, id := range wingIDs {
			if i == 4 {
				break
			}
			w := wingBy[id]
			shared := plural(w.kills, "shared kill")
			view.Wing = append(view.Wing, kmWing{ID: id, Name: app.displayCharacter(ctx, id), Kills: w.kills,
				ISK: iskCompact(w.isk), Shared: shared, Href: fmt.Sprintf("/pilot/?character=%d", id)})
		}
	}

	// Filter chips, counted within the system and hull narrowing.
	var scoped []killmailRow
	for _, row := range ranged {
		if systemID > 0 && row.SystemID != systemID {
			continue
		}
		if hullID > 0 && row.YourShipID != hullID {
			continue
		}
		scoped = append(scoped, row)
	}
	match := func(row killmailRow, f string) bool {
		switch f {
		case "kills":
			return !row.Warming && row.Kill
		case "losses":
			return !row.Warming && row.Loss
		case "solo":
			return row.Solo
		case "blow":
			return row.YourBlow
		case "big":
			return row.ISK >= killBigISK
		}
		return true
	}
	chips := []struct{ key, label string }{{"", "All"}, {"kills", "Kills"}, {"losses", "Losses"}, {"solo", "Solo kills"}}
	if view.CharView {
		chips = append(chips, struct{ key, label string }{"blow", "Your final blows"})
	}
	chips = append(chips, struct{ key, label string }{"big", "Over 100M"})
	for _, c := range chips {
		n := 0
		for _, row := range scoped {
			if match(row, c.key) {
				n++
			}
		}
		if n == 0 && c.key != "" && c.key != filter {
			continue
		}
		view.Filters = append(view.Filters, jobFilter{Label: c.label, Count: n, Href: href(map[string]string{"f": c.key}), On: filter == c.key})
	}
	var shown []killmailRow
	var shownDest, shownLost float64
	for _, row := range scoped {
		if !match(row, filter) {
			continue
		}
		row.Href = href(map[string]string{"kill": strconv.FormatInt(row.KillmailID, 10)})
		if row.KillmailID == killID {
			row.On = true
		}
		if row.Loss {
			shownLost += row.ISK
		} else {
			shownDest += row.ISK
		}
		shown = append(shown, row)
	}
	view.Filtered = when != "" || filter != "" || systemID > 0 || hullID > 0
	// Clearing the list's filters keeps the tile scope: the two
	// are independent, and clearing one must not move the other.
	clearVals := url.Values{}
	for k, vals := range base {
		clearVals[k] = vals
	}
	if scope != "" {
		clearVals.Set("scope", scope)
	}
	view.ClearHref = withQuery(view.BasePath, clearVals)
	view.Total = len(view.Rows)
	view.Rows = shown
	view.ShownDestroyed = iskCompact(shownDest)
	view.ShownLost = iskCompact(shownLost)
	view.ListHref = href(nil)
	// The Older link pages the board window: it keeps the
	// page's current choices and adds the union's cursor.
	if view.olderCursor != "" {
		view.Older = href(map[string]string{"before": view.olderCursor})
	}
	view.Depth = 1 // the list; the console has no directory level

	// The inspector opens a kill from the viewer's own recent list
	// only, so a private kill shows to the pilots it belongs to.
	if killID > 0 {
		for _, row := range shown {
			if row.KillmailID != killID {
				continue
			}
			insp := &killmailInspector{Row: row, CloseHref: view.ListHref}
			k, err := app.queries.GetKill(ctx, killID)
			switch {
			case err == nil:
				insp.Page = app.killPageFor(ctx, names, k)
				if k.Public {
					insp.PageHref = fmt.Sprintf("/intel/kill/%d/", killID)
				}
			case !errors.Is(err, pgx.ErrNoRows):
				logging.Errorf("killmails: board row %d: %v", killID, err)
			}
			view.Insp = insp
			view.Depth = 2
			break
		}
	}
}

// kmTileRecord is an entity's kill record at one scope, read
// from the stored profile and the involvement stats.
type kmTileRecord struct {
	kills, losses, solo int64
	destroyed, lost     float64
}

// killmailTileRecord reads the killmail pages' tile figures
// for the viewing entity at the tile scope. All time, a
// character's counts come from their stored activity profile
// when one backs them (the profile's figures cover their
// whole recorded history) and solo and ISK from the all-time
// stored aggregates, the division the pilot page and the
// killboard headline use.
// Without a profile the stored read carries every figure. The
// last 30 days read the stats directly for counts and ISK
// alike: the profile keeps no windowed counts. A nil record
// means the entity has no stored record to read at all, and
// the tiles fall back to the listed window's figures.
func (app *Application) killmailTileRecord(ctx context.Context, viewer killmailViewer, last30 bool, now time.Time) *kmTileRecord {
	since := time.Unix(0, 0).UTC()
	if last30 {
		since = now.Add(-30 * 24 * time.Hour)
	}
	rec, err := app.killmailStatsAt(ctx, viewer, since)
	if err != nil {
		logging.Warnf("killmails: tile stats: %v", err)
		return nil
	}
	if !last30 && viewer.characterID > 0 {
		if prof, perr := app.queries.GetPilotActivityProfile(ctx, viewer.characterID); perr == nil && prof.Kills+prof.Losses > 0 {
			rec.kills, rec.losses = prof.Kills, prof.Losses
		} else if perr != nil && !errors.Is(perr, pgx.ErrNoRows) {
			logging.Warnf("killmails: tile profile %d: %v", viewer.characterID, perr)
		}
	}
	if rec.kills+rec.losses > 0 {
		return rec
	}
	if !last30 {
		// The all-time scope is the whole record: empty there
		// is absent, not a quiet window.
		return nil
	}
	// A quiet 30 days is a figure, not a missing record: the
	// fallback turns on the entity having no record at all.
	if viewer.characterID > 0 {
		if prof, perr := app.queries.GetPilotActivityProfile(ctx, viewer.characterID); perr == nil && prof.Kills+prof.Losses > 0 {
			return &kmTileRecord{}
		}
	}
	all, err := app.killmailStatsAt(ctx, viewer, time.Unix(0, 0).UTC())
	if err != nil {
		logging.Warnf("killmails: tile stats: %v", err)
		return nil
	}
	if all.kills+all.losses > 0 {
		return &kmTileRecord{}
	}
	return nil
}

// killmailStatsAt reads one entity's kills, losses, solo
// kills and ISK destroyed and lost since a time, from the
// stored daily aggregates folded at ingest.
func (app *Application) killmailStatsAt(ctx context.Context, viewer killmailViewer, since time.Time) (*kmTileRecord, error) {
	kind, id := "character", viewer.characterID
	if viewer.corporationID > 0 {
		kind, id = "corporation", viewer.corporationID
	}
	st, err := app.queries.GetKillEntityStats(ctx, db.GetKillEntityStatsParams{Kind: kind, EntityID: id, Since: since})
	if err != nil {
		return nil, err
	}
	return &kmTileRecord{kills: st.Kills, losses: st.Losses, solo: st.Solo, destroyed: st.IskKilled, lost: st.IskLost}, nil
}

// killPulse lays out the 14-day ISK flow: one column a day, the
// day's destroyed value rising from the line and its lost value
// falling below it, both on one scale.
func killPulse(rows []killmailRow, now time.Time) *kmPulse {
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -(kmPulseDays - 1))
	days := make([]kmPulseDay, kmPulseDays)
	var dest, lost float64
	active := false
	for _, row := range rows {
		if row.Warming || row.At.Before(start) {
			continue
		}
		i := int(row.At.Sub(start).Hours() / 24)
		if i < 0 || i >= kmPulseDays {
			continue
		}
		days[i].HasActivity = true
		active = true
		if row.Loss {
			days[i].LostISK += row.ISK
			lost += row.ISK
		} else {
			days[i].Destroyed += row.ISK
			dest += row.ISK
		}
	}
	if !active {
		return nil
	}
	peak, peakI := 0.0, -1
	for i, d := range days {
		if d.Destroyed > peak {
			peak, peakI = d.Destroyed, i
		}
		if d.LostISK > peak {
			peak, peakI = d.LostISK, i
		}
	}
	p := &kmPulse{Destroyed: iskCompact(dest), Lost: iskCompact(lost), Width: kmPulseDays * kmPulseStep}
	for i := range days {
		d := &days[i]
		day := start.AddDate(0, 0, i)
		d.X = i*kmPulseStep + (kmPulseStep-kmPulseBar)/2
		d.Label = day.Format("Jan 2")
		d.Tick = i == 0 || i == kmPulseDays-1 || i == kmPulseDays/2
		if peak > 0 {
			d.KillH = scaleBar(d.Destroyed, peak)
			d.LossH = scaleBar(d.LostISK, peak)
		}
		d.KillY = kmPulseBase - d.KillH
		d.Title = fmt.Sprintf("%s: %s ISK destroyed, %s ISK lost", d.Label, iskCompact(d.Destroyed), iskCompact(d.LostISK))
	}
	if peakI >= 0 {
		p.Peak = days[peakI].Label
	}
	p.Days = days
	return p
}

// scaleBar is a bar's height for v on a scale topping out at max; a
// day with any value keeps a visible sliver.
func scaleBar(v, max float64) int {
	if v <= 0 {
		return 0
	}
	h := int(math.Round(v / max * kmPulseHalf))
	if h < 2 {
		h = 2
	}
	return h
}
