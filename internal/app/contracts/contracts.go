package contracts

import (
	"context"
	"errors"
	"evesynapse/internal/app/module"
	"evesynapse/internal/humanize"
	"fmt"
	json "github.com/goccy/go-json"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------------------
// Contracts page: the character's contracts from the contracts
// snapshot. Item lists behind item-exchange contracts are warmed into
// the contract_details store in the background, and this page reads the
// store only. Auction bids are not fetched (issuer-only in ESI); the
// buyout is shown from the contract row.
// ---------------------------------------------------------------------------

// contractItemRow is one warmed item line of a contract.
type contractItemRow struct {
	Name   string
	TypeID int64
	Qty    string
	Note   string // "wanted", "BPO", "BPC", "singleton"
}

// contractRow is one contract line.
type contractRow struct {
	Title      string
	Type       string // humanized
	Status     string // humanized
	Issuer     string
	IssuerID   int64
	Assignee   string // "Public" for public contracts
	AssigneeID int64
	// AssigneeIsChar / AssigneeIsCorp say how the assignee
	// resolved: an assignee can be a character or a corporation,
	// and each links to its own public page (public stays text).
	AssigneeIsChar bool
	AssigneeIsCorp bool
	Acceptor       string // "" until accepted
	AcceptorID     int64
	AcceptorIsCorp bool // accepted on a corporation's behalf
	// Pending labels: the counterparty's name is still on its
	// way; the cell renders a live region that swaps it in.
	IssuerPending   bool
	AssigneePending bool
	AcceptorPending bool
	Price           string
	Reward          string
	Collateral      string
	RouteStart      module.PlaceRef // couriers only: pickup location
	RouteEnd        module.PlaceRef // couriers only: drop-off location
	HasRoute        bool            // couriers: either end known
	Issued          string
	Expires         string
	Items           []contractItemRow
	ItemsNote       string // "details warming" while the store lacks the list

	ID   int64
	Href string // this contract in the inspector
	On   bool
	// Tone colours the status pill: good while outstanding, info
	// while a courier is in progress, bad once expired or failed,
	// "" when finished.
	Tone string
	Kind string // ESI type: item_exchange, auction, courier, loan

	// The row's one money figure and what it is (Price, Reward),
	// with the second figure a type carries under it (a courier's
	// collateral, an auction's buyout).
	Value      string
	ValueLabel string
	ValueSub   string
	Volume     string // m3, couriers
	Days       int64  // days to complete, couriers
	// Left is the time until expiry ("in 6d 2h", "Expired");
	// IssuedOn, ExpiresOn, Accepted and Completed are dates.
	Left      string
	IssuedOn  string
	ExpiresOn string
	Accepted  string
	Completed string
	// Who: the pilot issued it, it is assigned to the pilot, or it
	// was issued for the corporation; the filter chips read these.
	Mine, ToMe, ForCorp bool
	// Preview is the first few items for the row's icon strip;
	// MoreItems counts the rest.
	Preview   []contractItemRow
	MoreItems int
}

// contractFilter is one filter chip over the list.
type contractFilter struct {
	Label string
	Href  string
	Count int
	On    bool
}

// contractsView is the Contracts page body.
type contractsView struct {
	CharacterName string
	Contracts     module.EconSectionState
	Rows          []contractRow
	Cut           int

	// The framework console: the contracts are its main pane and
	// ?contract= opens one in the inspector (depth 2).
	Depth    int
	ListHref string
	Insp     *contractRow

	// Kinds and Whose are the filter chips (?kind=, ?whose=); Total
	// is every contract shown before a filter.
	Kinds []contractFilter
	Whose []contractFilter
	Total int
}

// maxContractsShown caps the contract list; details warm into the
// store for the contracts shown.
const maxContractsShown = 100

// contractPreviewItems is how many items a row's icon strip shows.
const contractPreviewItems = 3

// contractShowOrder is the filter chips' order and labels: the
// contract types first, then whose contract it is.
var contractShowOrder = []struct{ key, label string }{
	{"item_exchange", "Item exchange"},
	{"courier", "Courier"},
	{"auction", "Auction"},
	{"loan", "Loan"},
	{"issued", "Issued by you"},
	{"assigned", "Assigned to you"},
	{"corp", "For the corporation"},
}

func validContractShow(show string) bool {
	for _, s := range contractShowOrder {
		if s.key == show {
			return true
		}
	}
	return false
}

// contractShows lists the filter keys a contract answers to.
func contractShows(row contractRow) []string {
	keys := []string{row.Kind}
	if row.Mine {
		keys = append(keys, "issued")
	}
	if row.ToMe {
		keys = append(keys, "assigned")
	}
	if row.ForCorp {
		keys = append(keys, "corp")
	}
	return keys
}

func containsShow(keys []string, show string) bool {
	for _, k := range keys {
		if k == show {
			return true
		}
	}
	return false
}

// contractFilters builds the chips: All plus every type, then every
// whose-filter, each with its count. A chip with nothing behind it
// stays off the bar unless it is the one in force.
func contractFilters(base, show string, total int, counts map[string]int, party contractParty) (kinds, whose []contractFilter) {
	kinds = append(kinds, contractFilter{Label: "All", Href: base, Count: total, On: show == ""})
	for i, s := range contractShowOrder {
		if counts[s.key] == 0 && show != s.key {
			continue
		}
		label := s.label
		if party.CorpID > 0 {
			label = strings.Replace(label, "you", "the corporation", 1)
		}
		f := contractFilter{Label: label, Href: base + "&show=" + s.key, Count: counts[s.key], On: show == s.key}
		if i < 4 {
			kinds = append(kinds, f)
		} else {
			whose = append(whose, f)
		}
	}
	return kinds, whose
}

// contractTone reads a contract's state: its pill tone, and the time
// left before it expires. ESI keeps an expired contract outstanding,
// so the date decides.
func contractTone(c esi.Contract, now time.Time) (tone, left string) {
	switch c.Status {
	case "outstanding":
		if t, err := time.Parse(time.RFC3339, c.DateExpired); err == nil {
			left = module.TimeLeft(t, now)
			if left == "Expired" {
				return "bad", left
			}
		}
		return "good", left
	case "in_progress":
		// A courier in progress is due its days to complete after
		// it was accepted.
		if t, err := time.Parse(time.RFC3339, c.DateAccepted); err == nil && c.DaysToComplete > 0 {
			if left = module.TimeLeft(t.Add(time.Duration(c.DaysToComplete)*24*time.Hour), now); left == "Expired" {
				left = "Overdue"
			} else {
				left = "Due " + left
			}
		}
		return "info", left
	case "rejected", "failed", "deleted", "reversed":
		return "bad", ""
	}
	return "", ""
}

// contractDate is an ESI timestamp's date, "" when unset.
func contractDate(raw string) string {
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return module.ShortDate(t)
	}
	return ""
}

