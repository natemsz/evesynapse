package app

// ---------------------------------------------------------------------------
// Channel report groups and their composite picture.
//
// The scan tool has grouped its pastes since schema 086: one
// sitting's scans hang off the founding scan's id. Channel
// reports carry the same machinery (schema 087): a report joins
// the standing group for its channel and system when the
// group's latest activity is inside intelGroupWindow of the
// report's own moment, and every report that founds or joins a
// group has the group's composite computed and stored at
// ingest. The composite is the fused picture of the member
// readings: the newest d-scan governs the hulls, locals merge
// as the union of their pilots, fleet rows stand as fact
// pilot-to-ship links, and what is left is attributed by
// elimination only as likely, with its basis written down.
//
// Renders read the stored composite whole and never recompute
// it: the workbench shows it when the opened report's group has
// more than one member, and the feed and channel cards take
// their headline and counts from it. Grouping lives in the
// shared insert path, so the web form, the chat lines, Axon
// posts and the scan tool's own postings all group alike.
// ---------------------------------------------------------------------------

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/intelanalyze"
	"evesynapse/internal/logging"
	"evesynapse/internal/scan"
)

// intelGroupWindow is how far apart a channel's reports on one
// system may land and still read as one engagement: a new
// report joins the standing group when the group's latest
// activity is within the window of the report's own moment,
// either way (chat lines arrive out of order, so the window
// runs both directions, as the roster dedupe window does).
// Two hours is the starting value; it tunes from traffic.
const intelGroupWindow = 2 * time.Hour

// Bounds on the grouping reads at ingest: the candidate
// reports weighed for a group, and the members one composite
// is computed from (the newest, whose readings govern).
const (
	intelGroupCandidateCap = 100
	intelGroupMemberCap    = 50
	// intelGroupPilotCap bounds the pilot ids one member's
	// provenance entry carries: rail deltas count set
	// differences, and a Jita-sized local would otherwise
	// bloat every composite it joins.
	intelGroupPilotCap = 400
)

// --- The composite -----------------------------------------------------------

// compMember is one member report's provenance in a composite:
// what it was, when, who posted it, and the tallies a rail
// delta is counted from.
type compMember struct {
	ReportID int64             `json:"report"`
	Kind     string            `json:"kind"` // the rail's label: D-Scan, Local, a report kind
	ScanKind string            `json:"scan_kind,omitempty"`
	Reporter string            `json:"by,omitempty"`
	SeenAt   time.Time         `json:"at"`
	Tallies  map[string][2]int `json:"tallies,omitempty"` // hull name -> {count, on grid}
	PilotIDs []int64           `json:"pilots,omitempty"`
	NHulls   int               `json:"hulls,omitempty"`
}

// compLink is a fact pilot-to-ship link: a fleet composition
// row stated the two together. From is the report that stated
// it.
type compLink struct {
	PilotID   int64  `json:"pilot,omitempty"`
	PilotName string `json:"pilot_name"`
	ShipName  string `json:"ship"`
	TypeID    int64  `json:"type,omitempty"`
	From      int64  `json:"from"`
}

// compLikely is a pilot-to-ship attribution stored as
// likely, never as fact: from elimination in groupLikelyLinks
// (the only possibility left, so Prob is unset) or from a
// pilot's activity profile in groupProfileLikely
// (intel_gangs.go: Prob carries the attribution's share of
// the pilot's record). The basis is written out either way.
type compLikely struct {
	PilotID   int64   `json:"pilot,omitempty"`
	PilotName string  `json:"pilot_name"`
	ShipName  string  `json:"ship"`
	TypeID    int64   `json:"type,omitempty"`
	Prob      float64 `json:"prob,omitempty"`
	Basis     string  `json:"basis"`
}

