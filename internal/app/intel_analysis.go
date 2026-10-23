package app

// ---------------------------------------------------------------------------
// What intel reports say (internal/intelanalyze), worked out as each
// report is stored and stored with it (schema 083).
//
// A report is analysed against the site's own knowledge: ship names
// and classes from the SDE, gates from the system's neighbours, pilot
// names already looked up (intel_pilot_names), and each pilot's
// corporation and alliance (intel_pilot_affiliations, or the stored
// corporation of a linked character). A pilot is friendly by default
// when they are in a corporation or alliance of the channel's own
// side: its owner's characters, its recent reporters, and for a
// corporation's or alliance's channel that organisation.
//
// Names nobody has looked up yet, and pilots whose affiliation is not
// known, leave the report pending. The worker's short loop (every few
// seconds) looks them up with EVE in bulk and analyses those reports
// again, so a pasted report is fully understood within seconds without
// a scout waiting on EVE to post it.
// ---------------------------------------------------------------------------

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/intelanalyze"
	"evesynapse/internal/logging"
)

const (
	intelAnalysisWindow  = 30 * time.Minute // reports older than this are left as they are
	intelAnalysisBatch   = 40
	intelSideWindow      = 7 * 24 * time.Hour
	intelAffiliationLife = 24 * time.Hour
)

type intelShip struct {
	id          int64
	name, class string
}

type intelShipDict struct {
	mu     sync.Mutex
	ships  map[string]intelShip
	loaded time.Time
}

var intelShipDicts sync.Map // *Application -> *intelShipDict

// intelShips is every ship by lower-cased name, loaded from the SDE
// and kept a day.
func (app *Application) intelShips(ctx context.Context) map[string]intelShip {
	v, _ := intelShipDicts.LoadOrStore(app, &intelShipDict{})
	d := v.(*intelShipDict)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ships == nil || time.Since(d.loaded) > 24*time.Hour {
		if rows, err := app.queries.ListSDEShipNames(ctx); err == nil && len(rows) > 0 {
			d.ships = make(map[string]intelShip, len(rows))
			for _, r := range rows {
				d.ships[strings.ToLower(r.Name)] = intelShip{id: r.TypeID, name: r.Name, class: intelanalyze.ClassOfGroup(r.GroupName)}
			}
			d.loaded = time.Now()
		}
	}
	return d.ships
}

type intelType struct {
	id   int64
	name string
}

type intelTypeDict struct {
	mu     sync.Mutex
	types  map[string]intelType
	loaded time.Time
}

var intelTypeDicts sync.Map // *Application -> *intelTypeDict

// intelTypes is every type by lower-cased name, loaded from the
// SDE and kept a day, as intelShips loads the ships: a report's
// exact type names are the things named, not pilots. Rows arrive
// published first, so a name shared by two types keeps the
// published one's ID.
func (app *Application) intelTypes(ctx context.Context) map[string]intelType {
	v, _ := intelTypeDicts.LoadOrStore(app, &intelTypeDict{})
	d := v.(*intelTypeDict)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.types == nil || time.Since(d.loaded) > 24*time.Hour {
		if rows, err := app.queries.ListSDETypeNames(ctx); err == nil && len(rows) > 0 {
			d.types = make(map[string]intelType, len(rows))
			for _, r := range rows {
				l := strings.ToLower(r.Name)
				if _, dup := d.types[l]; !dup {
					d.types[l] = intelType{id: r.TypeID, name: r.Name}
				}
			}
			d.loaded = time.Now()
		}
	}
	return d.types
}

// intelSide is a channel's own side: the corporations and alliances
// whose pilots count as friendly in its reports.
type intelSide struct {
	corps, alliances map[int64]bool
}

