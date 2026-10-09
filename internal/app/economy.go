package app

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Economy cluster: Wallet, Orders, Contracts, Industry, rendered from
// worker-warmed snapshots and local tables only. Each dataset degrades
// on its own: a section whose snapshot has not landed says so, and a
// recorded fetch failure (a login that predates today's scope list
// answering 403, say) says that instead.
// ---------------------------------------------------------------------------

// econSectionState is the empty-state triage for one economy
// dataset: loaded, still warming, or unavailable with a note
// explaining why (mirrors the corporation sections' states).
type econSectionState struct {
	Loaded  bool
	Warming bool   // no snapshot yet and no failure recorded
	Note    string // why the data is absent, when not warming
}

// econSection resolves one economy section's display state: a
// decoded snapshot wins; otherwise the recorded fetch state
// decides between the warming note and the failure note. (The
// loader is loadCorpSnapshot — a plain snapshot-row decode
// despite the name; it never fetches.)
func (app *Application) econSection(ctx context.Context, characterID int64, kind string, out any) econSectionState {
	if app.loadCorpSnapshot(ctx, characterID, kind, out) {
		return econSectionState{Loaded: true}
	}
	state, detail, found := app.corpKindState(ctx, characterID, kind)
	if found && (state == fetchStateError || state == fetchStateRoleMissing) && detail != "" {
		return econSectionState{Note: detail}
	}
	return econSectionState{Warming: true}
}

// orderRangeLabel renders an order's range readably: the ESI
// enum is "station"|"solarsystem"|"region" or a jump count.
func orderRangeLabel(r string) string {
	switch r {
	case "station":
		return "Station"
	case "solarsystem":
		return "System"
	case "region":
		return "Region"
	case "":
		return ""
	default:
		return r + " jumps"
	}
}

// econLocationTitle renders an order/transaction/contract
// location: NPC station and system names from the local caches,
// then the worker-resolved structure name, then a
// "#<id>".
func (app *Application) econLocationTitle(ctx context.Context, locationID int64) string {
	if name, ok := app.esi.CachedPlaceName(ctx, locationID); ok {
		return name
	}
	if locationID > 1_000_000_000 {
		if name := app.resolvedStructureTitle(ctx, locationID); name != "" {
			return name
		}
		app.notePageWantFromContext(ctx, pageWantStructure, locationID)
		return fmt.Sprintf("Structure #%d", locationID)
	}
	app.notePageWantFromContext(ctx, pageWantPlace, locationID)
	return fmt.Sprintf("Station #%d", locationID)
}

// ---------------------------------------------------------------------------
// Wallet page: balance header, journal window, transaction window.
// ---------------------------------------------------------------------------

// walletJournalRow is one journal line. Counterparties carry
// their IDs and kinds, so the template links each by the name
// policy (characters, corporations, alliances).
type walletJournalRow struct {
	Date        string
	Type        string // humanized ref_type
	Amount      string // signed, esi.FormatISK
	Balance     string
	ShowParties bool
	FromName    string
	FromID      int64
	FromKind    string // character | corporation | alliance | "" (text)
	ToName      string
	ToID        int64
	ToKind      string
	Desc        string
}

// walletTxnRow is one transaction line.
type walletTxnRow struct {
	Date         string
	Item         string
	TypeID       int64
	Qty          string
	Unit         string
	Total        string
	Side         string // "Buy" | "Sell"
	Loc          placeRef
	With         string // counterparty display
	ClientID     int64
	ClientIsChar bool // counterparty resolved as a character
	ClientIsCorp bool // counterparty resolved as a corporation (else text)
}

// walletView is the Wallet page body.
type walletView struct {
	CharacterName string
	BalanceOK     bool
	Balance       string // esi.FormatISK, bare number
	Graph         *walletGraphView
	Journal       econSectionState
	JournalRows   []walletJournalRow
	JournalCut    int
	Txns          econSectionState
	TxnRows       []walletTxnRow
	TxnsCut       int
}

