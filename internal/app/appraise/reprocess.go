package appraise

// The reprocessing view: what an appraisal's lines would
// yield if reprocessed, valued at the appraisal's own basis
// from stored data only -- the SDE's stored reprocessing
// outputs (schema 066) priced through the same basis the
// appraisal itself used. The refine rate is a percent the
// reader controls; its presets are a max-skills perfect
// refine and a structure rate, and the choice persists like
// the pricing basis does. Ore stacks below one refine batch
// can be pruned from the yield instead of valued in part.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// refinePreset is one named refine-rate preset.
type refinePreset struct {
	Key   string
	Label string
	Rate  float64
}

var refinePresets = []refinePreset{
	{Key: "perfect", Label: "Perfect refine (max skills)", Rate: 100},
	{Key: "structure", Label: "Structure rate", Rate: 55.5},
}

// refineSettings is the reader's refine rate and prune choice.
type refineSettings struct {
	Rate  float64
	Prune bool
}

// reprocessLine is one appraised line in the reprocess view.
type reprocessLine struct {
	Name         string
	Qty          string
	Outputs      string // "Tritanium × 1,200 · Pyerite × 300"
	ReprocessVal string // ISK, "" when not valued
	DirectVal    string // ISK, "" when unpriced directly
	Note         string
}

// reprocessView is a snapshot's reprocessing, shaped for the
// template.
type reprocessView struct {
	Lines          []reprocessLine
	NonReprocess   []string // lines with no reprocessing outputs, named
	TotalReprocess string
	TotalDirect    string
	TotalVol       string
	ISKPerM3       string
	Rate           float64
	RateText       string
	Prune          bool
	Presets        []refinePreset
	BasisText      string
	BackURL        string
}

// formatVolume words a volume in m³ with up to two decimals,
// trimmed.
func formatVolume(v float64) string {
	s := strconv.FormatFloat(v, 'f', 2, 64)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	if s == "" || s == "-0" {
		s = "0"
	}
	return s
}

// rememberedRefine reads the persisted refine settings: the
// account preference when signed in, a signed cookie when not.
func (app *page) rememberedRefine(ctx context.Context, r *http.Request, userID int64) refineSettings {
	out := refineSettings{Rate: 100, Prune: true}
	if userID > 0 {
		if pref, err := app.queries.GetUserAppraisalPref(ctx, userID); err == nil {
			if pref.RefineRate > 0 {
				out.Rate = pref.RefineRate
			}
			out.Prune = pref.PruneOres
		}
		return out
	}
	if c, err := r.Cookie(anonRefineCookie); err == nil && c.Value != "" {
		if secret, err := app.anonHistorySecret(ctx); err == nil {
			if payload, ok := verifySignedValue(secret, c.Value); ok {
				parts := strings.Split(payload, "|")
				if len(parts) == 2 {
					if v, err := strconv.ParseFloat(parts[0], 64); err == nil && v > 0 {
						out.Rate = v
					}
					out.Prune = parts[1] == "1"
				}
			}
		}
	}
	return out
}

