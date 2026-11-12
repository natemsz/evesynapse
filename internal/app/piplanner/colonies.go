package piplanner

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
	"evesynapse/internal/piplan"
)

// The colony planner: a form describes the goal, the pilots and the
// planets in reach; solving runs once, on submit, and stores the
// answer under a token. The page and the colony view only read the
// stored plan.

// Skill type ids the planner reads.
const (
	skillInterplanetaryConsolidation = 2495
	skillCommandCenterUpgrades       = 2505
)

// planForm is what a solve was asked: stored with the plan, so the
// page shows the settings it was made with and a re-plan starts from
// them.
type planForm struct {
	Product     string  `json:"product"`
	PerDay      float64 `json:"perDay"`
	UseMine     bool    `json:"useMine"`
	CharIDs     []int64 `json:"charIds,omitempty"`
	Pilots      int     `json:"pilots"`
	PlanetsEach int     `json:"planetsEach"`
	CCLevel     int     `json:"ccLevel"`
	HomeID      int64   `json:"homeId,omitempty"`
	HomeName    string  `json:"homeName,omitempty"`
	Jumps       int     `json:"jumps"`
	Adapt       bool    `json:"adapt"`
	ResetHours  float64 `json:"resetHours"`
	HaulHours   float64 `json:"haulHours"`
	TaxPct      float64 `json:"taxPct"`
	Peak        float64 `json:"peak"`
	AllowBuy    bool    `json:"allowBuy"`
	SellSurplus bool    `json:"sellSurplus"`
	// Pinned colonies: ones the player shaped in the colony view.
	// A re-plan keeps them exactly and plans the rest around them.
	Pinned []pinnedColony `json:"pinned,omitempty"`
}

type pinnedColony struct {
	Colony   *piplan.Colony     `json:"colony"`
	CharID   int64              `json:"charId,omitempty"`
	Peak     map[string]float64 `json:"peak,omitempty"`
	System   string             `json:"system,omitempty"`
	Jumps    int                `json:"jumps,omitempty"`
	Existing bool               `json:"existing,omitempty"`
}

// storedPlan is the solution with each colony's game template, made
// at solve time.
type storedPlan struct {
	Solution  *piplan.Solution `json:"solution"`
	Templates []string         `json:"templates"`
	Prices    string           `json:"prices"`
	MadeAt    time.Time        `json:"madeAt"`
}

func defaultForm() planForm {
	return planForm{Product: "Coolant", Pilots: 1, PlanetsEach: 6, CCLevel: 5, Jumps: 2,
		ResetHours: 24, HaulHours: 24, TaxPct: 10, Peak: piplan.DefaultPeakPerHead, SellSurplus: true, UseMine: true}
}

func clampF(v, lo, hi, def float64) float64 {
	if math.IsNaN(v) || v < lo || v > hi {
		return def
	}
	return v
}

func parseForm(r *http.Request) planForm {
	f := defaultForm()
	num := func(k string, def float64) float64 {
		v, err := strconv.ParseFloat(strings.TrimSpace(r.FormValue(k)), 64)
		if err != nil {
			return def
		}
		return v
	}
	if p := strings.TrimSpace(r.FormValue("product")); p != "" {
		f.Product = p
	}
	f.PerDay = clampF(num("perday", 0), 0, 1e7, 0)
	f.UseMine = r.FormValue("source") != "pilots"
	for _, raw := range r.Form["chars"] {
		if id, err := strconv.ParseInt(raw, 10, 64); err == nil && id > 0 {
			f.CharIDs = append(f.CharIDs, id)
		}
	}
	f.Pilots = int(clampF(num("pilots", 1), 1, 20, 1))
	f.PlanetsEach = int(clampF(num("planets_each", 6), 1, 6, 6))
	f.CCLevel = int(clampF(num("cc", 5), 0, 5, 5))
	f.HomeID, _ = strconv.ParseInt(r.FormValue("home_id"), 10, 64)
	f.HomeName = strings.TrimSpace(r.FormValue("home"))
	f.Jumps = int(clampF(num("jumps", 2), 0, 5, 2))
	f.Adapt = r.FormValue("mode") == "adapt"
	f.ResetHours = clampF(num("reset", 24), 1, 336, 24)
	f.HaulHours = clampF(num("haul", 24), 1, 336, 24)
	f.TaxPct = clampF(num("tax", 10), 0, 100, 10)
	f.Peak = clampF(num("peak", piplan.DefaultPeakPerHead), 50, 50000, piplan.DefaultPeakPerHead)
	f.AllowBuy = r.FormValue("buy") == "1"
	f.SellSurplus = r.FormValue("surplus") == "1"
	return f
}

