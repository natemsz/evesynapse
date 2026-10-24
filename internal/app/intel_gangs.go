package app

// ---------------------------------------------------------------------------
// Tracked gangs (schema 089): the fusion layer over report
// groups (intel_groups.go).
//
// A group is one channel's reports on one system inside one
// sitting. A gang is the body of pilots those groups keep
// sighting: when a gang jumps a gate, the reports in the new
// system found a new group, and when it is reported again after
// its group's window closed, likewise. Fusion is what says the
// two sightings are the same people. It runs at ingest, when a
// group's composite lands: the composite is weighed against the
// channel's open gangs, and identity is decided on pilot
// overlap, the time since the gang was last seen, and the jump
// distance between the sightings. The rules are deliberately
// conservative: identified pilots gate the match (hull overlap
// only strengthens it, because gangs reship and hulls alone are
// how two gangs get read as one), and when the evidence is not
// enough the sighting founds a new gang. A wrong merge is worse
// intel than two gangs.
//
// A gang with no sighting for gangLifetime is closed and never
// matched again: the same pilots sighted later found a new
// gang. The old gang's record stands; nothing resurrects it
// silently.
//
// What renders read is stored: the gang's sightings, its
// identity strength, its last system and moment, and a snapshot
// of its freshest composite's picture. The confidence a surface
// shows is that stored strength decayed by the gang's age and
// by the jump distance from the system being read about, by
// gangConfidenceAt: arithmetic on the stored row, the same
// footing as an age.
//
// Likely ships (groupProfileLikely) are the second half of the
// layer: pilots a composite names without a ship, attributed
// from their stored activity profiles (schema 088) where an
// unclaimed hull on the scan matches the hull their record
// demonstrates. The attribution is stored in the composite's
// Likely list beside the elimination links, with its
// probability and its basis written out. A pilot with no
// profile, or no demonstrated hull among the unclaimed, gets
// no attribution: no profile, no guess.
// ---------------------------------------------------------------------------

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/intelanalyze"
	"evesynapse/internal/logging"
)

// The fusion constants. Each is a starting value that tunes
// from real channel traffic, like the group window's.
const (
	// gangLifetime is how long a gang stands without a new
	// sighting before it is closed (owner-set default).
	gangLifetime = 2 * time.Hour
	// gangMinSharedPilots is the least identified pilots two
	// sightings must share before they can be the same gang.
	gangMinSharedPilots = 2
	// gangMinPilotOverlap is the least containment of the
	// smaller roster in the larger: shared pilots over the
	// smaller roster's size.
	gangMinPilotOverlap = 0.5
	// gangMatchMaxJumps is the farthest apart, in gates, two
	// sightings may be and still match.
	gangMatchMaxJumps = 5
	// gangFoundStrength is a lone sighting's identity
	// strength: the sighting is certainly itself, and one
	// sighting is all the tracking there is so far.
	gangFoundStrength = 0.6
	// gangConfidenceHalfLife is how fast displayed confidence
	// halves without a new sighting.
	gangConfidenceHalfLife = 30 * time.Minute
	// gangJumpKeep is the confidence kept per gate of
	// distance from the system being read about.
	gangJumpKeep = 0.85
	// gangJumpCap bounds the gate-graph search: farther than
	// this reads as far, not as a number.
	gangJumpCap = 12
	// gangLikelyMinProb is the least demonstrated share of a
	// pilot's record a hull must hold before it is attributed
	// to them as likely.
	gangLikelyMinProb = 0.35
	// gangStaleConfidence is where a tracked gang's wording
	// turns from tracked to gone quiet.
	gangStaleConfidence = 0.35
	// Bounds on the ingest reads: open gangs weighed per
	// composite, groups one gang may stand on, and pilots per
	// composite given a profile read.
	gangCandidateCap   = 10
	gangMemberCap      = 20
	gangLikelyPilotCap = 25
)

// --- The stored picture ------------------------------------------------------

// gangPilot is one pilot in a gang's snapshot.
type gangPilot struct {
	ID   int64  `json:"id,omitempty"`
	Name string `json:"name"`
}

// gangHull is one hull type in a gang's snapshot, with the
// class the doctrine words read.
type gangHull struct {
	TypeID int64  `json:"type,omitempty"`
	Name   string `json:"name"`
	Class  string `json:"class,omitempty"`
	N      int    `json:"n"`
}

