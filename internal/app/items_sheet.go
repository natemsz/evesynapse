package app

import (
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"evesynapse/internal/app/module"
	"evesynapse/internal/esi"
	"evesynapse/internal/fitview"
	"evesynapse/internal/humanize"
	"evesynapse/internal/itemsheet"
	"evesynapse/internal/logging"
)

// itemSheetView is a type's stats as the item page shows them, built
// from its stored item sheet.
type itemSheetView struct {
	IsShip       bool
	Slots        []itemSlotTile
	Bonuses      []itemsheet.BonusBlock
	Resists      []itemResistRow
	Sections     []itemsheet.Section
	FitsTo       []itemFitsGroup
	Skills       []itemSkillLine
	SkillPilot   string // the signed-in pilot the marks are for; "" when none
	WorkbenchURL string
	Variations   *itemVariationsView
}

// itemSlotTile is one ship slot count on the slot strip.
type itemSlotTile struct {
	Label string
	Value string
}

// itemResistRow is one defense layer's hit points and four
// resistances, drawn with the workbench's resist cells.
type itemResistRow struct {
	Layer string
	HP    string
	Cells []itemResistCell
}

type itemResistCell struct {
	Kind string // em, th, kin, exp: the workbench's res-* classes
	Pct  float64
	Text string
}

// itemFitsGroup is one line of "Fits to": the slot it takes, the
// charges or launchers it is used with, or the hulls it is limited to.
type itemFitsGroup struct {
	Label string
	Tags  []itemFitsTag
}

type itemFitsTag struct {
	Label string
	Link  string
}

// itemSkillLine is one required skill with the signed-in pilot's
// mark: Mark is "ok" when trained to the level, "short" when not,
// "" when no pilot's skills are known.
type itemSkillLine struct {
	SkillID int64
	Name    string
	Level   int64
	Depth   int
	Mark    string
	Trained int
}

// itemVariationsView is the compare table of a module's variation
// family, with our own price on each row.
type itemVariationsView struct {
	Columns []string
	Rows    []itemVariationRow
}

type itemVariationRow struct {
	TypeID    int64
	Name      string
	Tier      string
	Values    []string
	Price     string // "" when no tier can price it
	PriceNote string // the tier's provenance and age when not a fresh hub price
	Current   bool
}

// Ship attributes the sheet view moves out of the attribute list: the
// slot strip and the resist grid show them instead.
var (
	shipSlotAttrs = []struct {
		id    int64
		label string
	}{
		{14, "High slots"}, {13, "Mid slots"}, {12, "Low slots"}, {1154, "Rig slots"},
		{102, "Turrets"}, {101, "Launchers"}, {283, "Drone bay"}, {1271, "Bandwidth"},
	}
	resistLayers = []struct {
		layer string
		hp    int64
		ids   [4]int64 // em, thermal, kinetic, explosive resonance
	}{
		{"Shield", 263, [4]int64{271, 274, 273, 272}},
		{"Armor", 265, [4]int64{267, 270, 269, 268}},
		{"Hull", 9, [4]int64{113, 110, 109, 111}},
	}
	resistKinds = [4]string{"em", "th", "kin", "exp"}
)

// Slot effects, from the dump's dgmEffects: which rack a module takes.
var slotEffects = map[int64]string{11: "Low slot", 12: "High slot", 13: "Mid slot", 2663: "Rig slot", 3772: "Subsystem slot"}

// buildItemSheetView is a type's sheet as the item page shows it: the
// static sheet, the acting pilot's skill marks, and for a module its
// variation table with prices.
func (app *Application) buildItemSheetView(ctx context.Context, typeID int64) *itemSheetView {
	_, v := app.itemSheetCore(ctx, typeID)
	if v == nil {
		return nil
	}
	app.markSheetSkills(ctx, v)
	if !v.IsShip {
		v.Variations = app.itemVariations(ctx, typeID)
	}
	return v
}

