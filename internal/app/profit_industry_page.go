package app

// The Profit page's Industry section and blueprint pages (schema
// 111): per blueprint, (a) realized, (b) at the time and (c) at
// today's prices, each with what covers it; a blueprint's jobs with
// the player's manual links; the facility bonuses that derive
// materials. Read from industry_bp_summary and bounded job lists.

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

const industryRowCap = 100

type profitBlueprintRow struct {
	Name, Href                  string
	Runs, Built, Sold, Lost     string
	Realized, RealizedDir       string
	RealizedNotes               []string
	AtTime, AtTimeDir, AtTimeNo string // AtTimeNo: why there is no figure
	AtTimeNote                  string
	Today, TodayDir, TodayNo    string
	TodayNote                   string
	Change, ChangeDir           string
}

type profitFacilityRow struct {
	ID         int64
	Name, Jobs string
	Bonus      string
}

type profitRegionChoice struct {
	ID       int64
	Name     string
	Selected bool
}

// profitPoolMembers resolves a pool key to the character ids in it,
// once the player is known to own it.
func (app *Application) profitPoolMembers(ctx context.Context, userID int64, kind string, id int64) ledgerPool {
	switch kind {
	case poolAcct:
		p := ledgerPool{kind: poolAcct, id: userID, ownerKind: ownerChar, userID: userID}
		if chars, err := app.queries.ListCharactersByUser(ctx, userID); err == nil {
			for _, c := range chars {
				p.members = append(p.members, c.CharacterID)
			}
		}
		return p
	case ownerCorp:
		return ledgerPool{kind: ownerCorp, id: id, ownerKind: ownerCorp, members: []int64{id}}
	}
	return ledgerPool{kind: ownerChar, id: id, ownerKind: ownerChar, members: []int64{id}, userID: userID}
}

func (app *Application) attachIndustry(ctx context.Context, view *profitView, userID int64, poolKind string, poolIDs []int64, prefs profitPrefs) error {
	for _, r := range marketRegions {
		view.Regions = append(view.Regions, profitRegionChoice{ID: r.ID, Name: r.Name, Selected: r.ID == prefs.IndustryRegion})
	}
	view.IndustrySideBuy = prefs.IndustrySide == module.AppraisalModeBuy
	view.IndustryRegion, _ = marketRegionName(prefs.IndustryRegion)
	rows, err := app.queries.ListIndustrySummary(ctx, db.ListIndustrySummaryParams{PoolKind: poolKind, PoolIds: poolIDs, RowCap: industryRowCap})
	if err != nil {
		return err
	}
	for _, s := range rows {
		row := profitBlueprintRow{
			Name: app.typeNameOrID(ctx, s.ProductTypeID),
			Href: fmt.Sprintf("/profit/blueprint/?pool=%s-%d&bp=%d", s.PoolKind, s.PoolID, s.BlueprintTypeID),
			Runs: esi.FormatInt(s.Runs), Built: esi.FormatInt(s.Units), Sold: esi.FormatInt(s.UnitsSold),
		}
		if s.UnitsLost > 0 {
			row.Lost = esi.FormatInt(s.UnitsLost)
		}
		if s.UnitsSold+s.UnitsLost > 0 {
			a := s.ARevenue - s.ATax - s.ACost
			row.Realized, row.RealizedDir = signedISK(a), dirOf(a)
		}
		if unsold := s.Units - s.UnitsSold - s.UnitsLost; unsold > 0 {
			row.RealizedNotes = append(row.RealizedNotes, esi.FormatInt(unsold)+" units not sold yet")
		}
		if s.UnitsMixed > 0 {
			row.RealizedNotes = append(row.RealizedNotes, esi.FormatInt(s.UnitsMixed)+" units sold beside bought stock of the item")
		}
		if s.UnitsAtMarket > 0 {
			row.RealizedNotes = append(row.RealizedNotes, esi.FormatInt(s.UnitsAtMarket)+" units used materials you didn't buy here, priced at the install day")
		}
		switch s.Invention {
		case "found":
			row.RealizedNotes = append(row.RealizedNotes, fmt.Sprintf("includes %s ISK of invention, %s per run", esi.FormatISK(s.InventionCost), esi.FormatISK(s.InventionPerRun)))
		case "missing":
			row.RealizedNotes = append(row.RealizedNotes, "no invention of this blueprint in your history (copies bought, or invented earlier), so invention isn't counted")
		}
		b := s.BOutput - s.BInputs - s.BJob
		if s.BPriced {
			row.AtTime, row.AtTimeDir = signedISK(b), dirOf(b)
			if s.BAtLanding {
				row.AtTimeNote = "some inputs had no price on their install day, so the price when the job was recorded is used"
			}
		} else {
			row.AtTimeNo = "not every input or the output had a price"
		}
		switch {
		case !s.CAsOf.Valid:
			row.TodayNo = "not priced yet"
		case !s.CPriced:
			row.TodayNo = fmt.Sprintf("no price for %s in %s", app.typeNameOrID(ctx, s.CMissingTypeID), marketRegionLabel(s.RegionID))
		default:
			row.Today, row.TodayDir = signedISK(s.CTotal), dirOf(s.CTotal)
			row.TodayNote = "as of " + ageText(time.Now(), s.CAsOf.Time)
			if s.CFallback {
				row.TodayNote += ", partly " + fallbackSourceLabel
			}
			if s.BPriced {
				d := s.CTotal - b
				row.Change, row.ChangeDir = signedISK(d), dirOf(d)
			}
		}
		view.Blueprints = append(view.Blueprints, row)
	}
	if len(poolIDs) == 1 && (poolKind == poolAcct || poolKind == ownerChar) {
		pool := app.profitPoolMembers(ctx, userID, poolKind, poolIDs[0])
		bonuses := map[int64]float64{}
		if rows, err := app.queries.ListUserFacilityBonuses(ctx, userID); err == nil {
			for _, b := range rows {
				bonuses[b.FacilityID] = b.MaterialBonus
			}
		}
		if facs, err := app.queries.ListPoolFacilities(ctx, db.ListPoolFacilitiesParams{OwnerKind: ownerChar, OwnerIds: pool.members}); err == nil {
			for _, f := range facs {
				row := profitFacilityRow{ID: f.FacilityID, Name: app.econLocationTitle(ctx, f.FacilityID), Jobs: esi.FormatInt(f.Jobs)}
				if b, ok := bonuses[f.FacilityID]; ok {
					row.Bonus = strconv.FormatFloat(b, 'f', -1, 64)
				}
				view.Facilities = append(view.Facilities, row)
			}
		}
	}
	return nil
}

