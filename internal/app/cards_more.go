package app

// Summary cards for the rest of the shareable pages (cards.go has the
// generator): a public fit, an item, an item's market, a corporation,
// an alliance, a shared battle report and the sovereignty tracker.
// Each card shows figures its page shows, read from the same stored
// rows. Intel channels and their reports never get a card: what a
// preview fetcher could draw from them is intel.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	json "github.com/goccy/go-json"

	"evesynapse/internal/app/contracts"
	"evesynapse/internal/card"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/fitview"
	"evesynapse/internal/markethistory"
	"evesynapse/internal/tradefees"
)

// cardID parses a positive id from a route parameter or query value;
// anything else is 0, false.
func cardID(s string) (int64, bool) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// --- public fit ------------------------------------------------------------

// fitCardJob reads a public fit's card: the hull render, the fit's
// name, hull and author, the stored simulation's damage and
// effective hit points, the price the page shows, and its rating.
func (app *Application) fitCardJob(ctx context.Context, id int64) (cardJob, bool) {
	row, err := app.queries.GetPublicFitting(ctx, id)
	if err != nil {
		return cardJob{}, false
	}
	ship := app.TypeNameOrID(ctx, row.ShipTypeID)
	c := card.Card{Kind: "FIT", Title: row.Name, Subtitle: ship}
	if row.AuthorName != "" {
		c.Subtitle += " · by " + row.AuthorName
	}
	if stored, ok := app.loadPublicFitStored(ctx, id, row.UpdatedAt); ok && stored.Sim != nil && stored.Sim.Stats != nil {
		// A fit with no weapons has no damage worth a figure.
		if s := stored.Sim.Stats.TotalDPS; s != "" && strings.Trim(s, "0.") != "" {
			c.Stats = append(c.Stats, card.Stat{Label: "DPS", Value: card.Figure(s)})
		}
		if s := stored.Sim.Stats.EHPOmni; s != "" {
			c.Stats = append(c.Stats, card.Stat{Label: "EHP", Value: card.Figure(s)})
		}
	}
	var doc fitview.FitDoc
	if json.Unmarshal([]byte(row.ItemsJson), &doc) == nil {
		if doc.ShipTypeID == 0 {
			doc.ShipTypeID = row.ShipTypeID
		}
		if cost := app.fitBuilder().FitCost(ctx, &doc, nil); cost.Known {
			price := formatISKShort(cost.TotalRaw)
			if cost.BuildPriced {
				price += "*"
			}
			c.Stats = append(c.Stats, card.Stat{Label: "PRICE", Value: card.Figure(price)})
		}
	}
	if row.RatingCount > 0 {
		c.Detail = &card.Detail{Label: "RATING", Text: fmt.Sprintf("%.1f of 5 from %s", float64(row.RatingSum)/float64(row.RatingCount),
			pluralCount(row.RatingCount, "rating", "ratings"))}
	}
	pic := fmt.Sprintf("https://images.evetech.net/types/%d/render?size=512", row.ShipTypeID)
	return cardJob{key: "fit:" + strconv.FormatInt(id, 10), version: cardVersion(row.UpdatedAt.UTC().Format(time.RFC3339Nano), pic, c),
		picture: pic, card: c, maxAge: time.Hour}, true
}

func (app *Application) handleFitCard(w http.ResponseWriter, r *http.Request) {
	id, ok := cardID(chi.URLParam(r, "fitID"))
	if !ok {
		app.serveFallbackCard(w)
		return
	}
	job, ok := app.fitCardJob(r.Context(), id)
	if !ok {
		app.serveFallbackCard(w)
		return
	}
	app.serveCard(w, r, job)
}

// --- item and market ---------------------------------------------------------

// itemArt is a type's card picture: its render, with its icon to
// try when the image server has no render, or its category's icon.
func itemArt(typeID, categoryID int64) (pic, alt string) {
	pic, alt = contracts.TypeArtURLs(typeID, categoryID, 512)
	if alt != "" {
		_, alt = contracts.TypeArtURLs(typeID, categoryID, 128)
	} else {
		pic, _ = contracts.TypeArtURLs(typeID, categoryID, 128)
	}
	return pic, alt
}

