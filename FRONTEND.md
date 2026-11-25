# EveSynapse front end

How the browser side of EveSynapse works, for anyone changing it.
The short version: the server renders finished HTML, JavaScript
only ever *enhances* it, and there is no build step. If you are
looking for the framework, this document is it, the conventions
below are the framework, and the test suite enforces the load-
bearing ones.

## The model

- **Server-rendered, always.** Go templates produce complete
  pages. With JavaScript disabled the site still works: the nav
  is `<details>`-driven, forms submit, panels are simply
  unfolded. Every script is progressive enhancement, never a
  requirement.
- **No framework, no bundler, no TypeScript, no npm.** This is
  deliberate and settled (my ruling): the deploy artifact is
  one Go binary with the assets embedded, and nothing in the
  deploy path may depend on a Node toolchain or a package
  registry. CI runs Node only to execute the JS tests.
- **Precompute at ingest, never at render** applies to the front
  end too: pages read stored results and ship them as HTML or as
  a JSON payload. Client code formats and interacts; it does not
  go back to the server for data the render already had.

## The files (internal/app/static/)

- `boot.js`, a plain script in `<head>`, not deferred, so a saved
  theme/nav state applies before first paint. Dependency-free and
  tiny on purpose: every byte here delays first paint.
- `swap.js`, the persistent shell. Intercepts navigation,
  fetches the next page, replaces `#main-content`, and fires the
  lifecycle events below. The sidebar, top bar and shell survive
  across pages.
- `app.js`, shell behavior that binds once (quick jump, theme
  toggle, search suggestions, the modal implementation, the
  content-ready bus itself) plus the generic widget kit.
- Per-surface files, loaded only where they belong (base.html
  gates them on section/page): `fit.js` (fitting), `plan.js`
  (build planner), `pi.js` (PI planner), `push.js` (notification
  settings), `home.js` (the home grid: placement, list fitting,
  Customize, the fleet filter, the Briefing pager), `map.js` (the universe map: a canvas drawn from the
  stored, content-hashed payload at `/map/universe.json`, its
  colors read from `map.css` tokens and the legend's `.sec-N`
  swatches), and the surface files split out of app.js. A page
  script gated to one page loads after the shell's first
  `content-ready`, so it runs once for the document as it stands
  and subscribes with `onReady` for later swaps (`plan.js` and
  `map.js` show the shape).
- `sw.js`, the service worker.
- CSS: `layout.css` (the layout framework: where page regions go
  and how they collapse; see "Layout" below), `style.css` (the
  core; it is at the practical size ceiling,
  so a feature with substantial styles gets its own file, linked
  in base.html after `plan.css`, see `plan.css`,
  `tradeideas.css`), `bg.css` (wallpaper). Both themes are one
  stylesheet: dark by default, light under
  `html[data-theme="light"]`.
- Every asset URL carries `?v={{.AssetVersion}}`. Cache-busting
  depends on it; never link a static file without it.

## The swap lifecycle (the part that bites)

Navigation does not reload the page. swap.js fires, in order:

1. `evesynapse:content-will-swap` on document, teardown time.
2. `#main-content` is replaced.
3. `evesynapse:content-ready` on document, with `detail.root` set
   to the new fragment.

Rules that follow from this:

- **Widgets subscribe, they don't run at parse time.** Use
  `EveSynapseContent.onReady(fn)`; `fn(root)` runs once for the
  first paint and again for every swap, with the fresh fragment
  as `root`. Initialize only what's inside `root`.
- **Initializers must be idempotent and root-scoped.** Running
  twice over the same markup must be harmless; query inside
  `root`, never the whole document, for widget elements.
- **Element bindings are safe; global ones are on you.** Listeners
  on elements inside the fragment die with it. Anything bound to
  `document`/`window`, and every timer, must be guarded against
  double-binding or torn down on `content-will-swap` (the
  countdown ticker and the page-sync loop are the worked
  examples in app.js).
- **Shell code binds once and never re-runs**, if it needs to
  react to new content (the modal closer, the character-selector
  pruner), it listens for the lifecycle events itself.
- A dialog open inside content being swapped out closes with it.
- **The swapper never strands a page without its assets.** A
  swap adopts the incoming page's stylesheet and any page
  scripts the shell hasn't loaded (in document order, against
  the swapped-in content, before content-ready), and prunes the
  stylesheet again when a later page doesn't carry it. Only an
  asset that fails to load falls back to a full navigation,
  plus the pages whose JS holds state (the fitting simulator
  and saved fits always load whole regardless). That's what
  makes per-surface files safe: gate the file in base.html and
  the swap brings it along.

## The common pieces

Everything below already exists. Use these; don't build a
second one. Markup contracts are the whole API, if you change
one side, the other side and its tests change in the same
commit.

### Modal

One implementation behind every pop-up (add-module, fit picker,
planner dialogs, quick jump). Markup: a `.modal-backdrop`
wrapping a `.modal`, rendered hidden. Drive it through
`EveSynapseModal`:

