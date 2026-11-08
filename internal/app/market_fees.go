package app

import (
	"context"
	"net/url"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/tradefees"
)

// tradeViewer is the fee input for one page render: the best
// Broker Relations and Accounting levels, and the best
// unmodified standings, across the characters in scope. An
// anonymous reader gets perfect skills and relations, so the
// viewer reads perfect and no snapshot is consulted. A
// character whose skill or standings snapshot has not landed
// yet counts as untrained with no standings: the worker syncs
// both kinds in its first cycle, so the window is short, and
// the level-0 fallback is the honest reading of skills the
// app has not seen.
type tradeViewer struct {
	perfect         bool
	brokerRelations int
	accounting      int
	standings       map[standingKey]float64
	owners          map[int64]stationOwner
}

// standingKey names one standings row: the kind of thing the
// standing is with (corporation or faction) and its id.
type standingKey struct {
	fromType string
	fromID   int64
}

// stationOwner is what the SDE says about a station's owner:
// the NPC corporation and its faction. known is false for a
// player structure, or a station the SDE has not named an
// owner for yet.
type stationOwner struct {
	corpID    int64
	factionID int64
	known     bool
}

// resolveTradeViewer builds the fee inputs for a render from
// the characters in scope. characters is the account's full
// list; scope picks the ones in play. With no characters (an
// anonymous reader, or an account with none linked) the viewer
// is perfect.
func (app *Application) resolveTradeViewer(ctx context.Context, characters []db.Character, scope charScope) *tradeViewer {
	v := &tradeViewer{
		standings: make(map[standingKey]float64),
		owners:    make(map[int64]stationOwner),
	}
	if len(characters) == 0 {
		v.perfect = true
		return v
	}
	for _, ch := range characters {
		if !scope.has(ch.CharacterID) {
			continue
		}
		var skills esi.Skills
		if app.LoadSnapshot(ctx, ch.CharacterID, esi.SnapSkills, &skills) {
			for _, s := range skills.Skills {
				switch s.SkillID {
				case tradefees.BrokerRelationsSkill:
					if s.TrainedSkillLevel > v.brokerRelations {
						v.brokerRelations = s.TrainedSkillLevel
					}
				case tradefees.AccountingSkill:
					if s.TrainedSkillLevel > v.accounting {
						v.accounting = s.TrainedSkillLevel
					}
				}
			}
		}
		var standings esi.Standings
		if app.LoadSnapshot(ctx, ch.CharacterID, esi.SnapStandings, &standings) {
			for _, st := range standings {
				key := standingKey{fromType: st.FromType, fromID: st.FromID}
				if st.Standing > v.standings[key] {
					v.standings[key] = st.Standing
				}
			}
		}
	}
	return v
}

// stationOwner resolves a station to its NPC owner, memoized
// per render: the SDE names the corporation, and the
// corporation table names its faction. A station the SDE does
// not know is a player structure (or a station from before the
// owners landed), and reads unknown.
func (app *Application) stationOwner(ctx context.Context, v *tradeViewer, stationID int64) stationOwner {
	if owner, ok := v.owners[stationID]; ok {
		return owner
	}
	owner := stationOwner{}
	if row, err := app.queries.GetSDEStationOwner(ctx, stationID); err == nil && row.CorporationID != 0 {
		owner.corpID = row.CorporationID
		owner.known = true
		if fac, err := app.queries.GetSDENPCCorporationFaction(ctx, row.CorporationID); err == nil {
			owner.factionID = fac.FactionID
		}
	}
	v.owners[stationID] = owner
	return owner
}

