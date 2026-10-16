package app

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"net/http"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// The anonymous landing page: a standalone shell (its own slim
// header, no sidebar) rendered from templates/landing.html. It is
// the site's public face: hero, a Fleet Overview preview rendered
// from a static showcase dataset (sample figures, captioned as
// such; no account data ever renders here), and the "New Eden,
// right now" strip, which reads stored rows only: the workers
// precompute, the landing never waits on ESI and never computes
// beyond shaping three small results.

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
}

// landingShowcase is the preview panel's static dataset: the
// approved sample figures, verbatim, shaped like the real home
// KPI band so the illustration cannot drift from the product.
type landingShowcaseData struct {
	NetWorth, NetWorthSub          string
	Liquid, LiquidSub              string
	Hangar, HangarSub              string
	Training, TrainingSub          string
	SparkPoints, SparkArea         string
	LiquidPct, HangarPct, TrainPct string
}

var landingShowcase = landingShowcaseData{
	NetWorth: "140,699,221,099", NetWorthSub: "▲ 2.4% this week",
	Liquid: "67,713,672,586", LiquidSub: "48.1% of net worth",
	Hangar: "72,985,548,513", HangarSub: "93.5% priced",
	Training: "3 of 8 training", TrainingSub: "5 queues idle, flagged below",
	SparkPoints: "0,22 13,21 25,21 38,17 50,18 63,12 75,13 88,7 100,6",
	SparkArea:   "0,22 13,21 25,21 38,17 50,18 63,12 75,13 88,7 100,6 100,28 0,28",
	LiquidPct:   "48", HangarPct: "93", TrainPct: "37",
}

type landingData struct {
	module.ShellData
	Error         string
	SSOConfigured bool
	Showcase      landingShowcaseData
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

// buildLanding assembles the strip from stored rows. Any source
// that errors or is empty simply leaves its card to the template's
// evergreen fallback; the page itself never fails on data.
func (app *Application) buildLanding(ctx context.Context) landingData {
	data := landingData{SSOConfigured: app.cfg.SSOConfigured(), Showcase: landingShowcase}

	rows, err := app.db.QueryContext(ctx, `
		WITH latest AS (
		  SELECT MAX(date) AS d FROM market_history WHERE region_id = 10000002
		), prev AS (
		  SELECT MAX(date) AS d FROM market_history WHERE region_id = 10000002 AND date < (SELECT d FROM latest)
		)
		SELECT COALESCE(tn.name, '') AS type_name, cur.average AS price,
		       ((cur.average - old.average) / old.average * 100) AS pct_change
		FROM market_history cur
		JOIN market_history old ON old.region_id = cur.region_id AND old.type_id = cur.type_id AND old.date = (SELECT d FROM prev)
		LEFT JOIN type_names tn ON tn.type_id = cur.type_id
		WHERE cur.region_id = 10000002 AND cur.date = (SELECT d FROM latest)
		  AND old.average > 0 AND cur.volume > 0
		ORDER BY ABS((cur.average - old.average) / old.average) DESC
		LIMIT 3`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var name string
			var price, pct float64
			if err := rows.Scan(&name, &price, &pct); err != nil {
				continue
			}
			data.Movers = append(data.Movers, landingMover{
				Name:  name,
				Price: formatISKShort(price),
				Pct:   fmt.Sprintf("%.1f%%", math.Abs(pct)),
				Up:    pct >= 0,
			})
		}
	}

	if fits, err := app.queries.BrowsePublicFittings(ctx, db.BrowsePublicFittingsParams{
		Q: "", Tag: "", RowLimit: 1,
	}); err == nil && len(fits) > 0 {
		f := fits[0]
		data.Fit = &landingFit{
			ShipName: f.ShipName,
			FitName:  f.Name,
			Author:   f.AuthorName,
			URL:      fmt.Sprintf("%sfit/%d/", module.PublicFitsPath, f.ID),
		}
	}

	if status, ok := app.loadServerStatus(ctx); ok {
		data.ServerStatus = status
	}

	data.HasStrip = len(data.Movers) > 0 || data.Fit != nil || data.ServerStatus != nil
	return data
}

func (app *Application) renderLanding(ctx context.Context, w http.ResponseWriter, data landingData) {
	app.fillShell(ctx, &data.ShellData, "landing.html")
	ts, err := parsedTemplate(&pageTemplates, "landing", "landing.html")
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
