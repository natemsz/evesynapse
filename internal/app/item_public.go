package app

import (
	"context"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
	"evesynapse/internal/tradefees"
)

// ---------------------------------------------------------------------------
// The Item Database type page's public market sections: hub
// prices from already-warmed market data, server-rendered. No
// want queueing: crawlers must never schedule ESI work. Also the
// redirect from the /ships/{id}/ address.
// ---------------------------------------------------------------------------

// publicHubRegions are the market hubs shown on item pages, matched by region name so no region ID is hardcoded.
var publicHubRegions = []string{"The Forge", "Domain", "Sinq Laison", "Metropolis", "Heimatar"}

type publicHubPrice struct {
	Region   string
	BestSell string
	BestBuy  string

	// Spread is the hub's buy-to-sell margin, the gap as a share
	// of the net sell proceeds, after broker fees and sales tax
	// at perfect skills and relations ("" unless both sides
	// have an order). CheapestSell and TopBuy mark the hub to
	// buy from and the hub to sell to among the hubs listed
	// (only when two or more hubs quote that side).
	Spread       string
	CheapestSell bool
	TopBuy       bool

	sell, buy float64
}

// publicTypeMarket loads one type's swept region stats with
// region names resolved in the same read, and derives both the
// named hub rows and The Forge's region ID from it: one stats
// read per render, no per-region lookups.
func (app *Application) publicTypeMarket(ctx context.Context, typeID int64) (hubs []publicHubPrice, forgeRegion int64) {
	rows, err := app.queries.ListPublicHubStats(ctx, typeID)
	if err != nil {
		logging.Errorf("public item %d: hub stats: %v", typeID, err)
		return nil, 0
	}
	want := map[string]bool{}
	for _, name := range publicHubRegions {
		want[name] = true
	}
	for _, row := range rows {
		region := row.RegionName
		if region == "" {
			region = "Region #" + idString(row.RegionID)
		}
		if region == "The Forge" && forgeRegion == 0 {
			forgeRegion = row.RegionID
		}
		if !want[region] {
			continue
		}
		hub := publicHubPrice{Region: region, sell: row.BestSell, buy: row.BestBuy}
		if row.BestSell > 0 && row.BestBuy > 0 {
			hub.Spread = strconv.FormatFloat(tradefees.NetMarginPct(row.BestBuy, row.BestSell, tradefees.PerfectBrokerFee(), tradefees.PerfectSalesTax()), 'f', 1, 64) + "%"
		}
		if row.BestSell > 0 {
			hub.BestSell = esi.FormatISK(row.BestSell)
		}
		if row.BestBuy > 0 {
			hub.BestBuy = esi.FormatISK(row.BestBuy)
		}
		if hub.BestSell != "" || hub.BestBuy != "" {
			hubs = append(hubs, hub)
		}
	}
	if forgeRegion == 0 && len(rows) > 0 {
		forgeRegion = rows[0].RegionID
	}
	markBestHubs(hubs)
	return hubs, forgeRegion
}

// markBestHubs flags the cheapest sell and the highest buy among
// the listed hubs: where to buy the item and where to sell it.
// A side quoted by fewer than two hubs has nothing to compare, so
// it stays unmarked.
func markBestHubs(hubs []publicHubPrice) {
	sellAt, buyAt := -1, -1
	sells, buys := 0, 0
	for i, h := range hubs {
		if h.sell > 0 {
			sells++
			if sellAt < 0 || h.sell < hubs[sellAt].sell {
				sellAt = i
			}
		}
		if h.buy > 0 {
			buys++
			if buyAt < 0 || h.buy > hubs[buyAt].buy {
				buyAt = i
			}
		}
	}
	if sells > 1 {
		hubs[sellAt].CheapestSell = true
	}
	if buys > 1 {
		hubs[buyAt].TopBuy = true
	}
}

// handleShipRedirect sends /ships/{id}/, an address search engines
// still hold, to the type's item page.
func (app *Application) handleShipRedirect(w http.ResponseWriter, r *http.Request) {
	typeID, err := strconv.ParseInt(chi.URLParam(r, "typeID"), 10, 64)
	if err != nil || typeID <= 0 {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/items/type/"+strconv.FormatInt(typeID, 10)+"/", http.StatusMovedPermanently)
}
