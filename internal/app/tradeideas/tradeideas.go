// Package tradeideas is the Trade Ideas module: a model's read on the
// Forge spread scanner, on its own page under Market. The worker asks
// the configured model (Meta's Muse Wonder with MODEL_API_KEY, else
// Gemma on Cloudflare Workers AI) about the top scanner rows every so
// often and stores the answer; the page renders the stored answer only,
// so the model is never on the render path. The external model calls
// belong to this module: a build without it never makes one.
//
// Cost bounds: the prompt carries at most tradeIdeasCSVRows scanner
// rows, refreshes run at most every tradeIdeasRefreshAge (and only when
// the Forge sweep has data), a failing refresh backs off from 30 minutes
// (trade_ideas_attempts), the call runs beside the worker cycle rather
// than in it, the answer is truncated to tradeIdeasBodyCap, and the
// token travels in the Authorization header alone: never a URL, never a
// log line.
package tradeideas

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"fmt"
	"math"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/ideas"
	"evesynapse/internal/logging"
)

// Module is the Trade Ideas module's registration.
var Module = module.Module{
	ID:            "tradeideas",
	Title:         "Trade Ideas",
	Requires:      []string{},
	Scopes:        []module.Scope{},
	EnhanceScopes: []module.Scope{},
	Activities:    []string{"market"},
	Widgets:       []module.WidgetDecl{{ID: "tradeideas", Title: "Trade Ideas", Description: "A model's read on the Forge spread scanner, one tab per trade strategy. Refreshed twice a day; the Trade Ideas page has the full list.", W: 2, H: 1, After: "watchlist"}},
	Nav: []module.NavSection{{
		Section: "market",
		Entries: []module.NavEntry{
			{Href: "/market/ideas/", Label: "Trade Ideas", Pages: []string{"market_tradeideas.html"}, After: "/market/scanner/"},
		},
	}},
	Pages: []module.Page{
		{Template: "market_tradeideas.html", Section: "market"},
	},
	WorkerJobs: []module.WorkerJob{{Name: "trade ideas refresh", After: module.AfterMarket, Start: Start}},
	Routes: func(r chi.Router, deps module.Deps) {
		r.With(deps.AnonPages()).Get("/market/ideas/", handlePage(deps))
	},
}

// PageData is the view model of market_tradeideas.html.
type PageData struct {
	module.ShellData
	TradeIdeas *ideas.Widget
}

// Shell returns the embedded shell state for the core renderer.
func (p *PageData) Shell() *module.ShellData { return &p.ShellData }

// handlePage renders the trade ideas page (/market/ideas/): the page's
// own stored answer only, no model call, one tab per strategy.
func handlePage(deps module.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		data := PageData{ShellData: deps.PublicShell(ctx)} // public: stored market data only
		data.TradeIdeas = ideas.StoredPage(ctx, deps.DB(), deps.ModelConfig().Configured())
		deps.Render(ctx, w, http.StatusOK, "market_tradeideas.html", &data)
	}
}

// refreshBusy is set while a refresh runs beside the worker: one at a
// time.
var refreshBusy atomic.Bool

// Start runs Refresh beside the worker cycle, never inside it: the model
// call can take up to tradeIdeasTimeout, and a cycle must not wait on it.
// One refresh at a time; shutdown waits for one in flight (Background).
func Start(ctx context.Context, deps module.Deps) {
	if !deps.ModelConfig().Configured() || !refreshBusy.CompareAndSwap(false, true) {
		return
	}
	deps.Background(func() {
		defer refreshBusy.Store(false)
		Refresh(ctx, deps)
	})
}