// gangSnapshot is a gang's picture, stored as JSON on
// intel_gangs: the governing (freshest) member composite's
// faces, kept lean. The systems are the gang's trail, oldest
// first.
type gangSnapshot struct {
	Pilots  []gangPilot                  `json:"pilots,omitempty"`
	Hulls   []gangHull                   `json:"hulls,omitempty"`
	Counts  *intelanalyze.StandingCounts `json:"counts,omitempty"`
	Likely  []compLikely                 `json:"likely,omitempty"`
	Systems []int64                      `json:"systems,omitempty"`
}

// trackedGang is a gang row with its snapshot parsed and its
// member groups listed.
type trackedGang struct {
	ID         int64
	ChannelID  int64
	FirstSeen  time.Time
	LastSeen   time.Time
	LastSystem int64
	Sightings  int
	Strength   float64
	Closed     bool
	Snap       gangSnapshot
	Groups     []int64
}

func gangFromRow(row db.IntelGang) *trackedGang {
	g := &trackedGang{
		ID: row.ID, ChannelID: row.ChannelID,
		FirstSeen: row.FirstSeenAt, LastSeen: row.LastSeenAt,
		LastSystem: row.LastSystemID, Sightings: int(row.Sightings),
		Strength: row.Strength, Closed: row.ClosedAt.Valid,
	}
	if row.Snapshot != "" {
		_ = json.Unmarshal([]byte(row.Snapshot), &g.Snap)
	}
	return g
}

// showsAsEvent is whether a gang carries anything beyond the
// one report being read: it has been sighted more than once,
// or across more than one group. A lone single sighting is
// just the report itself and wears no gang line.
func (g *trackedGang) showsAsEvent() bool {
	return g != nil && !g.Closed && (len(g.Groups) >= 2 || g.Sightings >= 2)
}

// gangConfidenceAt is the confidence a surface states for a
// gang: its stored identity strength, halved every
// gangConfidenceHalfLife since its last sighting, and kept at
// gangJumpKeep per gate between its last system and the system
// being read about. A closed gang states none. An unknown
// distance (the gate graph unread, or farther than the cap)
// counts as the cap.
func gangConfidenceAt(g *trackedGang, now time.Time, jumps int, jumpOK bool) float64 {
	if g == nil || g.Closed {
		return 0
	}
	age := now.Sub(g.LastSeen)
	if age < 0 {
		age = 0
	}
	c := g.Strength * math.Pow(0.5, float64(age)/float64(gangConfidenceHalfLife))
	d := jumps
	if !jumpOK {
		d = gangJumpCap
	}
	if d > 0 {
		c *= math.Pow(gangJumpKeep, float64(d))
	}
	if c > 1 {
		c = 1
	}
	return c
}

// --- Sets a composite names --------------------------------------------------

// compPilotSet is the identified pilots a composite names: its
// local roster, its analysis pilots, and its fact links'.
func compPilotSet(comp *intelComposite) map[int64]bool {
	out := map[int64]bool{}
	if comp.Local != nil {
		for _, lp := range comp.Local.Pilots {
			if lp.ID > 0 {
				out[lp.ID] = true
			}
		}
	}
	for _, p := range comp.Pilots {
		if p.ID > 0 {
			out[p.ID] = true
		}
	}
	for _, l := range comp.Links {
		if l.PilotID > 0 {
			out[l.PilotID] = true
		}
	}
	return out
}

// compPilotNames resolves the names of a composite's pilots
// from the same faces, for snapshots.
func compPilotNames(comp *intelComposite) map[int64]string {
	out := map[int64]string{}
	if comp.Local != nil {
		for _, lp := range comp.Local.Pilots {
			if lp.ID > 0 && lp.Name != "" {
				out[lp.ID] = lp.Name
			}
		}
	}
	for _, p := range comp.Pilots {
		if p.ID > 0 && p.Name != "" {
			out[p.ID] = p.Name
		}
	}
	for _, l := range comp.Links {
		if l.PilotID > 0 && l.PilotName != "" {
			out[l.PilotID] = l.PilotName
		}
	}
	return out
}

