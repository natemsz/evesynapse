package app

import (
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"evesynapse/internal/esi"
)

// ---------------------------------------------------------------------------
// Corporation subpages: members + tracking, wallets, orders,
// assets, structures, killmails. Every page follows the selected
// character's corporation (like Assets/Skills follow the selected
// character) and renders from the worker-warmed corporation
// snapshots and local tables only — no handler here touches ESI.
// Role-gated datasets render their recorded "needs the role"
// state instead of warming forever.
// ---------------------------------------------------------------------------

// humanizeEnum renders an ESI snake_case enum readably
// ("armor_reinforce" to "armor reinforce").
func humanizeEnum(s string) string {
	return strings.ReplaceAll(s, "_", " ")
}

// ---------------------------------------------------------------------------
// Members roster + member tracking.
// ---------------------------------------------------------------------------

// corpMemberRow is one roster line, enriched with member-tracking
// fields when the tracking snapshot is available.
type corpMemberRow struct {
	Name       string
	ID         int64
	Joined     string // start_date, date only
	LastSeen   string // last logon, formatted
	Ship       string // current ship type name
	ShipTypeID int64
	Location   string // current location title
}

// corpMembersView is the Corporation Members page body.
type corpMembersView struct {
	corpViewBase
	corpSectionState
	TrackingNote string // why tracking columns are absent ("" when present)
	Rows         []corpMemberRow
}

func (app *Application) handleCorpMembers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := pageData{
		LoggedIn:      true,
		CharacterName: app.sessions.GetString(ctx, sessionCharacterName),
		SSOConfigured: app.cfg.SSOConfigured(),
	}

	sel, ok, err := app.pickCorpPage(ctx, r)
	if err != nil {
		log.Printf("corp members: pick character: %v", err)
		data.Error = "Could not load corporation data; check the server log."
		app.render(ctx, w, http.StatusOK, "corp_members.html", data)
		return
	}
	if !ok {
		app.render(ctx, w, http.StatusOK, "corp_members.html", data)
		return
	}
	data.CorpChars = sel.Links

	view := &corpMembersView{corpViewBase: sel.Base}
	data.CorpMembers = view

	var members esi.CorpMembers
	view.corpSectionState = app.corpSection(ctx, sel.Active.CharacterID, esi.SnapCorpMembers, &members)
	if !view.Loaded {
		app.render(ctx, w, http.StatusOK, "corp_members.html", data)
		return
	}

	// Member tracking enriches the roster when its snapshot is
	// there; its absence (usually: no Director role) only costs
	// the extra columns, never the page.
	var tracking esi.CorpMemberTrackings
	trackingState := app.corpSection(ctx, sel.Active.CharacterID, esi.SnapCorpMemberTracking, &tracking)
	byCharacter := make(map[int64]esi.CorpMemberTracking, len(tracking))
	if trackingState.Loaded {
		for _, t := range tracking {
			byCharacter[t.CharacterID] = t
		}
	} else if trackingState.Forbidden {
		if trackingState.RoleMissing != "" {
			view.TrackingNote = fmt.Sprintf("Member tracking needs the %s role in-game — %s doesn't hold it, so only the roster is shown.", trackingState.RoleMissing, sel.Active.Name)
		} else {
			view.TrackingNote = "Member tracking was refused by ESI (403) — only the roster is shown."
		}
	} else {
		view.TrackingNote = "Member tracking is still warming up — only the roster is shown for now."
	}

	structureNames := app.corpStructureNames(ctx, sel.Active.CharacterID)
	for _, id := range members {
		row := corpMemberRow{Name: app.displayCharacter(ctx, id), ID: id}
		if t, ok := byCharacter[id]; ok {
			if len(t.StartDate) >= 10 {
				row.Joined = t.StartDate[:10]
			}
			row.LastSeen = formatFinish(t.LogonDate)
			if t.ShipTypeID > 0 {
				row.Ship = app.typeNameOrID(ctx, t.ShipTypeID)
				row.ShipTypeID = t.ShipTypeID
			}
			if t.LocationID > 0 {
				row.Location = app.corpLocationTitle(ctx, t.LocationID, structureNames)
			}
		}
		view.Rows = append(view.Rows, row)
	}
	sort.Slice(view.Rows, func(i, j int) bool { return view.Rows[i].Name < view.Rows[j].Name })

	app.render(ctx, w, http.StatusOK, "corp_members.html", data)
}