// itemSheetCore builds the part of a type's sheet that comes from the
// static data alone: the same for every reader until the next SDE
// import. Skill marks and prices are the item page's additions
// (buildItemSheetView).
func (app *Application) itemSheetCore(ctx context.Context, typeID int64) (*itemsheet.Sheet, *itemSheetView) {
	sh, err := itemsheet.Load(ctx, app.queries, typeID)
	if err != nil {
		if !errors.Is(err, itemsheet.ErrNotFound) {
			logging.Errorf("items: sheet %d: %v", typeID, err)
		}
		return nil, nil
	}
	v := &itemSheetView{IsShip: sh.CategoryID == itemsheet.CategoryShip, Bonuses: sh.Bonuses}
	moved := map[int64]bool{}

	if v.IsShip {
		v.WorkbenchURL = "/fittings/?ship=" + strconv.FormatInt(typeID, 10) + "#fit-editor"
		for _, s := range shipSlotAttrs {
			val, ok := sh.Value(s.id)
			if !ok {
				continue
			}
			moved[s.id] = true
			text := itemsheet.Number(val)
			switch s.id {
			case 283:
				text += " m3"
			case 1271:
				text += " Mbit/s"
			}
			v.Slots = append(v.Slots, itemSlotTile{Label: s.label, Value: text})
		}
		for _, l := range resistLayers {
			row := itemResistRow{Layer: l.layer}
			if hp, ok := sh.Value(l.hp); ok {
				row.HP = itemsheet.Number(hp) + " HP"
				moved[l.hp] = true
			}
			for i, id := range l.ids {
				res, ok := sh.Value(id)
				if !ok {
					res = 1 // no resistance
				}
				moved[id] = true
				pct := math.Max(0, math.Min(100, (1-res)*100))
				row.Cells = append(row.Cells, itemResistCell{Kind: resistKinds[i], Pct: pct, Text: itemsheet.Number(math.Round(pct*10)/10) + "%"})
			}
			v.Resists = append(v.Resists, row)
		}
	}

	// Fits to: the rack, then what it is used with, then the hulls
	// it is limited to.
	if eff, err := app.queries.ListSDETypeEffects(ctx, typeID); err == nil {
		for _, e := range eff {
			if slot, ok := slotEffects[e.EffectID]; ok {
				v.FitsTo = append(v.FitsTo, itemFitsGroup{Label: "Slot", Tags: []itemFitsTag{{Label: slot}}})
				break
			}
		}
	}
	var uses, hulls []itemFitsTag
	for _, sec := range sh.Sections {
		for _, r := range sec.Rows {
			switch {
			case strings.HasPrefix(r.Name, "chargeGroup"), strings.HasPrefix(r.Name, "launcherGroup"):
				uses = append(uses, itemFitsTag{Label: r.Value, Link: r.Link})
				moved[r.AttributeID] = true
			case strings.HasPrefix(r.Name, "canFitShip"):
				hulls = append(hulls, itemFitsTag{Label: r.Value, Link: r.Link})
				moved[r.AttributeID] = true
			}
		}
	}
	if len(uses) > 0 {
		v.FitsTo = append(v.FitsTo, itemFitsGroup{Label: "Used with", Tags: uses})
	}
	if len(hulls) > 0 {
		v.FitsTo = append(v.FitsTo, itemFitsGroup{Label: "Only fits on", Tags: hulls})
	}

	for _, sec := range sh.Sections {
		kept := itemsheet.Section{Title: sec.Title}
		for _, r := range sec.Rows {
			if !moved[r.AttributeID] {
				kept.Rows = append(kept.Rows, r)
			}
		}
		if len(kept.Rows) > 0 {
			v.Sections = append(v.Sections, kept)
		}
	}

	for _, l := range sh.Skills {
		v.Skills = append(v.Skills, itemSkillLine{SkillID: l.SkillID, Name: l.Name, Level: l.Level, Depth: l.Depth})
	}
	return sh, v
}

// markSheetSkills marks each required skill, for a signed-in account,
// against the acting pilot's stored skills.
func (app *Application) markSheetSkills(ctx context.Context, v *itemSheetView) {
	chars := app.sessionCharacters(ctx)
	if len(chars) == 0 {
		return
	}
	pilot := app.actingCharacterID(ctx, chars)
	var skills esi.Skills
	if pilot == 0 || !app.loadCorpSnapshot(ctx, pilot, esi.SnapSkills, &skills) {
		return
	}
	levels := fitview.ActiveSkillLevels(skills)
	for _, ch := range chars {
		if ch.CharacterID == pilot {
			v.SkillPilot = ch.Name
		}
	}
	for i := range v.Skills {
		l := &v.Skills[i]
		l.Trained = levels[l.SkillID]
		l.Mark = "short"
		if int64(l.Trained) >= l.Level {
			l.Mark = "ok"
		}
	}
}

