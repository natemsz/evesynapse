package app

// LP earnings per store (schema 114). ESI serves a character's LP
// balance per corporation, cached an hour, and no LP store purchase
// log (and nothing for corporations' LP). So:
//
//   - each pass samples the balances when they changed; a rise is LP
//     earned, a drop LP spent at that store, bracketed between the two
//     samples and never timed closer;
//   - a drop is solved against the store's stored offers: one offer
//     and a quantity whose LP is exactly the drop (and whose ISK is
//     the journal's LP store payments in the bracket, when any are
//     there; their absence voids nothing). One solution is inferred,
//     several ambiguous (the player picks), none unexplained;
//   - an inferred purchase asks for the character's assets again once
//     their cache runs out, and a rise of the product's quantity
//     against the assets held at the drop corroborates it. A product
//     not seen never voids it: it may be sold, contracted or moved.
//
// Inferred and corroborated purchases (and the player's picks) become
// stock lots at the ISK they cost and their required items, LP at 0;
// their sales are LP income. The daily rows keep the day's net LP
// apart, so profit at any LP rate is one multiplication.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// lpSolveMaxOffers bounds how many offers' worth one drop may be.
const lpSolveMaxOffers = 10000

// lpCandidate is one offer and quantity that explains a drop.
type lpCandidate struct {
	OfferID  int64 `json:"offer_id"`
	Quantity int64 `json:"quantity"`
}

// solveLPDrop finds the offers that, bought some whole number of
// times, spend exactly the LP that left the balance, and when the
// journal shows the bracket's LP store payments, cost exactly that
// ISK too. A bracket that mixed two offers finds none here; see
// solveLPPairs.
func solveLPDrop(offers []db.ListLPOffersForCorpRow, lp int64, journalISK float64, journalSeen bool) []lpCandidate {
	var out []lpCandidate
	for _, o := range offers {
		if o.LpCost <= 0 || lp%o.LpCost != 0 {
			continue
		}
		k := lp / o.LpCost
		if k > lpSolveMaxOffers {
			continue
		}
		if journalSeen && math.Abs(float64(k)*o.IskCost-journalISK) > 1 {
			continue
		}
		out = append(out, lpCandidate{OfferID: o.OfferID, Quantity: k})
	}
	return out
}

// solveLPPairs finds pairs of offers that together spend exactly the
// LP that left the balance and exactly the ISK the journal shows paid
// in the bracket: two offers, two equations, so each quantity is
// exact. Without the ISK leg a mixed bracket has too many answers to
// claim any, so it is never tried.
func solveLPPairs(offers []db.ListLPOffersForCorpRow, lp int64, isk float64) [][2]lpCandidate {
	var out [][2]lpCandidate
	for i := range offers {
		for j := i + 1; j < len(offers); j++ {
			a, b := offers[i], offers[j]
			if a.LpCost <= 0 || b.LpCost <= 0 {
				continue
			}
			det := float64(a.LpCost)*b.IskCost - float64(b.LpCost)*a.IskCost
			if det == 0 {
				continue
			}
			ka := math.Round((float64(lp)*b.IskCost - float64(b.LpCost)*isk) / det)
			kb := math.Round((float64(a.LpCost)*isk - float64(lp)*a.IskCost) / det)
			if ka < 1 || kb < 1 || ka > lpSolveMaxOffers || kb > lpSolveMaxOffers {
				continue
			}
			if int64(ka)*a.LpCost+int64(kb)*b.LpCost != lp || math.Abs(ka*a.IskCost+kb*b.IskCost-isk) > 1 {
				continue
			}
			out = append(out, [2]lpCandidate{{OfferID: a.OfferID, Quantity: int64(ka)}, {OfferID: b.OfferID, Quantity: int64(kb)}})
		}
	}
	return out
}

