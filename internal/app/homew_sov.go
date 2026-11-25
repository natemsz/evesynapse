package app

import (
	"context"
	"sort"
	"strconv"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

func init() {
	registerHomeWidget(homeWidgetDef{
		ID: widgetSov, Title: "Sovereignty", Category: wcatIntel, Feature: "intel", Requires: "intel",
		Href:        "/intel/sov/",
		Description: "The space your pilots' alliances hold: live contests, the next vulnerability windows and recent changes.",
		Default:     wSize{1, 2},
		Forms: []homeWidgetForm{
			{Name: "small", Sizes: "1x1", Says: "Systems held and live contests, or the next vulnerability window."},
			{Name: "tall", Sizes: "1x2 1x3", Says: "Live contests and the coming vulnerability windows, with countdowns."},
			{Name: "medium", Sizes: "2x2 2x3", Says: "Contests and windows beside the systems gained and lost this week."},
			{Name: "large", Sizes: "3x2 3x3", Says: "Contests, windows and this week's changes side by side."},
		},
		Needs:   []string{esi.SnapProfile},
		Options: []widgetOption{optScope()},
		Build:   buildSovWidget,
	})
}

// sovWidgetAlliances is the most alliances the widget reads; each costs
// a few bounded reads.
const sovWidgetAlliances = 3

// sovWidgetRows is the most rows each list reads per alliance.
const sovWidgetRows = 10

// sovWidgetItem is one contest or vulnerability window.
type sovWidgetItem struct {
	System   sovSystem
	What     string // "Sovereignty hub contest", "Vulnerable"
	Alliance string
	Open     bool   // running now
	State    string // "Defenders 62%" or "Open now"
	Left     string
	AtRaw    string
	At       string
	at       time.Time
}

// sovWidgetChange is a system gained or lost this week.
type sovWidgetChange struct {
	System sovSystem
	Gained bool
	Ago    string
	at     time.Time
}

// sovWidgetAlliance is one alliance's standing.
type sovWidgetAlliance struct {
	ID     int64
	Name   string
	Href   string
	Held   int64
	Gained int64
	Lost   int64
}

// sovWidgetView is what the Sovereignty forms draw.
type sovWidgetView struct {
	Lead      wLead
	Alliances []sovWidgetAlliance
	Contests  []sovWidgetItem
	Windows   []sovWidgetItem
	Changes   []sovWidgetChange
	Next      *sovWidgetItem // the next window to open, when none is open
	Held      int64
	Empty     wEmpty
}

func buildSovWidget(ctx context.Context, app *Application, in *widgetInput) any {
	v := &sovWidgetView{Empty: wEmpty{Text: "None of your pilots' alliances holds sovereignty.", Step: "Open the sovereignty tracker", Href: "/intel/sov/"}}
	now := in.Now.UTC()
	seen := map[int64]bool{}
	var ids []int64
	for _, b := range in.Bundles {
		if b.profile == nil || b.profile.AllianceID <= 0 || seen[b.profile.AllianceID] {
			continue
		}
		seen[b.profile.AllianceID] = true
		ids = append(ids, b.profile.AllianceID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	weekAgo := now.Add(-7 * 24 * time.Hour)
	changes := in.Draws("medium") || in.Draws("large")
	for _, id := range ids {
		if len(v.Alliances) >= sovWidgetAlliances {
			break
		}
		sum, err := app.queries.AllianceSovSummary(ctx, db.AllianceSovSummaryParams{AllianceID: id, Since: weekAgo})
		if err != nil {
			logging.Errorf("home: sovereignty of alliance %d: %v", id, err)
			continue
		}
		if sum.Held == 0 && sum.Lost == 0 {
			continue
		}
		name := app.AllianceDisplayName(ctx, id)
		v.Alliances = append(v.Alliances, sovWidgetAlliance{ID: id, Name: name, Href: "/intel/sov/?alliance=" + strconv.FormatInt(id, 10),
			Held: sum.Held, Gained: sum.Gained, Lost: sum.Lost})
		v.Held += sum.Held

		contests, err := app.queries.ListSovContests(ctx, db.ListSovContestsParams{AllianceID: id, MaxRows: sovWidgetRows})
		if err != nil {
			logging.Errorf("home: sovereignty contests of alliance %d: %v", id, err)
		}
		for _, c := range contests {
			it := sovWidgetItem{System: sovSystemOf(c.SystemID, c.SystemName, c.Security, c.RegionName),
				What: sovEventLabel(c.EventType), Alliance: name, at: c.StartTime.UTC()}
			if c.StartTime.After(now) {
				it.Left, it.AtRaw = durText(c.StartTime.Sub(now)), c.StartTime.UTC().Format(time.RFC3339)
				it.At = formatFinish(it.AtRaw)
			} else {
				it.Open = true
				it.State = "Defenders " + strconv.Itoa(sovPct(c.DefenderScore)) + "%"
			}
			v.Contests = append(v.Contests, it)
		}
		vuln, err := app.queries.ListSovVulnerable(ctx, db.ListSovVulnerableParams{
			Now: now, Until: now.Add(24 * time.Hour), AllianceID: id, MaxRows: sovWidgetRows,
		})
		if err != nil {
			logging.Errorf("home: sovereignty windows of alliance %d: %v", id, err)
		}
		for _, w := range vuln {
			if !w.VulnStart.Valid || !w.VulnEnd.Valid {
				continue
			}
			it := sovWidgetItem{System: sovSystemOf(w.SystemID, w.SystemName, w.Security, w.RegionName), What: "Vulnerable", Alliance: name}
			if w.VulnStart.Time.After(now) {
				it.at = w.VulnStart.Time.UTC()
			} else {
				it.Open, it.State, it.at = true, "Open now", w.VulnEnd.Time.UTC()
			}
			it.AtRaw = it.at.Format(time.RFC3339)
			it.At = formatFinish(it.AtRaw)
			it.Left = durText(it.at.Sub(now))
			v.Windows = append(v.Windows, it)
		}
		if changes {
			claims, err := app.queries.ListSovRecentClaims(ctx, db.ListSovRecentClaimsParams{Since: weekAgo, AllianceID: id, MaxRows: sovWidgetRows})
			if err != nil {
				logging.Errorf("home: sovereignty claims of alliance %d: %v", id, err)
			}
			for _, c := range claims {
				if !c.ClaimedSince.Valid {
					continue
				}
				v.Changes = append(v.Changes, sovWidgetChange{System: sovSystemOf(c.SystemID, c.SystemName, c.Security, c.RegionName),
					Gained: true, at: c.ClaimedSince.Time})
			}
			losses, err := app.queries.ListSovLosses(ctx, db.ListSovLossesParams{Since: weekAgo, AllianceID: id, MaxRows: sovWidgetRows})
			if err != nil {
				logging.Errorf("home: sovereignty losses of alliance %d: %v", id, err)
			}
			for _, l := range losses {
				v.Changes = append(v.Changes, sovWidgetChange{System: sovSystemOf(l.SystemID, l.SystemName, l.Security, l.RegionName), at: l.ChangedAt})
			}
		}
	}
	// Running first, then by when each opens or closes.
	byTime := func(items []sovWidgetItem) {
		sort.SliceStable(items, func(i, j int) bool {
			if items[i].Open != items[j].Open {
				return items[i].Open
			}
			return items[i].at.Before(items[j].at)
		})
	}
	byTime(v.Contests)
	byTime(v.Windows)
	sort.SliceStable(v.Changes, func(i, j int) bool { return v.Changes[i].at.After(v.Changes[j].at) })
	for i := range v.Changes {
		v.Changes[i].Ago = agoText(now.Sub(v.Changes[i].at))
	}
	for i := range v.Windows {
		if !v.Windows[i].Open {
			v.Next = &v.Windows[i]
			break
		}
	}
	live := 0
	for _, c := range v.Contests {
		if c.Open {
			live++
		}
	}
	v.Lead = wLead{Label: "Systems held", Value: thousands(v.Held), Href: "/intel/sov/",
		Full: pluralCount(v.Held, "system held", "systems held")}
	switch {
	case live > 0:
		v.Lead.Sub = pluralCount(int64(live), "live contest", "live contests")
	case len(v.Contests) > 0:
		v.Lead.Sub = pluralCount(int64(len(v.Contests)), "contest", "contests") + " coming"
	case len(v.Windows) > 0 && v.Windows[0].Open:
		v.Lead.Sub = pluralCount(int64(len(v.Windows)), "system", "systems") + " vulnerable"
	}
	return v
}
