package app

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// How long the worker's cycle takes, and how far behind it is. Kept so
// that its limits can be seen coming on the Sync page instead of being
// met: a cycle that takes longer and longer, a fetch allowance that is
// used up every time, characters put off to the next cycle, data that
// is further and further past its cache window when its turn comes.
//
// Nothing here changes what the worker does. It only measures.
// ---------------------------------------------------------------------------

// workerRecentCycles is how many finished cycles are kept for the
// "lately" line: about an hour of them at one a minute.
const workerRecentCycles = 60

// phaseTiming is how long one part of a cycle took.
type phaseTiming struct {
	Name string
	Took time.Duration
}

// workerTiming is the measurements of one finished cycle.
type workerTiming struct {
	At         time.Time
	Took       time.Duration
	Characters int // characters eligible for syncing
	// How many of them are in each tier (worker_tiers.go).
	Active, Watched, Recent, Dormant, Asleep int
	Fetches                                  int // of the character pass's allowance
	Budget                                   int // that allowance
	Deferred                                 int // characters put off to the next cycle
	// Overdue is how far past its cache window the stalest data of
	// an active account was when the cycle began: what somebody with
	// the site open was looking at. NeverFetched: some active
	// character had data that had never been fetched at all.
	Overdue      time.Duration
	NeverFetched bool
	// RateHeld is how many characters were left alone because ESI
	// rate-limited them, and Budgets the rate-limit budgets ESI
	// reported during the cycle, one per group of routes, the tightest
	// first (empty when it reported none).
	RateHeld int
	Budgets  []esi.RateHeadroom
	Phases   []phaseTiming
}

// phaseClock times the parts of a cycle, one after another.
type phaseClock struct {
	start, last time.Time
	phases      []phaseTiming
}

func newPhaseClock(now time.Time) *phaseClock { return &phaseClock{start: now, last: now} }

// mark closes the part that has just finished.
func (c *phaseClock) mark(name string, now time.Time) {
	c.phases = append(c.phases, phaseTiming{Name: name, Took: now.Sub(c.last)})
	c.last = now
}

// used reports how much of a fetch allowance of size was spent.
func (b *fetchBudget) used(size int) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return size - b.left
}

// recordWorkerTiming stores a finished cycle's measurements and adds
// it to the recent ones. A cycle that outran the worker's one-minute
// tick is said in the log, since the next one starts late because of
// it.
func (app *Application) recordWorkerTiming(t workerTiming) {
	app.updateWorkerStatus(func(s *workerStatus) {
		s.Timing = t
		s.Recent = append(s.Recent, t)
		if len(s.Recent) > workerRecentCycles {
			s.Recent = append([]workerTiming(nil), s.Recent[len(s.Recent)-workerRecentCycles:]...)
		}
	})
	if t.Took > time.Minute {
		logWorkerOverrun(t)
	}
}

