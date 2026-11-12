package piplanner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
	"evesynapse/internal/piplan"
)

// goalView is the goal form as the page shows it.
type goalView struct {
	Form        planForm
	PerDayStr   string
	Pilots      []pilotOption
	SignedIn    bool
	ResetChoice []choice
	HaulChoice  []choice
	CCChoice    []choice
	JumpChoice  []choice
}

type pilotOption struct {
	ID      int64
	Name    string
	Checked bool
	Detail  string
}

type choice struct {
	Value    string
	Label    string
	Selected bool
}

// planView is a stored plan, ready to render.
type planView struct {
	Token      string
	Target     string
	TypeID     int64
	MadeText   string
	PerDay     string
	Profit     string
	ProfitCls  string
	Revenue    string
	Taxes      string
	Buys       string
	Slots      string
	Haul       string
	Short      string
	Notes      []string
	Groups     []pilotGroup
	Flows      []flowRow
	Prices     string
	Exact      bool
	ResetText  string
	AnyAssumed bool
}

type pilotGroup struct {
	Name     string
	Colonies []colonyCard
}

type colonyCard struct {
	Index    int
	Planet   string
	Type     string
	TypeKey  string
	System   string
	Role     string
	Summary  string
	CPUPct   int
	PGPct    int
	CPUText  string
	PGText   string
	Fits     bool
	Out      []rateChip
	In       []rateChip
	Existing bool
	Changes  string
	Assumed  bool
	Template string
	Named    bool // a real planet, not one still to pick
}

type rateChip struct {
	Name string
	Rate string
}

type flowRow struct {
	From, To string
	Item     string
	PerDay   string
	M3       string
}

func goalChoices(f planForm) (reset, haul, cc, jumps []choice) {
	for _, h := range piplan.ProgramLengths {
		label := fmt.Sprintf("Every %d days", int(h/24))
		if h == 24 {
			label = "Every day"
		}
		reset = append(reset, choice{fmt.Sprint(h), label, h == f.ResetHours})
	}
	for _, h := range []float64{12, 24, 48, 72, 168} {
		label := fmt.Sprintf("Every %d days", int(h/24))
		switch h {
		case 12:
			label = "Twice a day"
		case 24:
			label = "Every day"
		case 168:
			label = "Once a week"
		}
		haul = append(haul, choice{fmt.Sprint(h), label, h == f.HaulHours})
	}
	for l := 0; l <= 5; l++ {
		cc = append(cc, choice{fmt.Sprint(l), fmt.Sprintf("Level %d", l), l == f.CCLevel})
	}
	for j := 0; j <= 5; j++ {
		label := fmt.Sprintf("Within %d jumps", j)
		if j == 0 {
			label = "That system only"
		} else if j == 1 {
			label = "Within 1 jump"
		}
		jumps = append(jumps, choice{fmt.Sprint(j), label, j == f.Jumps})
	}
	return
}

// buildGoal fills the goal form: the stored plan's settings when one
// is open, else the defaults with the product from the address.
func buildGoal(ctx context.Context, deps module.Deps, chars []db.Character, f planForm) goalView {
	g := goalView{Form: f, SignedIn: deps.UserID(ctx) != 0 && len(chars) > 0}
	if f.PerDay > 0 {
		g.PerDayStr = formatQty(f.PerDay)
	}
	want := map[int64]bool{}
	for _, id := range f.CharIDs {
		want[id] = true
	}
	for _, ch := range chars {
		slots := characterSlots(ctx, deps, ch, f)
		g.Pilots = append(g.Pilots, pilotOption{
			ID: ch.CharacterID, Name: ch.Name, Checked: len(want) == 0 || want[ch.CharacterID],
			Detail: fmt.Sprintf("%d planet%s, command center level %d", slots.MaxPlanets, map[bool]string{true: "", false: "s"}[slots.MaxPlanets == 1], slots.CCLevel),
		})
	}
	g.ResetChoice, g.HaulChoice, g.CCChoice, g.JumpChoice = goalChoices(f)
	return g
}

