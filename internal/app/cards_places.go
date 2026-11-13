package app

// Summary cards for places (cards.go has the generator): a solar
// system, a region, and the universe map with its focus and layer.
// Every figure is one the place's page shows, read from the same
// stored rows: the SDE, the distilled activity stats and the
// sovereignty table.

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"evesynapse/internal/card"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// placeCardMaxAge: activity figures move hourly.
const placeCardMaxAge = time.Hour

// mapLayerTags are the layers' short names for a map card's header
// tag.
var mapLayerTags = map[string]string{
	"security": "SECURITY",
	"kills":    "KILLS",
	"npc":      "NPC KILLS",
	"jumps":    "JUMPS",
	"sov":      "SOVEREIGNTY",
	"fw":       "FACTION WARFARE",
	"inc":      "INCURSIONS",
	"threat":   "THREAT",
}

// mapLayerTitles names the map's layers as the map's own legend
// does (LAYER_TITLES in map.js); security is the default layer.
var mapLayerTitles = map[string]string{
	"security": "Security",
	"kills":    "Ship and pod kills, last hour",
	"npc":      "NPC kills, last hour",
	"jumps":    "Jumps, last hour",
	"sov":      "Sovereignty",
	"fw":       "Faction Warfare",
	"inc":      "Incursions",
	"threat":   "Threat now",
}

// The security bands' colours on a region card: tiers from the
// killboard's scale, one per band.
const (
	bandHighTier = 8
	bandLowTier  = 3
	bandNullTier = 0
)

// placeActivityStats is the figure row for a place's trailing 24
// hours, as its page's activity block counts them.
func placeActivityStats(ship24, jumps24, npc24 int64) []card.Stat {
	return []card.Stat{
		{Label: "SHIP KILLS 24H", Value: card.Figure(esi.FormatInt(ship24))},
		{Label: "JUMPS 24H", Value: card.Figure(esi.FormatInt(jumps24))},
		{Label: "NPC KILLS 24H", Value: card.Figure(esi.FormatInt(npc24))},
	}
}

// systemCard reads a system's card: its security status in the
// panel, its region, the day's activity and its sovereignty holder.
func (app *Application) systemCard(ctx context.Context, id int64) (card.Card, bool) {
	sys, err := app.queries.GetSDESystem(ctx, id)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			logging.Warnf("system card %d: %v", id, err)
		}
		return card.Card{}, false
	}
	sec, tier := secRound(sys.Security)
	c := card.Card{Kind: "SYSTEM", Title: sys.Name,
		Badge: &card.Badge{Text: sec, Color: card.SecColor(tier), Caption: "SECURITY"}}
	if region, err := app.queries.GetSDERegion(ctx, sys.RegionID); err == nil {
		c.Subtitle = region.Name
	}
	if st, err := app.queries.GetSystemActivityStats(ctx, id); err == nil {
		c.Stats = placeActivityStats(st.ShipKills24h, st.Jumps24h, st.NpcKills24h)
	}
	if sov, err := app.queries.GetSystemSovereignty(ctx, id); err == nil {
		if holder := app.sovHolderName(ctx, sov.AllianceID, sov.CorporationID, sov.FactionID, sov.FactionName); holder != "" {
			c.Detail = &card.Detail{Label: "SOVEREIGNTY", Text: holder}
		}
	}
	return c, true
}

// regionCard reads a region's card: its systems split by security
// band in the panel, the day's activity and its largest holder.
func (app *Application) regionCard(ctx context.Context, id int64) (card.Card, bool) {
	region, err := app.queries.GetSDERegion(ctx, id)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			logging.Warnf("region card %d: %v", id, err)
		}
		return card.Card{}, false
	}
	c := card.Card{Kind: "REGION", Title: region.Name}
	if n, err := app.queries.CountRegionSystemsBySecurity(ctx, id); err == nil {
		total := n.High + n.Low + n.NullSec
		c.Subtitle = sovSystemsLabel(total)
		if total > 0 {
			frac := func(v int64) float64 { return float64(v) / float64(total) }
			c.Bars = []card.Bar{
				{Label: "HIGH-SEC", Count: esi.FormatInt(n.High), Frac: frac(n.High), Color: card.SecColor(bandHighTier)},
				{Label: "LOW-SEC", Count: esi.FormatInt(n.Low), Frac: frac(n.Low), Color: card.SecColor(bandLowTier)},
				{Label: "NULL-SEC", Count: esi.FormatInt(n.NullSec), Frac: frac(n.NullSec), Color: card.SecColor(bandNullTier)},
			}
		}
	}
	if st, err := app.queries.GetRegionActivityStats(ctx, id); err == nil {
		c.Stats = placeActivityStats(st.ShipKills24h, st.Jumps24h, st.NpcKills24h)
	}
	if holders, err := app.queries.ListRegionSovereigntyBreakdown(ctx, db.ListRegionSovereigntyBreakdownParams{RegionID: id, Lim: 2}); err == nil && len(holders) > 0 {
		h := holders[0]
		if name := app.sovHolderName(ctx, h.AllianceID, h.CorporationID, h.FactionID, h.FactionName); name != "" {
			label := "SOVEREIGNTY"
			if len(holders) > 1 {
				label = "LARGEST HOLDER"
			}
			c.Detail = &card.Detail{Label: label, Text: name + " · " + sovSystemsLabel(h.Systems)}
		}
	}
	return c, true
}

