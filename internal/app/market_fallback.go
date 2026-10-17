package app

// ---------------------------------------------------------------------------
// Fallback prices (schema 054): the layer under our own market
// data. EveSynapse prices from its own ESI sweeps first, but the
// sweeps cover the five hub regions; outside them most types
// showed no typical price at all. Fuzzwork's community
// aggregate scrape covers every region with live orders, so the
// worker mirrors its bulk aggregate file into
// market_stats_fallback -- its own table, never the sweep
// tables, with the provenance on every row.
//
// The mirror is deliberately narrow about what it keeps: the
// median and the five-percent band average, the two figures
// joke orders cannot move. The file's weighted average (one
// huge lowball drags it) and its extremes are never read into
// a stored field, so they can never be quoted anywhere.
//
// The read side swaps per (type, region): our sweep's row
// while it is fresh (fallbackOwnFresh), else the fallback row
// while it is inside its hard expiry (fallbackHardExpiry),
// else our sweep's row however old -- a sweep row shows its
// age wherever it renders, so it is never a quiet fossil, and
// a page that shows a price today never loses it to this
// layer. Past the hard expiry the fallback simply stops
// existing: no price, an honest gap.
//
// Nothing here fetches at render: the worker polls the
// orderset counter hourly and downloads the aggregate file
// only when the orderset has advanced, so an unchanged hour
// costs one small request.
// ---------------------------------------------------------------------------