// profitBlueprintView is a blueprint's jobs in one pool.
type profitBlueprintView struct {
	Pool, Name, Back string
	Jobs             []profitJobRow
	Sells            []profitSellChoice
	Orders           []profitSellChoice // open sell orders of the item a job can watch
	KillLost         string             // units of the item lost on killmails since the first job delivered
	KillMarked       string             // units already marked lost here
	TypeID           int64
	BP               int64
}

type profitJobRow struct {
	JobID                          int64
	Delivered, Runs, Units, Sold   string
	Lost, Mixed, Revenue, Unsold   string
	OnContract                     string
	Recipe, Cost, AtTime, Facility string
	Watching                       string // the order watched for this job
	HasUnsold                      bool
}

type profitSellChoice struct {
	ID    int64
	Label string
}

func (app *Application) handleProfitBlueprint(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.page(ctx)
	userID := app.userID(ctx)
	kind, idRaw, _ := strings.Cut(module.QueryValue(r.URL.Query(), "pool"), "-")
	id, _ := strconv.ParseInt(idRaw, 10, 64)
	bp, _ := strconv.ParseInt(module.QueryValue(r.URL.Query(), "bp"), 10, 64)
	if userID == 0 || bp <= 0 || !app.ownsProfitPool(ctx, userID, kind, id) {
		http.NotFound(w, r)
		return
	}
	pool := app.profitPoolMembers(ctx, userID, kind, id)
	view := &profitBlueprintView{Pool: kind + "-" + strconv.FormatInt(id, 10), Back: "/profit/", BP: bp}
	jobs, err := app.queries.ListPoolJobsForBlueprint(ctx, db.ListPoolJobsForBlueprintParams{
		PoolKind: pool.kind, PoolID: pool.id, OwnerKind: pool.ownerKind, OwnerIds: pool.members, BlueprintTypeID: bp,
	})
	if err != nil {
		logging.Errorf("profit: blueprint %d jobs: %v", bp, err)
		data.Error = "Jobs are unavailable right now. Try again shortly."
	}
	if sde, err := app.queries.GetSDEBlueprint(ctx, bp); err == nil {
		view.TypeID = sde.ProductTypeID
		view.Name = app.typeNameOrID(ctx, sde.ProductTypeID)
	}
	now := time.Now()
	for _, j := range jobs {
		row := profitJobRow{
			JobID: j.JobID, Delivered: ageText(now, j.At), Runs: esi.FormatInt(j.Runs), Units: esi.FormatInt(j.Units),
			Sold: esi.FormatInt(j.Sold), Revenue: esi.FormatISK(j.Revenue), Cost: esi.FormatISK(j.JobCost),
			Facility: app.econLocationTitle(ctx, j.FacilityID),
		}
		if j.Lost > 0 {
			row.Lost = esi.FormatInt(j.Lost)
		}
		if j.Mixed > 0 {
			row.Mixed = esi.FormatInt(j.Mixed)
		}
		if j.OnContract > 0 {
			row.OnContract = esi.FormatInt(j.OnContract)
		}
		if u := j.Units - j.Sold - j.Lost; u > 0 {
			row.Unsold, row.HasUnsold = esi.FormatInt(u), true
		}
		me := "ME unknown, 0 assumed"
		if j.MeSource == "at_delivery" {
			me = fmt.Sprintf("ME %d", j.Me)
		}
		bonus := "no structure bonus assumed"
		if j.BonusSource == "user_declared" {
			bonus = strconv.FormatFloat(j.MaterialBonus, 'f', -1, 64) + "% structure bonus"
		}
		row.Recipe = me + ", " + bonus
		switch j.InputsSource {
		case "unpriced":
			row.AtTime = "not priced"
		default:
			row.AtTime = esi.FormatISK(j.InputsAtTime)
			if j.InputsSource == "at_landing" {
				row.AtTime += " (partly at the price when recorded)"
			}
		}
		view.Jobs = append(view.Jobs, row)
	}
	app.attachJobFallbacks(ctx, view, pool, jobs, now)
	if view.TypeID > 0 {
		if sells, err := app.queries.ListRecentSellsOfType(ctx, db.ListRecentSellsOfTypeParams{OwnerKind: pool.ownerKind, OwnerIds: pool.members, TypeID: view.TypeID}); err == nil {
			for _, s := range sells {
				view.Sells = append(view.Sells, profitSellChoice{ID: s.TransactionID,
					Label: fmt.Sprintf("%s units at %s ISK, %s", esi.FormatInt(s.Quantity), esi.FormatISK(s.UnitPrice), ageText(now, s.At))})
			}
		}
	}
	data.ProfitBlueprint = view
	app.render(ctx, w, http.StatusOK, "profit_blueprint.html", data)
}