// ---------------------------------------------------------------------------
// Corporation wallets: division balances, per-division journal and
// transactions.
// ---------------------------------------------------------------------------

// corpWalletDivisionRow is one division-balance line.
type corpWalletDivisionRow struct {
	Division int64
	Label    string
	Balance  string // esi.FormatISK, bare number
	Active   bool
}

// corpJournalRow is one journal line of the selected division.
type corpJournalRow struct {
	Date    string
	Type    string // humanized ref_type
	Amount  string // signed, esi.FormatISK
	Balance string
	Desc    string
}

// corpTxnRow is one transaction line of the selected division.
type corpTxnRow struct {
	Date     string
	Item     string
	TypeID   int64
	Qty      string
	Unit     string // unit price
	Total    string // qty x unit price
	Side     string // "Buy" | "Sell"
	With     string // counterparty display
	ClientID int64
	// ClientIsChar: counterparty resolved as a character (else a
	// corporation — text).
	ClientIsChar bool
}

// corpWalletsView is the Corporation Wallets page body.
type corpWalletsView struct {
	corpViewBase
	corpSectionState
	Divisions  []corpWalletDivisionRow
	Division   int64 // selected division
	Journal    []corpJournalRow
	JournalCut int // entries hidden past the display cap
	Txns       []corpTxnRow
	TxnsCut    int
	LedgerNote string // why journal/transactions are absent ("" when shown)
}

// maxLedgerRows caps the journal/transaction tables; the snapshot
// holds the newest page (up to 1,000 entries) but a page wants
// headlines, not a ledger dump.
const maxLedgerRows = 100

func (app *Application) handleCorpWallets(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := pageData{
		LoggedIn:      true,
		CharacterName: app.sessions.GetString(ctx, sessionCharacterName),
		SSOConfigured: app.cfg.SSOConfigured(),
	}

	sel, ok, err := app.pickCorpPage(ctx, r)
	if err != nil {
		log.Printf("corp wallets: pick character: %v", err)
		data.Error = "Could not load corporation data; check the server log."
		app.render(ctx, w, http.StatusOK, "corp_wallets.html", data)
		return
	}
	if !ok {
		app.render(ctx, w, http.StatusOK, "corp_wallets.html", data)
		return
	}
	data.CorpChars = sel.Links

	view := &corpWalletsView{corpViewBase: sel.Base}
	data.CorpWallets = view

	var wallets esi.CorpWallets
	view.corpSectionState = app.corpSection(ctx, sel.Active.CharacterID, esi.SnapCorpWallets, &wallets)
	if !view.Loaded {
		app.render(ctx, w, http.StatusOK, "corp_wallets.html", data)
		return
	}

	division := int64(1)
	if d, err := strconv.ParseInt(r.URL.Query().Get("division"), 10, 64); err == nil && d >= 1 && d <= walletDivisionCount {
		division = d
	}
	view.Division = division

	balances := make(map[int64]float64, len(wallets))
	for _, div := range wallets {
		balances[div.Division] = div.Balance
	}
	for d := int64(1); d <= walletDivisionCount; d++ {
		row := corpWalletDivisionRow{
			Division: d,
			Label:    walletDivisionLabel(d),
			Balance:  esi.FormatISK(balances[d]),
			Active:   d == division,
		}
		view.Divisions = append(view.Divisions, row)
	}

	// The selected division's journal and transactions degrade
	// together (same role): shown when their snapshots are there,
	// a single note otherwise.
	var journal esi.CorpJournal
	journalState := app.corpSection(ctx, sel.Active.CharacterID, esi.CorpJournalKind(division), &journal)
	var txns esi.CorpWalletTransactions
	txnsState := app.corpSection(ctx, sel.Active.CharacterID, esi.CorpTxnsKind(division), &txns)

	switch {
	case journalState.Loaded:
		for _, e := range journal {
			if len(view.Journal) >= maxLedgerRows {
				view.JournalCut++
				continue
			}
			view.Journal = append(view.Journal, corpJournalRow{
				Date:    formatFinish(e.Date),
				Type:    humanizeEnum(e.RefType),
				Amount:  esi.FormatISK(e.Amount),
				Balance: esi.FormatISK(e.Balance),
				Desc:    e.Description,
			})
		}
	case journalState.Forbidden:
		if journalState.RoleMissing != "" {
			view.LedgerNote = fmt.Sprintf("Wallet ledgers need the %s role in-game — %s doesn't hold it.", journalState.RoleMissing, sel.Active.Name)
		} else {
			view.LedgerNote = "Wallet ledgers were refused by ESI (403)."
		}
	default:
		view.LedgerNote = "This division's journal is still warming up."
	}

	if txnsState.Loaded {
		for _, t := range txns {
			if len(view.Txns) >= maxLedgerRows {
				view.TxnsCut++
				continue
			}
			side := "Sell"
			if t.IsBuy {
				side = "Buy"
			}
			_, clientIsChar := app.esi.CachedCharacterName(t.ClientID)
			view.Txns = append(view.Txns, corpTxnRow{
				Date:         formatFinish(t.Date),
				Item:         app.typeNameOrID(ctx, t.TypeID),
				TypeID:       t.TypeID,
				Qty:          esi.FormatInt(t.Quantity),
				Unit:         esi.FormatISK(t.UnitPrice),
				Total:        esi.FormatISK(t.UnitPrice * float64(t.Quantity)),
				Side:         side,
				With:         app.displayCharacter(ctx, t.ClientID),
				ClientID:     t.ClientID,
				ClientIsChar: clientIsChar,
			})
		}
	}

	app.render(ctx, w, http.StatusOK, "corp_wallets.html", data)
}