// intelComposite is a group's fused picture, stored as JSON on
// intel_group_composites. Every element keeps its provenance
// in Members; the faces below are the newest reading of each
// kind, reconciled as the ingest rules say.
type intelComposite struct {
	GroupID    int64                        `json:"group"`
	ChannelID  int64                        `json:"channel"`
	SystemID   int64                        `json:"system"`
	Headline   string                       `json:"headline,omitempty"`
	ComputedAt time.Time                    `json:"computed_at"`
	LatestAt   time.Time                    `json:"latest_at"`
	Members    []compMember                 `json:"members"`
	DScan      *dscanReading                `json:"dscan,omitempty"`
	DScanFrom  int64                        `json:"dscan_from,omitempty"`
	DScanRows  []scan.DScanRow              `json:"dscan_rows,omitempty"`
	Local      *localReading                `json:"local,omitempty"`
	Pilots     []intelanalyze.Pilot         `json:"pilots,omitempty"` // analysis union, the no-local fallback
	Ships      []intelanalyze.Ship          `json:"ships,omitempty"`  // analysis union, the no-dscan fallback
	Gate       int64                        `json:"gate,omitempty"`
	Counts     *intelanalyze.StandingCounts `json:"counts,omitempty"`
	Links      []compLink                   `json:"links,omitempty"`
	Likely     []compLikely                 `json:"likely,omitempty"`
	InputsHash string                       `json:"ih,omitempty"` // fingerprint of the member inputs this was built from
}

// --- Group assignment --------------------------------------------------------

// reportSig is a report's content signature for group choice:
// the hull type ids its reading resolved and the pilot ids it
// names, from wherever the report keeps them.
func (app *Application) reportSig(ctx context.Context, text, analysisRaw, scanID string) (hulls, pilots map[int64]bool) {
	hulls, pilots = map[int64]bool{}, map[int64]bool{}
	for id := range app.dscanSetOf(ctx, scanID) {
		hulls[id] = true
	}
	if set := app.rosterSetOf(ctx, text, analysisRaw, scanID); set != nil {
		for id := range set {
			pilots[id] = true
		}
	}
	if analysisRaw != "" {
		var a intelanalyze.Analysis
		if json.Unmarshal([]byte(analysisRaw), &a) == nil {
			for _, s := range a.Ships {
				if s.TypeID > 0 {
					hulls[s.TypeID] = true
				}
			}
			for _, p := range a.Pilots {
				if p.ID > 0 {
					pilots[p.ID] = true
				}
			}
		}
	}
	return hulls, pilots
}

// intelGroupFor picks the group a new report joins: a standing
// group in its channel and system whose latest activity is
// inside intelGroupWindow of the report's moment. When several
// qualify, the greatest content overlap wins (shared hull types
// and pilots), then the nearest in time, then the younger
// group. 0 founds a new group. A systemless report groups with
// nothing.
func (app *Application) intelGroupFor(ctx context.Context, p db.InsertIntelReportParams) int64 {
	if p.SystemID == 0 {
		return 0
	}
	rows, err := app.queries.ListGroupCandidateReports(ctx, db.ListGroupCandidateReportsParams{
		ChannelID: p.ChannelID, SystemID: p.SystemID,
		Since: p.SeenAt.Add(-intelGroupWindow), Until: p.SeenAt.Add(intelGroupWindow),
		MaxRows: intelGroupCandidateCap})
	if err != nil {
		logging.Warnf("intel: group candidates for channel %d: %v", p.ChannelID, err)
		return 0
	}
	hulls, pilots := app.reportSig(ctx, p.Text, p.Analysis, p.ScanID)
	type cand struct {
		latest  time.Time
		overlap int
	}
	groups := map[int64]*cand{}
	for _, r := range rows {
		gid := r.GroupID
		if gid == 0 {
			gid = r.ID
		}
		c := groups[gid]
		if c == nil {
			c = &cand{}
			groups[gid] = c
		}
		if r.SeenAt.After(c.latest) {
			c.latest = r.SeenAt
		}
		h, pl := app.reportSig(ctx, r.Text, r.Analysis, r.ScanID)
		for id := range h {
			if hulls[id] {
				c.overlap++
			}
		}
		for id := range pl {
			if pilots[id] {
				c.overlap++
			}
		}
	}
	best, bestOverlap := int64(0), -1
	var bestGap time.Duration
	for gid, c := range groups {
		gap := p.SeenAt.Sub(c.latest)
		if gap < 0 {
			gap = -gap
		}
		switch {
		case c.overlap > bestOverlap,
			c.overlap == bestOverlap && best != 0 && gap < bestGap,
			c.overlap == bestOverlap && gap == bestGap && gid > best:
			best, bestOverlap, bestGap = gid, c.overlap, gap
		}
	}
	return best
}