// handleProfitJobEvent records the player's word on a job's output:
// units sold off-market at a price, units lost, or a market sale that
// was this job's output. The pool refolds on its next pass.
func (app *Application) handleProfitJobEvent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	if userID == 0 || r.ParseForm() != nil {
		http.Redirect(w, r, "/profit/", http.StatusSeeOther)
		return
	}
	kind, idRaw, _ := strings.Cut(r.Form.Get("pool"), "-")
	poolID, _ := strconv.ParseInt(idRaw, 10, 64)
	bp, _ := strconv.ParseInt(r.Form.Get("bp"), 10, 64)
	back := app.flashBack(w, r, fmt.Sprintf("/profit/blueprint/?pool=%s-%d&bp=%d", kind, poolID, bp))
	if !app.ownsProfitPool(ctx, userID, kind, poolID) {
		back("That isn't one of your pools.")
		return
	}
	pool := app.profitPoolMembers(ctx, userID, kind, poolID)
	jobID, _ := strconv.ParseInt(r.Form.Get("job_id"), 10, 64)
	job, err := app.queries.GetPoolJob(ctx, db.GetPoolJobParams{OwnerKind: pool.ownerKind, OwnerIds: pool.members, JobID: jobID})
	if err != nil {
		back("That job isn't in these books.")
		return
	}
	ev := db.InsertJobManualEventParams{PoolKind: pool.kind, PoolID: pool.id, JobID: jobID, Kind: r.Form.Get("kind"), At: time.Now().UTC()}
	units, _ := strconv.ParseInt(strings.ReplaceAll(r.Form.Get("units"), ",", ""), 10, 64)
	switch ev.Kind {
	case "sale", "lost":
		if units <= 0 || units > job.Units {
			back("Give a number of units up to what the job made.")
			return
		}
		ev.Units = units
		if ev.Kind == "sale" {
			price, err := strconv.ParseFloat(strings.ReplaceAll(r.Form.Get("unit_price"), ",", ""), 64)
			if err != nil || price < 0 || price > 1e14 || math.IsNaN(price) {
				back("Give the price each unit sold for.")
				return
			}
			ev.UnitPrice = price
		}
	case "watch":
		orderID, _ := strconv.ParseInt(r.Form.Get("order_id"), 10, 64)
		o, err := app.queries.GetPoolOpenSellOrder(ctx, db.GetPoolOpenSellOrderParams{CharacterIds: pool.members, OrderID: orderID})
		if err != nil || o.TypeID != job.ProductTypeID || pool.ownerKind != ownerChar {
			back("That isn't an open sell order of this job's output.")
			return
		}
		if err := app.queries.InsertJobOrderWatch(ctx, db.InsertJobOrderWatchParams{PoolKind: pool.kind, PoolID: pool.id, JobID: jobID, CharacterID: o.CharacterID, OrderID: o.OrderID}); err != nil {
			logging.Errorf("profit: order watch for user %d: %v", userID, err)
			back("Couldn't save that right now.")
			return
		}
		back("Watching. What that order sells from now on counts as this job's output.")
		return
	case "link":
		txID, _ := strconv.ParseInt(r.Form.Get("transaction_id"), 10, 64)
		sell, err := app.queries.GetPoolSell(ctx, db.GetPoolSellParams{OwnerKind: pool.ownerKind, OwnerIds: pool.members, TransactionID: txID})
		if err != nil || sell.TypeID != job.ProductTypeID {
			back("That sale isn't of this job's output.")
			return
		}
		ev.TransactionID, ev.Units, ev.At = txID, sell.Quantity, sell.At
	default:
		back("That form didn't come through. Try again.")
		return
	}
	if err := app.queries.InsertJobManualEvent(ctx, ev); err != nil {
		logging.Errorf("profit: job event for user %d: %v", userID, err)
		back("Couldn't save that right now.")
		return
	}
	_ = app.queries.ClearLedgerPoolMembers(ctx, db.ClearLedgerPoolMembersParams{PoolKind: pool.kind, PoolID: pool.id})
	back("Saved. The blueprint's figures are recomputed on the next sync.")
}