// Refresh asks the model about the Forge scanner rows and stores the
// answer, when one is due (tradeIdeasDue). It reports how many answers
// landed. Anything failing (unconfigured, no sweep yet, the model down)
// keeps the previous answer; a failed ask is recorded and backs off.
func Refresh(ctx context.Context, deps module.Deps) (stored int) {
	cfg := deps.ModelConfig()
	if !cfg.Configured() {
		return 0
	}
	q := deps.DB()
	now := time.Now().UTC()
	// One model answers both: Muse Wonder when its key is set, else
	// Gemma. An answer another model gave counts as never answered,
	// so switching models (setting or clearing MODEL_API_KEY) asks
	// the new one on the next cycle instead of up to 12 hours later.
	model := tradeIdeasModel
	if cfg.UsesMeta() {
		model = metaAIModel
	}
	var moduleAt, pageAt, attemptedAt time.Time
	failures := 0
	if row, err := q.GetTradeIdeas(ctx); err == nil && row.Model == model {
		moduleAt = row.AsOf
	}
	if row, err := q.GetTradeIdeasPage(ctx); err == nil && row.Model == model {
		pageAt = row.AsOf
	}
	if a, err := q.GetTradeIdeasAttempt(ctx); err == nil {
		attemptedAt, failures = a.AttemptedAt, int(a.Failures)
	}
	// The home module's answer and the page's share the cadence and the
	// backoff; each is asked for only when it is due itself.
	asks := []struct {
		want      bool
		name      string
		rows      int
		prompt    string
		bodyCap   int
		maxTokens int
		put       func(body string, rows []ideas.Row) error
	}{
		{tradeIdeasDue(now, moduleAt, attemptedAt, failures), "module", tradeIdeasCSVRows, tradeIdeasPrompt, tradeIdeasBodyCap, tradeIdeasModuleMaxTokens,
			func(body string, rows []ideas.Row) error {
				raw, err := ideas.EncodeRows(rows)
				if err != nil {
					return err
				}
				return q.PutTradeIdeas(ctx, db.PutTradeIdeasParams{Body: body, Rows: sql.NullString{String: raw, Valid: true}, AsOf: now, Model: model})
			}},
		{tradeIdeasDue(now, pageAt, attemptedAt, failures), "page", tradeIdeasPageCSVRows, tradeIdeasPagePrompt, tradeIdeasPageBodyCap, tradeIdeasPageMaxTokens,
			func(body string, rows []ideas.Row) error {
				raw, err := ideas.EncodeRows(rows)
				if err != nil {
					return err
				}
				return q.PutTradeIdeasPage(ctx, db.PutTradeIdeasPageParams{Body: body, Rows: sql.NullString{String: raw, Valid: true}, AsOf: now, Model: model})
			}},
	}
	asked := false
	for _, ask := range asks {
		if !ask.want {
			continue
		}
		rows, ok := BuildCSVRows(ctx, deps, ask.rows)
		if !ok {
			return stored // nothing to ask about yet: not an attempt
		}
		csvText := RenderCSV(rows)
		asked = true
		prompt := tradeIdeasWrap(ask.prompt, csvText)
		var body string
		var err error
		client := cfg.HTTP
		if client == nil {
			client = &http.Client{Timeout: tradeIdeasTimeout}
		}
		if cfg.UsesMeta() {
			base := cfg.MetaAPIBase
			if base == "" {
				base = metaAIAPIBase
			}
			body, err = callMetaAI(ctx, client, base, cfg.MetaKey, model, prompt, ask.bodyCap, ask.maxTokens)
		} else {
			body, err = callCloudflareAIWith(ctx, client, tradeIdeasAPIBase,
				cfg.CloudflareAccountID, cfg.CloudflareToken, model, prompt, ask.bodyCap, ask.maxTokens)
		}
		if err == nil {
			if err = ask.put(body, rows); err != nil {
				err = fmt.Errorf("store: %w", err)
			}
		}
		if err != nil {
			if ctx.Err() == nil {
				logging.Errorf("trade ideas: refresh %s (failure %d in a row; next try in %s): %v",
					ask.name, failures+1, tradeIdeasBackoff(failures+1), err)
				noteAttempt(ctx, q, now, failures+1)
			}
			return stored
		}
		stored++
	}
	if asked {
		noteAttempt(ctx, q, now, 0)
	}
	return stored
}

// noteAttempt records an ask and the run of failures it ends with (0
// after a success).
func noteAttempt(ctx context.Context, q *db.Queries, at time.Time, failures int) {
	if err := q.PutTradeIdeasAttempt(ctx, db.PutTradeIdeasAttemptParams{AttemptedAt: at, Failures: int32(failures)}); err != nil {
		logging.Errorf("trade ideas: note attempt: %v", err)
	}
}