// storeIntelReport inserts a settled report row: its group is
// chosen from the standing groups first (0 founds a new one,
// named for the report itself once its id is known), and the
// group's composite is refreshed with the report in. Every
// insert path that stores a finished report goes through here,
// so grouping and the composite never depend on the door a
// report came in by.
func (app *Application) storeIntelReport(ctx context.Context, p db.InsertIntelReportParams) (int64, error) {
	p.GroupID = app.intelGroupFor(ctx, p)
	id, err := app.queries.InsertIntelReport(ctx, p)
	if err != nil {
		return 0, err
	}
	gid := p.GroupID
	if gid == 0 {
		gid = id
		if err := app.queries.SetIntelReportGroup(ctx, db.SetIntelReportGroupParams{GroupID: gid, ID: id}); err != nil {
			logging.Warnf("intel: name group of report %d: %v", id, err)
		}
	}
	app.refreshGroupComposite(ctx, gid)
	return id, nil
}

// refreshReportGroup recomputes the composite of the group a
// stored report belongs to, after the report's own reading
// changed under it (the analysis drain settling it).
func (app *Application) refreshReportGroup(ctx context.Context, reportID int64) {
	rep, err := app.queries.GetIntelReportFull(ctx, reportID)
	if err != nil || rep.GroupID == 0 {
		return
	}
	app.refreshGroupComposite(ctx, rep.GroupID)
}