// itemCardJob reads an item's card: its art, name, group and
// category, and the swept hub prices the page lists (the lowest sell
// and highest buy across the hubs); a type the market cannot price
// shows the worker's stored value and build cost instead.
func (app *Application) itemCardJob(ctx context.Context, typeID int64) (cardJob, bool) {
	t, err := app.queries.GetSDEType(ctx, typeID)
	if err != nil {
		return cardJob{}, false
	}
	d := app.buildItemTypeDetail(ctx, t)
	c := card.Card{Kind: "ITEM", Title: d.Name}
	var catID int64
	var path []string
	if d.Group != nil {
		path = append(path, d.Group.Name)
	}
	if d.Category != nil {
		catID = d.Category.ID
		path = append(path, d.Category.Name)
	}
	c.Subtitle = strings.Join(path, " · ")
	if d.OnMarket {
		hubs, _ := app.publicTypeMarket(ctx, typeID)
		var sellAt, buyAt *publicHubPrice
		for i := range hubs {
			h := &hubs[i]
			if h.sell > 0 && (sellAt == nil || h.sell < sellAt.sell) {
				sellAt = h
			}
			if h.buy > 0 && (buyAt == nil || h.buy > buyAt.buy) {
				buyAt = h
			}
		}
		var where []string
		if sellAt != nil {
			c.Stats = append(c.Stats, card.Stat{Label: "LOWEST SELL", Value: card.Figure(formatISKShort(sellAt.sell))})
			where = append(where, "lowest sell in "+sellAt.Region)
		}
		if buyAt != nil {
			c.Stats = append(c.Stats, card.Stat{Label: "HIGHEST BUY", Value: card.Figure(formatISKShort(buyAt.buy))})
			where = append(where, "highest buy in "+buyAt.Region)
		}
		if len(where) > 0 {
			text := strings.Join(where, ", ")
			c.Detail = &card.Detail{Label: "HUBS", Text: strings.ToUpper(text[:1]) + text[1:]}
		}
	} else {
		if d.Value != "" {
			c.Stats = append(c.Stats, card.Stat{Label: "VALUE", Value: card.Figure(d.Value)})
		}
		if d.BuildCost != "" {
			c.Stats = append(c.Stats, card.Stat{Label: "BUILD COST", Value: card.Figure(d.BuildCost)})
		}
	}
	pic, alt := itemArt(typeID, catID)
	return cardJob{key: "item:" + strconv.FormatInt(typeID, 10), version: cardVersion(alt, pic, c),
		picture: pic, pictureAlt: alt, card: c, maxAge: time.Hour}, true
}

func (app *Application) handleItemCard(w http.ResponseWriter, r *http.Request) {
	id, ok := cardID(chi.URLParam(r, "typeID"))
	if !ok {
		app.serveFallbackCard(w)
		return
	}
	job, ok := app.itemCardJob(r.Context(), id)
	if !ok {
		app.serveFallbackCard(w)
		return
	}
	app.serveCard(w, r, job)
}

// marketCardPath is the market item view's card address.
func marketCardPath(typeID, regionID int64) string {
	return "/market/card.png?type=" + strconv.FormatInt(typeID, 10) + "&region=" + strconv.FormatInt(regionID, 10)
}

// marketSparkMinDays is how many stored history days a market
// card needs before its price line is worth drawing.
const marketSparkMinDays = 7

