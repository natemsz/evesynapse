package app

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// The Mining Ledger page: the mining ledgers of every character in
// scope (each character's stored ledger snapshot, the last 30 days
// ESI keeps), with per-pilot totals, the volume mined (units times
// the unit volume from the static data) and an estimated
// value through the house price order (AppraiseLines), labelled
// when a price comes from the outside fallback.

type miningPilot struct {
	ID       int64
	Name     string
	Volume   string // "1.84M m³"
	Value    string // "486M"
	Systems  int
	SharePct string // volume against the busiest pilot, 0 to 100
	Href     string
	On       bool
	Image    string
}

type miningEntryRow struct {
	Key     string
	Date    string // "Nov 2"
	DateISO string
	Ore     string
	TypeID  int64
	Icon    string
	Qty     string
	Volume  string
	Value   string // "" when unpriced
	System  placeRef
	Pilot   string
	PilotID int64
	Href    string
	On      bool
}

type miningInspector struct {
	Row         miningEntryRow
	PilotVolume string
	PilotValue  string
	PilotSys    int
	CloseHref   string
}

type miningView struct {
	Selector  assetScopeSelector
	Notes     []string
	Warming   []string
	Tiles     []jobTile
	Pilots    []miningPilot
	AllHref   string
	AllOn     bool
	DirHref   string
	Rows      []miningEntryRow
	Cut       int
	ListTitle string
	PriceNote string
	Depth     int
	Insp      *miningInspector
	Any       bool
	Multi     bool // more than one pilot in scope
	ShowPilot bool // the list names each entry's pilot (several pilots, none picked)
}

type miningQuery struct {
	scope charScope
	pilot int64
	entry string
}

func (q miningQuery) href(mut map[string]string) string {
	v := url.Values{}
	v.Set("list", "1")
	v.Set("scope", q.scope.param())
	if q.pilot != 0 {
		v.Set("pilot", strconv.FormatInt(q.pilot, 10))
	}
	v.Set("entry", q.entry)
	for k, val := range mut {
		v.Set(k, val)
	}
	return withQuery("/industry/mining/", v)
}

type minedEntry struct {
	esi.MiningEntry
	owner db.Character
}

func miningKey(e minedEntry) string {
	return strconv.FormatInt(e.owner.CharacterID, 10) + "-" + e.Date + "-" + strconv.FormatInt(e.TypeID, 10) + "-" + strconv.FormatInt(e.SolarSystemID, 10)
}

