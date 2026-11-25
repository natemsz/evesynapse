package app

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"sort"
	"strconv"
	"strings"
	"time"

	json "github.com/goccy/go-json"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Home widget kit: the one declaration every home widget makes, the
// registry that holds them, and the size rules every widget shares.
//
// A widget is a declaration (homew_<id>.go, registered from init) and
// a template file (templates/widgets/<id>.html) defining one body per
// form, "w-<id>-<form>". The declaration lists the widget's forms and
// the sizes that draw each one; the sizes the forms list are exactly
// the sizes the widget may take. Nothing else in the home code names a
// widget: the catalog, the size bounds, the snapshot kinds to load and
// the card shell all come from here.
// ---------------------------------------------------------------------------

// The home grid: W is 1 to homeGridCols columns, H is 1 to
// homeGridRows rows.
const (
	homeGridCols = 3
	homeGridRows = 4
)

// wSize is a widget size in grid cells: W columns (1-3) by H rows (1-4).
type wSize struct{ W, H int }

func (s wSize) String() string { return strconv.Itoa(s.W) + "x" + strconv.Itoa(s.H) }

// parseWSize reads "2x1" into a size inside the grid.
func parseWSize(s string) (wSize, bool) {
	ws, hs, ok := strings.Cut(strings.TrimSpace(s), "x")
	if !ok {
		return wSize{}, false
	}
	w, werr := strconv.Atoi(ws)
	h, herr := strconv.Atoi(hs)
	if werr != nil || herr != nil || w < 1 || w > homeGridCols || h < 1 || h > homeGridRows {
		return wSize{}, false
	}
	return wSize{w, h}, true
}

// Widget categories, in the order the catalog lists them.
const (
	wcatOverview = "overview"
	wcatPilots   = "pilots"
	wcatMoney    = "money"
	wcatIndustry = "industry"
	wcatMarket   = "market"
	wcatCorp     = "corporation"
	wcatFleets   = "fleets"
	wcatIntel    = "intel"
)

var widgetCategoryOrder = []string{wcatOverview, wcatPilots, wcatMoney, wcatIndustry, wcatMarket, wcatCorp, wcatFleets, wcatIntel}

// needPlanetLayouts in a widget's Needs loads the per-planet colony
// layouts, which ride suffix-keyed snapshots outside the batched kinds.
const needPlanetLayouts = "planet_layouts"

// homeWidgetForm is one look of a widget and the sizes that draw it.
type homeWidgetForm struct {
	Name  string // template suffix and the size picker's name for it
	Sizes string // "2x1 3x1": every size listed here draws this form
	Says  string // one plain sentence for the size picker: what this form shows
}

// homeWidgetDef is a widget's whole declaration.
type homeWidgetDef struct {
	ID          string // persisted in users.home_layout; never rename one
	Title       string
	Description string // the Add a widget line
	Category    string // one of the wcat* constants
	Feature     string // the manifest feature whose scopes power it (module_manifest.go)
	Requires    string // a module id that must be built in; "" for core widgets
	Href        string // the page with all of it; "" when there is none
	Default     wSize  // must be one of the forms' sizes
	Forms       []homeWidgetForm
	Needs       []string // snapshot kinds the builder reads (esi.Snap*), needPlanetLayouts for colony layouts
	Build       func(ctx context.Context, app *Application, in *widgetInput) any

	sizes  []wSize       // every allowed size, narrowest then shortest first
	formOf map[wSize]int // size -> index into Forms

	sizesText     string // sizeList, built once at registration
	sizeNamesText string // sizeNames, built once at registration
}

// widgetInput is what a builder is handed for one card.
type widgetInput struct {
	UserID  int64
	Key     string   // layout key: the widget id
	Size    wSize    // the size the card draws at
	Form    string   // the form drawn at Size
	Forms   []string // every form this render draws
	Chars   []db.Character
	Bundles []*charSnaps
	Shared  *homeShared // per-render cache
	Now     time.Time
}

// Draws reports whether this render draws the named form, so a builder
// can skip data no drawn form shows.
func (in *widgetInput) Draws(form string) bool {
	for _, f := range in.Forms {
		if f == form {
			return true
		}
	}
	return false
}