func (app *Application) intelSideOf(ctx context.Context, channelID int64) intelSide {
	side := intelSide{corps: map[int64]bool{}, alliances: map[int64]bool{}}
	corps, err := app.queries.ListIntelChannelSideCorps(ctx, db.ListIntelChannelSideCorpsParams{ChannelID: channelID, Since: time.Now().Add(-intelSideWindow)})
	if err != nil {
		logging.Warnf("intel: side of channel %d: %v", channelID, err)
	}
	for _, c := range corps {
		side.corps[c] = true
		if a, _ := app.corpAlliance(ctx, c); a > 0 {
			side.alliances[a] = true
		}
	}
	// A corporation's or alliance's channel (channels_orgs.go) is on
	// its owner's side.
	if ch, err := app.queries.GetIntelChannel(ctx, channelID); err == nil {
		switch ch.OwnerKind {
		case ownerCorporation:
			side.corps[ch.OwnerID] = true
			if a, _ := app.corpAlliance(ctx, ch.OwnerID); a > 0 {
				side.alliances[a] = true
			}
		case ownerAlliance:
			side.alliances[ch.OwnerID] = true
		}
	}
	return side
}

// pilotAffiliation is a pilot's corporation and alliance, from a
// linked character's stored corporation or the looked-up affiliation;
// ok is false when neither is known (or the lookup is a day old).
func (app *Application) pilotAffiliation(ctx context.Context, id int64) (corp, alliance int64, ok bool) {
	if cc, err := app.queries.GetCharacterCorporation(ctx, id); err == nil && cc.CorporationID > 0 {
		a, _ := app.corpAlliance(ctx, cc.CorporationID)
		return cc.CorporationID, a, true
	}
	if af, err := app.queries.GetIntelPilotAffiliation(ctx, id); err == nil && time.Since(af.CheckedAt) < intelAffiliationLife {
		return af.CorporationID, af.AllianceID, true
	}
	return 0, 0, false
}

// intelDict is the analyser's dictionaries for one channel's reports.
// missing collects the pilots whose affiliation is not known.
func (app *Application) intelDict(ctx context.Context, channelID int64, missing map[int64]bool) intelanalyze.Dict {
	ships := app.intelShips(ctx)
	types := app.intelTypes(ctx)
	systems := app.systemLookup(ctx)
	side := app.intelSideOf(ctx, channelID)
	// The channel's known hostiles are its set standing: a pilot
	// on the watch list reads red whatever their affiliation says.
	watch := map[int64]bool{}
	if rows, err := app.queries.ListIntelWatch(ctx, []int64{channelID}); err == nil {
		for _, r := range rows {
			watch[r.CharacterID] = true
		}
	}
	neighbours := map[int64]map[int64]bool{}
	pilots := map[string][3]any{}
	return intelanalyze.Dict{
		Ship: func(l string) (int64, string, string, bool) {
			s, ok := ships[l]
			return s.id, s.name, s.class, ok
		},
		Type: func(l string) (int64, string, bool) {
			t, ok := types[l]
			return t.id, t.name, ok
		},
		Pilot: func(l string) (int64, string, bool, bool) {
			if p, ok := pilots[l]; ok {
				return p[0].(int64), p[1].(string), p[0].(int64) > 0, p[2].(bool)
			}
			row, err := app.queries.GetIntelPilotName(ctx, l)
			known := err == nil
			pilots[l] = [3]any{row.CharacterID, row.Name, known}
			return row.CharacterID, row.Name, known && row.CharacterID > 0, known
		},
		Gate: func(sys int64, l string) (int64, bool) {
			id, _, ok := systems(l)
			if !ok || id == sys {
				return 0, false
			}
			if neighbours[sys] == nil {
				neighbours[sys] = map[int64]bool{}
				if rows, err := app.queries.ListSDESystemNeighbours(ctx, sys); err == nil {
					for _, n := range rows {
						neighbours[sys][n] = true
					}
				}
			}
			return id, neighbours[sys][id]
		},
		Friend: func(id int64) (bool, bool) {
			corp, alliance, ok := app.pilotAffiliation(ctx, id)
			if !ok {
				if missing != nil {
					missing[id] = true
				}
				return false, watch[id]
			}
			return side.corps[corp] || (alliance > 0 && side.alliances[alliance]), watch[id]
		},
	}
}

