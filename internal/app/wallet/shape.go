package wallet

import (
	"context"
	"fmt"
	"maps"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"evesynapse/internal/esi"
	"evesynapse/internal/humanize"
)

// ---------------------------------------------------------------------------
// The Wallet page's choices: the time range, the ISK format, the
// journal's quick filters, and the open trade. Everything reads the
// stored journal and transaction windows (ESI keeps 30 days of
// journal); nothing here calls ESI.
// ---------------------------------------------------------------------------

// walletBigISK is the "over 50M" filter's floor.
const walletBigISK = 50_000_000

// walletChoice is the page's parsed query.
type walletChoice struct {
	character string
	rng       string // "7d" | "30d"
	exact     bool   // ISK in full; false is compact
	cat       string // "" | market | fees | industry | income | big
	ref       string // one ref_type, from a source or sink
	txn       int64  // the open trade
	cutoff    time.Time
}

func parseWalletChoice(r *http.Request, now time.Time) walletChoice {
	q := r.URL.Query()
	c := walletChoice{character: q.Get("character"), rng: "30d", exact: q.Get("isk") != "short"}
	if q.Get("range") == "7d" {
		c.rng = "7d"
	}
	switch q.Get("cat") {
	case "market", "fees", "industry", "income", "big":
		c.cat = q.Get("cat")
	}
	c.ref = strings.TrimSpace(q.Get("ref"))
	if len(c.ref) > 64 {
		c.ref = ""
	}
	c.txn, _ = strconv.ParseInt(q.Get("txn"), 10, 64)
	days := 30
	if c.rng == "7d" {
		days = 7
	}
	c.cutoff = now.Add(-time.Duration(days) * 24 * time.Hour)
	return c
}

// href is the page with the current choices and the given changes
// ("" removes a choice). The trade stays closed unless set.
func (c walletChoice) href(base string, set map[string]string) string {
	cur := map[string]string{"character": c.character, "ref": c.ref, "cat": c.cat}
	if c.rng != "30d" {
		cur["range"] = c.rng
	}
	if !c.exact {
		cur["isk"] = "short"
	}
	maps.Copy(cur, set)
	v := url.Values{}
	for k, val := range cur {
		if val != "" {
			v.Set(k, val)
		}
	}
	if enc := v.Encode(); enc != "" {
		return base + "?" + enc
	}
	return base
}

// isk formats an amount in the chosen form.
func (c walletChoice) isk(v float64) string {
	if c.exact {
		return esi.FormatISK(v)
	}
	return humanize.ISKCompact(v)
}

// refCategory groups a journal ref type for the quick filters.
func refCategory(ref string) string {
	switch {
	case ref == "market_transaction" || ref == "market_escrow":
		return "market"
	case ref == "brokers_fee" || ref == "transaction_tax" || strings.HasSuffix(ref, "_tax") ||
		ref == "contract_brokers_fee" || ref == "contract_sales_tax":
		return "fees"
	case strings.HasPrefix(ref, "industry_") || ref == "manufacturing" || ref == "reaction" ||
		strings.HasPrefix(ref, "researching_") || ref == "copying" || strings.HasPrefix(ref, "planetary_"):
		return "industry"
	case strings.HasPrefix(ref, "bounty_") || strings.HasPrefix(ref, "agent_mission_") ||
		ref == "ess_escrow_transfer" || ref == "project_discovery_reward" || ref == "daily_goal_payouts" ||
		ref == "corporate_reward_payout":
		return "income"
	}
	return ""
}

func journalMatches(e esi.WalletJournalEntry, cat, ref string) bool {
	if ref != "" && e.RefType != ref {
		return false
	}
	switch cat {
	case "":
		return true
	case "big":
		return math.Abs(e.Amount) >= walletBigISK
	}
	return refCategory(e.RefType) == cat
}

// walletTile is one summary tile.
type walletTile struct {
	Label string
	Value string
	Sub   string
	Tone  string // "ok" | "warn" | ""
}