// ---------------------------------------------------------------------------
// Corporation orders (open).
// ---------------------------------------------------------------------------

// corpOrdersRow is one open-order line of the Corporation Orders
// page (renamed from the earlier corpOrderRow sketch).
type corpOrderRow struct {
	Item       string
	TypeID     int64
	Side       string // "Buy" | "Sell"
	Price      string
	Volume     string // "remain / total"
	Location   string
	Region     string
	Expires    string
	IssuedBy   string
	IssuedByID int64
}

// corpOrdersView is the Corporation Orders page body.
type corpOrdersView struct {
	corpViewBase
	corpSectionState
	Rows []corpOrderRow
}

func (app *Application) handleCorpOrders(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := pageData{
		LoggedIn:      true,
		CharacterName: app.sessions.GetString(ctx, sessionCharacterName),
		SSOConfigured: app.cfg.SSOConfigured(),
	}

	sel, ok, err := app.pickCorpPage(ctx, r)
	if err != nil {
		log.Printf("corp orders: pick character: %v", err)
		data.Error = "Could not load corporation data; check the server log."
		app.render(ctx, w, http.StatusOK, "corp_orders.html", data)
		return
	}
	if !ok {
		app.render(ctx, w, http.StatusOK, "corp_orders.html", data)
		return
	}
	data.CorpChars = sel.Links

	view := &corpOrdersView{corpViewBase: sel.Base}
	data.CorpOrders = view

	var orders esi.CorpOrders
	view.corpSectionState = app.corpSection(ctx, sel.Active.CharacterID, esi.SnapCorpOrders, &orders)
	if !view.Loaded {
		app.render(ctx, w, http.StatusOK, "corp_orders.html", data)
		return
	}

	structureNames := app.corpStructureNames(ctx, sel.Active.CharacterID)
	regionNames := make(map[int64]string)
	regionName := func(id int64) string {
		if name, ok := regionNames[id]; ok {
			return name
		}
		name := fmt.Sprintf("Region #%d", id)
		if row, err := app.queries.GetSDERegion(ctx, id); err == nil && row.Name != "" {
			name = row.Name
		}
		regionNames[id] = name
		return name
	}

	rows := make([]corpOrderRow, 0, len(orders))
	for _, o := range orders {
		side := "Sell"
		if o.IsBuyOrder {
			side = "Buy"
		}
		expires := ""
		if t, err := time.Parse(time.RFC3339, o.Issued); err == nil && o.Duration > 0 {
			expires = t.Add(time.Duration(o.Duration) * 24 * time.Hour).UTC().Format("2006-01-02 15:04 UTC")
		}
		rows = append(rows, corpOrderRow{
			Item:       app.typeNameOrID(ctx, o.TypeID),
			TypeID:     o.TypeID,
			Side:       side,
			Price:      esi.FormatISK(o.Price),
			Volume:     fmt.Sprintf("%s / %s", esi.FormatInt(o.VolumeRemain), esi.FormatInt(o.VolumeTotal)),
			Location:   app.corpLocationTitle(ctx, o.LocationID, structureNames),
			Region:     regionName(o.RegionID),
			Expires:    expires,
			IssuedBy:   app.displayCharacter(ctx, o.IssuedBy),
			IssuedByID: o.IssuedBy,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Item != rows[j].Item {
			return rows[i].Item < rows[j].Item
		}
		if rows[i].Side != rows[j].Side {
			return rows[i].Side < rows[j].Side // Buy before Sell
		}
		return rows[i].Price < rows[j].Price
	})
	view.Rows = rows

	app.render(ctx, w, http.StatusOK, "corp_orders.html", data)
}

// ---------------------------------------------------------------------------
// Corporation assets.
// ---------------------------------------------------------------------------

// corpAssetsView is the Corporation Assets page body (grouping and
// rows shared with the character Assets page).
type corpAssetsView struct {
	corpViewBase
	corpSectionState
	Stacks     int
	TotalItems int64
	Locations  []assetLocation
}

func (app *Application) handleCorpAssets(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := pageData{
		LoggedIn:      true,
		CharacterName: app.sessions.GetString(ctx, sessionCharacterName),
		SSOConfigured: app.cfg.SSOConfigured(),
	}

	sel, ok, err := app.pickCorpPage(ctx, r)
	if err != nil {
		log.Printf("corp assets: pick character: %v", err)
		data.Error = "Could not load corporation data; check the server log."
		app.render(ctx, w, http.StatusOK, "corp_assets.html", data)
		return
	}
	if !ok {
		app.render(ctx, w, http.StatusOK, "corp_assets.html", data)
		return
	}
	data.CorpChars = sel.Links

	view := &corpAssetsView{corpViewBase: sel.Base}
	data.CorpAssets = view

	var items []esi.Asset
	view.corpSectionState = app.corpSection(ctx, sel.Active.CharacterID, esi.SnapCorpAssets, &items)
	if !view.Loaded {
		app.render(ctx, w, http.StatusOK, "corp_assets.html", data)
		return
	}

	view.Stacks = len(items)
	for _, it := range items {
		view.TotalItems += it.Quantity
	}

	// Player-given singleton names (worker-warmed) override type
	// names; the corp's own structures title their locations.
	overrides := make(map[int64]string)
	if rows, err := app.queries.ListItemNames(ctx); err != nil {
		log.Printf("corp assets: list item names: %v", err)
	} else {
		for _, row := range rows {
			overrides[row.ItemID] = row.Name
		}
	}
	structureTitles := app.corpStructureNames(ctx, sel.Active.CharacterID)

	view.Locations = app.buildAssetLocationsWith(ctx, items, structureTitles, overrides)

	app.render(ctx, w, http.StatusOK, "corp_assets.html", data)
}

// ---------------------------------------------------------------------------
// Corporation structures.
// ---------------------------------------------------------------------------

// corpStructureRow is one structure line.
type corpStructureRow struct {
	Name     string
	Type     string
	TypeID   int64
	System   string
	State    string // humanized
	Fuel     string // formatted expiry ("" when unfuelled/unreported)
	FuelLeft string // "in 3d 4h", "" when expired/unknown
	Timer    string // state-timer end, shown for reinforce states
	Services []string
	Reinf    string // reinforce window note
}

// corpStructuresView is the Corporation Structures page body.
type corpStructuresView struct {
	corpViewBase
	corpSectionState
	Rows []corpStructureRow
}

func (app *Application) handleCorpStructures(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := pageData{
		LoggedIn:      true,
		CharacterName: app.sessions.GetString(ctx, sessionCharacterName),
		SSOConfigured: app.cfg.SSOConfigured(),
	}

	sel, ok, err := app.pickCorpPage(ctx, r)
	if err != nil {
		log.Printf("corp structures: pick character: %v", err)
		data.Error = "Could not load corporation data; check the server log."
		app.render(ctx, w, http.StatusOK, "corp_structures.html", data)
		return
	}
	if !ok {
		app.render(ctx, w, http.StatusOK, "corp_structures.html", data)
		return
	}
	data.CorpChars = sel.Links

	view := &corpStructuresView{corpViewBase: sel.Base}
	data.CorpStructs = view

	var structures esi.CorpStructures
	view.corpSectionState = app.corpSection(ctx, sel.Active.CharacterID, esi.SnapCorpStructures, &structures)
	if !view.Loaded {
		app.render(ctx, w, http.StatusOK, "corp_structures.html", data)
		return
	}

	rows := make([]corpStructureRow, 0, len(structures))
	for _, s := range structures {
		row := corpStructureRow{
			Name:   s.Name,
			Type:   app.typeNameOrID(ctx, s.TypeID),
			TypeID: s.TypeID,
			State:  humanizeEnum(s.State),
		}
		if row.Name == "" {
			if name := app.resolvedStructureTitle(ctx, s.StructureID); name != "" {
				row.Name = name
			} else {
				row.Name = fmt.Sprintf("Structure #%d", s.StructureID)
			}
		}
		row.System = app.locationTitle(ctx, s.SystemID, "solar_system")
		if s.FuelExpires != "" {
			row.Fuel = formatFinish(s.FuelExpires)
			if t, err := time.Parse(time.RFC3339, s.FuelExpires); err == nil {
				if left := time.Until(t); left > 0 {
					row.FuelLeft = "in " + humanDuration(left)
				} else {
					row.FuelLeft = "expired"
				}
			}
		}
		if strings.HasSuffix(s.State, "_reinforce") && s.StateTimerEnd != "" {
			row.Timer = formatFinish(s.StateTimerEnd)
		}
		if s.ReinforceHour != nil {
			row.Reinf = fmt.Sprintf("Reinforce window around %02d:00 UTC", *s.ReinforceHour)
		}
		for _, svc := range s.Services {
			row.Services = append(row.Services, fmt.Sprintf("%s (%s)", svc.Name, svc.State))
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	view.Rows = rows

	app.render(ctx, w, http.StatusOK, "corp_structures.html", data)
}

// ---------------------------------------------------------------------------
// Corporation killmails: the corp's recent kills and losses. The
// recent list is a corp_* snapshot; the details behind it share
// the killmail_details store and the Killmails page rendering with
// the character view — only the kill/loss test changes (a victim
// in this corporation is a loss).
// ---------------------------------------------------------------------------

func (app *Application) handleCorpKillmails(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := pageData{
		LoggedIn:      true,
		CharacterName: app.sessions.GetString(ctx, sessionCharacterName),
		SSOConfigured: app.cfg.SSOConfigured(),
		// This page renders killmails.html, which on its own maps
		// to the Character branch; the corp view belongs to the
		// Corporation branch of the top nav.
		Section: "corporation",
	}

	sel, ok, err := app.pickCorpPage(ctx, r)
	if err != nil {
		log.Printf("corp killmails: pick character: %v", err)
		data.Error = "Could not load killmail data; check the server log."
		app.render(ctx, w, http.StatusOK, "killmails.html", data)
		return
	}
	if !ok {
		app.render(ctx, w, http.StatusOK, "killmails.html", data)
		return
	}
	data.KillmailChars = sel.Links

	view := &killmailsView{
		CharacterName: sel.Active.Name,
		Title:         sel.Base.CorpTitle + " — Killmails",
		BasePath:      "/corporations/killmails/",
		Subject:       "this corporation",
		CorpID:        sel.Base.CorpID,
	}
	data.Killmails = view

	var refs []esi.KillmailRef
	state := app.corpSection(ctx, sel.Active.CharacterID, esi.SnapCorpKillmails, &refs)
	switch {
	case state.Loaded:
		view.Loaded = true
	case state.Forbidden:
		view.RoleMissing = state.RoleMissing
	default:
		view.Warming = true
	}
	if !view.Loaded {
		app.render(ctx, w, http.StatusOK, "killmails.html", data)
		return
	}

	if len(refs) > maxKillmailsShown {
		refs = refs[:maxKillmailsShown]
	}

	prices := app.cachedPrices()
	viewer := killmailViewer{corporationID: sel.Base.CorpID}
	for _, ref := range refs {
		view.Rows = append(view.Rows, app.killmailRow(ctx, viewer, ref, prices))
	}

	app.render(ctx, w, http.StatusOK, "killmails.html", data)
}