// marketCardJob reads a market item view's card: the item's art and
// name, the region, and the region's swept best sell, best buy and
// spread (the same stats row the item page lists per hub).
func (app *Application) marketCardJob(ctx context.Context, typeID, regionID int64) (cardJob, bool) {
	t, err := app.queries.GetSDEType(ctx, typeID)
	if err != nil {
		return cardJob{}, false
	}
	regionName, ok := marketRegionName(regionID)
	if !ok {
		return cardJob{}, false
	}
	c := card.Card{Kind: "MARKET", Title: t.Name, Subtitle: regionName}
	if rows, err := app.queries.ListPublicHubStats(ctx, typeID); err == nil {
		for _, row := range rows {
			if row.RegionID != regionID {
				continue
			}
			if row.BestSell > 0 {
				c.Stats = append(c.Stats, card.Stat{Label: "SELL", Value: card.Figure(formatISKShort(row.BestSell))})
			}
			if row.BestBuy > 0 {
				c.Stats = append(c.Stats, card.Stat{Label: "BUY", Value: card.Figure(formatISKShort(row.BestBuy))})
			}
			if row.BestSell > 0 && row.BestBuy > 0 {
				spread := tradefees.NetMarginPct(row.BestBuy, row.BestSell, tradefees.PerfectBrokerFee(), tradefees.PerfectSalesTax())
				c.Stats = append(c.Stats, card.Stat{Label: "SPREAD", Value: card.Figure(strconv.FormatFloat(spread, 'f', 1, 64) + "%")})
			}
			break
		}
	}
	// The price history line: the stored daily averages for this
	// region, the same rows the item page charts. Too few days
	// draws no line rather than a stub.
	if rows := app.recentHistoryRows(ctx, regionID, typeID, markethistory.ChartRows); len(rows) >= marketSparkMinDays {
		vals := make([]float64, 0, len(rows))
		for _, row := range rows {
			if row.Average > 0 {
				vals = append(vals, row.Average)
			}
		}
		if len(vals) >= marketSparkMinDays {
			c.Spark = &card.Spark{Label: "PRICE HISTORY", Values: vals}
		}
	}
	var catID int64
	if grp, err := app.queries.GetSDEGroup(ctx, t.GroupID); err == nil {
		catID = grp.CategoryID
	}
	pic, alt := itemArt(typeID, catID)
	key := "market:" + strconv.FormatInt(typeID, 10) + ":" + strconv.FormatInt(regionID, 10)
	return cardJob{key: key, version: cardVersion(alt, pic, c), picture: pic, pictureAlt: alt, card: c, maxAge: time.Hour}, true
}

func (app *Application) handleMarketCard(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	typeID, ok := cardID(q.Get("type"))
	regionID, rok := cardID(q.Get("region"))
	if !ok || !rok {
		app.serveFallbackCard(w)
		return
	}
	job, ok := app.marketCardJob(r.Context(), typeID, regionID)
	if !ok {
		app.serveFallbackCard(w)
		return
	}
	app.serveCard(w, r, job)
}

// --- corporation and alliance ------------------------------------------------

// corporationCardJob reads a corporation's card from its stored
// public record: logo, name and ticker, alliance, members, founding
// date, tax rate and CEO, as its page shows them.
func (app *Application) corporationCardJob(ctx context.Context, id int64) (cardJob, bool) {
	rec, err := app.queries.GetCorporationRecord(ctx, id)
	if err != nil || rec.State != orgStateReady || rec.Payload == "" {
		return cardJob{}, false
	}
	v, ok := app.buildCorporationView(ctx, id, rec.Payload)
	if !ok || v.Name == "" {
		return cardJob{}, false
	}
	c := card.Card{Kind: "CORPORATION", Title: v.Name}
	sub := []string{}
	if v.Ticker != "" {
		sub = append(sub, "["+v.Ticker+"]")
	}
	if v.AllianceLine != "" {
		sub = append(sub, v.AllianceLine)
	}
	c.Subtitle = strings.Join(sub, " · ")
	if v.MemberCount != "" {
		c.Stats = append(c.Stats, card.Stat{Label: "MEMBERS", Value: card.Figure(v.MemberCount)})
	}
	if v.Founded != "" {
		c.Stats = append(c.Stats, card.Stat{Label: "FOUNDED", Value: card.Figure(v.Founded)})
	}
	if v.TaxRate != "" {
		c.Stats = append(c.Stats, card.Stat{Label: "TAX", Value: card.Figure(v.TaxRate)})
	}
	if v.CEOName != "" && !v.CEOPending {
		c.Detail = &card.Detail{Label: "CEO", Text: v.CEOName}
	}
	pic := fmt.Sprintf("https://images.evetech.net/corporations/%d/logo?size=256", id)
	data := rec.FetchedAt.Time.UTC().Format(time.RFC3339Nano)
	return cardJob{key: "corporation:" + strconv.FormatInt(id, 10), version: cardVersion(data, pic, c),
		picture: pic, card: c, maxAge: time.Hour}, true
}

