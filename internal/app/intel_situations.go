package app

// ---------------------------------------------------------------------------
// Intel situations: a channel's (or the map reader's) recent reports
// taken together by system (intelanalyze.Situations) -- several scouts
// on one camp are one situation, a pilot seen next door shows where
// they came from -- each with a danger score and connected to what the
// site knows: every pilot's page and their kills this week, the ships,
// the gate.
//
// Danger is a score from 0 to 100, read as low, medium or high:
// what kind of situation it is, how many hostiles, how active its
// pilots have been (PvP kills in the last week), the ships (hunters,
// interdictors, bombers, capitals), and whether more than one scout
// saw it. Friendlies and clears are no danger.
// ---------------------------------------------------------------------------

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/intelanalyze"
	"evesynapse/internal/logging"
)

const intelPilotKillWindow = 7 * 24 * time.Hour

type situationPilot struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Friendly bool   `json:"friendly,omitempty"`
	Kills    int64  `json:"kills,omitempty"` // PvP kills this week
}

// situationView is one situation as the channel page and the map show it.
type situationView struct {
	Sys       int64            `json:"sys"`
	SysName   string           `json:"sysName"`
	SecText   string           `json:"sec"`
	SecTier   int              `json:"tier"`
	Kind      string           `json:"kind"`
	Label     string           `json:"label"`
	Tone      string           `json:"tone"`
	Danger    int              `json:"danger"`
	Level     string           `json:"level,omitempty"` // "High", "Medium", "Low"; "" when calm
	Pilots    []situationPilot `json:"pilots,omitempty"`
	Ships     []reportShip     `json:"ships,omitempty"`
	Count     int              `json:"count,omitempty"`
	Gate      string           `json:"gate,omitempty"`
	From      string           `json:"from,omitempty"`
	FromID    int64            `json:"fromID,omitempty"`
	Reporters int              `json:"reporters"`
	At        int64            `json:"at"`
	Ago       string           `json:"ago"`
	Calm      bool             `json:"calm,omitempty"`
}

// intelDanger scores a situation.
func intelDanger(s intelanalyze.Situation, kills map[int64]int64) int {
	if s.Calm() {
		return 0
	}
	base := map[string]int{
		intelanalyze.KindCapital: 70, intelanalyze.KindCamp: 60, intelanalyze.KindFleet: 55,
		intelanalyze.KindBubble: 50, intelanalyze.KindSighting: 30, intelanalyze.KindLocal: 15,
	}[s.Kind]
	score := base + min(20, 2*s.Count)
	active := int64(0)
	for _, p := range s.Pilots {
		if !p.Friendly || p.Red {
			active += min(10, kills[p.ID])
		}
	}
	score += int(min(25, active))
	ships := 0
	for _, sh := range s.Ships {
		switch sh.Class {
		case intelanalyze.ClassCapital:
			ships += 20
		case intelanalyze.ClassHunter, intelanalyze.ClassBomber:
			ships += 10
		case intelanalyze.ClassInterdict:
			ships += 8
		}
	}
	score += min(30, ships)
	if s.Reporters > 1 {
		score += 5
	}
	return min(100, score)
}

func dangerLevel(score int) string {
	switch {
	case score >= 70:
		return "High"
	case score >= 40:
		return "Medium"
	case score > 0:
		return "Low"
	}
	return ""
}

// recentReports is the analysed reports, as Situations takes them, of
// the last half hour's rows.
func recentReports(rows []db.ListRecentIntelReportsRow) []intelanalyze.Report {
	out := make([]intelanalyze.Report, 0, len(rows))
	for _, r := range rows {
		rep := intelanalyze.Report{ID: r.ID, System: r.SystemID, At: r.SeenAt.Unix(), Reporter: r.Reporter,
			Hostiles: int(r.Hostiles), Clear: r.Clear, Kind: r.Kind}
		if r.Analysis != "" {
			_ = json.Unmarshal([]byte(r.Analysis), &rep.Analysis)
		}
		out = append(out, rep)
	}
	return out
}

// channelSituations is what is going on now in one channel's reports.
func (app *Application) channelSituations(ctx context.Context, channelID int64) []situationView {
	rows, err := app.queries.ListRecentIntelReports(ctx, db.ListRecentIntelReportsParams{ChannelIds: []int64{channelID}, Since: time.Now().Add(-intelLive)})
	if err != nil {
		logging.Warnf("intel: situations of channel %d: %v", channelID, err)
		return nil
	}
	return app.intelSituations(ctx, recentReports(rows))
}

// intelSituations takes analysed reports together and shows them.
func (app *Application) intelSituations(ctx context.Context, reports []intelanalyze.Report) []situationView {
	if len(reports) == 0 {
		return nil
	}
	neighbours := map[int64]map[int64]bool{}
	adjacent := func(a, b int64) bool {
		if neighbours[a] == nil {
			neighbours[a] = map[int64]bool{}
			if rows, err := app.queries.ListSDESystemNeighbours(ctx, a); err == nil {
				for _, n := range rows {
					neighbours[a][n] = true
				}
			}
		}
		return neighbours[a][b]
	}
	sits := intelanalyze.Situations(reports, adjacent, int64(intelLive/time.Second))
	// The named pilots' PvP kills this week, in one query.
	var ids []int64
	for _, s := range sits {
		for _, p := range s.Pilots {
			ids = append(ids, p.ID)
		}
	}
	kills := map[int64]int64{}
	if len(ids) > 0 {
		rows, err := app.queries.CountPilotKillsSince(ctx, db.CountPilotKillsSinceParams{Ids: ids, Since: time.Now().Add(-intelPilotKillWindow)})
		if err != nil {
			logging.Warnf("intel: pilots' kills: %v", err)
		}
		for _, r := range rows {
			kills[r.CharacterID] = r.Kills
		}
	}
	names := app.newKillNames(ctx)
	now := time.Now()
	out := make([]situationView, 0, len(sits))
	for _, s := range sits {
		sys := names.system(s.System)
		v := situationView{Sys: s.System, SysName: sys.Name, Kind: s.Kind, Label: intelKindLabels[s.Kind], Count: s.Count,
			Reporters: s.Reporters, At: s.Last, Ago: ageText(now, time.Unix(s.Last, 0)), Calm: s.Calm()}
		v.SecText, v.SecTier = secRound(sys.Security)
		var rv reportView
		applyAnalysis(&rv, s.Kind, "", names)
		v.Tone = rv.KindTone
		v.Danger = intelDanger(s, kills)
		v.Level = dangerLevel(v.Danger)
		for _, p := range s.Pilots {
			v.Pilots = append(v.Pilots, situationPilot{ID: p.ID, Name: p.Name, Friendly: p.Friendly && !p.Red, Kills: kills[p.ID]})
		}
		// The most active pilots first.
		sort.SliceStable(v.Pilots, func(i, j int) bool { return v.Pilots[i].Kills > v.Pilots[j].Kills })
		for _, sh := range s.Ships {
			v.Ships = append(v.Ships, reportShip{ID: sh.TypeID, Name: sh.Name, N: sh.N})
		}
		if s.Gate > 0 {
			v.Gate = names.system(s.Gate).Name
		}
		if s.From > 0 {
			v.FromID, v.From = s.From, names.system(s.From).Name
		}
		out = append(out, v)
	}
	return out
}
