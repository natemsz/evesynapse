package app

// ---------------------------------------------------------------------------
// Corp pulse: what changed in the corporation, not its current
// state (the corp pages show that). Members joined (tracking
// start dates) and left (seen-state diffs), wallet movements
// (recent significant journal entries per division), and
// structures arriving, leaving, or running a timer. Roles
// changed is deliberately absent: ESI exposes only the viewer's
// own roles, not a per-member roles history.
//
// The worker records departures/arrivals into pulse_events by
// diffing already-stored corp snapshots: no ESI traffic of its
// own. Every section is permission-gated on the in-game role
// that unlocks its endpoint, unioned across the viewer's
// characters in the corporation; a section with no qualifying
// role simply does not render.
// ---------------------------------------------------------------------------

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// Pulse event kinds in pulse_events.
const (
	pulseMemberLeft     = "member_left"
	pulseStructureAdded = "structure_added"
	pulseStructureGone  = "structure_removed"
)

// pulseSeenKinds are the diffed ID sets in pulse_seen.
const (
	pulseSeenMember    = "member"
	pulseSeenStructure = "structure"
)

// pulseJoinedDays bounds "recently joined" by tracking start
// date; pulseEventDays bounds recorded departures/arrivals;
// pulsePruneDays bounds event retention.
const (
	pulseJoinedDays = 7
	pulseEventDays  = 30
	pulsePruneDays  = 90
)

// pulseMovesCap caps the wallet movements shown per
// corporation; pulseMoveMinISK floors them so pocket change
// never pages the widget.
const (
	pulseMovesCap   = 5
	pulseMoveMinISK = 1000000.0
)

// holdsAnyRole reports whether roles include any of the wanted
// in-game corporation roles (case-insensitive, like
// holdsDirector).
func holdsAnyRole(roles []string, wanted ...string) bool {
	for _, role := range roles {
		for _, w := range wanted {
			if strings.EqualFold(role, w) {
				return true
			}
		}
	}
	return false
}

// pulseCorp is one corporation's data for a diff or a render:
// its id, one linked character holding its snapshots, the
// union of the viewer's roles across their characters in it,
// and those characters' names for best-effort member naming.
func (app *Application) pulseCorps(ctx context.Context, characters []db.Character) []pulseCorpRef {
	type corpAcc struct {
		linkChar int64
		roles    map[string]bool
		names    map[int64]string
	}
	byCorp := make(map[int64]*corpAcc)
	var order []int64
	for _, ch := range characters {
		var pub esi.Character
		if !app.loadCorpSnapshot(ctx, ch.CharacterID, esi.SnapProfile, &pub) || pub.CorporationID == 0 {
			continue
		}
		acc := byCorp[pub.CorporationID]
		if acc == nil {
			acc = &corpAcc{roles: make(map[string]bool), names: make(map[int64]string)}
			byCorp[pub.CorporationID] = acc
			order = append(order, pub.CorporationID)
		}
		if acc.linkChar == 0 {
			acc.linkChar = ch.CharacterID
		}
		if ch.Name != "" {
			acc.names[ch.CharacterID] = ch.Name
		}
		var roles esi.CharacterRoles
		if app.loadCorpSnapshot(ctx, ch.CharacterID, esi.SnapCorpRoles, &roles) {
			for _, r := range roles.Roles {
				acc.roles[r] = true
			}
		}
	}
	var out []pulseCorpRef
	for _, corpID := range order {
		acc := byCorp[corpID]
		var roles []string
		for r := range acc.roles {
			roles = append(roles, r)
		}
		out = append(out, pulseCorpRef{CorpID: corpID, LinkChar: acc.linkChar, Roles: roles, Names: acc.names})
	}
	return out
}

// pulseCorpRef hands one corporation to the pulse worker and
// the pulse widget.
type pulseCorpRef struct {
	CorpID   int64
	LinkChar int64
	Roles    []string
	Names    map[int64]string
}

// pulseCorpName resolves a corporation's display name from its
// stored public record, falling back to the bare id.
func (app *Application) pulseCorpName(ctx context.Context, ref pulseCorpRef) string {
	var corp esi.Corporation
	if app.loadCorpSnapshot(ctx, ref.LinkChar, esi.SnapCorpInfo, &corp) && corp.Name != "" {
		return corp.Name
	}
	return "Corporation #" + strconv.FormatInt(ref.CorpID, 10)
}

