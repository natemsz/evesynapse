package app

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	"evesynapse/internal/esi"
)

func init() {
	registerHomeWidget(homeWidgetDef{
		ID: widgetContracts, Title: "Contracts", Category: wcatMoney, Feature: "contracts", Requires: "contracts",
		Href:        "/contracts/",
		Description: "Open contracts your pilots issued or were given, and when each one runs out.",
		Default:     wSize{1, 1},
		Forms: []homeWidgetForm{
			{Name: "small", Sizes: "1x1", Says: "How many contracts are open and the soonest deadline."},
			{Name: "tall", Sizes: "1x2 1x3", Says: "Each open contract with its kind, state, deadline and price."},
			{Name: "medium", Sizes: "2x2 2x3", Says: "Open contracts split into those for you and those you issued."},
			{Name: "large", Sizes: "3x2 3x3", Says: "Contracts for you beside the ones you issued."},
		},
		Needs:   []string{esi.SnapContracts},
		Options: []widgetOption{optScope()},
		Build:   buildContractsWidget,
	})
}

// contractsWidgetRows is the most open contracts the widget lists.
const contractsWidgetRows = 40

// contractWidgetRow is one open contract.
type contractWidgetRow struct {
	Href     string
	Kind     string // "Courier", "Item exchange", "Auction", "Loan"
	Name     string
	State    string // "Waiting" or "In progress"
	CharID   int64
	Pilot    string
	Money    string // the price, reward or buyout, short
	MoneyFor string // what Money is: "price", "reward"
	Due      string // "Expires" or "Due"
	DueLow   string // the same in lower case, inside a line
	Left     string
	DueRaw   string
	DueAt    string
	Overdue  bool
	Mine     bool // issued by one of the account's pilots
	due      time.Time
}

// contractsWidgetView is what the Contracts forms draw.
type contractsWidgetView struct {
	Lead    wLead
	Rows    []contractWidgetRow
	ForYou  []contractWidgetRow
	FromYou []contractWidgetRow
	Soonest *contractWidgetRow
	Known   bool
	Empty   wEmpty
}

func buildContractsWidget(ctx context.Context, app *Application, in *widgetInput) any {
	v := &contractsWidgetView{Empty: wEmpty{Text: "Contracts have not loaded yet.", Step: "Open Contracts", Href: "/contracts/"}}
	now := in.Now.UTC()
	own := viewerChars(in.Chars)
	seen := map[int64]bool{}
	for _, b := range in.Bundles {
		if !b.contractsKnown {
			continue
		}
		v.Known = true
		for _, c := range b.contracts {
			if seen[c.ContractID] {
				continue
			}
			row, ok := contractWidgetRowOf(b, c, own, now)
			if !ok {
				continue
			}
			seen[c.ContractID] = true
			v.Rows = append(v.Rows, row)
		}
	}
	// Soonest deadline first; contracts with no deadline last.
	sort.SliceStable(v.Rows, func(i, j int) bool {
		a, b := v.Rows[i].due, v.Rows[j].due
		if a.IsZero() != b.IsZero() {
			return !a.IsZero()
		}
		return a.Before(b)
	})
	// The lead counts every open contract; the list keeps the soonest.
	total := len(v.Rows)
	if total > contractsWidgetRows {
		v.Rows = v.Rows[:contractsWidgetRows]
	}
	for _, r := range v.Rows {
		if r.Mine {
			v.FromYou = append(v.FromYou, r)
		} else {
			v.ForYou = append(v.ForYou, r)
		}
	}
	if len(v.Rows) > 0 && !v.Rows[0].due.IsZero() {
		v.Soonest = &v.Rows[0]
	}
	v.Lead = wLead{Label: "Open contracts", Value: strconv.Itoa(total), Href: "/contracts/",
		Full: pluralCount(int64(total), "open contract", "open contracts")}
	if v.Known && len(v.Rows) == 0 {
		v.Empty = wEmpty{Text: "No open contracts.", Step: "Open Contracts", Href: "/contracts/"}
	}
	return v
}

// contractWidgetRowOf reads one contract from a pilot's snapshot: open
// contracts the pilot issued, was assigned or accepted. ok is false for
// a closed contract or one the account is not a party to.
func contractWidgetRowOf(b *charSnaps, c esi.Contract, own map[int64]bool, now time.Time) (contractWidgetRow, bool) {
	id := b.ch.CharacterID
	if c.IssuerID != id && c.AssigneeID != id && c.AcceptorID != id {
		return contractWidgetRow{}, false
	}
	row := contractWidgetRow{
		Href: fmt.Sprintf("/contracts/?character=%d&contract=%d", id, c.ContractID),
		Kind: contractKindName(c.Type), Name: contractName(c), CharID: id, Pilot: b.ch.Name,
		Mine: own[c.IssuerID],
	}
	switch c.Status {
	case "outstanding":
		expires, ok := parseRFC3339(c.DateExpired)
		if ok && !expires.After(now) {
			return contractWidgetRow{}, false
		}
		row.State, row.Due, row.DueLow = "Waiting", "Expires", "expires"
		if ok {
			row.due = expires
		}
	case "in_progress":
		row.State = "In progress"
		if c.Type == "courier" {
			if deadline, ok := courierDeadline(c); ok {
				row.Due, row.DueLow, row.due = "Due", "due", deadline
			}
		}
	default:
		return contractWidgetRow{}, false
	}
	if !row.due.IsZero() {
		row.DueRaw = row.due.UTC().Format(time.RFC3339)
		row.DueAt = formatFinish(row.DueRaw)
		if row.due.After(now) {
			row.Left = humanDuration(row.due.Sub(now))
		} else {
			row.Overdue = true
		}
	}
	switch {
	case c.Type == "courier" && c.Reward > 0:
		row.Money, row.MoneyFor = iskCompact(c.Reward), "reward"
	case c.Type == "auction" && c.Buyout > 0:
		row.Money, row.MoneyFor = iskCompact(c.Buyout), "buyout"
	case c.Price > 0:
		row.Money, row.MoneyFor = iskCompact(c.Price), "price"
	}
	return row, true
}

// contractKindName names an ESI contract type in plain words.
func contractKindName(t string) string {
	switch t {
	case "item_exchange":
		return "Item exchange"
	case "courier":
		return "Courier"
	case "auction":
		return "Auction"
	case "loan":
		return "Loan"
	}
	return "Contract"
}
