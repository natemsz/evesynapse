# EveSynapse front end

How the browser side of EveSynapse works, for anyone changing it.
The short version: the server renders finished HTML, JavaScript
only ever *enhances* it, and there is no build step. If you are
looking for the framework, this document is it — the conventions
below are the framework, and the test suite enforces the load-
bearing ones.

## The model

- **Server-rendered, always.** Go templates produce complete
  pages. With JavaScript disabled the site still works: the nav
  is `<details>`-driven, forms submit, panels are simply
  unfolded. Every script is progressive enhancement, never a
  requirement.
- **No framework, no bundler, no TypeScript, no npm.** This is
  deliberate and settled (owner doctrine): the deploy artifact is
  one Go binary with the assets embedded, and nothing in the
  deploy path may depend on a Node toolchain or a package
  registry. CI runs Node only to execute the JS tests.
- **Precompute at ingest, never at render** applies to the front
  end too: pages read stored results and ship them as HTML or as
  a JSON payload. Client code formats and interacts; it does not
  go back to the server for data the render already had.

## The files (internal/app/static/)

- `boot.js` — a plain script in `<head>`, not deferred, so a saved
  theme/nav state applies before first paint. Dependency-free and
  tiny on purpose: every byte here delays first paint.
- `swap.js` — the persistent shell. Intercepts navigation,
  fetches the next page, replaces `#main-content`, and fires the
  lifecycle events below. The sidebar, top bar and shell survive
  across pages.
- `app.js` — shell behavior that binds once (quick jump, theme
  toggle, search suggestions, the modal implementation, the
  content-ready bus itself) plus the generic widget kit.
- Per-surface files, loaded only where they belong (base.html
  gates them on section/page): `fit.js` (fitting), `plan.js`
  (build planner), `pi.js` (PI planner), `push.js` (notification
  settings), and the surface files split out of app.js.
- `sw.js` — the service worker.
- CSS: `style.css` (the core; it is at the practical size ceiling,
  so a feature with substantial styles gets its own file, linked
  in base.html after `plan.css` — see `plan.css`,
  `tradeideas.css`), `bg.css` (wallpaper). Both themes are one
  stylesheet: dark by default, light under
  `html[data-theme="light"]`.
- Every asset URL carries `?v={{.AssetVersion}}`. Cache-busting
  depends on it; never link a static file without it.

## The swap lifecycle (the part that bites)

Navigation does not reload the page. swap.js fires, in order:

1. `evesynapse:content-will-swap` on document — teardown time.
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
- **Shell code binds once and never re-runs** — if it needs to
  react to new content (the modal closer, the character-selector
  pruner), it listens for the lifecycle events itself.
- A dialog open inside content being swapped out closes with it.
- **The swapper never strands a page without its scripts.** If
  the incoming page needs a script the shell hasn't loaded, the
  swap falls back to a full navigation (fitting and the planner
  always load whole regardless — their JS holds state). That's
  what makes per-surface files safe: gate the file in base.html
  and the first arrival simply does a real load.

## The common pieces

Everything below already exists. Use these; don't build a
second one. Markup contracts are the whole API — if you change
one side, the other side and its tests change in the same
commit.

### Modal

One implementation behind every pop-up (add-module, fit picker,
planner dialogs, quick jump). Markup: a `.modal-backdrop`
wrapping a `.modal`, rendered hidden. Drive it through
`EveSynapseModal`:

- `open(el, {onClose, focus})` — shows the backdrop in the
  browser's top layer (manual popover, so the blur covers top
  bar and sidebar), captures focus and moves it in.
  `focus: false` opts out.
- `close(el)`, `isOpen(el)` — close returns focus to whatever
  had it; `onClose` runs on every close, however it came.
- Backdrop click, any `[data-close-modal]` inside, or Escape
  closes the topmost. Dialogs inside swapped-out content close
  with the swap.

### Tabs

A strategy/tabbed block is a `[data-tabs]` container whose
tablist renders `hidden` and whose panels — `role="tabpanel"`,
each with an `id`, each tab a `role="tab"` button with
`aria-controls` naming its panel and `id="<panel>-tab"` — all
render stacked under their own headings. That's the no-JS page.
JS unhides the list, shows one panel at a time, and adds
`.tabs-on` to the container so CSS can drop the per-panel
headings. Arrow keys move along the list; a URL hash naming a
panel opens it. The trade-ideas page and home module are the
worked example.

### Expanders (More / Less)

One mechanism for every summary/detail pair: the detail block
carries `data-wmore`, its control carries `data-expand` naming
the block's id. Home modules get their control injected into
the heading automatically (below); table rows that own a
detail row render their control server-side and use the same
switch. Without JS every detail block renders open. Open state
is a per-browser convenience in localStorage
(`evesynapse.home.expanded`).

### Collapsible sections

A section marked `data-fold="closed"` starts folded; fold
buttons are added per node by app.js, so fresh swapped content
folds the same way. Fold at the smallest sensible unit on the
page, not around whole pages.

### Section tabs

