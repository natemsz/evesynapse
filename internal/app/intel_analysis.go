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
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/intelanalyze"
	"evesynapse/internal/logging"
	"evesynapse/internal/scan"
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

// intelNameDict is the segmentation index over every pilot name
// the site has verified, loaded like the other dictionaries and
// kept a day; a name verified since (lookupIntelNames) marks it
// dirty and the next read rebuilds it.
type intelNameDict struct {
	mu     sync.Mutex
	index  *intelanalyze.NameIndex
	loaded time.Time
	dirty  bool
}

var intelNameDicts sync.Map // *Application -> *intelNameDict

func (app *Application) intelNameIndex(ctx context.Context) *intelanalyze.NameIndex {
	v, _ := intelNameDicts.LoadOrStore(app, &intelNameDict{})
	d := v.(*intelNameDict)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.index == nil || d.dirty || time.Since(d.loaded) > 24*time.Hour {
		if rows, err := app.queries.ListIntelPilotNames(ctx); err == nil && len(rows) > 0 {
			pilots := make([]intelanalyze.Pilot, len(rows))
			for i, r := range rows {
				pilots[i] = intelanalyze.Pilot{ID: r.CharacterID, Name: r.Name}
			}
			d.index = intelanalyze.NewNameIndex(pilots)
			d.loaded = time.Now()
			d.dirty = false
		}
	}
	return d.index
}

func (app *Application) intelNamesChanged() {
	if v, ok := intelNameDicts.Load(app); ok {
		d := v.(*intelNameDict)
		d.mu.Lock()
		d.dirty = true
		d.mu.Unlock()
	}
}

// taggedCoverage is how much of a text's letters and digits the
// plain reading's pilot tags account for: the length of each
// tagged pilot's name that appears in the text. A fragment tag
// ("Ice" read out of "RED ICE") accounts only for its fragment,
// which is how a roster re-read knows it explains the text
// better than the plain reading did.
func taggedCoverage(text string, pilots []intelanalyze.Pilot) int {
	lower := strings.ToLower(text)
	n := 0
	for _, p := range pilots {
		if !strings.Contains(lower, strings.ToLower(p.Name)) {
			continue
		}
		for _, r := range p.Name {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				n++
			}
		}
	}
	return n
}

// rosterRead is a report re-read as the local roster its text
// holds: the analysis the report carries, the reading (stored as
// its scan, the raw text kept as the scan's paste), and the
// interpreted sentence that becomes its headline.
type rosterRead struct {
	analysis intelanalyze.Analysis
	result   scanResult
	sentence string
}

// rosterReRead tries the roster reading of a report the plain
// analysis could not make sense of: the text segmented against
// the verified pilot names (intelanalyze.Segment), and only when
// the segmentation clears the roster bar and accounts for more
// of the text than the plain reading's tags do -- a roster the
// plain reading already reads whole is left exactly as it is. The reading itself is the scan
// tool's local reading of the segmented names, so a re-read
// report is built exactly as a pasted roster is: fresh
// affiliations, the channel's standing split, the roster
// sentence. Nil when the text is not a roster the site can read.
func (app *Application) rosterReRead(ctx context.Context, channelID int64, in intelanalyze.Input, plain intelanalyze.Analysis, posterID int64) *rosterRead {
	if len(plain.Ships) > 0 || len(strings.Fields(in.Text)) < 6 {
		return nil
	}
	ix := app.intelNameIndex(ctx)
	if ix == nil {
		return nil
	}
	seg, covered, total := ix.Segment(in.Text)
	if !intelanalyze.PassesRosterBar(seg, covered, total) || covered <= taggedCoverage(in.Text, plain.Pilots) {
		return nil
	}
	names := make([]string, len(seg))
	for i, p := range seg {
		names[i] = p.Name
	}
	lr := app.readLocal(ctx, names)
	if lr == nil || len(lr.Pilots) < intelanalyze.FusedMinPilots {
		return nil // EVE does not back the segmentation; leave the report as read
	}
	res := scanResult{Kind: scan.KindLocal, SystemID: in.System, Local: lr}
	sentence, split := app.scanReportText(ctx, channelID, res, nil, posterID)
	st := app.channelStandingOf(ctx, channelID)
	a := intelanalyze.Analysis{Kind: intelanalyze.KindLocal, Count: in.Hostiles}
	for _, lp := range lr.Pilots {
		a.Pilots = append(a.Pilots, intelanalyze.Pilot{ID: lp.ID, Name: lp.Name,
			Friendly: st.ofLocal(lp) == bucketFriendly, Red: st.watch[lp.ID]})
	}
	counts := intelanalyze.StandingCounts{Hostile: split[bucketHostile], Friendly: split[bucketFriendly],
		Neutral: split[bucketNeutral], Unclassified: split[bucketUnknown]}
	a.Counts = &counts
	return &rosterRead{analysis: a, result: res, sentence: sentence}
}