// HandleSolve runs the optimizer for the submitted form, stores the
// plan and opens it.
func HandleSolve(deps module.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		f := parseForm(r)
		// A re-plan from the colony view: the base plan's settings,
		// with the colonies the player shaped pinned.
		if base := strings.TrimSpace(r.FormValue("base")); base != "" {
			if row, err := deps.DB().GetPIPlan(ctx, base); err == nil {
				var old planForm
				if json.Unmarshal(row.Request, &old) == nil {
					f = old
					f.Pinned = nil
				}
			}
			if raw := r.FormValue("pins"); raw != "" {
				var pins []pinnedColony
				if err := json.Unmarshal([]byte(raw), &pins); err == nil && len(pins) <= 40 {
					for _, p := range pins {
						if p.Colony != nil && len(p.Colony.Pins) <= 80 {
							f.Pinned = append(f.Pinned, p)
						}
					}
				}
			}
		}
		token, err := solveAndStore(ctx, deps, r, f)
		if err != nil {
			logging.Errorf("piplanner: solve: %v", err)
			deps.Flash(ctx, "The plan could not be made: "+err.Error())
			http.Redirect(w, r, "/industry/pi/", http.StatusSeeOther)
			return
		}
		deps.NoteActivity("pi_plan")
		if r.Header.Get("X-Requested-With") == "XMLHttpRequest" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"token": token, "url": "/industry/pi/?plan=" + token + "#plan"})
			return
		}
		http.Redirect(w, r, "/industry/pi/?plan="+token+"#plan", http.StatusSeeOther)
	}
}