// compHullCounts is a composite's hull tally by type id, from
// the governing d-scan, or the analysis ships where there is
// no d-scan face.
func compHullCounts(comp *intelComposite) map[int64]int {
	out := map[int64]int{}
	if comp.DScan != nil {
		for _, c := range comp.DScan.Types {
			if c.ID > 0 {
				out[c.ID] += c.N
			}
		}
		return out
	}
	for _, s := range comp.Ships {
		if s.TypeID > 0 {
			n := s.N
			if n == 0 {
				n = 1
			}
			out[s.TypeID] += n
		}
	}
	return out
}

// --- The aggregate of a gang's member composites -----------------------------

type gangAggregate struct {
	firstSeen  time.Time
	lastSeen   time.Time
	lastSystem int64
	sightings  int
	roster     map[int64]bool
	snap       gangSnapshot
}

// gangAggregateOf folds a gang's member composites into the
// picture the gang stores: the freshest composite governs the
// faces, the roster is the union of every member's pilots, and
// the trail is every member's system ordered by its latest
// sighting there.
func gangAggregateOf(comps []*intelComposite) gangAggregate {
	agg := gangAggregate{roster: map[int64]bool{}}
	if len(comps) == 0 {
		return agg
	}
	var gov *intelComposite
	names := map[int64]string{}
	sysLatest := map[int64]time.Time{}
	for _, comp := range comps {
		if gov == nil || comp.LatestAt.After(gov.LatestAt) ||
			(comp.LatestAt.Equal(gov.LatestAt) && comp.GroupID > gov.GroupID) {
			gov = comp
		}
		if agg.lastSeen.IsZero() || comp.LatestAt.After(agg.lastSeen) {
			agg.lastSeen = comp.LatestAt
		}
		for _, m := range comp.Members {
			agg.sightings++
			if agg.firstSeen.IsZero() || m.SeenAt.Before(agg.firstSeen) {
				agg.firstSeen = m.SeenAt
			}
		}
		for id := range compPilotSet(comp) {
			agg.roster[id] = true
		}
		for id, n := range compPilotNames(comp) {
			names[id] = n
		}
		if t, ok := sysLatest[comp.SystemID]; !ok || comp.LatestAt.After(t) {
			sysLatest[comp.SystemID] = comp.LatestAt
		}
	}
	agg.lastSystem = gov.SystemID
	for id := range agg.roster {
		agg.snap.Pilots = append(agg.snap.Pilots, gangPilot{ID: id, Name: names[id]})
	}
	sort.Slice(agg.snap.Pilots, func(i, j int) bool { return agg.snap.Pilots[i].Name < agg.snap.Pilots[j].Name })
	if gov.DScan != nil {
		for _, c := range gov.DScan.Types {
			agg.snap.Hulls = append(agg.snap.Hulls, gangHull{TypeID: c.ID, Name: c.Name, Class: c.Class, N: c.N})
		}
	} else {
		for _, s := range gov.Ships {
			n := s.N
			if n == 0 {
				n = 1
			}
			agg.snap.Hulls = append(agg.snap.Hulls, gangHull{TypeID: s.TypeID, Name: s.Name, Class: s.Class, N: n})
		}
	}
	agg.snap.Counts = gov.Counts
	agg.snap.Likely = gov.Likely
	systems := make([]int64, 0, len(sysLatest))
	for id := range sysLatest {
		systems = append(systems, id)
	}
	sort.Slice(systems, func(i, j int) bool {
		if !sysLatest[systems[i]].Equal(sysLatest[systems[j]]) {
			return sysLatest[systems[i]].Before(sysLatest[systems[j]])
		}
		return systems[i] < systems[j]
	})
	agg.snap.Systems = systems
	return agg
}

// --- Identity ----------------------------------------------------------------

// gangMatch is one candidate gang weighed against a composite.
type gangMatch struct {
	ok          bool
	shared      int
	overlap     float64
	hullOverlap float64
	jumps       int
	gap         time.Duration
	strength    float64
}

