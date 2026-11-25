package app

import (
	"context"
	"fmt"
	"strconv"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
	"evesynapse/internal/standing"
)

func init() {
	registerHomeWidget(homeWidgetDef{
		ID: widgetSRP, Title: "Ship Replacement", Category: wcatFleets, Feature: "killmails", Requires: "srp",
		Href:        srpPath,
		Description: "Your ship replacement claims and what they pay, and the claims waiting for you to review.",
		Default:     wSize{1, 1},
		Forms: []homeWidgetForm{
			{Name: "small", Sizes: "1x1", Says: "Your open claims and what they would pay, and how many wait for your review."},
			{Name: "tall", Sizes: "1x2 1x3", Says: "Your claims, each with its ship, state and payout."},
			{Name: "medium", Sizes: "2x2 2x3", Says: "Your claims over the groups whose claims you review."},
			{Name: "large", Sizes: "3x2 3x3", Says: "Your claims beside the groups whose claims you review."},
		},
		Build: buildSRPWidget,
	})
}

// srpWidgetClaims is the most of the account's own claims the widget reads.
const srpWidgetClaims = 20

// srpClaimRow is one of the account's claims.
type srpClaimRow struct {
	Href     string
	ShipID   int64
	Ship     string
	State    string // "Waiting", "Paid" or "Denied"
	Tone     string // "", "srp-paid" or "srp-denied"
	Pay      string // the payout, or the suggested payout while waiting
	Ago      string
	Waiting  bool
	Suggests bool // Pay is a suggestion, not yet paid
}

// srpWidgetView is what the Ship Replacement forms draw.
type srpWidgetView struct {
	Lead    wLead
	Claims  []srpClaimRow
	Open    int
	Pending string // ISK the open claims would pay
	Review  []srpWaiting
	Waiting int // claims waiting for this account's review
	Empty   wEmpty
}

// Any reports whether the widget has something to show.
func (v *srpWidgetView) Any() bool { return len(v.Claims) > 0 || len(v.Review) > 0 }

func buildSRPWidget(ctx context.Context, app *Application, in *widgetInput) any {
	v := &srpWidgetView{Empty: wEmpty{Text: "No ship replacement claims, and none waiting for you.", Step: "Claim a loss", Href: srpPath}}
	now := in.Now.UTC()
	reqs, err := app.queries.ListSRPRequestsByUser(ctx, db.ListSRPRequestsByUserParams{UserID: in.UserID, RowLimit: srpWidgetClaims})
	if err != nil {
		logging.Errorf("home: ship replacement claims of user %d: %v", in.UserID, err)
	}
	for _, r := range reqs {
		row := srpClaimRow{
			Href: standing.Address(srpPath, orgOwner{Kind: r.OwnerKind, ID: r.OwnerID}.Key()), ShipID: r.ShipTypeID,
			Ship: app.typeNameOrID(ctx, r.ShipTypeID), Ago: humanDuration(now.Sub(r.CreatedAt)) + " ago",
		}
		switch r.Status {
		case srpPaid:
			row.State, row.Tone, row.Pay = "Paid", "srp-paid", iskCompact(r.Payout)
		case srpDenied:
			row.State, row.Tone = "Denied", "srp-denied"
		default:
			row.State, row.Waiting = "Waiting", true
			if r.SuggestedPayout > 0 {
				row.Pay, row.Suggests = iskCompact(r.SuggestedPayout), true
			}
		}
		v.Claims = append(v.Claims, row)
	}
	// The lead counts every open claim, not only the newest listed.
	pending := 0.0
	if open, err := app.queries.CountOpenSRPRequestsByUser(ctx, in.UserID); err != nil {
		logging.Errorf("home: open ship replacement claims of user %d: %v", in.UserID, err)
	} else {
		v.Open, pending = int(open.Requests), open.Suggested
	}
	v.Pending = iskCompact(pending)

	ids := make([]int64, 0, len(in.Chars))
	for _, c := range in.Chars {
		ids = append(ids, c.CharacterID)
	}
	if len(ids) > 0 {
		communities, err := app.queries.ListCommunitiesForCharacters(ctx, ids)
		if err != nil {
			logging.Errorf("home: communities of user %d: %v", in.UserID, err)
		}
		v.Review = app.srpWaitingFor(ctx, in.UserID, communities)
		for _, w := range v.Review {
			v.Waiting += w.Open
		}
	}

	v.Lead = wLead{Label: "Your open claims", Value: strconv.Itoa(v.Open), Full: fmt.Sprintf("%d claims waiting", v.Open)}
	if pending > 0 {
		v.Lead.Sub = v.Pending + " ISK suggested"
	}
	return v
}