func newToken() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func solveAndStore(ctx context.Context, deps module.Deps, r *http.Request, f planForm) (string, error) {
	if _, ok := piplan.Schematics[f.Product]; !ok {
		return "", errors.New("pick a P1 to P4 product")
	}
	req := piplan.Request{
		Target: f.Product, PerDay: f.PerDay, Peak: f.Peak,
		ProgramHours: f.ResetHours, HaulHours: f.HaulHours, TaxRate: f.TaxPct / 100,
		AllowBuy: f.AllowBuy, SellSurplus: f.SellSurplus, Adapt: f.Adapt,
	}
	prices, _, priceLine, _ := piPricesByName(ctx, deps, time.Now())
	req.Prices = prices

	// Pilots: the account's chosen characters with their skills, or
	// a stated number of pilots.
	userID := deps.UserID(ctx)
	var mine []db.Character
	if userID != 0 && f.UseMine {
		chars, _, _, err := deps.PickCharacter(ctx, r, "/industry/pi/")
		if err == nil {
			want := map[int64]bool{}
			for _, id := range f.CharIDs {
				want[id] = true
			}
			for _, ch := range chars {
				if len(want) == 0 || want[ch.CharacterID] {
					mine = append(mine, ch)
				}
			}
		}
	}
	if len(mine) > 0 {
		for _, ch := range mine {
			req.Characters = append(req.Characters, characterSlots(ctx, deps, ch, f))
		}
	} else {
		for i := 0; i < f.Pilots; i++ {
			req.Characters = append(req.Characters, piplan.Character{ID: int64(i + 1), Name: fmt.Sprintf("Pilot %d", i+1), MaxPlanets: f.PlanetsEach, CCLevel: f.CCLevel})
		}
	}

	// Planets: in reach of home, plus the colonies the pilots run.
	if f.HomeID != 0 {
		rows, err := deps.DB().ListPIPlanetsNear(ctx, db.ListPIPlanetsNearParams{HomeID: f.HomeID, MaxJumps: int32(f.Jumps), RowLimit: 400})
		if err != nil {
			return "", fmt.Errorf("read planets near home: %w", err)
		}
		perType := map[string]int{}
		for _, p := range rows {
			pt := piplan.PlanetTypeByID[p.TypeID]
			// The nearest few of each type are all a plan can want; more
			// only slow the search.
			if pt == "" || perType[pt] >= 6 {
				continue
			}
			perType[pt]++
			req.Candidates = append(req.Candidates, piplan.Candidate{
				PlanetID: p.ItemID, Name: p.Name, Type: pt, System: p.SystemName,
				Jumps: int(p.Jumps), RadiusKm: p.Radius / 1000,
			})
		}
		if len(req.Candidates) == 0 {
			return "", errors.New("no planets found near that system; the static data may still be importing")
		}
	}
	existing := existingColonies(ctx, deps, mine)
	if f.Adapt || f.HomeID == 0 {
		for _, e := range existing {
			if f.Adapt {
				req.Candidates = append(req.Candidates, e)
			} else if len(e.Peak) > 0 {
				// Fresh plan, no home: still offer the planets the pilots
				// know, with what their extractors measured there.
				e.Existing, e.OwnerID = nil, 0
				req.Candidates = append(req.Candidates, e)
			}
		}
	}
	// Measured yields carry over to the same planet when it is a
	// candidate near home.
	measured := map[int64]map[string]float64{}
	for _, e := range existing {
		if len(e.Peak) > 0 {
			measured[e.PlanetID] = e.Peak
		}
	}
	for i := range req.Candidates {
		if req.Candidates[i].Peak == nil {
			req.Candidates[i].Peak = measured[req.Candidates[i].PlanetID]
		}
	}
	for _, p := range f.Pinned {
		c := piplan.Candidate{
			PlanetID: p.Colony.PlanetID, Name: p.Colony.PlanetName, Type: p.Colony.PlanetType,
			RadiusKm: p.Colony.RadiusKm, Existing: p.Colony, OwnerID: p.CharID, Peak: p.Peak,
			System: p.System, Jumps: p.Jumps, Fixed: true,
		}
		if c.Name == "" {
			c.Name = "Any " + c.Type + " planet"
		}
		// A pinned colony replaces the candidate for its planet.
		kept := req.Candidates[:0]
		for _, k := range req.Candidates {
			if k.PlanetID == 0 || k.PlanetID != c.PlanetID || k.OwnerID != c.OwnerID {
				kept = append(kept, k)
			}
		}
		req.Candidates = append(kept, c)
	}

	sol, err := piplan.Optimize(req)
	if err != nil {
		return "", err
	}
	plan := storedPlan{Solution: sol, Prices: priceLine, MadeAt: time.Now().UTC()}
	for _, c := range sol.Colonies {
		t, terr := c.Colony.Template()
		if terr != nil {
			t = ""
		}
		plan.Templates = append(plan.Templates, t)
	}
	reqJSON, _ := json.Marshal(f)
	solJSON, err := json.Marshal(plan)
	if err != nil {
		return "", err
	}
	token := newToken()
	if err := deps.DB().InsertPIPlan(ctx, db.InsertPIPlanParams{Token: token, UserID: userID, Request: reqJSON, Solution: solJSON}); err != nil {
		return "", fmt.Errorf("store plan: %w", err)
	}
	_ = deps.DB().DeleteStalePIPlans(ctx)
	return token, nil
}

// characterSlots reads a pilot's planet allowance from stored skills;
// without them, the form's own figures stand in.
func characterSlots(ctx context.Context, deps module.Deps, ch db.Character, f planForm) piplan.Character {
	out := piplan.Character{ID: ch.CharacterID, Name: ch.Name, MaxPlanets: f.PlanetsEach, CCLevel: f.CCLevel}
	var skills esi.Skills
	if err := deps.StoredSnapshot(ctx, ch, esi.SnapSkills, &skills); err != nil {
		return out
	}
	out.MaxPlanets, out.CCLevel = 1, 0
	for _, s := range skills.Skills {
		switch s.SkillID {
		case skillInterplanetaryConsolidation:
			out.MaxPlanets = 1 + s.ActiveSkillLevel
		case skillCommandCenterUpgrades:
			out.CCLevel = s.ActiveSkillLevel
		}
	}
	return out
}

