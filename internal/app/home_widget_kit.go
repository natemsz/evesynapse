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

// sizeList is the allowed sizes as "1x1 2x1 ...", for data-sizes.
func (d *homeWidgetDef) sizeList() string {
	parts := make([]string, len(d.sizes))
	for i, s := range d.sizes {
		parts[i] = s.String()
	}
	return strings.Join(parts, " ")
}

// bounds is the smallest and largest width and height among the sizes.
func (d *homeWidgetDef) bounds() (lo, hi wSize) {
	lo = wSize{homeGridCols, homeGridRows}
	for _, s := range d.sizes {
		lo.W, lo.H = min(lo.W, s.W), min(lo.H, s.H)
		hi.W, hi.H = max(hi.W, s.W), max(hi.H, s.H)
	}
	return lo, hi
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

// homeWidget is the view a card's shell and body templates get.
type homeWidget struct {
	ID, Key, Title, Href string
	Size                 wSize
	Form                 string
	Sizes                string // data-sizes: every allowed size
	MinW, MinH           int    // the smallest width and height among the sizes (Customize's picker)
	MaxW, MaxH           int    // the largest
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
	size := wSize{item.W, item.H}
	if size.W == 0 {
		size.W = def.Default.W
	}
	if size.H == 0 {
		size.H = def.Default.H
	}
	size = def.Snap(size)
	form := def.FormFor(size)
	lo, hi := def.bounds()
	w := homeWidget{
		ID: def.ID, Key: def.ID, Title: def.Title, Href: def.Href,
		Size: size, Form: form.Name, Sizes: def.sizeList(),
		MinW: lo.W, MinH: lo.H, MaxW: hi.W, MaxH: hi.H,
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
