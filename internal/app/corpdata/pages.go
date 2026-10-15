package corpdata

import (
	"context"
	"evesynapse/internal/app/module"
	"evesynapse/internal/balancechart"
	"evesynapse/internal/humanize"
	"evesynapse/internal/standing"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Corporation subpages: members + tracking, wallets, orders, assets,
// structures, killmails. Each follows the selected character's
// corporation and renders from the worker-warmed corporation snapshots
// and local tables only. Role-gated datasets render their recorded
// "needs the role" state.
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
	Linked     bool   // linked on EveSynapse, so it can be put in a group
	Groups     string // the corporation's groups it is in
}

// corpMembersView is the Corporation Members page body.
type corpMembersView struct {
	module.CorpViewBase
	module.CorpSectionState
	TrackingNote string // why tracking columns are absent ("" when present)
	Rows         []corpMemberRow
	CanGroup     bool // the reader is a director here: may put members in groups
	Groups       []groupChoice
}

func (app *mod) handleCorpMembers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := &PageData{ShellData: app.deps.PageShell(ctx)}

	sel, ok, err := app.deps.PickCorpPage(ctx, r)
	if err != nil {
		logging.Errorf("corp members: pick character: %v", err)
		data.Error = "Could not load corporation data; check the server log."
		app.deps.Render(ctx, w, http.StatusOK, "corp_members.html", data)
		return
	}
	if !ok {
		app.deps.Render(ctx, w, http.StatusOK, "corp_members.html", data)
		return
	}
	data.CorpChars = sel.Links

	view := &corpMembersView{CorpViewBase: sel.Base}
	data.CorpMembers = view

	var members esi.CorpMembers
	view.CorpSectionState = app.deps.CorpSection(ctx, sel.Active.CharacterID, esi.SnapCorpMembers, &members)
	if !view.Loaded {
		app.deps.Render(ctx, w, http.StatusOK, "corp_members.html", data)
		return
	}

	// Member tracking enriches the roster when its snapshot is
	// there; its absence (usually: no Director role) only costs
	// the extra columns, never the page.
	var tracking esi.CorpMemberTrackings
	trackingState := app.deps.CorpSection(ctx, sel.Active.CharacterID, esi.SnapCorpMemberTracking, &tracking)
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
		view.TrackingNote = "Member tracking is still syncing. Only the roster is shown for now."
	}

	structureNames := app.deps.CorpStructureNames(ctx, sel.Active.CharacterID)
	for _, id := range members {
		row := corpMemberRow{Name: app.deps.DisplayCharacter(ctx, id), ID: id}
		if t, ok := byCharacter[id]; ok {
			if len(t.StartDate) >= 10 {
				row.Joined = t.StartDate[:10]
			}
			row.LastSeen = humanize.Finish(t.LogonDate)
			if t.ShipTypeID > 0 {
				row.Ship = app.deps.TypeNameOrID(ctx, t.ShipTypeID)
				row.ShipTypeID = t.ShipTypeID
			}
			if t.LocationID > 0 {
				row.Location = app.deps.CorpLocationTitle(ctx, t.LocationID, structureNames)
			}
		}
		view.Rows = append(view.Rows, row)
	}
	sort.Slice(view.Rows, func(i, j int) bool { return view.Rows[i].Name < view.Rows[j].Name })
	app.memberGroups(ctx, app.deps.UserID(ctx), view)

	app.deps.Render(ctx, w, http.StatusOK, "corp_members.html", data)
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
	// ClientIsChar: counterparty resolved as a character;
	// ClientIsCorp: resolved as a corporation (else text).
	ClientIsChar bool
	ClientIsCorp bool
}

// corpWalletsView is the Corporation Wallets page body.
type corpWalletsView struct {
	module.CorpViewBase
	module.CorpSectionState
	Divisions  []corpWalletDivisionRow
	Division   int64 // selected division
	Journal    []corpJournalRow
	JournalCut int // entries hidden past the display cap
	Txns       []corpTxnRow
	TxnsCut    int
	LedgerNote string                     // why journal/transactions are absent ("" when shown)
	Graph      *balancechart.BalanceChart // the division's balance over time; nil when there is nothing to draw
	GraphNote  string                     // why Graph is absent ("" when it is drawn)
}