// gangMatchOf weighs a candidate: the gate is identified
// pilots (enough shared, and enough of the smaller roster
// shared), a last sighting inside the gang lifetime, and a
// jump distance inside the match bound. Hull overlap never
// gates; it strengthens, because gangs reship.
func gangMatchOf(roster map[int64]bool, comp *intelComposite, cand *trackedGang, candAgg gangAggregate, jumps int, jumpOK bool) gangMatch {
	m := gangMatch{jumps: jumps}
	if !jumpOK {
		m.jumps = gangJumpCap + 1
	}
	for id := range roster {
		if candAgg.roster[id] {
			m.shared++
		}
	}
	smaller := len(roster)
	if len(candAgg.roster) < smaller {
		smaller = len(candAgg.roster)
	}
	if smaller > 0 {
		m.overlap = float64(m.shared) / float64(smaller)
	}
	m.gap = comp.LatestAt.Sub(cand.LastSeen)
	if m.gap < 0 {
		m.gap = -m.gap
	}
	ch, gh := compHullCounts(comp), map[int64]int{}
	for _, h := range candAgg.snap.Hulls {
		if h.TypeID > 0 {
			gh[h.TypeID] += h.N
		}
	}
	sharedHulls, totC, totG := 0, 0, 0
	for id, n := range ch {
		totC += n
		if g, ok := gh[id]; ok {
			if g < n {
				sharedHulls += g
			} else {
				sharedHulls += n
			}
		}
	}
	for _, n := range gh {
		totG += n
	}
	denom := totC
	if totG < denom {
		denom = totG
	}
	if denom > 0 {
		m.hullOverlap = float64(sharedHulls) / float64(denom)
	}
	m.ok = m.shared >= gangMinSharedPilots && m.overlap >= gangMinPilotOverlap &&
		m.gap <= gangLifetime && m.jumps <= gangMatchMaxJumps
	m.strength = gangFoundStrength + (1-gangFoundStrength)*m.overlap*(0.5+0.5*m.hullOverlap)
	return m
}

// betterGangMatch orders two passing matches: more shared
// pilots, then more of the roster, then nearer, then sooner,
// then the older gang.
func betterGangMatch(a, b gangMatch, aID, bID int64) bool {
	switch {
	case a.shared != b.shared:
		return a.shared > b.shared
	case a.overlap != b.overlap:
		return a.overlap > b.overlap
	case a.jumps != b.jumps:
		return a.jumps < b.jumps
	case a.gap != b.gap:
		return a.gap < b.gap
	default:
		return aID < bID
	}
}

// bestGangMatch weighs a composite against the channel's open
// gangs and returns the winner, if any pass the gate. exclude
// names a gang not to weigh (the composite's own).
func (app *Application) bestGangMatch(ctx context.Context, comp *intelComposite, roster map[int64]bool, exclude int64) (*trackedGang, gangMatch) {
	rows, err := app.queries.ListOpenIntelGangs(ctx, db.ListOpenIntelGangsParams{
		ChannelID: comp.ChannelID, MaxRows: gangCandidateCap})
	if err != nil {
		logging.Warnf("intel: gang candidates in channel %d: %v", comp.ChannelID, err)
		return nil, gangMatch{}
	}
	var best *trackedGang
	var bestMatch gangMatch
	for _, row := range rows {
		if row.ID == exclude {
			continue
		}
		cand := gangFromRow(row)
		groups, err := app.queries.ListIntelGangGroups(ctx, cand.ID)
		if err != nil || len(groups) == 0 {
			continue
		}
		cand.Groups = groups
		comps := app.memberComposites(ctx, groups)
		if len(comps) == 0 {
			continue
		}
		agg := gangAggregateOf(comps)
		jumps, jumpOK := app.jumpDistance(ctx, cand.LastSystem, comp.SystemID)
		m := gangMatchOf(roster, comp, cand, agg, jumps, jumpOK)
		if !m.ok {
			continue
		}
		if best == nil || betterGangMatch(m, bestMatch, cand.ID, best.ID) {
			best, bestMatch = cand, m
		}
	}
	return best, bestMatch
}

