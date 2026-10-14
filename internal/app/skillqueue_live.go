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