func (app *Application) buildMiningView(ctx context.Context, r *http.Request, characters []db.Character) *miningView {
	q := r.URL.Query()
	mq := miningQuery{scope: parseCharScope(q.Get("scope"), characters), entry: q.Get("entry")}
	mq.pilot, _ = strconv.ParseInt(q.Get("pilot"), 10, 64)

	view := &miningView{}
	view.Selector = buildScopeSelector(characters, mq.scope, func(scope string) string {
		return miningQuery{scope: parseCharScope(scope, characters)}.href(nil)
	})

	var all []minedEntry
	var scoped []db.Character
	for _, ch := range characters {
		if !mq.scope.has(ch.CharacterID) {
			continue
		}
		scoped = append(scoped, ch)
		var ledger esi.MiningLedger
		if st := app.econSection(ctx, ch.CharacterID, esi.SnapMining, &ledger); !st.Loaded {
			if st.Note != "" {
				view.Notes = append(view.Notes, ch.Name+": "+st.Note)
			} else {
				view.Warming = append(view.Warming, ch.Name)
			}
		}
		for _, e := range ledger {
			all = append(all, minedEntry{MiningEntry: e, owner: ch})
		}
	}
	view.Multi = len(scoped) > 1
	view.ShowPilot = view.Multi && mq.pilot == 0
	view.Any = len(all) > 0

	// Ore volumes (static data) and prices (house order), once per ore.
	types := map[int64]bool{}
	for _, e := range all {
		types[e.TypeID] = true
	}
	volume := app.typeVolumes(ctx, types)
	price, fallbackAt := map[int64]float64{}, time.Time{}
	if len(types) > 0 {
		lines := make([]module.AppraisalLine, 0, len(types))
		for id := range types {
			lines = append(lines, module.AppraisalLine{TypeID: id, Quantity: 1, Included: true})
		}
		if res, err := app.AppraiseLines(ctx, lines); err == nil {
			for _, l := range res.Lines {
				if !l.Priced {
					continue
				}
				price[l.TypeID] = l.UnitPrice
				if l.Tier == module.AppraisalTierFallback && (fallbackAt.IsZero() || l.AsOf.Before(fallbackAt)) {
					fallbackAt = l.AsOf
				}
			}
		}
	}
	unpriced := 0
	for id := range types {
		if _, ok := price[id]; !ok {
			unpriced++
		}
	}
	switch {
	case !fallbackAt.IsZero() && unpriced > 0:
		view.PriceNote = "Values are estimates at EveSynapse's own prices; some ores use the outside fallback (as of " + fallbackAt.UTC().Format("Jan 2, 15:04") + " UTC), and " + strconv.Itoa(unpriced) + " have no price yet."
	case !fallbackAt.IsZero():
		view.PriceNote = "Values are estimates at EveSynapse's own prices; some ores use the outside fallback (as of " + fallbackAt.UTC().Format("Jan 2, 15:04") + " UTC)."
	case unpriced > 0:
		view.PriceNote = "Values are estimates at EveSynapse's own prices; " + strconv.Itoa(unpriced) + " ores have no price yet and count as nothing."
	default:
		view.PriceNote = "Values are estimates at EveSynapse's own prices."
	}

	// Per-pilot and overall totals.
	type tot struct {
		vol, val float64
		sys      map[int64]bool
	}
	per := map[int64]*tot{}
	overall := &tot{sys: map[int64]bool{}}
	for _, e := range all {
		t := per[e.owner.CharacterID]
		if t == nil {
			t = &tot{sys: map[int64]bool{}}
			per[e.owner.CharacterID] = t
		}
		v := float64(e.Quantity) * volume[e.TypeID]
		p := float64(e.Quantity) * price[e.TypeID]
		t.vol += v
		t.val += p
		t.sys[e.SolarSystemID] = true
		overall.vol += v
		overall.val += p
		overall.sys[e.SolarSystemID] = true
	}
	view.Tiles = []jobTile{
		{Label: "Mined, 30 days", Value: iskCompact(overall.vol) + " m³", Sub: "volume of ore mined"},
		{Label: "Estimated value, 30 days", Value: iskCompact(overall.val) + " ISK", Sub: "at EveSynapse's own prices"},
		{Label: "Systems", Value: strconv.Itoa(len(overall.sys)), Sub: "mined in over 30 days"},
	}
	maxVol := 0.0
	for _, t := range per {
		if t.vol > maxVol {
			maxVol = t.vol
		}
	}
	for _, ch := range scoped {
		t := per[ch.CharacterID]
		p := miningPilot{ID: ch.CharacterID, Name: ch.Name, Image: portraitURL(ch.CharacterID, 64), On: mq.pilot == ch.CharacterID,
			Href: mq.href(map[string]string{"pilot": strconv.FormatInt(ch.CharacterID, 10), "entry": ""})}
		p.Volume, p.Value, p.SharePct = "0 m³", "0", "0"
		if t != nil {
			p.Volume = iskCompact(t.vol) + " m³"
			p.Value = iskCompact(t.val)
			p.Systems = len(t.sys)
			if maxVol > 0 {
				p.SharePct = strconv.Itoa(int(t.vol / maxVol * 100))
			}
		}
		view.Pilots = append(view.Pilots, p)
	}
	sort.SliceStable(view.Pilots, func(a, b int) bool {
		ta, tb := per[view.Pilots[a].ID], per[view.Pilots[b].ID]
		va, vb := 0.0, 0.0
		if ta != nil {
			va = ta.vol
		}
		if tb != nil {
			vb = tb.vol
		}
		return va > vb
	})
	view.AllOn = mq.pilot == 0
	view.AllHref = mq.href(map[string]string{"pilot": "", "entry": ""})
	view.DirHref = mq.href(map[string]string{"list": "", "pilot": "", "entry": ""})

	// The ledger: newest day first.
	var picked []minedEntry
	for _, e := range all {
		if mq.pilot == 0 || e.owner.CharacterID == mq.pilot {
			picked = append(picked, e)
		}
	}
	sort.SliceStable(picked, func(a, b int) bool {
		if picked[a].Date != picked[b].Date {
			return picked[a].Date > picked[b].Date
		}
		return picked[a].Quantity > picked[b].Quantity
	})
	if len(picked) > maxIndustryRows {
		view.Cut = len(picked) - maxIndustryRows
		picked = picked[:maxIndustryRows]
	}
	sysMemo := map[int64]placeRef{}
	for _, e := range picked {
		row := app.miningEntryRow(ctx, e, volume[e.TypeID], price, sysMemo)
		row.Href = mq.href(map[string]string{"entry": row.Key})
		row.On = row.Key == mq.entry
		view.Rows = append(view.Rows, row)
	}
	view.ListTitle = "Ledger"
	for _, p := range view.Pilots {
		if p.ID == mq.pilot {
			view.ListTitle = "Ledger: " + p.Name
		}
	}

	view.Depth = 1
	if q.Get("list") != "1" && mq.pilot == 0 {
		view.Depth = 0
	}
	if mq.entry != "" {
		for _, e := range all {
			if miningKey(e) != mq.entry {
				continue
			}
			row := app.miningEntryRow(ctx, e, volume[e.TypeID], price, sysMemo)
			in := &miningInspector{Row: row, CloseHref: mq.href(map[string]string{"entry": ""})}
			if t := per[e.owner.CharacterID]; t != nil {
				in.PilotVolume = iskCompact(t.vol) + " m³"
				in.PilotValue = iskCompact(t.val) + " ISK"
				in.PilotSys = len(t.sys)
			}
			view.Insp = in
			view.Depth = 2
			break
		}
	}
	return view
}

