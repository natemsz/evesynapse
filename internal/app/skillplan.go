package app

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
	attrCharisma     = 164
	attrIntelligence = 165
	attrMemory       = 166
	attrPerception   = 167
	attrWillpower    = 168
)

// attributeName names a dogma attribute ID for display.
func attributeName(id int64) string {
	switch id {
	case attrCharisma:
		return "Charisma"
	case attrIntelligence:
		return "Intelligence"
	case attrMemory:
		return "Memory"
	case attrPerception:
		return "Perception"
	case attrWillpower:
		return "Willpower"
	}
	return "—"
}

// attributeIDs lists the five training attributes in canonical order.
var attributeIDs = []int64{attrCharisma, attrIntelligence, attrMemory, attrPerception, attrWillpower}

// attrSet is a character's five training attribute values.
type attrSet struct {
	Charisma     int
	Intelligence int
	Memory       int
	Perception   int
	Willpower    int
}

// flatAttrSet is the honest fallback while the attributes snapshot
// is still warming: EVE's unmodified spread is 20 across the board.
var flatAttrSet = attrSet{Charisma: 20, Intelligence: 20, Memory: 20, Perception: 20, Willpower: 20}

func (a attrSet) value(id int64) int {
	switch id {
	case attrCharisma:
		return a.Charisma
	case attrIntelligence:
		return a.Intelligence
	case attrMemory:
		return a.Memory
	case attrPerception:
		return a.Perception
	case attrWillpower:
		return a.Willpower
	}
	return 0
}

func (a attrSet) with(id int64, v int) attrSet {
	switch id {
	case attrCharisma:
		a.Charisma = v
	case attrIntelligence:
		a.Intelligence = v
	case attrMemory:
		a.Memory = v
	case attrPerception:
		a.Perception = v
	case attrWillpower:
		a.Willpower = v
	}
	return a
}

// skillMeta is one skill's training profile from sde_skill_meta.
type skillMeta struct {
	Rank      float64
	Primary   int64 // dogma attribute ID
	Secondary int64
}

// skillRequirement is one required-skill row: to use/train the
// type, SkillID must be trained to Level.
type skillRequirement struct {
	SkillID int64
	Level   int
}

// skillGraph supplies the plan engine's static data. The handler
// backs it with the SDE tables (memoized per render); tests serve
// fixtures from maps.
type skillGraph interface {
	Meta(skillID int64) (skillMeta, bool)
	Requirements(typeID int64) []skillRequirement
}

