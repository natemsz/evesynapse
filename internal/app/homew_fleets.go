package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"evesynapse/internal/logging"
)

func init() {
	registerHomeWidget(homeWidgetDef{
		ID: widgetFleets, Title: "Fleets", Category: wcatFleets, Feature: "fleet_ops",
		Href:        fleetsPath,
		Description: "Fleets running now and coming up that you can join, with your sign-up and, for fleets you run, who is coming.",
		Default:     wSize{1, 2},
		Forms: []homeWidgetForm{
			{Name: "small", Sizes: "1x1", Says: "A fleet running now, else the next one and when it forms up."},
			{Name: "wide", Sizes: "2x1 3x1", Says: "The next three fleets side by side."},
			{Name: "tall", Sizes: "1x2 1x3 1x4", Says: "Running fleets, then upcoming ones with their commander, doctrine and your sign-up."},
			{Name: "medium", Sizes: "2x2 2x3", Says: "The list, and for fleets you run, who is coming in each role."},
			{Name: "large", Sizes: "3x2 3x3", Says: "The list beside the fleets you run, each with its roles."},
		},
		Options: []widgetOption{
			optChoice("show", "Show", fleetsShowAll,
				fleetsShowAll+":Fleets I can see", fleetsShowSigned+":Fleets I signed up for", fleetsShowRun+":Fleets I run"),
		},
		Build: buildFleetsWidget,
	})
}

// The show option's values.
const (
	fleetsShowAll    = "all"
	fleetsShowSigned = "signed"
	fleetsShowRun    = "run"
)

// fleetsWidgetRows is the most fleets a list form names.
const fleetsWidgetRows = 12

// fleetsWidgetRun is the most fleets the account runs whose roles the
// medium and large forms count.
const fleetsWidgetRun = 3

// homeFleetRow is one fleet as the widget draws it.
type homeFleetRow struct {
	ID                 int64
	Href, Title, Owner string
	FC, Doctrine       string // empty when the account may not see them
	FormUp             string // empty when the account may not see it
	StartsRaw, Starts  string
	Left               string // "in 3h 20m" for a fleet ahead
	Running            string // "Running for 40m" for a live one
	Live               bool
	Answer             string // "Coming", "Maybe", "Not coming" or ""
	Coming             string // "12 coming, 3 maybe"
	Run                bool   // the account may manage it
	Roles              []opRoleCount
	RolesRanked        *wRanked
}

// fleetsWidgetView is what the Fleets forms draw.
type fleetsWidgetView struct {
	Rows  []homeFleetRow
	First *homeFleetRow // small: the running fleet, else the next
	Next  []homeFleetRow
	Run   []homeFleetRow // large: fleets the account runs, with their roles
	Empty wEmpty
}

func buildFleetsWidget(ctx context.Context, app *Application, in *widgetInput) any {
	v := &fleetsWidgetView{Empty: wEmpty{Text: "No fleets running or coming up for you.", Step: "Open the Fleets board", Href: fleetsPath}}
	show := in.Opts.Get("show")
	switch show {
	case fleetsShowSigned:
		v.Empty = wEmpty{Text: "You have not signed up for a fleet coming up.", Step: "Find one on the Fleets board", Href: fleetsPath}
	case fleetsShowRun:
		v.Empty = wEmpty{Text: "You run no fleet coming up.", Step: "Plan one", Href: fleetsPath}
	}
	for _, f := range in.Shared.Fleets(ctx) {
		if !f.Access.View {
			continue
		}
		switch show {
		case fleetsShowSigned:
			if f.Answer != opYes && f.Answer != opMaybe {
				continue
			}
		case fleetsShowRun:
			if !f.Access.Manage {
				continue
			}
		}
		if len(v.Rows) >= fleetsWidgetRows {
			break
		}
		v.Rows = append(v.Rows, app.homeFleetRowOf(ctx, f, in.Now))
	}
	if len(v.Rows) == 0 {
		return v
	}
	v.First = &v.Rows[0]
	v.Next = v.Rows[:min(3, len(v.Rows))]
	if in.Draws("medium") || in.Draws("large") {
		app.fillFleetRoles(ctx, v, in.Draws("medium"), in.Draws("large"))
	}
	return v
}

// homeFleetRowOf writes one fleet for the widget, showing only what the
// Fleets page would show the account.
func (app *Application) homeFleetRowOf(ctx context.Context, f homeFleet, now time.Time) homeFleetRow {
	op := f.Op
	start := op.StartsAt.UTC()
	row := homeFleetRow{
		ID: op.ID, Href: opURL(op.ID), Title: op.Title, Owner: f.Owner,
		StartsRaw: start.Format(time.RFC3339), Starts: formatFinish(start.Format(time.RFC3339)),
		Live: f.Live, Answer: answerLabel(f.Answer), Coming: fleetCountText(op), Run: f.Access.Manage,
	}
	if f.Access.SeeNames && op.FcCharacterID != 0 {
		row.FC = app.displayCharacter(ctx, op.FcCharacterID)
	}
	row.Doctrine = strings.TrimSpace(op.Doctrine)
	if f.Access.SeeFormUp {
		row.FormUp = strings.TrimSpace(op.FormUp)
	}
	if f.Live {
		row.Running = "Running for " + fleetRunningText(now.Sub(op.StartsAt))
	} else {
		row.Left = "in " + humanDuration(op.StartsAt.Sub(now))
	}
	return row
}

// fillFleetRoles counts who is coming in each role to the first fleets
// the account runs, in one grouped read: on their list rows (inline, the
// medium form) and as ranked bars beside the list (aside, the large).
func (app *Application) fillFleetRoles(ctx context.Context, v *fleetsWidgetView, inline, aside bool) {
	var ids []int64
	at := map[int64]int{}
	for i, r := range v.Rows {
		if !r.Run || len(ids) >= fleetsWidgetRun {
			continue
		}
		ids = append(ids, r.ID)
		at[r.ID] = i
	}
	if len(ids) == 0 {
		return
	}
	counts, err := app.queries.ListOpRoleCounts(ctx, ids)
	if err != nil {
		logging.Errorf("home: fleet roles: %v", err)
		return
	}
	byOp := map[int64]map[string]int{}
	for _, c := range counts {
		if byOp[c.OpID] == nil {
			byOp[c.OpID] = map[string]int{}
		}
		byOp[c.OpID][c.FleetRole] = int(c.N)
	}
	for _, id := range ids {
		i := at[id]
		var roles []opRoleCount
		var ranked []wRankItem
		for _, role := range opFleetRoles {
			if n := byOp[id][role]; n > 0 {
				roles = append(roles, opRoleCount{Role: role, Count: n})
				ranked = append(ranked, wRankItem{Name: role, Value: float64(n), Text: fmt.Sprint(n), Full: fmt.Sprintf("%d coming as %s", n, role)})
			}
		}
		if inline {
			v.Rows[i].Roles = roles
		}
		if aside {
			run := v.Rows[i]
			run.RolesRanked, _ = wRankedBars("Coming to "+run.Title+" by role", ranked, len(ranked))
			v.Run = append(v.Run, run)
		}
	}
}
