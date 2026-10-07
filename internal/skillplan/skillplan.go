// Package skillplan is the arithmetic behind skill plans: how many
// skill points a level takes, how fast a character trains, the order
// a set of targets has to be trained in once prerequisites are
// added, and which attribute remap would shorten it. It is pure: the
// pages hand it a skill graph and a character's state and it does no
// I/O of its own.
package skillplan

import (
	"math"
	"sort"
	"time"
)

// ---------------------------------------------------------------------------
// Skill plan engine (Phase 4). Pure functions over three inputs the
// handlers assemble from local state: the SDE skill graph (schema
// 012: rank/attributes per skill, required-skill rows per type), a
// character's trained skills + queue (snapshots), and their current
// attributes (attributes snapshot). Nothing here touches the DB or
// the network.
//
// EVE training math:
//   - cumulative SP for level L of a rank-r skill is
//     round(250 × r × 2^(2.5×(L−1))): 250 / 1,414 / 8,000 / 45,255 /
//     256,000 at rank 1 (the 2^2.5 step is what the game rounds);
//   - training speed for a skill is (primary + secondary/2) SP
//     per minute against the character's current attributes.
// ---------------------------------------------------------------------------

// Dogma character-attribute IDs (dgmAttributeTypes 164-168), the
// values sde_skill_meta's primary/secondary columns hold.
const (
	AttrCharisma     = 164
	AttrIntelligence = 165
	AttrMemory       = 166
	AttrPerception   = 167
	AttrWillpower    = 168
)

// AttributeName names a dogma attribute ID for display.
func AttributeName(id int64) string {
	switch id {
	case AttrCharisma:
		return "Charisma"
	case AttrIntelligence:
		return "Intelligence"
	case AttrMemory:
		return "Memory"
	case AttrPerception:
		return "Perception"
	case AttrWillpower:
		return "Willpower"
	}
	return "—"
}

// AttributeIDs lists the five training attributes in canonical order.
var AttributeIDs = []int64{AttrCharisma, AttrIntelligence, AttrMemory, AttrPerception, AttrWillpower}

// AttrSet is a character's five training attribute values.
type AttrSet struct {
	Charisma     int
	Intelligence int
	Memory       int
	Perception   int
	Willpower    int
}

// FlatAttrSet is the honest fallback while the attributes snapshot
// is still warming: EVE's unmodified spread is 20 across the board.
var FlatAttrSet = AttrSet{Charisma: 20, Intelligence: 20, Memory: 20, Perception: 20, Willpower: 20}

func (a AttrSet) value(id int64) int {
	switch id {
	case AttrCharisma:
		return a.Charisma
	case AttrIntelligence:
		return a.Intelligence
	case AttrMemory:
		return a.Memory
	case AttrPerception:
		return a.Perception
	case AttrWillpower:
		return a.Willpower
	}
	return 0
}

func (a AttrSet) with(id int64, v int) AttrSet {
	switch id {
	case AttrCharisma:
		a.Charisma = v
	case AttrIntelligence:
		a.Intelligence = v
	case AttrMemory:
		a.Memory = v
	case AttrPerception:
		a.Perception = v
	case AttrWillpower:
		a.Willpower = v
	}
	return a
}

// SkillMeta is one skill's training profile from sde_skill_meta.
type SkillMeta struct {
	Rank      float64
	Primary   int64 // dogma attribute ID
	Secondary int64
}

// Requirement is one required-skill row: to use/train the
// type, SkillID must be trained to Level.
type Requirement struct {
	SkillID int64
	Level   int
}

// Graph supplies the plan engine's static data. The handler
// backs it with the SDE tables (memoized per render); tests serve
// fixtures from maps.
type Graph interface {
	Meta(skillID int64) (SkillMeta, bool)
	Requirements(typeID int64) []Requirement
}

// SPForLevel is the cumulative SP at which level completes for a
// skill of the given rank (level 0 = 0 SP).
func SPForLevel(rank float64, level int) int64 {
	if level <= 0 {
		return 0
	}
	if level > 5 {
		level = 5
	}
	if rank <= 0 {
		rank = 1
	}
	return int64(math.Round(250 * rank * math.Pow(2, 2.5*float64(level-1))))
}

