package app

import (
	"bytes"
	"math"
	"net/http"
	"strconv"

	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Live regions: pending states that fill themselves in. A page
// rendered while data is still warming carries a live region
// (data-live-region + data-poll-url + data-poll-state); the
// served app.js polls the matching fragment endpoint every
// ~1.5s and swaps the section in when its state leaves the
// pending one, then stops. No JavaScript keeps today's static
// copy — the pending text and a manual reload.
//
// Every fragment renders strictly from stored rows — the
// transport never moves — so polling is cheap and the
// cache-only-render rule holds end to end.
// ---------------------------------------------------------------------------

// renderFragment executes one named define from a page template
// (history-section and trader-section in market.html, pilot-body
// in pilot.html, type-description in items.html) with the same
// link helpers the full pages use.
func (app *Application) renderFragment(w http.ResponseWriter, page, define string, data any) {
	app.renderFragmentStatus(w, http.StatusOK, page, define, data)
}

// renderFragmentStatus is renderFragment with a status of the caller's
// choosing (the planner answers a refused move with a 409 and the editor
// carrying the reason).
func (app *Application) renderFragmentStatus(w http.ResponseWriter, status int, page, define string, data any) {
	ts, err := parsedTemplate(&fragmentTemplates, "fragment", page, "templates/balancechart.html", "templates/charselector.html", "templates/locked.html")
	if err != nil {
		logging.Errorf("parse fragment template %s: %v", page, err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	buf := new(bytes.Buffer)
	if err := ts.ExecuteTemplate(buf, define, data); err != nil {
		logging.Errorf("execute fragment %s/%s: %v", page, define, err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

// handleMarketHistoryFragment re-renders just the price-history
// section of the market item view from stored rows.
func (app *Application) handleMarketHistoryFragment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	typeID, terr := strconv.ParseInt(q.Get("type"), 10, 64)
	regionID, rerr := strconv.ParseInt(q.Get("region"), 10, 64)
	if terr != nil || typeID <= 0 || rerr != nil {
		http.Error(w, "bad history fragment request", http.StatusBadRequest)
		return
	}
	if _, ok := marketRegionName(regionID); !ok {
		regionID = defaultMarketRegion
	}
	regionName, _ := marketRegionName(regionID)
	item := &marketItem{TypeID: typeID, RegionID: regionID, RegionName: regionName}
	app.attachHistory(ctx, item, typeID, regionID, 0)
	app.renderFragment(w, "market.html", "history-section", item)
}

// handleMarketTraderFragment re-renders just the trading
// snapshot of the market item view from stored rows, so the
// averages fill in alongside the chart instead of waiting for
// a manual refresh. The order book's best prices ride in from
// the page render (bs/bb query params — numbers the page had
// already fetched) purely so the margin row survives the swap;
// the handler itself never fetches.
func (app *Application) handleMarketTraderFragment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	typeID, terr := strconv.ParseInt(q.Get("type"), 10, 64)
	regionID, rerr := strconv.ParseInt(q.Get("region"), 10, 64)
	if terr != nil || typeID <= 0 || rerr != nil {
		http.Error(w, "bad trader fragment request", http.StatusBadRequest)
		return
	}
	if _, ok := marketRegionName(regionID); !ok {
		regionID = defaultMarketRegion
	}
	regionName, _ := marketRegionName(regionID)
	item := &marketItem{TypeID: typeID, RegionID: regionID, RegionName: regionName}
	if v, err := strconv.ParseFloat(q.Get("bs"), 64); err == nil && v > 0 && !math.IsInf(v, 0) {
		item.BestSellRaw = v
	}
	if v, err := strconv.ParseFloat(q.Get("bb"), 64); err == nil && v > 0 && !math.IsInf(v, 0) {
		item.BestBuyRaw = v
	}
	app.attachHistory(ctx, item, typeID, regionID, 0)
	app.renderFragment(w, "market.html", "trader-section", item)
}

// handleWalletGraphFragment re-renders just the wallet page's
// balance-history section from stored rows, so the graph fills
// in when the journal snapshot lands instead of waiting for a
// manual refresh. Cache-only, like every fragment.
func (app *Application) handleWalletGraphFragment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_, active, links, err := app.pickCharacter(ctx, r, "/wallet/")
	if err != nil || links == nil {
		app.renderFragment(w, "wallet.html", "wallet-graph", &walletGraphView{State: walletGraphEmpty})
		return
	}
	var journal esi.WalletJournal
	loaded := app.loadCorpSnapshot(ctx, active.CharacterID, esi.SnapWalletJournal, &journal)
	if !loaded {
		journal = nil
	}
	app.renderFragment(w, "wallet.html", "wallet-graph",
		app.attachWalletGraph(ctx, active.UserID, active.CharacterID, journal, loaded))
}

// handlePilotFragment re-renders just the pilot record body from
// the pilot_records queue. One of the viewer's own characters
// gets a settled pointer to the full sheet (the page itself
// redirects; the fragment cannot).
func (app *Application) handlePilotFragment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := strconv.ParseInt(r.URL.Query().Get("character"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "bad pilot fragment request", http.StatusBadRequest)
		return
	}
	if app.isOwnCharacter(ctx, id) {
		view := &pilotView{CharacterID: id, State: "ready", Name: "Your character"}
		app.renderFragment(w, "pilot.html", "pilot-own-fragment", view)
		return
	}
	app.renderFragment(w, "pilot.html", "pilot-body", app.loadPilotView(ctx, id))
}

// handleItemDescriptionFragment re-renders just an item's
// description block from the type_details queue.
func (app *Application) handleItemDescriptionFragment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	typeID, err := strconv.ParseInt(r.URL.Query().Get("type"), 10, 64)
	if err != nil || typeID <= 0 {
		http.Error(w, "bad description fragment request", http.StatusBadRequest)
		return
	}
	state, description := app.itemDescription(ctx, typeID)
	app.renderFragment(w, "items.html", "type-description", &itemTypeDetail{
		ID: typeID, DescState: state, Description: description,
	})
}