// refreshPulseEvents diffs stored corp snapshots against the
// seen sets and records departures, arrivals, and removals.
// Local reads and writes only; it runs after the cycle's
// fetches are stored. It reports recorded events.
func (app *Application) refreshPulseEvents(ctx context.Context, characters []db.Character) (stored int, limited bool) {
	now := time.Now().UTC()
	names := make(map[int64]string, len(characters))
	for _, ch := range characters {
		if ch.Name != "" {
			names[ch.CharacterID] = ch.Name
		}
	}
	for _, ref := range app.pulseCorps(ctx, characters) {
		stored += app.diffPulseMembers(ctx, ref, names, now)
		stored += app.diffPulseStructures(ctx, ref, now)
	}
	if err := app.queries.PrunePulseEvents(ctx, now.Add(-pulsePruneDays*24*time.Hour)); err != nil {
		logging.Errorf("pulse: prune events: %v", err)
	}
	return stored, false
}

// diffPulseMembers records member departures: seen IDs missing
// from the current member list. Current IDs (re)enter the seen
// set with their best-known name.
func (app *Application) diffPulseMembers(ctx context.Context, ref pulseCorpRef, names map[int64]string, now time.Time) int {
	var members esi.CorpMembers
	if !app.loadCorpSnapshot(ctx, ref.LinkChar, esi.SnapCorpMembers, &members) {
		return 0
	}
	current := make(map[string]bool, len(members))
	for _, id := range members {
		current[strconv.FormatInt(id, 10)] = true
	}
	seen, err := app.queries.ListPulseSeen(ctx, db.ListPulseSeenParams{CorpID: ref.CorpID, Kind: pulseSeenMember})
	if err != nil {
		logging.Errorf("pulse: list seen members for corp %d: %v", ref.CorpID, err)
		return 0
	}
	recorded := 0
	for _, s := range seen {
		if !current[s.Key] {
			if err := app.queries.InsertPulseEvent(ctx, db.InsertPulseEventParams{
				CorpID: ref.CorpID, Kind: pulseMemberLeft, Key: s.Key, Name: s.Name, At: now,
			}); err != nil {
				logging.Errorf("pulse: record departure for corp %d: %v", ref.CorpID, err)
				continue
			}
			if err := app.queries.DeletePulseSeen(ctx, db.DeletePulseSeenParams{
				CorpID: ref.CorpID, Kind: pulseSeenMember, Key: s.Key,
			}); err != nil {
				logging.Errorf("pulse: drop seen member for corp %d: %v", ref.CorpID, err)
			}
			recorded++
		}
	}
	for _, id := range members {
		key := strconv.FormatInt(id, 10)
		name, ok := names[id]
		if !ok || name == "" {
			name = "Character #" + key
		}
		if err := app.queries.UpsertPulseSeen(ctx, db.UpsertPulseSeenParams{
			CorpID: ref.CorpID, Kind: pulseSeenMember, Key: key, Name: name,
		}); err != nil {
			logging.Errorf("pulse: note seen member for corp %d: %v", ref.CorpID, err)
		}
	}
	return recorded
}

// diffPulseStructures records structure arrivals and removals
// the same way, keyed by structure id with the payload's name.
func (app *Application) diffPulseStructures(ctx context.Context, ref pulseCorpRef, now time.Time) int {
	var structures esi.CorpStructures
	if !app.loadCorpSnapshot(ctx, ref.LinkChar, esi.SnapCorpStructures, &structures) {
		return 0
	}
	current := make(map[string]string, len(structures))
	for _, s := range structures {
		name := s.Name
		if name == "" {
			name = "Structure #" + strconv.FormatInt(s.StructureID, 10)
		}
		current[strconv.FormatInt(s.StructureID, 10)] = name
	}
	seen, err := app.queries.ListPulseSeen(ctx, db.ListPulseSeenParams{CorpID: ref.CorpID, Kind: pulseSeenStructure})
	if err != nil {
		logging.Errorf("pulse: list seen structures for corp %d: %v", ref.CorpID, err)
		return 0
	}
	// First sight adopts the current set silently: everything
	// present would otherwise report as freshly anchored.
	adopting := len(seen) == 0
	recorded := 0
	for _, s := range seen {
		if _, ok := current[s.Key]; !ok {
			if err := app.queries.InsertPulseEvent(ctx, db.InsertPulseEventParams{
				CorpID: ref.CorpID, Kind: pulseStructureGone, Key: s.Key, Name: s.Name, At: now,
			}); err != nil {
				logging.Errorf("pulse: record structure removal for corp %d: %v", ref.CorpID, err)
				continue
			}
			if err := app.queries.DeletePulseSeen(ctx, db.DeletePulseSeenParams{
				CorpID: ref.CorpID, Kind: pulseSeenStructure, Key: s.Key,
			}); err != nil {
				logging.Errorf("pulse: drop seen structure for corp %d: %v", ref.CorpID, err)
			}
			recorded++
		}
	}
	for key, name := range current {
		known := false
		for _, s := range seen {
			if s.Key == key {
				known = true
				break
			}
		}
		if known {
			continue
		}
		if err := app.queries.UpsertPulseSeen(ctx, db.UpsertPulseSeenParams{
			CorpID: ref.CorpID, Kind: pulseSeenStructure, Key: key, Name: name,
		}); err != nil {
			logging.Errorf("pulse: note seen structure for corp %d: %v", ref.CorpID, err)
			continue
		}
		if !adopting {
			if err := app.queries.InsertPulseEvent(ctx, db.InsertPulseEventParams{
				CorpID: ref.CorpID, Kind: pulseStructureAdded, Key: key, Name: name, At: now,
			}); err != nil {
				logging.Errorf("pulse: record structure arrival for corp %d: %v", ref.CorpID, err)
				continue
			}
			recorded++
		}
	}
	return recorded
}