// analyzeIntel works out what a report says, and whether anything in
// it is still to be looked up.
func (app *Application) analyzeIntel(ctx context.Context, channelID int64, in intelanalyze.Input) (intelanalyze.Analysis, bool) {
	missing := map[int64]bool{}
	a := intelanalyze.Analyze(in, app.intelDict(ctx, channelID, missing))
	return a, len(a.Unknown) > 0 || len(missing) > 0
}

// insertIntelReport stores a report with what it says. nv is "no
// visual", which the stored report keeps only in its analysis.
func (app *Application) insertIntelReport(ctx context.Context, p db.InsertIntelReportParams, nv bool) (int64, error) {
	a, pending := app.analyzeIntel(ctx, p.ChannelID, intelanalyze.Input{System: p.SystemID, Text: p.Text, Hostiles: int(p.Hostiles), Clear: p.Clear, NoVisual: nv})
	p.Kind = a.Kind
	if raw, err := json.Marshal(a); err == nil {
		p.Analysis = string(raw)
	}
	p.AnalysisPending = pending
	return app.queries.InsertIntelReport(ctx, p)
}

// drainIntelAnalysis looks up, with EVE, the names and affiliations
// the newest pending reports are waiting on, in bulk, and analyses
// those reports again. Returns ESI's error-limit signal.
func (app *Application) drainIntelAnalysis(ctx context.Context) (limited bool) {
	now := time.Now().UTC()
	rows, err := app.queries.ListPendingIntelReports(ctx, db.ListPendingIntelReportsParams{Since: now.Add(-intelAnalysisWindow), MaxRows: intelAnalysisBatch})
	if err != nil || len(rows) == 0 {
		return false
	}
	input := func(r db.ListPendingIntelReportsRow) intelanalyze.Input {
		var prev intelanalyze.Analysis
		_ = json.Unmarshal([]byte(r.Analysis), &prev)
		nv := false
		for _, f := range prev.Flags {
			nv = nv || f == "nv"
		}
		return intelanalyze.Input{System: r.SystemID, Text: r.Text, Hostiles: int(r.Hostiles), Clear: r.Clear, NoVisual: nv}
	}
	// What the reports are waiting on.
	names, pilots := map[string]bool{}, map[int64]bool{}
	for _, r := range rows {
		missing := map[int64]bool{}
		a := intelanalyze.Analyze(input(r), app.intelDict(ctx, r.ChannelID, missing))
		for _, n := range a.Unknown {
			names[n] = true
		}
		for id := range missing {
			pilots[id] = true
		}
	}
	if len(names) > 0 {
		if limited = app.lookupIntelNames(ctx, names, now); limited {
			return true
		}
		// The pilots just found need their affiliation too.
		for n := range names {
			if row, err := app.queries.GetIntelPilotName(ctx, n); err == nil && row.CharacterID > 0 {
				if _, _, ok := app.pilotAffiliation(ctx, row.CharacterID); !ok {
					pilots[row.CharacterID] = true
				}
			}
		}
	}
	if len(pilots) > 0 {
		if limited = app.lookupIntelAffiliations(ctx, pilots, now); limited {
			return true
		}
	}
	// Analyse again with what was found. A report is settled now
	// either way: what EVE does not know is not asked again.
	for _, r := range rows {
		a, _ := app.analyzeIntel(ctx, r.ChannelID, input(r))
		raw, _ := json.Marshal(a)
		if err := app.queries.SetIntelReportAnalysis(ctx, db.SetIntelReportAnalysisParams{ID: r.ID, Kind: a.Kind, Analysis: string(raw), AnalysisPending: false}); err != nil {
			logging.Warnf("intel: save analysis of report %d: %v", r.ID, err)
		}
	}
	return false
}

// --- Showing it ---

type reportPilot struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Friendly bool   `json:"friendly,omitempty"`
}

type reportShip struct {
	ID   int64  `json:"id,omitempty"`
	Name string `json:"name"`
	N    int    `json:"n,omitempty"`
}

