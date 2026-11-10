package app

// The region sweep, run on its own lane (runMarketLane), never inside
// the worker's one-minute cycle. Each swept region is read whole
// inside one of ESI's five-minute cache windows, started just after a
// window rolls over, and every page must carry page 1's Last-Modified:
// a page from another window would count some orders twice and miss
// others, so a mixed read is dropped and retried at the next
// rollover. The read book is compared with the region's held book
// (market_hub_book.go) and only the items that changed are written.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	json "github.com/goccy/go-json"

	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// esiWindow is one ESI answer's cache window.
type esiWindow struct {
	lastModified time.Time
	expires      time.Time
}

// windowOf reads an answer's cache window from its headers; zero
// times when ESI sent none.
func windowOf(h http.Header) esiWindow {
	var w esiWindow
	if t, err := http.ParseTime(h.Get("Last-Modified")); err == nil {
		w.lastModified = t
	}
	if t, err := http.ParseTime(h.Get("Expires")); err == nil {
		w.expires = t
	}
	return w
}

const (
	// sweepGateLead lets a region's next sweep start up to a minute
	// before regionSweepGate is up, so the sweep lands on the window
	// rollover that falls at the gate rather than the one after it.
	sweepGateLead = time.Minute
	// sweepStartGrace is how long after a window rolls over a sweep
	// may still start: ESI needs a moment to publish the new copy,
	// and a start this close leaves the whole window for the read.
	sweepStartAfter = 2 * time.Second
	sweepStartGrace = 45 * time.Second
	// defaultBookWindow is the market order route's cache window,
	// used when ESI's headers do not give one.
	defaultBookWindow = 5 * time.Minute
)

// regionSweepDue reports whether a region may start a sweep now: its
// stats are about regionSweepGate old and ESI has just rolled the
// region's book over to a new copy. A region whose stats are twice
// the gate old starts at once, whatever the window.
func (app *Application) regionSweepDue(ctx context.Context, regionID int64, now time.Time) bool {
	state, err := app.queries.GetMarketFetchState(ctx, regionSweepKind(regionID))
	if err != nil {
		return true // never swept
	}
	if state.State == fetchStateError {
		return now.Sub(state.AttemptedAt) >= marketFetchErrorGate
	}
	age := now.Sub(state.AttemptedAt)
	if age < regionSweepGate-sweepGateLead {
		return false
	}
	if age >= 2*regionSweepGate {
		return true
	}
	app.hubBooksMu.RLock()
	w, ok := app.hubWindows[regionID]
	app.hubBooksMu.RUnlock()
	if !ok || w.expires.IsZero() {
		return true
	}
	return atWindowStart(w, now)
}

// atWindowStart reports whether now falls just after one of the
// rollovers that follow w: ESI rolls a route's copy over at a fixed
// period (Expires minus Last-Modified), so the rollovers are w's
// Expires plus whole periods.
func atWindowStart(w esiWindow, now time.Time) bool {
	period := defaultBookWindow
	if !w.lastModified.IsZero() && w.expires.After(w.lastModified) {
		period = w.expires.Sub(w.lastModified)
	}
	if period < time.Minute {
		period = time.Minute
	}
	since := now.Sub(w.expires)
	if since < 0 {
		return false
	}
	phase := since % period
	return phase >= sweepStartAfter && phase <= sweepStartGrace
}

// errSweepTorn marks a read whose pages came from two of ESI's
// windows.
var errSweepTorn = errors.New("pages from two cache windows")

// regionSweepOutcome is one region's sweep in a pass: how many pages
// it read successfully and whether ESI's error limit stopped it.
type regionSweepOutcome struct {
	pages   int
	limited bool
}

// sweepRegionStats is the sweep pass: every due region is read and
// stored, side by side, all drawing on the one allowance so ESI's
// error-limit backoff stays shared. It returns how many pages it
// read, and whether ESI cut the pass short.
func (app *Application) sweepRegionStats(ctx context.Context, allowance *fetchBudget) (stored int, limited bool) {
	app.sweepMu.Lock()
	defer app.sweepMu.Unlock()

	var limitHit atomic.Bool
	outcomes := make([]regionSweepOutcome, len(sweptRegions))
	var wg sync.WaitGroup
	now := time.Now()
	for i, region := range sweptRegions {
		if !app.regionSweepDue(ctx, region.ID, now) || app.sweepBackedOff(region.ID, now) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer recoverWorkerPanic("region sweep")
			outcomes[i] = app.sweepRegion(ctx, region.ID, allowance, &limitHit)
		}()
	}
	wg.Wait()
	for _, outcome := range outcomes {
		stored += outcome.pages
		if outcome.limited {
			limited = true
		}
	}
	return stored, limited
}

