package app

// ---------------------------------------------------------------------------
// Upkeep & fuel: the lose-something-irreplaceable deadlines in
// one place — structures low on fuel, unanchoring, or running a
// state timer. Station Managers and Directors only: the
// structures endpoint needs the in-game role, so anyone else
// simply gets no section. Rendered reads over the stored corp
// structures snapshots.
// ---------------------------------------------------------------------------

import (
	"context"
	"sort"
	"strconv"
	"time"

	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// upkeepFuelDays flags fuel expiring within the week;
// upkeepCap bounds one corporation's list.
const (
	upkeepFuelDays = 7
	upkeepCap      = 8
)

// structureClock is one structure running a clock: unanchoring, or
// a state timer. The corporation pulse and the upkeep module both
// list these, from one reading (structureClocks).
type structureClock struct {
	Name string
	Text string // "unanchoring" or "state change"
	When string // formatFinish of the end time
	At   time.Time
}

// structureName is a structure's name, or its ID while unnamed.
func structureName(s esi.CorpStructure) string {
	if s.Name != "" {
		return s.Name
	}
	return "Structure #" + strconv.FormatInt(s.StructureID, 10)
}

// structureClocks reads every running clock from a structures
// snapshot, in snapshot order. A structure unanchoring shows that
// clock and not its state timer; a time that does not parse is no
// clock.
func structureClocks(structures esi.CorpStructures) []structureClock {
	var out []structureClock
	for _, s := range structures {
		if at, ok := parseRFC3339(s.UnanchorsAt); ok {
			out = append(out, structureClock{Name: structureName(s), Text: "unanchoring", When: formatFinish(s.UnanchorsAt), At: at})
		} else if at, ok := parseRFC3339(s.StateTimerEnd); ok {
			out = append(out, structureClock{Name: structureName(s), Text: "state change", When: formatFinish(s.StateTimerEnd), At: at})
		}
	}
	return out
}

// upkeepItem is one structure deadline.
type upkeepItem struct {
	Name   string
	Text   string
	When   string
	Urgent bool // already past: fuel out or timer elapsed
	at     time.Time
}

// upkeepCorpView is one corporation's deadlines.
type upkeepCorpView struct {
	Name  string
	Items []upkeepItem
}

// upkeepWidget lists one view per visible corporation; empty
// means no qualifying role anywhere. Sov bills and office
// rentals are deliberately absent: no synced endpoint carries
// them (see the corp structures comment in esi.go).
type upkeepWidget struct {
	Corps []upkeepCorpView
	// Count and Urgent total the deadlines shown, for the
	// module's at-rest summary line.
	Count, Urgent int
}

// upkeepRoles unlocks the widget: the roles behind the corp
// structures endpoint.
var upkeepRoles = []string{"Director", "Station_Manager"}

// buildUpkeep assembles the widget for one user: their
// corporations, gated on Station Manager or Director, soonest
// deadline first with past-due leading.
func (app *Application) buildUpkeep(ctx context.Context, userID int64) *upkeepWidget {
	w := &upkeepWidget{}
	characters, err := app.queries.ListCharactersByUser(ctx, userID)
	if err != nil {
		logging.Errorf("upkeep: list characters for user %d: %v", userID, err)
		return w
	}
	now := time.Now().UTC()
	for _, ref := range app.pulseCorps(ctx, characters) {
		// Read from a character that holds the role itself: the
		// structures snapshot exists only under that character.
		reader := ref.reader(upkeepRoles...)
		if reader == 0 {
			continue
		}
		var structures esi.CorpStructures
		if !app.loadCorpSnapshot(ctx, reader, esi.SnapCorpStructures, &structures) {
			continue
		}
		view := upkeepCorpView{Name: app.pulseCorpName(ctx, ref)}
		for _, s := range structures {
			if at, ok := parseRFC3339(s.FuelExpires); ok && !at.After(now.Add(upkeepFuelDays*24*time.Hour)) {
				view.Items = append(view.Items, upkeepItem{
					Name: structureName(s), Text: "fuel expires", When: formatFinish(s.FuelExpires),
					Urgent: !at.After(now), at: at,
				})
			}
		}
		for _, c := range structureClocks(structures) {
			view.Items = append(view.Items, upkeepItem{
				Name: c.Name, Text: c.Text, When: c.When,
				Urgent: !c.At.After(now), at: c.At,
			})
		}
		sort.SliceStable(view.Items, func(i, j int) bool { return view.Items[i].at.Before(view.Items[j].at) })
		if len(view.Items) > upkeepCap {
			view.Items = view.Items[:upkeepCap]
		}
		if len(view.Items) > 0 {
			w.Corps = append(w.Corps, view)
			w.Count += len(view.Items)
			for _, it := range view.Items {
				if it.Urgent {
					w.Urgent++
				}
			}
		}
	}
	return w
}
