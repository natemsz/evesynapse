package app

import (
	"context"
	"encoding/json"
	"time"

	"evesynapse/internal/app/module"
	"html"
	"html/template"
	"strings"

	"evesynapse/internal/esi"
	"evesynapse/internal/fit"
)

// publicPage is the page data anonymous-safe routes start from:
// the session's login state, never assumed.
func (app *Application) publicPage(ctx context.Context) pageData {
	loggedIn := app.sessions.GetBool(ctx, sessionAuthenticated)
	return pageData{
		LoggedIn:      loggedIn,
		CharacterName: app.sessions.GetString(ctx, sessionCharacterName),
		SSOConfigured: app.cfg.SSOConfigured(),
	}
}

// publicFitSections groups a stored document's items the way the
// saved-fits list does, resolving every name from the local
// caches. Classification needs the dogma snapshot; without it
// every line lands in Other rather than vanishing.
func (app *Application) publicFitSections(ctx context.Context, doc *fitDoc) []fittingGroup {
	typeIDs := []int64{doc.ShipTypeID}
	for _, it := range doc.Items {
		typeIDs = append(typeIDs, it.TypeID)
	}
	names := app.esi.CachedTypeNames(ctx, typeIDs)
	nameOf := func(typeID int64) string {
		if n, ok := names[typeID]; ok && n != "" {
			return n
		}
		return "Type #" + idString(typeID)
	}
	familyOf := map[int64]string{}
	if snap, err := fit.LoadSnapshot(ctx, app.queries, typeIDs); err == nil && snap != nil {
		for _, id := range typeIDs {
			familyOf[id] = fitSlotFamilyOf(snap, id)
		}
	}
	familyLabel := map[string]string{
		fitFamilyHigh: "High slots", fitFamilyMedium: "Medium slots",
		fitFamilyLow: "Low slots", fitFamilyRig: "Rig slots",
		fitFamilySubsystem: "Subsystem slots", fitFamilyDrone: "Drone bay",
		fitFamilyCargo: "Cargo hold",
	}
	byFamily := map[string][]fittingItemRow{}
	order := []string{}
	for _, it := range doc.Items {
		family := familyOf[it.TypeID]
		if _, ok := familyLabel[family]; !ok {
			family = "other"
		}
		if _, ok := byFamily[family]; !ok {
			order = append(order, family)
		}
		byFamily[family] = append(byFamily[family], fittingItemRow{
			Name: nameOf(it.TypeID), TypeID: it.TypeID,
			Quantity: esi.FormatInt(int64(it.Qty)),
		})
	}
	ordered := []string{fitFamilyHigh, fitFamilyMedium, fitFamilyLow, fitFamilyRig, fitFamilySubsystem, fitFamilyDrone, fitFamilyCargo}
	seen := map[string]bool{}
	var sections []fittingGroup
	for _, family := range ordered {
		items, ok := byFamily[family]
		if !ok {
			continue
		}
		seen[family] = true
		sections = append(sections, fittingGroup{Label: familyLabel[family], Items: items})
	}
	for _, family := range order {
		if seen[family] {
			continue
		}
		sections = append(sections, fittingGroup{Label: "Other", Items: byFamily[family]})
	}
	return sections
}

// fitCostView is what a fit costs, as every page shows it: the
// fitting tool's stats panel, the public fit page and its card, and
// the browser's price filter all read this one composition.
type fitCostView struct {
	Known    bool
	Hull     string // the ship itself
	Parts    string // every line on the hull: modules, rigs, drones, cargo
	Total    string
	TotalRaw float64
	HullRaw  float64
	PartsRaw float64
	Unpriced int // lines the guide has no price for (hull included)
}

// fitUnitPrice is the guide's price for one unit: the market
// average, else the adjusted price CCP publishes for every type.
// RepriceLocalFittings makes the same choice in SQL: change both.
func fitUnitPrice(prices map[int64]esi.MarketPrice, typeID int64) (float64, bool) {
	p, ok := prices[typeID]
	if !ok {
		return 0, false
	}
	if p.AveragePrice > 0 {
		return p.AveragePrice, true
	}
	if p.AdjustedPrice > 0 {
		return p.AdjustedPrice, true
	}
	return 0, false
}

