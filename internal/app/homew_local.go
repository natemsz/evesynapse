package app

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/intelanalyze"
	"evesynapse/internal/logging"
)

func init() {
	registerHomeWidget(homeWidgetDef{
		ID: widgetLocal, Title: "Radar", Category: wcatIntel, Feature: "location",
		Href:        "/map/",
		Description: "The systems your pilots are in: kills and jumps in the last hour, gate camps, and the intel your channels have on them.",
		Default:     wSize{1, 2},
		Forms: []homeWidgetForm{
			{Name: "small", Sizes: "1x1", Says: "How many of your pilots' systems saw kills in the last hour, and any gate camp."},
			{Name: "tall", Sizes: "1x2 1x3 1x4", Says: "Each system with its pilots, last hour's kills and jumps, any gate camp, and the latest intel."},
			{Name: "medium", Sizes: "2x2 2x3", Says: "The systems beside every intel report on them from the last hour."},
			{Name: "large", Sizes: "3x2 3x3", Says: "Adds each system's pilots by name, its last 24 hours, and who keeps turning up on a camp's kills."},
		},
		Needs:   []string{esi.SnapLocation, esi.SnapOnline},
		Options: []widgetOption{optScope()},
		Build:   buildNearWidget,
	})
}

// nearWidgetSystems is the most systems the widget reads activity for:
// pilots spread wider than this are rare, and the list shows fewer.
const nearWidgetSystems = 50

// nearWidgetReports is the most intel reports the widget reads.
const nearWidgetReports = 20

// nearWidgetWindow is how far back the widget reads intel: the same
// hour the kill and jump figures cover.
const nearWidgetWindow = time.Hour

// nearPilot is one of the account's pilots in a system.
type nearPilot struct {
	ID     int64
	Name   string
	Online bool
	Docked bool
}

// nearSystem is one system the account's pilots are in.
type nearSystem struct {
	System   sovSystem
	MapHref  string
	Pilots   []nearPilot
	Online   int
	HasStats bool
	Kills    int64 // ships and pods destroyed by players, last hour
	Pods     int64
	Jumps    int64
	Kills24  int64
	Jumps24  int64
	Latest   *reportView // the newest intel report about the system
	Reports  int         // intel reports about the system, last hour
	Camp     *nearCamp   // a gate camp standing in the system
}

// nearCamp is a gate camp the threat layer found and stored
// (system_camps): who holds it, how long, and where in the system.
type nearCamp struct {
	Group string
	Href  string // the group on the killboard
	Kills int32
	Began string // "34 minutes ago"
	Where string // "at Perimeter gate", "across the system" or ""
	Crew  string // the recurring pilots, named
}

// PilotsText is how many of the account's pilots are in the system, and
// how many of them are online.
func (s nearSystem) PilotsText() string {
	t := pluralCount(int64(len(s.Pilots)), "pilot", "pilots")
	if s.Online > 0 && s.Online < len(s.Pilots) {
		t += fmt.Sprintf(", %d online", s.Online)
	} else if s.Online > 0 {
		t += ", online"
	}
	return t
}

// PilotNames lists the pilots for a title.
func (s nearSystem) PilotNames() string {
	names := make([]string, 0, len(s.Pilots))
	for _, p := range s.Pilots {
		names = append(names, p.Name)
	}
	return strings.Join(names, ", ")
}

// KillsText and JumpsText are the last hour's figures in words.
func (s nearSystem) KillsText() string { return countText(s.Kills, "kill", "kills") }
func (s nearSystem) JumpsText() string { return countText(s.Jumps, "jump", "jumps") }

// DayText is the last 24 hours' figures in words.
func (s nearSystem) DayText() string {
	return countText(s.Kills24, "kill", "kills") + ", " + countText(s.Jumps24, "jump", "jumps")
}

// countText is "No kills", "1 kill" or "1,204 kills".
func countText(n int64, one, many string) string {
	switch n {
	case 0:
		return "No " + many
	case 1:
		return "1 " + one
	}
	return thousands(n) + " " + many
}