func (app *mod) handleContracts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := &PageData{ShellData: app.deps.PageShell(ctx)}

	_, active, links, err := app.deps.PickCharacter(ctx, r, "/contracts/")
	if err != nil {
		logging.Errorf("contracts: list characters: %v", err)
		data.Error = "Could not load contract data; check the server log."
		app.deps.Render(ctx, w, http.StatusOK, "contracts.html", data)
		return
	}
	if links == nil {
		app.deps.Render(ctx, w, http.StatusOK, "contracts.html", data)
		return
	}
	data.ContractsChars = links

	view := &contractsView{CharacterName: active.Name, Depth: 1}
	base := "/contracts/?character=" + strconv.FormatInt(active.CharacterID, 10)
	show := r.URL.Query().Get("show")
	if !validContractShow(show) {
		show = ""
	}
	view.ListHref = base
	if show != "" {
		view.ListHref += "&show=" + show
	}
	picked, _ := strconv.ParseInt(r.URL.Query().Get("contract"), 10, 64)
	data.Contracts = view

	var contracts esi.Contracts
	view.Contracts = app.deps.EconSection(ctx, active.CharacterID, esi.SnapContracts, &contracts)
	if !view.Contracts.Loaded {
		app.deps.Render(ctx, w, http.StatusOK, "contracts.html", data)
		return
	}

	app.fillContracts(ctx, view, contracts, contractParty{CharacterID: active.CharacterID}, base, show, picked)
	app.deps.Render(ctx, w, http.StatusOK, "contracts.html", data)
}

// corpContractsView is the corporation Contracts page's head and gate
// state; the list itself is a contractsView like the pilot page's.
type corpContractsView struct {
	module.CorpViewBase
	module.CorpSectionState
}

