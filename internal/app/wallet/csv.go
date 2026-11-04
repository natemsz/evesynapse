package wallet

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// The ledgers as CSV, with the page's range and journal filters:
// the stored windows only, every row in them (no 100-row cut).

func (app *mod) handleJournalCSV(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_, active, links, err := app.deps.PickCharacter(ctx, r, "/wallet/")
	if err != nil || links == nil {
		http.Error(w, "No character to export.", http.StatusNotFound)
		return
	}
	c := parseWalletChoice(r, time.Now().UTC())
	var journal esi.WalletJournal
	if !app.deps.LoadCorpSnapshot(ctx, active.CharacterID, esi.SnapWalletJournal, &journal) {
		http.Error(w, "The journal hasn't synced yet.", http.StatusNotFound)
		return
	}
	cw := startCSV(w, fmt.Sprintf("journal-%d.csv", active.CharacterID))
	_ = cw.Write([]string{"Date", "Type", "Amount", "Balance", "First party", "Second party", "Description", "Reason", "Journal ID"})
	for _, e := range journal {
		if !inRange(e.Date, c.cutoff) || !journalMatches(e, c.cat, c.ref) {
			continue
		}
		from, _ := app.journalParty(ctx, e.FirstPartyID, e.FirstPartyType)
		to, _ := app.journalParty(ctx, e.SecondPartyID, e.SecondPartyType)
		if e.FirstPartyID == 0 {
			from = ""
		}
		if e.SecondPartyID == 0 {
			to = ""
		}
		_ = cw.Write([]string{e.Date, e.RefType, num(e.Amount), num(e.Balance), cell(from), cell(to), cell(e.Description), cell(e.Reason), strconv.FormatInt(e.ID, 10)})
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		logging.Errorf("wallet: journal csv: %v", err)
	}
}

func (app *mod) handleTxnsCSV(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_, active, links, err := app.deps.PickCharacter(ctx, r, "/wallet/")
	if err != nil || links == nil {
		http.Error(w, "No character to export.", http.StatusNotFound)
		return
	}
	c := parseWalletChoice(r, time.Now().UTC())
	var txns esi.WalletTransactions
	if !app.deps.LoadCorpSnapshot(ctx, active.CharacterID, esi.SnapWalletTxns, &txns) {
		http.Error(w, "Transactions haven't synced yet.", http.StatusNotFound)
		return
	}
	cw := startCSV(w, fmt.Sprintf("transactions-%d.csv", active.CharacterID))
	_ = cw.Write([]string{"Date", "Item", "Type ID", "Side", "Quantity", "Unit price", "Total", "Location", "Counterparty", "Transaction ID"})
	for _, t := range txns {
		total := t.UnitPrice * float64(t.Quantity)
		if !inRange(t.Date, c.cutoff) || c.cat == "big" && total < walletBigISK {
			continue
		}
		side := "sell"
		if t.IsBuy {
			side = "buy"
		}
		with, _, _ := app.deps.TxnCounterparty(ctx, t.ClientID)
		_ = cw.Write([]string{t.Date, cell(app.deps.TypeNameOrID(ctx, t.TypeID)), strconv.FormatInt(t.TypeID, 10), side,
			strconv.FormatInt(t.Quantity, 10), num(t.UnitPrice), num(total),
			cell(app.deps.EconLocationTitle(ctx, t.LocationID)), cell(with), strconv.FormatInt(t.TransactionID, 10)})
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		logging.Errorf("wallet: transactions csv: %v", err)
	}
}

func startCSV(w http.ResponseWriter, name string) *csv.Writer {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	return csv.NewWriter(w)
}

func num(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }

// cell keeps a text cell from reading as a formula in a spreadsheet.
func cell(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}
