package app

// ---------------------------------------------------------------------------
// Training horizon: the next finishes across every character's
// whole skill queue, plus who is sitting idle, second and third
// queued skills included, so one glance replaces opening every
// character's queue. homew_training.go draws it at each size.
// ---------------------------------------------------------------------------

import (
	"context"
	"fmt"
	"sort"
	"time"

	"evesynapse/internal/esi"
)

// trainingEvent is one upcoming finish anywhere in a queue.
type trainingEvent struct {
	Char    string
	CharID  int64
	Skill   string
	SkillID int64
	Level   string
	// The character page's level pips (skillLevel): levels already
	// trained, the level this row finishes, and "training" for the
	// queue head or "queued" behind it.
	TrainedLvl int
	LevelNum   int
	State      string
	Finish     string
	FinishAt   string // RFC3339, for the live countdown
	Left       string
	finish     time.Time
}

// trainingWidget is the horizon: the soonest finishes across
// the fleet and the names training nothing. IdleMore counts
// the idle names cut by idleNamesCap; IdleCount is how many
// characters are idle.
type trainingWidget struct {
	Events    []trainingEvent
	Idle      []string
	IdleMore  int
	IdleCount int
}

// trainingHorizonCap bounds one render; the queue snapshots
// are small but the fleet may not be. It is as many rows as the
// tallest form holds.
const trainingHorizonCap = 20

// idleNamesCap bounds the idle list; the count beside it stays
// exact when names are cut.
const idleNamesCap = 6

// buildTraining assembles the horizon from every queue entry
// still to finish, soonest first. Characters with no active
// queue land in Idle by name.
func (app *Application) buildTraining(ctx context.Context, bundles []*charSnaps) *trainingWidget {
	return app.buildTrainingWithin(ctx, bundles, time.Now(), 0)
}

// buildTrainingWithin is the horizon over the finishes due within the
// given span of now (0 for everything queued).
func (app *Application) buildTrainingWithin(ctx context.Context, bundles []*charSnaps, now time.Time, within time.Duration) *trainingWidget {
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
		live := liveSkillqueue(b.queue, now)
		// Levels still in the queue, per skill: the level below the
		// lowest one queued is trained (the character page's rule).
		queuedLevels := map[int64]map[int]bool{}
		for _, e := range live {
			if queuedLevels[e.SkillID] == nil {
				queuedLevels[e.SkillID] = map[int]bool{}
			}
			queuedLevels[e.SkillID][e.FinishedLevel] = true
		}
		head := queueHead(b.queue, now)
		levelOf := func(e esi.SkillqueueEntry) (trained int, state string) {
			trained = e.FinishedLevel - 1
			for trained > 0 && queuedLevels[e.SkillID][trained] {
				trained--
			}
			state = "queued"
			if head != nil && e.SkillID == head.SkillID && e.FinishedLevel == head.FinishedLevel {
				state = "training"
			}
			return trained, state
		}
		for _, entry := range live {
			// The horizon is dated: an entry without a finish date
			// (a paused queue) has no place on it.
			finish, ok := parseRFC3339(entry.FinishDate)
			if !ok || (within > 0 && finish.After(now.Add(within))) {
				continue
			}
			ev := app.trainingEventFor(ctx, b, entry, finish, now)
			ev.TrainedLvl, ev.State = levelOf(entry)
			pending = append(pending, ev)
		}
	}
	sort.SliceStable(pending, func(i, j int) bool { return pending[i].finish.Before(pending[j].finish) })
	w.IdleCount = len(w.Idle)
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

// trainingEventFor is one queue entry as a horizon row.
func (app *Application) trainingEventFor(ctx context.Context, b *charSnaps, entry esi.SkillqueueEntry, finish, now time.Time) trainingEvent {
	name := app.esi.CachedTypeName(ctx, entry.SkillID)
	if name == "" {
		name = fmt.Sprintf("Type #%d", entry.SkillID)
	}
	return trainingEvent{
		Char:     b.ch.Name,
		CharID:   b.ch.CharacterID,
		Skill:    name,
		SkillID:  entry.SkillID,
		Level:    esi.RomanLevel(entry.FinishedLevel),
		LevelNum: entry.FinishedLevel,
		Finish:   formatFinish(entry.FinishDate),
		FinishAt: finish.UTC().Format(time.RFC3339),
		Left:     humanDuration(finish.Sub(now)),
		finish:   finish,
	}
}
