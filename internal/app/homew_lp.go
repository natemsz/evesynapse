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
	regions := make([]string, len(marketRegions))
	for i, r := range marketRegions {
		regions[i] = strconv.FormatInt(r.ID, 10) + ":" + r.Name
	}
	registerHomeWidget(homeWidgetDef{
		ID: widgetLP, Title: "Loyalty Points", Category: wcatMarket, Feature: "standings",
		Href:        "/market/lp/",
		Description: "Loyalty points your pilots hold and what the best offer in each store turns them into.",
		Default:     wSize{1, 2},
		Forms: []homeWidgetForm{
			{Name: "small", Sizes: "1x1", Says: "LP held across every corporation and its best ISK value."},
			{Name: "tall", Sizes: "1x2 1x3", Says: "LP per corporation, with the best ISK per LP and what it is worth."},
			{Name: "medium", Sizes: "2x2 2x3", Says: "LP per corporation beside the LP each pilot holds."},
			{Name: "large", Sizes: "3x2 3x3", Says: "LP per corporation, its value as ranked bars, and each pilot's LP."},
		},
		Needs: []string{esi.SnapLoyalty},
		Options: []widgetOption{optScope(),
			withHint(optChoice("region", "Value offers in", strconv.FormatInt(defaultMarketRegion, 10), regions...),
				"The market the best offer is sold into."),
		},
		Build: buildLPWidget,
	})
}

// lpWidgetCorps is the most corporations the widget values: one stored
// offer read each.
const lpWidgetCorps = 10

// lpCorpRow is the LP held with one corporation.
type lpCorpRow struct {
	CorpID int64
	Name   string
	Href   string
	LP     string
	PerLP  string // the best ISK per LP, "" when no offer is valued
	Value  string // LP times the best rate, short
	Pilots int
	points int64
	value  float64
}

// lpPilotRow is the LP one pilot holds.
type lpPilotRow struct {
	CharID int64
	Name   string
	LP     string
	Corps  int
	points int64
}

// lpWidgetView is what the Loyalty Points forms draw.
type lpWidgetView struct {
	Lead   wLead
	Corps  []lpCorpRow
	Pilots []lpPilotRow
	Ranked *wRanked
	Note   string // where the values come from and how old they are
	Known  bool
	Empty  wEmpty
}

func buildLPWidget(ctx context.Context, app *Application, in *widgetInput) any {
	v := &lpWidgetView{Empty: wEmpty{Text: "Loyalty points have not loaded yet.", Step: "Open the LP stores", Href: "/market/lp/"}}
	regionID, _ := strconv.ParseInt(in.Opts.Get("region"), 10, 64)
	if !marketHubRegion(regionID) {
		regionID = defaultMarketRegion
	}
	regionName, _ := app.marketRegionName(ctx, regionID)
	byCorp := map[int64]*lpCorpRow{}
	var total int64
	for _, b := range in.Bundles {
		if !b.loyaltyKnown {
			continue
		}
		v.Known = true
		p := lpPilotRow{CharID: b.ch.CharacterID, Name: b.ch.Name}
		for _, l := range b.loyalty {
			if l.LoyaltyPoints <= 0 {
				continue
			}
			c := byCorp[l.CorporationID]
			if c == nil {
				c = &lpCorpRow{CorpID: l.CorporationID, Href: "/market/lp/?corp=" + strconv.FormatInt(l.CorporationID, 10)}
				byCorp[l.CorporationID] = c
			}
			c.points += l.LoyaltyPoints
			c.Pilots++
			p.points += l.LoyaltyPoints
			p.Corps++
			total += l.LoyaltyPoints
		}
		if p.points > 0 {
			p.LP = esi.FormatInt(p.points)
			v.Pilots = append(v.Pilots, p)
		}
	}
	for _, c := range byCorp {
		v.Corps = append(v.Corps, *c)
	}
	sort.Slice(v.Corps, func(i, j int) bool {
		if v.Corps[i].points != v.Corps[j].points {
			return v.Corps[i].points > v.Corps[j].points
		}
		return v.Corps[i].CorpID < v.Corps[j].CorpID
	})
	sort.Slice(v.Pilots, func(i, j int) bool { return v.Pilots[i].points > v.Pilots[j].points })

	var worth float64
	var newest time.Time
	priced := 0
	for i := range v.Corps {
		c := &v.Corps[i]
		c.Name = app.CorpDisplayName(ctx, c.CorpID)
		c.LP = esi.FormatInt(c.points)
		if i >= lpWidgetCorps {
			continue
		}
		best, err := app.queries.GetBestLPOfferValue(ctx, db.GetBestLPOfferValueParams{RegionID: regionID, CorporationID: c.CorpID})
		if err != nil {
			logging.Errorf("home: best LP offer of corporation %d: %v", c.CorpID, err)
			continue
		}
		if best.IskPerLp <= 0 {
			continue
		}
		c.value = best.IskPerLp * float64(c.points)
		c.PerLP = esi.FormatISK(best.IskPerLp)
		c.Value = iskCompact(c.value)
		worth += c.value
		priced++
		if best.AsOf.After(newest) {
			newest = best.AsOf
		}
	}
	v.Lead = wLead{Label: "LP held", Value: iskCompact(float64(total)), Unit: "LP", Full: esi.FormatInt(total) + " LP", Href: "/market/lp/"}
	if worth > 0 {
		v.Lead.Sub = "Worth " + iskCompact(worth) + " ISK at the best offers"
		v.Note = "Best offer per store, sold into " + regionName + ", valued " + ageText(in.Now, newest) + "."
	} else if total > 0 {
		v.Lead.Sub = "No store offer valued yet"
	}
	if in.Draws("large") && priced > 0 {
		items := make([]wRankItem, 0, priced)
		for _, c := range v.Corps {
			if c.value > 0 {
				items = append(items, wRankItem{Name: c.Name, Text: c.Value, Full: c.Value + " ISK", Value: c.value})
			}
		}
		sort.SliceStable(items, func(i, j int) bool { return items[i].Value > items[j].Value })
		v.Ranked, _ = wRankedBars("Value of the LP held, by corporation", items, lpWidgetCorps)
	}
	if v.Known && total == 0 {
		v.Empty = wEmpty{Text: "Your pilots hold no loyalty points.", Step: "Open the LP stores", Href: "/market/lp/"}
	}
	return v
}
