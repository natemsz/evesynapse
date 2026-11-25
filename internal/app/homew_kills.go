package app

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

func init() {
	registerHomeWidget(homeWidgetDef{
		ID: widgetKills, Title: "Kills and Losses", Category: wcatIntel, Feature: "killmails",
		Href:        "/intel/kills/",
		Description: "Your pilots' kills and losses: ISK efficiency, the latest of each, where you fight, and your recent battles.",
		Default:     wSize{1, 2},
		Forms: []homeWidgetForm{
			{Name: "small", Sizes: "1x1", Says: "ISK efficiency with the kills and losses behind it."},
			{Name: "wide", Sizes: "2x1 3x1", Says: "7- and 30-day figures, each with its efficiency."},
			{Name: "tall", Sizes: "1x2 1x3 1x4", Says: "The latest kills and losses with ship, system, value and when."},
			{Name: "medium", Sizes: "2x2 2x3", Says: "The latest beside the systems where you kill most."},
			{Name: "large", Sizes: "3x2 3x3", Says: "Adds your recent battles."},
		},
		Options: []widgetOption{
			optScope(),
			optChoice("window", "Window", "7", "7:7 days", "30:30 days"),
			optChoice("show", "Show", "all", "all:Kills and losses", "losses:Losses only"),
		},
		Build: buildKillsWidget,
	})
}

// killsLatest is the most kills and losses the list forms read.
const killsLatest = 10

// killsBattles is the most battles the large form lists.
const killsBattles = 3

// homeKillsTotals is one window's figures.
type homeKillsTotals struct {
	Window      string // "7 days"
	Kills       int64
	Losses      int64
	Killed      string // ISK destroyed by our appraisal, short; empty when none is valued
	Lost        string // ISK lost by our appraisal, short; empty when none is valued
	KillWord    string // "kill" or "kills"
	LossWord    string // "loss" or "losses"
	Efficiency  string // "82%", or "" with nothing destroyed or lost
	Pct         float64
	Unpriced    int64 // kills and losses we have not valued yet
	killed, lst float64
}

// homeKillRow is one kill or loss.
type homeKillRow struct {
	Href     string
	ShipID   int64
	Ship     string
	System   string
	SystemID int64
	SecText  string
	SecTier  int
	SecKnown bool
	Value    string
	Ago      string
	At       string
	Lost     bool
}

// homeKillBattle is one recent battle.
type homeKillBattle struct {
	Href, Where, When, Outcome string
	Kills, Losses              int32
}

// homeKillsView is what the Kills and Losses forms draw.
type homeKillsView struct {
	Main     homeKillsTotals // the window the settings pick
	Week     homeKillsTotals // wide: 7 days
	Month    homeKillsTotals // wide: 30 days
	Latest   []homeKillRow
	Systems  *wRanked
	Battles  []homeKillBattle
	Any      bool
	LossOnly bool
	Empty    wEmpty
}

func buildKillsWidget(ctx context.Context, app *Application, in *widgetInput) any {
	v := &homeKillsView{LossOnly: in.Opts.Get("show") == "losses",
		Empty: wEmpty{Text: "No kills or losses for your pilots yet.", Step: "Open the killboard", Href: "/intel/kills/"}}
	ids := make([]int64, 0, len(in.Chars))
	for _, c := range in.Chars {
		ids = append(ids, c.CharacterID)
	}
	if len(ids) == 0 {
		return v
	}
	now := in.Now.UTC()
	days, _ := strconv.Atoi(in.Opts.Get("window"))
	if days != 30 {
		days = 7
	}
	var systems []db.ListAccountKillsBySystemRow
	v.Main, systems = app.killsWindow(ctx, ids, now, days)
	if in.Draws("wide") {
		v.Week = v.Main
		if days == 30 {
			v.Month = v.Main
			v.Week, _ = app.killsWindow(ctx, ids, now, 7)
		} else {
			v.Month, _ = app.killsWindow(ctx, ids, now, 30)
		}
	}
	v.Any = v.Main.Kills+v.Main.Losses > 0 || v.Month.Kills+v.Month.Losses > 0

	if in.Draws("tall") || in.Draws("medium") || in.Draws("large") {
		// Losses only reads the losses themselves, so the list holds
		// as many as fit however many kills came after them.
		var rows []db.ListAccountLatestKillsRow
		var err error
		if v.LossOnly {
			var lost []db.ListAccountLatestLossesRow
			lost, err = app.queries.ListAccountLatestLosses(ctx, db.ListAccountLatestLossesParams{CharacterIds: ids, RowLimit: killsLatest})
			for _, r := range lost {
				rows = append(rows, db.ListAccountLatestKillsRow(r))
			}
		} else {
			rows, err = app.queries.ListAccountLatestKills(ctx, db.ListAccountLatestKillsParams{CharacterIds: ids, RowLimit: killsLatest})
		}
		if err != nil {
			logging.Errorf("home: latest kills of user %d: %v", in.UserID, err)
		}
		for _, r := range rows {
			v.Latest = append(v.Latest, app.homeKillRowOf(ctx, r, now))
		}
		if len(v.Latest) > 0 {
			v.Any = true
		}
	}
	if in.Draws("medium") || in.Draws("large") {
		sort.SliceStable(systems, func(i, j int) bool {
			if systems[i].Kills != systems[j].Kills {
				return systems[i].Kills > systems[j].Kills
			}
			return systems[i].OwnKilled > systems[j].OwnKilled
		})
		var ranked []wRankItem
		for _, s := range systems {
			if s.Kills == 0 || len(ranked) >= 5 {
				continue
			}
			name := app.homeKillSystem(ctx, s.SystemID)
			full := fmt.Sprintf("%d kills", s.Kills)
			if s.OwnKilled > 0 {
				full += fmt.Sprintf(", %s ISK destroyed", iskCompact(s.OwnKilled))
			}
			ranked = append(ranked, wRankItem{Name: name, Link: systemLink(s.SystemID, name), Value: float64(s.Kills),
				Text: strconv.FormatInt(s.Kills, 10), Full: full})
		}
		v.Systems, _ = wRankedBars("Where your pilots kill most, last "+v.Main.Window, ranked, len(ranked))
	}
	if in.Draws("large") {
		fights, err := app.queries.ListRecentBattleFightsByUser(ctx, db.ListRecentBattleFightsByUserParams{UserID: in.UserID, RowLimit: killsBattles})
		if err != nil {
			logging.Errorf("home: battles of user %d: %v", in.UserID, err)
		}
		for _, f := range fights {
			v.Battles = append(v.Battles, homeKillBattle{
				Href: fmt.Sprintf("/intel/battle/%d/", f.ID), Where: app.homeKillSystem(ctx, f.SystemID),
				When: humanDuration(now.Sub(f.StartedAt)) + " ago", Outcome: f.Outcome, Kills: f.Kills, Losses: f.Losses,
			})
		}
	}
	return v
}