// landLPSamples samples a character's LP balances, infers a purchase
// from each drop, and runs the timed asset check on purchases still
// waiting for it. It reports whether a purchase that makes stock lots
// was added, which the caller refolds the pool for, and whether
// anything the store summary shows changed.
func (app *Application) landLPSamples(ctx context.Context, ch db.Character, now time.Time) (lots, changed bool) {
	snap, err := app.queries.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: ch.CharacterID, Kind: esi.SnapLoyalty})
	if err != nil {
		return false, false
	}
	var balances esi.LoyaltyBalances
	if json.Unmarshal([]byte(snap.Payload), &balances) != nil {
		return false, false
	}
	latest := map[int64]db.ListLatestLPSamplesRow{}
	if rows, err := app.queries.ListLatestLPSamples(ctx, ch.CharacterID); err == nil {
		for _, r := range rows {
			latest[r.CorporationID] = r
		}
	}
	at := snap.FetchedAt.UTC()
	cur := map[int64]int64{}
	for _, b := range balances {
		cur[b.CorporationID] = b.LoyaltyPoints
	}
	for corp, prev := range latest {
		if _, ok := cur[corp]; !ok && prev.Points > 0 {
			cur[corp] = 0 // ESI leaves out a balance spent to nothing
		}
	}
	sampled := false
	var first time.Time
	for corp, points := range cur {
		prev, had := latest[corp]
		if had && (!prev.SampledAt.Before(at) || prev.Points == points) {
			continue
		}
		if err := app.queries.InsertLPSample(ctx, db.InsertLPSampleParams{CharacterID: ch.CharacterID, CorporationID: corp, SampledAt: at, Points: points}); err != nil {
			logging.Errorf("worker: lp sample for character %d: %v", ch.CharacterID, err)
			continue
		}
		sampled = true
		if first.IsZero() || at.Before(first) {
			first = at.Truncate(24 * time.Hour)
		}
		if had && points < prev.Points {
			if app.inferLPPurchase(ctx, ch, corp, prev.SampledAt, at, prev.Points-points) {
				lots = true
			}
		}
	}
	if sampled {
		if err := app.queries.FoldProfitLPDelta(ctx, db.FoldProfitLPDeltaParams{CharacterID: ch.CharacterID, FromDay: first}); err != nil {
			logging.Errorf("worker: lp delta for character %d: %v", ch.CharacterID, err)
		}
	}
	checked := app.checkLPPurchases(ctx, ch, now)
	return lots, sampled || checked
}

// inferLPPurchase stores what a drop at one store can claim, and
// reports whether it makes stock lots. A drop one offer explains is
// one inferred purchase; a drop two offers explain together (by the
// LP and the journal's ISK) is one inferred purchase per offer.
func (app *Application) inferLPPurchase(ctx context.Context, ch db.Character, corp int64, from, to time.Time, lp int64) bool {
	p := db.InsertLPPurchaseParams{CharacterID: ch.CharacterID, CorporationID: corp, FromAt: from, ToAt: to, LpSpent: lp, Tier: "unexplained"}
	paid, err := app.queries.SumLPStorePaymentsBetween(ctx, db.SumLPStorePaymentsBetweenParams{OwnerID: ch.CharacterID, FromAt: from, ToAt: to})
	journalSeen := err == nil && paid.N > 0
	if err == nil {
		p.JournalIsk = paid.Isk
		p.IskPaid = paid.Isk
	}
	offers, _ := app.queries.ListLPOffersForCorp(ctx, corp)
	byID := map[int64]db.ListLPOffersForCorpRow{}
	for _, o := range offers {
		byID[o.OfferID] = o
	}
	var assets *db.CharacterSnapshot
	if snap, err := app.queries.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: ch.CharacterID, Kind: esi.SnapAssets}); err == nil {
		assets = &snap
	}
	inferred := func(c lpCandidate) db.InsertLPPurchaseParams {
		q := p
		offer := byID[c.OfferID]
		app.fillLPPurchase(ctx, &q, corp, offer, c.Quantity)
		q.LpSpent = c.Quantity * offer.LpCost
		q.Tier = "inferred"
		if assets != nil {
			q.AssetsBefore = heldOfType(assets.Payload, q.ProductTypeID)
			q.AssetsAsOf = timeSet(assets.FetchedAt)
		}
		return q
	}
	rows := []db.InsertLPPurchaseParams{p}
	cands := solveLPDrop(offers, lp, paid.Isk, journalSeen)
	switch {
	case len(cands) == 1:
		rows = []db.InsertLPPurchaseParams{inferred(cands[0])}
	case len(cands) > 1:
		rows[0].Tier = "ambiguous"
		if len(cands) > 10 {
			cands = cands[:10]
		}
		b, _ := json.Marshal(cands)
		rows[0].Candidates = string(b)
	case journalSeen:
		pairs := solveLPPairs(offers, lp, paid.Isk)
		switch {
		case len(pairs) == 1:
			rows = []db.InsertLPPurchaseParams{inferred(pairs[0][0]), inferred(pairs[0][1])}
		case len(pairs) > 1:
			rows[0].Tier = "ambiguous" // several pairs fit: nothing to pick from one list
		}
	}
	for _, r := range rows {
		if err := app.queries.InsertLPPurchase(ctx, r); err != nil {
			logging.Errorf("worker: lp purchase for character %d: %v", ch.CharacterID, err)
			return false
		}
	}
	return rows[0].Tier == "inferred"
}

