// Package tradeideas is the Trade Ideas module: a model's read on the
// Forge spread scanner, on its own page under Market. The worker asks
// the configured model (Meta's Muse Wonder with MODEL_API_KEY, else
// Gemma on Cloudflare Workers AI) about the top scanner rows every so
// often and stores the answer; the page renders the stored answer only,
// so the model is never on the render path. One model call per refresh:
// the home module displays a smaller amount of the page's saved answer
// and never asks itself. The external model calls
// belong to this module: a build without it never makes one.
//
// Cost bounds: the prompt carries at most tradeIdeasPageCSVRows
// scanner rows, refreshes run at most every tradeIdeasRefreshAge (and
// only when the Forge sweep has data), a failing refresh backs off
// from 30 minutes (trade_ideas_attempts), the call runs beside the
// worker cycle rather than in it, the answer is truncated to
// tradeIdeasPageBodyCap, and the token travels in the Authorization
// header alone: never a URL, never a log line.
package tradeideas

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"fmt"
	"math"
	"net/http"
	"sort"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/ideas"
	"evesynapse/internal/logging"
	"evesynapse/internal/markethistory"
	"evesynapse/internal/tradefees"
)

// Module is the Trade Ideas module's registration.
var Module = module.Module{
	ID:            "tradeideas",
	Title:         "Trade Ideas",
	Requires:      []string{},
	Scopes:        []module.Scope{},
	EnhanceScopes: []module.Scope{},
	Activities:    []string{"market"},
	Sitemap:       []module.SitemapEntry{{Path: "/market/ideas/", ChangeFreq: "daily", Priority: "0.6"}},
	Nav: []module.NavSection{{
		Section: "market",
		Entries: []module.NavEntry{
			{Href: "/market/ideas/", Label: "Trade Ideas", Pages: []string{"market_tradeideas.html"}, After: "/market/baskets/"},
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
// answer, when one is due (tradeIdeasDue). One ask only, the page's:
// the home module displays a smaller amount of the page's saved
// answer and never asks itself, so a short widget cap can never block
// the page. It reports how many answers landed. Anything failing
// (unconfigured, no sweep yet, the model down) keeps the previous
// answer; a failed ask is recorded and backs off.
func Refresh(ctx context.Context, deps module.Deps) (stored int) {
	cfg := deps.ModelConfig()
	if !cfg.Configured() {
		return 0
	}
	q := deps.DB()
	now := time.Now().UTC()
	// One model answers: Muse Wonder when its key is set, else
	// Gemma. An answer another model gave counts as never answered,
	// so switching models (setting or clearing MODEL_API_KEY) asks
	// the new one on the next cycle instead of up to 12 hours later.
	model := tradeIdeasModel
	if cfg.UsesMeta() {
		model = metaAIModel
	}
	var pageAt, attemptedAt time.Time
	failures := 0
	if row, err := q.GetTradeIdeasPage(ctx); err == nil && row.Model == model {
		pageAt = row.AsOf
	}
	if a, err := q.GetTradeIdeasAttempt(ctx); err == nil {
		attemptedAt, failures = a.AttemptedAt, int(a.Failures)
	}
	if !tradeIdeasDue(now, pageAt, attemptedAt, failures) {
		return 0
	}
	rows, ok := BuildCSVRows(ctx, deps, tradeIdeasPageCSVRows)
	if !ok {
		return 0 // nothing to ask about yet: not an attempt
	}
	csvText := RenderCSV(rows)
	prompt := tradeIdeasWrap(tradeIdeasPagePrompt, csvText)
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
		body, err = callMetaAI(ctx, client, base, cfg.MetaKey, model, prompt, tradeIdeasPageBodyCap, metaAIPageMaxTokens)
	} else {
		body, err = callCloudflareAIWith(ctx, client, tradeIdeasAPIBase,
			cfg.CloudflareAccountID, cfg.CloudflareToken, model, prompt, tradeIdeasPageBodyCap, tradeIdeasPageMaxTokens)
	}
	if err == nil {
		raw, encErr := ideas.EncodeRows(rows)
		if encErr != nil {
			err = encErr
		} else if err = q.PutTradeIdeasPage(ctx, db.PutTradeIdeasPageParams{Body: body, Rows: sql.NullString{String: raw, Valid: true}, AsOf: now, Model: model}); err != nil {
			err = fmt.Errorf("store: %w", err)
		}
	}
	if err != nil {
		if ctx.Err() == nil {
			logging.Errorf("trade ideas: refresh page (failure %d in a row; next try in %s): %v",
				failures+1, tradeIdeasBackoff(failures+1), err)
			noteAttempt(ctx, q, now, failures+1)
		}
		return 0
	}
	noteAttempt(ctx, q, now, 0)
	return 1
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
const forgeRegionID = markethistory.ForgeRegionID

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
	_ = out.Write([]string{"#", "Item", "Station", "Typical buy price", "Typical sell price", "Spread %", "Sold per day", "Estimated daily profit"})
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
	opp, err := q.ListScannerOpportunities(ctx, db.ListScannerOpportunitiesParams{
		RegionID:  forgeRegionID,
		MinSpread: minSpread,
		MinVolume: 0,
		MinIsk:    float64(markethistory.DefaultMinISK(forgeRegionID)),
		RowCap:    int64(rowLimit),
	})
	if err != nil {
		logging.Errorf("trade ideas: list opportunities for region %d: %v", forgeRegionID, err)
		return nil, false
	}
	if len(opp) == 0 {
		return nil, false
	}
	type netCandidate struct {
		s         db.ListScannerOpportunitiesRow
		spreadPct float64
		netProfit float64
	}
	broker, salesTax := tradefees.PerfectBrokerFee(), tradefees.PerfectSalesTax()
	kept := make([]netCandidate, 0, len(opp))
	for _, s := range opp {
		// The rows are built by the background worker with no
		// viewer, so the spread and profit are net of broker
		// fees and sales tax at perfect skills and relations.
		// Drop any row whose net spread falls below the floor
		// or whose net profit is non-positive before anything
		// reaches the model.
		spreadPct := tradefees.NetSpreadPct(s.TypicalBuy, s.TypicalSell, broker, salesTax)
		if spreadPct < minSpread {
			continue
		}
		unitProfit, _ := tradefees.NetRoundTrip(s.TypicalBuy, s.TypicalSell, broker, salesTax)
		if unitProfit <= 0 {
			continue
		}
		// The tradeable size is whichever runs out first: what trades
		// each day, what sellers hold, or what buyers want.
		size := s.DailyVolume
		if float64(s.SellVolume) < size {
			size = float64(s.SellVolume)
		}
		if float64(s.BuyVolume) < size {
			size = float64(s.BuyVolume)
		}
		kept = append(kept, netCandidate{
			s:         s,
			spreadPct: spreadPct,
			netProfit: unitProfit * size,
		})
	}
	if len(kept) == 0 {
		return nil, false
	}
	// Re-rank by net daily profit after fees, breaking ties deterministically.
	sort.SliceStable(kept, func(i, j int) bool {
		if kept[i].netProfit != kept[j].netProfit {
			return kept[i].netProfit > kept[j].netProfit
		}
		if kept[i].s.TypeID != kept[j].s.TypeID {
			return kept[i].s.TypeID < kept[j].s.TypeID
		}
		return kept[i].s.LocationID < kept[j].s.LocationID
	})
	capN := min(limit, rowLimit)
	if len(kept) > capN {
		kept = kept[:capN]
	}
	rows := make([]ideas.Row, 0, len(kept))
	stationMemo := make(map[int64]module.PlaceRef)
	for i, c := range kept {
		rows = append(rows, ideas.Row{
			N:       i + 1,
			Item:    tradeIdeasCell(deps.TypeName(ctx, c.s.TypeID)),
			Station: tradeIdeasCell(deps.StationRef(ctx, stationMemo, c.s.LocationID).Name),
			Buy:     esi.FormatISK(c.s.TypicalBuy),
			Sell:    esi.FormatISK(c.s.TypicalSell),
			Spread:  fmt.Sprintf("%.1f%%", c.spreadPct),
			Sold:    esi.FormatInt(int64(math.Round(c.s.DailyVolume))),
			Profit:  esi.FormatISK(c.netProfit),
		})
	}
	return rows, true
}