// storeRosterScan stores a roster reading as a scan, its raw
// text kept as the scan's paste, as scanReportOf stores a pasted
// scan's reading; it returns the scan's ID.
func (app *Application) storeRosterScan(ctx context.Context, userID int64, roster *rosterRead, raw string) (string, error) {
	body, err := json.Marshal(roster.result)
	if err != nil {
		return "", err
	}
	id, err := newScanID()
	if err != nil {
		return "", err
	}
	err = app.queries.InsertScan(ctx, db.InsertScanParams{ID: id, Kind: string(roster.result.Kind),
		SystemID: roster.result.SystemID, UserID: userID, Raw: raw, Result: string(body), CreatedAt: time.Now().UTC()})
	return id, err
}

// analyzeIntel works out what a report says, and whether anything in
// it is still to be looked up. A report carrying a scan reading
// (hasScan) is read as it stands; any other report whose text is a
// roster the plain reading cannot place -- the lines fused on the
// way in, or the names matched only in fragments -- is offered
// the roster re-read (rosterReRead), returned beside the plain
// analysis for the caller to apply.
func (app *Application) analyzeIntel(ctx context.Context, channelID int64, in intelanalyze.Input, posterID int64, hasScan bool) (intelanalyze.Analysis, *rosterRead, bool) {
	missing := map[int64]bool{}
	a := intelanalyze.Analyze(in, app.intelDict(ctx, channelID, missing))
	var roster *rosterRead
	if !hasScan && !in.Clear {
		roster = app.rosterReRead(ctx, channelID, in, a, posterID)
	}
	return a, roster, len(a.Unknown) > 0 || len(missing) > 0
}

// errIntelDuplicate is insertIntelReport's answer for a report
// the channel already holds: a roster whose pilot set already
// stands there (rosterDuplicate). Callers count it as a
// duplicate, not a failure, and the ID returned is the standing
// report's.
var errIntelDuplicate = errors.New("intel: report already in the channel")

// intelRosterDedupeWindow is how far apart two sightings of the
// one local roster may land and still be the same snapshot: a
// roster pictures a moment, and its re-sends -- an Axon restart,
// a second character logging the same channel, the form -- land
// within minutes of it, not half an hour later.
const intelRosterDedupeWindow = 30 * time.Minute

// pilotIDSet is a pilot list as a set of IDs, or nil below the
// roster bar: a set too small names nothing worth matching on.
func pilotIDSet(pilots []intelanalyze.Pilot) map[int64]bool {
	set := make(map[int64]bool, len(pilots))
	for _, p := range pilots {
		if p.ID > 0 {
			set[p.ID] = true
		}
	}
	if len(set) < intelanalyze.FusedMinPilots {
		return nil
	}
	return set
}