// LevelForSP is the highest completed level for sp in a skill.
func LevelForSP(rank float64, sp int64) int {
	level := 0
	for l := 1; l <= 5; l++ {
		if sp >= SPForLevel(rank, l) {
			level = l
		}
	}
	return level
}

// spPerMinute is the training speed of one skill under attrs.
func spPerMinute(meta SkillMeta, attrs AttrSet) float64 {
	return float64(attrs.value(meta.Primary)) + float64(attrs.value(meta.Secondary))/2
}

// trainSeconds is how long spRemaining takes to train in one skill.
func trainSeconds(meta SkillMeta, attrs AttrSet, spRemaining int64) float64 {
	rate := spPerMinute(meta, attrs)
	if rate <= 0 || spRemaining <= 0 {
		return 0
	}
	return float64(spRemaining) / rate * 60
}

// ---------------------------------------------------------------------------
// Plan computation.
// ---------------------------------------------------------------------------

// CharTraining is the character-side input: current SP per skill,
// the level each skill reaches when the live queue drains, when
// that happens, and the unallocated-SP total (reported, never
// silently spent).
type CharTraining struct {
	SP          map[int64]int64
	QueuedTo    map[int64]int // skill → highest level the queue completes
	QueueEnd    time.Time     // zero when the queue is empty or already past
	Unallocated int64
}

// Target is one skill the user wants at a level; Intent is the
// plan item's position (lower = asked for earlier). PrereqOf (set
// during expansion) records which direct target pulled a skill in.
type Target struct {
	SkillID int64
	Level   int
	Intent  int
}

// Step is one trainable chunk of a computed plan: from the
// character's effective position after the queue drains to the
// target level.
type Step struct {
	SkillID     int64
	FromLevel   int
	ToLevel     int
	SPRemaining int64
	Seconds     float64
	Finish      time.Time // cumulative, starting when the queue drains
	Prereq      bool      // pulled in as a prerequisite, not a direct target
}

// DroppedTarget is a plan entry the computation nets out, with the
// reason a user would recognize.
type DroppedTarget struct {
	SkillID int64
	Level   int
	Reason  string // "already trained" | "already in queue"
	Intent  int
}

// Outcome is a computed plan: ordered steps plus totals.
// Unknown lists skills with no SDE meta (cannot be timed) that
// expansion met along the way — handlers surface them rather than
// inventing numbers.
type Outcome struct {
	Steps       []Step
	Dropped     []DroppedTarget
	Unknown     []int64
	TotalSP     int64
	TotalSecond float64
	StartsAt    time.Time // when plan training can begin (queue end, or now)
}