- `open(el, {onClose, focus})`, shows the backdrop in the
  browser's top layer (manual popover, so the blur covers top
  bar and sidebar), captures focus and moves it in.
  `focus: false` opts out.
- `close(el)`, `isOpen(el)`, close returns focus to whatever
  had it; `onClose` runs on every close, however it came.
- Backdrop click, any `[data-close-modal]` inside, or Escape
  closes the topmost. Dialogs inside swapped-out content close
  with the swap.

### Share dialog

One dialog for passing a card image on: `templates/sharecard.html`
(`{{template "sharecard" .}}` with a `module.ShareCard`). It shows
the card's preview, loaded only when the dialog first opens, with
Copy link, Copy image, Download and the device's own share sheet
(the last two copy and share buttons show only where the browser
supports them). Any element with `data-share-card="{id}"` opens
it through `EveSynapseModal`; an opener may carry
`data-share-image`, `data-share-link`, `data-share-download` and
`data-share-name` to point the dialog at another card (the
fitting tool does, once a fit has a page). Card routes answer
`?download=1` with an attachment disposition.

### Tabs

A strategy/tabbed block is a `[data-tabs]` container whose
tablist renders `hidden` and whose panels, `role="tabpanel"`,
each with an `id`, each tab a `role="tab"` button with
`aria-controls` naming its panel and `id="<panel>-tab"`, all
render stacked under their own headings. That's the no-JS page.
JS unhides the list, shows one panel at a time, and adds
`.tabs-on` to the container so CSS can drop the per-panel
headings. Arrow keys move along the list; a URL hash naming a
panel opens it. The trade-ideas page and home module are the
worked example.

### Expanders (More / Less)

One mechanism for every summary/detail pair: the detail block
carries `data-wmore`, its control carries `data-expand` naming
the block's id. Table rows that own a detail row render their
control server-side and use the same switch. Without JS every
detail block renders open. Home widgets do not use it: a card is
exactly its cells, so it has Expand instead (below).

### Collapsible sections

A section marked `data-fold="closed"` starts folded; fold
buttons are added per node by app.js, so fresh swapped content
folds the same way. Fold at the smallest sensible unit on the
page, not around whole pages.

### Section tabs (the sub-nav)

A page's section tabs are `{{template "sectiontabs" .}}` in its
`pagetabs` block. base.html renders that block inside the sub-nav
bar (`.subnav`), first in main, so it docks under the top bar and
rides with it. The bar carries the section's category navigation
on one row: the groups as inverted folder tabs hanging from the top
bar's edge, then the active group's pages. It renders the current
section's links from `sectionNav` (nav.go), the same list the
sidebar menus render from, and appears only on the section's own
pages. Never hand-write a tab strip in a page body. When tabs stop
fitting their row, app.js folds the ones that don't fit into the
strip's More disclosure: the fit is measured, the active tab never
folds, and widening restores.

In the phone band (max-width: 45rem) the shell navigates from a
fixed bottom bar instead: Home, the `bottomBarSlots` sections
(nav.go) and a Sections button opening a sheet with every section,
its groups, the character switcher and the account actions, all
derived from the same `sectionNav`. The sub-nav keeps one row
there: the group tabs step aside and the page tabs swipe sideways.
The bar and sheet live in the shell (base.html); app.js re-marks
their active section after each swap.

### Search boxes

Every search box is the same autocomplete widget over one
suggestion endpoint. Markup: a form whose input carries
`role="combobox"`, `aria-controls` naming a
`<ul class="suggest" role="listbox" hidden>` beside it. The
surface's JS calls `attachSuggest(input, list, endpoint,
onPick)`: the box names its feed (items, pilots, types, …, a
pool of the one endpoint) and what a pick does (navigate, fill
a hidden id, add a row). Without JS, or without a feed, the
plain form submits exactly as before.

### Quick jump

Ctrl+K opens the palette, a modal over two sources: pages,
from `quickJumpPages` in app.js (which must mirror `sectionNav`
exactly; `TestQuickJumpPagesMatchNav` enforces it), and
entities (Pages / Characters / Items / Corporations /
Alliances / Pilots) from the search endpoint, each kind with
its own target URL. Adding a page to the palette is the nav
recipe under "Adding things".

### Buttons and forms

- `.btn` is the button; `.btn-quiet` is the low-key variant,
  `.btn-danger` the destructive one. Disabled is the native
  attribute / `aria-disabled`.
- A button that navigates carries `data-href="/path"` (same-
  site paths only), CSP allows no inline handlers, so there is
  no `onclick`, ever.
- A form that needs a confirm carries `data-confirm="…"`; the
  ask happens before submit, and the form still works without
  JS as a plain submit.
- A filter control that should apply immediately carries
  `data-autosubmit`; its form keeps its submit button for
  no-JS readers.

### Pending names (live regions)

Pages render cache-only, so a name the app hasn't resolved yet
renders as a live region that fills itself in: the view sets a
`Pending` flag, and the template renders

```html
<span data-live-region data-poll-url="/labels/corporation-fragment?id={{.ID}}"
      data-poll-state="pending"><span class="loading-pulse"></span> Loading name…</span>