// fillLPPurchase sets a purchase's offer, product, units, ISK and the
// house value of the items it took in exchange (that day's price).
func (app *Application) fillLPPurchase(ctx context.Context, p *db.InsertLPPurchaseParams, corp int64, offer db.ListLPOffersForCorpRow, k int64) {
	p.OfferID, p.Quantity, p.ProductTypeID = offer.OfferID, k, offer.TypeID
	p.Units = k * max(offer.Quantity, 1)
	p.IskPaid = float64(k) * offer.IskCost
	p.ReqValue = app.lpRequiredValue(ctx, corp, offer.OfferID, k)
}

func (app *Application) lpRequiredValue(ctx context.Context, corp, offerID, k int64) float64 {
	reqs, err := app.queries.ListLPOfferRequirementsForOffer(ctx, db.ListLPOfferRequirementsForOfferParams{CorporationID: corp, OfferID: offerID})
	if err != nil || len(reqs) == 0 {
		return 0
	}
	typicals := app.TypicalSells(ctx)
	estimates := app.storedTrueValues(ctx)
	v := 0.0
	for _, r := range reqs {
		if price, ok := pickValuationPrice(typicals[r.TypeID], estSellOf(estimates, r.TypeID)); ok {
			v += price * float64(r.Quantity*k)
		}
	}
	return v
}

// heldOfType is how many of a type an assets payload holds.
func heldOfType(payload string, typeID int64) int64 {
	var assets []esi.Asset
	if json.Unmarshal([]byte(payload), &assets) != nil {
		return 0
	}
	var n int64
	for _, a := range assets {
		if a.TypeID == typeID {
			n += a.Quantity
		}
	}
	return n
}

// checkLPPurchases runs the timed asset check: once assets fetched
// after the drop are in, a rise of the product's quantity by the
// purchase's units corroborates it; anything less leaves it inferred.
// Until then, once the assets cache has run out, it asks the worker
// for them at its next round. It reports whether any was checked.
func (app *Application) checkLPPurchases(ctx context.Context, ch db.Character, now time.Time) bool {
	waiting, err := app.queries.ListUncheckedLPPurchases(ctx, ch.CharacterID)
	if err != nil || len(waiting) == 0 {
		return false
	}
	snap, err := app.queries.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: ch.CharacterID, Kind: esi.SnapAssets})
	if err != nil {
		return false
	}
	checked := false
	for _, p := range waiting {
		if !p.AssetsAsOf.Valid || !snap.FetchedAt.After(p.AssetsAsOf.Time) {
			if !esi.CacheWindowOpen(snap.CachedUntil) {
				app.activity.boost(ch.CharacterID, esi.SnapAssets, now)
			}
			continue
		}
		tier := "inferred"
		if heldOfType(snap.Payload, p.ProductTypeID)-p.AssetsBefore >= p.Units {
			tier = "corroborated"
		}
		if err := app.queries.SetLPPurchaseChecked(ctx, db.SetLPPurchaseCheckedParams{ID: p.ID, Tier: tier}); err != nil {
			logging.Errorf("worker: lp purchase check %d: %v", p.ID, err)
		}
		checked = true
	}
	return checked
}