func (app *Application) miningEntryRow(ctx context.Context, e minedEntry, unitVol float64, price map[int64]float64, sysMemo map[int64]placeRef) miningEntryRow {
	row := miningEntryRow{
		Key:     miningKey(e),
		DateISO: e.Date,
		Date:    e.Date,
		Ore:     app.typeNameOrID(ctx, e.TypeID),
		TypeID:  e.TypeID,
		Icon:    "https://images.evetech.net/types/" + strconv.FormatInt(e.TypeID, 10) + "/icon?size=64",
		Qty:     esi.FormatInt(e.Quantity),
		Volume:  iskCompact(float64(e.Quantity)*unitVol) + " m³",
		Pilot:   e.owner.Name,
		PilotID: e.owner.CharacterID,
	}
	if t, err := time.Parse("2006-01-02", e.Date); err == nil {
		row.Date = t.Format("Jan 2")
	}
	if p, ok := price[e.TypeID]; ok {
		row.Value = iskCompact(float64(e.Quantity)*p) + " ISK"
	}
	sys, ok := sysMemo[e.SolarSystemID]
	if !ok {
		sys = app.linkPlace(ctx, e.SolarSystemID, app.locationTitle(ctx, e.SolarSystemID, "solar_system"))
		sysMemo[e.SolarSystemID] = sys
	}
	row.System = sys
	return row
}

// typeVolumes reads the packaged volume per unit of each type from
// the static data, in one bounded query.
func (app *Application) typeVolumes(ctx context.Context, types map[int64]bool) map[int64]float64 {
	out := make(map[int64]float64, len(types))
	if len(types) == 0 || app.db == nil {
		return out
	}
	var b strings.Builder
	b.WriteString("SELECT type_id, volume FROM sde_type_physics WHERE type_id IN (")
	args := make([]any, 0, len(types))
	for id := range types {
		if len(args) > 0 {
			b.WriteString(", ")
		}
		args = append(args, id)
		b.WriteString("$" + strconv.Itoa(len(args)))
	}
	b.WriteString(")")
	rows, err := app.db.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var v float64
		if rows.Scan(&id, &v) == nil {
			out[id] = v
		}
	}
	return out
}