// widgetQuiet is implemented by widget data that can have nothing to
// say: a quiet widget's card is left off the home outside Customize.
type widgetQuiet interface{ Quiet() bool }

// homeWidgetRegistry is written only from init functions, then read.
var homeWidgetRegistry = map[string]*homeWidgetDef{}

// registerHomeWidget adds a declaration; each homew_*.go calls it from
// init(). It panics on a duplicate id, a malformed size, a size in two
// forms, or a default outside the forms: declarations are code, and
// the convention test runs them.
func registerHomeWidget(def homeWidgetDef) {
	if def.ID == "" {
		panic("home widget: empty id")
	}
	if _, dup := homeWidgetRegistry[def.ID]; dup {
		panic("home widget: duplicate id " + def.ID)
	}
	if def.Build == nil {
		panic("home widget " + def.ID + ": no builder")
	}
	if len(def.Forms) == 0 {
		panic("home widget " + def.ID + ": no forms")
	}
	def.formOf = map[wSize]int{}
	for i, f := range def.Forms {
		if f.Name == "" {
			panic("home widget " + def.ID + ": a form has no name")
		}
		for _, raw := range strings.Fields(f.Sizes) {
			s, ok := parseWSize(raw)
			if !ok {
				panic(fmt.Sprintf("home widget %s: form %s lists malformed size %q", def.ID, f.Name, raw))
			}
			if _, dup := def.formOf[s]; dup {
				panic(fmt.Sprintf("home widget %s: size %s is in two forms", def.ID, s))
			}
			def.formOf[s] = i
			def.sizes = append(def.sizes, s)
		}
	}
	if _, ok := def.formOf[def.Default]; !ok {
		panic(fmt.Sprintf("home widget %s: default %s is not one of its sizes", def.ID, def.Default))
	}
	sort.Slice(def.sizes, func(i, j int) bool {
		if def.sizes[i].W != def.sizes[j].W {
			return def.sizes[i].W < def.sizes[j].W
		}
		return def.sizes[i].H < def.sizes[j].H
	})
	d := def
	homeWidgetRegistry[def.ID] = &d
	// The size picker's data depends only on the declaration: built
	// here once, not on every card render.
	d.sizesText, d.sizeNamesText = d.sizeList(), d.sizeNames()
}

// builtIn reports whether the widget's module is compiled in.
func (d *homeWidgetDef) builtIn() bool {
	return d.Requires == "" || moduleCompiledIn(d.Requires)
}

// homeWidgetCatalog is every registered widget whose module is built
// in, by category, then title.
func homeWidgetCatalog() []*homeWidgetDef {
	rank := make(map[string]int, len(widgetCategoryOrder))
	for i, c := range widgetCategoryOrder {
		rank[c] = i
	}
	out := make([]*homeWidgetDef, 0, len(homeWidgetRegistry))
	for _, d := range homeWidgetRegistry {
		if d.builtIn() {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if rank[out[i].Category] != rank[out[j].Category] {
			return rank[out[i].Category] < rank[out[j].Category]
		}
		return out[i].Title < out[j].Title
	})
	return out
}

// widgetDefFor is the declaration for id, when it is registered and its
// module is built in.
func widgetDefFor(id string) (*homeWidgetDef, bool) {
	d, ok := homeWidgetRegistry[id]
	if !ok || !d.builtIn() {
		return nil, false
	}
	return d, true
}

// widgetNeeds is the snapshot needs a widget declares, whether or not
// its module is built in: other surfaces (notifications, the Wallet
// page) read the same bundles a widget does.
func widgetNeeds(id string) []string {
	if d, ok := homeWidgetRegistry[id]; ok {
		return d.Needs
	}
	return nil
}

// Sizes is every size the widget may take.
func (d *homeWidgetDef) Sizes() []wSize { return d.sizes }

// Allows reports whether s is one of the widget's sizes.
func (d *homeWidgetDef) Allows(s wSize) bool {
	_, ok := d.formOf[s]
	return ok
}

// FormFor is the form that draws size s (which must be allowed).
func (d *homeWidgetDef) FormFor(s wSize) homeWidgetForm {
	if i, ok := d.formOf[s]; ok {
		return d.Forms[i]
	}
	return d.Forms[d.formOf[d.Default]]
}

