package app

import (
	"sort"
	"time"

	"evesynapse/internal/esi"
)

// ---------------------------------------------------------------------------
// The live skill queue: how every view reads the stored skillqueue.
//
// ESI keeps a finished entry in the queue until the character next
// logs in, so a stored snapshot can lead with skills that are already
// trained. The worker owns the snapshot and stores it as fetched; the
// views own its interpretation, and they all read it through
// liveSkillqueue: an entry whose finish date is at or before now is
// trained, not queued. An entry with no finish date, or one that does
// not parse, stays (unknown is not finished; a paused queue has none).
//
// Event producers that are about finished entries (skill-finished
// notifications, Foresight's queue-ran-dry trigger) read the raw
// queue on purpose.
// ---------------------------------------------------------------------------

// liveSkillqueue returns the entries still to train at now, in queue
// order. The stored queue is not modified.
func liveSkillqueue(queue esi.Skillqueue, now time.Time) esi.Skillqueue {
	live := make(esi.Skillqueue, 0, len(queue))
	for _, e := range queue {
		if finish, err := time.Parse(time.RFC3339, e.FinishDate); err == nil && !finish.After(now) {
			continue
		}
		live = append(live, e)
	}
	sort.SliceStable(live, func(i, j int) bool { return live[i].QueuePosition < live[j].QueuePosition })
	return live
}

// queueHead is what is training at now: the first live entry, or nil
// when nothing in the queue is still to train.
func queueHead(queue esi.Skillqueue, now time.Time) *esi.SkillqueueEntry {
	live := liveSkillqueue(queue, now)
	if len(live) == 0 {
		return nil
	}
	return &live[0]
}

// pendingLevels is the levels still to train per skill, read off the
// live queue. The skills snapshot can lag the queue (a level that just
// finished is not in it yet), so the trained level is also read off
// the queue: every lower level of a skill that is no longer pending
// has finished.
type pendingLevels map[int64]map[int]bool

// add marks one level of a skill as still to train.
func (p pendingLevels) add(skillID int64, level int) {
	if p[skillID] == nil {
		p[skillID] = map[int]bool{}
	}
	p[skillID][level] = true
}

// trainedBelow returns the highest level below level that is no
// longer pending: the level the skill is trained to by the time the
// queue reaches level.
func (p pendingLevels) trainedBelow(skillID int64, level int) int {
	t := level - 1
	for t > 0 && p[skillID][t] {
		t--
	}
	return t
}

// trained returns the level below the lowest one still pending, and
// false when the skill has nothing pending.
func (p pendingLevels) trained(skillID int64) (int, bool) {
	levels := p[skillID]
	if len(levels) == 0 {
		return 0, false
	}
	lowest := 6
	for l := range levels {
		lowest = min(lowest, l)
	}
	return lowest - 1, true
}