// handleProfitFacility sets (or clears) the material bonus a player
// declares for a facility, rederives the materials of their jobs
// there, and has their books refold on the next pass.
func (app *Application) handleProfitFacility(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	back := app.flashBack(w, r, "/profit/")
	if userID == 0 || r.ParseForm() != nil {
		back("That form didn't come through. Try again.")
		return
	}
	facility, _ := strconv.ParseInt(r.Form.Get("facility_id"), 10, 64)
	raw := strings.TrimSuffix(strings.TrimSpace(r.Form.Get("bonus")), "%")
	chars, err := app.queries.ListCharactersByUser(ctx, userID)
	if err != nil || facility <= 0 {
		back("That form didn't come through. Try again.")
		return
	}
	ids := make([]int64, 0, len(chars))
	for _, c := range chars {
		ids = append(ids, c.CharacterID)
	}
	bonus, source := 0.0, "none_assumed"
	if raw == "" {
		err = app.queries.DeleteFacilityBonus(ctx, db.DeleteFacilityBonusParams{UserID: userID, FacilityID: facility})
	} else {
		bonus, err = strconv.ParseFloat(raw, 64)
		if err != nil || bonus < 0 || bonus > 50 || math.IsNaN(bonus) {
			back("A material bonus is a percent from 0 to 50.")
			return
		}
		source = "user_declared"
		err = app.queries.UpsertFacilityBonus(ctx, db.UpsertFacilityBonusParams{UserID: userID, FacilityID: facility, MaterialBonus: bonus})
	}
	if err != nil {
		logging.Errorf("profit: facility bonus for user %d: %v", userID, err)
		back("Couldn't save that right now.")
		return
	}
	jobs, err := app.queries.ListJobsAtFacility(ctx, db.ListJobsAtFacilityParams{OwnerKind: ownerChar, OwnerIds: ids, FacilityID: facility})
	if err == nil {
		for _, j := range jobs {
			mats, err := app.queries.ListSDEBlueprintMaterials(ctx, j.BlueprintTypeID)
			if err != nil {
				continue
			}
			for _, m := range mats {
				_ = app.queries.UpdateJobMaterialQuantity(ctx, db.UpdateJobMaterialQuantityParams{
					OwnerKind: j.OwnerKind, OwnerID: j.OwnerID, JobID: j.JobID, TypeID: m.MaterialTypeID,
					Quantity: jobMaterialQty(m.Quantity, j.Runs, j.Me, bonus),
				})
			}
			_ = app.queries.RecomputeJobInputsAtTime(ctx, db.RecomputeJobInputsAtTimeParams{OwnerKind: j.OwnerKind, OwnerID: j.OwnerID, JobID: j.JobID})
			_ = app.queries.SetJobBonus(ctx, db.SetJobBonusParams{OwnerKind: j.OwnerKind, OwnerID: j.OwnerID, JobID: j.JobID, MaterialBonus: bonus, BonusSource: source})
		}
	}
	_ = app.queries.ClearLedgerPoolMembers(ctx, db.ClearLedgerPoolMembersParams{PoolKind: poolAcct, PoolID: userID})
	for _, id := range ids {
		_ = app.queries.ClearLedgerPoolMembers(ctx, db.ClearLedgerPoolMembersParams{PoolKind: ownerChar, PoolID: id})
	}
	back("Saved. Industry figures are recomputed on the next sync.")
}