import (
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

const (
	// fuzzworkFetchKind names the mirror's row in
	// market_fetch_state: the hourly poll gate and the
	// transient-failure backoff ride the market pass's
	// bookkeeping, like every other market fetch.
	fuzzworkFetchKind = "fuzzwork_orderset"
	// fuzzworkPollGate is how often the orderset counter is
	// polled. Fuzzwork republishes about every half hour; an
	// hourly poll keeps the mirror at most ~1.5h old for one
	// small request in the hours nothing changed.
	fuzzworkPollGate = time.Hour
	// fuzzworkFetchTimeout bounds one poll or download. The
	// aggregate file is ~6.5 MB gzipped; five minutes is far
	// beyond a healthy fetch and far short of a stuck one
	// stalling the worker cycle.
	fuzzworkFetchTimeout = 5 * time.Minute
	// fallbackOwnFresh is how long our own sweep's row wins
	// outright: about three missed sweep cycles. Their
	// half-hour cadence never outbids a live sweep because of
	// it -- the swap prefers source, then freshness, never
	// newest-wins, so hub prices cannot flap between sources.
	fallbackOwnFresh = 3 * time.Hour
	// fallbackHardExpiry is the fallback's use-by: past it the
	// row renders as no price at all, however empty the page
	// looks without it. A stale third-party number presented
	// as current is the failure this layer exists to avoid.
	fallbackHardExpiry = 48 * time.Hour
	// fallbackMinRows is the sanity floor for one snapshot:
	// a real one holds on the order of 200,000 (region, type)
	// rows, so anything under this is a truncated or broken
	// file, not a market.
	fallbackMinRows = 10000
	// fallbackMaxSideRows caps the parse: the file carries
	// ~280,000 side rows, and past this the stream is not
	// the aggregate file, whatever it claims.
	fallbackMaxSideRows = 2000000
	// fallbackSource is the provenance label stored on every
	// row and shown wherever a fallback number renders.
	fallbackSource = "fuzzwork"
	// fallbackSourceLabel is the provenance as readers see it.
	fallbackSourceLabel = "Fuzzwork aggregate"
)

// fuzzworkUserAgent names the mirror to the service it reads:
// a descriptive agent is the etiquette the operator asks for.
const fuzzworkUserAgent = "EveSynapse market fallback (github.com/natemsz/evesynapse)"

// fuzzworkHTTPClient downloads from the aggregate service. No
// total timeout of its own; each request runs under the pass's
// deadline (fuzzworkFetchTimeout).
var fuzzworkHTTPClient = &http.Client{}

// fallbackKey is one (region, type) pair in a snapshot.
type fallbackKey struct {
	RegionID int64
	TypeID   int64
}

// fallbackAggregate is one (region, type) row of a snapshot,
// merged from the file's two side rows. A side with no orders
// keeps zeros: absent data, never a price of zero that a
// reader could quote.
type fallbackAggregate struct {
	MedianSell, FivePctSell float64
	MedianBuy, FivePctBuy   float64
	SellOrders, BuyOrders   int64
	SellVolume, BuyVolume   float64
}

// resolvedTypicals is one (type, region)'s typical prices as
// the swap picked them: the figures themselves, whose they
// are, and when that source's data was current. Band figures
// are the winner's own (the sweep's 9-in-10 bands, or the
// fallback's five-percent averages) -- the two are never
// blended into one figure.
type resolvedTypicals struct {
	TypicalSell, TypicalBuy float64
	SellBand, BuyBand       float64
	SellOrders, BuyOrders   int64
	Fallback                bool // true: Fuzzwork's figures, not our sweep's
	AsOf                    time.Time
}

// pickRegionTypicals applies the swap order to one (type,
// region)'s two stored rows (either may be nil). ok is false
// when neither row may speak: nothing stored, or only a
// fallback row past its hard expiry.
func pickRegionTypicals(own *db.MarketRegionStat, fb *db.MarketStatsFallback, now time.Time) (resolvedTypicals, bool) {
	if own != nil && now.Sub(own.UpdatedAt) < fallbackOwnFresh {
		return resolvedTypicals{
			TypicalSell: own.TypicalSell, TypicalBuy: own.TypicalBuy,
			SellBand: own.SellBand, BuyBand: own.BuyBand,
			SellOrders: own.SellOrders, BuyOrders: own.BuyOrders,
			AsOf: own.UpdatedAt,
		}, true
	}
	if fb != nil && now.Sub(fb.SourceAsof) < fallbackHardExpiry {
		return resolvedTypicals{
			TypicalSell: fb.MedianSell, TypicalBuy: fb.MedianBuy,
			SellBand: fb.FivepctSell, BuyBand: fb.FivepctBuy,
			SellOrders: fb.SellOrders, BuyOrders: fb.BuyOrders,
			Fallback: true, AsOf: fb.SourceAsof,
		}, true
	}
	if own != nil {
		// Our own row, however old: it renders with its age
		// beside it, and the fallback (absent or expired) has
		// nothing fresher to offer.
		return resolvedTypicals{
			TypicalSell: own.TypicalSell, TypicalBuy: own.TypicalBuy,
			SellBand: own.SellBand, BuyBand: own.BuyBand,
			SellOrders: own.SellOrders, BuyOrders: own.BuyOrders,
			AsOf: own.UpdatedAt,
		}, true
	}
	return resolvedTypicals{}, false
}

// resolvedRegionTypicals reads both stored rows for one
// (type, region) and picks between them. Stored reads only.
func (app *Application) resolvedRegionTypicals(ctx context.Context, regionID, typeID int64) (resolvedTypicals, bool) {
	var own *db.MarketRegionStat
	if row, err := app.queries.GetMarketRegionStat(ctx, db.GetMarketRegionStatParams{
		RegionID: regionID, TypeID: typeID,
	}); err == nil {
		own = &row
	}
	var fb *db.MarketStatsFallback
	if row, err := app.queries.GetMarketStatsFallback(ctx, db.GetMarketStatsFallbackParams{
		RegionID: regionID, TypeID: typeID,
	}); err == nil {
		fb = &row
	}
	return pickRegionTypicals(own, fb, time.Now())
}

// refreshFallbackPrices is the worker pass: poll the orderset
// counter on the hourly gate, and when it has advanced past
// the orderset the table holds, download the aggregate file
// and replace the mirror wholesale inside one transaction.
// Reports whether it stored a new snapshot.
func (app *Application) refreshFallbackPrices(ctx context.Context) bool {
	if !app.marketFetchDue(ctx, fuzzworkFetchKind, fuzzworkPollGate) {
		return false
	}
	fctx, cancel := context.WithTimeout(ctx, fuzzworkFetchTimeout)
	defer cancel()

	orderset, err := app.fetchFuzzworkOrderset(fctx)
	if err != nil {
		if ctx.Err() == nil {
			logging.Errorf("worker: fallback prices: orderset poll: %v", err)
		}
		app.recordMarketFetch(ctx, fuzzworkFetchKind, fetchStateError, "orderset poll failed")
		return false
	}
	var haveOrderset, haveRows int64
	if meta, err := app.queries.GetMarketStatsFallbackMeta(ctx); err == nil {
		haveOrderset, haveRows = meta.OrdersetID, meta.RowCount
	}
	if orderset > 0 && orderset == haveOrderset {
		// Unchanged: the poll was the whole cost of this hour.
		app.recordMarketFetch(ctx, fuzzworkFetchKind, fetchStateOK, "")
		return false
	}

	rows, fileOrderset, asof, err := app.downloadFuzzworkAggregates(fctx)
	if err != nil {
		if ctx.Err() == nil {
			logging.Errorf("worker: fallback prices: download: %v", err)
		}
		app.recordMarketFetch(ctx, fuzzworkFetchKind, fetchStateError, "aggregate download failed")
		return false
	}
	if !fallbackSnapshotSane(int64(len(rows)), haveRows) {
		// A truncated or broken file must not replace a good
		// snapshot: keep serving the previous one and let the
		// next poll try again.
		logging.Errorf("worker: fallback prices: snapshot rejected (%d rows, previous %d); keeping the previous snapshot", len(rows), haveRows)
		app.recordMarketFetch(ctx, fuzzworkFetchKind, fetchStateError, "snapshot rejected")
		return false
	}

	now := time.Now().UTC()
	tx, err := app.db.BeginTx(ctx, nil)
	if err != nil {
		logging.Errorf("worker: fallback prices: begin tx: %v", err)
		return false
	}
	defer tx.Rollback()
	qtx := app.queries.WithTx(tx)
	if err := qtx.DeleteMarketStatsFallback(ctx); err != nil {
		logging.Errorf("worker: fallback prices: clear: %v", err)
		return false
	}
	keys := make([]fallbackKey, 0, len(rows))
	for key := range rows {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].RegionID != keys[j].RegionID {
			return keys[i].RegionID < keys[j].RegionID
		}
		return keys[i].TypeID < keys[j].TypeID
	})
	for _, key := range keys {
		row := rows[key]
		if err := qtx.InsertMarketStatsFallback(ctx, db.InsertMarketStatsFallbackParams{
			RegionID: key.RegionID, TypeID: key.TypeID,
			MedianSell: row.MedianSell, FivepctSell: row.FivePctSell,
			MedianBuy: row.MedianBuy, FivepctBuy: row.FivePctBuy,
			SellOrders: row.SellOrders, BuyOrders: row.BuyOrders,
			SellVolume: row.SellVolume, BuyVolume: row.BuyVolume,
			Source: fallbackSource, OrdersetID: fileOrderset,
			SourceAsof: asof, FetchedAt: now,
		}); err != nil {
			logging.Errorf("worker: fallback prices: store %d/%d: %v", key.RegionID, key.TypeID, err)
			return false
		}
	}
	if err := qtx.UpsertMarketStatsFallbackMeta(ctx, db.UpsertMarketStatsFallbackMetaParams{
		OrdersetID: fileOrderset,
		FetchedAt:  sql.NullTime{Time: now, Valid: true},
		RowCount:   int64(len(rows)),
	}); err != nil {
		logging.Errorf("worker: fallback prices: store meta: %v", err)
		return false
	}
	if err := tx.Commit(); err != nil {
		logging.Errorf("worker: fallback prices: commit: %v", err)
		return false
	}
	app.recordMarketFetch(ctx, fuzzworkFetchKind, fetchStateOK, "")
	logging.Infof("worker: fallback prices stored (orderset %d, %d region/type rows, as of %s)",
		fileOrderset, len(rows), asof.Format(time.RFC3339))
	return true
}