func (app *Application) handleWallet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := pageData{
		LoggedIn:      true,
		CharacterName: app.sessions.GetString(ctx, sessionCharacterName),
		SSOConfigured: app.cfg.SSOConfigured(),
	}

	_, active, links, err := app.pickCharacter(ctx, r, "/wallet/")
	if err != nil {
		logging.Errorf("wallet: list characters: %v", err)
		data.Error = "Could not load wallet data; check the server log."
		app.render(ctx, w, http.StatusOK, "wallet.html", data)
		return
	}
	if links == nil {
		app.render(ctx, w, http.StatusOK, "wallet.html", data)
		return
	}
	data.WalletChars = links

	view := &walletView{CharacterName: active.Name}
	data.Wallet = view

	// Balance header reuses the existing wallet snapshot.
	var balance float64
	if app.loadCorpSnapshot(ctx, active.CharacterID, esi.SnapWallet, &balance) {
		view.Balance = esi.FormatISK(balance)
		view.BalanceOK = true
	}

	var journal esi.WalletJournal
	view.Journal = app.econSection(ctx, active.CharacterID, esi.SnapWalletJournal, &journal)
	if view.Journal.Loaded {
		for _, e := range journal {
			if len(view.JournalRows) >= maxLedgerRows {
				view.JournalCut++
				continue
			}
			row := walletJournalRow{
				Date:    formatFinish(e.Date),
				Type:    humanizeEnum(e.RefType),
				Amount:  esi.FormatISK(e.Amount),
				Balance: esi.FormatISK(e.Balance),
			}
			if e.FirstPartyID > 0 || e.SecondPartyID > 0 {
				row.ShowParties = true
				row.FromName, row.FromKind = app.journalParty(ctx, e.FirstPartyID, e.FirstPartyType)
				row.FromID = e.FirstPartyID
				row.ToName, row.ToKind = app.journalParty(ctx, e.SecondPartyID, e.SecondPartyType)
				row.ToID = e.SecondPartyID
			}
			desc := e.Description
			if e.Reason != "" {
				if desc != "" {
					desc += " — "
				}
				desc += e.Reason
			}
			row.Desc = desc
			view.JournalRows = append(view.JournalRows, row)
		}
	}

	var txns esi.WalletTransactions
	view.Txns = app.econSection(ctx, active.CharacterID, esi.SnapWalletTxns, &txns)
	if view.Txns.Loaded {
		for _, t := range txns {
			if len(view.TxnRows) >= maxLedgerRows {
				view.TxnsCut++
				continue
			}
			side := "Sell"
			if t.IsBuy {
				side = "Buy"
			}
			with, clientIsChar, clientIsCorp := app.txnCounterparty(ctx, t.ClientID)
			view.TxnRows = append(view.TxnRows, walletTxnRow{
				Date:         formatFinish(t.Date),
				Item:         app.typeNameOrID(ctx, t.TypeID),
				TypeID:       t.TypeID,
				Qty:          esi.FormatInt(t.Quantity),
				Unit:         esi.FormatISK(t.UnitPrice),
				Total:        esi.FormatISK(t.UnitPrice * float64(t.Quantity)),
				Side:         side,
				Loc:          app.linkPlace(ctx, t.LocationID, app.econLocationTitle(ctx, t.LocationID)),
				With:         with,
				ClientID:     t.ClientID,
				ClientIsChar: clientIsChar,
				ClientIsCorp: clientIsCorp,
			})
		}
	}

	// Balance history rides the same stored rows: the decoded
	// journal above, the daily samples, and the wallet snapshot
	// the header balance came from.
	view.Graph = app.attachWalletGraph(ctx, active.UserID, active.CharacterID, journal, view.Journal.Loaded)

	app.render(ctx, w, http.StatusOK, "wallet.html", data)
}

// journalParty resolves one journal counterparty to its display
// name and kind, so the template links it by the name policy.
// The kind comes from ESI's party_type when the snapshot carries
// it; older snapshots fall back to the local name tiers — a
// character hit means character, a corporation hit corporation,
// and anything else renders through the character fallback (text
// whenever it never resolves to a linkable name).
func (app *Application) journalParty(ctx context.Context, id int64, kind string) (name string, partyKind string) {
	switch kind {
	case "character":
		return app.displayCharacter(ctx, id), "character"
	case "corporation":
		return app.corpDisplayName(ctx, id), "corporation"
	case "alliance":
		return app.allianceDisplayName(ctx, id), "alliance"
	case "":
		if _, ok := app.esi.CachedCharacterName(id); ok {
			return app.displayCharacter(ctx, id), "character"
		}
		if name, ok := app.esi.CachedCorpName(id); ok && name != "" {
			return name, "corporation"
		}
		return app.displayCharacter(ctx, id), ""
	default:
		return app.displayCharacter(ctx, id), ""
	}
}