// evaluateGang runs fusion for a group whose composite just
// landed: stale gangs in the channel close, the composite's
// own gang (if it stands in one) takes the sighting, else the
// open gangs are weighed and the winner takes the group, else
// the sighting founds a new gang.
//
// One refinement on the middle step: reports arrive one paste
// at a time, so a group can found a gang on a partial picture
// (its first report naming a single pilot) before its fuller
// picture lands. Such a gang, one standing on this group
// alone, is provisional: each new composite re-weighs it
// against the channel's other open gangs, and when one matches
// the fuller picture, the group moves to it and the
// provisional gang is deleted (its sightings live on in the
// winner's picture, which is recomputed from the groups
// themselves). A gang standing on two or more groups is
// established and never loses a group this way: one partial
// reading does not tear settled tracking.
//
// A composite naming no identified pilots founds and matches
// nothing: there is no identity to track. evaluateGang never
// blocks or fails the ingest that called it; its errors are
// logged and the picture simply waits for the next sighting.
func (app *Application) evaluateGang(ctx context.Context, comp *intelComposite) {
	if comp == nil || comp.GroupID == 0 || comp.ChannelID == 0 || comp.LatestAt.IsZero() {
		return
	}
	now := time.Now().UTC()
	if _, err := app.queries.CloseStaleIntelGangs(ctx, db.CloseStaleIntelGangsParams{
		ChannelID: comp.ChannelID, Before: now.Add(-gangLifetime),
		LifetimeSecs: int64(gangLifetime / time.Second)}); err != nil {
		logging.Warnf("intel: close stale gangs in channel %d: %v", comp.ChannelID, err)
	}

	current := app.gangForGroup(ctx, comp.GroupID)
	if current != nil && current.Closed {
		// A closed gang's claim on the group does not hold it:
		// the sighting is weighed fresh, and founds anew if
		// nothing open takes it.
		if err := app.queries.DeleteIntelGangGroupMembership(ctx, comp.GroupID); err != nil {
			logging.Warnf("intel: drop closed gang's claim on group %d: %v", comp.GroupID, err)
		}
		current = nil
	}

	roster := compPilotSet(comp)
	if len(roster) == 0 {
		if current != nil {
			app.updateGangPicture(ctx, current, current.Strength)
		}
		return
	}
	if current != nil {
		if len(current.Groups) <= 1 {
			if winner, m := app.bestGangMatch(ctx, comp, roster, current.ID); winner != nil {
				if err := app.queries.AddIntelGangGroup(ctx, db.AddIntelGangGroupParams{
					GangID: winner.ID, GroupID: comp.GroupID}); err != nil {
					logging.Warnf("intel: group %d moves to gang %d: %v", comp.GroupID, winner.ID, err)
					return
				}
				if err := app.queries.DeleteIntelGang(ctx, current.ID); err != nil {
					logging.Warnf("intel: drop provisional gang %d: %v", current.ID, err)
				}
				strength := winner.Strength
				if m.strength > strength {
					strength = m.strength
				}
				if current.Strength > strength {
					strength = current.Strength
				}
				app.updateGangPicture(ctx, winner, strength)
				return
			}
		}
		app.updateGangPicture(ctx, current, current.Strength)
		return
	}

	best, bestMatch := app.bestGangMatch(ctx, comp, roster, 0)
	if best != nil {
		if err := app.queries.AddIntelGangGroup(ctx, db.AddIntelGangGroupParams{
			GangID: best.ID, GroupID: comp.GroupID}); err != nil {
			logging.Warnf("intel: group %d joins gang %d: %v", comp.GroupID, best.ID, err)
			return
		}
		strength := best.Strength
		if bestMatch.strength > strength {
			strength = bestMatch.strength
		}
		app.updateGangPicture(ctx, best, strength)
		return
	}

	agg := gangAggregateOf([]*intelComposite{comp})
	raw, err := json.Marshal(agg.snap)
	if err != nil {
		return
	}
	id, err := app.queries.InsertIntelGang(ctx, db.InsertIntelGangParams{
		ChannelID: comp.ChannelID, FirstSeenAt: agg.firstSeen, LastSeenAt: agg.lastSeen,
		LastSystemID: agg.lastSystem, Sightings: int32(agg.sightings),
		Strength: gangFoundStrength, Snapshot: string(raw)})
	if err != nil {
		logging.Warnf("intel: found gang for group %d: %v", comp.GroupID, err)
		return
	}
	if err := app.queries.AddIntelGangGroup(ctx, db.AddIntelGangGroupParams{
		GangID: id, GroupID: comp.GroupID}); err != nil {
		logging.Warnf("intel: group %d joins its gang %d: %v", comp.GroupID, id, err)
	}
}