// intelKindLabels says each kind of report in words; chatter and
// questions get no label.
var intelKindLabels = map[string]string{
	intelanalyze.KindCapital:  "Capitals or cyno",
	intelanalyze.KindCamp:     "Gate camp",
	intelanalyze.KindBubble:   "Bubble",
	intelanalyze.KindFleet:    "Fleet",
	intelanalyze.KindSighting: "Hostiles",
	intelanalyze.KindLocal:    "In Local",
	intelanalyze.KindFriendly: "Friendly",
	intelanalyze.KindClear:    "Clear",
}

// applyAnalysis puts what a stored report says on its view.
func applyAnalysis(rv *reportView, kind, raw string, names *killNames) {
	rv.Kind = kind
	rv.KindLabel = intelKindLabels[kind]
	switch kind {
	case intelanalyze.KindClear, intelanalyze.KindFriendly:
		rv.KindTone = "kill"
	case intelanalyze.KindLocal:
		rv.KindTone = "wait"
	default:
		rv.KindTone = "loss"
	}
	if raw == "" {
		return
	}
	var a intelanalyze.Analysis
	if json.Unmarshal([]byte(raw), &a) != nil {
		return
	}
	for _, p := range a.Pilots {
		rv.Pilots = append(rv.Pilots, reportPilot{ID: p.ID, Name: p.Name, Friendly: p.Friendly && !p.Red})
	}
	for _, s := range a.Ships {
		rv.Ships = append(rv.Ships, reportShip{ID: s.TypeID, Name: s.Name, N: s.N})
	}
	if a.Gate > 0 {
		rv.Gate = names.system(a.Gate).Name
	}
}

// lookupIntelNames asks EVE which of the names are pilots (exact
// names, in bulk) and keeps the answers, "no such pilot" included.
func (app *Application) lookupIntelNames(ctx context.Context, names map[string]bool, now time.Time) (limited bool) {
	list := make([]string, 0, len(names))
	for n := range names {
		list = append(list, n)
	}
	for start := 0; start < len(list); start += 500 {
		end := min(start+500, len(list))
		var ids struct {
			Characters []struct {
				ID   int64  `json:"id"`
				Name string `json:"name"`
			} `json:"characters"`
		}
		if err := app.esi.PostJSON(ctx, "/universe/ids/", list[start:end], &ids); err != nil {
			logging.Warnf("intel: look up %d names: %v", end-start, err)
			return app.esi.HardLimitActive(time.Now())
		}
		found := map[string]bool{}
		for _, c := range ids.Characters {
			l := strings.ToLower(c.Name)
			found[l] = true
			app.esi.StoreCharacterName(c.ID, c.Name)
			_ = app.queries.UpsertIntelPilotName(ctx, db.UpsertIntelPilotNameParams{NameLower: l, CharacterID: c.ID, Name: c.Name, CheckedAt: now})
		}
		for _, n := range list[start:end] {
			if !found[n] {
				_ = app.queries.UpsertIntelPilotName(ctx, db.UpsertIntelPilotNameParams{NameLower: n, CheckedAt: now})
			}
		}
	}
	return false
}

// lookupIntelAffiliations asks EVE for the pilots' corporations and
// alliances, in bulk.
func (app *Application) lookupIntelAffiliations(ctx context.Context, pilots map[int64]bool, now time.Time) (limited bool) {
	list := make([]int64, 0, len(pilots))
	for id := range pilots {
		list = append(list, id)
	}
	for start := 0; start < len(list); start += 1000 {
		end := min(start+1000, len(list))
		var rows []struct {
			CharacterID   int64 `json:"character_id"`
			CorporationID int64 `json:"corporation_id"`
			AllianceID    int64 `json:"alliance_id"`
		}
		if err := app.esi.PostJSON(ctx, "/characters/affiliation/", list[start:end], &rows); err != nil {
			logging.Warnf("intel: affiliation of %d pilots: %v", end-start, err)
			return app.esi.HardLimitActive(time.Now())
		}
		for _, r := range rows {
			_ = app.queries.UpsertIntelPilotAffiliation(ctx, db.UpsertIntelPilotAffiliationParams{CharacterID: r.CharacterID, CorporationID: r.CorporationID, AllianceID: r.AllianceID, CheckedAt: now})
		}
	}
	return false
}