// KillsTitle says what the kill figure counts.
func (s nearSystem) KillsTitle() string {
	return fmt.Sprintf("%s and %s destroyed in the last hour",
		pluralCount(s.Kills-s.Pods, "ship", "ships"), pluralCount(s.Pods, "pod", "pods"))
}

// nearReport is one intel report about a system the pilots are in.
type nearReport struct {
	reportView
	About sovSystem // the pilots' system the report is about
	Far   bool      // filed under the far side of a gate into About
}

// nearView is what the Radar forms draw.
type nearView struct {
	Lead     wLead
	Systems  []nearSystem
	Reports  []nearReport
	Hot      int  // systems with kills in the last hour or a camp standing
	Camps    int  // systems with a gate camp standing
	Channels bool // the account can read at least one intel channel
	Empty    wEmpty
}

func buildNearWidget(ctx context.Context, app *Application, in *widgetInput) any {
	v := &nearView{Empty: wEmpty{Text: "No locations for your pilots yet.", Step: "Open the map", Href: "/map/"}}
	names := app.newKillNames(ctx)
	bySystem := map[int64]*nearSystem{}
	var order []int64
	for _, b := range in.Bundles {
		if b.location == nil || b.location.SolarSystemID <= 0 {
			continue
		}
		id := b.location.SolarSystemID
		s := bySystem[id]
		if s == nil {
			if len(order) >= nearWidgetSystems {
				continue
			}
			sys := names.system(id)
			s = &nearSystem{System: sovSystemOf(id, sys.Name, sys.Security, names.region(sys.RegionID)),
				MapHref: fmt.Sprintf("/map/?system=%d", id)}
			bySystem[id] = s
			order = append(order, id)
		}
		p := nearPilot{ID: b.ch.CharacterID, Name: b.ch.Name, Docked: b.location.StationID > 0 || b.location.StructureID > 0}
		if b.online != nil && b.online.Online {
			p.Online = true
			s.Online++
		}
		s.Pilots = append(s.Pilots, p)
	}
	if len(order) == 0 {
		return v
	}

	for id, c := range app.standingCamps(ctx, order, in.Now) {
		camp := &nearCamp{Group: c.GroupName, Kills: c.Kills, Began: agoText(in.Now.Sub(c.Since)),
			Href: "/intel/kills/?" + c.GroupKind + "=" + strconv.FormatInt(c.GroupID, 10), Crew: strings.Join(c.Crew, ", ")}
		switch {
		case c.Celestial != "":
			camp.Where = "at " + c.Celestial
		case c.Spread:
			camp.Where = "across the system"
		}
		bySystem[id].Camp = camp
	}

	stats, err := app.queries.ListSystemActivityStatsByIDs(ctx, order)
	if err != nil {
		logging.Errorf("home: activity around the pilots of user %d: %v", in.UserID, err)
	}
	for _, st := range stats {
		if s := bySystem[st.SystemID]; s != nil {
			s.HasStats = true
			s.Kills, s.Pods, s.Jumps = st.ShipKills1h+st.PodKills1h, st.PodKills1h, st.Jumps1h
			s.Kills24, s.Jumps24 = st.ShipKills24h+st.PodKills24h, st.Jumps24h
		}
	}

	if in.Draws("tall") || in.Draws("medium") || in.Draws("large") {
		app.nearReports(ctx, in, v, bySystem, order)
	}

	for _, id := range order {
		s := bySystem[id]
		sort.SliceStable(s.Pilots, func(i, j int) bool {
			if s.Pilots[i].Online != s.Pilots[j].Online {
				return s.Pilots[i].Online
			}
			return s.Pilots[i].Name < s.Pilots[j].Name
		})
		if s.Hot() {
			v.Hot++
		}
		if s.Camp != nil {
			v.Camps++
		}
		v.Systems = append(v.Systems, *s)
	}
	// Where the trouble is first: a gate camp, kills, then fresh
	// intel, then where most of the pilots are.
	sort.SliceStable(v.Systems, func(i, j int) bool {
		a, b := v.Systems[i], v.Systems[j]
		if (a.Camp != nil) != (b.Camp != nil) {
			return a.Camp != nil
		}
		if a.Kills != b.Kills {
			return a.Kills > b.Kills
		}
		if (a.Latest != nil) != (b.Latest != nil) {
			return a.Latest != nil
		}
		if a.Latest != nil && a.Latest.At != b.Latest.At {
			return a.Latest.At > b.Latest.At
		}
		if len(a.Pilots) != len(b.Pilots) {
			return len(a.Pilots) > len(b.Pilots)
		}
		return a.System.Place.Name < b.System.Place.Name
	})

	of := pluralCount(int64(len(v.Systems)), "system", "systems")
	v.Lead = wLead{Label: "Systems with kills, last hour", Href: "/map/"}
	if v.Camps > 0 {
		v.Lead.Label = "Systems with kills or a camp"
	}
	if v.Hot > 0 {
		v.Lead.Value = thousands(int64(v.Hot))
		v.Lead.Sub = "of the " + of + " your pilots are in"
		v.Lead.Full = fmt.Sprintf("%d of the %s your pilots are in saw kills in the last hour", v.Hot, of)
		if v.Camps > 0 {
			v.Lead.Sub = campLeadText(v)
		}
	} else {
		v.Lead.Label = "Last hour"
		v.Lead.Value = "Quiet"
		v.Lead.Sub = "No kills in the " + of + " your pilots are in"
		v.Lead.Full = "Quiet where your pilots are: no kills in the last hour"
	}
	return v
}

