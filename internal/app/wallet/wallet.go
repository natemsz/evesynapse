package wallet

import (
	"context"
	"evesynapse/internal/app/module"
	"evesynapse/internal/humanize"
	"net/http"
	"strconv"
	"time"

	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

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
	ID           int64
	Href         string // opens this trade in the inspector
	On           bool
	Date         string
	Item         string
	TypeID       int64
	Qty          string
	Unit         string
	Total        string
	Side         string // "Buy" | "Sell"
	Loc          module.PlaceRef
	With         string // counterparty display
	ClientID     int64
	ClientIsChar bool // counterparty resolved as a character
	ClientIsCorp bool // counterparty resolved as a corporation (else text)
}

// walletView is the Wallet page body.
type walletView struct {
	CharacterName string
	CharacterID   int64
	BalanceOK     bool
	Balance       string // esi.FormatISK, bare number
	Graph         any
	Flow          any // the home Wallet Flow module, for this character alone
	Journal       module.EconSectionState
	JournalRows   []walletJournalRow
	JournalCut    int
	Txns          module.EconSectionState
	TxnRows       []walletTxnRow
	TxnsCut       int

	// The summary, the flow, the filters and the open trade
	// (shape.go).
	Tiles         []walletTile
	Ranges        []walletChip
	FormatHref    string
	FormatLabel   string
	Sources       []walletFlowLine
	Sinks         []walletFlowLine
	InTotal       string
	OutTotal      string
	Kept          string
	BestSeller    string
	BestSellerID  int64
	BestSellerISK string
	Chips         []walletChip
	RefLabel      string
	RefClear      string
	Filtered      bool
	ClearHref     string
	JournalCSV    string
	TxnsCSV       string
	Trade         *walletTrade
	Depth         int // ledger console: 1 the ledgers, 2 one trade
}