// sweepRegion reads one region's whole book and stores it. A read cut
// short (the allowance, the error limit, a failed page, a mixed
// window) stores nothing; the region is read again from page 1.
func (app *Application) sweepRegion(ctx context.Context, regionID int64, allowance *fetchBudget, limitHit *atomic.Bool) regionSweepOutcome {
	var out regionSweepOutcome
	label := marketRegionLabel(regionID)
	if !allowance.take() {
		return out
	}
	first, total, header, err := app.fetchRegionBookPage(ctx, regionID, 1)
	if err != nil {
		return app.sweepFailed(ctx, regionID, 1, err, limitHit, out)
	}
	out.pages++
	win := windowOf(header)
	app.hubBooksMu.Lock()
	if app.hubWindows == nil {
		app.hubWindows = make(map[int64]esiWindow)
	}
	app.hubWindows[regionID] = win
	app.hubBooksMu.Unlock()
	if total > maxRegionSweepPagesTotal {
		total = maxRegionSweepPagesTotal
	}

	pages := make([][]esi.MarketOrder, total)
	pages[0] = first
	errs := make([]error, total)
	var read atomic.Int64
	window := app.sweepWindow(regionID)
	sem := make(chan struct{}, window)
	var wg sync.WaitGroup
	for page := 2; page <= total; page++ {
		if ctx.Err() != nil || limitHit.Load() {
			errs[page-1] = ctx.Err()
			if errs[page-1] == nil {
				errs[page-1] = esi.ErrErrorLimit
			}
			break
		}
		if !allowance.take() {
			logging.Warnf("worker: region sweep: %s stopped at page %d of %d: the pass allowance ran out", label, page, total)
			wg.Wait()
			out.pages += int(read.Load())
			return out
		}
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			defer recoverWorkerPanic("region sweep page")
			orders, _, h, err := app.fetchRegionBookPage(ctx, regionID, page)
			if err == nil && !windowOf(h).lastModified.Equal(win.lastModified) {
				err = errSweepTorn
			}
			if err != nil && page > 1 && sweepPageMissing(err) {
				err = errSweepTorn // the book shrank: a new window began mid-read
			}
			pages[page-1], errs[page-1] = orders, err
			if err == nil {
				read.Add(1)
			}
			if errors.Is(err, esi.ErrErrorLimit) {
				limitHit.Store(true)
			}
		}()
	}
	wg.Wait()
	out.pages += int(read.Load())
	for i, err := range errs {
		if err != nil {
			return app.sweepFailed(ctx, regionID, i+1, err, limitHit, out)
		}
	}
	app.noteSweepPageSuccess(regionID)
	app.noteSweepWindowClean(regionID)

	book := newHubBook(pages)
	book.lastModified, book.expires = win.lastModified, win.expires
	res, err := app.storeRegionBook(ctx, regionID, book, app.heldHubBook(regionID))
	if err != nil {
		logging.Errorf("worker: region sweep: store %s: %v", label, err)
		return out
	}
	app.holdHubBook(regionID, book)
	logging.Infof("worker: region sweep: %s done: %d of %d types written, %d station rows, %d orders over %d pages",
		label, res.written, len(book.types), res.stationRows, book.orders, total)
	app.afterRegionStore(ctx, regionID, res)
	return out
}

// sweepFailed settles a read that stopped on a failed page: the error
// limit stops every region's read; a mixed window waits for the next
// rollover; a shutdown is nobody's failure; any other failure
// enters the region's backoff.
func (app *Application) sweepFailed(ctx context.Context, regionID int64, page int, err error, limitHit *atomic.Bool, out regionSweepOutcome) regionSweepOutcome {
	label := marketRegionLabel(regionID)
	switch {
	case errors.Is(err, esi.ErrErrorLimit):
		limitHit.Store(true)
		out.limited = true
		logging.Warnf("worker: region sweep: %s cut short by ESI error limit on page %d", label, page)
	case ctx.Err() != nil:
		// shutting down: not the region's failure
	case errors.Is(err, errSweepTorn):
		logging.Warnf("worker: region sweep: %s page %d came from a newer cache window; reading again at the next rollover", label, page)
	default:
		wait := app.noteSweepPageFailure(regionID, time.Now())
		logging.Errorf("worker: region sweep: %s page %d: %v (next try in %s)", label, page, err, shortDuration(wait))
	}
	return out
}

// sweepPageMissing reports a 404: past page 1 it means the book
// ended earlier than page 1 said.
func sweepPageMissing(err error) bool {
	status, ok := esi.StatusCode(err)
	return ok && status == http.StatusNotFound
}

// fetchRegionBookPage reads one page of a region's whole book (no
// type filter) with its headers, and reports the total page count
// from X-Pages. The read rides the ESI client's bulk path: a
// whole-book page is the one payload large enough that a slow route
// needs the long deadline, not the shared client's fast failure.
func (app *Application) fetchRegionBookPage(ctx context.Context, regionID int64, page int) ([]esi.MarketOrder, int, http.Header, error) {
	path := fmt.Sprintf("/markets/%d/orders/?order_type=all&page=%d", regionID, page)
	body, header, err := app.esi.FetchRawLong(ctx, "", path)
	if err != nil {
		return nil, 0, header, err
	}
	totalPages := 1
	if raw := header.Get("X-Pages"); raw != "" {
		if n, perr := strconv.Atoi(raw); perr == nil && n > 0 {
			totalPages = n
		}
	}
	var orders []esi.MarketOrder
	if err := json.Unmarshal(body, &orders); err != nil {
		return nil, 0, header, fmt.Errorf("region book page: decode: %w", err)
	}
	return orders, totalPages, header, nil
}

// afterRegionStore runs the in-memory and derived refreshes a stored
// sweep feeds: the stat pack learns the written rows, and the
// industry recipe values reprice from them. A sweep that wrote
// nothing changes neither.
func (app *Application) afterRegionStore(ctx context.Context, regionID int64, res regionStoreResult) {
	if res.written == 0 && len(res.gone) == 0 {
		return
	}
	if res.full {
		app.refreshStatPackRegion(ctx, regionID)
	} else {
		app.refreshStatPackTypes(ctx, regionID, res.changed, res.gone, res.stations)
	}
	app.refreshIndustryRecipeValues(ctx, regionID, time.Now())
}