// maxLedgerRows caps the journal/transaction tables; the snapshot
// holds the newest page (up to 1,000 entries) but a page wants
// headlines, not a ledger dump.
const maxLedgerRows = 100

func (app *mod) handleCorpWallets(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := &PageData{ShellData: app.deps.PageShell(ctx)}

	sel, ok, err := app.deps.PickCorpPage(ctx, r)
	if err != nil {
		logging.Errorf("corp wallets: pick character: %v", err)
		data.Error = "Could not load corporation data; check the server log."
		app.deps.Render(ctx, w, http.StatusOK, "corp_wallets.html", data)
		return
	}
	if !ok {
		app.deps.Render(ctx, w, http.StatusOK, "corp_wallets.html", data)
		return
	}
	data.CorpChars = sel.Links

	view := &corpWalletsView{CorpViewBase: sel.Base}
	data.CorpWallets = view

	var wallets esi.CorpWallets
	view.CorpSectionState = app.deps.CorpSection(ctx, sel.Active.CharacterID, esi.SnapCorpWallets, &wallets)
	if !view.Loaded {
		app.deps.Render(ctx, w, http.StatusOK, "corp_wallets.html", data)
		return
	}

	division := int64(1)
	if d, err := strconv.ParseInt(r.URL.Query().Get("division"), 10, 64); err == nil && d >= 1 && d <= module.WalletDivisionCount {
		division = d
	}
	view.Division = division

	balances := make(map[int64]float64, len(wallets))
	for _, div := range wallets {
		balances[div.Division] = div.Balance
	}
	for d := int64(1); d <= module.WalletDivisionCount; d++ {
		row := corpWalletDivisionRow{
			Division: d,
			Label:    module.WalletDivisionLabel(d),
			Balance:  esi.FormatISK(balances[d]),
			Active:   d == division,
		}
		view.Divisions = append(view.Divisions, row)
	}

	// The selected division's journal and transactions degrade
	// together (same role): shown when their snapshots are there,
	// a single note otherwise.
	var journal esi.CorpJournal
	journalState := app.deps.CorpSection(ctx, sel.Active.CharacterID, esi.CorpJournalKind(division), &journal)
	var txns esi.CorpWalletTransactions
	txnsState := app.deps.CorpSection(ctx, sel.Active.CharacterID, esi.CorpTxnsKind(division), &txns)

	switch {
	case journalState.Loaded:
		for _, e := range journal {
			if len(view.Journal) >= maxLedgerRows {
				view.JournalCut++
				continue
			}
			view.Journal = append(view.Journal, corpJournalRow{
				Date:    humanize.Finish(e.Date),
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
		view.LedgerNote = "This division's journal is still syncing."
	}

	// The division's balance over time, from the same journal plus the
	// current balance; when there is no journal to draw from, the graph
	// section says why in the ledger's own words.
	if journalState.Loaded {
		var balanceAt time.Time
		if snap, err := app.queries.GetSnapshot(ctx, db.GetSnapshotParams{
			CharacterID: sel.Active.CharacterID, Kind: esi.SnapCorpWallets,
		}); err == nil {
			balanceAt = snap.FetchedAt
		}
		balance, balanceKnown := balances[division]
		view.Graph, view.GraphNote = corpWalletGraph(journal, balance, balanceKnown, balanceAt)
	} else {
		view.GraphNote = view.LedgerNote
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
			with, clientIsChar, clientIsCorp := app.deps.TxnCounterparty(ctx, t.ClientID)
			view.Txns = append(view.Txns, corpTxnRow{
				Date:         humanize.Finish(t.Date),
				Item:         app.deps.TypeNameOrID(ctx, t.TypeID),
				TypeID:       t.TypeID,
				Qty:          esi.FormatInt(t.Quantity),
				Unit:         esi.FormatISK(t.UnitPrice),
				Total:        esi.FormatISK(t.UnitPrice * float64(t.Quantity)),
				Side:         side,
				With:         with,
				ClientID:     t.ClientID,
				ClientIsChar: clientIsChar,
				ClientIsCorp: clientIsCorp,
			})
		}
	}

	app.deps.Render(ctx, w, http.StatusOK, "corp_wallets.html", data)
}

// ---------------------------------------------------------------------------
// Corporation orders (open).
// ---------------------------------------------------------------------------

// corpOrdersRow is one open-order line of the Corporation Orders
// page (renamed from the earlier corpOrderRow sketch).
type corpOrderRow struct {
	Item     string
	TypeID   int64
	Side     string // "Buy" | "Sell"
	Price    string
	Volume   string // "remain / total"
	Location module.PlaceRef
	Region   string
	Expires  string
	// Status: the order-health verdict when the worker has one
	// for this order (see handleCorpOrders); "" when the order
	// was placed by someone whose orders the app doesn't check.
	Status     string
	Bad        bool // Status is a needs-attention verdict (undercut)
	IssuedBy   string
	IssuedByID int64
}

// corpOrdersView is the Corporation Orders page body.
type corpOrdersView struct {
	module.CorpViewBase
	module.CorpSectionState
	Rows []corpOrderRow
}

func (app *mod) handleCorpOrders(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := &PageData{ShellData: app.deps.PageShell(ctx)}

	sel, ok, err := app.deps.PickCorpPage(ctx, r)
	if err != nil {
		logging.Errorf("corp orders: pick character: %v", err)
		data.Error = "Could not load corporation data; check the server log."
		app.deps.Render(ctx, w, http.StatusOK, "corp_orders.html", data)
		return
	}
	if !ok {
		app.deps.Render(ctx, w, http.StatusOK, "corp_orders.html", data)
		return
	}
	data.CorpChars = sel.Links

	view := &corpOrdersView{CorpViewBase: sel.Base}
	data.CorpOrders = view

	var orders esi.CorpOrders
	view.CorpSectionState = app.deps.CorpSection(ctx, sel.Active.CharacterID, esi.SnapCorpOrders, &orders)
	if !view.Loaded {
		app.deps.Render(ctx, w, http.StatusOK, "corp_orders.html", data)
		return
	}

	structureNames := app.deps.CorpStructureNames(ctx, sel.Active.CharacterID)
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

	// Order health reaches corporation orders too: an order a synced
	// character placed for the corp sits in that character's order
	// snapshot under the same order id, so the worker's health rows
	// cover it keyed (issuer, order). A sell order placed by one of the
	// account's own characters without a verdict yet gets the
	// not-checked line; orders from issuers the app does not sync stay
	// blank.
	userID := app.deps.UserID(ctx)
	health := make(map[int64]db.OrderHealth)
	ownChars := make(map[int64]bool)
	if userID > 0 {
		if rows, err := app.queries.ListOrderHealthByUser(ctx, userID); err != nil {
			logging.Errorf("corp orders: list health for user %d: %v", userID, err)
		} else {
			for _, h := range rows {
				health[h.OrderID] = h
			}
		}
		if chars, err := app.queries.ListCharactersByUser(ctx, userID); err != nil {
			logging.Errorf("corp orders: list characters for user %d: %v", userID, err)
		} else {
			for _, ch := range chars {
				ownChars[ch.CharacterID] = true
			}
		}
	}

	rows := make([]corpOrderRow, 0, len(orders))
	placeMemo := make(map[int64]module.PlaceRef)
	for _, o := range orders {
		side := "Sell"
		if o.IsBuyOrder {
			side = "Buy"
		}
		expires := ""
		if t, err := time.Parse(time.RFC3339, o.Issued); err == nil && o.Duration > 0 {
			expires = t.Add(time.Duration(o.Duration) * 24 * time.Hour).UTC().Format("2006-01-02 15:04 UTC")
		}
		row := corpOrderRow{
			Item:       app.deps.TypeNameOrID(ctx, o.TypeID),
			TypeID:     o.TypeID,
			Side:       side,
			Price:      esi.FormatISK(o.Price),
			Volume:     fmt.Sprintf("%s / %s", esi.FormatInt(o.VolumeRemain), esi.FormatInt(o.VolumeTotal)),
			Location:   app.deps.LinkPlaceMemo(ctx, placeMemo, o.LocationID, app.deps.CorpLocationTitle(ctx, o.LocationID, structureNames)),
			Region:     regionName(o.RegionID),
			Expires:    expires,
			IssuedBy:   app.deps.DisplayCharacter(ctx, o.IssuedBy),
			IssuedByID: o.IssuedBy,
		}
		if !o.IsBuyOrder {
			if h, ok := health[o.OrderID]; ok && h.CharacterID == o.IssuedBy {
				row.Status, row.Bad = app.deps.OrderHealthText(h.MyPrice, h.Status, h.StationBest, h.RegionBest)
			} else if ownChars[o.IssuedBy] {
				row.Status = "Not checked against the order book yet"
			}
		}
		rows = append(rows, row)
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

	app.deps.Render(ctx, w, http.StatusOK, "corp_orders.html", data)
}

// ---------------------------------------------------------------------------
// Corporation assets.
// ---------------------------------------------------------------------------

// corpAssetsView is the Corporation Assets page body (grouping and
// rows shared with the character Assets page).
type corpAssetsView struct {
	module.CorpViewBase
	module.CorpSectionState
	Stacks     int
	TotalItems int64
	Locations  any
}

func (app *mod) handleCorpAssets(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := &PageData{ShellData: app.deps.PageShell(ctx)}

	sel, ok, err := app.deps.PickCorpPage(ctx, r)
	if err != nil {
		logging.Errorf("corp assets: pick character: %v", err)
		data.Error = "Could not load corporation data; check the server log."
		app.deps.Render(ctx, w, http.StatusOK, "corp_assets.html", data)
		return
	}
	if !ok {
		app.deps.Render(ctx, w, http.StatusOK, "corp_assets.html", data)
		return
	}
	data.CorpChars = sel.Links

	view := &corpAssetsView{CorpViewBase: sel.Base}
	data.CorpAssets = view

	var items []esi.Asset
	view.CorpSectionState = app.deps.CorpSection(ctx, sel.Active.CharacterID, esi.SnapCorpAssets, &items)
	if !view.Loaded {
		app.deps.Render(ctx, w, http.StatusOK, "corp_assets.html", data)
		return
	}

	view.Stacks = len(items)
	for _, it := range items {
		view.TotalItems += it.Quantity
	}

	// Player-given singleton names (worker-warmed) override type
	// names; the corp's own structures title their locations.
	// The lookup is batched over this page's item IDs only.
	overrides := make(map[int64]string)
	if len(items) > 0 {
		itemIDs := make([]int64, 0, len(items))
		for _, it := range items {
			itemIDs = append(itemIDs, it.ItemID)
		}
		if rows, err := app.queries.ListItemNamesByIDs(ctx, itemIDs); err != nil {
			logging.Errorf("corp assets: list item names: %v", err)
		} else {
			for _, row := range rows {
				overrides[row.ItemID] = row.Name
			}
		}
	}
	structureTitles := app.deps.CorpStructureNames(ctx, sel.Active.CharacterID)

	view.Locations = app.deps.AssetLocations(ctx, items, structureTitles, overrides)

	app.deps.Render(ctx, w, http.StatusOK, "corp_assets.html", data)
}

// ---------------------------------------------------------------------------
// Corporation structures.
// ---------------------------------------------------------------------------

// corpStructureRow is one structure line.
type corpStructureRow struct {
	Name     string
	LinkID   int64 // structure id when Name is a resolved name (links to its page); 0 for the "#<id>" fallback
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
	module.CorpViewBase
	module.CorpSectionState
	Rows []corpStructureRow
}

func (app *mod) handleCorpStructures(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := &PageData{ShellData: app.deps.PageShell(ctx)}

	sel, ok, err := app.deps.PickCorpPage(ctx, r)
	if err != nil {
		logging.Errorf("corp structures: pick character: %v", err)
		data.Error = "Could not load corporation data; check the server log."
		app.deps.Render(ctx, w, http.StatusOK, "corp_structures.html", data)
		return
	}
	if !ok {
		app.deps.Render(ctx, w, http.StatusOK, "corp_structures.html", data)
		return
	}
	data.CorpChars = sel.Links

	view := &corpStructuresView{CorpViewBase: sel.Base}
	data.CorpStructs = view

	var structures esi.CorpStructures
	view.CorpSectionState = app.deps.CorpSection(ctx, sel.Active.CharacterID, esi.SnapCorpStructures, &structures)
	if !view.Loaded {
		app.deps.Render(ctx, w, http.StatusOK, "corp_structures.html", data)
		return
	}

	rows := make([]corpStructureRow, 0, len(structures))
	for _, s := range structures {
		row := corpStructureRow{
			Name:   s.Name,
			Type:   app.deps.TypeNameOrID(ctx, s.TypeID),
			TypeID: s.TypeID,
			State:  humanizeEnum(s.State),
		}
		if row.Name == "" {
			if name := app.deps.ResolvedStructureTitle(ctx, s.StructureID); name != "" {
				row.Name = name
			} else {
				row.Name = fmt.Sprintf("Structure #%d", s.StructureID)
			}
		}
		if row.Name != fmt.Sprintf("Structure #%d", s.StructureID) {
			row.LinkID = s.StructureID
		}
		row.System = app.deps.LocationTitle(ctx, s.SystemID, "solar_system")
		if s.FuelExpires != "" {
			row.Fuel = humanize.Finish(s.FuelExpires)
			if t, err := time.Parse(time.RFC3339, s.FuelExpires); err == nil {
				if left := time.Until(t); left > 0 {
					row.FuelLeft = "in " + humanize.Duration(left)
				} else {
					row.FuelLeft = "expired"
				}
			}
		}
		if strings.HasSuffix(s.State, "_reinforce") && s.StateTimerEnd != "" {
			row.Timer = humanize.Finish(s.StateTimerEnd)
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

	app.deps.Render(ctx, w, http.StatusOK, "corp_structures.html", data)
}

// memberGroups fills in what a director needs on the members list to
// put members in the corporation's groups: the groups, which members
// are linked here, and which groups each is already in.
func (app *mod) memberGroups(ctx context.Context, userID int64, view *corpMembersView) {
	owner := standing.Owner{Kind: standing.OwnerCorporation, ID: view.CorpID}
	if !standing.Includes(app.deps.ManageableOwners(ctx, userID), owner) {
		return
	}
	view.CanGroup = true
	linked := map[int64]bool{}
	if rows, err := app.queries.ListCharactersInCorporations(ctx, []int64{view.CorpID}); err == nil {
		for _, row := range rows {
			linked[row.CharacterID] = true
		}
	}
	in := map[int64][]string{}
	groups, err := app.queries.ListOrgGroupsForOwner(ctx, db.ListOrgGroupsForOwnerParams{OwnerKind: owner.Kind, OwnerID: owner.ID})
	if err != nil {
		logging.Errorf("groups: groups of %s: %v", owner.Key(), err)
	}
	for _, group := range groups {
		view.Groups = append(view.Groups, groupChoice{ID: group.ID, Name: group.Name})
		members, _ := app.queries.ListOrgGroupMembers(ctx, group.ID)
		for _, m := range members {
			in[m.CharacterID] = append(in[m.CharacterID], group.Name)
		}
	}
	for i := range view.Rows {
		row := &view.Rows[i]
		row.Linked = linked[row.ID]
		row.Groups = strings.Join(in[row.ID], ", ")
	}
}

// groupChoice is one group in the members list's "add to" picker.
type groupChoice struct {
	ID   int64
	Name string
}
