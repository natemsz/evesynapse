package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
	"github.com/jackc/pgx/v5"
)

func init() {
	registerHomeWidget(homeWidgetDef{
		ID: widgetDoctrines, Title: "Doctrine Readiness", Category: wcatFleets, Feature: "skills", Requires: "doctrines",
		Href:        "/doctrines/",
		Description: "Which of your pilots can fly your doctrines, and the skills the nearest pilot still needs.",
		Default:     wSize{1, 2},
		Forms: []homeWidgetForm{
			{Name: "small", Sizes: "1x1", Says: "How many of your doctrines a pilot can fly, and the nearest gap."},
			{Name: "tall", Sizes: "1x2 1x3 1x4", Says: "Each doctrine with how many pilots are ready and what the nearest one needs."},
			{Name: "medium", Sizes: "2x2 2x3", Says: "The doctrines beside the pilots closest to flying one."},
			{Name: "large", Sizes: "3x2 3x3", Says: "Each doctrine's fits with who can fly them."},
		},
		Build: buildDoctrinesWidget,
	})
}

// doctrinesWidgetRows is the most doctrines the widget reads.
const doctrinesWidgetRows = 30

// The large form lists fits under the first doctrinesFitDoctrines
// doctrines, at most doctrinesFitsEach each: more than the card can
// show, and within one bounded read.
const (
	doctrinesFitDoctrines = 16
	doctrinesFitsEach     = 6
)

// readyGapView is what the nearest pilot still needs.
type readyGapView struct {
	Pilot    string
	PilotID  int64
	Fit      string // the fit's name and hull: "Muninn (Shield)"
	Missing  string // up to three skills, the longest first
	More     int    // missing skills beyond those named
	SP       string // "450K"
	Doctrine string
	Href     string // the doctrine's page
	sp       int64
}

// Text is the gap in one line: "Heavy Assault Cruisers IV and 2 more,
// 450K skill points".
func (g readyGapView) Text() string {
	t := g.Missing
	if g.More > 0 {
		t += fmt.Sprintf(" and %d more", g.More)
	}
	return t + ", " + g.SP + " skill points"
}

// readyFitView is one fit and who can fly it.
type readyFitView struct {
	Name, Role string
	ShipID     int64
	Ready      []readyPilotRef
	Near       *readyGapView
}

type readyPilotRef struct {
	ID   int64
	Name string
}

// readyDoctrineView is one doctrine's readiness.
type readyDoctrineView struct {
	ID          int64
	Name        string
	Href        string
	Category    string
	Fits        int32
	FitsFlyable int32
	Pilots      int32
	Ready       int32
	Pct         float64
	Near        *readyGapView
	FitRows     []readyFitView
	FitsMore    string // "3 more fits" when the large form lists only the first
}

// ReadyText is "2 of 3 pilots ready".
func (d readyDoctrineView) ReadyText() string {
	return fmt.Sprintf("%d of %s ready", d.Ready, pluralCount(int64(d.Pilots), "pilot", "pilots"))
}

// Tone is the meter's colour: bad when no pilot can fly it.
func (d readyDoctrineView) Tone() string {
	if d.Ready == 0 {
		return "bad"
	}
	return ""
}

// doctrinesWidgetView is what the Doctrine Readiness forms draw.
type doctrinesWidgetView struct {
	Lead      wLead
	Doctrines []readyDoctrineView
	Nearest   []readyGapView // medium: the closest gaps across doctrines
	Pending   bool           // the worker has not folded the account yet
	Empty     wEmpty
}

