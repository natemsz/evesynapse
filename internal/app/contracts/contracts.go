package contracts

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"evesynapse/internal/app/module"
	"evesynapse/internal/humanize"
	"fmt"
	"net/http"
	"sort"

	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
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
}

// contractsView is the Contracts page body.
type contractsView struct {
	CharacterName string
	Contracts     module.EconSectionState
	Rows          []contractRow
	Cut           int
}

// maxContractsShown caps the contract list; details warm into the
// store for the contracts shown.
const maxContractsShown = 100

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

	view := &contractsView{CharacterName: active.Name}
	data.Contracts = view

	var contracts esi.Contracts
	view.Contracts = app.deps.EconSection(ctx, active.CharacterID, esi.SnapContracts, &contracts)
	if !view.Contracts.Loaded {
		app.deps.Render(ctx, w, http.StatusOK, "contracts.html", data)
		return
	}

	// Newest issued first.
	sort.SliceStable(contracts, func(i, j int) bool { return contracts[i].DateIssued > contracts[j].DateIssued })
	if len(contracts) > maxContractsShown {
		view.Cut = len(contracts) - maxContractsShown
		contracts = contracts[:maxContractsShown]
	}

	for _, c := range contracts {
		row := contractRow{
			Title:      c.Title,
			Type:       app.deps.HumanizeEnum(c.Type),
			Status:     app.deps.HumanizeEnum(c.Status),
			IssuerID:   c.IssuerID,
			Price:      esi.FormatISK(c.Price),
			Reward:     esi.FormatISK(c.Reward),
			Collateral: esi.FormatISK(c.Collateral),
			Issued:     humanize.Finish(c.DateIssued),
			Expires:    humanize.Finish(c.DateExpired),
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
			row.Acceptor, row.AcceptorPending = app.contractCharLabel(ctx, c.AcceptorID)
			row.AcceptorID = c.AcceptorID
		}
		if c.Type == "courier" && (c.StartLocationID > 0 || c.EndLocationID > 0) {
			row.HasRoute = true
			row.RouteStart = app.deps.LinkPlace(ctx, c.StartLocationID, app.deps.EconLocationTitle(ctx, c.StartLocationID))
			row.RouteEnd = app.deps.LinkPlace(ctx, c.EndLocationID, app.deps.EconLocationTitle(ctx, c.EndLocationID))
		}

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
				case it.IsSingleton:
					note = "singleton"
				}
				row.Items = append(row.Items, contractItemRow{
					Name:   app.deps.TypeNameOrID(ctx, it.TypeID),
					TypeID: it.TypeID,
					Qty:    esi.FormatInt(it.Quantity),
					Note:   note,
				})
			}
		case c.Type == "item_exchange":
			row.ItemsNote = "Item details are still loading."
		}
		view.Rows = append(view.Rows, row)
	}

	app.deps.Render(ctx, w, http.StatusOK, "contracts.html", data)
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
		if errors.Is(err, sql.ErrNoRows) {
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