// Compute expands targets against the graph and times the
// result. Expansion rules:
//   - a skill's own prerequisites (at their required levels) are
//     pulled in recursively before the skill itself;
//   - levels dedupe to the maximum anyone needs;
//   - trained SP and queued levels net out (queued levels count
//     as trained once the queue drains; partial SP counts);
//   - steps are ordered prerequisites-first, breaking ties by the
//     intent order of the target that pulled each skill in;
//   - training times assume the plan starts when the current
//     queue finishes (StartsAt), which the UI states openly.
func Compute(g Graph, targets []Target, char CharTraining, attrs AttrSet, now time.Time) Outcome {
	startsAt := now
	if char.QueueEnd.After(now) {
		startsAt = char.QueueEnd
	}
	out := Outcome{StartsAt: startsAt}

	// Desired levels with expansion. want maps skill → (level,
	// intent of the earliest target that needs it).
	type wantT struct {
		level  int
		intent int
		direct bool
	}
	want := make(map[int64]*wantT)
	var addWant func(skillID int64, level, intent int, direct bool, stack map[int64]bool)
	addWant = func(skillID int64, level, intent int, direct bool, stack map[int64]bool) {
		if level > 5 {
			level = 5
		}
		if level < 1 {
			return
		}
		w, seen := want[skillID]
		if !seen {
			w = &wantT{level: level, intent: intent, direct: direct}
			want[skillID] = w
		} else {
			if level > w.level {
				w.level = level
			}
			if direct && intent < w.intent {
				w.intent = intent
				w.direct = true
			}
		}
		if stack[skillID] {
			return // requirement cycle in the data: stop descending
		}
		stack[skillID] = true
		for _, req := range g.Requirements(skillID) {
			addWant(req.SkillID, req.Level, intent, false, stack)
		}
		delete(stack, skillID)
	}
	for _, t := range targets {
		addWant(t.SkillID, t.Level, t.Intent, true, make(map[int64]bool))
	}

	// Topo order (Kahn) over the wanted set: a skill's
	// prerequisites inside the set come first; ties break by
	// intent, then type ID (deterministic output).
	indegree := make(map[int64]int, len(want))
	dependents := make(map[int64][]int64)
	for skillID := range want {
		for _, req := range g.Requirements(skillID) {
			if _, inSet := want[req.SkillID]; inSet && req.SkillID != skillID {
				indegree[skillID]++
				dependents[req.SkillID] = append(dependents[req.SkillID], skillID)
			}
		}
	}
	var ready []int64
	for skillID := range want {
		if indegree[skillID] == 0 {
			ready = append(ready, skillID)
		}
	}
	less := func(ids []int64) func(i, j int) bool {
		return func(i, j int) bool {
			a, b := want[ids[i]], want[ids[j]]
			if a.intent != b.intent {
				return a.intent < b.intent
			}
			return ids[i] < ids[j]
		}
	}
	sort.Slice(ready, less(ready))
	ordered := make([]int64, 0, len(want))
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		ordered = append(ordered, id)
		for _, dep := range dependents[id] {
			indegree[dep]--
			if indegree[dep] == 0 {
				ready = append(ready, dep)
			}
		}
		sort.Slice(ready, less(ready))
	}
	// A data-level cycle would strand nodes; append any leftovers
	// in intent order rather than losing them.
	if len(ordered) < len(want) {
		placed := make(map[int64]bool, len(ordered))
		for _, id := range ordered {
			placed[id] = true
		}
		var rest []int64
		for skillID := range want {
			if !placed[skillID] {
				rest = append(rest, skillID)
			}
		}
		sort.Slice(rest, less(rest))
		ordered = append(ordered, rest...)
	}

	cursor := startsAt
	for _, skillID := range ordered {
		w := want[skillID]
		meta, ok := g.Meta(skillID)
		if !ok {
			out.Unknown = append(out.Unknown, skillID)
			continue
		}
		targetSP := SPForLevel(meta.Rank, w.level)

		// Effective trained state: snapshot SP, raised to the
		// queued completion when the queue gets further — the
		// plan trains after the queue, so it starts from there.
		// Partial SP (mid-level progress) counts through the SP.
		basisSP := char.SP[skillID]
		queuedTo := char.QueuedTo[skillID]
		if q := SPForLevel(meta.Rank, queuedTo); q > basisSP {
			basisSP = q
		}
		if basisSP >= targetSP {
			if w.direct {
				reason := "already trained"
				if queuedTo >= w.level && SPForLevel(meta.Rank, queuedTo) > char.SP[skillID] {
					reason = "already in queue"
				}
				out.Dropped = append(out.Dropped, DroppedTarget{
					SkillID: skillID, Level: w.level, Reason: reason, Intent: w.intent,
				})
			}
			continue
		}

		seconds := trainSeconds(meta, attrs, targetSP-basisSP)
		cursor = cursor.Add(time.Duration(seconds * float64(time.Second)))
		out.Steps = append(out.Steps, Step{
			SkillID:     skillID,
			FromLevel:   LevelForSP(meta.Rank, basisSP),
			ToLevel:     w.level,
			SPRemaining: targetSP - basisSP,
			Seconds:     seconds,
			Finish:      cursor,
			Prereq:      !w.direct,
		})
		out.TotalSP += targetSP - basisSP
		out.TotalSecond += seconds
	}
	sort.Slice(out.Dropped, func(i, j int) bool { return out.Dropped[i].Intent < out.Dropped[j].Intent })
	return out
}

// ---------------------------------------------------------------------------
// Remap advisor (display only): the SP-weighted best legal spread
// for the plan's steps. EVE remaps start from 17 in every attribute
// with 14 extra points to place, no attribute below 17 or above 27.
// Only the (primary, secondary) focus matters for a plan, so the
// search tries every ordered attribute pair and every legal split
// of the 14 points between them.
// ---------------------------------------------------------------------------