// shortDuration writes a duration the way the Sync page shows it:
// "0.4s", "12s", "3m 20s", "2h 5m".
func shortDuration(d time.Duration) string {
	switch {
	case d < 0:
		d = 0
		fallthrough
	case d < 10*time.Second:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
}

// workerTimingView is the Sync page's account of the worker's pace.
// statRow is one figure on the Sync page: what it is, and its value.
type statRow struct{ Label, Value string }

// budgetRow is one of ESI's rate-limit groups as last seen.
type budgetRow struct {
	Group string
	Left  int
	Limit string // "150/15m"; "" when ESI did not say
}

type workerTimingView struct {
	Last    string   // the last cycle, in a line
	Phases  []string // where its time went, longest first
	Budgets []string // ESI's rate-limit budgets seen, the tightest first
	Lately  string   // the recent cycles, in a line; "" with fewer than two

	// The same figures a row each, which is how the Sync page lays
	// them out; the lines above are what the log and the tests read.
	LastRows   []statRow
	PhaseRows  []statRow
	BudgetRows []budgetRow
	HourTitle  string // "Last 60 cycles"; "" with fewer than two
	HourRows   []statRow
	// Behind: the worker is not keeping up, and why, in words.
	Behind string
}

// workerTimingViewFor turns the stored measurements into the page's
// lines. Empty before the first cycle has finished.
func workerTimingViewFor(s workerStatus) *workerTimingView {
	t := s.Timing
	if t.At.IsZero() {
		return nil
	}
	v := &workerTimingView{}
	tiers := fmt.Sprintf("%d active, %d watched, %d recent, %d dormant", t.Active, t.Watched, t.Recent, t.Dormant)
	if t.Asleep > 0 {
		tiers += fmt.Sprintf(", %d asleep", t.Asleep)
	}
	parts := []string{
		"took " + shortDuration(t.Took),
		plural(t.Characters, "character") + " (" + tiers + ")",
		fmt.Sprintf("%d of %d fetches", t.Fetches, t.Budget),
	}
	if t.Deferred > 0 {
		parts = append(parts, fmt.Sprintf("%d put off to the next cycle", t.Deferred))
	}
	if t.RateHeld > 0 {
		parts = append(parts, plural(t.RateHeld, "character")+" left alone at ESI's request (rate limit)")
	}
	switch {
	case t.NeverFetched:
		parts = append(parts, "some data not fetched yet")
	case t.Overdue > 0:
		parts = append(parts, "stalest active data "+shortDuration(t.Overdue)+" past its refresh time")
	}
	v.Last = strings.Join(parts, " · ")
	v.LastRows = []statRow{
		{"Took", shortDuration(t.Took)},
		{"Characters", strconv.Itoa(t.Characters) + ": " + tiers},
		{"Fetches", fmt.Sprintf("%d of %d allowed", t.Fetches, t.Budget)},
	}
	if t.Deferred > 0 {
		v.LastRows = append(v.LastRows, statRow{"Put off to the next cycle", plural(t.Deferred, "character")})
	}
	if t.RateHeld > 0 {
		v.LastRows = append(v.LastRows, statRow{"Left alone at ESI's request", plural(t.RateHeld, "character") + " (rate limit)"})
	}
	switch {
	case t.NeverFetched:
		v.LastRows = append(v.LastRows, statRow{"Stalest active data", "some not fetched yet"})
	case t.Overdue > 0:
		v.LastRows = append(v.LastRows, statRow{"Stalest active data", shortDuration(t.Overdue) + " past its refresh time"})
	}

	// ESI's budgets, the tightest first: which groups of routes are
	// nearest their limit, which is where to ease off next.
	for i, b := range t.Budgets {
		if i == 8 {
			break
		}
		line := fmt.Sprintf("%s %d left", b.Group, b.Remaining)
		if b.Limit != "" {
			line += " of " + b.Limit
		}
		v.Budgets = append(v.Budgets, line)
		v.BudgetRows = append(v.BudgetRows, budgetRow{Group: b.Group, Left: b.Remaining, Limit: b.Limit})
	}

	phases := append([]phaseTiming(nil), t.Phases...)
	for i := 1; i < len(phases); i++ {
		for j := i; j > 0 && phases[j].Took > phases[j-1].Took; j-- {
			phases[j], phases[j-1] = phases[j-1], phases[j]
		}
	}
	for _, p := range phases {
		if p.Took >= 50*time.Millisecond {
			v.Phases = append(v.Phases, p.Name+" "+shortDuration(p.Took))
			v.PhaseRows = append(v.PhaseRows, statRow{p.Name, shortDuration(p.Took)})
		}
	}

	if n := len(s.Recent); n >= 2 {
		var total, longest, worst time.Duration
		spent, deferred := 0, 0
		fetches, most := 0, 0
		var active, watched, recent, dormant, asleep int
		for _, r := range s.Recent {
			total += r.Took
			fetches += r.Fetches
			if r.Fetches > most {
				most = r.Fetches
			}
			active, watched, recent, dormant, asleep = active+r.Active, watched+r.Watched, recent+r.Recent, dormant+r.Dormant, asleep+r.Asleep
			if r.Took > longest {
				longest = r.Took
			}
			if r.Overdue > worst {
				worst = r.Overdue
			}
			if r.Fetches >= r.Budget && r.Budget > 0 {
				spent++
			}
			if r.Deferred > 0 {
				deferred++
			}
		}
		lately := []string{
			fmt.Sprintf("last %d cycles: average %s, longest %s", n, shortDuration(total/time.Duration(n)), shortDuration(longest)),
		}
		// What the characters cost: the fetches a cycle makes, beside how
		// many characters were in each tier while it made them. One cycle
		// says little (a tier's characters come due in bursts); an hour's
		// average is what to size the allowance by.
		mean := func(sum int) string { return strconv.FormatFloat(float64(sum)/float64(n), 'f', 1, 64) }
		lately = append(lately, fmt.Sprintf("fetches: average %s a cycle, most %d", mean(fetches), most))
		mix := fmt.Sprintf("characters on average: %s active, %s watched, %s recent, %s dormant", mean(active), mean(watched), mean(recent), mean(dormant))
		if asleep > 0 {
			mix += ", " + mean(asleep) + " asleep"
		}
		lately = append(lately, mix)
		if spent > 0 {
			lately = append(lately, fmt.Sprintf("allowance used up in %d", spent))
		}
		if deferred > 0 {
			lately = append(lately, fmt.Sprintf("characters put off in %d", deferred))
		}
		if worst > 0 {
			lately = append(lately, "stalest data up to "+shortDuration(worst)+" late")
		}
		v.Lately = strings.Join(lately, " · ")

		v.HourTitle = fmt.Sprintf("Last %d cycles", n)
		mixRow := fmt.Sprintf("%s active, %s watched, %s recent, %s dormant", mean(active), mean(watched), mean(recent), mean(dormant))
		if asleep > 0 {
			mixRow += ", " + mean(asleep) + " asleep"
		}
		v.HourRows = []statRow{
			{"Cycle time", "average " + shortDuration(total/time.Duration(n)) + ", longest " + shortDuration(longest)},
			{"Fetches a cycle", fmt.Sprintf("average %s, most %d", mean(fetches), most)},
			{"Characters on average", mixRow},
		}
		if spent > 0 {
			v.HourRows = append(v.HourRows, statRow{"Allowance used up", "in " + plural(spent, "cycle")})
		}
		if deferred > 0 {
			v.HourRows = append(v.HourRows, statRow{"Characters put off", "in " + plural(deferred, "cycle")})
		}
		if worst > 0 {
			v.HourRows = append(v.HourRows, statRow{"Stalest active data", "up to " + shortDuration(worst) + " late"})
		}

		// Not keeping up: most recent cycles ran out of allowance or
		// put characters off. One such cycle is a busy minute; most
		// of an hour of them is the limit.
		switch {
		case deferred*2 > n:
			v.Behind = fmt.Sprintf("The worker is not keeping up: it put characters off in %d of the last %d cycles. Data is refreshed later than its cache window allows.", deferred, n)
		case longest > time.Minute:
			v.Behind = "A cycle took longer than the minute between cycles, so the next one started late."
		}
	}
	return v
}

// logWorkerOverrun says in the log that a cycle outran the tick.
func logWorkerOverrun(t workerTiming) {
	var parts []string
	for _, p := range t.Phases {
		if p.Took >= time.Second {
			parts = append(parts, p.Name+" "+shortDuration(p.Took))
		}
	}
	logging.Warnf("worker: the cycle took %s, longer than the minute between cycles (%s)", shortDuration(t.Took), strings.Join(parts, ", "))
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}
