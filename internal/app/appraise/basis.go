package appraise

// The user-selected pricing basis: a hub region, a mode
// (typical sell, typical buy, or split between them), and an
// optional percent modifier. It is part of every stored
// snapshot, it pre-selects from URL parameters so a preset can
// be linked, and the last one used is remembered -- an account
// preference for signed-in users, a signed cookie for
// anonymous ones.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// pricingBasis is the basis as the appraisal tool shapes it:
// hub region id and display name, mode, percent.
type pricingBasis struct {
	RegionID int64
	HubName  string
	Mode     string
	Percent  float64
}

func (b pricingBasis) toModule() module.AppraisalBasis {
	return module.AppraisalBasis{RegionID: b.RegionID, Mode: b.Mode, Percent: b.Percent}
}

// snapshotBasis is the basis as stored in a snapshot.
type snapshotBasis struct {
	RegionID int64   `json:"region_id"`
	HubName  string  `json:"hub_name"`
	Mode     string  `json:"mode"`
	Percent  float64 `json:"percent"`
}

func (b pricingBasis) snapshot() snapshotBasis {
	return snapshotBasis(b)
}

func (s snapshotBasis) basis() pricingBasis {
	b := pricingBasis(s)
	if b.Mode == "" {
		b.Mode = module.AppraisalModeSell
	}
	if b.Percent == 0 {
		b.Percent = 100
	}
	return b
}

// hubOption is one selectable hub in the form.
type hubOption struct {
	RegionID int64
	Name     string
	Selected bool
}

// hubOptions lists the hub regions the market pages use, in
// their order, with the basis's hub selected.
func (app *page) hubOptions(sel int64) []hubOption {
	regions := app.deps.MarketRegions()
	out := make([]hubOption, 0, len(regions))
	for _, r := range regions {
		out = append(out, hubOption{RegionID: r.ID, Name: r.Name, Selected: r.ID == sel})
	}
	return out
}

// hubName names a hub region for labels: "The Forge" reads as
// "Forge" in "Our Forge typical (sell)".
func hubShortName(name string) string {
	return strings.TrimPrefix(name, "The ")
}

// parseHubValue reads a hub parameter: a region id, or a hub
// name in any case ("Domain", "the forge", "Jita" for The
// Forge's hub station's region).
func (app *page) parseHubValue(raw string) (int64, string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, "", false
	}
	if id, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if name, ok := app.deps.MarketRegionName(id); ok {
			return id, name, true
		}
		return 0, "", false
	}
	switch strings.ToLower(raw) {
	case "jita", "forge", "the forge":
		raw = "The Forge"
	case "amarr":
		raw = "Domain"
	case "dodixie":
		raw = "Sinq Laison"
	case "rens":
		raw = "Heimatar"
	case "hek":
		raw = "Metropolis"
	}
	for _, r := range app.deps.MarketRegions() {
		if strings.EqualFold(r.Name, raw) {
			return r.ID, r.Name, true
		}
	}
	return 0, "", false
}

// clampPercent bounds the percent modifier to its sane range.
func clampPercent(p float64) float64 {
	if p < 50 {
		return 50
	}
	if p > 200 {
		return 200
	}
	return p
}

// basisFromValues reads hub/mode/pct parameters over a base
// basis: any parameter present and valid overrides its field.
func (app *page) basisFromValues(base pricingBasis, q url.Values) pricingBasis {
	out := base
	if raw := q.Get("hub"); raw != "" {
		if id, name, ok := app.parseHubValue(raw); ok {
			out.RegionID, out.HubName = id, name
		}
	}
	switch strings.ToLower(strings.TrimSpace(q.Get("mode"))) {
	case module.AppraisalModeSell, module.AppraisalModeBuy, module.AppraisalModeSplit:
		out.Mode = strings.ToLower(strings.TrimSpace(q.Get("mode")))
	}
	if raw := strings.TrimSpace(q.Get("pct")); raw != "" {
		if p, err := strconv.ParseFloat(strings.TrimSuffix(raw, "%"), 64); err == nil && p > 0 {
			out.Percent = clampPercent(p)
		}
	}
	if out.Mode == "" {
		out.Mode = module.AppraisalModeSell
	}
	if out.Percent == 0 {
		out.Percent = 100
	}
	if out.RegionID == 0 {
		out.RegionID = app.deps.DefaultMarketRegion()
		if name, ok := app.deps.MarketRegionName(out.RegionID); ok {
			out.HubName = name
		}
	}
	if out.HubName == "" {
		if name, ok := app.deps.MarketRegionName(out.RegionID); ok {
			out.HubName = name
		}
	}
	return out
}

func (app *page) defaultBasis() pricingBasis {
	id := app.deps.DefaultMarketRegion()
	name, _ := app.deps.MarketRegionName(id)
	return pricingBasis{RegionID: id, HubName: name, Mode: module.AppraisalModeSell, Percent: 100}
}

