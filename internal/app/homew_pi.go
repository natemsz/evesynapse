package app

import (
	"context"
	"fmt"
	"sort"
	"time"

	"evesynapse/internal/esi"
)

func init() {
	registerHomeWidget(homeWidgetDef{
		ID: widgetPI, Title: "Planetary Industry", Category: wcatIndustry, Feature: "planets",
		Href:        "/planets/",
		Description: "Colonies across your characters: extractor timers, expired heads, and the next planet needing a visit.",
		Default:     wSize{1, 2},
		Forms: []homeWidgetForm{
			{Name: "small", Sizes: "1x1", Says: "Extractors stopped, or all running, and the next to stop."},
			{Name: "wide", Sizes: "2x1 3x1", Says: "When each planet's extractors stop, over the next three days."},
			{Name: "tall", Sizes: "1x2 1x3 1x4", Says: "Planets by when their extractors stop, with how far each run has gone."},
			{Name: "medium", Sizes: "2x2 2x3", Says: "The three-day track over the planets."},
			{Name: "large", Sizes: "3x2 3x3", Says: "The planets grouped by character, with the track."},
		},
		Needs: []string{esi.SnapPlanets, needPlanetLayouts},
		Options: []widgetOption{
			optScope(),
			withHint(optToggle("visit", "Only planets that need a visit", false),
				"Planets whose extractors have stopped or stop within a day."),
		},
		Build: buildPIWidget,
	})
}

// piSoon is how close a stop has to be for its planet to need a visit.
const piSoon = 24 * time.Hour

// piTrackSpan is the track's width: extractors stopping later sit at
// its right edge.
const piTrackSpan = 72 * time.Hour

// piRowCap is how many planets the list carries; the card shows the
// ones that fit and links to the Planets page for the rest.
const piRowCap = 30

// piExpiryLine is one planet's extractors that stop together. Search
// is the text the list's filter matches.
type piExpiryLine struct {
	Char       string
	CharID     int64
	Planet     string
	ExpiresRaw string // RFC3339, for the countdown
	Expires    string // the stop, written out
	Left       string // time left; "" once stopped
	Expired    bool
	Soon       bool // stops within a day
	Search     string
	Count      int     // extractors on the planet that stop together
	HasCycle   bool    // the layout carries the program's start
	CyclePct   float64 // share of the program already run, 0 to 100
	at         time.Time
}

// Tone is the row's status: bad once stopped, warn within a day.
func (r piExpiryLine) Tone() string {
	switch {
	case r.Expired:
		return "bad"
	case r.Soon:
		return "warn"
	}
	return ""
}

// piMark is one planet on the track: where it sits (percent of the
// span, stopped ones at the left edge) and on which lane, so marks
// close together do not cover each other.
type piMark struct {
	Left, Top float64
	Tone      string
	Label     string
}

// piTrack is the three-day line the wide, medium and large forms draw.
type piTrack struct {
	Marks []piMark
	Ticks []wTick
	Table wTable
	Tones []piTone // the legend: only the states the track shows
}

type piTone struct{ Tone, Name string }

// piGroup is one character's planets in the large form.
type piGroup struct {
	Char   string
	CharID int64
	Rows   []piExpiryLine
}

// piView is what the Planetary Industry forms draw.
type piView struct {
	Any      bool // any colonies snapshot at all
	Colonies int
	Expired  int
	Soon     int
	Lead     wLead
	Next     *piExpiryLine // the next extractor to stop
	Soonest  []piExpiryLine
	Groups   []piGroup
	Track    *piTrack
	Filtered bool // the visit option is on
	Empty    wEmpty
}