// RemapAdvice is the advisor's answer: the best spread found, what
// the plan takes under it vs under the current attributes, and
// whether a remap is even available to report honestly.
type RemapAdvice struct {
	Current     AttrSet
	Best        AttrSet
	PrimaryID   int64
	SecondaryID int64
	BestSeconds float64
	CurSeconds  float64
}

// AdviseRemap finds the fastest legal remap for steps. With no
// steps the current spread is returned unmodified.
func AdviseRemap(g Graph, steps []Step, current AttrSet) RemapAdvice {
	advice := RemapAdvice{Current: current, Best: current}
	// Time of the same SP under any candidate spread.
	timeUnder := func(attrs AttrSet) float64 {
		total := 0.0
		for _, s := range steps {
			meta, ok := g.Meta(s.SkillID)
			if !ok {
				continue
			}
			total += trainSeconds(meta, attrs, s.SPRemaining)
		}
		return total
	}
	advice.CurSeconds = timeUnder(current)
	advice.BestSeconds = advice.CurSeconds
	if len(steps) == 0 {
		return advice
	}
	for _, p := range AttributeIDs {
		for _, s := range AttributeIDs {
			if s == p {
				continue
			}
			// x points to primary, the rest to secondary; both
			// must stay within 17..27.
			for x := 4; x <= 10; x++ {
				candidate := AttrSet{Charisma: 17, Intelligence: 17, Memory: 17, Perception: 17, Willpower: 17}
				candidate = candidate.with(p, 17+x).with(s, 17+(14-x))
				if secs := timeUnder(candidate); secs < advice.BestSeconds {
					advice.BestSeconds = secs
					advice.Best = candidate
					advice.PrimaryID = p
					advice.SecondaryID = s
				}
			}
		}
	}
	return advice
}

// ---------------------------------------------------------------------------
// Fit requirements closure.
// ---------------------------------------------------------------------------

// FitSkillClosure resolves what a character must have trained to
// use a set of types (a fitting's ship + modules): every type's
// required skills at their levels, expanded through the skills'
// own prerequisites, each at the maximum level anyone needs.
// The result is in plan-engine order (prerequisites first),
// ready to become plan items.
func FitSkillClosure(g Graph, typeIDs []int64) []Target {
	required := make(map[int64]int)
	var require func(typeID int64, stack map[int64]bool)
	require = func(typeID int64, stack map[int64]bool) {
		if stack[typeID] {
			return
		}
		stack[typeID] = true
		for _, req := range g.Requirements(typeID) {
			if req.Level > required[req.SkillID] {
				required[req.SkillID] = req.Level
			}
			require(req.SkillID, stack)
		}
		delete(stack, typeID)
	}
	for _, id := range typeIDs {
		require(id, make(map[int64]bool))
	}

	// Topo-order like the plan engine: reuse Compute's
	// expansion ordering by feeding empty training state and
	// taking its ordered wants — simpler: run the expansion sort
	// directly over the requirement edges.
	indegree := make(map[int64]int, len(required))
	dependents := make(map[int64][]int64)
	for skillID := range required {
		for _, req := range g.Requirements(skillID) {
			if _, inSet := required[req.SkillID]; inSet && req.SkillID != skillID {
				indegree[skillID]++
				dependents[req.SkillID] = append(dependents[req.SkillID], skillID)
			}
		}
	}
	var ready []int64
	for skillID := range required {
		if indegree[skillID] == 0 {
			ready = append(ready, skillID)
		}
	}
	sort.Slice(ready, func(i, j int) bool { return ready[i] < ready[j] })
	var targets []Target
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		targets = append(targets, Target{SkillID: id, Level: required[id], Intent: len(targets)})
		for _, dep := range dependents[id] {
			indegree[dep]--
			if indegree[dep] == 0 {
				ready = append(ready, dep)
			}
		}
		sort.Slice(ready, func(i, j int) bool { return ready[i] < ready[j] })
	}
	if len(targets) < len(required) {
		placed := make(map[int64]bool, len(targets))
		for _, t := range targets {
			placed[t.SkillID] = true
		}
		var rest []int64
		for skillID := range required {
			if !placed[skillID] {
				rest = append(rest, skillID)
			}
		}
		sort.Slice(rest, func(i, j int) bool { return rest[i] < rest[j] })
		for _, id := range rest {
			targets = append(targets, Target{SkillID: id, Level: required[id], Intent: len(targets)})
		}
	}
	return targets
}