// fitCost prices a document from the guide mirror: the hull plus
// every item line. Known is false when nothing in it is priced
// (guide not warmed yet); the browser treats a 0 total as unknown.
func (app *Application) fitCost(ctx context.Context, doc *fitDoc) *fitCostView {
	c := &fitCostView{}
	prices := app.valuationPrices(ctx)
	if len(prices) == 0 || doc == nil || doc.ShipTypeID == 0 {
		return c
	}
	if p, ok := fitUnitPrice(prices, doc.ShipTypeID); ok {
		c.HullRaw = p
	} else {
		c.Unpriced++
	}
	for _, it := range doc.Items {
		if p, ok := fitUnitPrice(prices, it.TypeID); ok {
			c.PartsRaw += float64(it.Qty) * p
		} else {
			c.Unpriced++
		}
	}
	c.TotalRaw = c.HullRaw + c.PartsRaw
	if c.TotalRaw > 0 {
		c.Known = true
		c.Hull = esi.FormatISK(c.HullRaw)
		c.Parts = esi.FormatISK(c.PartsRaw)
		c.Total = esi.FormatISK(c.TotalRaw)
	}
	return c
}

// fitDocPrice is fitCost's total, stored on the fit for the
// browser's max-price filter and sorts (0 = unknown). The worker
// makes the same sum in SQL whenever the guide refreshes
// (RepriceLocalFittings): change both together.
func (app *Application) fitDocPrice(ctx context.Context, doc *fitDoc) float64 {
	return app.fitCost(ctx, doc).TotalRaw
}

// headMeta composes escaped description and Open Graph tags for a
// public page; values are attribute-escaped at build time. og:url
// is left out when the page has no absolute address (pageURL "").
func headMeta(title, description, pageURL string) template.HTML {
	esc := html.EscapeString
	var b strings.Builder
	b.WriteString(`<meta name="description" content="` + esc(description) + `">` + "\n")
	b.WriteString(`<meta property="og:title" content="` + esc(title) + `">` + "\n")
	b.WriteString(`<meta property="og:description" content="` + esc(description) + `">` + "\n")
	b.WriteString(`<meta property="og:type" content="website">` + "\n")
	if pageURL != "" {
		b.WriteString(`<meta property="og:url" content="` + esc(pageURL) + `">` + "\n")
	}
	return template.HTML(b.String())
}

// publicFitsPath, publicFitCard and publicFitCardOf are the public fits
// module's card, read by the ship pages.
const publicFitsPath = module.PublicFitsPath

type publicFitCard = module.PublicFitCard

var publicFitCardOf = module.PublicFitCardOf

// PublicFitBody is the fit-derived part of a public fit's page: priced
// now the way the fitting tool's stats panel does, and the modules,
// simulation and EFT read from the stored page (char_fit_public_store.go);
// a view never runs the engine.
func (app *Application) PublicFitBody(ctx context.Context, fitID int64, updatedAt time.Time, itemsJSON string, shipTypeID int64) module.PublicFitBody {
	var doc fitDoc
	if json.Unmarshal([]byte(itemsJSON), &doc) != nil {
		return module.PublicFitBody{Preparing: true}
	}
	if doc.ShipTypeID == 0 {
		doc.ShipTypeID = shipTypeID
	}
	cost := app.fitCost(ctx, &doc)
	body := module.PublicFitBody{PriceKnown: cost.Known, PriceRaw: cost.TotalRaw, PriceTotal: cost.Total}
	if stored, ok := app.loadPublicFitStored(ctx, fitID, updatedAt); ok {
		body.Sections, body.EFT = stored.Sections, stored.EFT
		if stored.Sim != nil {
			// The stored page may be shared between requests: render
			// a copy, as the tool's own workbench made read-only.
			sim := *stored.Sim
			sim.ReadOnly = true
			sim.Cost = cost
			body.Sim = &sim
		}
	} else {
		body.Preparing = true
	}
	return body
}