// allianceCardJob reads an alliance's card from its stored public
// record and the sovereignty it holds: logo, name and ticker, member
// corporations, systems held, founding date and executor.
func (app *Application) allianceCardJob(ctx context.Context, id int64) (cardJob, bool) {
	rec, err := app.queries.GetAllianceRecord(ctx, id)
	if err != nil || rec.State != orgStateReady || rec.Payload == "" {
		return cardJob{}, false
	}
	v, ok := app.buildAllianceView(ctx, id, rec.Payload)
	if !ok || v.Name == "" {
		return cardJob{}, false
	}
	c := card.Card{Kind: "ALLIANCE", Title: v.Name}
	if v.Ticker != "" {
		c.Subtitle = "<" + v.Ticker + ">"
	}
	if v.MemberCount > 0 {
		c.Stats = append(c.Stats, card.Stat{Label: "CORPORATIONS", Value: card.Figure(esi.FormatInt(int64(v.MemberCount)))})
	}
	if sov := app.loadAllianceSov(ctx, id, time.Now().UTC()); sov != nil && sov.Held != "" {
		c.Stats = append(c.Stats, card.Stat{Label: "SYSTEMS HELD", Value: card.Figure(sov.Held)})
	}
	if v.Founded != "" {
		c.Stats = append(c.Stats, card.Stat{Label: "FOUNDED", Value: card.Figure(v.Founded)})
	}
	if v.ExecutorCorp != "" && !v.ExecutorPending {
		c.Detail = &card.Detail{Label: "EXECUTOR", Text: v.ExecutorCorp}
	}
	pic := fmt.Sprintf("https://images.evetech.net/alliances/%d/logo?size=128", id)
	data := rec.FetchedAt.Time.UTC().Format(time.RFC3339Nano)
	return cardJob{key: "alliance:" + strconv.FormatInt(id, 10), version: cardVersion(data, pic, c),
		picture: pic, card: c, maxAge: time.Hour}, true
}

func (app *Application) handleCorporationCard(w http.ResponseWriter, r *http.Request) {
	id, ok := cardID(r.URL.Query().Get("corporation"))
	if !ok {
		app.serveFallbackCard(w)
		return
	}
	job, ok := app.corporationCardJob(r.Context(), id)
	if !ok {
		app.serveFallbackCard(w)
		return
	}
	app.serveCard(w, r, job)
}

func (app *Application) handleAllianceCard(w http.ResponseWriter, r *http.Request) {
	id, ok := cardID(r.URL.Query().Get("alliance"))
	if !ok {
		app.serveFallbackCard(w)
		return
	}
	job, ok := app.allianceCardJob(r.Context(), id)
	if !ok {
		app.serveFallbackCard(w)
		return
	}
	app.serveCard(w, r, job)
}

// --- shared battle report ------------------------------------------------------

// battleCardJob reads a shared battle report's card from its stored
// fight: the hull render, where and when, the kills, losses and ISK
// both ways, and the pilot the log is from, as the shared page shows.
func (app *Application) battleCardJob(ctx context.Context, f db.BattleFight) cardJob {
	title := "Battle report"
	if name := app.battleSystemName(ctx, f.SystemID); name != "" {
		title = "Battle in " + name
	}
	c := card.Card{Kind: "BATTLE REPORT", Title: title,
		Subtitle: f.StartedAt.UTC().Format("2006-01-02 15:04") + " EVE · " + battleLength(f) + " long"}
	c.Stats = []card.Stat{
		{Label: "KILLS", Value: card.Figure(esi.FormatInt(int64(f.Kills)))},
		{Label: "LOSSES", Value: card.Figure(esi.FormatInt(int64(f.Losses)))},
	}
	if f.IskDestroyed > 0 {
		c.Stats = append(c.Stats, card.Stat{Label: "ISK DESTROYED", Value: card.Figure(formatISKShort(f.IskDestroyed))})
	}
	if f.IskLost > 0 {
		c.Stats = append(c.Stats, card.Stat{Label: "ISK LOST", Value: card.Figure(formatISKShort(f.IskLost))})
	}
	pilot := f.Listener
	if f.HullTypeID > 0 {
		pilot = joinNonEmpty(" · ", pilot, app.TypeNameOrID(ctx, f.HullTypeID))
	}
	if pilot != "" {
		c.Detail = &card.Detail{Label: "PILOT", Text: pilot}
	}
	var pic string
	if f.HullTypeID > 0 {
		pic = fmt.Sprintf("https://images.evetech.net/types/%d/render?size=512", f.HullTypeID)
	} else {
		c.Mark = true
	}
	return cardJob{key: "battle:" + f.ShareToken, version: cardVersion("", pic, c), picture: pic, card: c, maxAge: 24 * time.Hour}
}