// txnCounterparty resolves a market-transaction counterparty (no
// kind travels with transactions): a resolved character name
// means character, a resolved corporation name means
// corporation, anything else falls back to the character label.
func (app *Application) txnCounterparty(ctx context.Context, id int64) (name string, isChar, isCorp bool) {
	if name, settled := app.resolvedCharacterName(ctx, id); settled && name != "" {
		return name, true, false
	}
	if name, ok := app.esi.CachedCorpName(id); ok && name != "" {
		return name, false, true
	}
	return app.displayCharacter(ctx, id), false, false
}

// ---------------------------------------------------------------------------
// Orders page: open orders plus recent closed history.
// ---------------------------------------------------------------------------

// orderRow is one order line (open or historical).
type orderRow struct {
	Item   string
	TypeID int64
	Side   string // "Buy" | "Sell"
	Price  string
	Volume string // "remain / total"
	Loc    placeRef
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
	Open          econSectionState
	OpenRows      []orderRow
	History       econSectionState
	HistoryRows   []orderRow
	HistoryCut    int
	// Lifecycle (schema 033): stored order history for the
	// active character, computed from order_lifecycle rows only.
	LifecycleSummary *orderLifecycleSummary
	LifecycleRows    []orderLifecycleRow
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
	Loc          placeRef
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
func (app *Application) buildOrderLifecycle(ctx context.Context, characterID int64) (*orderLifecycleSummary, []orderLifecycleRow) {
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
			Item:         app.typeNameOrID(ctx, row.TypeID),
			TypeID:       row.TypeID,
			Loc:          app.linkPlace(ctx, row.LocationID, app.econLocationTitle(ctx, row.LocationID)),
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
func (app *Application) orderRegionName(ctx context.Context, cache map[int64]string, id int64) string {
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

func (app *Application) handleOrders(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := pageData{
		LoggedIn:      true,
		CharacterName: app.sessions.GetString(ctx, sessionCharacterName),
		SSOConfigured: app.cfg.SSOConfigured(),
	}

	_, active, links, err := app.pickCharacter(ctx, r, "/orders/")
	if err != nil {
		logging.Errorf("orders: list characters: %v", err)
		data.Error = "Could not load order data; check the server log."
		app.render(ctx, w, http.StatusOK, "orders.html", data)
		return
	}
	if links == nil {
		app.render(ctx, w, http.StatusOK, "orders.html", data)
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
			Item:   app.typeNameOrID(ctx, o.TypeID),
			TypeID: o.TypeID,
			Side:   side,
			Price:  esi.FormatISK(o.Price),
			Volume: fmt.Sprintf("%s / %s", esi.FormatInt(o.VolumeRemain), esi.FormatInt(o.VolumeTotal)),
			Loc:    app.linkPlace(ctx, o.LocationID, app.econLocationTitle(ctx, o.LocationID)),
			Region: app.orderRegionName(ctx, regions, o.RegionID),
			Range:  orderRangeLabel(o.Range),
			Issued: formatFinish(o.Issued),
			Corp:   o.IsCorporation,
		}
	}

	var open esi.CharOrders
	view.Open = app.econSection(ctx, active.CharacterID, esi.SnapOrders, &open)
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
				row.Expires = formatFinish(t.Add(time.Duration(o.Duration) * 24 * time.Hour).Format(time.RFC3339))
			}
			if !o.IsBuyOrder {
				row.Status = "Not checked against the order book yet"
				if h, ok := health[o.OrderID]; ok && h.CharacterID == active.CharacterID {
					row.Status, row.Bad = orderHealthText(h.MyPrice, h.Status, h.StationBest, h.RegionBest)
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
	view.History = app.econSection(ctx, active.CharacterID, esi.SnapOrdersHistory, &history)
	if view.History.Loaded {
		rows := make([]orderRow, 0, len(history))
		for _, h := range history {
			row := rowOf(h.CharOrder)
			row.State = humanizeEnum(h.State)
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

	app.render(ctx, w, http.StatusOK, "orders.html", data)
}