func buildDoctrinesWidget(ctx context.Context, app *Application, in *widgetInput) any {
	v := &doctrinesWidgetView{Empty: wEmpty{Text: "None of your corporations, alliances or communities keeps a doctrine yet.",
		Step: "Open Doctrines", Href: "/doctrines/"}}
	if _, err := app.queries.GetDoctrineReadinessFold(ctx, in.UserID); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			logging.Errorf("home: doctrine readiness of user %d: %v", in.UserID, err)
		}
		v.Pending = true
		v.Empty = wEmpty{Text: "Checking which of your pilots can fly your doctrines. This shows after the next update.",
			Step: "Open Doctrines", Href: "/doctrines/"}
		return v
	}
	rows, err := app.queries.ListDoctrineReadinessByUser(ctx, db.ListDoctrineReadinessByUserParams{UserID: in.UserID, RowLimit: doctrinesWidgetRows})
	if err != nil {
		logging.Errorf("home: doctrine readiness of user %d: %v", in.UserID, err)
		return v
	}
	names := make(map[int64]string, len(in.Chars))
	for _, c := range in.Chars {
		names[c.CharacterID] = c.Name
	}
	gapOf := func(charID int64, fit, missing string, n int32, sp int64) *readyGapView {
		if charID == 0 {
			return nil
		}
		g := &readyGapView{PilotID: charID, Pilot: names[charID], Fit: fit, Missing: missing, SP: compactSP(sp, 1), sp: sp}
		if g.Pilot == "" {
			g.Pilot = "One of your pilots"
		}
		if shown := len(strings.Split(missing, ", ")); int(n) > shown {
			g.More = int(n) - shown
		}
		return g
	}
	ids := make([]int64, 0, len(rows))
	flyable := 0
	for _, r := range rows {
		d := readyDoctrineView{ID: r.DoctrineID, Name: r.Name, Href: fmt.Sprintf("/doctrines/%d", r.DoctrineID), Category: r.Category,
			Fits: r.Fits, FitsFlyable: r.FitsFlyable, Pilots: r.Pilots, Ready: r.PilotsReady}
		if r.Pilots > 0 {
			d.Pct = float64(r.PilotsReady) / float64(r.Pilots) * 100
		}
		fit := r.NearFitName
		if r.NearShip > 0 {
			if ship := app.typeNameOrID(ctx, r.NearShip); ship != "" && !strings.Contains(strings.ToLower(fit), strings.ToLower(ship)) {
				fit += " (" + ship + ")"
			}
		}
		if d.Near = gapOf(r.NearCharacter, fit, r.NearMissing, r.NearMissingN, r.NearSp); d.Near != nil {
			d.Near.Doctrine, d.Near.Href = d.Name, d.Href
		}
		if r.PilotsReady > 0 {
			flyable++
		}
		v.Doctrines = append(v.Doctrines, d)
		ids = append(ids, r.DoctrineID)
	}
	if len(v.Doctrines) == 0 {
		return v
	}

	if in.Draws("medium") {
		for _, d := range v.Doctrines {
			if d.Near != nil && d.Ready < d.Pilots {
				v.Nearest = append(v.Nearest, *d.Near)
			}
		}
		// Closest to flying first: the fewest skill points to go.
		sort.SliceStable(v.Nearest, func(i, j int) bool { return v.Nearest[i].sp < v.Nearest[j].sp })
	}
	if in.Draws("large") {
		if len(ids) > doctrinesFitDoctrines {
			ids = ids[:doctrinesFitDoctrines]
		}
		fits, err := app.queries.ListDoctrineFitReadinessByUser(ctx, db.ListDoctrineFitReadinessByUserParams{UserID: in.UserID, DoctrineIds: ids, FitsEach: doctrinesFitsEach})
		if err != nil {
			logging.Errorf("home: doctrine fit readiness of user %d: %v", in.UserID, err)
		}
		at := make(map[int64]int, len(v.Doctrines))
		for i, d := range v.Doctrines {
			at[d.ID] = i
		}
		for _, f := range fits {
			i, ok := at[f.DoctrineID]
			if !ok {
				continue
			}
			fv := readyFitView{Name: f.Name, Role: f.FleetRole, ShipID: f.ShipTypeID}
			for _, id := range f.ReadyIds {
				fv.Ready = append(fv.Ready, readyPilotRef{ID: id, Name: names[id]})
			}
			fv.Near = gapOf(f.NearCharacter, f.Name, f.NearMissing, f.NearMissingN, f.NearSp)
			v.Doctrines[i].FitRows = append(v.Doctrines[i].FitRows, fv)
		}
		for i, d := range v.Doctrines {
			if n := int64(d.Fits) - int64(len(d.FitRows)); len(d.FitRows) > 0 && n > 0 {
				v.Doctrines[i].FitsMore = pluralCount(n, "more fit", "more fits")
			}
		}
	}

	// The lead counts every doctrine, not only the ones listed: the
	// list keeps the least ready first and drops the most flyable.
	total, flyableAll := int64(len(v.Doctrines)), int64(flyable)
	if c, err := app.queries.CountDoctrineReadinessByUser(ctx, in.UserID); err != nil {
		logging.Errorf("home: doctrine readiness totals of user %d: %v", in.UserID, err)
	} else {
		total, flyableAll = c.Doctrines, c.Flyable
	}

	v.Lead = wLead{Label: "Doctrines your pilots can fly", Href: "/doctrines/",
		Value: fmt.Sprintf("%d of %d", flyableAll, total),
		Full:  fmt.Sprintf("A pilot of yours can fly %d of your %d doctrines", flyableAll, total)}
	// The nearest gap anywhere: the doctrine whose nearest pilot has the
	// fewest skill points to go.
	var g *readyGapView
	for _, d := range v.Doctrines {
		if d.Near != nil && (g == nil || d.Near.sp < g.sp) {
			g = d.Near
		}
	}
	if g != nil {
		v.Lead.Sub = fmt.Sprintf("Closest: %s, %s skill points for %s", g.Pilot, g.SP, g.Doctrine)
	} else {
		v.Lead.Sub = "Every pilot can fly every doctrine"
	}
	return v
}