// forgeRegionID is The Forge (Jita): the region the trade-ideas prompt
// asks about. It matches the scanner's default region.
const forgeRegionID = int64(10000002)

// BuildCSV renders the Forge scanner's top rows as the CSV the model
// reads, capped to limit rows. Each row leads with its 1-based "#"
// number, the reference the model's answer names; the rest of the
// CSV is byte-identical to what it has always been.
func BuildCSV(ctx context.Context, deps module.Deps, limit int) (string, bool) {
	rows, ok := BuildCSVRows(ctx, deps, limit)
	if !ok {
		return "", false
	}
	return RenderCSV(rows), true
}

// RenderCSV renders row values as the CSV text the model reads: the
// header and one line per row, the leading "#" column the model's
// "(#n)" references name.
func RenderCSV(rows []ideas.Row) string {
	var buf bytes.Buffer
	out := csv.NewWriter(&buf)
	_ = out.Write([]string{"#", "Item", "Station", "Highest buy price", "Lowest sell price", "Spread %", "Sold per day", "Estimated daily profit"})
	for _, r := range rows {
		_ = out.Write([]string{fmt.Sprintf("%d", r.N), r.Item, r.Station, r.Buy, r.Sell, r.Spread, r.Sold, r.Profit})
	}
	out.Flush()
	return buf.String()
}

// BuildCSVRows reads the Forge scanner's top rows as the values the
// CSV carries and the answer later stores, capped to limit rows. It
// reads the stored scanner statistics itself (the same bounded
// stored-statistics query the spread scanner page uses, at the
// core-owned spread defaults, Deps.SpreadDefaults) and formats the
// rows here, so it does not depend on the scanner page's view. False
// when the sweep has not covered the Forge yet.
func BuildCSVRows(ctx context.Context, deps module.Deps, limit int) ([]ideas.Row, bool) {
	q := deps.DB()
	stamp, err := q.GetMarketStationStatsStamp(ctx, forgeRegionID)
	if err != nil || stamp.RowCount == 0 {
		if err != nil {
			logging.Errorf("trade ideas: station stats stamp for region %d: %v", forgeRegionID, err)
		}
		return nil, false
	}
	minSpread, rowLimit := deps.SpreadDefaults()
	rowCap := int64(limit)
	if rowCap > int64(rowLimit) {
		rowCap = int64(rowLimit)
	}
	opp, err := q.ListScannerOpportunities(ctx, db.ListScannerOpportunitiesParams{
		RegionID:  forgeRegionID,
		MinSpread: minSpread,
		MinVolume: 1,
		RowCap:    rowCap,
	})
	if err != nil {
		logging.Errorf("trade ideas: list opportunities for region %d: %v", forgeRegionID, err)
		return nil, false
	}
	if len(opp) == 0 {
		return nil, false
	}
	rows := make([]ideas.Row, 0, len(opp))
	stationMemo := make(map[int64]module.PlaceRef)
	for i, s := range opp {
		// The tradeable size is whichever runs out first: what trades
		// each day, what sellers hold, or what buyers want.
		size := s.DailyVolume
		if float64(s.SellVolume) < size {
			size = float64(s.SellVolume)
		}
		if float64(s.BuyVolume) < size {
			size = float64(s.BuyVolume)
		}
		rows = append(rows, ideas.Row{
			N:       i + 1,
			Item:    tradeIdeasCell(deps.TypeName(ctx, s.TypeID)),
			Station: tradeIdeasCell(deps.StationRef(ctx, stationMemo, s.LocationID).Name),
			Buy:     esi.FormatISK(s.BestBuy),
			Sell:    esi.FormatISK(s.BestSell),
			Spread:  fmt.Sprintf("%.1f%%", (s.BestSell-s.BestBuy)/s.BestBuy*100),
			Sold:    esi.FormatInt(int64(math.Round(s.DailyVolume))),
			Profit:  esi.FormatISK((s.BestSell - s.BestBuy) * size),
		})
	}
	return rows, true
}