// fallbackSnapshotSane is the ingest guard: a real snapshot
// holds on the order of 200,000 (region, type) rows and never
// halves or doubles between ordersets, so a file far outside
// that shape is a broken download, not a market event.
func fallbackSnapshotSane(rows, previous int64) bool {
	if rows < fallbackMinRows {
		return false
	}
	if previous > 0 && (rows < previous/2 || rows > previous*2) {
		return false
	}
	return true
}

// fetchFuzzworkOrderset polls the orderset counter: one small
// JSON read that versions the whole snapshot, so an unchanged
// hour downloads nothing.
func (app *Application) fetchFuzzworkOrderset(ctx context.Context) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		app.cfg.FuzzworkBaseURL()+"api/orderset", nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", fuzzworkUserAgent)
	resp, err := fuzzworkHTTPClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("orderset poll: HTTP %d", resp.StatusCode)
	}
	var body struct {
		Orderset int64 `json:"orderset"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&body); err != nil {
		return 0, fmt.Errorf("orderset poll: decode: %w", err)
	}
	if body.Orderset <= 0 {
		return 0, fmt.Errorf("orderset poll: no orderset in response")
	}
	return body.Orderset, nil
}

// downloadFuzzworkAggregates fetches the bulk aggregate file
// and parses it. asof is the file's own Last-Modified -- when
// the snapshot was current at its source -- or the download
// time when the header is absent.
func (app *Application) downloadFuzzworkAggregates(ctx context.Context) (map[fallbackKey]*fallbackAggregate, int64, time.Time, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		app.cfg.FuzzworkBaseURL()+"aggregatecsv.csv.gz", nil)
	if err != nil {
		return nil, 0, time.Time{}, err
	}
	req.Header.Set("User-Agent", fuzzworkUserAgent)
	resp, err := fuzzworkHTTPClient.Do(req)
	if err != nil {
		return nil, 0, time.Time{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, 0, time.Time{}, fmt.Errorf("aggregate download: HTTP %d", resp.StatusCode)
	}
	asof := time.Now().UTC()
	if lm := resp.Header.Get("Last-Modified"); lm != "" {
		if t, perr := http.ParseTime(lm); perr == nil {
			asof = t.UTC()
		}
	}
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return nil, 0, time.Time{}, fmt.Errorf("aggregate download: gzip: %w", err)
	}
	defer gz.Close()
	rows, orderset, err := parseFuzzworkAggregates(gz)
	if err != nil {
		return nil, 0, time.Time{}, err
	}
	return rows, orderset, asof, nil
}

// parseFuzzworkAggregates reads the aggregate CSV stream into
// merged (region, type) rows. The file's columns are matched
// by header name; only median, fivepercent, volume and
// numorders are read -- weightedaverage, maxval, minval and
// stddev are never looked at, so nothing downstream can quote
// them. A side whose numorders is zero contributes nothing:
// no orders is absent data, not a price of zero. The whole
// file must carry one orderset; a mixed file is mid-regeneration
// at the source and is refused whole rather than stored torn.
func parseFuzzworkAggregates(r io.Reader) (map[fallbackKey]*fallbackAggregate, int64, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	cr.ReuseRecord = true

	header, err := cr.Read()
	if err != nil {
		return nil, 0, fmt.Errorf("aggregate csv: header: %w", err)
	}
	col := make(map[string]int, len(header))
	for i, name := range header {
		col[strings.ToLower(strings.TrimSpace(name))] = i
	}
	for _, name := range []string{"what", "median", "fivepercent", "volume", "numorders", "orderset"} {
		if _, ok := col[name]; !ok {
			return nil, 0, fmt.Errorf("aggregate csv: no %s column", name)
		}
	}
	field := func(rec []string, name string) string {
		if i := col[name]; i < len(rec) {
			return strings.TrimSpace(rec[i])
		}
		return ""
	}

	out := make(map[fallbackKey]*fallbackAggregate)
	var orderset int64
	sideRows, badRows, foreignRows := 0, 0, 0
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, 0, fmt.Errorf("aggregate csv: read: %w", err)
		}
		sideRows++
		if sideRows > fallbackMaxSideRows {
			return nil, 0, fmt.Errorf("aggregate csv: more than %d rows; not the aggregate file", fallbackMaxSideRows)
		}
		set, perr := strconv.ParseInt(field(rec, "orderset"), 10, 64)
		if perr != nil || set <= 0 {
			badRows++
			continue
		}
		if orderset == 0 {
			orderset = set
		} else if set != orderset {
			foreignRows++
			continue
		}
		parts := strings.Split(field(rec, "what"), "|")
		if len(parts) != 3 {
			badRows++
			continue
		}
		regionID, err1 := strconv.ParseInt(parts[0], 10, 64)
		typeID, err2 := strconv.ParseInt(parts[1], 10, 64)
		orders, err3 := parseFuzzworkCount(field(rec, "numorders"))
		median, err4 := strconv.ParseFloat(field(rec, "median"), 64)
		fivePct, err5 := strconv.ParseFloat(field(rec, "fivepercent"), 64)
		volume, err6 := strconv.ParseFloat(field(rec, "volume"), 64)
		if err1 != nil || err2 != nil || err3 != nil || err4 != nil || err5 != nil || err6 != nil ||
			regionID <= 0 || typeID <= 0 {
			badRows++
			continue
		}
		if orders <= 0 || (median <= 0 && fivePct <= 0) {
			continue // no orders on this side: absent data
		}
		key := fallbackKey{RegionID: regionID, TypeID: typeID}
		row := out[key]
		if row == nil {
			row = &fallbackAggregate{}
			out[key] = row
		}
		if parts[2] == "true" { // buy side
			row.MedianBuy, row.FivePctBuy = median, fivePct
			row.BuyOrders, row.BuyVolume = orders, volume
		} else {
			row.MedianSell, row.FivePctSell = median, fivePct
			row.SellOrders, row.SellVolume = orders, volume
		}
	}
	if foreignRows > 0 {
		return nil, 0, fmt.Errorf("aggregate csv: %d rows from another orderset; snapshot is torn", foreignRows)
	}
	if sideRows > 0 && badRows*100 > sideRows {
		return nil, 0, fmt.Errorf("aggregate csv: %d of %d rows unreadable", badRows, sideRows)
	}
	if orderset <= 0 || len(out) == 0 {
		return nil, 0, fmt.Errorf("aggregate csv: no usable rows")
	}
	return out, orderset, nil
}

// parseFuzzworkCount reads a count field that arrives as a
// plain integer in the CSV and tolerates a decimal spelling.
func parseFuzzworkCount(s string) (int64, error) {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, err
	}
	return int64(f), nil
}