// nearReports reads the last hour's intel about the pilots' systems in
// the channels the account can read, and gives each system its newest.
// A gate reading filed under a neighbouring system that names one of
// the pilots' systems as its gate is about that system too; a clear one
// is phrased from the far side, as the system page phrases it.
func (app *Application) nearReports(ctx context.Context, in *widgetInput, v *nearView, bySystem map[int64]*nearSystem, order []int64) {
	id, chans := in.Shared.IntelChannels(ctx)
	if len(chans) == 0 {
		return
	}
	v.Channels = true
	ids := make([]int64, len(chans))
	chName := make(map[int64]string, len(chans))
	for i, c := range chans {
		ids[i] = c.ID
		chName[c.ID] = c.Name
	}
	rows, err := app.queries.ListIntelReportsAboutSystems(ctx, db.ListIntelReportsAboutSystemsParams{
		ChannelIds: ids, Since: in.Now.Add(-nearWidgetWindow), SystemIds: order, MaxRows: nearWidgetReports,
	})
	if err != nil {
		logging.Errorf("home: intel around the pilots of user %d: %v", in.UserID, err)
		return
	}
	feed := make([]db.ListIntelReportsAcrossChannelsRow, len(rows))
	for i, r := range rows {
		feed[i] = db.ListIntelReportsAcrossChannelsRow(r)
	}
	for _, rv := range app.reportViewsOf(ctx, id, feed, chName) {
		about, far := bySystem[rv.SystemID], false
		if about == nil {
			// Filed under the far side of a gate into one of the
			// pilots' systems.
			about, far = bySystem[rv.GateID], true
			if about == nil {
				continue
			}
			if rv.Kind == intelanalyze.KindClear {
				rv.Text = "The " + rv.SystemName + " side is clear"
			}
		}
		about.Reports++
		if about.Latest == nil {
			r := rv
			about.Latest = &r
		}
		v.Reports = append(v.Reports, nearReport{reportView: rv, About: about.System, Far: far})
	}
}

// campLeadText names the gate camps for the small form's sub line.
func campLeadText(v *nearView) string {
	if v.Camps == 1 {
		for _, s := range v.Systems {
			if s.Camp != nil {
				return "Gate camp in " + s.System.Place.Name
			}
		}
	}
	return fmt.Sprintf("Gate camps in %d of the systems your pilots are in", v.Camps)
}

// Hot reports whether the system needs the bad edge: kills in the last
// hour, or a camp standing.
func (s nearSystem) Hot() bool { return s.Kills > 0 || s.Camp != nil }