// handleCorpContracts is the corporation's contracts: the contracts
// the corporation issued or is assigned, from the corp_contracts
// snapshot the worker keeps for the viewing character, in the same
// console as the pilot's own.
func (app *mod) handleCorpContracts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := &PageData{ShellData: app.deps.PageShell(ctx)}

	sel, ok, err := app.deps.PickCorpPage(ctx, r)
	if err != nil {
		logging.Errorf("corp contracts: pick character: %v", err)
		data.Error = "Could not load corporation data; check the server log."
		app.deps.Render(ctx, w, http.StatusOK, "corp_contracts.html", data)
		return
	}
	if !ok {
		app.deps.Render(ctx, w, http.StatusOK, "corp_contracts.html", data)
		return
	}
	data.CorpChars = sel.Links
	head := &corpContractsView{CorpViewBase: sel.Base}
	data.CorpContracts = head

	var contracts esi.Contracts
	head.CorpSectionState = app.deps.CorpSection(ctx, sel.Active.CharacterID, esi.SnapCorpContracts, &contracts)
	if !head.Loaded {
		app.deps.Render(ctx, w, http.StatusOK, "corp_contracts.html", data)
		return
	}

	view := &contractsView{CharacterName: sel.Base.CorpTitle, Depth: 1, Contracts: module.EconSectionState{Loaded: true, FetchedAt: head.FetchedAt}}
	base := "/corporations/contracts/?character=" + strconv.FormatInt(sel.Active.CharacterID, 10)
	show := r.URL.Query().Get("show")
	if !validContractShow(show) {
		show = ""
	}
	view.ListHref = base
	if show != "" {
		view.ListHref += "&show=" + show
	}
	picked, _ := strconv.ParseInt(r.URL.Query().Get("contract"), 10, 64)
	data.Contracts = view
	app.fillContracts(ctx, view, contracts, contractParty{CorpID: sel.Base.CorpID}, base, show, picked)
	app.deps.Render(ctx, w, http.StatusOK, "corp_contracts.html", data)
}

// contractParty is whose contracts a page lists: one pilot's
// (CharacterID) or their corporation's (CorpID).
type contractParty struct {
	CharacterID int64
	CorpID      int64
}

// whose reads a contract from the party's side: issued by it,
// assigned to (or accepted by) it, and, on a pilot's page, issued
// for the corporation.
func (p contractParty) whose(c esi.Contract) (mine, toMe, forCorp bool) {
	if p.CorpID > 0 {
		return c.ForCorporation && c.IssuerCorporationID == p.CorpID, c.AssigneeID == p.CorpID, false
	}
	return c.IssuerID == p.CharacterID, c.AssigneeID == p.CharacterID || c.AcceptorID == p.CharacterID, c.ForCorporation
}