// groupInputsHash fingerprints the member inputs a composite
// is derived from: member identity, moment, scan, and stored
// reading. Any change to any of them rebuilds; none means the
// standing composite already says it.
func groupInputsHash(members []db.IntelReport) string {
	h := sha256.New()
	for _, m := range members {
		fmt.Fprintf(h, "%d|%d|%s|%s|%s\n", m.ID, m.SeenAt.UnixNano(), m.ScanID, m.Kind, m.Analysis)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// refreshGroupComposite recomputes a group's composite from its
// standing members and stores it, the standing composite
// rotating into the previous slot the diff view reads. A group
// with no members left loses its composite with them.
func (app *Application) refreshGroupComposite(ctx context.Context, groupID int64) {
	if groupID <= 0 {
		return
	}
	members, err := app.queries.ListIntelGroupMembers(ctx, db.ListIntelGroupMembersParams{
		GroupID: groupID, MaxRows: intelGroupMemberCap})
	if err != nil {
		logging.Warnf("intel: group %d members: %v", groupID, err)
		return
	}
	if len(members) == 0 {
		if err := app.queries.DeleteIntelGroupComposite(ctx, groupID); err != nil {
			logging.Warnf("intel: drop composite of empty group %d: %v", groupID, err)
		}
		// The group is gone, so its gang claim goes with it;
		// a gang left standing on nothing is pruned.
		if err := app.queries.DeleteIntelGangGroupMembership(ctx, groupID); err != nil {
			logging.Warnf("intel: drop gang claim of empty group %d: %v", groupID, err)
		}
		if _, err := app.queries.PruneIntelGangs(ctx); err != nil {
			logging.Warnf("intel: prune gangs: %v", err)
		}
		return
	}
	// One bounded pass per change: the composite is derived
	// from the stored member inputs, and when those inputs
	// are unchanged since the standing composite was built
	// the rebuild — every member re-read, re-decoded, and
	// re-weighed — is skipped. A drain settling ten reports
	// of one group used to rebuild the whole group ten
	// times (quadratic over the group's life); now it
	// builds once and the rest compare a fingerprint.
	inputsHash := groupInputsHash(members)
	if cur, _, ok := app.loadGroupComposite(ctx, groupID); ok && cur.InputsHash == inputsHash {
		// No rebuild, but gang fusion is time-based (stale
		// gangs close by the clock), so it still runs on
		// the standing composite.
		app.evaluateGang(ctx, cur)
		return
	}
	comp := app.buildGroupComposite(ctx, members)
	if comp == nil {
		return
	}
	comp.InputsHash = inputsHash
	raw, err := json.Marshal(comp)
	if err != nil {
		return
	}
	if err := app.queries.UpsertIntelGroupComposite(ctx, db.UpsertIntelGroupCompositeParams{
		GroupID: groupID, ChannelID: comp.ChannelID, SystemID: comp.SystemID,
		Composite: string(raw), Members: int32(len(members)), ComputedAt: time.Now().UTC()}); err != nil {
		logging.Warnf("intel: store composite of group %d: %v", groupID, err)
		return
	}
	// Fusion weighs the group for gang identity on the stored
	// composite (intel_gangs.go).
	app.evaluateGang(ctx, comp)
}

// loadGroupComposite reads a group's stored composite and the
// previous one retained beside it, parsed. ok is false when no
// composite stands (a group of one whose refresh has not run,
// or none was ever stored).
func (app *Application) loadGroupComposite(ctx context.Context, groupID int64) (cur, prev *intelComposite, ok bool) {
	if groupID <= 0 {
		return nil, nil, false
	}
	row, err := app.queries.GetIntelGroupComposite(ctx, groupID)
	if err != nil || row.Composite == "" {
		return nil, nil, false
	}
	if json.Unmarshal([]byte(row.Composite), &cur) != nil || cur == nil {
		return nil, nil, false
	}
	if row.Previous != "" {
		_ = json.Unmarshal([]byte(row.Previous), &prev)
	}
	return cur, prev, true
}

// --- Computing the composite -------------------------------------------------

// memberReading is one member report with everything its
// composite needs read from storage.
type memberReading struct {
	rep      db.IntelReport
	analysis intelanalyze.Analysis
	res      scanResult
	hasRes   bool
	raw      string
}

// buildGroupComposite fuses a group's member reports (the
// query returns them newest first) into the stored picture.
// The rules are the brief's: the newest d-scan governs the
// hulls (a hull absent from it leaves the composite), locals
// reconcile as the union of pilots with each pilot's latest
// data, fleet rows are fact links, and elimination over what
// remains yields at most a likely link, stored with its basis.
func (app *Application) buildGroupComposite(ctx context.Context, members []db.IntelReport) *intelComposite {
	if len(members) == 0 {
		return nil
	}
	// Oldest first: later writes win wherever the rules say the
	// latest data governs.
	ordered := make([]db.IntelReport, len(members))
	for i, m := range members {
		ordered[len(members)-1-i] = m
	}
	comp := &intelComposite{
		GroupID:   ordered[0].GroupID,
		ChannelID: ordered[0].ChannelID,
		SystemID:  ordered[0].SystemID,
	}
	if comp.GroupID == 0 {
		comp.GroupID = ordered[0].ID
	}
	comp.ComputedAt = time.Now().UTC()

	reads := make([]memberReading, len(ordered))
	for i, rep := range ordered {
		mr := memberReading{rep: rep}
		if rep.Analysis != "" {
			_ = json.Unmarshal([]byte(rep.Analysis), &mr.analysis)
		}
		if rep.ScanID != "" {
			if row, err := app.queries.GetScan(ctx, rep.ScanID); err == nil {
				mr.raw = row.Raw
				if json.Unmarshal([]byte(row.Result), &mr.res) == nil {
					mr.hasRes = true
				}
			}
		}
		reads[i] = mr
		if rep.SeenAt.After(comp.LatestAt) {
			comp.LatestAt = rep.SeenAt
		}
		// Provenance: the member's tallies and pilot set, for
		// the rail's deltas.
		cm := compMember{ReportID: rep.ID, Reporter: rep.Reporter, SeenAt: rep.SeenAt}
		if mr.hasRes {
			cm.ScanKind = string(mr.res.Kind)
			cm.Kind = wbScanKindLabel(mr.res.Kind)
		} else if label := intelKindLabels[rep.Kind]; label != "" {
			cm.Kind = label
		} else {
			cm.Kind = "Report"
		}
		if mr.hasRes && mr.res.DScan != nil {
			cm.Tallies = map[string][2]int{}
			for _, list := range [][]scanCount{mr.res.DScan.Types, mr.res.DScan.Deployables} {
				for _, c := range list {
					v := cm.Tallies[c.Name]
					v[0] += c.N
					v[1] += c.Grid
					cm.Tallies[c.Name] = v
					cm.NHulls += c.N
				}
			}
		} else {
			for _, s := range mr.analysis.Ships {
				if cm.Tallies == nil {
					cm.Tallies = map[string][2]int{}
				}
				n := s.N
				if n == 0 {
					n = 1
				}
				v := cm.Tallies[s.Name]
				v[0] += n
				cm.Tallies[s.Name] = v
				cm.NHulls += n
			}
		}
		pilotSet := map[int64]bool{}
		if mr.hasRes && mr.res.Local != nil {
			for _, lp := range mr.res.Local.Pilots {
				pilotSet[lp.ID] = true
			}
		}
		for _, p := range mr.analysis.Pilots {
			pilotSet[p.ID] = true
		}
		for id := range pilotSet {
			if len(cm.PilotIDs) < intelGroupPilotCap {
				cm.PilotIDs = append(cm.PilotIDs, id)
			}
		}
		sort.Slice(cm.PilotIDs, func(a, b int) bool { return cm.PilotIDs[a] < cm.PilotIDs[b] })
		comp.Members = append(comp.Members, cm)
	}

	// The faces, newest reading of each kind.
	var newestFleet *fleetReading
	var primaryKind scan.Kind
	var primaryPoster int64
	var primaryParsed *scan.Result
	for i := len(reads) - 1; i >= 0; i-- {
		mr := reads[i]
		if !mr.hasRes {
			continue
		}
		if primaryKind == "" {
			primaryKind = mr.res.Kind
			primaryPoster = mr.rep.AuthorCharacterID
		}
		if comp.DScan == nil && mr.res.DScan != nil {
			comp.DScan = mr.res.DScan
			comp.DScanFrom = mr.rep.ID
			if p, err := scan.Parse(mr.raw); err == nil && p.Kind == scan.KindDScan {
				comp.DScanRows = p.DScan
				if primaryKind == scan.KindDScan {
					parsed := p
					primaryParsed = &parsed
				}
			}
		}
		if newestFleet == nil && mr.res.Fleet != nil {
			newestFleet = mr.res.Fleet
		}
	}

	comp.Local = mergeGroupLocals(reads)

	// Analysis unions: the fallbacks where a face is missing,
	// and the pilots the cards name.
	pilotByID := map[int64]intelanalyze.Pilot{}
	shipByName := map[string]intelanalyze.Ship{}
	for _, mr := range reads {
		for _, p := range mr.analysis.Pilots {
			pilotByID[p.ID] = p
		}
		for _, s := range mr.analysis.Ships {
			shipByName[s.Name] = s
		}
		if mr.analysis.Gate > 0 {
			comp.Gate = mr.analysis.Gate
		}
	}
	for _, p := range pilotByID {
		if len(comp.Pilots) < intelGroupPilotCap {
			comp.Pilots = append(comp.Pilots, p)
		}
	}
	sort.Slice(comp.Pilots, func(a, b int) bool { return comp.Pilots[a].Name < comp.Pilots[b].Name })
	for _, s := range shipByName {
		comp.Ships = append(comp.Ships, s)
	}
	sort.Slice(comp.Ships, func(a, b int) bool {
		if comp.Ships[a].N != comp.Ships[b].N {
			return comp.Ships[a].N > comp.Ships[b].N
		}
		return comp.Ships[a].Name < comp.Ships[b].Name
	})

	// Counts: the local union's own split when the composite
	// has one; otherwise the newest member counts that were
	// stored with a report.
	if comp.Local != nil && len(comp.Local.Pilots) > 0 {
		st := app.channelStandingOf(ctx, comp.ChannelID)
		var c intelanalyze.StandingCounts
		for _, lp := range comp.Local.Pilots {
			switch st.ofLocal(lp) {
			case bucketHostile:
				c.Hostile++
			case bucketFriendly:
				c.Friendly++
			case bucketNeutral:
				c.Neutral++
			default:
				c.Unclassified++
			}
		}
		if c.Total() > 0 {
			comp.Counts = &c
		}
	} else {
		for i := len(reads) - 1; i >= 0; i-- {
			if reads[i].analysis.Counts != nil && reads[i].analysis.Counts.Total() > 0 {
				comp.Counts = reads[i].analysis.Counts
				break
			}
		}
	}

	comp.Links = app.groupFactLinks(ctx, reads)
	comp.Likely = groupLikelyLinks(comp)
	// What elimination left over, a pilot's stored record can
	// sometimes carry: profile attributions join the likely
	// list, each with its probability and basis stated.
	if extra := app.groupProfileLikely(ctx, comp); len(extra) > 0 {
		comp.Likely = append(comp.Likely, extra...)
	}

	// The headline: the sentence the primary face's reading
	// makes, over the composite's own faces (the local face is
	// the union, the d-scan the governing one). A group with no
	// reading at all keeps its newest member's own headline.
	if primaryKind != "" {
		res := scanResult{Kind: primaryKind, SystemID: comp.SystemID,
			DScan: comp.DScan, Local: comp.Local, Fleet: newestFleet}
		headline, _ := app.scanReportText(ctx, comp.ChannelID, res, primaryParsed, primaryPoster)
		comp.Headline = headline
	}
	if comp.Headline == "" {
		comp.Headline = ordered[len(ordered)-1].Text
	}
	return comp
}

// mergeGroupLocals reconciles the members' local readings as
// one: the union of pilots, each pilot's row from the newest
// reading that names them, and the alliance sections rebuilt
// from the merged roster with the readings' own names.
func mergeGroupLocals(reads []memberReading) *localReading {
	byID := map[int64]localPilot{}
	alliName, corpName := map[int64]string{}, map[int64]string{}
	unknown := map[string]bool{}
	var unknownOrder []string
	partial := false
	any := false
	for _, mr := range reads {
		if !mr.hasRes || mr.res.Local == nil {
			continue
		}
		any = true
		lr := mr.res.Local
		if lr.Partial {
			partial = true
		}
		for _, g := range lr.Alliances {
			if g.Corp {
				corpName[g.ID] = g.Name
			} else {
				alliName[g.ID] = g.Name
				for _, c := range g.Corps {
					corpName[c.ID] = c.Name
				}
			}
		}
		for _, lp := range lr.Pilots {
			byID[lp.ID] = lp
		}
		for _, u := range lr.Unknown {
			if !unknown[u] {
				unknown[u] = true
				unknownOrder = append(unknownOrder, u)
			}
		}
	}
	if !any || len(byID) == 0 {
		return nil
	}
	out := &localReading{Partial: partial}
	for _, lp := range byID {
		out.Pilots = append(out.Pilots, lp)
	}
	sort.Slice(out.Pilots, func(a, b int) bool { return out.Pilots[a].Name < out.Pilots[b].Name })
	out.Unknown = unknownOrder
	// Sections: alliances by their pilots, then corporations
	// standing outside any alliance.
	alliMembers := map[int64][]localPilot{}
	corpMembers := map[int64][]localPilot{}
	for _, lp := range out.Pilots {
		switch {
		case lp.AllianceID > 0:
			alliMembers[lp.AllianceID] = append(alliMembers[lp.AllianceID], lp)
		case lp.CorpID > 0:
			corpMembers[lp.CorpID] = append(corpMembers[lp.CorpID], lp)
		}
	}
	killsOf := func(members []localPilot) int64 {
		var n int64
		for _, lp := range members {
			n += lp.Kills
		}
		return n
	}
	for aid, members := range alliMembers {
		g := localGroup{ID: aid, Name: alliName[aid], N: len(members), Kills: killsOf(members)}
		byCorp := map[int64]int{}
		for _, lp := range members {
			if lp.CorpID > 0 {
				byCorp[lp.CorpID]++
			}
		}
		for cid, n := range byCorp {
			g.Corps = append(g.Corps, localGroup{ID: cid, Name: corpName[cid], N: n, Corp: true})
		}
		sort.Slice(g.Corps, func(a, b int) bool { return g.Corps[a].Name < g.Corps[b].Name })
		out.Alliances = append(out.Alliances, g)
	}
	for cid, members := range corpMembers {
		out.Alliances = append(out.Alliances, localGroup{ID: cid, Name: corpName[cid], N: len(members), Kills: killsOf(members), Corp: true})
	}
	sort.Slice(out.Alliances, func(a, b int) bool {
		if out.Alliances[a].N != out.Alliances[b].N {
			return out.Alliances[a].N > out.Alliances[b].N
		}
		return out.Alliances[a].Name < out.Alliances[b].Name
	})
	return out
}

// groupFactLinks reads the members' fleet composition rows as
// fact pilot-to-ship links: a fleet row states the two
// together. The newest statement of a pilot's ship wins; names
// are resolved to ids where a stored record backs them, and
// the link stands on the names alone where none does.
func (app *Application) groupFactLinks(ctx context.Context, reads []memberReading) []compLink {
	byPilot := map[string]*compLink{}
	for _, mr := range reads {
		if !mr.hasRes || mr.res.Fleet == nil {
			continue
		}
		for _, m := range mr.res.Fleet.Members {
			if m.Name == "" || m.Ship == "" {
				continue
			}
			key := strings.ToLower(m.Name)
			l := byPilot[key]
			if l == nil {
				l = &compLink{PilotName: m.Name}
				byPilot[key] = l
			}
			l.ShipName = m.Ship
			l.From = mr.rep.ID
			l.TypeID = 0
			if row, err := app.queries.GetSDETypeByName(ctx, m.Ship); err == nil {
				l.TypeID = row.TypeID
			}
			if row, err := app.queries.GetIntelPilotName(ctx, key); err == nil && row.CharacterID > 0 {
				l.PilotID = row.CharacterID
			}
		}
	}
	out := make([]compLink, 0, len(byPilot))
	for _, l := range byPilot {
		out = append(out, *l)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].PilotName < out[b].PilotName })
	return out
}

