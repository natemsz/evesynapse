package contracts

// The contract search's live feed: the unfiltered listing, newest
// first, gains the contracts a sweep adds without a reload. A
// watcher runs only while a stream is open: every liveCheckEvery it
// reads the corpus's newest contract id (an index read), and only
// when that moved does it run the listing's own search and send the
// first page's rows, rendered by the page's own template define.
// The browser slots in the rows it does not have yet and highlights
// them until they are seen.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sync"
	"time"

	"evesynapse/internal/app/module"
	"evesynapse/internal/logging"
)

// liveFeedName names the contract search's broadcast feed.
const liveFeedName = "contracts-search"

// liveCheckEvery is how often the watcher looks for new contracts
// while a stream is open. Sweeps land region by region on ESI's own
// cadence, so a check every half minute is current without load.
const liveCheckEvery = 30 * time.Second

// liveWatch is the one watcher shared by every stream: running while
// any stream is open, stopped once the last one closes.
var liveWatch struct {
	mu      sync.Mutex
	running bool
}

// liveRow is one streamed result: the contract id and its rendered
// rows (the result row and its drawer row).
type liveRow struct {
	ID   int64  `json:"id"`
	HTML string `json:"html"`
}

// unfilteredSearch reports whether a search query is the default
// listing: no filter set, first page, newest first.
func unfilteredSearch(q url.Values) bool {
	for k, vs := range q {
		for _, v := range vs {
			if v == "" {
				continue
			}
			switch {
			case k == "sort" && v == "newest":
			case k == "page" && v == "1":
			default:
				return false
			}
		}
	}
	return true
}

// handleSearchStream is GET /contracts/search/stream: the live feed
// of the unfiltered listing.
func (app *mod) handleSearchStream(w http.ResponseWriter, r *http.Request) {
	feed := app.deps.LiveFeed(liveFeedName)
	feed.Serve(w, r, func() { startLiveWatch(app.deps, feed) })
}

// startLiveWatch starts the shared watcher unless it already runs.
func startLiveWatch(deps module.Deps, feed module.LiveFeed) {
	liveWatch.mu.Lock()
	defer liveWatch.mu.Unlock()
	if liveWatch.running {
		return
	}
	liveWatch.running = true
	go runLiveWatch(deps, feed)
}

// runLiveWatch checks for new contracts until no stream is open. The
// first read only sets the baseline: the page a viewer just loaded
// already shows the newest contracts.
func runLiveWatch(deps module.Deps, feed module.LiveFeed) {
	tick := time.NewTicker(liveCheckEvery)
	defer tick.Stop()
	var newest int64
	first := true
	for {
		if feed.Subscribers() == 0 {
			liveWatch.mu.Lock()
			// A stream may have opened since the count: it found
			// the watcher still running and did not start one.
			if feed.Subscribers() == 0 {
				liveWatch.running = false
				liveWatch.mu.Unlock()
				return
			}
			liveWatch.mu.Unlock()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		n, err := deps.DB().GetNewestPublicContractID(ctx)
		if err != nil {
			logging.Warnf("contract search live: newest contract: %v", err)
		} else if first {
			newest, first = n, false
		} else if n != newest {
			newest = n
			publishLiveRows(ctx, deps, feed)
		}
		cancel()
		<-tick.C
	}
}

// publishLiveRows runs the unfiltered listing and sends its first
// page, newest first, as one "contracts" event.
func publishLiveRows(ctx context.Context, deps module.Deps, feed module.LiveFeed) {
	view := newMod(deps).buildSearchView(ctx, url.Values{})
	if view == nil || len(view.Rows) == 0 {
		return
	}
	rows := make([]liveRow, 0, len(view.Rows))
	for _, row := range view.Rows {
		html, err := deps.ExecuteFragment("contract_search.html", "cs-result-row", row)
		if err != nil {
			logging.Errorf("contract search live: %v", err)
			return
		}
		rows = append(rows, liveRow{ID: row.ContractID, HTML: html})
	}
	payload, err := json.Marshal(struct {
		Rows []liveRow `json:"rows"`
	}{rows})
	if err != nil {
		return
	}
	feed.Publish("contracts", payload)
}
