package orders

import (
	"context"
	"evesynapse/internal/app/module"
	"evesynapse/internal/humanize"
	"fmt"
	"net/http"
	"sort"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// orderRow is one order line (open or historical).
type orderRow struct {
	Item   string
	TypeID int64
	Side   string // "Buy" | "Sell"
	Price  string
	Volume string // "remain / total"
	Loc    module.PlaceRef
	Region string
	Range  string
	Issued string
	// Expires: open orders ("" for history rows).
	Expires string
	// Status: open sell rows only — the same order-health
	// verdict the Market page's "Your orders" section shows
	// ("" until the worker's first book pass; buy rows and
	// closed history carry none, by design).
	Status string
	Bad    bool   // Status is a needs-attention verdict (undercut)
	State  string // history rows: humanized state
	Corp   bool   // placed for the corporation
}

// ordersView is the Orders page body.
type ordersView struct {
	CharacterName string
	Open          module.EconSectionState
	OpenRows      []orderRow
	History       module.EconSectionState
	HistoryRows   []orderRow
	HistoryCut    int
	// Lifecycle (schema 033): stored order history for the
	// active character, computed from order_lifecycle rows only.
	LifecycleSummary *orderLifecycleSummary
	LifecycleRows    []orderLifecycleRow
	// Beaten: the account's open orders beaten at their own
	// station (schema 105), every character, newest first.
	Beaten []beatenRow
}

// orderLifecycleSummary is the summary strip for one
// character: how many orders have been tracked, what share of
// finished orders filled completely, the typical time a filled
// order stayed open, and how often orders were beaten at their
// own station.
type orderLifecycleSummary struct {
	Tracked      int
	Filled       int
	Ended        int
	FillShare    string // e.g. "75% (3 of 4)" or "--" when nothing finished
	TypicalFill  string // median open time for filled orders, or "--"
	OutbidEvents int64
}

// orderLifecycleRow is one closed-order line of the history.
type orderLifecycleRow struct {
	Item         string
	TypeID       int64
	Loc          module.PlaceRef
	Price        string
	Filled       string // "sold 800 of 1,000" or "bought ..."
	OpenDuration string // "3 days 4 hours"
	Outcome      string // "Filled" | "Ended before filling"
}

// formatOpenDuration renders an order's time open the way the
// Orders page phrases it: "3 days 4 hours", "5 hours 12 minutes",
// "45 minutes". Sub-minute spans read "<1 minute".
func formatOpenDuration(d time.Duration) string {
	if d < time.Minute {
		return "<1 minute"
	}
	days := int(d / (24 * time.Hour))
	hours := int((d - time.Duration(days)*24*time.Hour) / time.Hour)
	minutes := int((d - time.Duration(days)*24*time.Hour - time.Duration(hours)*time.Hour) / time.Minute)
	plural := func(n int, unit string) string {
		if n == 1 {
			return fmt.Sprintf("%d %s", n, unit)
		}
		return fmt.Sprintf("%d %ss", n, unit)
	}
	switch {
	case days > 0:
		if hours == 0 {
			return plural(days, "day")
		}
		return plural(days, "day") + " " + plural(hours, "hour")
	case hours > 0:
		if minutes == 0 {
			return plural(hours, "hour")
		}
		return plural(hours, "hour") + " " + plural(minutes, "minute")
	default:
		return plural(minutes, "minute")
	}
}

// medianDuration returns the median of durations (average of the
// two middle values on an even count).
func medianDuration(durations []time.Duration) time.Duration {
	if len(durations) == 0 {
		return 0
	}
	sorted := make([]time.Duration, len(durations))
	copy(sorted, durations)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	n := len(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

// buildOrderLifecycle reads the stored lifecycle rows for one
// character and distils the summary strip and the newest closed
// orders. Cache-only: no ESI call ever leaves this function.
func (app *mod) buildOrderLifecycle(ctx context.Context, characterID int64) (*orderLifecycleSummary, []orderLifecycleRow) {
	rows, err := app.queries.ListOrderLifecycleByCharacter(ctx, characterID)
	if err != nil {
		logging.Errorf("orders: lifecycle for %d: %v", characterID, err)
		return &orderLifecycleSummary{FillShare: "--", TypicalFill: "--"}, nil
	}
	summary := &orderLifecycleSummary{FillShare: "--", TypicalFill: "--"}
	summary.Tracked = len(rows)
	var filledDurations []time.Duration
	for _, row := range rows {
		summary.OutbidEvents += row.OutbidEvents
		if !row.ClosedAt.Valid {
			continue
		}
		if row.CloseKind == "filled" {
			summary.Filled++
			if d := row.ClosedAt.Time.Sub(row.FirstSeenAt); d >= 0 {
				filledDurations = append(filledDurations, d)
			}
		} else {
			summary.Ended++
		}
	}
	closedCount := summary.Filled + summary.Ended
	if closedCount > 0 {
		summary.FillShare = fmt.Sprintf("%d%% (%d of %d)", summary.Filled*100/closedCount, summary.Filled, closedCount)
	}
	if len(filledDurations) > 0 {
		summary.TypicalFill = formatOpenDuration(medianDuration(filledDurations))
	}
	// Newest closed first, capped.
	var closed []db.OrderLifecycle
	for _, row := range rows {
		if row.ClosedAt.Valid {
			closed = append(closed, row)
		}
	}
	sort.Slice(closed, func(i, j int) bool {
		if !closed[i].ClosedAt.Time.Equal(closed[j].ClosedAt.Time) {
			return closed[i].ClosedAt.Time.After(closed[j].ClosedAt.Time)
		}
		return closed[i].OrderID > closed[j].OrderID
	})
	if len(closed) > 50 {
		closed = closed[:50]
	}
	out := make([]orderLifecycleRow, 0, len(closed))
	for _, row := range closed {
		filledQty := row.VolumeTotal - row.VolumeRemainLast
		if filledQty < 0 {
			filledQty = 0
		}
		verb := "sold"
		if row.IsBuyOrder == 1 {
			verb = "bought"
		}
		outcome := "Ended before filling"
		if row.CloseKind == "filled" {
			outcome = "Filled"
		}
		durText := formatOpenDuration(row.ClosedAt.Time.Sub(row.FirstSeenAt))
		out = append(out, orderLifecycleRow{
			Item:         app.deps.TypeNameOrID(ctx, row.TypeID),
			TypeID:       row.TypeID,
			Loc:          app.deps.LinkPlace(ctx, row.LocationID, app.deps.EconLocationTitle(ctx, row.LocationID)),
			Price:        esi.FormatISK(row.ListedPrice),
			Filled:       fmt.Sprintf("%s %s of %s", verb, esi.FormatInt(filledQty), esi.FormatInt(row.VolumeTotal)),
			OpenDuration: durText,
			Outcome:      outcome,
		})
	}
	return summary, out
}

// orderRegionName resolves a region ID through the SDE region
// table with a small per-request cache (mirrors the corp orders
// page).
func (app *mod) orderRegionName(ctx context.Context, cache map[int64]string, id int64) string {
	if name, ok := cache[id]; ok {
		return name
	}
	name := fmt.Sprintf("Region #%d", id)
	if row, err := app.queries.GetSDERegion(ctx, id); err == nil && row.Name != "" {
		name = row.Name
	}
	cache[id] = name
	return name
}

func (app *mod) handleOrders(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := &PageData{ShellData: app.deps.PageShell(ctx)}

	_, active, links, err := app.deps.PickCharacter(ctx, r, "/orders/")
	if err != nil {
		logging.Errorf("orders: list characters: %v", err)
		data.Error = "Could not load order data; check the server log."
		app.deps.Render(ctx, w, http.StatusOK, "orders.html", data)
		return
	}
	if links == nil {
		app.deps.Render(ctx, w, http.StatusOK, "orders.html", data)
		return
	}
	data.OrdersChars = links

	view := &ordersView{CharacterName: active.Name}
	data.Orders = view

	regions := make(map[int64]string)
	rowOf := func(o esi.CharOrder) orderRow {
		side := "Sell"
		if o.IsBuyOrder {
			side = "Buy"
		}
		return orderRow{
			Item:   app.deps.TypeNameOrID(ctx, o.TypeID),
			TypeID: o.TypeID,
			Side:   side,
			Price:  esi.FormatISK(o.Price),
			Volume: fmt.Sprintf("%s / %s", esi.FormatInt(o.VolumeRemain), esi.FormatInt(o.VolumeTotal)),
			Loc:    app.deps.LinkPlace(ctx, o.LocationID, app.deps.EconLocationTitle(ctx, o.LocationID)),
			Region: app.orderRegionName(ctx, regions, o.RegionID),
			Range:  module.OrderRangeLabel(o.Range),
			Issued: humanize.Finish(o.Issued),
			Corp:   o.IsCorporation,
		}
	}

	var open esi.CharOrders
	view.Open = app.deps.EconSection(ctx, active.CharacterID, esi.SnapOrders, &open)
	if view.Open.Loaded {
		// Open sell rows carry the stored order-health verdict —
		// the same rows and wording the Market page's "Your
		// orders" section renders (market.go buildYourOrders).
		// Buy rows and the history below carry none: health is a
		// sell-side check, and closed orders are pruned.
		health := make(map[int64]db.OrderHealth)
		if rows, err := app.queries.ListOrderHealthByCharacter(ctx, active.CharacterID); err != nil {
			logging.Errorf("orders: list health for character %d: %v", active.CharacterID, err)
		} else {
			for _, h := range rows {
				health[h.OrderID] = h
			}
		}
		for _, o := range open {
			row := rowOf(o)
			if t, err := time.Parse(time.RFC3339, o.Issued); err == nil && o.Duration > 0 {
				row.Expires = humanize.Finish(t.Add(time.Duration(o.Duration) * 24 * time.Hour).Format(time.RFC3339))
			}
			if !o.IsBuyOrder {
				row.Status = "Not checked against the order book yet"
				if h, ok := health[o.OrderID]; ok && h.CharacterID == active.CharacterID {
					row.Status, row.Bad = app.deps.OrderHealthText(h.MyPrice, h.Status, h.StationBest, h.RegionBest)
				}
			}
			view.OpenRows = append(view.OpenRows, row)
		}
		sort.Slice(view.OpenRows, func(i, j int) bool {
			if view.OpenRows[i].Item != view.OpenRows[j].Item {
				return view.OpenRows[i].Item < view.OpenRows[j].Item
			}
			if view.OpenRows[i].Side != view.OpenRows[j].Side {
				return view.OpenRows[i].Side < view.OpenRows[j].Side // Buy before Sell
			}
			return view.OpenRows[i].Price < view.OpenRows[j].Price
		})
	}

	var history esi.CharOrderHistory
	view.History = app.deps.EconSection(ctx, active.CharacterID, esi.SnapOrdersHistory, &history)
	if view.History.Loaded {
		rows := make([]orderRow, 0, len(history))
		for _, h := range history {
			row := rowOf(h.CharOrder)
			row.State = app.deps.HumanizeEnum(h.State)
			if row.State == "" {
				row.State = "closed"
			}
			rows = append(rows, row)
		}
		// Newest issued first (ESI returns them ordered, but the
		// page contract is ours).
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].Issued > rows[j].Issued })
		if len(rows) > maxLedgerRows {
			view.HistoryCut = len(rows) - maxLedgerRows
			rows = rows[:maxLedgerRows]
		}
		view.HistoryRows = rows
	}

	// Order history: stored lifecycle rows for the active
	// character (see buildOrderLifecycle -- cache-only).
	view.LifecycleSummary, view.LifecycleRows = app.buildOrderLifecycle(ctx, active.CharacterID)
	view.Beaten = app.buildBeatenOrders(ctx, app.deps.UserID(ctx))

	app.deps.Render(ctx, w, http.StatusOK, "orders.html", data)
}

// maxLedgerRows caps the journal and transaction tables; the snapshot
// holds the newest page (up to 1,000 entries) but a page wants headlines,
// not a ledger dump.
const maxLedgerRows = 100