// groupLikelyLinks attributes by elimination, once, when the
// composite leaves exactly one pilot without a ship and
// exactly one hull instance unclaimed: the two belong
// together, as a likelihood with its basis. Anything less
// certain is left unattributed.
func groupLikelyLinks(comp *intelComposite) []compLikely {
	if comp.DScan == nil || comp.Local == nil || len(comp.Local.Pilots) == 0 {
		return nil
	}
	linked := map[int64]bool{}
	linkedName := map[string]bool{}
	claimed := map[string]int{}
	for _, l := range comp.Links {
		if l.PilotID > 0 {
			linked[l.PilotID] = true
		}
		linkedName[strings.ToLower(l.PilotName)] = true
		claimed[l.ShipName]++
	}
	var unlinked []localPilot
	for _, lp := range comp.Local.Pilots {
		if linked[lp.ID] || linkedName[strings.ToLower(lp.Name)] {
			continue
		}
		unlinked = append(unlinked, lp)
	}
	if len(unlinked) != 1 {
		return nil
	}
	left, shipName, typeID := 0, "", int64(0)
	for _, c := range comp.DScan.Types {
		rest := c.N - claimed[c.Name]
		if rest < 0 {
			rest = 0
		}
		left += rest
		if rest == 1 {
			shipName, typeID = c.Name, c.ID
		}
	}
	if left != 1 || shipName == "" {
		return nil
	}
	return []compLikely{{
		PilotID: unlinked[0].ID, PilotName: unlinked[0].Name,
		ShipName: shipName, TypeID: typeID,
		Basis: "The only pilot in local without a ship, and the only unclaimed hull on the scan.",
	}}
}