// updateGangPicture recomputes a gang's stored picture from
// its member groups' composites and writes it back, with the
// given identity strength.
func (app *Application) updateGangPicture(ctx context.Context, g *trackedGang, strength float64) {
	groups, err := app.queries.ListIntelGangGroups(ctx, g.ID)
	if err != nil || len(groups) == 0 {
		return
	}
	comps := app.memberComposites(ctx, groups)
	if len(comps) == 0 {
		return
	}
	agg := gangAggregateOf(comps)
	first := g.FirstSeen
	if first.IsZero() || agg.firstSeen.Before(first) {
		first = agg.firstSeen
	}
	raw, err := json.Marshal(agg.snap)
	if err != nil {
		return
	}
	if err := app.queries.UpdateIntelGang(ctx, db.UpdateIntelGangParams{
		ID: g.ID, FirstSeenAt: first, LastSeenAt: agg.lastSeen,
		LastSystemID: agg.lastSystem, Sightings: int32(agg.sightings),
		Strength: strength, Snapshot: string(raw)}); err != nil {
		logging.Warnf("intel: gang %d picture: %v", g.ID, err)
		return
	}
	g.FirstSeen, g.LastSeen, g.LastSystem = first, agg.lastSeen, agg.lastSystem
	g.Sightings, g.Strength, g.Snap, g.Groups = agg.sightings, strength, agg.snap, groups
}

// memberComposites loads a gang's member groups' composites,
// bounded as every ingest read is.
func (app *Application) memberComposites(ctx context.Context, groupIDs []int64) []*intelComposite {
	if len(groupIDs) > gangMemberCap {
		groupIDs = groupIDs[:gangMemberCap]
	}
	byID := app.groupCompositesFor(ctx, groupIDs)
	out := make([]*intelComposite, 0, len(byID))
	ids := make([]int64, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		out = append(out, byID[id])
	}
	return out
}

// gangForGroup reads the gang a group currently stands in, if
// any, with its member groups listed.
func (app *Application) gangForGroup(ctx context.Context, groupID int64) *trackedGang {
	m := app.gangsForGroups(ctx, []int64{groupID})
	return m[groupID]
}

// gangsForGroups reads the gangs standing behind several
// groups in one read, keyed by group id: the loader the feed,
// the channel pages and the workbench share.
func (app *Application) gangsForGroups(ctx context.Context, groupIDs []int64) map[int64]*trackedGang {
	out := map[int64]*trackedGang{}
	if len(groupIDs) == 0 {
		return out
	}
	rows, err := app.queries.ListIntelGangsByGroups(ctx, groupIDs)
	if err != nil {
		logging.Warnf("intel: gangs by groups: %v", err)
		return out
	}
	byID := map[int64]*trackedGang{}
	for _, r := range rows {
		g := byID[r.ID]
		if g == nil {
			g = gangFromRow(db.IntelGang{
				ID: r.ID, ChannelID: r.ChannelID, FirstSeenAt: r.FirstSeenAt,
				LastSeenAt: r.LastSeenAt, LastSystemID: r.LastSystemID,
				Sightings: r.Sightings, Strength: r.Strength,
				ClosedAt: r.ClosedAt, Snapshot: r.Snapshot})
			byID[r.ID] = g
		}
		g.Groups = append(g.Groups, r.GroupID)
		out[r.GroupID] = g
	}
	return out
}

// --- Jump distance -----------------------------------------------------------

// jumpDistance is the gate distance between two systems, by
// breadth-first search over the stargate graph, bounded at
// gangJumpCap. ok is false when the distance is unknown: the
// graph unread, a system outside it, or farther than the cap.
// The same system is no distance, graph or none.
func (app *Application) jumpDistance(ctx context.Context, from, to int64) (int, bool) {
	if from == to {
		return 0, true
	}
	if from == 0 || to == 0 {
		return 0, false
	}
	graph := app.gateGraphLoad(ctx)
	if graph == nil {
		return 0, false
	}
	seen := map[int64]int{from: 0}
	frontier := []int64{from}
	for depth := 1; depth <= gangJumpCap; depth++ {
		var next []int64
		for _, sys := range frontier {
			for _, nb := range graph[sys] {
				if _, ok := seen[nb]; ok {
					continue
				}
				if nb == to {
					return depth, true
				}
				seen[nb] = depth
				next = append(next, nb)
			}
		}
		if len(next) == 0 {
			return 0, false
		}
		frontier = next
	}
	return 0, false
}