func (app *mod) handleWallet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := &PageData{ShellData: app.deps.PageShell(ctx)}

	_, active, links, err := app.deps.PickCharacter(ctx, r, "/wallet/")
	if err != nil {
		logging.Errorf("wallet: list characters: %v", err)
		data.Error = "Could not load wallet data; check the server log."
		app.deps.Render(ctx, w, http.StatusOK, "wallet.html", data)
		return
	}
	if links == nil {
		app.deps.Render(ctx, w, http.StatusOK, "wallet.html", data)
		return
	}
	data.WalletChars = links

	view := &walletView{CharacterName: active.Name, CharacterID: active.CharacterID, Depth: 1}
	data.Wallet = view
	choice := parseWalletChoice(r, time.Now().UTC())
	if choice.character == "" {
		choice.character = strconv.FormatInt(active.CharacterID, 10)
	}

	// Balance header reuses the existing wallet snapshot.
	var balance float64
	if app.deps.LoadCorpSnapshot(ctx, active.CharacterID, esi.SnapWallet, &balance) {
		view.Balance = esi.FormatISK(balance)
		view.BalanceOK = true
	}

	var journal esi.WalletJournal
	view.Journal = app.deps.EconSection(ctx, active.CharacterID, esi.SnapWalletJournal, &journal)
	if view.Journal.Loaded {
		for _, e := range journal {
			if !inRange(e.Date, choice.cutoff) || !journalMatches(e, choice.cat, choice.ref) {
				continue
			}
			if len(view.JournalRows) >= maxLedgerRows {
				view.JournalCut++
				continue
			}
			row := walletJournalRow{
				Date:    humanize.Finish(e.Date),
				Type:    app.deps.HumanizeEnum(e.RefType),
				Amount:  choice.isk(e.Amount),
				Balance: choice.isk(e.Balance),
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
	view.Txns = app.deps.EconSection(ctx, active.CharacterID, esi.SnapWalletTxns, &txns)
	if view.Txns.Loaded {
		for _, t := range txns {
			if !inRange(t.Date, choice.cutoff) || choice.cat == "big" && t.UnitPrice*float64(t.Quantity) < walletBigISK {
				continue
			}
			if len(view.TxnRows) >= maxLedgerRows {
				view.TxnsCut++
				continue
			}
			side := "Sell"
			if t.IsBuy {
				side = "Buy"
			}
			with, clientIsChar, clientIsCorp := app.deps.TxnCounterparty(ctx, t.ClientID)
			row := walletTxnRow{
				ID:           t.TransactionID,
				Href:         choice.href("/wallet/", map[string]string{"txn": strconv.FormatInt(t.TransactionID, 10)}) + "#wl-txns",
				On:           t.TransactionID == choice.txn,
				Date:         humanize.Finish(t.Date),
				Item:         app.deps.TypeNameOrID(ctx, t.TypeID),
				TypeID:       t.TypeID,
				Qty:          esi.FormatInt(t.Quantity),
				Unit:         choice.isk(t.UnitPrice),
				Total:        choice.isk(t.UnitPrice * float64(t.Quantity)),
				Side:         side,
				Loc:          app.deps.LinkPlace(ctx, t.LocationID, app.deps.EconLocationTitle(ctx, t.LocationID)),
				With:         with,
				ClientID:     t.ClientID,
				ClientIsChar: clientIsChar,
				ClientIsCorp: clientIsCorp,
			}
			view.TxnRows = append(view.TxnRows, row)
			if row.On {
				app.shapeTrade(view, choice, journal, txns, row, t)
			}
		}
	}

	// The account's total across characters, from each wallet
	// snapshot (one read per character on the account).
	var total float64
	for _, l := range links {
		if l.ID == active.CharacterID {
			total += balance
			continue
		}
		var b float64
		if app.deps.LoadCorpSnapshot(ctx, l.ID, esi.SnapWallet, &b) {
			total += b
		}
	}
	if view.Journal.Loaded {
		app.shapeTiles(view, choice, journal, balance, view.BalanceOK, total, len(links))
		app.shapeFlow(ctx, view, choice, journal, txns)
		app.shapeChips(view, choice, journal)
	} else {
		app.shapeTiles(view, choice, nil, balance, view.BalanceOK, total, len(links))
		app.shapeChips(view, choice, nil)
	}

	// Balance history rides the same stored rows: the decoded
	// journal above, the daily samples, and the wallet snapshot
	// the header balance came from.
	view.Graph = app.deps.AttachWalletGraph(ctx, active.UserID, active.CharacterID, journal, view.Journal.Loaded, r.URL.Query().Get("range"))
	// Wallet flow, as the home module reads it, for this character:
	// the same stored journal window, one bundle.
	view.Flow = app.deps.WalletFlow(ctx, active)

	app.deps.Render(ctx, w, http.StatusOK, "wallet.html", data)
}

// journalParty resolves one journal counterparty to its display
// name and kind, so the template links it by the name policy.
// The kind comes from ESI's party_type when the snapshot carries
// it; older snapshots fall back to the local name tiers — a
// character hit means character, a corporation hit corporation,
// and anything else renders through the character fallback (text
// whenever it never resolves to a linkable name).
func (app *mod) journalParty(ctx context.Context, id int64, kind string) (name string, partyKind string) {
	switch kind {
	case "character":
		return app.deps.DisplayCharacter(ctx, id), "character"
	case "corporation":
		return app.deps.CorpDisplayName(ctx, id), "corporation"
	case "alliance":
		return app.deps.AllianceDisplayName(ctx, id), "alliance"
	case "":
		if _, ok := app.deps.CachedCharacterName(id); ok {
			return app.deps.DisplayCharacter(ctx, id), "character"
		}
		if name, ok := app.deps.CachedCorpName(id); ok && name != "" {
			return name, "corporation"
		}
		return app.deps.DisplayCharacter(ctx, id), ""
	default:
		return app.deps.DisplayCharacter(ctx, id), ""
	}
}

// handleWalletGraphFragment re-renders just the wallet page's
// balance-history section from stored rows, so the graph fills
// in when the journal snapshot lands instead of waiting for a
// manual refresh. Cache-only, like every fragment.
func (app *mod) handleWalletGraphFragment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_, active, links, err := app.deps.PickCharacter(ctx, r, "/wallet/")
	if err != nil || links == nil {
		app.deps.RenderFragment(w, "wallet.html", "wallet-graph", app.deps.EmptyWalletGraph())
		return
	}
	var journal esi.WalletJournal
	loaded := app.deps.LoadCorpSnapshot(ctx, active.CharacterID, esi.SnapWalletJournal, &journal)
	if !loaded {
		journal = nil
	}
	app.deps.RenderFragment(w, "wallet.html", "wallet-graph",
		app.deps.AttachWalletGraph(ctx, active.UserID, active.CharacterID, journal, loaded, r.URL.Query().Get("range")))
}

// maxLedgerRows caps the journal and transaction tables; the snapshot
// holds the newest page (up to 1,000 entries) but a page wants headlines,
// not a ledger dump.
const maxLedgerRows = 100