// existingColonies reads the pilots' colonies from stored snapshots,
// each with the yields its extractors measured.
func existingColonies(ctx context.Context, deps module.Deps, chars []db.Character) []piplan.Candidate {
	type owned struct {
		ch     db.Character
		colony esi.Colony
	}
	var all []owned
	var ids []int64
	for _, ch := range chars {
		var colonies esi.Colonies
		if !deps.LoadCorpSnapshot(ctx, ch.CharacterID, esi.SnapPlanets, &colonies) {
			continue
		}
		for _, c := range colonies {
			all = append(all, owned{ch, c})
			ids = append(ids, c.PlanetID)
		}
	}
	if len(all) == 0 {
		return nil
	}
	shape := map[int64]db.ListPIPlanetsByIDRow{}
	if rows, err := deps.DB().ListPIPlanetsByID(ctx, ids); err == nil {
		for _, row := range rows {
			shape[row.ItemID] = row
		}
	}
	var out []piplan.Candidate
	for _, o := range all {
		pt := displayPlanetType(o.colony.PlanetType)
		if _, ok := piplan.PlanetTypeIDs[pt]; !ok {
			continue
		}
		var layout esi.PlanetLayout
		if !deps.LoadCorpSnapshot(ctx, o.ch.CharacterID, esi.PlanetLayoutKind(o.colony.PlanetID), &layout) {
			continue
		}
		sh := shape[o.colony.PlanetID]
		radius := sh.Radius / 1000
		if radius <= 0 {
			radius = piplan.TypicalRadiusKm[pt]
		}
		col := piplan.FromESI(layout, pt, radius, int(o.colony.UpgradeLevel))
		name := sh.Name
		if name == "" {
			name = fmt.Sprintf("Planet #%d", o.colony.PlanetID)
		}
		col.PlanetID, col.PlanetName = o.colony.PlanetID, name
		peak := map[string]float64{}
		for _, p := range col.Pins {
			if p.Kind == piplan.PinExtractor && p.QtyPerCycle > 0 && p.Heads > 0 && p.Product != "" {
				peak[p.Product] = math.Max(peak[p.Product], piplan.PeakPerHeadHour(p.QtyPerCycle, p.Heads))
			}
		}
		out = append(out, piplan.Candidate{
			PlanetID: o.colony.PlanetID, Name: name, Type: pt, System: sh.SystemName,
			RadiusKm: radius, Existing: col, OwnerID: o.ch.CharacterID, Peak: peak,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// HandlePlanJSON serves a stored plan for the colony view.
func HandlePlanJSON(deps module.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		row, err := deps.DB().GetPIPlan(r.Context(), r.URL.Query().Get("plan"))
		if err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				logging.Errorf("piplanner: read plan: %v", err)
			}
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "private, max-age=300")
		_, _ = w.Write([]byte(`{"catalog":`))
		_, _ = w.Write(catalogJSON())
		_, _ = w.Write([]byte(`,"plan":`))
		_, _ = w.Write(row.Solution)
		_, _ = w.Write([]byte(`}`))
	}
}

// catalog is the static PI data the colony view offers choices from:
// what each planet type yields and what each factory kind can make.
type catalog struct {
	Yields     map[string][]string         `json:"yields"`
	Recipes    map[string][]string         `json:"recipes"` // pin kind -> products
	Tiers      map[string]string           `json:"tiers"`
	Holds      map[string][]piplan.PinKind `json:"holds"`
	Labels     map[piplan.PinKind]string   `json:"labels"`
	Programs   []float64                   `json:"programs"`
	MaxHeads   int                         `json:"maxHeads"`
	MinSep     float64                     `json:"minSep"`
	Step       float64                     `json:"step"`
	PeakAssume float64                     `json:"peakAssume"`
}

var (
	catalogOnce  sync.Once
	catalogBytes []byte
)

func catalogJSON() []byte {
	catalogOnce.Do(buildCatalog)
	return catalogBytes
}

func buildCatalog() {
	c := catalog{
		Yields: piplan.PlanetYields, Recipes: map[string][]string{}, Tiers: map[string]string{},
		Holds: map[string][]piplan.PinKind{}, Labels: map[piplan.PinKind]string{},
		Programs: piplan.ProgramLengths, MaxHeads: piplan.MaxHeads, MinSep: piplan.MinPinSeparation,
		Step: 0.0125, PeakAssume: piplan.DefaultPeakPerHead,
	}
	for name, s := range piplan.Schematics {
		k := string(piplan.FacilityPin(s.Facility))
		c.Recipes[k] = append(c.Recipes[k], name)
	}
	for k := range c.Recipes {
		sort.Strings(c.Recipes[k])
	}
	for name, cm := range piplan.AllCommodities {
		c.Tiers[name] = string(cm.Tier)
	}
	for _, pt := range piplan.PlanetTypes {
		for _, k := range piplan.PinKinds {
			if piplan.CanHold(pt, k) {
				c.Holds[pt] = append(c.Holds[pt], k)
			}
		}
	}
	for _, k := range piplan.PinKinds {
		c.Labels[k] = piplan.PinLabel(k)
	}
	catalogBytes, _ = json.Marshal(c)
}

// colonyAnswer is the colony view's recompute: the colony relinked,
// its fit, its rates and its game template.
type colonyAnswer struct {
	Colony   *piplan.Colony     `json:"colony"`
	Fit      piplan.Fit         `json:"fit"`
	Fits     bool               `json:"fits"`
	Out      map[string]float64 `json:"out"`
	In       map[string]float64 `json:"in"`
	Template string             `json:"template"`
	Crowded  []int              `json:"crowded"`
	Error    string             `json:"error,omitempty"`
}

// HandleColony recomputes one colony the player edited: links and
// routes laid again, fit and rates worked out, template rendered. It
// is the same math the optimizer used, so the view never disagrees
// with the plan.
func HandleColony(deps module.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var c piplan.Colony
		body := http.MaxBytesReader(w, r.Body, 256<<10)
		if err := json.NewDecoder(body).Decode(&c); err != nil || len(c.Pins) > 80 {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(colonyAnswer{Error: "That colony could not be read."})
			return
		}
		ans := recomputeColony(&c)
		_ = json.NewEncoder(w).Encode(ans)
	}
}

