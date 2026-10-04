package app

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sort"
	"time"

	"evesynapse/internal/esi"
)

// ---------------------------------------------------------------------------
// Economy cluster (module sweep, cluster 3): Wallet, Orders,
// Contracts, Industry. Like every other page these render from
// worker-warmed snapshots and local tables only; the handlers
// below never touch ESI. Each dataset degrades independently: a
// section whose snapshot has not landed yet says so (warming), and
// a recorded fetch failure (e.g. a login that predates today's
// scope list answering 403) says that instead — never an endless
// warm-up and never an error page.
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
// then the worker-resolved structure name, then an honest
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
// their IDs and whether each is a character, so the template can
// link characters and leave corporations as text.
type walletJournalRow struct {
	Date        string
	Type        string // humanized ref_type
	Amount      string // signed, esi.FormatISK
	Balance     string
	ShowParties bool
	FromName    string
	FromID      int64
	FromIsChar  bool
	ToName      string
	ToID        int64
	ToIsChar    bool
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
	Location     string
	With         string // counterparty display
	ClientID     int64
	ClientIsChar bool // counterparty resolved as a character (else a corporation — text)
}

// walletView is the Wallet page body.
type walletView struct {
	CharacterName string
	BalanceOK     bool
	Balance       string // esi.FormatISK, bare number
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
		log.Printf("wallet: list characters: %v", err)
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
				row.FromName, row.FromIsChar = app.journalParty(ctx, e.FirstPartyID, e.FirstPartyType)
				row.FromID = e.FirstPartyID
				row.ToName, row.ToIsChar = app.journalParty(ctx, e.SecondPartyID, e.SecondPartyType)
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
			view.TxnRows = append(view.TxnRows, walletTxnRow{
				Date:     formatFinish(t.Date),
				Item:     app.typeNameOrID(ctx, t.TypeID),
				TypeID:   t.TypeID,
				Qty:      esi.FormatInt(t.Quantity),
				Unit:     esi.FormatISK(t.UnitPrice),
				Total:    esi.FormatISK(t.UnitPrice * float64(t.Quantity)),
				Side:     side,
				Location: app.econLocationTitle(ctx, t.LocationID),
				With:     app.displayCharacter(ctx, t.ClientID),
				ClientID: t.ClientID,
				ClientIsChar: func() bool {
					_, ok := app.esi.CachedCharacterName(t.ClientID)
					return ok
				}(),
			})
		}
	}

	app.render(ctx, w, http.StatusOK, "wallet.html", data)
}

// journalParty resolves one journal counterparty to its display
// name and whether it is a character (characters link; everything
// else stays text). The kind comes from ESI's party_type when the
// snapshot carries it; older snapshots fall back to the character
// name cache — a hit means character, a miss stays unlinked
// (corporations deliberately render as text in this pass).
func (app *Application) journalParty(ctx context.Context, id int64, kind string) (name string, isChar bool) {
	name = app.displayCharacter(ctx, id)
	switch kind {
	case "character":
		return name, id > 0
	case "":
		_, ok := app.esi.CachedCharacterName(id)
		return name, ok
	default:
		return name, false
	}
}

// ---------------------------------------------------------------------------
// Orders page: open orders plus recent closed history.
// ---------------------------------------------------------------------------

// orderRow is one order line (open or historical).
type orderRow struct {
	Item     string
	TypeID   int64
	Side     string // "Buy" | "Sell"
	Price    string
	Volume   string // "remain / total"
	Location string
	Region   string
	Range    string
	Issued   string
	Expires  string // open orders ("" for history rows)
	State    string // history rows: humanized state
	Corp     bool   // placed for the corporation
}

// ordersView is the Orders page body.
type ordersView struct {
	CharacterName string
	Open          econSectionState
	OpenRows      []orderRow
	History       econSectionState
	HistoryRows   []orderRow
	HistoryCut    int
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
		log.Printf("orders: list characters: %v", err)
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
			Item:     app.typeNameOrID(ctx, o.TypeID),
			TypeID:   o.TypeID,
			Side:     side,
			Price:    esi.FormatISK(o.Price),
			Volume:   fmt.Sprintf("%s / %s", esi.FormatInt(o.VolumeRemain), esi.FormatInt(o.VolumeTotal)),
			Location: app.econLocationTitle(ctx, o.LocationID),
			Region:   app.orderRegionName(ctx, regions, o.RegionID),
			Range:    orderRangeLabel(o.Range),
			Issued:   formatFinish(o.Issued),
			Corp:     o.IsCorporation,
		}
	}

	var open esi.CharOrders
	view.Open = app.econSection(ctx, active.CharacterID, esi.SnapOrders, &open)
	if view.Open.Loaded {
		for _, o := range open {
			row := rowOf(o)
			if t, err := time.Parse(time.RFC3339, o.Issued); err == nil && o.Duration > 0 {
				row.Expires = formatFinish(t.Add(time.Duration(o.Duration) * 24 * time.Hour).Format(time.RFC3339))
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

	app.render(ctx, w, http.StatusOK, "orders.html", data)
}