func buildPIWidget(ctx context.Context, app *Application, in *widgetInput) any {
	now := in.Now
	v := &piView{Filtered: in.Opts.Get("visit") == toggleOn}
	var rows []piExpiryLine
	grouped := map[string]int{}
	for _, b := range in.Bundles {
		if !b.planetsKnown {
			continue
		}
		v.Any = true
		v.Colonies += len(b.colonies)
		for _, colony := range b.colonies {
			layout := b.layouts[colony.PlanetID]
			if layout == nil {
				continue
			}
			planet := app.planetDisplayName(ctx, colony.PlanetID)
			for _, ex := range layoutExtractors(*layout) {
				if !ex.ExpiryOK {
					continue
				}
				// Extractors on one planet installed together stop
				// together: one row with a count rather than one each.
				key := fmt.Sprintf("%d|%d|%d", b.ch.CharacterID, colony.PlanetID, ex.Expiry.Unix()/60)
				if i, seen := grouped[key]; seen {
					rows[i].Count++
					continue
				}
				row := piExpiryLine{
					Char: b.ch.Name, CharID: b.ch.CharacterID, Planet: planet, Count: 1,
					ExpiresRaw: ex.Expiry.UTC().Format(time.RFC3339),
					Expires:    formatFinish(ex.Expiry.UTC().Format(time.RFC3339)),
					at:         ex.Expiry,
				}
				if ex.Expiry.Before(now) {
					row.Expired = true
					row.Search = b.ch.Name + " " + planet + " expired"
				} else {
					row.Left = humanDuration(ex.Expiry.Sub(now))
					row.Soon = ex.Expiry.Sub(now) <= piSoon
					row.Search = b.ch.Name + " " + planet
				}
				if !ex.Installed.IsZero() && ex.Expiry.After(ex.Installed) {
					pct := float64(now.Sub(ex.Installed)) * 100 / float64(ex.Expiry.Sub(ex.Installed))
					row.HasCycle, row.CyclePct = true, min(max(pct, 0), 100)
				}
				grouped[key] = len(rows)
				rows = append(rows, row)
			}
		}
	}
	if !v.Any {
		v.Empty = wEmpty{Text: "Colonies appear here once your characters have synced."}
		return v
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].at.Before(rows[j].at) })
	for _, r := range rows {
		switch {
		case r.Expired:
			v.Expired += r.Count
		case r.Soon:
			v.Soon += r.Count
		}
	}
	if v.Filtered {
		kept := rows[:0]
		for _, r := range rows {
			if r.Expired || r.Soon {
				kept = append(kept, r)
			}
		}
		rows = kept
	}
	// The next stop is picked before the list is cut: a list of only
	// stopped extractors can still have others running behind it.
	for i := range rows {
		if !rows[i].Expired {
			next := rows[i]
			v.Next = &next
			break
		}
	}
	if len(rows) > piRowCap {
		rows = rows[:piRowCap]
	}
	v.Soonest = rows

	switch {
	case v.Expired > 0:
		v.Lead = wLead{Label: "Extractors stopped", Value: esi.FormatInt(int64(v.Expired)),
			Full: fmt.Sprintf("%d extractors have stopped", v.Expired), Href: "/planets/"}
	case v.Soon > 0:
		v.Lead = wLead{Label: "Stopping within a day", Value: esi.FormatInt(int64(v.Soon)),
			Full: fmt.Sprintf("%d extractors stop within a day", v.Soon)}
	default:
		v.Lead = wLead{Label: "Extractors", Value: "All running"}
	}
	v.Lead.Sub = fmt.Sprintf("%d colonies", v.Colonies)
	if v.Colonies == 1 {
		v.Lead.Sub = "1 colony"
	}
	switch {
	case len(rows) == 0 && v.Filtered:
		v.Empty = wEmpty{Text: "No planet needs a visit within a day."}
	case len(rows) == 0:
		v.Empty = wEmpty{Text: "No extractor timers yet. They show once each colony's layout has synced."}
	}

	if in.Draws("wide") || in.Draws("medium") || in.Draws("large") {
		v.Track = piTrackOf(rows, now)
	}
	if in.Draws("large") {
		at := map[int64]int{}
		for _, r := range rows {
			i, ok := at[r.CharID]
			if !ok {
				i = len(v.Groups)
				at[r.CharID] = i
				v.Groups = append(v.Groups, piGroup{Char: r.Char, CharID: r.CharID})
			}
			v.Groups[i].Rows = append(v.Groups[i].Rows, r)
		}
	}
	return v
}

// piTrackLanes is how many lanes the track stacks close marks on.
const piTrackLanes = 3

// piTrackOf places each planet on the three-day track. A mark within
// a few percent of the last one on a lane moves to the next lane.
func piTrackOf(rows []piExpiryLine, now time.Time) *piTrack {
	if len(rows) == 0 {
		return nil
	}
	t := &piTrack{
		Ticks: []wTick{{Label: "Now", Pos: 0, Anchor: "start"}, {Label: "1 day", Pos: 100.0 / 3, Anchor: "middle"},
			{Label: "2 days", Pos: 200.0 / 3, Anchor: "middle"}, {Label: "3 days", Pos: 100, Anchor: "end"}},
		Table: wTable{Caption: "When each planet's extractors stop", Head: []string{"Planet", "Character", "Stops"}},
	}
	var last [piTrackLanes]float64
	for i := range last {
		last[i] = -100
	}
	seen := map[string]bool{}
	for _, r := range rows {
		left := 0.0
		if !r.Expired {
			left = min(float64(r.at.Sub(now))*100/float64(piTrackSpan), 100)
		}
		lane := 0
		for lane < piTrackLanes-1 && left-last[lane] < 4 {
			lane++
		}
		last[lane] = left
		when := "stopped " + r.Expires
		if !r.Expired {
			when = "in " + r.Left
		}
		t.Marks = append(t.Marks, piMark{Left: left, Top: 20 + float64(lane)*30, Tone: r.Tone(),
			Label: r.Planet + ", " + r.Char + ": " + when})
		t.Table.Rows = append(t.Table.Rows, []string{r.Planet, r.Char, when})
		seen[r.Tone()] = true
	}
	for _, k := range []piTone{{"bad", "Stopped"}, {"warn", "Within a day"}, {"", "Later"}} {
		if seen[k.Tone] {
			t.Tones = append(t.Tones, k)
		}
	}
	return t
}