```

instead of the name. The fragment endpoints exist per kind
(character / corporation / alliance / place); a settled miss
stops polling and leaves the plain `#<id>` text. A whole page
still loading uses the same mechanism on a wrapper with the
page's own fragment URL. Once resolved, names render through
the link helpers (`charLink`, `corpLink`, …), a resolved name
is a link to its page, never bare text.

### Countdowns

Any element with `data-finish="<RFC3339>"` counts down to that
moment. The server renders the static remainder as the text,
that's the no-JS fallback and the pre-JS paint, and one shared
ticker updates all of them, stopping on will-swap and starting
on content-ready. Each tick reads every countdown in the document,
so a fragment swapped in alone joins the same ticker.

### Tables with a filter box

An `<input data-table-filter="<table id>" data-row-class="<row
class>" data-header-class="<header class>">` filters its table
client-side: rows carry a `data-search` attribute, rows whose
text misses hide, and category headers with no visible rows
hide with them (`data-header-class` is optional). The industry
pages are the worked example. This is a convenience over an
already-rendered table, never a substitute for the bounded-
read rule on big data.

### Home widgets

A widget is one declaration and one template file:

- `internal/app/homew_<id>.go` calls `registerHomeWidget` from
  `init()` with a `homeWidgetDef`: id, title, description,
  category, manifest feature, the module it requires (if any),
  the page it links to, its default size, its forms with the
  sizes that draw each one, the snapshot kinds its builder reads
  (`Needs`), its options, and the builder. The sizes the forms
  list are exactly the sizes the widget may take; a stored size
  outside them snaps to one of them. The types, the registry and
  the size rules are in `home_widget_kit.go`.
- `internal/app/templates/widgets/<id>.html` defines one body per
  form, `w-<id>-<form>`. Its data is the card view (`.Size`,
  `.Form`, `.Cap`, `.Customize`, `.ViewerChars`) with the
  builder's value as `.Data`. Element ids come from
  `{{.Uid "name"}}`, never a fixed id: a page may hold more than
  one body.