// ---------------------------------------------------------------------------
// Widget. One gated section per corporation the viewer may see.
// ---------------------------------------------------------------------------

// pulseJoin is a recently joined member (tracking start date).
type pulseJoin struct {
	Name   string
	Joined string
}

// pulseMove is one significant wallet movement: division,
// signed amount, and the journal line behind it.
type pulseMove struct {
	Division int64
	Text     string
	When     string
}

// pulseChange is one recorded departure, arrival, or removal.
type pulseChange struct {
	Text string
	When string
}

// pulseTimer is a structure running a clock: unanchoring or a
// pending state transition.
type pulseTimer struct {
	Name string
	Text string
	When string
}

// pulseCorpView is one corporation's visible pulse.
type pulseCorpView struct {
	Name    string
	Joined  []pulseJoin
	Left    []pulseChange
	Moves   []pulseMove
	Added   []pulseChange
	Removed []pulseChange
	Timers  []pulseTimer
}

// pulseWidget lists one view per visible corporation; empty
// means no qualifying role anywhere.
type pulseWidget struct {
	Corps []pulseCorpView
}

// pulseMemberRoles unlock the members/tracking sections,
// pulseWalletRoles the wallet movements, pulseStructureRoles
// the structure sections: the in-game roles behind each ESI
// endpoint the pulse reads.
var (
	pulseMemberRoles    = []string{"Director", "Personnel_Manager"}
	pulseWalletRoles    = []string{"Director", "Accountant", "Junior Accountant"}
	pulseStructureRoles = []string{"Director", "Station_Manager"}
)

// buildPulse assembles the widget for one user: their
// corporations, each section gated on the union of their
// characters' roles there. Rendered reads only.
func (app *Application) buildPulse(ctx context.Context, userID int64) *pulseWidget {
	w := &pulseWidget{}
	characters, err := app.queries.ListCharactersByUser(ctx, userID)
	if err != nil {
		logging.Errorf("pulse: list characters for user %d: %v", userID, err)
		return w
	}
	now := time.Now().UTC()
	for _, ref := range app.pulseCorps(ctx, characters) {
		members := holdsAnyRole(ref.Roles, pulseMemberRoles...)
		wallet := holdsAnyRole(ref.Roles, pulseWalletRoles...)
		structs := holdsAnyRole(ref.Roles, pulseStructureRoles...)
		if !members && !wallet && !structs {
			continue
		}
		view := pulseCorpView{Name: app.pulseCorpName(ctx, ref)}
		if members {
			view.Joined = app.pulseJoined(ctx, ref, now)
			view.Left, _ = app.pulseChanges(ctx, ref.CorpID, pulseMemberLeft, now)
		}
		if wallet {
			view.Moves = app.pulseMoves(ctx, ref, now)
		}
		if structs {
			view.Added, _ = app.pulseChanges(ctx, ref.CorpID, pulseStructureAdded, now)
			view.Removed, _ = app.pulseChanges(ctx, ref.CorpID, pulseStructureGone, now)
			view.Timers = app.pulseTimers(ctx, ref)
		}
		w.Corps = append(w.Corps, view)
	}
	return w
}