// itemVariations reads the stored compare table of the type's
// variation family and prices each member in the house order (own
// swept orders, own estimates, the labeled outside fallback, else no
// price). Nil when the type has no family.
func (app *Application) itemVariations(ctx context.Context, typeID int64) *itemVariationsView {
	rows, err := app.queries.ListItemVariationRows(ctx, typeID)
	if err != nil {
		logging.Errorf("items: variations %d: %v", typeID, err)
		return nil
	}
	if len(rows) < 2 {
		return nil
	}
	cols, err := app.queries.ListItemVariationColumns(ctx, typeID)
	if err != nil {
		logging.Errorf("items: variation columns %d: %v", typeID, err)
		return nil
	}
	view := &itemVariationsView{}
	for _, c := range cols {
		view.Columns = append(view.Columns, c.DisplayName)
	}
	lines := make([]module.AppraisalLine, 0, len(rows))
	for _, r := range rows {
		lines = append(lines, module.AppraisalLine{TypeID: r.TypeID, Quantity: 1})
	}
	prices := map[int64]module.AppraisedLine{}
	if res, err := app.AppraiseLines(ctx, lines); err == nil {
		for _, l := range res.Lines {
			prices[l.TypeID] = l
		}
	} else {
		logging.Errorf("items: variation prices %d: %v", typeID, err)
	}
	now := time.Now()
	for _, r := range rows {
		row := itemVariationRow{TypeID: r.TypeID, Name: r.Name, Tier: r.MetaGroupName, Current: r.TypeID == typeID}
		for i, c := range cols {
			if i >= len(r.Vals) || math.IsNaN(r.Vals[i]) {
				row.Values = append(row.Values, "")
				continue
			}
			row.Values = append(row.Values, itemsheet.FormatValue(c.UnitID, c.UnitName, r.Vals[i]))
		}
		if p, ok := prices[r.TypeID]; ok && p.Priced {
			row.Price = esi.FormatISK(p.UnitPrice)
			row.PriceNote = variationPriceNote(p, now)
		}
		view.Rows = append(view.Rows, row)
	}
	return view
}

// variationPriceNote names where a price came from when it is not a
// fresh hub price, with its age.
func variationPriceNote(p module.AppraisedLine, now time.Time) string {
	age := ""
	if !p.AsOf.IsZero() {
		age = humanize.Duration(now.Sub(p.AsOf)) + " old"
	}
	switch p.Tier {
	case module.AppraisalTierTypical:
		if now.Sub(p.AsOf) >= fallbackOwnFresh {
			return "Forge price, " + age
		}
		return ""
	case module.AppraisalTierEstimate:
		return "Estimate"
	case module.AppraisalTierFallback:
		return "Fuzzwork, " + age
	}
	return ""
}

// itemSheetTopRows caps the sheet rows a tooltip shows.
const itemSheetTopRows = 6

// itemSheetJSON is a type's static sheet for the fitting workbench:
// the rows a tooltip shows and the full sheet the info drawer shows,
// rendered by the item page's own partial.
type itemSheetJSON struct {
	ID    int64             `json:"id"`
	Name  string            `json:"name"`
	Group string            `json:"group,omitempty"`
	URL   string            `json:"url"`
	Top   []itemSheetTopRow `json:"top"`
	HTML  template.HTML     `json:"html"`
}

type itemSheetTopRow struct {
	Label   string   `json:"k"`
	Value   string   `json:"v"`
	Section string   `json:"s,omitempty"` // the sheet section, for the row's glyph and colour
	Layer   string   `json:"l,omitempty"` // a folded resist line: structure, armor, shield or bonus
	Res     []string `json:"r,omitempty"` // its EM, thermal, kinetic, explosive figures
}