The strip under a page's H1 is `{{template "sectiontabs" .}}`.
It renders the current section's links from `sectionNav`
(nav.go) — the same list the sidebar menus render from — and
appears only on the section's own pages, only when the section
has more than one page and one of them is active. Never
hand-write a tab strip in a page template. When a strip's tabs
stop fitting their row, app.js folds the ones that don't fit
into the strip's More disclosure (rendered empty and hidden
by the template): the fit is measured, the active tab never
folds, and widening restores — so a strip whose tabs fit
renders exactly as it did without the disclosure.

### Search boxes

Every search box is the same autocomplete widget over one
suggestion endpoint. Markup: a form whose input carries
`role="combobox"`, `aria-controls` naming a
`<ul class="suggest" role="listbox" hidden>` beside it. The
surface's JS calls `attachSuggest(input, list, endpoint,
onPick)`: the box names its feed (items, pilots, types, … — a
pool of the one endpoint) and what a pick does (navigate, fill
a hidden id, add a row). Without JS, or without a feed, the
plain form submits exactly as before.

### Quick jump

Ctrl+K opens the palette — a modal over two sources: pages,
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
  site paths only) — CSP allows no inline handlers, so there is
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
the link helpers (`charLink`, `corpLink`, …) — a resolved name
is a link to its page, never bare text.

### Countdowns

Any element with `data-finish="<RFC3339>"` counts down to that
moment. The server renders the static remainder as the text —
that's the no-JS fallback and the pre-JS paint — and one shared
ticker updates all of them, stopping on will-swap and starting
on content-ready.

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

A widget is three registrations and a body:

- the module manifest (module_manifest.go): id, title, layer;
- the overview registry (home_overview.go): default size and
  grid bounds (W/H, MinW/MinH);
- a block in home.html keyed by widget id.

home.html renders the shell for every widget —
`<section class="card" data-widget data-gw data-gh …>` with an
`<h3>` title and a `.card-body` — and the block fills
`.card-body` from stored data only. Long lists render in full
and mark rows beyond the widget's cap with `data-wmore`; the
expander machinery (above) supplies the More/Less toggle and
the "all of it on the X page" link is part of the block.
Customize (drag, resize, remove, add) is layered over the
plain customize forms, never a replacement for them; a widget
that renders its shell honestly gets all of it for free.

### Character selector

Pages scoped to one pilot render the `charselector` partial
(templates/charselector.html) with the viewer's character
list: a filterable dropdown of `?character=` links. Picking
one reloads the page under that character, and the page's
section links carry the parameter from there. Character-
scoped pages take the selected character from the shared page
data — don't parse the parameter or build a second picker.

### Auto-refresh pages

A page that refreshes itself sets `data-autorefresh` on the
body (the page data's AutoRefresh flag) and ships a
`<noscript>` meta-refresh doing the same. The JS reload waits
out typing in a field and hidden tabs, never runs for
reduced-motion readers, and dies on swap-away — swaps *onto*
an auto-refresh page fall back to a full load, so the chain
only ever starts on first paint. Sync is the worked example.

### Geometry through data attributes

CSP forbids inline `style`, so per-element numbers ride
attributes and app.js applies them through the CSSOM:
`data-w` (bar fill width, %), `data-left` / `data-top`
(position, %), `data-rot` (rotation, deg — the element is also
centered on its point), `data-pad-left` (indent, px). Applied
at load and again for injected markup. If a number describes
an element, it goes in one of these, never in a style
attribute.

### Tooltips

Shell controls (nav, top bar) use `data-tip="…"` — CSS-only,
positioned by the shell's styles. Rich content tooltips (fit
modules, market chart points) are surface code (fit.js, the
chart code in app.js) with their own anchoring rules; don't
stretch `data-tip` into one of those.

## Components are Go partials

Reusable UI is a shared template with a documented data shape,
rendered by the server — `templates/fit_workbench.html` is the
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
  (`quickJumpPages`) must carry the same labels —
  `TestQuickJumpPagesMatchNav` fails otherwise.
- **Home widgets:** the module manifest (module_manifest.go) and
  the overview registry (home_overview.go) define each widget's
  id, title, default size and layer. A widget renders through the
  home template by id; its body comes from stored data.

## Server → JS contracts

- Data reaches JS as `data-*` attributes for small facts and as a
  JSON payload for structured state (the planner). The contract
  lives on the Go DTO: invariants are written there (planner
  payload lists are never null — they are coerced in
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
  scripts in templates — CSP serves scripts from 'self' plus a
  per-request nonce, and no template uses one; the single
  inline pattern is a `<script type="application/json">` data
  block (the planner's `plan-data`), which is data, not code.
- **A JS widget:** a banner section in the surface's file (the
  `// --- Name ---` banners are the table of contents), an
  `onReady` subscription scoped to `root`, teardown on
  will-swap for anything global, and a node test in
  `internal/app/jstest/` if it carries logic worth pinning —
  the workflow's JS step names its files explicitly, so add new
  test files to `.github/workflows/release.yml` too.
- **A home widget:** manifest entry, registry entry, template
  block keyed by widget id, data from a stored source. Follow an
  existing widget end to end; the trade-ideas module is a recent
  complete example.

## Testing

Mirror CI exactly before pushing: `make assets`, `gofmt -l cmd
internal`, `go vet ./...`, `go test ./...`, and the node list
from the workflow. Tests ship in the same commit as the behavior
change — every historical CI failure here was tests lagging the
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