// walletChip is one filter chip.
type walletChip struct {
	Label string
	Count int
	Href  string
	On    bool
}

// walletFlowLine is one source or sink in the chosen range.
type walletFlowLine struct {
	Label    string
	Total    string
	Pct      int
	PctClass string // pw-N width class
	Href     string
	On       bool
}

// walletTrade is the open trade in the inspector.
type walletTrade struct {
	Row        walletTxnRow
	Date       string
	Tax        string // matched sales tax, "" when none
	TaxNote    string
	Net        string
	NetLabel   string
	BalanceAt  string // the wallet balance after the trade, from its journal entry
	ItemBuys   string // "3 buys, 1,200 units at 5.10 average"
	ItemSells  string
	Spread     string // average sell over average buy, in this window
	WithTrades string // "4 trades, 12.4M"
	CloseHref  string
	Copy       string
}

// shapeTiles fills the summary tiles from the journal in range.
func (app *mod) shapeTiles(view *walletView, c walletChoice, journal esi.WalletJournal, balance float64, balanceOK bool, total float64, chars int) {
	var in, out, broker, tax float64
	for _, e := range journal {
		if !inRange(e.Date, c.cutoff) {
			continue
		}
		if e.Amount > 0 {
			in += e.Amount
		} else {
			out += e.Amount
		}
		if refCategory(e.RefType) == "fees" && e.Amount < 0 {
			if e.RefType == "brokers_fee" || e.RefType == "contract_brokers_fee" {
				broker -= e.Amount
			} else {
				tax -= e.Amount
			}
		}
	}
	span := "30 days"
	if c.rng == "7d" {
		span = "7 days"
	}
	if balanceOK {
		view.Tiles = append(view.Tiles, walletTile{Label: "Balance", Value: humanize.ISKCompact(balance), Sub: esi.FormatISK(balance) + " ISK"})
	}
	net := in + out
	netVal := humanize.ISKCompact(net)
	tone := "warn"
	if net >= 0 {
		netVal = "+" + netVal
		tone = "ok"
	}
	view.Tiles = append(view.Tiles,
		walletTile{Label: "Net flow, " + span, Value: netVal, Tone: tone,
			Sub: "in +" + humanize.ISKCompact(in) + ", out " + humanize.ISKCompact(out)})
	feeSub := "broker " + humanize.ISKCompact(broker) + ", taxes " + humanize.ISKCompact(tax)
	if in > 0 {
		feeSub += fmt.Sprintf(", %.1f%% of income", (broker+tax)/in*100)
	}
	view.Tiles = append(view.Tiles, walletTile{Label: "Fees and taxes, " + span, Value: humanize.ISKCompact(broker + tax), Sub: feeSub})
	if chars > 1 && total > 0 {
		sub := fmt.Sprintf("%d characters", chars)
		if balanceOK {
			sub = fmt.Sprintf("%d characters, %.0f%% held here", chars, balance/total*100)
		}
		view.Tiles = append(view.Tiles, walletTile{Label: "All characters", Value: humanize.ISKCompact(total), Sub: sub})
	}
}