// handleItemSheetJSON serves GET /items/sheet/{typeID}.json. The
// sheet comes from the static data only, so it is the same for every
// reader until the next SDE import or release: the ETag names both,
// and browsers keep it a day.
func (app *Application) handleItemSheetJSON(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	typeID, err := strconv.ParseInt(strings.TrimSuffix(chi.URLParam(r, "file"), ".json"), 10, 64)
	if err != nil || typeID <= 0 || !strings.HasSuffix(chi.URLParam(r, "file"), ".json") {
		http.NotFound(w, r)
		return
	}
	stamp, _ := app.sdeMeta(ctx, "imported_at")
	etag := `"` + strings.NewReplacer(":", "", "-", "").Replace(stamp) + "-" + sdeImportVersion + "-" + appVersion + `"`
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	sh, v := app.itemSheetCore(ctx, typeID)
	if v == nil {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Del("ETag")
		http.NotFound(w, r)
		return
	}
	out := itemSheetJSON{ID: sh.TypeID, Name: sh.Name, Group: sh.GroupName, URL: "/items/type/" + strconv.FormatInt(sh.TypeID, 10) + "/", Top: sheetTopRows(v)}
	html, err := app.Fragment("itemsheet.html", "item-sheet", v)
	if err != nil {
		logging.Errorf("items: sheet json %d: %v", typeID, err)
		http.Error(w, "That item's details couldn't be read.", http.StatusInternalServerError)
		return
	}
	out.HTML = html
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(out)
}

// sheetFigureKinds gives the figures with a glyph of their own
// (fitview.FitGlyphKind's optimal, falloff, tracking) that kind
// instead of their section's.
var sheetFigureKinds = map[string]string{
	"maxRange": "optimal", "falloff": "falloff", "trackingSpeed": "tracking",
}

// sheetTopRows is the start of a type's sheet as a tooltip shows it:
// the first rows after the Fitting section (the tooltip states CPU
// and powergrid already), without a module's own hitpoints, its tech
// and meta level (the tier says those) or its skill rows (the drawer
// lists them), and with a layer's four
// "<Layer> <Kind> Damage Resistance" rows folded into one line in EM,
// thermal, kinetic, explosive order. Zero figures are left out.
func sheetTopRows(v *itemSheetView) []itemSheetTopRow {
	top := []itemSheetTopRow{}
	folded := map[string]*[4]string{}
	at := map[string]int{}
	for _, sec := range v.Sections {
		if sec.Title == "Fitting" {
			continue
		}
		for _, row := range sec.Rows {
			if (row.Name == "hp" && !v.IsShip) || row.Name == "techLevel" || row.Name == "metaLevelOld" || strings.HasPrefix(row.Name, "requiredSkill") {
				continue
			}
			if zeroFigure(row.Value) {
				continue
			}
			if m := resistLabel.FindStringSubmatch(row.Label); m != nil {
				layer, key := m[1]+" resists", strings.ToLower(m[1])
				if m[1] == "" {
					layer, key = "Resist bonus", "bonus"
				}
				if folded[layer] == nil {
					if len(top) >= itemSheetTopRows {
						continue
					}
					folded[layer] = &[4]string{"0", "0", "0", "0"}
					at[layer] = len(top)
					top = append(top, itemSheetTopRow{Label: layer, Layer: key})
				}
				fig := strings.TrimSuffix(row.Value, "%")
				if m[1] == "" {
					// A lower damage multiplier is a resistance gain.
					fig = "+" + strings.TrimPrefix(fig, "-")
				}
				folded[layer][resistKindOrder[m[2]]] = fig
				continue
			}
			if len(top) < itemSheetTopRows {
				kind := sheetFigureKinds[row.Name]
				if kind == "" {
					kind = strings.ToLower(sec.Title)
				}
				top = append(top, itemSheetTopRow{Label: row.Label, Value: row.Value, Section: kind})
			}
		}
	}
	for layer, vals := range folded {
		top[at[layer]].Value = strings.Join(vals[:], " / ") + "%"
		top[at[layer]].Res = vals[:]
	}
	return top
}

var (
	resistLabel     = regexp.MustCompile(`^(?:(Structure|Armor|Shield) )?(EM|Thermal|Kinetic|Explosive) Damage Resistance(?: Bonus)?$`)
	resistKindOrder = map[string]int{"EM": 0, "Thermal": 1, "Kinetic": 2, "Explosive": 3}
)

// zeroFigure reports a formatted sheet value that says nothing: "0",
// "0%", "0 HP" and the like.
func zeroFigure(v string) bool {
	num, _, _ := strings.Cut(strings.TrimSuffix(v, "%"), " ")
	f, err := strconv.ParseFloat(num, 64)
	return err == nil && f == 0
}
