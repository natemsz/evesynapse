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
	Options     []widgetOption
	// Multi lets the widget stand on the home more than once, each copy
	// with its own settings. Every widget with a scope option may.
	Multi bool
	Build func(ctx context.Context, app *Application, in *widgetInput) any

	sizes  []wSize       // every allowed size, narrowest then shortest first
	formOf map[wSize]int // size -> index into Forms

	sizesText     string // sizeList, built once at registration
	sizeNamesText string // sizeNames, built once at registration
}

// widgetInput is what a builder is handed for one card.
type widgetInput struct {
	UserID  int64
	Size    wSize         // the size the card draws at
	Forms   []string      // every form this render draws: the one at Size, and the phone body's
	Opts    widgetOptions // the account's settings, each checked against the account
	Scope   widgetScope   // the scope option's value: Chars and Bundles are already narrowed to it
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

// widgetOption is one setting a widget declares. The account's value
// comes from its settings sheet (home_widget_config.go); a widget never
// stored or holding a value no longer valid draws with the default.
type widgetOption struct {
	Key     string
	Label   string
	Kind    string // optKindScope, optKindChoice, optKindToggle or optKindMulti
	Choices []widgetChoice
	Default string
	Hint    string // one plain sentence under the field, when the label needs it
	Max     int    // multi: the most values that may be picked; 0 for any number
}

// widgetChoice is one value a choice option can take and its label.
// Requires names the module the choice needs; the settings sheet leaves
// it out when that module is not built in.
type widgetChoice struct{ Value, Label, Requires string }

// choiceRequires marks the choice v as needing a module.
func choiceRequires(o widgetOption, v, module string) widgetOption {
	for i := range o.Choices {
		if o.Choices[i].Value == v {
			o.Choices[i].Requires = module
		}
	}
	return o
}

// builtIn reports whether the choice's module is compiled in.
func (c widgetChoice) builtIn() bool { return c.Requires == "" || moduleCompiledIn(c.Requires) }

const (
	optKindScope  = "scope"  // all characters, a tag, or one character: all, tag:<name>, char:<id>
	optKindChoice = "choice" // one of the declared choices
	optKindToggle = "toggle" // on or off: yes, no
	optKindMulti  = "multi"  // some of the declared choices, comma-joined in declared order
)

// optScope is the characters a widget reads: all of them by default.
func optScope() widgetOption {
	return widgetOption{Key: "scope", Label: "Characters", Kind: optKindScope, Default: "all"}
}

// optChoice declares a choice option; each choice is "value:Label".
func optChoice(key, label, def string, choices ...string) widgetOption {
	o := widgetOption{Key: key, Label: label, Kind: optKindChoice, Default: def}
	for _, c := range choices {
		v, l, _ := strings.Cut(c, ":")
		o.Choices = append(o.Choices, widgetChoice{Value: v, Label: l})
	}
	return o
}

// optToggle declares an on or off option.
func optToggle(key, label string, on bool) widgetOption {
	o := widgetOption{Key: key, Label: label, Kind: optKindToggle, Default: toggleOff}
	if on {
		o.Default = toggleOn
	}
	return o
}

// optMulti declares an option that picks some of its choices (at most
// max, 0 for any number); def is the default picks, comma-joined, and
// each choice is "value:Label".
func optMulti(key, label, def string, max int, choices ...string) widgetOption {
	o := optChoice(key, label, def, choices...)
	o.Kind, o.Max = optKindMulti, max
	return o
}

// withHint gives an option the sentence its sheet shows under it.
func withHint(o widgetOption, hint string) widgetOption {
	o.Hint = hint
	return o
}

// allows reports whether v is a value the option can take. A scope
// value is only checked for its shape here; whether it names the
// account's characters is valid's job.
func (o widgetOption) allows(v string) bool {
	switch o.Kind {
	case optKindScope:
		return v == scopeAll ||
			(strings.HasPrefix(v, scopeCharPfx) && len(v) > len(scopeCharPfx)) ||
			(strings.HasPrefix(v, scopeTagPfx) && len(v) > len(scopeTagPfx))
	case optKindToggle:
		return v == toggleOn || v == toggleOff
	case optKindCorp:
		return v == scopeAll || corpOptionID(v) > 0
	case optKindMulti:
		if v == "" {
			return false
		}
		picks := strings.Split(v, ",")
		if o.Max > 0 && len(picks) > o.Max {
			return false
		}
		seen := map[string]bool{}
		for _, p := range picks {
			if seen[p] || !o.hasChoice(p) {
				return false
			}
			seen[p] = true
		}
		return true
	}
	return o.hasChoice(v)
}

// hasChoice reports whether v is one of the declared choices.
func (o widgetOption) hasChoice(v string) bool {
	for _, c := range o.Choices {
		if c.Value == v {
			return true
		}
	}
	return false
}

// widgetOptions is a widget's option values by key.
type widgetOptions map[string]string

// Get is the value of the option key ("" when the widget has none).
func (o widgetOptions) Get(key string) string { return o[key] }

// defaultOptions is every declared option at its default.
func (d *homeWidgetDef) defaultOptions() widgetOptions {
	out := make(widgetOptions, len(d.Options))
	for _, o := range d.Options {
		out[o.Key] = o.Default
	}
	return out
}

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
	keys := map[string]bool{}
	for _, o := range def.Options {
		if o.Key == "" || keys[o.Key] || o.Key == "key" || o.Key == "next" {
			panic(fmt.Sprintf("home widget %s: option key %q is empty, repeated or taken by the settings form", def.ID, o.Key))
		}
		if (o.Kind == optKindChoice || o.Kind == optKindMulti) == (len(o.Choices) == 0) {
			panic(fmt.Sprintf("home widget %s: option %s has choices only when it is a choice or a multi", def.ID, o.Key))
		}
		keys[o.Key] = true
		if !o.allows(o.Default) {
			panic(fmt.Sprintf("home widget %s: option %s defaults to %q, not one of its choices", def.ID, o.Key, o.Default))
		}
	}
	sort.Slice(def.sizes, func(i, j int) bool {
		if def.sizes[i].W != def.sizes[j].W {
			return def.sizes[i].W < def.sizes[j].W
		}
		return def.sizes[i].H < def.sizes[j].H
	})
	if _, ok := def.scopeOption(); ok {
		def.Multi = true
	}
	d := def
	homeWidgetRegistry[def.ID] = &d
	// The size picker's data depends only on the declaration: built
	// here once, not on every card render.
	d.sizesText, d.sizeNamesText = d.sizeList(), d.sizeNames()
}