// rememberedBasis is the last-used basis: the account
// preference when signed in, the basis cookie when not.
func (app *page) rememberedBasis(ctx context.Context, r *http.Request, userID int64) pricingBasis {
	if userID > 0 {
		if pref, err := app.queries.GetUserAppraisalPref(ctx, userID); err == nil {
			return app.basisFromValues(app.defaultBasis(), url.Values{
				"hub":  {strconv.FormatInt(pref.HubRegion, 10)},
				"mode": {pref.Mode},
				"pct":  {strconv.FormatFloat(pref.Percent, 'f', -1, 64)},
			})
		}
		return app.defaultBasis()
	}
	if c, err := r.Cookie(anonBasisCookie); err == nil && c.Value != "" {
		if secret, err := app.anonHistorySecret(ctx); err == nil {
			if payload, ok := verifySignedValue(secret, c.Value); ok {
				parts := strings.Split(payload, "|")
				if len(parts) == 3 {
					return app.basisFromValues(app.defaultBasis(), url.Values{
						"hub": {parts[0]}, "mode": {parts[1]}, "pct": {parts[2]},
					})
				}
			}
		}
	}
	return app.defaultBasis()
}

// rememberBasis stores the basis just used.
func (app *page) rememberBasis(ctx context.Context, w http.ResponseWriter, r *http.Request, userID int64, b pricingBasis) {
	if userID > 0 {
		if err := app.queries.UpsertUserAppraisalPref(ctx, db.UpsertUserAppraisalPrefParams{
			UserID: userID, HubRegion: b.RegionID, Mode: b.Mode, Percent: b.Percent,
		}); err != nil {
			logging.Errorf("appraisal: remember basis: %v", err)
		}
		return
	}
	secret, err := app.anonHistorySecret(ctx)
	if err != nil {
		logging.Errorf("appraisal: basis secret: %v", err)
		return
	}
	payload := fmt.Sprintf("%d|%s|%s", b.RegionID, b.Mode, strconv.FormatFloat(b.Percent, 'f', -1, 64))
	http.SetCookie(w, &http.Cookie{
		Name:     anonBasisCookie,
		Value:    signValue(secret, payload),
		Path:     "/",
		MaxAge:   int(anonAppraisalTTL / time.Second),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// anonBasisCookie names the remembered-basis cookie.
const anonBasisCookie = "evesynapse-appraisal-basis"

// modeLabel words a mode for tier labels and the basis line.
func modeLabel(mode string) string {
	switch mode {
	case module.AppraisalModeBuy:
		return "buy"
	case module.AppraisalModeSplit:
		return "split"
	default:
		return "sell"
	}
}

// basisModeLong is the pricing mode's full label, the words
// the form's basis select uses: the workbench's cards name
// the basis with them ("The Forge • Typical sell").
func basisModeLong(mode string) string {
	switch mode {
	case module.AppraisalModeBuy:
		return "Typical buy"
	case module.AppraisalModeSplit:
		return "Split"
	default:
		return "Typical sell"
	}
}

// tierLabelWithBasis words a price tier for readers, naming
// the hub and mode for the typical family so a price is never
// ambiguous about its source. The basis-independent tiers
// keep their plain labels.
func tierLabelWithBasis(tier string, b pricingBasis) string {
	hub := hubShortName(b.HubName)
	if hub == "" {
		hub = "Forge"
	}
	switch tier {
	case module.AppraisalTierTypical:
		return "Our " + hub + " typical (sell)"
	case module.AppraisalTierTypicalBuy:
		return "Our " + hub + " typical (buy)"
	case module.AppraisalTierTypicalSplit:
		return "Our " + hub + " typical (split)"
	case module.AppraisalTierTypicalSplitSell:
		return "Our " + hub + " typical (split, sell side only)"
	case module.AppraisalTierTypicalSplitBuy:
		return "Our " + hub + " typical (split, buy side only)"
	case module.AppraisalTierEstimate:
		return "Estimated sell"
	case module.AppraisalTierAbyssal:
		return "Our abyssal estimate"
	case module.AppraisalTierCopyMedian:
		return "Our copy median"
	case module.AppraisalTierFallback:
		return "Fuzzwork aggregate"
	}
	return "Not priced"
}

// tierLabelForLine words one priced line's tier: a typical
// that spoke from a book other than the basis hub's -- the
// global market's, the only other book the pricing core ever
// speaks from -- names that market instead of the hub, so a
// line's stated source is never a hub the type does not
// trade in. Every other line words as tierLabelWithBasis.
func tierLabelForLine(tier string, b pricingBasis, sourceRegionID int64) string {
	if sourceRegionID == 0 || sourceRegionID == b.RegionID || !isTypicalTier(tier) {
		return tierLabelWithBasis(tier, b)
	}
	switch tier {
	case module.AppraisalTierTypical:
		return "Our global PLEX market typical (sell)"
	case module.AppraisalTierTypicalBuy:
		return "Our global PLEX market typical (buy)"
	case module.AppraisalTierTypicalSplit:
		return "Our global PLEX market typical (split)"
	case module.AppraisalTierTypicalSplitSell:
		return "Our global PLEX market typical (split, sell side only)"
	case module.AppraisalTierTypicalSplitBuy:
		return "Our global PLEX market typical (split, buy side only)"
	}
	return tierLabelWithBasis(tier, b)
}

// basisText words the whole basis for the share page: hub,
// mode, and the modifier when one is set.
func (b pricingBasis) basisText() string {
	hub := b.HubName
	if hub == "" {
		hub = "The Forge"
	}
	s := fmt.Sprintf("Priced from %s typical %s prices", hub, modeLabel(b.Mode))
	if b.Percent != 100 && b.Percent != 0 {
		s += fmt.Sprintf(" at %s%%", strconv.FormatFloat(b.Percent, 'f', -1, 64))
	}
	return s + "."
}