// gateGraphLoad returns the stargate adjacency, loading it
// once from the SDE jumps. The SDE is static per deploy, so
// the graph is kept for the process's life; a failed load is
// remembered and distances read as unknown rather than
// retried on every sighting.
func (app *Application) gateGraphLoad(ctx context.Context) map[int64][]int64 {
	app.gateGraphMu.Lock()
	defer app.gateGraphMu.Unlock()
	if app.gateGraphDone {
		return app.gateGraph
	}
	app.gateGraphDone = true
	rows, err := app.queries.ListMapJumps(ctx)
	if err != nil {
		logging.Warnf("intel: gate graph: %v", err)
		return nil
	}
	graph := make(map[int64][]int64, len(rows)*2)
	for _, r := range rows {
		graph[r.A] = append(graph[r.A], r.B)
		graph[r.B] = append(graph[r.B], r.A)
	}
	app.gateGraph = graph
	return graph
}

// --- Likely ships from profiles ----------------------------------------------

// groupProfileLikely attributes ships to the pilots a
// composite names without one, from their stored activity
// profiles: a hull is attributed when the pilot's record
// demonstrates it (its share of their final blows clears
// gangLikelyMinProb) and instances of it stand unclaimed on
// the governing d-scan after the fact links and the
// elimination link took theirs. Each attribution is stored as
// likely, with its probability and its basis written out. One
// attribution per pilot, the strongest first; a single
// unclaimed hull goes to one pilot only. A pilot with no
// profile, or none of their demonstrated hulls unclaimed,
// gets nothing.
func (app *Application) groupProfileLikely(ctx context.Context, comp *intelComposite) []compLikely {
	if comp.DScan == nil || comp.Local == nil || len(comp.Local.Pilots) == 0 {
		return nil
	}
	// The unclaimed pool, by type: the d-scan's tally less
	// what the fact links and the elimination link claimed.
	pool := map[int64]int{}
	nameOf := map[int64]string{}
	for _, c := range comp.DScan.Types {
		if c.ID > 0 {
			pool[c.ID] += c.N
			nameOf[c.ID] = c.Name
		}
	}
	idByName := map[string]int64{}
	for id, name := range nameOf {
		idByName[name] = id
	}
	linkedID := map[int64]bool{}
	linkedName := map[string]bool{}
	claim := func(l compLink) {
		linkedName[strings.ToLower(l.PilotName)] = true
		if l.PilotID > 0 {
			linkedID[l.PilotID] = true
		}
		if l.TypeID > 0 {
			pool[l.TypeID]--
		} else if id, ok := idByName[l.ShipName]; ok {
			pool[id]--
		}
	}
	for _, l := range comp.Links {
		claim(l)
	}
	for _, l := range comp.Likely {
		linkedName[strings.ToLower(l.PilotName)] = true
		if l.PilotID > 0 {
			linkedID[l.PilotID] = true
		}
		if l.TypeID > 0 {
			pool[l.TypeID]--
		} else if id, ok := idByName[l.ShipName]; ok {
			pool[id]--
		}
	}
	open := 0
	for _, n := range pool {
		if n > 0 {
			open += n
		}
	}
	if open == 0 {
		return nil
	}

	type cand struct {
		pilot     localPilot
		typeID    int64
		prob      float64
		unclaimed int
	}
	var cands []cand
	checked := 0
	for _, lp := range comp.Local.Pilots {
		if lp.ID <= 0 || linkedID[lp.ID] || linkedName[strings.ToLower(lp.Name)] {
			continue
		}
		if checked >= gangLikelyPilotCap {
			break
		}
		checked++
		prof, err := app.queries.GetPilotActivityProfile(ctx, lp.ID)
		if err != nil || prof.FinalBlows <= 0 {
			continue
		}
		rows, err := app.queries.ListPilotActivityShips(ctx, db.ListPilotActivityShipsParams{
			CharacterID: lp.ID, MaxRows: 8})
		if err != nil {
			continue
		}
		var best *cand
		for _, r := range rows {
			if r.FinalBlows <= 0 || pool[r.ShipTypeID] <= 0 {
				continue
			}
			share := float64(r.FinalBlows) / float64(prof.FinalBlows)
			if share < gangLikelyMinProb {
				continue
			}
			if best == nil || share > best.prob {
				c := cand{pilot: lp, typeID: r.ShipTypeID, prob: share, unclaimed: pool[r.ShipTypeID]}
				best = &c
			}
		}
		if best != nil {
			cands = append(cands, *best)
		}
	}
	if len(cands) == 0 {
		return nil
	}
	// The strongest attributions take the pool first; a hull
	// with one unclaimed instance goes to one pilot.
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].prob != cands[j].prob {
			return cands[i].prob > cands[j].prob
		}
		return cands[i].pilot.Name < cands[j].pilot.Name
	})
	var out []compLikely
	for _, c := range cands {
		if pool[c.typeID] <= 0 {
			continue
		}
		pool[c.typeID]--
		ship := nameOf[c.typeID]
		out = append(out, compLikely{
			PilotID: c.pilot.ID, PilotName: c.pilot.Name,
			ShipName: ship, TypeID: c.typeID, Prob: c.prob,
			Basis: fmt.Sprintf("Usually flies %s (%d%% of final blows on their record); %d %s on scan unclaimed.",
				ship, int(math.Round(100*c.prob)), c.unclaimed, ship),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PilotName < out[j].PilotName })
	return out
}