Nothing else names the widget: the catalog, the size picker's
bounds, the snapshot read (one batched query for the union of the
visible widgets' `Needs`) and the card shell all come from the
declaration. `templates/widgets/_card.html` is the one shell,
`<section class="card w-card" data-widget data-key data-gw
data-gh data-sizes …>` with an `<h3>` title (a link to the
widget's page outside Customize), the Expand button, and a
`.card-body` holding the rendered bodies, plus `data-size-names`
(JSON: each size to its form and the size picker's sentence),
`data-href` and `data-href-name` (the page and its menu name, for
"3 more on the Jobs page"). Bodies render from stored data only.
Data that can have nothing to say implements `Quiet() bool`, and
a quiet card is left off the home outside Customize. Customize
(drag, resize, remove, add) is layered over the plain customize
forms, never a replacement for them.

**Forms.** A size draws the form that lists it, and a template
picks parts, never sizes: the builder is told which forms this
render draws (`in.Draws("large")`) and skips data no drawn form
shows. When every size of the drawn form is two or more columns
wide and the widget has a one-column size, the card also renders
that one-column form (at the nearest height) into `NarrowBody`;
home.css shows it in place of the wide body when the card is
under 30rem wide, which is a phone. Options (`optScope`,
`optChoice`) are declared now and draw at their defaults until
the settings sheet stores values.

**Kit parts** (`templates/widgets/_kit.html`; Go types in
home_widget_kit.go and home_widget_chart.go; styles in home.css).
A form's body is one `<div class="w-body {{.BodyClass}}">`, a
column of parts:

- `w-lead` (`wLead`): the hero figure, short ("4.6B ISK"), the
  whole figure as its title, its change with direction and
  whether up is good news (`.w-good`, `.w-bad`).
- `w-tiles` (`[]wTile`): stat tiles on `.l-stat` that wrap by
  width; where rows are fixed a second row of tiles is not drawn.
- `w-chart` (`*wChart`): a line or stacked area chart that fills
  the room the other parts leave. Built by `wLineChart` and
  `wStackedChart` from at most a year of days (a long series keeps
  every nth day): data units on an SVG with
  `preserveAspectRatio="none"`, lines that keep 2px through
  `vector-effect`, a legend for two or more series, value labels
  in a gutter (hidden on a chart under 6rem tall), dates placed by
  `data-left`, hover columns for the shared chart tooltip
  (`data-date`, `data-tip-rows`), and a visually hidden table twin
  (`w-twin`). Where rows are fixed a chart claims more than half
  its body, so a later part that no longer fits is not drawn.
- `w-spark`: the same line with no labels, filling its slot.
- `w-segbar` (`wSegments`): one whole split into up to six parts,
  a thin bar with each part's name and figure under it.
- `w-rank` (`wRankedBars`): ranked bars, largest first; it fits
  its card like any list.
- `w-meters`: name, figure and bar rows (`.Name`, `.Pct`, `.Text`,
  `.Tone`).
- `w-note`: the muted as-of or provenance line, left out on a card
  under 12rem tall. `w-empty` (`wEmpty`): one sentence and the next
  step, centred.
- Lists: any list marked `data-w-fit` (its children are its rows)
  fits its card; rows past the server's cap carry `data-w-over`.
  home.js writes the "3 more on the Jobs page" line after it
  (`.w-more`).

Arrangement, the same for every widget: `.w-split` puts the first
part (or a `.w-side`) beside the rest when the card is much wider
than tall (a size container's aspect ratio, so cards that take
their content's height stack); `.w-head` sets the lead beside its
tiles; `.w-cols` sets the chart beside a side list. Series colours
are `--w-s1` to `--w-s4` (`.w-s1` on a mark), one categorical
order checked for colour-blind separation on the card surface;
text never takes them.

**Expand.** Each card's heading has an Expand button (shown by
home.js): it opens the widget's largest form in the shared modal,
fetched from the one-card route with `view=expand` into a box the
form fills. A legacy widget's detail blocks (`data-w-detail`) and
its rows past the cap show only there. Without JavaScript the
title links to the widget's page.

One card renders alone at `GET /home/widget/{key}` (signed in, own
home only, `Cache-Control: no-store`): at its stored size, at `w`
and `h` when the widget declares that size, or with `view=expand`
at its largest. It reads only that widget's `Needs`, answers 204
when the widget has nothing to show, and never moves the
Briefing's "since you last looked" mark. Customize swaps a card
for this render after each resize and each add; the old card
stays, dimmed, until the new one lands. home.js then dispatches
`evesynapse:content-ready` with `detail.root` set to the card and
`detail.part` set, so every `onReady` initializer runs over the
card alone. A listener that restarts page-level work (the page
sync poller) skips events marked `part`.

### Character selector

Pages scoped to one pilot render the `charselector` partial
(templates/charselector.html) with the viewer's character
list: a filterable dropdown of `?character=` links. Picking
one reloads the page under that character, and the page's
section links carry the parameter from there. Character-
scoped pages take the selected character from the shared page
data, don't parse the parameter or build a second picker.

### Auto-refresh pages

A page that refreshes itself sets `data-autorefresh` on the
body (the page data's AutoRefresh flag) and ships a
`<noscript>` meta-refresh doing the same. The JS reload waits
out typing in a field and hidden tabs, never runs for
reduced-motion readers, and dies on swap-away, swaps *onto*
an auto-refresh page fall back to a full load, so the chain
only ever starts on first paint. Sync is the worked example.

### Tables

A data table with a header row fits its box at every width with
nothing extra. app.js measures each one, and re-measures whenever
its box changes width (the window, the sidebar, a pane beside it):
a table whose columns fit stays a table, and a table that would
scroll sideways reads as cards. Each row becomes a card, its first
cell the title and every other cell a figure under its column's
label. The labels are copied from the header row (`data-label`), so
no template carries them twice, and a sortable table keeps a Sort
picker that drives the header-click sort. A table's wrapper never
grows past its container, so the measure is always honest. Levers
on the markup:

- `class="opt"` on a header cell keeps that column off the
  cards (the same low-priority mark the home modules use).
- `class="card-title"` on a header cell makes that column the
  card's title in place of the first cell (Killmails titles each
  card with the victim's ship, not the time). A cell with nothing
  in it stays off its card.
- `class="card-figure"` on a header cell makes that column the
  card's headline figure: unlabelled, on the title's line at the
  right edge (a basket item's worth).
- `class="cards-strip"` on the table runs a card's few short
  figures as one even strip under the title line instead of two-up
  (basket items: qty, share, change, effect).
- Action-only cells (a Pin, move arrows) gather on the card's
  bottom row, the last at the right edge. A table's shrink-to-fit
  cell (`width: 1%`) is reset on cards, so it never collapses.
- The Sort picker sits inside the table's wrapper, so a table laid
  out in a grid keeps its place.
- `data-cards="on"` forces cards; `data-cards="off"` keeps a
  table that scrolls sideways inside its own wrapper (on phones its
  text columns wrap). Use "off" only for grids whose point is
  comparing columns: order books, price history, the calendar.
- Key/value tables (`th` + `td` rows, no header row) wrap their
  values on phones on their own.
- A highlighted row (`tr.yours`) takes the theme's selection
  paint, the pinned first cell included; as a card the highlight is
  the card's own.
- A row marks itself with an edge (ready to build, a loss, an
  order outbid) by setting `--row-edge` (a colour) and
  `--row-edge-w` (a width) on the `tr`, never a box-shadow of its
  own: the edge rides the first cell in a table and the card's side
  as cards, so it never lands beside a control.

KPI tiles (`.kpis` > `.kpi`) wrap as many across as fit, each at
least `--kpi-min` wide (13rem, 11rem on `.kpis-small`); tiles on a
short last row grow to fill it, so a lone tile never leaves a hole.
For two across, set the tiles' `flex-basis: 40%`, not an exact half.

Grid cells (`.l-grid` with `.s3`/`.s4`): in the six-track band a
half cell left alone on its row takes the whole row.

### Phone sheets

A block of secondary controls (filters, options) that should
step aside on a phone carries `data-sheet` (with an `id` and a
`data-sheet-title`), and its trigger is a button with
`data-sheet-open="<id>"` rendered `hidden`. Off the phone band
the block renders inline and the trigger stays hidden. In the
band, app.js turns the block into a bottom sheet in the top
layer (an auto popover). It stays in place in the DOM, so a
sheet of form fields still submits with its form. JS adds the
sheet's head and close button. Anything inside marked
`.sheet-only` shows only in sheet mode, and a `.sheet-foot`
rides the sheet's bottom edge: put the submit there. Contract
search's filters are the worked example. The character picker
(`[data-checkselector]`) builds its own sheet, titled from its
`data-title`: a picker needs no sheet markup.

Labelled forms on phones (a `<label>` wrapping its caption and
field) share rows two-up with the submit under them; the rule holds
in a page's column layout too, where every field takes the full
width. Don't override it per form.

### Phone and touch styles

`phone.css` is the touch and phone layer. base.html links it
last behind a media attribute, so mouse desktops never block on
it. It is split into bands (touch, phone, short landscape),
each with its own query. Shared phone treatment goes there, in
the band it belongs to. A page with substantial phone styles
keeps them in its own sheet inside the same 45rem query.
Sticky in-page bars offset from `--l-top` (or `--sticky-top` on a
page without a sub-nav), never a literal header height. On short landscape screens the top bar scrolls
away, and the token follows it.

### Geometry through data attributes

CSP forbids inline `style`, so per-element numbers ride
attributes and app.js applies them through the CSSOM:
`data-w` (bar fill width, %), `data-left` / `data-top`
(position, %), `data-rot` (rotation, deg, the element is also
centered on its point), `data-pad-left` (indent, px). Applied
at load and again for injected markup. If a number describes
an element, it goes in one of these, never in a style
attribute.

### Tooltips

Shell controls (nav, top bar) use `data-tip="…"`, CSS-only,
positioned by the shell's styles. Rich content tooltips (fit
modules, market chart points) are surface code (fit.js, the
chart code in app.js) with their own anchoring rules; don't
stretch `data-tip` into one of those.

## Layout

`layout.css` owns where a page's regions go and how they collapse.
A page composes from its parts; its own sheet styles what the
content looks like, never where it goes or when it stacks. That
is the whole rule, and `TestFrameworkPageSheetsMeasureTheirOwnSpace`
holds it for every page on the framework.

### Three rules

1. **Measure the space you have, not the window.** A framework page
   wraps its content in `.l-page`, and the console measures itself.
   Both are size containers. Use the parts below, or a container
   query (`@container page (…)` or `@container console (…)`), when
   something must change with width. Never a window-width
   `@media` in a page sheet: the sidebar opens, collapses and hides,
   so the window says nothing about the page's room.
2. **Collapse into something usable.** Fitting is not enough. When a
   page runs out of room it shows fewer things, never a longer
   stack of everything: one level of a console at a time, the
   pane's search and filters at its top, an obvious way back.
3. **Nothing fixed inside a page.** A size container is the
   containing block for fixed children. Overlays inside `.l-page`
   use the console drawer or the top layer (the modal, the sheets).

### The frame

- The top bar, the sidebar and the bottom bar are the shell
  (base.html). They are glass.
- The sub-nav (`.subnav`) docks under the top bar: the section's
  tabs (see "Section tabs"). Glass.
- The page body is solid (`--surface-page`): dense text and tables
  sit on an opaque ground. Glass is for the frame and for things
  that float (menus, sheets, drawers).
- In-page sticky bars park on `--l-top` (under the top bar and the
  sub-nav), never on a literal height.

### Tokens

- Spacing: `--space-1` to `--space-6` and `--gutter`, each fluid
  between a floor and a ceiling. Use them for gaps and padding
  instead of fixed rem values that jump at breakpoints.
- Surfaces: `--surface-page` (the body), `--surface-pane` (a
  console), `--surface-float` (drawers, bars, anything over
  content), `--surface-inset` (a box inside a pane), `--hairline`.
- Selection: `--sel-bg`, `--sel-edge` and `--sel-row`, all from the
  theme's accent, so every scheme and the light theme highlight in
  their own colour. Never a fixed colour for "selected".
- `--tap`: the smallest touch target (2.75rem).

### Type roles

Type is information. Each attribute says one thing everywhere, so a
reader learns it once and reads every page by it:

- **Face** says what kind of text it is. Rajdhani capitals
  (`--font-display`) are the interface talking: titles, labels,
  column headers, buttons, tabs, chips, badges. Eve Sans Neue
  (`--font-body`) is the data and the sentences: names, places,
  values, descriptions. The figure face (`--font-num`) is a figure
  that stands on its own: a KPI value, a total, a count in a chip.
  Prose (`.prose`) explains.
- **Size** says rank. One step per level, never a one-off.
- **Weight** says emphasis or state, never decoration. Regular is
  the norm; bold marks a group or category row, a total, unread or
  new, or the one figure a panel exists to show. A list entry's
  name is never bold: its color already marks it.
- **Color** says what the text does. Gold (`--gold`) opens
  something. `--text` is plain data, `--text-bright` the name of a
  row that does not link. `--muted` is secondary (sub-lines, units,
  timestamps). Status colors (`--v-*`, `--kept`, `--danger`) carry
  a state and nothing else. Section titles take the heading gold.

| Role | Face | Size | Weight | Color |
| --- | --- | --- | --- | --- |
| Page title (h1) | display caps | `--fs-4xl` | 700 | `--text` |
| Panel title (h2) | display caps | `--fs-xl` | `--fw-display` | heading gold |
| Card or pane title (h3, `.l-pane-title`) | display caps | `--fs-md` | `--fw-display` | heading gold |
| Kicker (a sub-section inside a panel) | display caps | `--fs-xs` | `--fw-display` | `--sch-hi`, `--muted` without a scheme |
| Label (field label, filter group, legend) | display caps | `--fs-2xs` | `--fw-display` | `--muted-2` |
| Column header (`thead th`) | display caps | `--fs-2xs` | `--fw-display` | `--text-bright` |
| Stat label (over a tile figure) | display caps | `--fs-xs` | `--fw-display` | `--muted-2` |
| Button, tab | display caps | `--fs-xs` | `--fw-display` | as its rule |
| Chip (a filter control) | display caps | `--fs-2xs` | 600 | `--muted`, on: `--text-bright` |
| Badge (a status word) | display caps | `--fs-2xs` | `--fw-display` | its tone |
| Security status | figure face in a pill | `--fs-2xs` | `--fw-num` | `.sec-N` |
| Data (cells, list rows, row headers) | body | `--fs-sm` | 400 | gold if it links, else `--text` |
| Sub-line (under a name) | body | `--fs-xs` | 400 | `--muted` |
| Figure (tile value, total) | figure face | by its tile | `--fw-num` | `--text` |
| Hint (under a control) | body | `--fs-xs` | 400 | `--muted` |

Weights come from tokens because platforms draw the same weight
differently: boot.js marks Windows and Linux on `<html>`
(`data-os`), Windows takes `--fw-display`, `--fw-display-strong`
and `--fw-num` one step lighter, and Linux draws glyphs unhinted.
Never write 500 or 600 for the display or figure face; name the
token.

In style.css each role is one rule under "Type roles", and a class
takes a role by joining that rule's selector list. A page sheet
gives a member its shape, spacing and tone, never its face, size,
weight, case or letter spacing.

### Arrangement patterns

Each arranges its children from the space it has, with custom
properties as knobs and no media queries:

- `.l-stack`: a column with one gap (`--l-gap`).
- `.l-cluster`: a wrapping row of controls or chips; `.l-push`
  sends one to the far end.
- `.l-switcher`: side by side while each child gets `--l-min`,
  all stacked once one would not. Never a ragged split.
- `.l-sidebar`: a side of `--l-side` beside a main that wraps under
  when it would drop below `--l-main-min`. `.l-sidebar--end` puts
  the side on the right.

### Page parts

- `.l-page`: the page's container. Everything below measures it.
- `.l-head`: the page head under the title. `.l-head-pick` holds
  the page's character or scope picker, `.l-head-sum` is the
  one-line summary, `.l-head-act` holds the scope control and the
  primary action at the far end.
- `.l-tiles`: a row of stat tiles that steps 4, 2, 1 (or 3, 1, or
  6, 3, 1). It counts its own tiles, so conditional tiles need
  nothing extra, and a row never splits 3 and 1.
  `.l-tiles--cards` is the same row for detail cards (a heading
  over rows of keys and values): one to a row until the page is
  34rem wide, because a pair of them on a phone breaks figures.
- `.l-grid`: the 12-track grid (Home's widget grid, generalized).
  Cells take `.s3` to `.s9` (full width otherwise); under 52rem it
  is 6 tracks with every cell at least half, under 34rem one
  column.
- `.l-panel`: the standard solid block on the page.
- `.l-root`: page content that belongs to the top level (summary
  cards, overview tables, banners). On a narrow page it steps
  aside once the reader goes below the console's top level (the
  directory, or the list when the console has no directory), so
  the deeper screen starts under the page head.

### The home grid

The home is a grid of widget cards on its own sheet, `home.css`,
inside `.l-page`, so it measures the page and never the window.

- **Columns** come from the page's width: three when the page is
  at least 60rem wide, two from 36rem, one below. A card takes
  `gw1`-`gw3` columns and `gh1`-`gh4` rows (its stored size); with
  two columns a width of 3 draws as 2, and with one every card is
  full width. `--home-cols` on the grid carries the count to
  home.js.
- **Rows** are named on the grid by `data-rows`. `auto`: a row is
  as tall as what its cards hold (with two columns every card spans
  one row). `fixed` (`?rows=fixed`, not stored): every row is
  `--w-row` tall, growing with the page, and a card is exactly its
  cells. Cards are size containers there (`container: widget /
  size`), so their parts can read the box. Below 36rem rows are auto
  in both kinds and cards are only inline-size containers: a size
  container with an auto height collapses to nothing.
- **Placement**: home.js writes each card's `grid-column` and
  `grid-row` through the CSSOM. `homePack` puts each card at the
  first free spot, row by row, the rule CSS `row dense` follows;
  outside Customize `homeFillGaps` then closes a hole beside a card
  by widening it, or under one by lengthening it, only to a size the
  card declares. In Customize the holes show as "Add a widget"
  cells that open the Add a widget dialog. It runs on load, on
  content-ready, when the grid changes width, and inside every
  Customize change. Without JavaScript the span classes and `row
  dense` lay the grid out.
- **Fitting** (fixed rows): a list marked `data-w-fit` (its
  children are its rows) shows the whole rows that fit, and a line
  after it says how many more there are, linking to the card's
  `data-href`. When the card still does not fit, its parts after the
  first are not drawn, the last first: a card never shows a part cut
  in half; in a kit body the parts are the `.w-body`'s children.
  `homeFitCount` is the pure count. Where cards take their
  content's height (auto rows, one column) the server's cap
  (`data-w-over`) holds, with the same line.

### The console

The master-detail page: a directory, a main pane and an inspector.
Assets and the Character overview are the worked examples.

```html
<div class="l-console" data-depth="1" data-land-top>
  <div class="l-panes">
    <nav class="l-pane l-pane--dir">
      <div class="l-pane-bar"><h2 class="l-pane-title">…</h2> …filter…</div>
      … entries, each <a … data-l-step="in"> …
    </nav>
    <section class="l-pane l-pane--main">
      <div class="l-pane-bar"><a class="l-back" data-l-step="out" href="…">Directory</a> …search…</div>
      …
    </section>
    <aside class="l-pane l-pane--insp">…</aside>
    <a class="l-scrim" href="…close…" aria-label="Close" tabindex="-1"></a>
  </div>
</div>
```

- **Depth comes from the URL.** The page renders `data-depth`: 0
  with nothing picked (the directory is the reader's place), 1 with
  a directory entry open, 2 with a row open in the inspector. The
  server already knows this from the query; no script decides it.
- **The console shows as many panes as fit, always the deepest.**
  70rem and up: all three side by side (the inspector's column
  opens with it). 46 to 70rem: directory and main, and an open
  inspector is a drawer over the main pane with a scrim that closes
  it. Under 46rem: one pane, the reader's own level.
- **Every pane has a bar.** `.l-pane-bar` is its top: sticky, and
  the home of the pane's search and filters, so they stay at the
  top in every form. `.l-back` in the bar returns one level; it
  shows only while the pane it returns to is off screen, so it is
  the native back control on a phone and invisible on a desktop.
  `.l-close` closes a drawer.
- **Level links carry `data-l-step`** (`in` or `out`). With one
  pane showing they land at the console's top like a new screen;
  side by side they keep the reader's place (swap.js).
- **An ambient inspector** (`data-insp="ambient"` on the console)
  is one that shows a default detail before anything is picked. It
  takes the third column in the wide form and stays out of the way
  in the others until a pick opens it.
- Column widths are the page's: set `--l-dir` and `--l-insp` on
  its `.l-panes`.
- Without JS every level is a plain link and the server renders
  the right one.

### Checking a page

A page is done when it is right at every width, not at three.
Sweep it from 320 to 1600px in 20px steps, with the sidebar open,
collapsed and hidden: zero page-level sideways overflow, zero
panels or tables painting outside their box. Then walk it on a
phone width as a reader would: the first screen answers the page's
question, every pick opens a screen that starts at its top, back
returns, and search is where the thumb expects it.

## Components are Go partials

Reusable UI is a shared template with a documented data shape,
rendered by the server, `templates/fit_workbench.html` is the
model (the simulator and the read-only public copies render the
same markup). The shared set lives in `pageSharedTemplates`
(core_pages.go): base, notifybell, balancechart, walletflow,
charselector, fitbrowser, fit_workbench, locked, sectiontabs.
Before building a new pattern, check whether a partial already
does it; before changing a partial, check every page that
renders it.

Two registries are single sources of truth, and tests hold them
to it:

- **Navigation:** `sectionNav` in nav.go drives the sidebar menus
  AND the section tab strip. The quick-jump palette in app.js
  (`quickJumpPages`) must carry the same labels,
  `TestQuickJumpPagesMatchNav` fails otherwise.
- **Home widgets:** one declaration per widget (`homew_<id>.go`,
  on the registry in home_widget_kit.go) defines its id, title,
  sizes and forms, data needs, and the module it requires; its
  manifest feature names the scopes that power it. A widget's
  bodies are `w-<id>-<form>` templates in templates/widgets; its
  data comes from stored sources.

## Server → JS contracts

- Data reaches JS as `data-*` attributes for small facts and as a
  JSON payload for structured state (the planner). The contract
  lives on the Go DTO: invariants are written there (planner
  payload lists are never null, they are coerced in
  `planFinishPreset`, because Go nil slices marshal to `null`
  and plan.js dereferences them) and mirrored in the JS, which
  normalizes defensively in one place (`setPayload`).
- Mirror each payload's shape as a JSDoc `@typedef` in the JS
  file that consumes it. Editors light up, no toolchain needed.
- When a contract changes, the Go DTO comment, the typedef and
  the tests change in the same commit.

## Adding things

- **A nav item:** add the entry to `sectionNav` (nav.go) and the
  matching line to `quickJumpPages` (app.js), same label, Title
  Case. The parity test checks you.
- **A page:** template in `templates/`, route + section wiring,
  and if it needs its own JS, a per-surface file gated in
  base.html on the section or NavPage. No executable inline
  scripts in templates, CSP serves scripts from 'self' plus a
  per-request nonce, and no template uses one; the single
  inline pattern is a `<script type="application/json">` data
  block (the planner's `plan-data`), which is data, not code.
- **A JS widget:** a banner section in the surface's file (the
  `// --- Name ---` banners are the table of contents), an
  `onReady` subscription scoped to `root`, teardown on
  will-swap for anything global, and a node test in
  `internal/app/jstest/` if it carries logic worth pinning,
  the pipeline's JS step names its files explicitly, so add new
  test files to `.github/workflows/heavy-gates.yml` too.
- **A home widget:** `homew_<id>.go` and `templates/widgets/<id>.html`,
  data from a stored source. Nothing else is touched (a preset only
  if the widget belongs in one). Net Worth (`homew_networth.go`,
  `templates/widgets/networth.html`) is the worked example:

  ```go
  func init() {
  	registerHomeWidget(homeWidgetDef{
  		ID: widgetNetWorth, Title: "Net Worth", Category: wcatMoney, Feature: "networth", Href: "/wallet/",
  		Description: "Wallets, assets and buy-order escrow across your characters, and how the total has moved.",
  		Default:     wSize{1, 2},
  		Forms: []homeWidgetForm{
  			{Name: "small", Sizes: "1x1", Says: "The total, its 7-day change and a 30-day line."},
  			{Name: "wide", Sizes: "2x1 3x1", Says: "The total beside its chart."},
  			{Name: "tall", Sizes: "1x2 1x3", Says: "The total, how it splits, and its chart."},
  			{Name: "medium", Sizes: "2x2 2x3", Says: "Wallets, assets and escrow over a chart with dates and values."},
  			{Name: "large", Sizes: "3x2 3x3", Says: "Adds the split by character or by location."},
  		},
  		Needs: []string{esi.SnapWallet, esi.SnapAssets, esi.SnapOrders},
  		Options: []widgetOption{optScope(),
  			optChoice("range", "History", "30", "30:30 days", "90:90 days", "365:A year"),
  			optChoice("split", "Split by", "chars", "chars:Character", "places:Location")},
  		Build: buildNetWorthWidget,
  	})
  }
  ```

  1. Name the forms by what they show and give each a sentence for
     the size picker. Every size must show something its
     neighbours do not (more rows, a chart, a breakdown); if two
     would look alike, drop one.
  2. The builder returns one value for every drawn form and builds
     only what `in.Draws(form)` asks for: Net Worth reads its
     history once and draws a stacked chart for the wide, medium
     and large forms, the total's line for the tall one, ranked
     bars only for the large one. Every read is bounded in SQL;
     shared facts come from `in.Shared` (computed once per render).
  3. Each form template is one `w-body` of kit parts:
     `{{define "w-networth-tall"}}<div class="w-body {{.BodyClass}}">`
     then `w-lead`, `w-segbar`, `w-chart`, `w-note`, and `w-empty`
     for the nothing-yet case. No size checks, no fixed ids.
  4. Check it with the rig: every size at several widths in fixed
     rows (no overflow, a chart at two rows or more at least 40%
     of the body), a phone at 390px (the narrow body), and Expand.

  `TestHomeWidgetDeclarationsHoldTheirContract` checks the
  declaration, its options, and that every form has its template.

## Testing

Mirror CI exactly before pushing: `make assets`, `gofmt -l cmd
internal`, `go vet ./...`, `go test ./...`, and the node list
from the workflow. Tests ship in the same commit as the behavior
change, every historical CI failure here was tests lagging the
code, never the reverse. While iterating, run the affected
package only; run the full gate once, on the final tree.

## Don'ts

- No bundler, TypeScript, or npm dependency for the shipped
  front end (see "The model"). Native language features only.
- No new global namespace. `window.EveSynapseContent` and
  `window.EveSynapseModal` are the existing surface; extend
  those patterns rather than minting another.
- No document-level listener or timer without a will-swap
  teardown or a bind-once guard.
- No fetching at render what ingest could have stored.
- No inline event handlers or executable inline scripts in
  templates (JSON data blocks are the one exception).
- No second implementation of anything in "The common pieces".