// homeMaxCopies is the most cards one widget may have on a home.
const homeMaxCopies = 4

// copyKey is the layout key of a widget's nth card: the id for the
// first, "id#n" for a copy.
func copyKey(id string, n int) string {
	if n <= 1 {
		return id
	}
	return id + "#" + strconv.Itoa(n)
}

// widgetDefForKey is the declaration a layout key names: a widget id,
// or "id#n" for a copy of a widget that allows copies (n from 2 to
// homeMaxCopies).
func widgetDefForKey(key string) (*homeWidgetDef, bool) {
	id, n, copied := strings.Cut(key, "#")
	def, ok := widgetDefFor(id)
	if !ok || !copied {
		return def, ok
	}
	i, err := strconv.Atoi(n)
	if err != nil || i < 2 || i > homeMaxCopies || strconv.Itoa(i) != n || !def.Multi {
		return nil, false
	}
	return def, true
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

// narrowSize is the size a phone draws in place of s: when every size
// of the form drawing s is two or more columns wide and the widget
// allows a one-column size, the one-column size at the nearest height
// (the shorter on a tie). ok is false when s's form already has a
// one-column size or the widget has none.
func (d *homeWidgetDef) narrowSize(s wSize) (wSize, bool) {
	i, ok := d.formOf[s]
	if !ok {
		return wSize{}, false
	}
	for _, raw := range strings.Fields(d.Forms[i].Sizes) {
		if fs, _ := parseWSize(raw); fs.W < 2 {
			return wSize{}, false
		}
	}
	var best wSize
	found := false
	for _, c := range d.sizes {
		if c.W != 1 {
			continue
		}
		if !found || absInt(c.H-s.H) < absInt(best.H-s.H) {
			best, found = c, true
		}
	}
	return best, found
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

// wLead is the hero figure a widget leads with, for the w-lead
// partial: the value shown short, the whole figure for its title, and
// how it changed.
type wLead struct {
	Label      string
	Value      string // auto-compact: "4.21B"
	Unit       string // "ISK"
	Full       string // the whole figure, for the title
	Change     string // "+1.24B (+3.1%)"
	ChangeText string // the change in words, for its title
	Up         bool   // the change's direction
	UpIsGood   bool   // whether up reads as good news
	Sub        string
	Href       string
}

// Tone is the class the change takes: good or bad news.
func (l wLead) Tone() string {
	if l.Change == "" {
		return ""
	}
	if l.Up == l.UpIsGood {
		return "w-good"
	}
	return "w-bad"
}

// wTile is one stat tile in a w-tiles row.
type wTile struct {
	Label string
	Value string // auto-compact
	Unit  string
	Full  string // the whole figure, for the title
	Sub   string
	Href  string
}

// wEmpty is what a widget with nothing to show says: one sentence and,
// when there is one, the next step and where it is.
type wEmpty struct{ Text, Step, Href string }

// homeShared is the per-render cache: each fact a widget reads is
// computed once and reused by every widget that shows it.
type homeShared struct {
	app     *Application
	userID  int64
	chars   []db.Character
	bundles []*charSnaps
	now     time.Time
	market  *homeMarketShared
	configs map[string]string // stored settings by layout key, read on first use

	netWorth      *netWorthWidget
	netWorthBuilt bool

	briefingSince    time.Time
	briefingSinceSet bool

	queueEnds map[int64]time.Time // QueueEnds: each pilot's last queued finish

	piRows      []piExpiryLine // PIExpiries
	piRowsBuilt bool
	upkeep      *upkeepWidget // Upkeep
	fleets      []homeFleet   // Fleets
	fleetsBuilt bool

	intelID         intelIdentity   // IntelChannels
	intelChannels   []accessChannel // IntelChannels
	intelChansBuilt bool
}

// NetWorth is the account's net worth, built at most once per render:
// At a Glance carries the same figures, and the default layout holds both.
func (s *homeShared) NetWorth(ctx context.Context) *netWorthWidget {
	if !s.netWorthBuilt {
		s.netWorth, s.netWorthBuilt = s.app.buildNetWorth(ctx, s.userID, s.bundles, nil), true
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

// pinBriefingSince keeps the Briefing on the window a page was drawn
// with (unix seconds, sent back by the page): the full render moved
// the anchor, so a card fetched after it would otherwise drop the
// lines the reader is looking at. The window still reaches back at
// most briefingMaxWindow; a malformed or future value is ignored.
func (s *homeShared) pinBriefingSince(v string) {
	sec, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return
	}
	at := time.Unix(sec, 0)
	if at.After(s.now) {
		return
	}
	if floor := s.now.Add(-briefingMaxWindow); at.Before(floor) {
		at = floor
	}
	s.briefingSince, s.briefingSinceSet = at, true
}

// QueueEnds is when each pilot's skill queue runs out, by character id:
// the last finish date in the stored queue (the Briefing's
// lastQueueFinish). A pilot with no dated queue entry is left out.
// Computed once per render for every widget that shows it.
func (s *homeShared) QueueEnds() map[int64]time.Time {
	if s.queueEnds == nil {
		s.queueEnds = make(map[int64]time.Time, len(s.bundles))
		for _, b := range s.bundles {
			if !b.queueKnown {
				continue
			}
			if t, ok := parseRFC3339(lastQueueFinish(b.queue)); ok {
				s.queueEnds[b.ch.CharacterID] = t
			}
		}
	}
	return s.queueEnds
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
	HrefName             string // the page Href opens, by its menu name: "3 more on the Wallet page"
	Size                 wSize
	Form                 string
	Sizes                string              // data-sizes: every allowed size
	SizeNames            string              // data-size-names: JSON, size -> {"form", "says"} for the size picker
	SizeClass            string              // gw1-3 gh1-4: the grid classes the CSS applies
	Cap                  int                 // rows or items the server renders before "N more", by height
	Customize            bool                // rendering under /?customize=1 (form return targets)
	Expanded             bool                // the Expand view: the largest form, in the shared modal
	Settings             *widgetSettingsView // Customize: the settings sheet, when the widget has options
	Data                 any                 // the builder's value
	Body                 template.HTML
	NarrowBody           template.HTML  // the one-column form a phone draws in place of Body, when the widget has one
	BodyClass            string         // set while a body renders: "w-wide" beside a narrow body, "w-narrow" on it
	ViewerChars          map[int64]bool // the account's characters, for charLink
}

// Uid is an element id unique to this body: widget templates use it
// instead of fixed ids, since one page may hold more than one body (a
// phone body beside the wide one, the Expand view beside the card).
func (w homeWidget) Uid(name string) string {
	id := "w-" + strings.ReplaceAll(w.Key, "#", "-") + "-" + name
	if w.BodyClass == "w-narrow" {
		id += "-n"
	}
	if w.Expanded {
		id += "-x"
	}
	return id
}

// buildHomeWidget builds one card for item at its snapped size and
// renders its body. It returns false when the widget is quiet and the
// home is not being customized.
func (app *Application) buildHomeWidget(ctx context.Context, ts *template.Template, def *homeWidgetDef, item homeLayoutItem, shared *homeShared, customize, expanded bool, viewer map[int64]bool) (homeWidget, bool) {
	size := def.sizeOf(item)
	form := def.FormFor(size)
	w := homeWidget{
		ID: def.ID, Key: item.LayoutKey(), Title: def.Title, Href: def.Href, HrefName: navLabelFor(def.Href),
		Size: size, Form: form.Name, Sizes: def.sizesText, SizeNames: def.sizeNamesText,
		SizeClass:   fmt.Sprintf("gw%d gh%d", size.W, size.H),
		Cap:         homeCap(size.H),
		Customize:   customize,
		Expanded:    expanded,
		ViewerChars: viewer,
	}
	in := &widgetInput{
		UserID: shared.userID, Size: size, Forms: []string{form.Name},
		Opts:  def.defaultOptions(),
		Scope: widgetScope{Value: scopeAll},
		Chars: shared.chars, Bundles: shared.bundles, Shared: shared, Now: shared.now,
	}
	if len(def.Options) > 0 {
		in.Opts = def.optionsFrom(shared.Config(ctx, w.Key), shared.chars)
		if o, ok := def.scopeOption(); ok {
			in.Chars, in.Bundles, in.Scope = applyScope(in.Opts.Get(o.Key), shared.chars, shared.bundles)
			if label := in.Scope.Label(); label != "" {
				w.Title += " · " + label
			}
		}
		if customize {
			w.Settings = def.settingsView(w.Key, in.Opts, shared.chars)
			shared.fillCorpChoices(ctx, w.Settings, in.Opts)
		}
	}
	if ns, ok := def.narrowSize(size); ok {
		in.Forms = append(in.Forms, def.FormFor(ns).Name)
	}
	w.Data = def.Build(ctx, app, in)
	if q, ok := w.Data.(widgetQuiet); ok && q.Quiet() && !customize {
		return w, false
	}
	if ts != nil {
		if err := renderWidgetBodies(ts, def, &w); err != nil {
			logging.Errorf("home: render widget %s: %v", w.ID, err)
		}
	}
	return w, true
}

// renderWidgetBodies executes the card's form template into Body and,
// when the widget has a one-column form a phone draws instead, that
// form into NarrowBody. home.css shows one of the two by the card's
// width.
func renderWidgetBodies(ts *template.Template, def *homeWidgetDef, w *homeWidget) error {
	ns, narrow := def.narrowSize(w.Size)
	var buf bytes.Buffer
	if narrow {
		w.BodyClass = "w-wide"
	}
	if err := ts.ExecuteTemplate(&buf, "w-"+w.ID+"-"+w.Form, w); err != nil {
		return err
	}
	w.Body = template.HTML(buf.String())
	if narrow {
		n := *w
		n.Size, n.Form, n.BodyClass, n.Body = ns, def.FormFor(ns).Name, "w-narrow", ""
		buf.Reset()
		if err := ts.ExecuteTemplate(&buf, "w-"+n.ID+"-"+n.Form, n); err != nil {
			return err
		}
		w.NarrowBody = template.HTML(buf.String())
	}
	w.BodyClass = ""
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

// navLabelFor is the menu name of the page at href ("Wallet"), or ""
// when no menu entry opens it.
func navLabelFor(href string) string {
	if href == "" {
		return ""
	}
	for _, entries := range sectionNav {
		for _, e := range entries {
			if e.Href == href {
				return e.Label
			}
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// The corporation option: every corporation the account's characters
// are in, or one of them, for widgets that read corporation data.
// Values are "all" and "corp:<id>". The settings sheet lists the
// corporations by name and leaves the field out while there is only
// one; a value naming a corporation the card cannot read reads as all
// (the builder's job, since only it knows what it can read).
// ---------------------------------------------------------------------------

const (
	optKindCorp = "corp" // all corporations or one: all, corp:<id>
	corpOptPfx  = "corp:"
)

// optCorp is the corporations a widget reads: all of them by default.
func optCorp() widgetOption {
	return widgetOption{Key: "corp", Label: "Corporation", Kind: optKindCorp, Default: scopeAll}
}

// corpOptionID is the corporation a corporation option's value names,
// 0 for all corporations or a malformed value.
func corpOptionID(v string) int64 {
	raw, ok := strings.CutPrefix(v, corpOptPfx)
	if !ok {
		return 0
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0
	}
	return id
}

// fillCorpChoices gives a settings sheet's corporation fields their
// choices: every corporation the account's characters are in, by
// name. A field with one corporation or none to choose from is left
// out of the sheet.
func (s *homeShared) fillCorpChoices(ctx context.Context, view *widgetSettingsView, opts widgetOptions) {
	if view == nil {
		return
	}
	var fields []widgetSettingsField
	var corps []pulseCorpRef
	read := false
	for _, f := range view.Fields {
		if f.Kind == optKindCorp {
			if !read {
				corps, read = s.app.pulseCorps(ctx, s.chars), true
			}
			if len(corps) < 2 {
				continue
			}
			cur := opts.Get(f.Key)
			f.Choices = []widgetSettingsChoice{{Value: scopeAll, Label: "All corporations", Selected: cur == scopeAll}}
			for _, c := range corps {
				val := corpOptPfx + strconv.FormatInt(c.CorpID, 10)
				f.Choices = append(f.Choices, widgetSettingsChoice{Value: val, Label: s.app.pulseCorpName(ctx, c), Selected: cur == val})
			}
		}
		fields = append(fields, f)
	}
	view.Fields = fields
}
