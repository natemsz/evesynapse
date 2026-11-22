package gamelog

import (
	"slices"
	"time"
)

// FightRead is one stored read of a stretch of a pilot's log. A log
// read again can be cut into fights differently, and a pasted part of
// a log covers only some of a fight, so one pilot can hold several
// reads of the same seconds.
type FightRead struct {
	ID       int64
	Start    time.Time
	End      time.Time
	Received time.Time
}

// StepRange is a run of clock steps counted from one read: From is
// the first step's Unix second and To the second after the last step.
type StepRange struct {
	ReadID int64
	From   int64
	To     int64
}

// CountedSteps says which read counts for each clock step, in time
// order, so no step of a pilot's log is counted twice. A step goes to
// the read covering the most of it, and to the newest of those on a
// tie, so a short newer read never removes time only an older read
// holds.
func CountedSteps(reads []FightRead) []StepRange {
	type pick struct {
		read  FightRead
		cover int64
	}
	best := map[int64]pick{}
	for _, r := range reads {
		start, end := r.Start.Unix(), r.End.Unix()
		for st := ClockStep(start); st <= end; st += SliceSeconds {
			cover := min(end, st+SliceSeconds-1) - max(start, st) + 1
			p, ok := best[st]
			if !ok || cover > p.cover || (cover == p.cover && newerRead(r, p.read)) {
				best[st] = pick{r, cover}
			}
		}
	}
	steps := make([]int64, 0, len(best))
	for st := range best {
		steps = append(steps, st)
	}
	slices.Sort(steps)
	var out []StepRange
	for _, st := range steps {
		id := best[st].read.ID
		if n := len(out); n > 0 && out[n-1].ReadID == id && out[n-1].To == st {
			out[n-1].To = st + SliceSeconds
			continue
		}
		out = append(out, StepRange{ReadID: id, From: st, To: st + SliceSeconds})
	}
	return out
}

func newerRead(a, b FightRead) bool {
	if c := a.Received.Compare(b.Received); c != 0 {
		return c > 0
	}
	return a.ID > b.ID
}
