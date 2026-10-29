package app

// The market API, version 1 (/api/market/v1). Read-only market data
// from the same stored rows the pages read, in the wire format of
// internal/marketwire:
//
//	GET  /health                         the API's own state, no key (status page)
//	GET  /key                            this key's quota and today's use
//	GET  /prices?region=&types=1,2,...   resolved prices, book statistics, trends
//	GET  /history?region=&type=&days=    a type's stored daily rows
//	GET  /changes?region=&days=&kind=    the region's change feed
//	POST /appraise                       lines priced in the house order:
//	                                     abyssal items by item id, blueprint
//	                                     copies by runs, researched originals
//	                                     by ME and TE, everything else by type
//	GET  /abyssal/{type_id}/{item_id}    one mutated item's estimate, or its queue state
//
// Every answer is built from stored rows with bounded reads; no
// request reaches ESI. An abyssal item the corpus has not seen is
// queued for the worker (market_api_abyssal.go) and answers pending.
// The front door (market_api_guard.go) holds keys, rates, quotas,
// concurrency, body size and deadlines.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
	"evesynapse/internal/marketwire"
)

const (
	apiAbyssalWantsPerKeyDay = 200
	apiAbyssalWantsPending   = 5000
	apiHistoryMaxDays        = 365
)

// mountMarketAPI registers the routes.
func (app *Application) mountMarketAPI(r chi.Router) {
	r.Route("/api/market/v1", func(r chi.Router) {
		r.Get("/health", app.handleMarketAPIHealth)
		r.Group(func(r chi.Router) {
			r.Use(app.marketAPIGuard)
			r.Get("/key", app.handleMarketAPIKey)
			r.Get("/prices", app.handleMarketAPIPrices)
			r.Get("/history", app.handleMarketAPIHistory)
			r.Get("/changes", app.handleMarketAPIChanges)
			r.Post("/appraise", app.handleMarketAPIAppraise)
			r.Get("/abyssal/{typeID}/{itemID}", app.handleMarketAPIAbyssal)
		})
	})
}