// sizeOf is the size a layout entry draws at: its stored size, the
// default on each axis it leaves unset, snapped to an allowed size.
// Customize, the presets and the card renderer all size by it.
func (d *homeWidgetDef) sizeOf(item homeLayoutItem) wSize {
	s := d.Default
	if item.W > 0 {
		s.W = item.W
	}
	if item.H > 0 {
		s.H = item.H
	}
	return d.Snap(s)
}

// sizeList is the allowed sizes as "1x1 2x1 ...", for data-sizes.
func (d *homeWidgetDef) sizeList() string {
	parts := make([]string, len(d.sizes))
	for i, s := range d.sizes {
		parts[i] = s.String()
	}
	return strings.Join(parts, " ")
}

// sizeNames is, for each allowed size, the form it draws and the
// size picker's line for it: "Medium: the list and a chart." A widget
// with one form has no form name to show, only its sentence.
func (d *homeWidgetDef) sizeNames() string {
	type named struct {
		Form string `json:"form"`
		Says string `json:"says"`
	}
	out := make(map[string]named, len(d.sizes))
	for _, sz := range d.sizes {
		f := d.FormFor(sz)
		n := named{Says: f.Says}
		if len(d.Forms) > 1 {
			n.Form = f.Name
			n.Says = strings.ToUpper(f.Name[:1]) + f.Name[1:] + ": " + lowerFirst(f.Says)
		}
		out[sz.String()] = n
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

// lowerFirst lowers a sentence's first letter, for a line that follows
// a form name; a leading acronym ("ISK flow") keeps its case.
func lowerFirst(s string) string {
	if len(s) < 2 || strings.ToUpper(s[1:2]) == s[1:2] {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// largest is the widget's biggest size: the most cells, the wider on
// a tie. Expand shows the form that draws it.
func (d *homeWidgetDef) largest() wSize {
	best := d.sizes[0]
	for _, s := range d.sizes[1:] {
		if s.W*s.H > best.W*best.H || (s.W*s.H == best.W*best.H && s.W > best.W) {
			best = s
		}
	}
	return best
}

// widthsAt is the allowed widths at height h, narrowest first.
func (d *homeWidgetDef) widthsAt(h int) []int {
	var out []int
	for _, s := range d.sizes {
		if s.H == h {
			out = append(out, s.W)
		}
	}
	return out
}

// Snap turns a stored size into an allowed one: the size itself when
// allowed; else the same width at the nearest height; else the
// smallest allowed size that covers it; else the nearest allowed size.
// Ties go to the smaller size.
func (d *homeWidgetDef) Snap(s wSize) wSize {
	if d.Allows(s) {
		return s
	}
	var best wSize
	found := false
	for _, c := range d.sizes {
		if c.W != s.W {
			continue
		}
		if !found || absInt(c.H-s.H) < absInt(best.H-s.H) {
			best, found = c, true
		}
	}
	if found {
		return best
	}
	for _, c := range d.sizes {
		if c.W < s.W || c.H < s.H {
			continue
		}
		if !found || c.W*c.H < best.W*best.H || (c.W*c.H == best.W*best.H && c.W < best.W) {
			best, found = c, true
		}
	}
	if found {
		return best
	}
	dist := func(c wSize) int { return absInt(c.W-s.W) + absInt(c.H-s.H) }
	best = d.sizes[0]
	for _, c := range d.sizes[1:] {
		if dist(c) < dist(best) || (dist(c) == dist(best) && c.W*c.H < best.W*best.H) {
			best = c
		}
	}
	return best
}

func absInt(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// homeShared is the per-render cache: each fact a widget reads is
// computed once and reused by every widget that shows it.
type homeShared struct {
	app     *Application
	userID  int64
	chars   []db.Character
	bundles []*charSnaps
	now     time.Time
	market  *homeMarketShared

	netWorth      *netWorthWidget
	netWorthBuilt bool

	briefingSince    time.Time
	briefingSinceSet bool
}

// NetWorth is the account's net worth, built at most once per render:
// At a Glance carries the same figures, and the default layout holds both.
func (s *homeShared) NetWorth(ctx context.Context) *netWorthWidget {
	if !s.netWorthBuilt {
		s.netWorth, s.netWorthBuilt = s.app.buildNetWorth(ctx, s.userID, s.bundles), true
	}
	return s.netWorth
}

// BriefingSince is the start of the Briefing's "since you last looked"
// window. Reading it never moves the anchor: only a full home render does.
func (s *homeShared) BriefingSince(ctx context.Context) time.Time {
	if !s.briefingSinceSet {
		s.briefingSince, s.briefingSinceSet = s.app.briefingWindowStart(ctx, s.userID, s.now), true
	}
	return s.briefingSince
}

// viewerChars is the account's characters as the set charLink reads.
func viewerChars(chars []db.Character) map[int64]bool {
	out := make(map[int64]bool, len(chars))
	for _, c := range chars {
		out[c.CharacterID] = true
	}
	return out
}

// homeWidget is the view a card's shell and body templates get.
type homeWidget struct {
	ID, Key, Title, Href string
	Size                 wSize
	Form                 string
	Sizes                string // data-sizes: every allowed size
	SizeNames            string // data-size-names: JSON, size -> {"form", "says"} for the size picker
	SizeClass            string // gw1-3 gh1-4: the grid classes the CSS applies
	Cap                  int    // rows or items shown before More, by height
	Customize            bool   // rendering under /?customize=1 (form return targets)
	Data                 any    // the builder's value
	Body                 template.HTML
	ViewerChars          map[int64]bool // the account's characters, for charLink
}

// Uid is an element id unique to this card: widget templates use it
// instead of fixed ids, since one page may hold more than one body.
func (w homeWidget) Uid(name string) string {
	return "w-" + strings.ReplaceAll(w.Key, "#", "-") + "-" + name
}

// buildHomeWidget builds one card for item at its snapped size and
// renders its body. It returns false when the widget is quiet and the
// home is not being customized.
func (app *Application) buildHomeWidget(ctx context.Context, ts *template.Template, def *homeWidgetDef, item homeLayoutItem, shared *homeShared, customize bool, viewer map[int64]bool) (homeWidget, bool) {
	size := def.sizeOf(item)
	form := def.FormFor(size)
	w := homeWidget{
		ID: def.ID, Key: def.ID, Title: def.Title, Href: def.Href,
		Size: size, Form: form.Name, Sizes: def.sizesText, SizeNames: def.sizeNamesText,
		SizeClass:   fmt.Sprintf("gw%d gh%d", size.W, size.H),
		Cap:         homeCap(size.H),
		Customize:   customize,
		ViewerChars: viewer,
	}
	in := &widgetInput{
		UserID: shared.userID, Key: w.Key, Size: size, Form: form.Name, Forms: []string{form.Name},
		Chars: shared.chars, Bundles: shared.bundles, Shared: shared, Now: shared.now,
	}
	w.Data = def.Build(ctx, app, in)
	if q, ok := w.Data.(widgetQuiet); ok && q.Quiet() && !customize {
		return w, false
	}
	if ts != nil {
		if err := renderWidgetBodies(ts, &w); err != nil {
			logging.Errorf("home: render widget %s: %v", w.ID, err)
		}
	}
	return w, true
}

// renderWidgetBodies executes the card's form template into Body.
func renderWidgetBodies(ts *template.Template, w *homeWidget) error {
	var buf bytes.Buffer
	if err := ts.ExecuteTemplate(&buf, "w-"+w.ID+"-"+w.Form, w); err != nil {
		return err
	}
	w.Body = template.HTML(buf.String())
	return nil
}

// pageExtraTemplates are files a page is parsed with on top of the
// shared ones: the home page carries every widget template.
var pageExtraTemplates = map[string][]string{
	"home.html": {"templates/widgets/*.html"},
}

// pageTemplateFiles is the shared file list a page is parsed with.
func pageTemplateFiles(page string) []string {
	extra := pageExtraTemplates[page]
	if len(extra) == 0 {
		return pageSharedTemplates
	}
	return append(append([]string{}, pageSharedTemplates...), extra...)
}

// homeTemplateSet is the parsed home page set, the one render uses.
func homeTemplateSet() (*template.Template, error) {
	return parsedTemplate(&pageTemplates, "base", "home.html", pageTemplateFiles("home.html")...)
}