// attachJobFallbacks adds what the blueprint page offers where FIFO
// matching isn't the right answer: the open sell orders of the item a
// job's output can be tied to (and which one each job watches), and
// how many of the item the pool's characters lost on killmails since
// the first of these jobs delivered, beside how many are marked lost.
func (app *Application) attachJobFallbacks(ctx context.Context, view *profitBlueprintView, pool ledgerPool, jobs []db.ListPoolJobsForBlueprintRow, now time.Time) {
	if view.TypeID == 0 || len(jobs) == 0 || pool.ownerKind != ownerChar {
		return
	}
	if orders, err := app.queries.ListPoolOpenSellOrdersOfType(ctx, db.ListPoolOpenSellOrdersOfTypeParams{CharacterIds: pool.members, TypeID: view.TypeID}); err == nil {
		for _, o := range orders {
			view.Orders = append(view.Orders, profitSellChoice{ID: o.OrderID,
				Label: fmt.Sprintf("%s left at %s ISK, %s", esi.FormatInt(o.VolumeRemainLast), esi.FormatISK(o.ListedPrice), app.econLocationTitle(ctx, o.LocationID))})
		}
	}
	ids := make([]int64, len(jobs))
	first := jobs[0].At
	var marked int64
	for i, j := range jobs {
		ids[i] = j.JobID
		if j.At.Before(first) {
			first = j.At
		}
		marked += j.Lost
	}
	if watches, err := app.queries.ListJobOrderWatches(ctx, db.ListJobOrderWatchesParams{PoolKind: pool.kind, PoolID: pool.id, JobIds: ids}); err == nil {
		byJob := map[int64]string{}
		for _, w := range watches {
			state := "open"
			if w.ClosedAt.Valid {
				state = "closed"
			}
			byJob[w.JobID] = fmt.Sprintf("order %d (%s), since %s", w.OrderID, state, ageText(now, w.CreatedAt))
		}
		for i := range view.Jobs {
			view.Jobs[i].Watching = byJob[view.Jobs[i].JobID]
		}
	}
	if k, err := app.queries.CountKillmailLossesOfType(ctx, db.CountKillmailLossesOfTypeParams{TypeID: view.TypeID, CharacterIds: pool.members, Since: first}); err == nil && k.Units > 0 {
		view.KillLost = fmt.Sprintf("%s on %s killmails", esi.FormatInt(k.Units), esi.FormatInt(k.Killmails))
		view.KillMarked = esi.FormatInt(marked)
	}
}