// rememberRefine persists the refine settings just used.
func (app *page) rememberRefine(ctx context.Context, w http.ResponseWriter, r *http.Request, userID int64, s refineSettings) {
	if userID > 0 {
		if err := app.queries.UpsertUserAppraisalRefine(ctx, db.UpsertUserAppraisalRefineParams{
			UserID: userID, RefineRate: s.Rate, PruneOres: s.Prune,
		}); err != nil {
			logging.Errorf("appraisal: remember refine: %v", err)
		}
		return
	}
	secret, err := app.anonHistorySecret(ctx)
	if err != nil {
		return
	}
	prune := "0"
	if s.Prune {
		prune = "1"
	}
	http.SetCookie(w, &http.Cookie{
		Name:     anonRefineCookie,
		Value:    signValue(secret, strconv.FormatFloat(s.Rate, 'f', -1, 64)+"|"+prune),
		Path:     "/",
		MaxAge:   int(anonAppraisalTTL / time.Second),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

const anonRefineCookie = "evesynapse-appraisal-refine"

// refineFromQuery reads rate/preset/prune parameters over the
// remembered settings.
func (app *page) refineFromQuery(base refineSettings, r *http.Request) refineSettings {
	out := base
	q := r.URL.Query()
	if key := q.Get("preset"); key != "" {
		for _, p := range refinePresets {
			if p.Key == key {
				out.Rate = p.Rate
			}
		}
	}
	if raw := strings.TrimSpace(q.Get("rate")); raw != "" {
		if v, err := strconv.ParseFloat(strings.TrimSuffix(raw, "%"), 64); err == nil && v > 0 && v <= 100 {
			out.Rate = v
		}
	}
	if raw := q.Get("prune"); raw != "" {
		out.Prune = raw == "1" || raw == "on" || raw == "true"
	}
	return out
}

// computeReprocess builds the reprocess view for a snapshot
// under refine settings, pricing outputs at the snapshot's
// basis from stored data only.
func (app *page) computeReprocess(ctx context.Context, snap *snapshot, s refineSettings) (*reprocessView, error) {
	basis := snap.Basis.basis()
	if basis.HubName == "" {
		if name, ok := app.deps.MarketRegionName(basis.RegionID); ok {
			basis.HubName = name
		}
	}
	view := &reprocessView{
		Rate: s.Rate, Prune: s.Prune, Presets: refinePresets,
		BasisText: basis.basisText(),
	}
	view.RateText = strconv.FormatFloat(s.Rate, 'f', -1, 64)

	var typeIDs []int64
	seen := map[int64]bool{}
	for _, ln := range snap.Lines {
		if !seen[ln.TypeID] {
			seen[ln.TypeID] = true
			typeIDs = append(typeIDs, ln.TypeID)
		}
	}
	outputs := map[int64][]reprocessOutput{}
	portions := map[int64]int64{}
	names := map[int64]string{}
	if len(typeIDs) > 0 {
		rows, err := app.queries.ListReprocessingOutputsForTypes(ctx, typeIDs)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			outputs[row.TypeID] = append(outputs[row.TypeID], reprocessOutput{materialID: row.MaterialTypeID, qty: row.Quantity})
		}
		phys, err := app.queries.ListSDETypePhysicsByIDs(ctx, typeIDs)
		if err != nil {
			return nil, err
		}
		for _, row := range phys {
			portions[row.TypeID] = row.PortionSize
		}
	}

	// Aggregate the yields across lines, then price each
	// output type once at the appraisal's basis.
	yieldQty := map[int64]float64{}
	lineYield := map[int]map[int64]float64{}
	for i, ln := range snap.Lines {
		outs, ok := outputs[ln.TypeID]
		if !ok || len(outs) == 0 {
			continue
		}
		portion := portions[ln.TypeID]
		batches, pruned := reprocessBatches(ln.Qty, portion, s.Prune)
		ly := map[int64]float64{}
		for _, o := range outs {
			q := batches * float64(o.qty) * s.Rate / 100
			if q > 0 {
				ly[o.materialID] = q
				yieldQty[o.materialID] += q
			}
		}
		lineYield[i] = ly
		_ = pruned
	}
	var outIDs []int64
	for id := range yieldQty {
		outIDs = append(outIDs, id)
	}
	sort.Slice(outIDs, func(a, b int) bool { return outIDs[a] < outIDs[b] })
	unitPrice := map[int64]float64{}
	if len(outIDs) > 0 {
		lines := make([]module.AppraisalLine, 0, len(outIDs))
		for _, id := range outIDs {
			lines = append(lines, module.AppraisalLine{TypeID: id, Quantity: 1, Included: true})
			names[id] = app.deps.TypeName(ctx, id)
		}
		res, err := app.deps.AppraiseLinesWithBasis(ctx, lines, basis.toModule())
		if err != nil {
			return nil, err
		}
		for j, id := range outIDs {
			if j < len(res.Lines) && res.Lines[j].Priced {
				unitPrice[id] = res.Lines[j].UnitPrice
			}
		}
	}

	var totalReprocess, totalDirect float64
	for i, ln := range snap.Lines {
		rl := reprocessLine{Name: ln.Name, Qty: esi.FormatInt(ln.Qty)}
		if ln.Priced {
			rl.DirectVal = esi.FormatISK(ln.LineValue)
			totalDirect += ln.LineValue
		}
		outs, ok := outputs[ln.TypeID]
		if !ok || len(outs) == 0 {
			rl.Note = "Does not reprocess."
			view.NonReprocess = append(view.NonReprocess, ln.Name)
			view.Lines = append(view.Lines, rl)
			continue
		}
		portion := portions[ln.TypeID]
		_, pruned := reprocessBatches(ln.Qty, portion, s.Prune)
		var parts []string
		var val float64
		ly := lineYield[i]
		ids := make([]int64, 0, len(ly))
		for id := range ly {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(a, b int) bool { return ids[a] < ids[b] })
		for _, id := range ids {
			name := names[id]
			if name == "" {
				name = app.deps.TypeName(ctx, id)
			}
			parts = append(parts, fmt.Sprintf("%s × %s", name, esi.FormatInt(int64(ly[id]+0.5))))
			val += ly[id] * unitPrice[id]
		}
		rl.Outputs = strings.Join(parts, " · ")
		if pruned {
			rl.Note = "Below one refine batch — pruned."
		} else if portion > 1 && ln.Qty%portion != 0 {
			rl.Note = fmt.Sprintf("Only full %s-unit batches reprocess; the rest is left over.", esi.FormatInt(portion))
		}
		if val > 0 {
			rl.ReprocessVal = esi.FormatISK(val)
			totalReprocess += val
		}
		view.Lines = append(view.Lines, rl)
	}
	view.TotalReprocess = esi.FormatISK(totalReprocess)
	view.TotalDirect = esi.FormatISK(totalDirect)
	if snap.TotalVolume > 0 {
		view.TotalVol = formatVolume(snap.TotalVolume)
		if snap.TotalValue > 0 {
			view.ISKPerM3 = esi.FormatISK(snap.TotalValue / snap.TotalVolume)
		}
	}
	return view, nil
}

type reprocessOutput struct {
	materialID int64
	qty        int64
}

// reprocessBatches works out how many portions of a stack
// reprocess: full batches only, unless pruning is off, in
// which case a partial batch counts in proportion. A stack
// below one batch with pruning on is pruned entirely.
func reprocessBatches(qty, portion int64, prune bool) (batches float64, pruned bool) {
	if portion <= 1 {
		return float64(qty), false
	}
	if qty < portion && prune {
		return 0, true
	}
	if prune {
		return float64(qty / portion), false
	}
	return float64(qty) / float64(portion), false
}

// handleAppraiseReprocess renders the reprocess sub-view of
// one shared appraisal.
func (app *page) handleAppraiseReprocess(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := &PageData{ShellData: app.deps.PublicShell(ctx)}
	token := chi.URLParam(r, "token")
	row, err := app.queries.GetUserAppraisalByShareToken(ctx, token)
	if err != nil {
		data.Error = "That shared appraisal could not be found."
		basis := app.basisFromValues(app.rememberedBasis(ctx, r, app.deps.UserID(ctx)), r.URL.Query())
		data.Appraise = app.consoleView(data, basis)
		app.deps.Render(ctx, w, http.StatusNotFound, "market_appraise.html", data)
		return
	}
	res, err := app.storedResult(row)
	if err != nil {
		logging.Errorf("appraisal: reprocess snapshot: %v", err)
		data.Error = "That shared appraisal could not be read."
		data.Appraise = app.consoleView(data, app.storedBasis(row))
		app.deps.Render(ctx, w, http.StatusInternalServerError, "market_appraise.html", data)
		return
	}
	res.Shared = true
	res.ShareURL = "/market/appraise/s/" + row.ShareToken + "/"
	if row.IsAnonymous {
		_ = app.queries.TouchUserAppraisalByToken(ctx, token)
	}
	var snap snapshot
	if err := json.Unmarshal(row.Snapshot, &snap); err != nil {
		logging.Errorf("appraisal: reprocess snapshot decode: %v", err)
		data.Error = "That shared appraisal could not be read."
		data.Appraise = app.consoleView(data, app.storedBasis(row))
		app.deps.Render(ctx, w, http.StatusInternalServerError, "market_appraise.html", data)
		return
	}
	userID := app.deps.UserID(ctx)
	settings := app.refineFromQuery(app.rememberedRefine(ctx, r, userID), r)
	if r.URL.Query().Has("refine") {
		// The refine form was submitted: an absent prune
		// checkbox means off, not "keep the remembered one".
		settings.Prune = r.URL.Query().Has("prune")
		app.rememberRefine(ctx, w, r, userID, settings)
	}
	// The paste console rides along on this view too, with the
	// appraisal's own basis in its controls, like every other
	// rendering of the page.
	view := &appraiseView{SignedIn: data.LoggedIn, Result: res, Basis: res.Basis, Hubs: app.hubOptions(res.Basis.RegionID)}
	rp, err := app.computeReprocess(ctx, &snap, settings)
	if err != nil {
		logging.Errorf("appraisal: reprocess compute: %v", err)
		data.Error = "That reprocessing view could not be run."
		data.Appraise = view
		app.deps.Render(ctx, w, http.StatusOK, "market_appraise.html", data)
		return
	}
	rp.BackURL = "/market/appraise/s/" + row.ShareToken + "/"
	view.Reprocess = rp
	data.Appraise = view
	app.deps.Render(ctx, w, http.StatusOK, "market_appraise.html", data)
}