// tradeRatesFor is the broker fee and sales tax for a round
// trip at a station, for a viewer. An Upwell structure does
// not use the NPC formula, and its fee is not on file, so the
// rates fall back to the NPC formula with no standing
// reduction and the figure quoting them is marked estimated.
func (app *Application) tradeRatesFor(ctx context.Context, v *tradeViewer, stationID int64) (broker, salesTax float64, estimated bool) {
	if v == nil || v.perfect {
		return tradefees.PerfectBrokerFee(), tradefees.PerfectSalesTax(), false
	}
	owner := app.stationOwner(ctx, v, stationID)
	if !owner.known {
		return tradefees.BrokerFeeRate(v.brokerRelations, 0, 0), tradefees.SalesTaxRate(v.accounting), true
	}
	faction := v.standings[standingKey{fromType: "faction", fromID: owner.factionID}]
	corp := v.standings[standingKey{fromType: "corporation", fromID: owner.corpID}]
	return tradefees.BrokerFeeRate(v.brokerRelations, faction, corp), tradefees.SalesTaxRate(v.accounting), false
}

// tradeRatesNoStation is the broker fee and sales tax for a
// round trip with no station in play (a global-market type,
// which trades nowhere in particular): the NPC formula at the
// viewer's skill levels with no standing reduction. Not an
// estimate: there is no station fee to be unknown about.
func (app *Application) tradeRatesNoStation(v *tradeViewer) (broker, salesTax float64) {
	if v == nil || v.perfect {
		return tradefees.PerfectBrokerFee(), tradefees.PerfectSalesTax()
	}
	return tradefees.BrokerFeeRate(v.brokerRelations, 0, 0), tradefees.SalesTaxRate(v.accounting)
}

// tradeViewerForRequest resolves the fee inputs for a render
// from the request: the signed-in user's characters in the
// ?scope= parameter's scope, or perfect rates when anonymous.
// It also returns the parsed scope and the account's
// characters, so the caller can build the scope control.
func (app *Application) tradeViewerForRequest(ctx context.Context, q url.Values) (*tradeViewer, charScope, []db.Character) {
	userID := app.UserID(ctx)
	var characters []db.Character
	if userID != 0 {
		if chars, err := app.queries.ListCharactersByUser(ctx, userID); err == nil {
			characters = chars
		}
	}
	scope := parseCharScope(q.Get("scope"), characters)
	return app.resolveTradeViewer(ctx, characters, scope), scope, characters
}

// tradePricer is the module TradePricer for one render: the
// viewer plus the scope control for the page.
type tradePricer struct {
	app      *Application
	viewer   *tradeViewer
	selector *module.ScopeSelector
}

func (p *tradePricer) Rates(ctx context.Context, stationID int64) (broker, salesTax float64, estimated bool) {
	return p.app.tradeRatesFor(ctx, p.viewer, stationID)
}

func (p *tradePricer) ScopeControl() *module.ScopeSelector {
	return p.selector
}

// TradePricer resolves net-trade rates for a module page
// render, for the characters in the page's scope. Anonymous
// readers get perfect skills and relations.
func (app *Application) TradePricer(ctx context.Context, q map[string][]string, path string) module.TradePricer {
	viewer, scope, characters := app.tradeViewerForRequest(ctx, url.Values(q))
	p := &tradePricer{app: app, viewer: viewer}
	if app.UserID(ctx) != 0 {
		sel := buildScopeSelector(characters, scope, func(scope string) string {
			return scopeHref(q, path, scope)
		})
		p.selector = &module.ScopeSelector{
			Label: sel.Label, All: sel.All, AllHref: sel.AllHref,
		}
		for _, c := range sel.Chars {
			p.selector.Chars = append(p.selector.Chars, module.ScopeSelectorChar{
				ID: c.ID, Name: c.Name, Image: c.Image, Tags: c.Tags,
				TagsText: c.TagsText, Checked: c.Checked,
				ToggleHref: c.ToggleHref, OnlyHref: c.OnlyHref,
			})
		}
	}
	return p
}

// scopeHref builds a link to a page with the given scope value
// ("" for all characters), keeping the page's other query
// parameters.
func scopeHref(q map[string][]string, path, scope string) string {
	v := url.Values{}
	for k, vals := range q {
		for _, val := range vals {
			v.Add(k, val)
		}
	}
	if scope == "" {
		v.Del("scope")
	} else {
		v.Set("scope", scope)
	}
	return withQuery(path, v)
}