// handleBattleCard serves /intel/battle/s/{token}/card.png while the
// report's share link is on.
func (app *Application) handleBattleCard(w http.ResponseWriter, r *http.Request) {
	f, err := app.queries.GetBattleFightByToken(r.Context(), chi.URLParam(r, "token"))
	if err != nil {
		app.serveFallbackCard(w)
		return
	}
	app.serveCard(w, r, app.battleCardJob(r.Context(), f))
}

// --- sovereignty tracker -------------------------------------------------------

// sovCardPath is the tracker card's address for the page's filters.
func sovCardPath(regionID, allianceID int64) string {
	q := url.Values{}
	if regionID > 0 {
		q.Set("region", strconv.FormatInt(regionID, 10))
	}
	if allianceID > 0 {
		q.Set("alliance", strconv.FormatInt(allianceID, 10))
	}
	if len(q) == 0 {
		return "/intel/sov/card.png"
	}
	return "/intel/sov/card.png?" + q.Encode()
}

// sovCardJob reads the tracker's card for its filters: the summary
// tiles the page opens with (systems held, changed hands in a day,
// live contests, vulnerable now), titled by the alliance or region
// in view.
func (app *Application) sovCardJob(ctx context.Context, regionID, allianceID int64) cardJob {
	now := time.Now().UTC()
	c := card.Card{Kind: "SOVEREIGNTY", Title: "Sovereignty", Subtitle: "Who holds null-sec", Mark: true}
	var pic string
	switch {
	case allianceID > 0:
		c.Title = app.allianceDisplayName(ctx, allianceID)
		c.Subtitle = "Sovereignty held"
		c.Mark = false
		pic = fmt.Sprintf("https://images.evetech.net/alliances/%d/logo?size=128", allianceID)
	case regionID > 0:
		if region, err := app.queries.GetSDERegion(ctx, regionID); err == nil {
			c.Title = region.Name
			c.Subtitle = "Sovereignty in the region"
		}
	}
	sum, err := app.queries.SovSummary(ctx, db.SovSummaryParams{
		SinceDay: now.Add(-24 * time.Hour), SinceWeek: now.Add(-7 * 24 * time.Hour), Now: now, Until: now.Add(sovWindowAhead),
		RegionID: regionID, AllianceID: allianceID,
	})
	if err == nil {
		c.Stats = []card.Stat{
			{Label: "SYSTEMS", Value: card.Figure(thousands(sum.Held))},
			{Label: "CHANGED 24H", Value: card.Figure(thousands(sum.ClaimedDay))},
			{Label: "CONTESTS", Value: card.Figure(thousands(sum.Contests))},
			{Label: "VULNERABLE", Value: card.Figure(thousands(sum.VulnerableNow))},
		}
	}
	key := "sov:" + strconv.FormatInt(regionID, 10) + ":" + strconv.FormatInt(allianceID, 10)
	return cardJob{key: key, version: cardVersion("", pic, c), picture: pic, card: c, maxAge: 15 * time.Minute}
}

func (app *Application) handleSovCard(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	regionID, _ := cardID(q.Get("region"))
	allianceID, _ := cardID(q.Get("alliance"))
	app.serveCard(w, r, app.sovCardJob(r.Context(), regionID, allianceID))
}