// --- Cards and the rail ------------------------------------------------------

// groupCompositesFor fetches the stored composites of several
// groups in one read, keyed by group id. Groups without a
// stored composite are absent.
func (app *Application) groupCompositesFor(ctx context.Context, groupIDs []int64) map[int64]*intelComposite {
	out := map[int64]*intelComposite{}
	if len(groupIDs) == 0 {
		return out
	}
	rows, err := app.queries.ListIntelGroupComposites(ctx, groupIDs)
	if err != nil {
		logging.Warnf("intel: group composites: %v", err)
		return out
	}
	for _, r := range rows {
		if r.Composite == "" {
			continue
		}
		var c intelComposite
		if json.Unmarshal([]byte(r.Composite), &c) == nil {
			comp := c
			out[r.GroupID] = &comp
		}
	}
	return out
}

// applyCompositeCard fills a grouped report's card from its
// group's composite: the headline, the pilots and ships the
// group's picture names, and the standing split it counted.
// The card keeps the report's own kind badge: the group is
// read through the report that was opened or listed.
func (app *Application) applyCompositeCard(ctx context.Context, rv *reportView, kind string, comp *intelComposite, names *killNames) {
	applyKind(rv, kind)
	if comp.Headline != "" {
		rv.Text = comp.Headline
	}
	seen := map[int64]bool{}
	for _, p := range comp.Pilots {
		seen[p.ID] = true
		rv.Pilots = append(rv.Pilots, reportPilot{ID: p.ID, Name: p.Name, Friendly: p.Friendly && !p.Red})
	}
	if comp.Local != nil {
		st := app.channelStandingOf(ctx, comp.ChannelID)
		for _, lp := range comp.Local.Pilots {
			if seen[lp.ID] {
				continue
			}
			seen[lp.ID] = true
			b := st.ofLocal(lp)
			rv.Pilots = append(rv.Pilots, reportPilot{ID: lp.ID, Name: lp.Name, Friendly: b == bucketFriendly})
		}
	}
	if comp.DScan != nil {
		for _, c := range comp.DScan.Types {
			rv.Ships = append(rv.Ships, reportShip{ID: c.ID, Name: c.Name, N: c.N})
		}
	} else {
		for _, s := range comp.Ships {
			rv.Ships = append(rv.Ships, reportShip{ID: s.TypeID, Name: s.Name, N: s.N})
		}
	}
	if comp.Gate > 0 {
		rv.Gate = names.system(comp.Gate).Name
		rv.GateID = comp.Gate
	}
	if comp.Counts != nil && kind != intelanalyze.KindQuestion && kind != intelanalyze.KindChatter {
		rv.Split = standingSplit(comp.Counts)
		rv.Hostiles = int32(comp.Counts.Hostile)
	}
}