// killsWindow is the account's kills and losses over the last days, and
// the per-system rows they came from. ISK is our own appraisal only: a
// kill we have not valued counts as unpriced, never at an outside value.
func (app *Application) killsWindow(ctx context.Context, ids []int64, now time.Time, days int) (homeKillsTotals, []db.ListAccountKillsBySystemRow) {
	t := homeKillsTotals{Window: fmt.Sprintf("%d days", days)}
	rows, err := app.queries.ListAccountKillsBySystem(ctx, db.ListAccountKillsBySystemParams{CharacterIds: ids, Since: now.AddDate(0, 0, -days)})
	if err != nil {
		logging.Errorf("home: kills by system: %v", err)
		return t, nil
	}
	for _, r := range rows {
		t.Kills += r.Kills
		t.Losses += r.Losses
		t.killed += r.OwnKilled
		t.lst += r.OwnLost
		t.Unpriced += r.Unpriced
	}
	if t.killed > 0 {
		t.Killed = iskCompact(t.killed)
	}
	if t.lst > 0 {
		t.Lost = iskCompact(t.lst)
	}
	t.KillWord, t.LossWord = "kills", "losses"
	if t.Kills == 1 {
		t.KillWord = "kill"
	}
	if t.Losses == 1 {
		t.LossWord = "loss"
	}
	if sum := t.killed + t.lst; sum > 0 {
		t.Pct = t.killed / sum * 100
		t.Efficiency = strconv.FormatFloat(t.Pct, 'f', 0, 64) + "%"
	}
	return t, rows
}

// homeKillRowOf writes one kill or loss.
func (app *Application) homeKillRowOf(ctx context.Context, r db.ListAccountLatestKillsRow, now time.Time) homeKillRow {
	row := homeKillRow{
		Href: fmt.Sprintf("/intel/kill/%d/", r.KillmailID), ShipID: r.ShipTypeID, Ship: app.typeNameOrID(ctx, r.ShipTypeID),
		SystemID: r.SystemID, System: app.homeKillSystem(ctx, r.SystemID), Lost: r.Lost,
		Ago: humanDuration(now.Sub(r.KillmailTime)) + " ago", At: r.KillmailTime.UTC().Format(time.RFC3339),
	}
	if r.Isk > 0 {
		row.Value = iskCompact(r.Isk)
	}
	if sys, ok := app.sdeSystem(r.SystemID); ok {
		row.SecText, row.SecTier, row.SecKnown = sys.SecText, sys.SecTier, true
	}
	return row
}

// homeKillSystem names a system from the in-memory static data, else
// the place-name cache.
func (app *Application) homeKillSystem(ctx context.Context, id int64) string {
	if sys, ok := app.sdeSystem(id); ok && sys.Name != "" {
		return sys.Name
	}
	return app.locationTitle(ctx, id, "solar_system")
}

// Tone is the efficiency meter's colour: good from half up.
func (t homeKillsTotals) Tone() string {
	if t.Efficiency == "" || t.Pct >= 50 {
		return ""
	}
	return "bad"
}