// fillContracts builds a contracts console from one stored contracts
// list: the newest first, capped, filtered by show, with the picked
// contract open in the inspector. base is the page's own list URL
// (the filter chips add to it); view.ListHref must already carry the
// filter in force.
func (app *mod) fillContracts(ctx context.Context, view *contractsView, contracts esi.Contracts, party contractParty, base, show string, picked int64) {
	// Newest issued first.
	sort.SliceStable(contracts, func(i, j int) bool { return contracts[i].DateIssued > contracts[j].DateIssued })
	if len(contracts) > maxContractsShown {
		view.Cut = len(contracts) - maxContractsShown
		contracts = contracts[:maxContractsShown]
	}

	now := time.Now()
	counts := make(map[string]int)
	var rows []contractRow
	for _, c := range contracts {
		row := contractRow{
			Title:      c.Title,
			Type:       app.deps.HumanizeEnum(c.Type),
			Kind:       c.Type,
			Status:     app.deps.HumanizeEnum(c.Status),
			IssuerID:   c.IssuerID,
			Price:      esi.FormatISK(c.Price),
			Reward:     esi.FormatISK(c.Reward),
			Collateral: esi.FormatISK(c.Collateral),
			Issued:     humanize.Finish(c.DateIssued),
			Expires:    humanize.Finish(c.DateExpired),
			IssuedOn:   contractDate(c.DateIssued),
			ExpiresOn:  contractDate(c.DateExpired),
			Accepted:   contractDate(c.DateAccepted),
			Completed:  contractDate(c.DateCompleted),
			ID:         c.ContractID,
		}
		row.Mine, row.ToMe, row.ForCorp = party.whose(c)
		row.Tone, row.Left = contractTone(c, now)
		if row.Left == "Expired" {
			row.Status = "Expired"
		}
		switch c.Type {
		case "courier":
			row.Value, row.ValueLabel = row.Reward, "Reward"
			if c.Collateral > 0 {
				row.ValueSub = "collateral " + humanize.ISKCompact(c.Collateral)
			}
			if c.Volume > 0 {
				row.Volume = esi.FormatInt(int64(math.Round(c.Volume))) + " m³"
			}
			row.Days = c.DaysToComplete
		case "auction":
			row.Value, row.ValueLabel = row.Price, "Starting bid"
			if c.Buyout > 0 {
				row.ValueSub = "buyout " + humanize.ISKCompact(c.Buyout)
			}
		default:
			row.Value, row.ValueLabel = row.Price, "Price"
		}
		row.Issuer, row.IssuerPending = app.contractCharLabel(ctx, c.IssuerID)
		if row.Title == "" {
			row.Title = "Untitled"
		}
		if c.AssigneeID > 0 {
			row.AssigneeID = c.AssigneeID
			// An assignee can be a character or a corporation;
			// resolve whichever tier knows the ID and link
			// accordingly. While neither tier knows it, the cell
			// polls the character label like the issuer does.
			name, isChar, isCorp := app.deps.TxnCounterparty(ctx, c.AssigneeID)
			row.Assignee = name
			row.AssigneeIsChar = isChar
			row.AssigneeIsCorp = isCorp
			if !isChar && !isCorp {
				_, row.AssigneePending = app.contractCharLabel(ctx, c.AssigneeID)
			}
		} else if c.Availability == "public" {
			row.Assignee = "Public"
		}
		if c.AcceptorID > 0 {
			row.AcceptorID = c.AcceptorID
			// A contract accepted on a corporation's behalf names
			// the corporation; resolve it like the assignee.
			if name, _, isCorp := app.deps.TxnCounterparty(ctx, c.AcceptorID); isCorp {
				row.Acceptor, row.AcceptorIsCorp = name, true
			} else {
				row.Acceptor, row.AcceptorPending = app.contractCharLabel(ctx, c.AcceptorID)
			}
		}
		if c.Type == "courier" && (c.StartLocationID > 0 || c.EndLocationID > 0) {
			row.HasRoute = true
			row.RouteStart = app.deps.LinkPlace(ctx, c.StartLocationID, app.deps.EconLocationTitle(ctx, c.StartLocationID))
			row.RouteEnd = app.deps.LinkPlace(ctx, c.EndLocationID, app.deps.EconLocationTitle(ctx, c.EndLocationID))
		}

		for _, key := range contractShows(row) {
			counts[key]++
		}
		if show != "" && !containsShow(contractShows(row), show) {
			continue
		}
		row.Href = view.ListHref + "&contract=" + strconv.FormatInt(c.ContractID, 10)
		row.On = picked > 0 && c.ContractID == picked

		// Warmed item list from the detail store (item-exchange
		// contracts mainly; other types carry lists too).
		items, ok, err := app.loadContractItems(ctx, c.ContractID)
		if err != nil {
			logging.Errorf("contracts: read items for contract %d: %v", c.ContractID, err)
		}
		switch {
		case ok:
			for _, it := range items {
				note := ""
				switch {
				case !it.IsIncluded:
					note = "wanted"
				case it.RawQuantity == -1:
					note = "BPO"
				case it.RawQuantity == -2:
					note = "BPC"
				}
				row.Items = append(row.Items, contractItemRow{
					Name:   app.deps.TypeNameOrID(ctx, it.TypeID),
					TypeID: it.TypeID,
					Qty:    esi.FormatInt(it.Quantity),
					Note:   note,
				})
			}
			row.Preview = row.Items
			if len(row.Preview) > contractPreviewItems {
				row.Preview = row.Preview[:contractPreviewItems]
				row.MoreItems = len(row.Items) - contractPreviewItems
			}
		case c.Type == "item_exchange":
			row.ItemsNote = "Item details are still loading."
		}
		rows = append(rows, row)
	}
	view.Rows = rows
	view.Total = len(contracts)
	for i := range view.Rows {
		if view.Rows[i].On {
			view.Insp = &view.Rows[i]
			view.Depth = 2
		}
	}
	view.Kinds, view.Whose = contractFilters(base, show, view.Total, counts, party)

}

// contractCharLabel resolves one counterparty label for the
// contracts table: the name when a local tier has it, and a
// pending flag while it is still on its way (the cell then
// renders a live region). Rendering the label on a page leaves a
// viewed-priority want behind via displayCharacter.
func (app *mod) contractCharLabel(ctx context.Context, characterID int64) (string, bool) {
	name := app.deps.DisplayCharacter(ctx, characterID)
	if name == fmt.Sprintf("Character #%d", characterID) && app.deps.CharacterLabelPending(ctx, characterID) {
		return name, true
	}
	return name, false
}

// loadContractItems reads one contract's warmed item list from
// contract_details. ok=false means the worker has not warmed it
// yet (or the contract has no list stored).
func (app *mod) loadContractItems(ctx context.Context, contractID int64) (esi.ContractItems, bool, error) {
	row, err := app.queries.GetContractDetail(ctx, contractID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, err
	}
	var items esi.ContractItems
	if err := json.Unmarshal([]byte(row.Payload), &items); err != nil {
		return nil, false, fmt.Errorf("decode contract %d items: %w", contractID, err)
	}
	return items, true, nil
}