// loadPlan reads a stored plan and shapes it for the page.
func loadPlan(ctx context.Context, deps module.Deps, token string) (*planView, planForm, bool) {
	var f planForm
	row, err := deps.DB().GetPIPlan(ctx, token)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			logging.Errorf("piplanner: read plan %q: %v", token, err)
		}
		return nil, f, false
	}
	var sp storedPlan
	if err := json.Unmarshal(row.Solution, &sp); err != nil || sp.Solution == nil {
		logging.Errorf("piplanner: decode plan %q: %v", token, err)
		return nil, f, false
	}
	_ = json.Unmarshal(row.Request, &f)
	sol := sp.Solution
	v := &planView{
		Token: token, Target: sol.Target, Short: sol.Short, Notes: sol.Notes, Prices: sp.Prices, Exact: sol.Exact,
		PerDay:   formatQty(math.Round(sol.PerDay*10) / 10),
		Slots:    fmt.Sprintf("%d of %d", sol.SlotsUsed, sol.SlotsTotal),
		Haul:     esi.FormatInt(int64(math.Round(sol.HaulM3Day))) + " m³",
		Taxes:    esi.FormatISK(sol.TaxDay),
		Buys:     esi.FormatISK(sol.BuyDay),
		Revenue:  esi.FormatISK(sol.RevenueDay),
		MadeText: "Planned " + deps.AgeText(row.CreatedAt),
	}
	v.TypeID = piplan.CommodityTypeIDs[sol.Target]
	if sol.PricesKnown {
		v.Profit = esi.FormatISK(sol.ProfitDay)
		if sol.ProfitDay >= 0 {
			v.ProfitCls = "ok"
		} else {
			v.ProfitCls = "bad"
		}
	} else {
		v.Profit = "-"
	}
	v.ResetText = "Extractors reset every day"
	if sol.ProgramHours > 24 {
		v.ResetText = fmt.Sprintf("Extractors reset every %d days", int(sol.ProgramHours/24))
	}

	names := map[int]string{}
	groups := map[string]*pilotGroup{}
	var order []string
	// Planets not yet named ("Any Storm planet") are numbered so the
	// cards and the moves can tell them apart.
	generic := map[string]int{}
	for _, c := range sol.Colonies {
		if c.PlanetID == 0 {
			generic[c.PlanetType]++
		}
	}
	seen := map[string]int{}
	for i, c := range sol.Colonies {
		if c.PlanetID == 0 {
			seen[c.PlanetType]++
			c.PlanetName = c.PlanetType + " planet"
			if generic[c.PlanetType] > 1 {
				c.PlanetName += fmt.Sprintf(" %d", seen[c.PlanetType])
			}
			sol.Colonies[i].PlanetName = c.PlanetName
		}
		names[c.Index] = c.PlanetName
		card := colonyCard{
			Index: c.Index, Planet: c.PlanetName, Type: c.PlanetType, TypeKey: strings.ToLower(c.PlanetType),
			System: c.System, Role: c.Role, Summary: c.Summary, Fits: c.Fit.Fits(), Existing: c.Existing,
			Assumed: c.Assumed, Named: c.PlanetID != 0,
		}
		if c.Assumed {
			v.AnyAssumed = true
		}
		if c.Fit.CPUCap > 0 {
			card.CPUPct = int(math.Min(100, float64(c.Fit.CPUUsed)*100/float64(c.Fit.CPUCap)))
			card.PGPct = int(math.Min(100, float64(c.Fit.PGUsed)*100/float64(c.Fit.PGCap)))
		}
		card.CPUText = fmt.Sprintf("%s / %s tf", esi.FormatInt(c.Fit.CPUUsed), esi.FormatInt(c.Fit.CPUCap))
		card.PGText = fmt.Sprintf("%s / %s MW", esi.FormatInt(c.Fit.PGUsed), esi.FormatInt(c.Fit.PGCap))
		card.Out = rateChips(c.Out)
		card.In = rateChips(c.In)
		if c.Existing {
			var parts []string
			if c.Added > 0 {
				parts = append(parts, fmt.Sprintf("build %d", c.Added))
			}
			if c.Retooled > 0 {
				parts = append(parts, fmt.Sprintf("switch %d", c.Retooled))
			}
			if c.Removed > 0 {
				parts = append(parts, fmt.Sprintf("remove %d", c.Removed))
			}
			if len(parts) == 0 {
				card.Changes = "No changes"
			} else {
				card.Changes = strings.Join(parts, ", ")
			}
		}
		if i < len(sp.Templates) {
			card.Template = sp.Templates[i]
		}
		pilot := c.CharName
		if pilot == "" {
			pilot = "Unassigned"
		}
		g := groups[pilot]
		if g == nil {
			g = &pilotGroup{Name: pilot}
			groups[pilot] = g
			order = append(order, pilot)
		}
		g.Colonies = append(g.Colonies, card)
	}
	for _, name := range order {
		v.Groups = append(v.Groups, *groups[name])
	}
	where := func(i int) string {
		if i < 0 {
			return "Market"
		}
		return names[i]
	}
	for _, fl := range sol.Flows {
		v.Flows = append(v.Flows, flowRow{
			From: where(fl.From), To: where(fl.To), Item: fl.Item,
			PerDay: esi.FormatInt(int64(math.Round(fl.PerDay))),
			M3:     esi.FormatInt(int64(math.Round(fl.M3Day))),
		})
	}
	return v, f, true
}

func rateChips(m map[string]float64) []rateChip {
	var out []rateChip
	for _, k := range sortedNames(m) {
		out = append(out, rateChip{Name: k, Rate: esi.FormatInt(int64(math.Round(m[k]*24))) + "/day"})
	}
	return out
}

func sortedNames(m map[string]float64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		ti, tj := piplan.TierRank(piplan.TierOf(out[i])), piplan.TierRank(piplan.TierOf(out[j]))
		if ti != tj {
			return ti > tj
		}
		return out[i] < out[j]
	})
	return out
}

// planFromRequest reads ?plan= for the page.
func planFromRequest(r *http.Request) string {
	t := strings.TrimSpace(r.URL.Query().Get("plan"))
	if len(t) > 40 {
		return ""
	}
	return t
}