// pulseJoined lists members whose tracking start date is
// within pulseJoinedDays. Names resolve for linked characters;
// other members show by id (tracking carries no names).
func (app *Application) pulseJoined(ctx context.Context, ref pulseCorpRef, now time.Time) []pulseJoin {
	var tracking esi.CorpMemberTrackings
	if !app.loadCorpSnapshot(ctx, ref.LinkChar, esi.SnapCorpMemberTracking, &tracking) {
		return nil
	}
	names := ref.Names
	if names == nil {
		names = make(map[int64]string)
	}
	var out []pulseJoin
	for _, t := range tracking {
		start, err := time.Parse(time.RFC3339, t.StartDate)
		if err != nil || now.Sub(start) < 0 || now.Sub(start) > pulseJoinedDays*24*time.Hour {
			continue
		}
		name, ok := names[t.CharacterID]
		if !ok || name == "" {
			name = "Character #" + strconv.FormatInt(t.CharacterID, 10)
		}
		out = append(out, pulseJoin{Name: name, Joined: formatFinish(t.StartDate)})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if len(out) > 5 {
		out = out[:5]
	}
	return out
}

// pulseChanges reads recorded events of one kind within
// pulseEventDays, newest first.
func (app *Application) pulseChanges(ctx context.Context, corpID int64, kind string, now time.Time) ([]pulseChange, bool) {
	rows, err := app.queries.ListPulseEvents(ctx, db.ListPulseEventsParams{
		CorpID: corpID, At: now.Add(-pulseEventDays * 24 * time.Hour),
	})
	if err != nil {
		logging.Errorf("pulse: list events for corp %d: %v", corpID, err)
		return nil, false
	}
	var out []pulseChange
	for _, r := range rows {
		if r.Kind != kind {
			continue
		}
		out = append(out, pulseChange{Text: r.Name, When: formatFinish(r.At.UTC().Format(time.RFC3339))})
		if len(out) >= 5 {
			break
		}
	}
	return out, len(out) > 0
}

// pulseMoves lists the largest recent journal movements across
// the corporation's divisions: what moved the wallet, not its
// balance (the corp wallet pages show that).
func (app *Application) pulseMoves(ctx context.Context, ref pulseCorpRef, now time.Time) []pulseMove {
	var wallets esi.CorpWallets
	if !app.loadCorpSnapshot(ctx, ref.LinkChar, esi.SnapCorpWallets, &wallets) {
		return nil
	}
	type candidate struct {
		move pulseMove
		abs  float64
		at   time.Time
	}
	var all []candidate
	for _, div := range wallets {
		var journal esi.CorpJournal
		if !app.loadCorpSnapshot(ctx, ref.LinkChar, esi.CorpJournalKind(div.Division), &journal) {
			continue
		}
		for _, e := range journal {
			at, err := time.Parse(time.RFC3339, e.Date)
			if err != nil || now.Sub(at) < 0 || now.Sub(at) > pulseJoinedDays*24*time.Hour {
				continue
			}
			abs := e.Amount
			if abs < 0 {
				abs = -abs
			}
			if abs < pulseMoveMinISK {
				continue
			}
			text := e.RefType
			if e.Description != "" {
				text += " — " + e.Description
			}
			all = append(all, candidate{
				move: pulseMove{
					Division: div.Division,
					Text:     text + " (" + esi.FormatISK(e.Amount) + " ISK)",
					When:     formatFinish(e.Date),
				},
				abs: abs,
				at:  at,
			})
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].abs > all[j].abs })
	var out []pulseMove
	for i, c := range all {
		if i >= pulseMovesCap {
			break
		}
		out = append(out, c.move)
	}
	return out
}

// pulseTimers lists structures running a clock: unanchoring or
// heading into/out of a state.
func (app *Application) pulseTimers(ctx context.Context, ref pulseCorpRef) []pulseTimer {
	var structures esi.CorpStructures
	if !app.loadCorpSnapshot(ctx, ref.LinkChar, esi.SnapCorpStructures, &structures) {
		return nil
	}
	var out []pulseTimer
	for _, s := range structures {
		name := s.Name
		if name == "" {
			name = "Structure #" + strconv.FormatInt(s.StructureID, 10)
		}
		if s.UnanchorsAt != "" {
			out = append(out, pulseTimer{Name: name, Text: "unanchoring", When: formatFinish(s.UnanchorsAt)})
		} else if s.StateTimerEnd != "" {
			out = append(out, pulseTimer{Name: name, Text: "state change", When: formatFinish(s.StateTimerEnd)})
		}
		if len(out) >= 5 {
			break
		}
	}
	return out
}