// marketAPIHostGate keeps the API's own host to the API: any other
// path there is not found, so the site's pages, sign-in and session
// cookies never live on it.
func (app *Application) marketAPIHostGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if host := app.cfg.marketAPIHost; host != "" {
			reqHost := strings.ToLower(r.Host)
			if i := strings.LastIndexByte(reqHost, ':'); i > 0 && !strings.Contains(reqHost[i:], "]") {
				reqHost = reqHost[:i]
			}
			if reqHost == host && !strings.HasPrefix(r.URL.Path, "/api/") {
				http.NotFound(w, r)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// Health: for the public status page. State and age only.
// ---------------------------------------------------------------------------

var (
	marketAPIHealthMu   sync.Mutex
	marketAPIHealthAt   time.Time
	marketAPIHealthBody map[string]string
	marketAPIHealthRate = newAnonThrottle(30, 10)
)

func (app *Application) handleMarketAPIHealth(w http.ResponseWriter, r *http.Request) {
	if origin := app.cfg.statusPageOrigin; origin != "" && r.Header.Get("Origin") == origin {
		w.Header().Set("Access-Control-Allow-Origin", origin)
	}
	w.Header().Add("Vary", "Origin")
	if ok, wait := marketAPIHealthRate.allow(clientAddr(r)); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		writeMarketAPIError(w, http.StatusTooManyRequests, "slow_down", "Too many health checks from this address.")
		return
	}
	marketAPIHealthMu.Lock()
	defer marketAPIHealthMu.Unlock()
	now := time.Now().UTC()
	if marketAPIHealthBody == nil || now.Sub(marketAPIHealthAt) > 30*time.Second {
		state := "ok"
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		if err := app.db.PingContext(ctx); err != nil {
			state = "down"
		} else if len(marketAPIInFlight)*4 >= cap(marketAPIInFlight)*3 {
			state = "degraded" // three quarters of the request slots busy
		}
		cancel()
		marketAPIHealthAt = now
		marketAPIHealthBody = map[string]string{"service": "market_api", "state": state, "checked_at": now.Format(time.RFC3339)}
	}
	w.Header().Set("Cache-Control", "public, max-age=30")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_ = json.NewEncoder(w).Encode(marketAPIHealthBody)
}

// ---------------------------------------------------------------------------
// Parameters.
// ---------------------------------------------------------------------------

// apiRegion reads ?region=: a hub, the global market, or any region
// the static data knows; the default hub when absent.
func (app *Application) apiRegion(ctx context.Context, raw string) (int64, bool) {
	if raw == "" {
		return defaultMarketRegion, true
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	if _, ok := marketRegionName(id); ok || id == globalMarketRegion {
		return id, true
	}
	rows, err := app.queries.ListSDERegionNames(ctx, []int64{id})
	return id, err == nil && len(rows) == 1
}

// apiTypeList reads a comma list of type ids, deduplicated, capped.
func apiTypeList(raw string) ([]int64, bool) {
	var out []int64
	seen := map[int64]bool{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil || id <= 0 {
			return nil, false
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
		if len(out) > marketAPIMaxBatch {
			return nil, false
		}
	}
	return out, len(out) > 0
}

func nullPtr(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	f := v.Float64
	return &f
}

// ---------------------------------------------------------------------------
// Handlers.
// ---------------------------------------------------------------------------

func (app *Application) handleMarketAPIKey(w http.ResponseWriter, r *http.Request) {
	key := apiKeyFrom(r.Context())
	writeMarketAPIData(w, marketwire.KeyStatus{
		DailyQuota: key.Quota, UsedToday: marketAPIUsage.used(key.ID),
		PerMinute: marketAPIPerMinute, Burst: marketAPIBurst, MaxBatch: marketAPIMaxBatch,
	})
}

func (app *Application) handleMarketAPIPrices(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	region, ok := app.apiRegion(ctx, q.Get("region"))
	if !ok {
		writeMarketAPIError(w, http.StatusBadRequest, "bad_region", "Unknown region id.")
		return
	}
	types, ok := apiTypeList(q.Get("types"))
	if !ok {
		writeMarketAPIError(w, http.StatusBadRequest, "bad_types", "Send types=1,2,3: one to 200 type ids.")
		return
	}
	packs, err := app.loadStatPacks(ctx, region, types)
	if err != nil {
		logging.Errorf("market api: prices: %v", err)
		writeMarketAPIError(w, http.StatusServiceUnavailable, "unavailable", "Try again shortly.")
		return
	}
	now := time.Now()
	out := make([]marketwire.TypePrice, 0, len(types))
	for _, id := range types {
		p := packs[id]
		tp := marketwire.TypePrice{TypeID: id, RegionID: region}
		if p == nil {
			out = append(out, tp)
			continue
		}
		for _, side := range []struct {
			mode string
			dst  **marketwire.Price
		}{{module.AppraisalModeSell, &tp.Sell}, {module.AppraisalModeBuy, &tp.Buy}} {
			if pr := resolveAppraisalPriceMode(p.Own, p.Estimate, p.Fallback, now, side.mode); pr.ok {
				wp := &marketwire.Price{Value: pr.unit, Tier: pr.tier, AsOf: pr.asOf.UTC()}
				if p.FromGlobal && typicalTier(pr.tier) {
					wp.SourceRegionID = globalMarketRegion
				}
				*side.dst = wp
			}
		}
		if s := p.Own; s != nil {
			tp.BookAsOf = s.UpdatedAt.UTC()
			if s.SellOrders > 0 {
				tp.BookSell = &marketwire.BookSide{Typical: s.TypicalSell, Average: s.SellAvg, Best: s.BestSell, Far: s.SellMax,
					Edge5: s.SellP5, StdDev: s.SellStddev, Orders: s.SellOrders, Units: s.SellVolume}
			}
			if s.BuyOrders > 0 {
				tp.BookBuy = &marketwire.BookSide{Typical: s.TypicalBuy, Average: s.BuyAvg, Best: s.BestBuy, Far: s.BuyMin,
					Edge5: s.BuyP5, StdDev: s.BuyStddev, Orders: s.BuyOrders, Units: s.BuyVolume}
			}
			tp.Trend = &marketwire.Trend{
				SellChange1d: nullPtr(s.SellChg1d), SellChange7d: nullPtr(s.SellChg7d), SellChange30d: nullPtr(s.SellChg30d),
				BuyChange1d: nullPtr(s.BuyChg1d), BuyChange7d: nullPtr(s.BuyChg7d), BuyChange30d: nullPtr(s.BuyChg30d),
				Volatility30d: nullPtr(s.Volatility30d), HistoryDays: s.HistoryDays, TradedPerDay30d: s.AvgDailyVolume30d,
			}
		}
		out = append(out, tp)
	}
	writeMarketAPIData(w, out, "Prices resolve in order: our own swept order book, our stored estimate, the labeled Fuzzwork fallback; a missing price is absent.")
}

func (app *Application) handleMarketAPIHistory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	region, ok := app.apiRegion(ctx, q.Get("region"))
	if !ok {
		writeMarketAPIError(w, http.StatusBadRequest, "bad_region", "Unknown region id.")
		return
	}
	typeID, err := strconv.ParseInt(q.Get("type"), 10, 64)
	if err != nil || typeID <= 0 {
		writeMarketAPIError(w, http.StatusBadRequest, "bad_type", "Send type=<type id>.")
		return
	}
	days := 90
	if v, err := strconv.Atoi(q.Get("days")); err == nil {
		days = min(max(v, 1), apiHistoryMaxDays)
	}
	rows, err := app.queries.ListMarketRegionStatsDailyRecent(ctx, db.ListMarketRegionStatsDailyRecentParams{
		RegionID: region, TypeID: typeID, Since: time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02"),
	})
	if err != nil {
		logging.Errorf("market api: history: %v", err)
		writeMarketAPIError(w, http.StatusServiceUnavailable, "unavailable", "Try again shortly.")
		return
	}
	out := make([]marketwire.HistoryDay, 0, len(rows))
	for _, d := range rows {
		out = append(out, marketwire.HistoryDay{Day: d.Day, TypicalSell: d.TypicalSell, TypicalBuy: d.TypicalBuy,
			BestSell: d.BestSell, BestBuy: d.BestBuy, SellOrders: d.SellOrders, BuyOrders: d.BuyOrders,
			SellUnits: d.SellVolume, BuyUnits: d.BuyVolume})
	}
	writeMarketAPIData(w, out, "Daily rows from our own sweeps of the region's whole book; at most 90 rows per request.")
}

func (app *Application) handleMarketAPIChanges(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	region, ok := app.apiRegion(ctx, q.Get("region"))
	if !ok {
		writeMarketAPIError(w, http.StatusBadRequest, "bad_region", "Unknown region id.")
		return
	}
	days := 1
	if v, err := strconv.Atoi(q.Get("days")); err == nil {
		days = min(max(v, 1), marketChangeKeepDays)
	}
	kind := q.Get("kind")
	valid := false
	for _, k := range marketChangeKinds {
		valid = valid || k.key == kind
	}
	if !valid {
		writeMarketAPIError(w, http.StatusBadRequest, "bad_kind", "kind is one of price_up, price_down, supply_down, spread_wide, or empty.")
		return
	}
	rows, err := app.queries.ListMarketChangesForRegion(ctx, db.ListMarketChangesForRegionParams{
		RegionID: region, Since: time.Now().UTC().AddDate(0, 0, -days+1).Format("2006-01-02"), Kind: kind,
	})
	if err != nil {
		logging.Errorf("market api: changes: %v", err)
		writeMarketAPIError(w, http.StatusServiceUnavailable, "unavailable", "Try again shortly.")
		return
	}
	out := make([]marketwire.Change, 0, len(rows))
	for _, c := range rows {
		out = append(out, marketwire.Change{TypeID: c.TypeID, Day: c.Day, Kind: c.Kind, Before: c.Before, After: c.After, Pct: c.Pct, DetectedAt: c.DetectedAt.UTC()})
	}
	writeMarketAPIData(w, out, "At most 100 changes, newest day first.")
}

func (app *Application) handleMarketAPIAppraise(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req marketwire.AppraiseRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeMarketAPIError(w, http.StatusBadRequest, "bad_body", "Send JSON: {\"region_id\":..., \"mode\":\"sell\", \"lines\":[{\"type_id\":..., \"quantity\":...}]}.")
		return
	}
	if len(req.Lines) == 0 || len(req.Lines) > marketAPIMaxBatch {
		writeMarketAPIError(w, http.StatusBadRequest, "bad_lines", "Send one to 200 lines.")
		return
	}
	region, ok := app.apiRegion(ctx, strconv.FormatInt(req.RegionID, 10))
	if req.RegionID == 0 {
		region, ok = defaultMarketRegion, true
	}
	if !ok {
		writeMarketAPIError(w, http.StatusBadRequest, "bad_region", "Unknown region id.")
		return
	}
	mode := req.Mode
	switch mode {
	case "", module.AppraisalModeSell:
		mode = module.AppraisalModeSell
	case module.AppraisalModeBuy, module.AppraisalModeSplit:
	default:
		writeMarketAPIError(w, http.StatusBadRequest, "bad_mode", "mode is sell, buy or split.")
		return
	}
	lines := make([]module.AppraisalLine, 0, len(req.Lines))
	for _, l := range req.Lines {
		if l.TypeID <= 0 || l.Quantity <= 0 || l.Quantity > 1_000_000_000 || l.ItemID < 0 ||
			l.Runs < 0 || l.ME < 0 || l.ME > 10 || l.TE < 0 || l.TE > 20 {
			writeMarketAPIError(w, http.StatusBadRequest, "bad_line", "Each line needs type_id and quantity above zero; me 0 to 10, te 0 to 20.")
			return
		}
		lines = append(lines, module.AppraisalLine{TypeID: l.TypeID, Quantity: l.Quantity, ItemID: l.ItemID, Included: true,
			BlueprintCopy: l.Copy, Runs: l.Runs, MaterialEfficiency: l.ME, TimeEfficiency: l.TE})
	}
	res, err := app.AppraiseLinesWithBasis(ctx, lines, module.AppraisalBasis{RegionID: region, Mode: mode})
	if err != nil {
		logging.Errorf("market api: appraise: %v", err)
		writeMarketAPIError(w, http.StatusServiceUnavailable, "unavailable", "Try again shortly.")
		return
	}
	out := marketwire.Appraisal{RegionID: region, Mode: mode, Lines: make([]marketwire.AppraisedLine, 0, len(res.Lines))}
	key := apiKeyFrom(ctx)
	pending := app.wantAbyssalPending(ctx, key, res.Lines, req.Lines)
	for i, al := range res.Lines {
		line := marketwire.AppraisedLine{TypeID: al.TypeID, Quantity: al.Quantity, ItemID: al.ItemID,
			UnitPrice: al.UnitPrice, LineValue: al.LineValue, Priced: al.Priced, Tier: al.Tier,
			ResearchUnmatched: al.ResearchUnmatched, SourceRegionID: al.SourceRegionID}
		if al.Priced {
			line.AsOf = al.AsOf.UTC()
			out.Total += al.LineValue
			out.PricedLines++
		}
		if !al.Priced && req.Lines[i].ItemID > 0 && al.Tier == module.AppraisalTierUnpriced {
			line.Pending = pending[req.Lines[i].ItemID]
		}
		out.Lines = append(out.Lines, line)
	}
	writeMarketAPIData(w, out, "Lines with an item_id are priced as that one mutated item; pending ones are queued, ask again in a few minutes.")
}

// wantAbyssalPending resolves the queue state of every unpriced
// item line in one appraisal with batched reads: the wants and the
// known items load as sets, and the two cap counts are read once per
// request, never once per line. Items the corpus has not seen are
// queued while both caps allow, mirroring wantAbyssal's per-item
// rules. The result maps item id to "queued or already queued".
func (app *Application) wantAbyssalPending(ctx context.Context, key apiKeyRef, appraised []module.AppraisedLine, requested []marketwire.AppraiseLine) map[int64]bool {
	out := make(map[int64]bool)
	type candidate struct{ itemID, typeID int64 }
	var cands []candidate
	seen := make(map[int64]bool)
	for i, al := range appraised {
		if al.Priced || requested[i].ItemID <= 0 || al.Tier != module.AppraisalTierUnpriced {
			continue
		}
		if id := requested[i].ItemID; !seen[id] {
			seen[id] = true
			cands = append(cands, candidate{id, requested[i].TypeID})
		}
	}
	if len(cands) == 0 {
		return out
	}
	ids := make([]int64, len(cands))
	for i, c := range cands {
		ids[i] = c.itemID
	}
	wantState := make(map[int64]string)
	if rows, err := app.queries.ListAbyssalWantsByItems(ctx, ids); err == nil {
		for _, row := range rows {
			wantState[row.ItemID] = row.State
		}
	}
	known := make(map[int64]bool)
	if items, err := app.queries.ListAbyssalItemsByIDs(ctx, ids); err == nil {
		for _, item := range items {
			known[item.ItemID] = true
		}
	}
	keyLeft, globalLeft := -1, -1
	for _, c := range cands {
		if state, ok := wantState[c.itemID]; ok {
			out[c.itemID] = state == abyssalWantPending
			continue
		}
		if known[c.itemID] {
			continue
		}
		if keyLeft < 0 {
			keyLeft, globalLeft = 0, 0
			if n, err := app.queries.CountAbyssalWantsSince(ctx, db.CountAbyssalWantsSinceParams{KeyID: key.ID, Since: time.Now().UTC().Add(-24 * time.Hour)}); err == nil {
				if left := apiAbyssalWantsPerKeyDay - n; left > 0 {
					keyLeft = int(left)
				}
			}
			if n, err := app.queries.CountPendingAbyssalWants(ctx); err == nil {
				if left := apiAbyssalWantsPending - n; left > 0 {
					globalLeft = int(left)
				}
			}
		}
		if keyLeft == 0 || globalLeft == 0 {
			continue
		}
		if _, err := app.queries.AddAbyssalWant(ctx, db.AddAbyssalWantParams{ItemID: c.itemID, TypeID: c.typeID, KeyID: key.ID, RequestedAt: time.Now().UTC()}); err != nil {
			logging.Errorf("market api: queue abyssal %d: %v", c.itemID, err)
			continue
		}
		keyLeft--
		globalLeft--
		out[c.itemID] = true
	}
	return out
}

// wantAbyssal queues a mutated item the corpus has not seen, within
// the per-key daily and total caps, and reports its state.
func (app *Application) wantAbyssal(ctx context.Context, key apiKeyRef, itemID, typeID int64) string {
	if want, err := app.queries.GetAbyssalWant(ctx, itemID); err == nil {
		return want.State
	}
	if _, err := app.queries.GetAbyssalItem(ctx, itemID); err == nil {
		return "unpriced" // read already; the scoring pass has no estimate for it
	}
	if n, err := app.queries.CountAbyssalWantsSince(ctx, db.CountAbyssalWantsSinceParams{KeyID: key.ID, Since: time.Now().UTC().Add(-24 * time.Hour)}); err != nil || n >= apiAbyssalWantsPerKeyDay {
		return "unpriced"
	}
	if n, err := app.queries.CountPendingAbyssalWants(ctx); err != nil || n >= apiAbyssalWantsPending {
		return "unpriced"
	}
	if _, err := app.queries.AddAbyssalWant(ctx, db.AddAbyssalWantParams{ItemID: itemID, TypeID: typeID, KeyID: key.ID, RequestedAt: time.Now().UTC()}); err != nil {
		logging.Errorf("market api: queue abyssal %d: %v", itemID, err)
		return "unpriced"
	}
	return abyssalWantPending
}

func (app *Application) handleMarketAPIAbyssal(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	typeID, err1 := strconv.ParseInt(chi.URLParam(r, "typeID"), 10, 64)
	itemID, err2 := strconv.ParseInt(chi.URLParam(r, "itemID"), 10, 64)
	if err1 != nil || err2 != nil || typeID <= 0 || itemID <= 0 {
		writeMarketAPIError(w, http.StatusBadRequest, "bad_item", "Use /abyssal/{type_id}/{item_id}.")
		return
	}
	out := marketwire.Abyssal{ItemID: itemID, TypeID: typeID}
	item, err := app.queries.GetAbyssalItem(ctx, itemID)
	switch {
	case err == nil:
		if item.TypeID != typeID {
			writeMarketAPIError(w, http.StatusBadRequest, "type_mismatch", "That item is a different type.")
			return
		}
		out.State = "unpriced"
		if item.EstValue.Valid {
			out.State, out.Estimate = "priced", item.EstValue.Float64
			out.Low, out.High = item.EstLow.Float64, item.EstHigh.Float64
			out.Basis, out.Comps = item.EstBasis, item.EstComps
			out.Score = item.Score.Float64
			if item.ScoredAt.Valid {
				out.ScoredAt = item.ScoredAt.Time.UTC()
			}
		}
	case errors.Is(err, sql.ErrNoRows):
		out.State = app.wantAbyssal(ctx, apiKeyFrom(ctx), itemID, typeID)
	default:
		logging.Errorf("market api: abyssal %d: %v", itemID, err)
		writeMarketAPIError(w, http.StatusServiceUnavailable, "unavailable", "Try again shortly.")
		return
	}
	if out.State == abyssalWantPending {
		writeMarketAPIJSON(w, http.StatusAccepted, marketwire.Envelope{Data: out, Meta: marketwire.Meta{
			Version: marketwire.Version, GeneratedAt: time.Now().UTC(), Notes: []string{"Queued: its rolls are read and scored within a few minutes; ask again then."},
		}})
		return
	}
	writeMarketAPIData(w, out)
}