// rosterSetOf is a report's roster pilot set, from wherever its
// reading keeps it: a roster reading's analysis, its scan
// reading's own pilot list, or a plain reading whose pilots are
// (nearly) the whole text -- a roster that needed no re-read
// because every name was already known. The bar for the last is
// the roster bar itself: the tagged pilots must cover as much
// of the text as a segmentation would have to. Nil when the
// report is no roster the site can name a set for.
func (app *Application) rosterSetOf(ctx context.Context, text, analysisRaw, scanID string) map[int64]bool {
	var a intelanalyze.Analysis
	if analysisRaw != "" {
		_ = json.Unmarshal([]byte(analysisRaw), &a)
	}
	if a.Kind == intelanalyze.KindLocal {
		if set := pilotIDSet(a.Pilots); set != nil {
			return set
		}
	}
	if scanID != "" {
		if row, err := app.queries.GetScan(ctx, scanID); err == nil {
			var res scanResult
			if json.Unmarshal([]byte(row.Result), &res) == nil && res.Kind == scan.KindLocal && res.Local != nil {
				pilots := make([]intelanalyze.Pilot, len(res.Local.Pilots))
				for i, lp := range res.Local.Pilots {
					pilots[i] = intelanalyze.Pilot{ID: lp.ID}
				}
				if set := pilotIDSet(pilots); set != nil {
					return set
				}
			}
		}
	}
	if set := pilotIDSet(a.Pilots); set != nil && text != "" {
		if total := countAlnum(text); total > 0 && taggedCoverage(text, a.Pilots)*100 >= total*intelanalyze.FusedMinCoverPct {
			return set
		}
	}
	return nil
}

// countAlnum is a text's letters and digits: the denominator a
// coverage is measured against.
func countAlnum(s string) int {
	n := 0
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			n++
		}
	}
	return n
}

// rosterDuplicate is the ID of a report already standing on the
// channel for the system, seen within the dedupe window of
// seenAt either way, whose roster pilot set is this one -- or 0.
// excludeID is the report itself, when it already stands (the
// drain re-reading one).
func (app *Application) rosterDuplicate(ctx context.Context, channelID, systemID, excludeID int64, seenAt time.Time, set map[int64]bool) int64 {
	rows, err := app.queries.ListRecentRosterReports(ctx, db.ListRecentRosterReportsParams{
		ChannelID: channelID, SystemID: systemID, ExcludeID: excludeID,
		Since: seenAt.Add(-intelRosterDedupeWindow), Until: seenAt.Add(intelRosterDedupeWindow)})
	if err != nil {
		return 0
	}
	for _, r := range rows {
		other := app.rosterSetOf(ctx, r.Text, r.Analysis, r.ScanID)
		if other == nil || len(other) != len(set) {
			continue
		}
		same := true
		for id := range set {
			if !other[id] {
				same = false
				break
			}
		}
		if same {
			return r.ID
		}
	}
	return 0
}

// insertIntelReport stores a report with what it says. nv is "no
// visual", which the stored report keeps only in its analysis.
// counts, when given, are the standing split the report's own
// reading counted (a scan's synthesis): they are stored over the
// split the analysis of the headline derives, a reading's counts
// being the exact ones. A report read as a roster whose pilot
// set already stands on the channel is not stored again:
// errIntelDuplicate, with the standing report's ID.
func (app *Application) insertIntelReport(ctx context.Context, p db.InsertIntelReportParams, nv bool, counts *intelanalyze.StandingCounts) (int64, error) {
	a, roster, pending := app.analyzeIntel(ctx, p.ChannelID, intelanalyze.Input{System: p.SystemID, Text: p.Text, Hostiles: int(p.Hostiles), Clear: p.Clear, NoVisual: nv}, p.AuthorCharacterID, p.ScanID != "")
	if roster != nil {
		// The report is the roster its text holds: store the
		// reading as its scan (the raw text kept as the scan's
		// paste) and let the headline be the reading's sentence,
		// never the fused text itself. The re-read settles the
		// report; a failure to store it leaves the plain read.
		if id, err := app.storeRosterScan(ctx, p.AuthorUserID, roster, p.Text); err == nil {
			a = roster.analysis
			p.ScanID = id
			p.Text = roster.sentence
			p.Pilots = roster.sentence
			if p.Hostiles == 0 {
				p.Hostiles = scanHostiles(roster.result)
			}
			pending = false
			counts = nil // the reading's own split is in its analysis
		} else {
			logging.Warnf("intel: store roster reading: %v", err)
		}
	}
	if counts != nil && counts.Total() > 0 {
		a.Counts = counts
	}
	p.Kind = a.Kind
	if raw, err := json.Marshal(a); err == nil {
		p.Analysis = string(raw)
	}
	p.AnalysisPending = pending
	// A roster is recognised by its pilot set, not its text:
	// the same snapshot arriving fused, spaced and plain within
	// the window is one report, however it was written.
	if set := app.rosterSetOf(ctx, p.Text, p.Analysis, p.ScanID); set != nil {
		if dup := app.rosterDuplicate(ctx, p.ChannelID, p.SystemID, 0, p.SeenAt, set); dup > 0 {
			return dup, errIntelDuplicate
		}
	}
	return app.queries.InsertIntelReport(ctx, p)
}

