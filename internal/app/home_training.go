package app

// ---------------------------------------------------------------------------
// Training horizon: the next finishes across every character's
// whole skill queue, plus who is sitting idle. The Skills widget
// shows queue heads only (the single next finish per character);
// this looks deeper — second and third queued skills included —
// so one glance replaces opening every character's queue.
// ---------------------------------------------------------------------------

import (
	"context"
	"fmt"
	"sort"
	"time"

	"evesynapse/internal/esi"
)

// trainingEvent is one upcoming finish anywhere in a queue.
// Finish/Left render exactly like the Skills widget's rows.
type trainingEvent struct {
	Char    string
	CharID  int64
	Skill   string
	SkillID int64
	Level   string
	Finish  string
	Left    string
	finish  time.Time
}

// trainingWidget is the horizon: the soonest finishes across
// the fleet and the names training nothing. IdleMore counts
// the idle names cut by idleNamesCap.
type trainingWidget struct {
	Events   []trainingEvent
	Idle     []string
	IdleMore int
}

// trainingHorizonCap bounds one render; the queue snapshots
// are small but the fleet may not be.
const trainingHorizonCap = 10

// idleNamesCap bounds the idle list; the count beside it stays
// exact when names are cut.
const idleNamesCap = 6

// buildTraining assembles the horizon from every queue entry
// still to finish, soonest first. Characters with no active
// queue land in Idle by name.
func (app *Application) buildTraining(ctx context.Context, bundles []*charSnaps) *trainingWidget {
	now := time.Now()
	w := &trainingWidget{}
	var pending []trainingEvent
	for _, b := range bundles {
		if !b.queueKnown {
			continue
		}
		training, _ := queueState(b.queue, now)
		if !training {
			w.Idle = append(w.Idle, b.ch.Name)
			continue
		}
		for _, entry := range b.queue {
			// A stored queue still lists skills that have finished
			// since it was fetched: they are behind us, not ahead.
			finish, ok := parseRFC3339(entry.FinishDate)
			if !ok || !finish.After(now) {
				continue
			}
			name := app.esi.CachedTypeName(ctx, entry.SkillID)
			if name == "" {
				name = fmt.Sprintf("Type #%d", entry.SkillID)
			}
			pending = append(pending, trainingEvent{
				Char:    b.ch.Name,
				CharID:  b.ch.CharacterID,
				Skill:   name,
				SkillID: entry.SkillID,
				Level:   esi.RomanLevel(entry.FinishedLevel),
				Finish:  formatFinish(entry.FinishDate),
				Left:    humanDuration(finish.Sub(now)),
				finish:  finish,
			})
		}
	}
	sort.SliceStable(pending, func(i, j int) bool { return pending[i].finish.Before(pending[j].finish) })
	if len(pending) > trainingHorizonCap {
		pending = pending[:trainingHorizonCap]
	}
	w.Events = pending
	sort.Strings(w.Idle)
	if len(w.Idle) > idleNamesCap {
		w.IdleMore = len(w.Idle) - idleNamesCap
		w.Idle = w.Idle[:idleNamesCap]
	}
	return w
}