// wbRailItem is one member's line in the workbench's related
// rail: its kind, age and poster, what it adds to or misses
// from the group picture, and whether it is the report open.
type wbRailItem struct {
	ReportID int64
	Kind     string
	Ago      string
	Reporter string
	Delta    string
	Here     bool
}

// wbRailCap bounds the members the rail lists: the newest, and
// always the report being viewed.
const wbRailCap = 25

// wbRail builds the related rail for a grouped workbench page:
// the group's members, newest first, each with its delta
// against the composite, the viewed report marked. It returns
// the items and how many members stand beyond the cap.
func wbRail(now time.Time, comp *intelComposite, viewedID int64) ([]wbRailItem, int) {
	out := make([]wbRailItem, 0, len(comp.Members))
	for i := len(comp.Members) - 1; i >= 0; i-- {
		m := comp.Members[i]
		out = append(out, wbRailItem{
			ReportID: m.ReportID, Kind: m.Kind, Ago: ageText(now, m.SeenAt),
			Reporter: m.Reporter, Delta: compMemberDelta(comp, m), Here: m.ReportID == viewedID,
		})
	}
	if len(out) <= wbRailCap {
		return out, 0
	}
	more := len(out) - wbRailCap
	shown := out[:wbRailCap]
	// The viewed report keeps its line even past the cap.
	found := false
	for _, it := range shown {
		found = found || it.Here
	}
	if !found {
		for _, it := range out[wbRailCap:] {
			if it.Here {
				shown[wbRailCap-1] = it
				break
			}
		}
	}
	return shown, more
}