// drainIntelAnalysis looks up, with EVE, the names and affiliations
// the newest pending reports are waiting on, in bulk, and analyses
// those reports again. Returns ESI's error-limit signal.
func (app *Application) drainIntelAnalysis(ctx context.Context) (limited bool) {
	now := time.Now().UTC()
	rows, err := app.queries.ListPendingIntelReports(ctx, db.ListPendingIntelReportsParams{Since: now.Add(-intelAnalysisWindow), MaxRows: intelAnalysisBatch})
	if err != nil {
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
		// A report no pilot could be read from may be a roster
		// whose lines fused on the way in: propose the names its
		// case boundaries give up, so the re-read at settle time
		// segments against a verified dictionary.
		if len(a.Pilots) == 0 {
			for _, cand := range intelanalyze.FusedCandidates(r.Text) {
				names[cand] = true
			}
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
		a, roster, _ := app.analyzeIntel(ctx, r.ChannelID, input(r), r.AuthorCharacterID, r.ScanID != "")
		// A scan report keeps the split its reading counted at
		// ingest: re-reading its headline sentence can only
		// approximate them.
		var prev intelanalyze.Analysis
		_ = json.Unmarshal([]byte(r.Analysis), &prev)
		if r.ScanID != "" && prev.Counts != nil {
			a.Counts = prev.Counts
		}
		// A report re-read as a roster is settled as one: its
		// reading stored as a scan, its headline the sentence.
		// Settled as a roster it is recognised by its set like
		// any roster at insert: if the roster already stands in
		// the channel -- it arrived spaced while this one waited
		// on its names -- this report goes away instead of
		// standing twice.
		if roster != nil {
			if set := pilotIDSet(roster.analysis.Pilots); set != nil &&
				app.rosterDuplicate(ctx, r.ChannelID, r.SystemID, r.ID, r.SeenAt, set) > 0 {
				if err := app.queries.DeleteIntelReport(ctx, r.ID); err != nil {
					logging.Warnf("intel: drop duplicate roster report %d: %v", r.ID, err)
				}
				continue
			}
			if err := app.settleRosterReport(ctx, r, roster); err == nil {
				continue
			} else {
				logging.Warnf("intel: settle roster report %d: %v", r.ID, err)
			}
		}
		raw, _ := json.Marshal(a)
		if err := app.queries.SetIntelReportAnalysis(ctx, db.SetIntelReportAnalysisParams{ID: r.ID, Kind: a.Kind, Analysis: string(raw), AnalysisPending: false}); err != nil {
			logging.Warnf("intel: save analysis of report %d: %v", r.ID, err)
		}
	}
	app.backfillStoredRosters(ctx, now)
	return false
}

// rosterOfferedFlag marks, in a stored report's analysis, that
// the roster re-read has been offered to it: the backfill below
// makes the offer once, and the candidate list holds back any
// report already carrying it.
const rosterOfferedFlag = "rr"

// backfillStoredRosters offers the roster re-read to settled
// reports that never had it. The re-read runs at ingest and in
// the pending drain above, so a report stored before it existed
// -- or settled by a reading that had no roster to give --
// stands forever as the prose its text was misread for, blob
// and all. A candidate is a settled report with no scan of its
// own, inside the retention window but older than the pending
// one (a fresh report is the ingest paths' business), and the
// pass takes the drain's usual batch. A report the re-read
// converts is settled as a roster exactly as a pending one is
// (settleRosterReport) and carries its scan from then on, so
// it is never a candidate again. A report whose roster already
// stands in the channel is left as it stands. Anything else
// keeps the fresh plain reading, flagged as offered so it is
// not offered again.
func (app *Application) backfillStoredRosters(ctx context.Context, now time.Time) {
	rows, err := app.queries.ListStoredRosterCandidates(ctx, db.ListStoredRosterCandidatesParams{
		Since: now.Add(-intelRetention), Until: now.Add(-intelAnalysisWindow), MaxRows: intelAnalysisBatch})
	if err != nil {
		logging.Warnf("intel: list stored roster candidates: %v", err)
		return
	}
	for _, c := range rows {
		r := db.ListPendingIntelReportsRow(c)
		var prev intelanalyze.Analysis
		_ = json.Unmarshal([]byte(r.Analysis), &prev)
		nv := false
		for _, f := range prev.Flags {
			nv = nv || f == "nv"
		}
		in := intelanalyze.Input{System: r.SystemID, Text: r.Text, Hostiles: int(r.Hostiles), Clear: r.Clear, NoVisual: nv}
		a, roster, _ := app.analyzeIntel(ctx, r.ChannelID, in, r.AuthorCharacterID, false)
		if roster != nil {
			if set := pilotIDSet(roster.analysis.Pilots); set != nil &&
				app.rosterDuplicate(ctx, r.ChannelID, r.SystemID, r.ID, r.SeenAt, set) > 0 {
				roster = nil // the roster already stands in the channel: leave this one as it is
			}
		}
		if roster != nil {
			if err := app.settleRosterReport(ctx, r, roster); err != nil {
				logging.Warnf("intel: settle stored roster report %d: %v", r.ID, err)
			}
			continue
		}
		offered := false
		for _, f := range a.Flags {
			offered = offered || f == rosterOfferedFlag
		}
		if !offered {
			a.Flags = append(a.Flags, rosterOfferedFlag)
			sort.Strings(a.Flags)
		}
		raw, _ := json.Marshal(a)
		if err := app.queries.SetIntelReportAnalysis(ctx, db.SetIntelReportAnalysisParams{ID: r.ID, Kind: a.Kind, Analysis: string(raw), AnalysisPending: false}); err != nil {
			logging.Warnf("intel: save analysis of stored report %d: %v", r.ID, err)
		}
	}
}

// settleRosterReport stores a pending report's roster re-read:
// the reading as its scan (the report's text to date kept as the
// scan's paste), the headline sentence in the text's place.
func (app *Application) settleRosterReport(ctx context.Context, r db.ListPendingIntelReportsRow, roster *rosterRead) error {
	id, err := app.storeRosterScan(ctx, r.AuthorUserID, roster, r.Text)
	if err != nil {
		return err
	}
	hostiles := r.Hostiles
	if hostiles == 0 {
		hostiles = scanHostiles(roster.result)
	}
	raw, err := json.Marshal(roster.analysis)
	if err != nil {
		return err
	}
	return app.queries.SetIntelReportRoster(ctx, db.SetIntelReportRosterParams{
		ID: r.ID, Text: roster.sentence, Pilots: roster.sentence, Hostiles: hostiles,
		ScanID: id, Kind: roster.analysis.Kind, Analysis: string(raw)})
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
		rv.GateID = a.Gate
	}
	// The stats bar: the standing split stored with the report.
	// Questions and chatter carry no split worth a bar, as they
	// carry no kind label.
	if a.Counts != nil && kind != intelanalyze.KindQuestion && kind != intelanalyze.KindChatter {
		rv.Split = standingSplit(a.Counts)
	}
}

// standingSplit turns stored counts into bar segments, in the
// order the site speaks of them, empty buckets left out.
func standingSplit(c *intelanalyze.StandingCounts) []reportSeg {
	total := c.Total()
	if total == 0 {
		return nil
	}
	all := []reportSeg{
		{Name: "Hostiles", N: c.Hostile, Class: "hostile"},
		{Name: "Friendly", N: c.Friendly, Class: "friendly"},
		{Name: "Neutral", N: c.Neutral, Class: "neutral"},
		{Name: "Unclassified", N: c.Unclassified, Class: "unclassified"},
	}
	out := make([]reportSeg, 0, len(all))
	for _, s := range all {
		if s.N > 0 {
			s.Pct = s.N * 100 / total
			out = append(out, s)
		}
	}
	return out
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
		if len(found) > 0 {
			app.intelNamesChanged()
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