// --- The event words ---------------------------------------------------------

// gangCompLine is a gang's composition in words: its leading
// hulls with their counts, pilots when no hull face stands.
func gangCompLine(snap gangSnapshot) string {
	if len(snap.Hulls) == 0 {
		if n := len(snap.Pilots); n > 0 {
			return plural(n, "pilot")
		}
		return ""
	}
	hulls := append([]gangHull(nil), snap.Hulls...)
	sort.SliceStable(hulls, func(i, j int) bool { return hulls[i].N > hulls[j].N })
	var parts []string
	for i, h := range hulls {
		if i >= 4 {
			parts = append(parts, fmt.Sprintf("+%d more", len(hulls)-4))
			break
		}
		if h.N > 1 {
			parts = append(parts, fmt.Sprintf("%s ×%d", h.Name, h.N))
		} else {
			parts = append(parts, h.Name)
		}
	}
	return strings.Join(parts, ", ")
}

// gangDoctrineLabel names a gang's doctrine from its hull
// classes, in the workbench's own flag words, or says nothing:
// a composition that supports no flag is described by its
// hulls instead (doctrineFlags in intel_workbench.go).
func gangDoctrineLabel(snap gangSnapshot) string {
	roleN := map[string]int{}
	for _, h := range snap.Hulls {
		roleN[wbRoleOf(h.Class)] += h.N
	}
	return strings.Join(doctrineFlags(roleN), " + ")
}

// gangLikelyLine words one likely attribution for a card:
// "Likely: Pilot Two in a Guardian (62%)".
func gangLikelyLine(l compLikely) string {
	s := "Likely: " + l.PilotName + " in a " + l.ShipName
	if l.Prob > 0 {
		s += fmt.Sprintf(" (%d%%)", int(math.Round(100*l.Prob)))
	}
	return s
}

// reportGang is a tracked gang as a feed or channel card shows
// it: the event line's parts, composed from the stored gang at
// render.
type reportGang struct {
	Title    string `json:"title"`
	Comp     string `json:"comp,omitempty"`
	Doctrine string `json:"doctrine,omitempty"`
	Seen     string `json:"seen"`
	Likely   string `json:"likely,omitempty"`
	Conf     int    `json:"conf"`
	Stale    bool   `json:"stale,omitempty"`
}

// reportGangView composes a gang's card line from its stored
// row: composition, doctrine where the hulls name one, last
// seen, the strongest likely attribution, and the confidence
// decayed to the system the card is about.
func (app *Application) reportGangView(ctx context.Context, g *trackedGang, contextSystem int64, now time.Time) *reportGang {
	jumps, jumpOK := app.jumpDistance(ctx, g.LastSystem, contextSystem)
	conf := gangConfidenceAt(g, now, jumps, jumpOK)
	rg := &reportGang{
		Title: "Tracked gang",
		Comp:  gangCompLine(g.Snap),
		Seen:  "last seen " + ageText(now, g.LastSeen),
		Conf:  int(math.Round(100 * conf)),
	}
	if conf < gangStaleConfidence {
		rg.Title = "Gang gone quiet"
		rg.Stale = true
	}
	rg.Doctrine = gangDoctrineLabel(g.Snap)
	if len(g.Snap.Likely) > 0 {
		rg.Likely = gangLikelyLine(g.Snap.Likely[0])
	}
	return rg
}