// sovHolderName names a sovereignty holder the way the place pages
// do: an alliance, a corporation or an NPC faction.
func (app *Application) sovHolderName(ctx context.Context, allianceID, corpID, factionID int64, factionName string) string {
	switch {
	case allianceID > 0:
		return app.allianceDisplayName(ctx, allianceID)
	case corpID > 0:
		return app.corpDisplayName(ctx, corpID)
	case factionID > 0:
		if factionName != "" {
			return factionName
		}
		return "Faction #" + strconv.FormatInt(factionID, 10)
	}
	return ""
}

func placeCardJob(key string, c card.Card) cardJob {
	return cardJob{key: key, version: cardVersion("", "", c), card: c, maxAge: placeCardMaxAge}
}

// handleSystemCard serves /system/card.png?system={id}.
func (app *Application) handleSystemCard(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.URL.Query().Get("system"), 10, 64)
	if err != nil || id <= 0 {
		app.serveFallbackCard(w)
		return
	}
	c, ok := app.systemCard(r.Context(), id)
	if !ok {
		app.serveFallbackCard(w)
		return
	}
	app.serveCard(w, r, placeCardJob("system:"+strconv.FormatInt(id, 10), c))
}

// handleRegionCard serves /region/card.png?region={id}.
func (app *Application) handleRegionCard(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.URL.Query().Get("region"), 10, 64)
	if err != nil || id <= 0 {
		app.serveFallbackCard(w)
		return
	}
	c, ok := app.regionCard(r.Context(), id)
	if !ok {
		app.serveFallbackCard(w)
		return
	}
	app.serveCard(w, r, placeCardJob("region:"+strconv.FormatInt(id, 10), c))
}

// mapFocus is what a map link shows: the selected system, the
// region it is zoomed to, and the layer. Unknown values read as
// absent, so any link resolves to a card.
type mapFocus struct {
	System int64
	Region int64
	Layer  string
}

// mapFocusOf reads a map link's focus from its query.
func mapFocusOf(q url.Values) mapFocus {
	f := mapFocus{Layer: "security"}
	if id, err := strconv.ParseInt(q.Get("system"), 10, 64); err == nil && id > 0 {
		f.System = id
	}
	if id, err := strconv.ParseInt(q.Get("region"), 10, 64); err == nil && id > 0 {
		f.Region = id
	}
	if _, ok := mapLayerTitles[q.Get("layer")]; ok {
		f.Layer = q.Get("layer")
	}
	return f
}

// cardPath is the map card's address for this focus, its query in
// a fixed order.
func (f mapFocus) cardPath() string {
	q := url.Values{}
	if f.System > 0 {
		q.Set("system", strconv.FormatInt(f.System, 10))
	}
	if f.Region > 0 {
		q.Set("region", strconv.FormatInt(f.Region, 10))
	}
	if f.Layer != "security" {
		q.Set("layer", f.Layer)
	}
	if len(q) == 0 {
		return "/map/card.png"
	}
	return "/map/card.png?" + q.Encode()
}

// mapCard reads a map link's card: the focus system's card, else
// the focus region's, else the whole universe, with the layer named
// in the header tag (and in full on the universe card).
func (app *Application) mapCard(ctx context.Context, f mapFocus) card.Card {
	var c card.Card
	ok := false
	if f.System > 0 {
		c, ok = app.systemCard(ctx, f.System)
	}
	if !ok && f.Region > 0 {
		c, ok = app.regionCard(ctx, f.Region)
	}
	if !ok {
		c = card.Card{Title: "New Eden", Subtitle: "The universe map", Mark: true,
			Detail: &card.Detail{Label: "LAYER", Text: mapLayerTitles[f.Layer]}}
	}
	c.Kind = "MAP · " + mapLayerTags[f.Layer]
	return c
}

// handleMapCard serves /map/card.png with the map link's focus.
func (app *Application) handleMapCard(w http.ResponseWriter, r *http.Request) {
	f := mapFocusOf(r.URL.Query())
	key := "map:" + strconv.FormatInt(f.System, 10) + ":" + strconv.FormatInt(f.Region, 10) + ":" + f.Layer
	app.serveCard(w, r, placeCardJob(key, app.mapCard(r.Context(), f)))
}