func recomputeColony(c *piplan.Colony) colonyAnswer {
	if _, ok := piplan.PlanetTypeIDs[c.PlanetType]; !ok {
		return colonyAnswer{Error: "Unknown planet type."}
	}
	c.CCLevel = max(0, min(5, c.CCLevel))
	if c.RadiusKm <= 0 || c.RadiusKm > 200000 {
		c.RadiusKm = piplan.TypicalRadiusKm[c.PlanetType]
	}
	// Drop what cannot stand here, clamp the rest.
	kept := c.Pins[:0]
	for _, p := range c.Pins {
		if !piplan.CanHold(c.PlanetType, p.Kind) {
			continue
		}
		p.Heads = max(0, min(piplan.MaxHeads, p.Heads))
		if p.Kind == piplan.PinExtractor && p.Heads < 1 {
			p.Heads = 1
		}
		if p.ProgramHours <= 0 || p.ProgramHours > 336 {
			p.ProgramHours = 24
		}
		kept = append(kept, p)
	}
	c.Pins = kept
	piplan.Relink(c)
	f := c.Fit()
	out, in := piplan.ColonyRates(c)
	t, err := c.Template()
	ans := colonyAnswer{Colony: c, Fit: f, Fits: f.Fits(), Out: out, In: in, Template: t, Crowded: c.Crowded()}
	if err != nil {
		ans.Error = err.Error()
	}
	return ans
}

// HandleSystemSuggest offers solar systems for the home box.
func HandleSystemSuggest(deps module.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		var items []module.SuggestItem
		if len(q) >= 2 {
			rows, err := deps.DB().SuggestSDESystems(r.Context(), db.SuggestSDESystemsParams{Q: q, Lim: 8})
			if err != nil {
				logging.Errorf("piplanner: suggest systems: %v", err)
			}
			for _, s := range rows {
				items = append(items, module.SuggestItem{ID: s.SystemID, Name: s.Name, Label: fmt.Sprintf("%.1f, %s", s.Security, s.RegionName), Value: strconv.FormatInt(s.SystemID, 10)})
			}
		}
		deps.WriteSuggest(w, items)
	}
}