// spForLevel is the cumulative SP at which level completes for a
// skill of the given rank (level 0 = 0 SP).
func spForLevel(rank float64, level int) int64 {
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

// levelForSP is the highest completed level for sp in a skill.
func levelForSP(rank float64, sp int64) int {
	level := 0
	for l := 1; l <= 5; l++ {
		if sp >= spForLevel(rank, l) {
			level = l
		}
	}
	return level
}

// spPerMinute is the training speed of one skill under attrs.
func spPerMinute(meta skillMeta, attrs attrSet) float64 {
	return float64(attrs.value(meta.Primary)) + float64(attrs.value(meta.Secondary))/2
}

// trainSeconds is how long spRemaining takes to train in one skill.
func trainSeconds(meta skillMeta, attrs attrSet, spRemaining int64) float64 {
	rate := spPerMinute(meta, attrs)
	if rate <= 0 || spRemaining <= 0 {
		return 0
	}
	return float64(spRemaining) / rate * 60
}

// ---------------------------------------------------------------------------
// Plan computation.
// ---------------------------------------------------------------------------

// charTraining is the character-side input: current SP per skill,
// the level each skill reaches when the live queue drains, when
// that happens, and the unallocated-SP total (reported, never
// silently spent).
type charTraining struct {
	SP          map[int64]int64
	QueuedTo    map[int64]int // skill → highest level the queue completes
	QueueEnd    time.Time     // zero when the queue is empty or already past
	Unallocated int64
}

// planTarget is one skill the user wants at a level; Intent is the
// plan item's position (lower = asked for earlier). PrereqOf (set
// during expansion) records which direct target pulled a skill in.
type planTarget struct {
	SkillID int64
	Level   int
	Intent  int
}

// planStep is one trainable chunk of a computed plan: from the
// character's effective position after the queue drains to the
// target level.
type planStep struct {
	SkillID     int64
	FromLevel   int
	ToLevel     int
	SPRemaining int64
	Seconds     float64
	Finish      time.Time // cumulative, starting when the queue drains
	Prereq      bool      // pulled in as a prerequisite, not a direct target
}

// droppedTarget is a plan entry the computation nets out, with the
// reason a user would recognize.
type droppedTarget struct {
	SkillID int64
	Level   int
	Reason  string // "already trained" | "already in queue"
	Intent  int
}

// planOutcome is a computed plan: ordered steps plus totals.
// Unknown lists skills with no SDE meta (cannot be timed) that
// expansion met along the way — handlers surface them rather than
// inventing numbers.
type planOutcome struct {
	Steps       []planStep
	Dropped     []droppedTarget
	Unknown     []int64
	TotalSP     int64
	TotalSecond float64
	StartsAt    time.Time // when plan training can begin (queue end, or now)
}

// computePlan expands targets against the graph and times the
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
func computePlan(g skillGraph, targets []planTarget, char charTraining, attrs attrSet, now time.Time) planOutcome {
	startsAt := now
	if char.QueueEnd.After(now) {
		startsAt = char.QueueEnd
	}
	out := planOutcome{StartsAt: startsAt}

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
		targetSP := spForLevel(meta.Rank, w.level)

		// Effective trained state: snapshot SP, raised to the
		// queued completion when the queue gets further — the
		// plan trains after the queue, so it starts from there.
		// Partial SP (mid-level progress) counts through the SP.
		basisSP := char.SP[skillID]
		queuedTo := char.QueuedTo[skillID]
		if q := spForLevel(meta.Rank, queuedTo); q > basisSP {
			basisSP = q
		}
		if basisSP >= targetSP {
			if w.direct {
				reason := "already trained"
				if queuedTo >= w.level && spForLevel(meta.Rank, queuedTo) > char.SP[skillID] {
					reason = "already in queue"
				}
				out.Dropped = append(out.Dropped, droppedTarget{
					SkillID: skillID, Level: w.level, Reason: reason, Intent: w.intent,
				})
			}
			continue
		}

		seconds := trainSeconds(meta, attrs, targetSP-basisSP)
		cursor = cursor.Add(time.Duration(seconds * float64(time.Second)))
		out.Steps = append(out.Steps, planStep{
			SkillID:     skillID,
			FromLevel:   levelForSP(meta.Rank, basisSP),
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

// remapAdvice is the advisor's answer: the best spread found, what
// the plan takes under it vs under the current attributes, and
// whether a remap is even available to report honestly.
type remapAdvice struct {
	Current     attrSet
	Best        attrSet
	PrimaryID   int64
	SecondaryID int64
	BestSeconds float64
	CurSeconds  float64
}

// adviseRemap finds the fastest legal remap for steps. With no
// steps the current spread is returned unmodified.
func adviseRemap(g skillGraph, steps []planStep, current attrSet) remapAdvice {
	advice := remapAdvice{Current: current, Best: current}
	// Time of the same SP under any candidate spread.
	timeUnder := func(attrs attrSet) float64 {
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
	for _, p := range attributeIDs {
		for _, s := range attributeIDs {
			if s == p {
				continue
			}
			// x points to primary, the rest to secondary; both
			// must stay within 17..27.
			for x := 4; x <= 10; x++ {
				candidate := attrSet{Charisma: 17, Intelligence: 17, Memory: 17, Perception: 17, Willpower: 17}
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

// fitSkillClosure resolves what a character must have trained to
// use a set of types (a fitting's ship + modules): every type's
// required skills at their levels, expanded through the skills'
// own prerequisites, each at the maximum level anyone needs.
// The result is in plan-engine order (prerequisites first),
// ready to become plan items.
func fitSkillClosure(g skillGraph, typeIDs []int64) []planTarget {
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

	// Topo-order like the plan engine: reuse computePlan's
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
	var targets []planTarget
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		targets = append(targets, planTarget{SkillID: id, Level: required[id], Intent: len(targets)})
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
			targets = append(targets, planTarget{SkillID: id, Level: required[id], Intent: len(targets)})
		}
	}
	return targets
}