// wbRailKinds summarises a group's members by kind for the
// rail's header: "D-Scan x2, Local x1" in the order the kinds
// first appear, newest first.
func wbRailKinds(comp *intelComposite) string {
	var order []string
	counts := map[string]int{}
	for i := len(comp.Members) - 1; i >= 0; i-- {
		k := comp.Members[i].Kind
		if counts[k] == 0 {
			order = append(order, k)
		}
		counts[k]++
	}
	parts := make([]string, 0, len(order))
	for _, k := range order {
		s := k
		if counts[k] > 1 {
			s += " ×" + strconv.Itoa(counts[k])
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, " · ")
}

// compMemberDelta words one member's difference from the group
// picture: hulls and pilots the composite holds that the member
// does not (+), and hulls and pilots the member holds that the
// composite no longer does (-, a superseded reading's leftovers).
func compMemberDelta(comp *intelComposite, m compMember) string {
	var parts []string
	if comp.DScan != nil && m.Tallies != nil {
		compT := map[string]int{}
		for _, list := range [][]scanCount{comp.DScan.Types, comp.DScan.Deployables} {
			for _, c := range list {
				compT[c.Name] += c.N
			}
		}
		plus, minus := 0, 0
		for name, n := range compT {
			if d := n - m.Tallies[name][0]; d > 0 {
				plus += d
			}
		}
		for name, v := range m.Tallies {
			if d := v[0] - compT[name]; d > 0 {
				minus += d
			}
		}
		if plus > 0 {
			parts = append(parts, "+"+plural(plus, "hull"))
		}
		if minus > 0 {
			parts = append(parts, "-"+plural(minus, "hull"))
		}
	}
	if comp.Local != nil && m.PilotIDs != nil {
		inComp := map[int64]bool{}
		for _, lp := range comp.Local.Pilots {
			inComp[lp.ID] = true
		}
		inMember := map[int64]bool{}
		for _, id := range m.PilotIDs {
			inMember[id] = true
		}
		plus, minus := 0, 0
		for id := range inComp {
			if !inMember[id] {
				plus++
			}
		}
		for id := range inMember {
			if !inComp[id] {
				minus++
			}
		}
		if plus > 0 {
			parts = append(parts, "+"+plural(plus, "pilot"))
		}
		if minus > 0 {
			parts = append(parts, "-"+plural(minus, "pilot"))
		}
	}
	if len(parts) == 0 && (m.Tallies != nil || m.PilotIDs != nil) {
		return "matches the group picture"
	}
	return strings.Join(parts, ", ")
}