// shapeFlow fills the sources and sinks in range, each a link that
// filters the journal to its ref type.
func (app *mod) shapeFlow(ctx context.Context, view *walletView, c walletChoice, journal esi.WalletJournal, txns esi.WalletTransactions) {
	in, out := map[string]float64{}, map[string]float64{}
	var inSum, outSum float64
	for _, e := range journal {
		if !inRange(e.Date, c.cutoff) || e.Amount == 0 {
			continue
		}
		if e.Amount > 0 {
			in[e.RefType] += e.Amount
			inSum += e.Amount
		} else {
			out[e.RefType] -= e.Amount
			outSum -= e.Amount
		}
	}
	lines := func(m map[string]float64, sum float64, sign string) []walletFlowLine {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			if m[keys[i]] != m[keys[j]] {
				return m[keys[i]] > m[keys[j]]
			}
			return keys[i] < keys[j]
		})
		var outL []walletFlowLine
		for i, k := range keys {
			if i >= 6 && k != c.ref {
				continue
			}
			f := m[k] / sum
			l := walletFlowLine{Label: app.deps.HumanizeEnum(k), Total: sign + humanize.ISKCompact(m[k]),
				Pct: int(math.Round(f * 100)), PctClass: fmt.Sprintf("pw-%d", int(math.Round(f*100))),
				Href: c.href("/wallet/", map[string]string{"ref": k, "cat": ""}) + "#wl-journal", On: k == c.ref}
			if l.On {
				l.Href = c.href("/wallet/", map[string]string{"ref": ""}) + "#wl-journal"
			}
			outL = append(outL, l)
		}
		return outL
	}
	view.Sources = lines(in, inSum, "+")
	view.Sinks = lines(out, outSum, "-")
	view.InTotal = "+" + humanize.ISKCompact(inSum)
	view.OutTotal = "-" + humanize.ISKCompact(outSum)
	if inSum > 0 {
		kept := inSum - outSum
		view.Kept = fmt.Sprintf("%.1f%% of income kept (%s ISK)", kept/inSum*100, signed(kept))
	}
	// The best seller: the item that brought in the most from sales.
	sold := map[int64]float64{}
	for _, t := range txns {
		if !t.IsBuy && inRange(t.Date, c.cutoff) {
			sold[t.TypeID] += t.UnitPrice * float64(t.Quantity)
		}
	}
	var best int64
	for id, v := range sold {
		if best == 0 || v > sold[best] || v == sold[best] && id < best {
			best = id
		}
	}
	if best > 0 {
		view.BestSeller = app.deps.TypeNameOrID(ctx, best)
		view.BestSellerID = best
		view.BestSellerISK = "+" + humanize.ISKCompact(sold[best])
	}
}

// shapeChips fills the journal's quick filters with their counts.
func (app *mod) shapeChips(view *walletView, c walletChoice, journal esi.WalletJournal) {
	defs := []struct{ key, label string }{
		{"", "All"}, {"market", "Market"}, {"fees", "Fees and taxes"},
		{"industry", "Industry"}, {"income", "Bounties and missions"}, {"big", "Over 50M"},
	}
	for _, d := range defs {
		n := 0
		for _, e := range journal {
			if inRange(e.Date, c.cutoff) && journalMatches(e, d.key, c.ref) {
				n++
			}
		}
		if n == 0 && d.key != "" && d.key != c.cat {
			continue
		}
		view.Chips = append(view.Chips, walletChip{Label: d.label, Count: n,
			Href: c.href("/wallet/", map[string]string{"cat": d.key}) + "#wl-journal", On: c.cat == d.key})
	}
	if c.ref != "" {
		view.RefLabel = app.deps.HumanizeEnum(c.ref)
		view.RefClear = c.href("/wallet/", map[string]string{"ref": ""}) + "#wl-journal"
	}
	for _, rg := range []struct{ key, label string }{{"30d", "30 days"}, {"7d", "7 days"}} {
		set := rg.key
		if set == "30d" {
			set = ""
		}
		view.Ranges = append(view.Ranges, walletChip{Label: rg.label, Href: c.href("/wallet/", map[string]string{"range": set}), On: c.rng == rg.key})
	}
	exact := ""
	if c.exact {
		exact = "short"
	}
	view.FormatHref = c.href("/wallet/", map[string]string{"isk": exact})
	view.FormatLabel = "Compact ISK"
	if !c.exact {
		view.FormatLabel = "Exact ISK"
	}
	view.Filtered = c.cat != "" || c.ref != ""
	view.ClearHref = c.href("/wallet/", map[string]string{"cat": "", "ref": ""}) + "#wl-journal"
	q := url.Values{}
	if c.character != "" {
		q.Set("character", c.character)
	}
	if c.rng != "30d" {
		q.Set("range", c.rng)
	}
	if c.cat != "" {
		q.Set("cat", c.cat)
	}
	if c.ref != "" {
		q.Set("ref", c.ref)
	}
	suffix := ""
	if enc := q.Encode(); enc != "" {
		suffix = "?" + enc
	}
	view.JournalCSV = "/wallet/journal.csv" + suffix
	view.TxnsCSV = "/wallet/transactions.csv" + suffix
}

