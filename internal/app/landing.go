package app

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"net/http"
	"strings"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// The anonymous landing page: a standalone shell (its own slim
// header, no sidebar) rendered from templates/landing.html. It is
// the site's public face: a hero with the three doors (sign up,
// the demo, the public tools), the tool inventory, and the "New
// Eden, right now" strip, which reads stored rows only: the
// workers precompute, the landing never waits on ESI and never
// computes beyond shaping small stored results.

type landingMover struct {
	Name  string
	Price string
	Pct   string
	Up    bool
}

type landingFit struct {
	ShipName string
	FitName  string
	Author   string
	URL      string
	// Price is the fit's stored total_price, formatted with
	// " ISK" appended; empty when the worker has not priced the
	// fit, in which case the template makes no pricing claim.
	Price string
	// IsNewest marks the showcased fit as the most recently
	// updated public fit, so the strip can call it that; the
	// showcase prefers a priced fit over a merely newer one.
	IsNewest bool
}

type landingData struct {
	module.ShellData
	Error         string
	SSOConfigured bool
	Movers        []landingMover
	Fit           *landingFit
	ServerStatus  *serverStatusView
	HasStrip      bool
}

func formatISKShort(v float64) string {
	switch {
	case v >= 1_000_000_000:
		return fmt.Sprintf("%.2fB", v/1_000_000_000)
	case v >= 1_000_000:
		return fmt.Sprintf("%.2fM", v/1_000_000)
	case v >= 10_000:
		return fmt.Sprintf("%.1fK", v/1_000)
	default:
		return fmt.Sprintf("%.2f", v)
	}
}

// landingWatched is the strip's watched set — name, market
// region, type ID — in display order. ONE list drives both the
// pricing query and the history-want queue in buildLanding, so
// the two can never drift apart. They once did: the query
// rebuilt its set from type_names, a sparse name cache that
// fills lazily (search hits, character warming, the swarm),
// while the queue used these triples. History flowed for all
// three items and the strip still starved, because the cache
// simply had no rows for them (2026-10-16, after two fixes
// whose tests seeded the cache and so could not see it).
type landingWatchedItem struct {
	name     string
	regionID int64
	typeID   int64
}

var landingWatched = []landingWatchedItem{
	{"PLEX", globalMarketRegion, 44992},
	{"Large Skill Injector", defaultMarketRegion, 40520},
	{"Small Skill Injector", defaultMarketRegion, 45635},
}