// foldLPSummary rewrites a character pool's per-store rows and marks
// their unsold units at today's stat pack.
func (app *Application) foldLPSummary(ctx context.Context, pool ledgerPool, now time.Time) error {
	if pool.ownerKind != ownerChar {
		return nil // corporations' LP is not in ESI
	}
	tx, err := app.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	qtx := app.queries.WithTx(tx)
	if err := qtx.DeleteLPStoreSummaryForPool(ctx, db.DeleteLPStoreSummaryForPoolParams{PoolKind: pool.kind, PoolID: pool.id}); err != nil {
		return err
	}
	if err := qtx.FoldLPStoreSummary(ctx, db.FoldLPStoreSummaryParams{PoolKind: pool.kind, PoolID: pool.id, CharacterIds: pool.members, FoldedAt: now.UTC()}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	unsold, err := app.queries.ListLPUnsoldByCorp(ctx, db.ListLPUnsoldByCorpParams{PoolKind: pool.kind, PoolID: pool.id, CharacterIds: pool.members})
	if err != nil || len(unsold) == 0 {
		return err
	}
	ids := make([]int64, 0, len(unsold))
	for _, u := range unsold {
		ids = append(ids, u.ProductTypeID)
	}
	region := app.profitPrefsFor(ctx, pool.userID).IndustryRegion
	packs, err := app.loadStatPacks(ctx, region, ids)
	if err != nil {
		return err
	}
	type mark struct {
		value  float64
		priced bool
	}
	byCorp := map[int64]*mark{}
	for _, u := range unsold {
		m := byCorp[u.CorporationID]
		if m == nil {
			m = &mark{priced: true}
			byCorp[u.CorporationID] = m
		}
		if u.Unsold <= 0 {
			continue
		}
		pk := packs[u.ProductTypeID]
		if pk == nil {
			m.priced = false
			continue
		}
		if p := resolveAppraisalPriceMode(pk.Own, pk.Estimate, pk.Fallback, now, module.AppraisalModeSell); p.ok {
			m.value += p.unit * float64(u.Unsold)
		} else {
			m.priced = false
		}
	}
	for corp, m := range byCorp {
		if err := app.queries.SetLPStoreUnsold(ctx, db.SetLPStoreUnsoldParams{PoolKind: pool.kind, PoolID: pool.id, CorporationID: corp, UnsoldValue: m.value, UnsoldPriced: m.priced}); err != nil {
			return err
		}
	}
	return nil
}

// writeLPSettlements stores what LP store purchases' units sold for:
// a sale's takes from LP lots, at the lot's ISK (LP at 0).
func writeLPSettlements(ctx context.Context, qtx *db.Queries, pool ledgerPool, typeID int64, cp time.Time, sales []realizedSale) error {
	if pool.ownerKind != ownerChar {
		return nil
	}
	if err := qtx.DeleteLPSaleLinksFrom(ctx, db.DeleteLPSaleLinksFromParams{PoolKind: pool.kind, PoolID: pool.id, TypeID: typeID, FromAt: cp}); err != nil {
		return err
	}
	p := db.InsertLPSaleLinksBatchParams{PoolKind: pool.kind, PoolID: pool.id, TypeID: typeID}
	for _, s := range sales {
		if s.Kind != lotTxn {
			continue
		}
		for _, t := range s.Takes {
			if !isLPRef(t.RefID) {
				continue
			}
			p.PurchaseIds = append(p.PurchaseIds, lpPurchaseRef-t.RefID)
			p.OwnerIds = append(p.OwnerIds, s.Owner)
			p.TransactionIds = append(p.TransactionIds, s.RefID)
			p.Ats = append(p.Ats, s.At.UTC().Format(time.RFC3339))
			p.Units = append(p.Units, t.Quantity)
			p.Revenues = append(p.Revenues, float64(t.Quantity)*s.Unit)
			p.Costs = append(p.Costs, float64(t.Quantity)*t.Unit)
		}
	}
	if len(p.PurchaseIds) == 0 {
		return nil
	}
	return qtx.InsertLPSaleLinksBatch(ctx, p)
}

// ---------------------------------------------------------------------------
// The LP section.
// ---------------------------------------------------------------------------

type profitLPRow struct {
	Name                       string
	Earned, Spent, Tiers       string
	ISKPaid, Acquired, Sold    string
	Net, NetDir, PerLP         string
	Unsold                     string
	AtBest, AtMedian, AtCustom string // net at an LP cost of each, when meaningful
	AtBestDir, AtMedianDir     string
	AtCustomDir                string
	BestRate                   string
}

type profitLPPick struct {
	ID         int64
	Store, LP  string
	When       string
	Candidates []profitLPChoice
}

type profitLPChoice struct {
	Value, Label string
}

func (app *Application) attachLP(ctx context.Context, view *profitView, poolKind string, poolIDs, charIDs []int64, prefs profitPrefs) error {
	rows, err := app.queries.ListLPStoreSummary(ctx, db.ListLPStoreSummaryParams{PoolKind: poolKind, PoolIds: poolIDs})
	if err != nil {
		return err
	}
	if prefs.LPValue > 0 {
		view.LPValue = strconv.FormatFloat(prefs.LPValue, 'f', -1, 64)
	}
	// The player's usual realized ISK per LP across their stores: the
	// middle of the stores that sold anything.
	var rates []float64
	for _, s := range rows {
		if s.LpSold > 0 {
			rates = append(rates, (s.Revenue-s.Tax-s.SoldCost)/s.LpSold)
		}
	}
	median := 0.0
	if len(rates) > 0 {
		sort.Float64s(rates)
		median = rates[len(rates)/2]
		view.LPMedian = esi.FormatISK(median)
	}
	now := time.Now()
	for _, s := range rows {
		spent := s.LpCorroborated + s.LpInferred + s.LpAmbiguous + s.LpUnexplained
		row := profitLPRow{
			Name:   app.CorpDisplayName(ctx, s.CorporationID),
			Earned: esi.FormatInt(s.LpEarned), Spent: esi.FormatInt(spent),
			ISKPaid: esi.FormatISK(s.IskPaid), Acquired: esi.FormatInt(s.UnitsAcquired), Sold: esi.FormatInt(s.UnitsSold),
		}
		if spent > 0 {
			var parts []string
			for _, t := range []struct {
				n     int64
				label string
			}{{s.LpCorroborated, "corroborated"}, {s.LpInferred, "inferred"}, {s.LpAmbiguous, "ambiguous"}, {s.LpUnexplained, "unexplained"}} {
				if t.n > 0 {
					parts = append(parts, esi.FormatInt(t.n)+" "+t.label)
				}
			}
			row.Tiers = strings.Join(parts, ", ")
		}
		if s.UnitsSold > 0 {
			net := s.Revenue - s.Tax - s.SoldCost
			row.Net, row.NetDir = signedISK(net), dirOf(net)
			if s.LpSold > 0 {
				row.PerLP = esi.FormatISK(net / s.LpSold)
				if best, err := app.queries.GetBestLPOfferValue(ctx, db.GetBestLPOfferValueParams{RegionID: prefs.IndustryRegion, CorporationID: s.CorporationID}); err == nil && best.IskPerLp > 0 {
					v := net - best.IskPerLp*s.LpSold
					row.AtBest, row.AtBestDir = signedISK(v), dirOf(v)
					row.BestRate = esi.FormatISK(best.IskPerLp) + " ISK per LP, " + ageText(now, best.AsOf)
				}
				if median > 0 {
					v := net - median*s.LpSold
					row.AtMedian, row.AtMedianDir = signedISK(v), dirOf(v)
				}
				if prefs.LPValue > 0 {
					v := net - prefs.LPValue*s.LpSold
					row.AtCustom, row.AtCustomDir = signedISK(v), dirOf(v)
				}
			}
		}
		switch {
		case !s.UnsoldPriced:
			row.Unsold = "some unpriced"
		case s.UnsoldValue > 0:
			row.Unsold = esi.FormatISK(s.UnsoldValue)
		}
		view.LPStores = append(view.LPStores, row)
	}
	picks, err := app.queries.ListLPPurchasesToPick(ctx, charIDs)
	if err != nil {
		return err
	}
	for _, p := range picks {
		pick := profitLPPick{ID: p.ID, Store: app.CorpDisplayName(ctx, p.CorporationID), LP: esi.FormatInt(p.LpSpent),
			When: fmt.Sprintf("between %s and %s", p.FromAt.UTC().Format("Jan 2 15:04"), p.ToAt.UTC().Format("Jan 2 15:04"))}
		var cands []lpCandidate
		_ = json.Unmarshal([]byte(p.Candidates), &cands)
		if len(cands) > 0 {
			offers, _ := app.queries.ListLPOffersForCorp(ctx, p.CorporationID)
			byID := map[int64]db.ListLPOffersForCorpRow{}
			for _, o := range offers {
				byID[o.OfferID] = o
			}
			for _, c := range cands {
				o, ok := byID[c.OfferID]
				if !ok {
					continue
				}
				pick.Candidates = append(pick.Candidates, profitLPChoice{
					Value: fmt.Sprintf("%d:%d", c.OfferID, c.Quantity),
					Label: fmt.Sprintf("%s × %s (%s ISK)", esi.FormatInt(c.Quantity*max(o.Quantity, 1)), app.typeNameOrID(ctx, o.TypeID), esi.FormatISK(float64(c.Quantity)*o.IskCost)),
				})
			}
		}
		view.LPPicks = append(view.LPPicks, pick)
	}
	return nil
}

// handleProfitLPPick settles an ambiguous purchase with the offer the
// player says they bought; the pool refolds on its next pass.
func (app *Application) handleProfitLPPick(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	back := app.flashBack(w, r, "/profit/")
	if userID == 0 || r.ParseForm() != nil {
		back("That form didn't come through. Try again.")
		return
	}
	chars, err := app.queries.ListCharactersByUser(ctx, userID)
	if err != nil {
		back("Couldn't save that right now.")
		return
	}
	ids := make([]int64, 0, len(chars))
	for _, c := range chars {
		ids = append(ids, c.CharacterID)
	}
	id, _ := strconv.ParseInt(r.Form.Get("id"), 10, 64)
	p, err := app.queries.GetLPPurchase(ctx, db.GetLPPurchaseParams{ID: id, CharacterIds: ids})
	if err != nil || p.Tier != "ambiguous" {
		back("That purchase can't be settled here.")
		return
	}
	offerRaw, kRaw, _ := strings.Cut(r.Form.Get("choice"), ":")
	offerID, _ := strconv.ParseInt(offerRaw, 10, 64)
	k, _ := strconv.ParseInt(kRaw, 10, 64)
	var cands []lpCandidate
	_ = json.Unmarshal([]byte(p.Candidates), &cands)
	ok := false
	for _, c := range cands {
		ok = ok || (c.OfferID == offerID && c.Quantity == k)
	}
	offers, _ := app.queries.ListLPOffersForCorp(ctx, p.CorporationID)
	var offer db.ListLPOffersForCorpRow
	for _, o := range offers {
		if o.OfferID == offerID {
			offer = o
		}
	}
	if !ok || offer.OfferID == 0 {
		back("Pick one of the offers listed.")
		return
	}
	fill := db.InsertLPPurchaseParams{}
	app.fillLPPurchase(ctx, &fill, p.CorporationID, offer, k)
	if err := app.queries.ConfirmLPPurchase(ctx, db.ConfirmLPPurchaseParams{
		ID: p.ID, OfferID: fill.OfferID, Quantity: fill.Quantity, ProductTypeID: fill.ProductTypeID,
		Units: fill.Units, IskPaid: fill.IskPaid, ReqValue: fill.ReqValue,
	}); err != nil {
		logging.Errorf("profit: lp pick for user %d: %v", userID, err)
		back("Couldn't save that right now.")
		return
	}
	pool := app.profitPoolForCharacter(ctx, p.CharacterID)
	_ = app.queries.ClearLedgerPoolMembers(ctx, db.ClearLedgerPoolMembersParams{PoolKind: pool.kind, PoolID: pool.id})
	back("Saved. That store's figures are recomputed on the next sync.")
}

// handleProfitLPValue saves the player's own ISK per LP for the
// sensitivity lines, with their other preferences.
func (app *Application) handleProfitLPValue(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	back := app.flashBack(w, r, "/profit/")
	if userID == 0 || r.ParseForm() != nil {
		back("That form didn't come through. Try again.")
		return
	}
	raw := strings.ReplaceAll(strings.TrimSpace(r.Form.Get("lp_value")), ",", "")
	v := 0.0
	if raw != "" {
		var err error
		v, err = strconv.ParseFloat(raw, 64)
		if err != nil || v < 0 || v > 1e6 || math.IsNaN(v) {
			back("Give ISK per LP as a number, or leave it blank.")
			return
		}
	}
	if err := app.queries.SetUserLPValue(ctx, db.SetUserLPValueParams{UserID: userID, LpIskValue: v}); err != nil {
		logging.Errorf("profit: lp value for user %d: %v", userID, err)
		back("Couldn't save that right now.")
		return
	}
	back("Saved.")
}