// shapeTrade fills the inspector for the open trade: its sales tax
// matched through the journal, what it netted, the balance after it,
// and the item's and the counterparty's other trades in this window.
func (app *mod) shapeTrade(view *walletView, c walletChoice, journal esi.WalletJournal, txns esi.WalletTransactions, row walletTxnRow, t esi.WalletTransaction) {
	tr := &walletTrade{Row: row, Date: humanize.Finish(t.Date), CloseHref: c.href("/wallet/", nil) + "#wl-txns"}
	total := t.UnitPrice * float64(t.Quantity)
	var tax float64
	for _, e := range journal {
		if e.RefType == "transaction_tax" && e.ContextID == t.TransactionID {
			tax -= e.Amount
		}
		if t.JournalRefID > 0 && e.ID == t.JournalRefID {
			tr.BalanceAt = esi.FormatISK(e.Balance) + " ISK"
		}
	}
	if t.IsBuy {
		tr.NetLabel = "Paid"
		tr.Net = esi.FormatISK(total) + " ISK"
	} else {
		tr.NetLabel = "Kept after sales tax"
		tr.Net = esi.FormatISK(total-tax) + " ISK"
		if tax > 0 {
			tr.Tax = esi.FormatISK(tax) + " ISK"
			tr.TaxNote = fmt.Sprintf("%.2f%% of the sale", tax/total*100)
		} else {
			tr.TaxNote = "No sales tax entry in the journal for this sale."
		}
	}
	var bq, sq int64
	var bv, sv float64
	var bn, sn, wn int
	var wv float64
	for _, o := range txns {
		v := o.UnitPrice * float64(o.Quantity)
		if o.TypeID == t.TypeID {
			if o.IsBuy {
				bn++
				bq += o.Quantity
				bv += v
			} else {
				sn++
				sq += o.Quantity
				sv += v
			}
		}
		if o.ClientID == t.ClientID && o.TransactionID != t.TransactionID {
			wn++
			wv += v
		}
	}
	if bq > 0 {
		tr.ItemBuys = fmt.Sprintf("%s, %s units at %s average", humanize.Plural(bn, "buy"), esi.FormatInt(bq), esi.FormatISK(bv/float64(bq)))
	}
	if sq > 0 {
		tr.ItemSells = fmt.Sprintf("%s, %s units at %s average", humanize.Plural(sn, "sale"), esi.FormatInt(sq), esi.FormatISK(sv/float64(sq)))
	}
	if bq > 0 && sq > 0 {
		ab, as := bv/float64(bq), sv/float64(sq)
		tr.Spread = fmt.Sprintf("%+.1f%%", (as-ab)/ab*100)
	}
	if wn > 0 {
		tr.WithTrades = fmt.Sprintf("%s, %s ISK", humanize.Plural(wn, "other trade"), humanize.ISKCompact(wv))
	}
	tr.Copy = fmt.Sprintf("%s %s x%s at %s ISK = %s ISK\n%s\n%s: %s\nTransaction %d",
		row.Side, row.Item, row.Qty, row.Unit, row.Total, tr.Date, tr.NetLabel, tr.Net, t.TransactionID)
	view.Trade = tr
	view.Depth = 2
}

func inRange(date string, cutoff time.Time) bool {
	t, err := time.Parse(time.RFC3339, date)
	return err == nil && !t.Before(cutoff)
}

func signed(v float64) string {
	if v >= 0 {
		return "+" + humanize.ISKCompact(v)
	}
	return humanize.ISKCompact(v)
}