// buildLanding assembles the strip from stored rows. Any source
// that errors or is empty simply leaves its card to the template's
// evergreen fallback; the page itself never fails on data.
func (app *Application) buildLanding(ctx context.Context) landingData {
	data := landingData{SSOConfigured: app.cfg.SSOConfigured()}

	// The strip shows three watched items, always the same: PLEX
	// and the two skill injectors, the prices every pilot checks.
	// Each item is priced from its OWN two most recent stored
	// days, in its own market: the injectors in The Forge, PLEX
	// on the global market (region 19000001, where it has traded
	// since 2021; it has no Forge history at all). Per-item days
	// matter: history lands on each item's own fetch schedule,
	// so keying the joins to the region-wide latest dates would
	// silently drop any item whose stored history runs a day
	// behind the freshest type in its region. An item with fewer
	// than two stored days drops out — and queues a history want
	// below so the worker fetches it for a later visit.
	//
	// The watched set is the hardcoded landingWatched list,
	// never a table lookup: display names resolve from sde_types
	// (the complete local catalog) by type ID, with the list's
	// own name as fallback. type_names is deliberately NOT used
	// — it is a sparse, lazily-warmed cache, not the catalog.
	var watchedRows []string
	var watchedArgs []any
	for i, w := range landingWatched {
		base := i * 3
		watchedRows = append(watchedRows, fmt.Sprintf(
			"($%d::bigint, $%d::bigint, $%d::text)", base+1, base+2, base+3))
		watchedArgs = append(watchedArgs, w.typeID, w.regionID, w.name)
	}
	rows, err := app.db.QueryContext(ctx, `
		WITH watched AS (
		  SELECT v.type_id, v.region_id, COALESCE(s.name, v.name) AS name
		  FROM (VALUES `+strings.Join(watchedRows, ", ")+`) AS v(type_id, region_id, name)
		  LEFT JOIN sde_types s ON s.type_id = v.type_id
		), ranked AS (
		  SELECT w.type_id, w.name, h.average,
		         ROW_NUMBER() OVER (PARTITION BY w.type_id ORDER BY h.date DESC) AS rn
		  FROM watched w
		  JOIN market_history h ON h.type_id = w.type_id AND h.region_id = w.region_id
		)
		SELECT cur.type_id, cur.name, cur.average,
		       ((cur.average - old.average) / old.average * 100) AS pct_change
		FROM ranked cur
		JOIN ranked old ON old.type_id = cur.type_id AND old.rn = 2
		WHERE cur.rn = 1 AND old.average > 0`, watchedArgs...)
	priced := map[int64]landingMover{}
	if err != nil {
		// The page never fails on data, but a broken strip query
		// must not be invisible either: the 2026-10-16 empty
		// strip hid behind this silence through two "fixes".
		logging.Errorf("landing: strip pricing query: %v", err)
	} else {
		defer rows.Close()
		for rows.Next() {
			var typeID int64
			var name string
			var price, pct float64
			if err := rows.Scan(&typeID, &name, &price, &pct); err != nil {
				logging.Errorf("landing: strip pricing scan: %v", err)
				continue
			}
			priced[typeID] = landingMover{
				Name:  name,
				Price: formatISKShort(price),
				Pct:   fmt.Sprintf("%.1f%%", math.Abs(pct)),
				Up:    pct >= 0,
			}
		}
		if err := rows.Err(); err != nil {
			logging.Errorf("landing: strip pricing rows: %v", err)
		}
		// Display order is the watched list's, not the query's.
		for _, w := range landingWatched {
			if m, ok := priced[w.typeID]; ok {
				data.Movers = append(data.Movers, m)
			}
		}
	}

	// Queue a history want for any watched item the strip could
	// not price, so its market history starts (or keeps) flowing.
	for _, w := range landingWatched {
		if _, ok := priced[w.typeID]; !ok {
			app.QueueHistoryWant(ctx, w.regionID, w.typeID)
		}
	}

	// The showcased fit shows its stored price, so the pick is
	// the newest public fit the worker has priced (total_price
	// is maintained by the reprice pass, never computed here).
	// A short window of the newest fits is browsed; the first
	// priced one wins. When none of them is priced yet, the
	// newest fit still shows, just without a price or the claim.
	if fits, err := app.queries.BrowsePublicFittings(ctx, db.BrowsePublicFittingsParams{
		Q: "", Tag: "", RowLimit: 8,
	}); err == nil && len(fits) > 0 {
		pick := 0
		for i, f := range fits {
			if f.TotalPrice > 0 {
				pick = i
				break
			}
		}
		f := fits[pick]
		fit := &landingFit{
			ShipName: f.ShipName,
			FitName:  f.Name,
			Author:   f.AuthorName,
			URL:      fmt.Sprintf("%sfit/%d/", module.PublicFitsPath, f.ID),
			IsNewest: pick == 0,
		}
		if f.TotalPrice > 0 {
			fit.Price = esi.FormatISK(f.TotalPrice) + " ISK"
		}
		data.Fit = fit
	}

	if status, ok := app.loadServerStatus(ctx); ok {
		data.ServerStatus = status
	}

	data.HasStrip = len(data.Movers) > 0 || data.Fit != nil || data.ServerStatus != nil
	return data
}

func (app *Application) renderLanding(ctx context.Context, w http.ResponseWriter, data landingData) {
	app.fillShell(ctx, &data.ShellData, "landing.html")
	ts, err := parsedTemplate(&pageTemplates, "landing", "landing.html", "templates/wordmark.html")
	if err != nil {
		logging.Errorf("parse template landing.html: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	buf := new(bytes.Buffer)
	if err := ts.ExecuteTemplate(buf, "landing", data); err != nil {
		logging.Errorf("execute template landing.html: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = buf.WriteTo(w)
}
