package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
	"evesynapse/internal/markethistory"
)

// ---------------------------------------------------------------------------
// Phase 1B: the signed-in Home is no longer a single character
// sheet — it is the account overview: a customizable set of
// widgets that read across every linked character. Every widget
// renders cache-only from worker-warmed snapshots (the same rule
// as every other page); the old home's live identity fetch is
// gone — identity now comes from the profile snapshot the worker
// keeps warm (esi.SnapProfile), and the character sheet itself
// lives at /character/.
//
// Layout: which widgets are on, in what order, with a per-user
// span preference for flexible ones, persisted per account as a
// JSON array on the user record (schema 010; v2 object entries
// since the grid engine — v1 id arrays still load). Unknown ids
// are ignored on load, so a layout saved by a newer build never
// breaks an older binary.
// ---------------------------------------------------------------------------

// Widget ids. These strings are persisted in users.home_layout;
// never rename one.
const (
	widgetBriefing  = "briefing"
	widgetFleet     = "fleet"
	widgetAttention = "attention"
	widgetNetWorth  = "networth"
	widgetIndustry  = "industry"
	widgetMarket    = "market"
	widgetWatchlist = "watchlist"
	widgetSkills    = "skills"
	widgetServer    = "server"
	widgetPI        = "pi"
)

// Widget size classes for the home grid engine. A full module
// always owns its row; a flex module takes half a row and pairs
// with the next flex module that fits (see solveHomeSpans).
const (
	widgetSizeFull = "full"
	widgetSizeFlex = "flex"
)

// Per-user span preferences for flex modules, persisted in the
// layout: auto pairs the module with a neighbor when the solver
// can; wide forces it onto its own row.
const (
	spanAuto = ""
	spanWide = "wide"
)

// widgetDef is one entry of the widget catalog: what Customize
// offers and what a saved layout may name.
type widgetDef struct {
	ID          string
	Title       string
	Description string
	Size        string // widgetSizeFull | widgetSizeFlex
}

var homeWidgetCatalog = []widgetDef{
	{widgetBriefing, "Briefing", "What changed since you last looked, and what needs you in the next day.", widgetSizeFlex},
	{widgetFleet, "Fleet overview", "Every linked character at a glance: where they are, what they're flying, what they're training.", widgetSizeFull},
	{widgetAttention, "Needs attention", "Characters that need you: re-links, idle queues, finished jobs, expiring orders, waiting contracts.", widgetSizeFull},
	{widgetNetWorth, "Net worth", "Wallets, assets and open-order escrow across all characters, at market prices. An estimate.", widgetSizeFlex},
	{widgetIndustry, "Industry", "Active industry jobs across characters, soonest delivery first.", widgetSizeFlex},
	{widgetPI, "Planetary industry", "Colonies across your characters: extractor timers, expired heads, and the next planet needing a visit.", widgetSizeFlex},
	{widgetMarket, "Market", "Open orders across characters: counts, sell/buy value, orders expiring soonest.", widgetSizeFlex},
	{widgetWatchlist, "Market watchlist", "The items you're watching: latest prices, 7- and 30-day moves, and a flag when one crosses your alert line.", widgetSizeFlex},
	{widgetSkills, "Skills", "The next skill finishes across the fleet, plus who isn't training.", widgetSizeFlex},
	{widgetServer, "Tranquility", "Server status: players online.", widgetSizeFlex},
}

// defaultHomeLayout is what accounts with no saved layout get:
// the briefing first, then the fleet, what needs the user next,
// then the money. Accounts with a saved layout keep theirs —
// the briefing only joins a home by the user's own arrangement.
var defaultHomeLayout = []string{
	widgetBriefing, widgetFleet, widgetAttention, widgetNetWorth,
	widgetIndustry, widgetMarket, widgetServer,
}

func widgetDefFor(id string) (widgetDef, bool) {
	for _, def := range homeWidgetCatalog {
		if def.ID == id {
			return def, true
		}
	}
	return widgetDef{}, false
}

// widgetSizeOf resolves a widget's catalog size class; unknown
// ids (which a normalized layout cannot contain) read as flex.
func widgetSizeOf(id string) string {
	if def, ok := widgetDefFor(id); ok {
		return def.Size
	}
	return widgetSizeFlex
}

// homeLayoutItem is one persisted layout entry: the widget, plus
// its span preference (flex modules only; ignored for full ones).
type homeLayoutItem struct {
	ID   string `json:"id"`
	Span string `json:"span,omitempty"` // spanAuto ("") | spanWide
}

// parseHomeLayout normalizes a persisted layout: unknown ids
// dropped, duplicates dropped, order preserved. Empty storage
// (or unparseable JSON) yields the default layout; a saved empty
// array is honored — the user turned everything off.
//
// Two storage formats load identically: v1, the bare id array
// (["fleet", ...]) every build up to Phase 2 wrote, and v2, the
// object array ([{"id":"fleet"}, {"id":"market","span":"wide"}])
// written since the grid engine. Each entry is decoded on its
// own, so a corrupted entry drops out instead of taking the
// whole layout down; an invalid span value drops back to auto.
func parseHomeLayout(raw string) []homeLayoutItem {
	if strings.TrimSpace(raw) == "" {
		return defaultHomeLayoutItems()
	}
	var entries []json.RawMessage
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return defaultHomeLayoutItems()
	}
	out := make([]homeLayoutItem, 0, len(entries))
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		item, ok := parseHomeLayoutEntry(entry)
		if !ok || seen[item.ID] {
			continue
		}
		seen[item.ID] = true
		out = append(out, item)
	}
	return out
}

// parseHomeLayoutEntry decodes one layout entry in either format.
func parseHomeLayoutEntry(entry json.RawMessage) (homeLayoutItem, bool) {
	// v1: a bare widget id string.
	var id string
	if err := json.Unmarshal(entry, &id); err == nil {
		if _, ok := widgetDefFor(id); !ok {
			return homeLayoutItem{}, false
		}
		return homeLayoutItem{ID: id}, true
	}
	// v2: {"id": ..., "span": ...}.
	var obj struct {
		ID   string `json:"id"`
		Span string `json:"span"`
	}
	if err := json.Unmarshal(entry, &obj); err != nil {
		return homeLayoutItem{}, false
	}
	if _, ok := widgetDefFor(obj.ID); !ok {
		return homeLayoutItem{}, false
	}
	item := homeLayoutItem{ID: obj.ID}
	if obj.Span == spanWide {
		item.Span = spanWide
	}
	return item, true
}

// encodeHomeLayout serializes a normalized layout for storage —
// always in the v2 object format (span omitted when auto).
func encodeHomeLayout(items []homeLayoutItem) string {
	if items == nil {
		items = []homeLayoutItem{}
	}
	data, err := json.Marshal(items)
	if err != nil {
		return "[]"
	}
	return string(data)
}

// defaultHomeLayoutItems is the default layout in item form.
func defaultHomeLayoutItems() []homeLayoutItem {
	items := make([]homeLayoutItem, len(defaultHomeLayout))
	for i, id := range defaultHomeLayout {
		items[i] = homeLayoutItem{ID: id}
	}
	return items
}

// layoutIDs extracts the widget ids of a layout, in order.
func layoutIDs(items []homeLayoutItem) []string {
	ids := make([]string, len(items))
	for i, item := range items {
		ids[i] = item.ID
	}
	return ids
}

// ---------------------------------------------------------------------------
// The home grid engine: a pure packing solver decides every
// module's column span, so the arrangement is computed — never
// stretched into place by the browser. The Go solver renders the
// page (correct with zero JavaScript); the app.js mirror
// re-solves live while a module is dragged. Both implement the
// one rule below.
// ---------------------------------------------------------------------------

// solveHomeSpans resolves each layout item's column span on a
// grid of cols columns. Walking the saved order left to right:
//
//   - a full module, and a flex module marked wide, takes the
//     whole row (cols);
//   - a flex module takes half the row (cols/2) and pairs with
//     the next flex module that fits beside it;
//   - a flex module left ALONE in its row — nothing pairable
//     follows before the next full/wide row boundary — stretches
//     to the whole row.
//
// Rows therefore always tile exactly: no stranded gaps and no
// overflow, at any column count.
func solveHomeSpans(items []homeLayoutItem, cols int) []int {
	spans := make([]int, len(items))
	if cols < 2 {
		for i := range spans {
			spans[i] = 1
		}
		return spans
	}
	flex := cols / 2
	rowUsed, rowStart := 0, 0
	// closeRow ends the current row at index next, stretching a
	// lone half-width module in it to the full row.
	closeRow := func(next int) {
		if next-rowStart == 1 && spans[rowStart] < cols {
			spans[rowStart] = cols
		}
		rowStart, rowUsed = next, 0
	}
	for i, item := range items {
		span := flex
		if item.Span == spanWide || widgetSizeOf(item.ID) == widgetSizeFull {
			span = cols
		}
		if rowUsed > 0 && rowUsed+span > cols {
			closeRow(i)
		}
		spans[i] = span
		rowUsed += span
		if rowUsed == cols {
			closeRow(i + 1)
		}
	}
	if rowUsed > 0 {
		closeRow(len(items))
	}
	return spans
}

// spanClass names the CSS class carrying a span solved on the
// 6-column grid: span3 (half row) or span6 (whole row). The CSS
// maps the same two classes onto the phone grids (2 columns,
// then 1), so the server renders one class set for every screen.
func spanClass(span int) string {
	if span > 3 {
		return "span6"
	}
	return "span3"
}

// ---------------------------------------------------------------------------
// Snapshot store: one batched read of the kinds the visible
// widgets need, decoded per character on demand. Rendering never
// calls esi.Get/GetCached — a stale or missing snapshot dims its
// own widget, exactly like every other page.
// ---------------------------------------------------------------------------

var widgetSnapshotKinds = map[string][]string{
	widgetBriefing:  {esi.SnapSkillqueue, esi.SnapSkills, esi.SnapIndustryJobs, esi.SnapContracts, esi.SnapOrders, esi.SnapOrdersHistory, esi.SnapPlanets, esi.SnapMail, esi.SnapMailLabels, esi.SnapCalendar},
	widgetFleet:     {esi.SnapProfile, esi.SnapCorpInfo, esi.SnapLocation, esi.SnapShip, esi.SnapOnline, esi.SnapSkillqueue, esi.SnapWallet, esi.SnapMailLabels},
	widgetAttention: {esi.SnapSkillqueue, esi.SnapIndustryJobs, esi.SnapContracts, esi.SnapOrders, esi.SnapPlanets},
	widgetNetWorth:  {esi.SnapWallet, esi.SnapAssets, esi.SnapOrders},
	widgetIndustry:  {esi.SnapIndustryJobs},
	widgetMarket:    {esi.SnapOrders},
	widgetWatchlist: {}, // watchlist + stored history only; no character snapshots
	widgetSkills:    {esi.SnapSkillqueue},
	widgetServer:    {},
	widgetPI:        {esi.SnapPlanets},
}

// charSnaps is one character's decoded snapshot bundle. Fields
// stay nil/false when the snapshot is absent or undecodable —
// builders treat that as "unknown", never as an error.
type charSnaps struct {
	ch db.Character

	profile  *esi.Character
	corpInfo *esi.Corporation

	location *esi.Location
	ship     *esi.Ship
	online   *esi.Online

	walletKnown bool
	wallet      float64

	queueKnown bool
	queue      esi.Skillqueue

	ordersKnown bool
	orders      []esi.CharOrder

	jobsKnown bool
	jobs      []esi.IndustryJob

	contractsKnown bool
	contracts      []esi.Contract

	assetsKnown bool
	assets      []esi.Asset

	// Phase 2: planetary industry (colonies list + whichever
	// per-planet layouts have warmed) and the mail label set
	// (the fleet's unread badge reads the total from it).
	planetsKnown bool
	colonies     []esi.Colony
	layouts      map[int64]*esi.PlanetLayout

	mailLabels *esi.MailLabels

	// Phase 6 (Briefing): trained skills (industry slot math),
	// mail headers (newest unread sender), calendar summaries,
	// and the closed-orders history (expired-order events).
	skillsKnown bool
	skills      esi.Skills

	mailKnown bool
	mail      esi.MailHeaders

	calendarKnown bool
	calendar      esi.CalendarEventSummaries

	orderHistKnown bool
	orderHist      esi.CharOrderHistory

	// fetched records when each snapshot was fetched, so widgets
	// can date their data ("as of").
	fetched map[string]time.Time
}

// loadCharSnaps reads the snapshots of every linked character in
// one query (kinds restricted to what the visible widgets need)
// and decodes them into per-character bundles.
func (app *Application) loadCharSnaps(ctx context.Context, userID int64, chars []db.Character, layout []string) []*charSnaps {
	kindSet := map[string]bool{}
	for _, id := range layout {
		for _, kind := range widgetSnapshotKinds[id] {
			kindSet[kind] = true
		}
	}
	kinds := make([]string, 0, len(kindSet))
	for kind := range kindSet {
		kinds = append(kinds, kind)
	}

	bundles := make(map[int64]*charSnaps, len(chars))
	out := make([]*charSnaps, 0, len(chars))
	for _, ch := range chars {
		b := &charSnaps{ch: ch}
		bundles[ch.CharacterID] = b
		out = append(out, b)
	}
	if len(kinds) == 0 {
		return out
	}

	rows, err := app.queries.ListSnapshotsForUser(ctx, db.ListSnapshotsForUserParams{
		UserID: userID,
		Kinds:  kinds,
	})
	if err != nil {
		// A store failure degrades the whole overview to
		// character shells (name/tags) plus warming notes,
		// never to an error page.
		logging.Errorf("home: list snapshots for user %d: %v", userID, err)
		return out
	}
	for _, row := range rows {
		b := bundles[row.CharacterID]
		if b == nil {
			continue
		}
		if b.fetched == nil {
			b.fetched = map[string]time.Time{}
		}
		b.fetched[row.Kind] = row.FetchedAt
		b.decode(row.Kind, row.Payload)
	}

	// Per-planet layouts ride suffix-keyed snapshots, so they
	// cannot join the batched kind list above: load them in a
	// second pass when a visible widget (attention, PI) reads
	// them.
	needLayouts := false
	for _, id := range layout {
		if id == widgetAttention || id == widgetPI || id == widgetBriefing {
			needLayouts = true
			break
		}
	}
	if needLayouts {
		layoutRows, err := app.queries.ListPlanetLayoutsForUser(ctx, userID)
		if err != nil {
			logging.Errorf("home: list planet layouts for user %d: %v", userID, err)
		} else {
			for _, row := range layoutRows {
				b := bundles[row.CharacterID]
				if b == nil {
					continue
				}
				planetID, ok := esi.PlanetLayoutPlanetID(row.Kind)
				if !ok {
					continue
				}
				var v esi.PlanetLayout
				if json.Unmarshal([]byte(row.Payload), &v) != nil {
					continue
				}
				if b.layouts == nil {
					b.layouts = map[int64]*esi.PlanetLayout{}
				}
				b.layouts[planetID] = &v
			}
		}
	}
	return out
}

func (b *charSnaps) decode(kind, payload string) {
	switch kind {
	case esi.SnapProfile:
		var v esi.Character
		if json.Unmarshal([]byte(payload), &v) == nil {
			b.profile = &v
		}
	case esi.SnapCorpInfo:
		var v esi.Corporation
		if json.Unmarshal([]byte(payload), &v) == nil {
			b.corpInfo = &v
		}
	case esi.SnapLocation:
		var v esi.Location
		if json.Unmarshal([]byte(payload), &v) == nil {
			b.location = &v
		}
	case esi.SnapShip:
		var v esi.Ship
		if json.Unmarshal([]byte(payload), &v) == nil {
			b.ship = &v
		}
	case esi.SnapOnline:
		var v esi.Online
		if json.Unmarshal([]byte(payload), &v) == nil {
			b.online = &v
		}
	case esi.SnapWallet:
		var v float64
		if json.Unmarshal([]byte(payload), &v) == nil {
			b.walletKnown, b.wallet = true, v
		}
	case esi.SnapSkillqueue:
		var v esi.Skillqueue
		if json.Unmarshal([]byte(payload), &v) == nil {
			b.queueKnown, b.queue = true, v
		}
	case esi.SnapOrders:
		var v []esi.CharOrder
		if json.Unmarshal([]byte(payload), &v) == nil {
			b.ordersKnown, b.orders = true, v
		}
	case esi.SnapIndustryJobs:
		var v []esi.IndustryJob
		if json.Unmarshal([]byte(payload), &v) == nil {
			b.jobsKnown, b.jobs = true, v
		}
	case esi.SnapContracts:
		var v []esi.Contract
		if json.Unmarshal([]byte(payload), &v) == nil {
			b.contractsKnown, b.contracts = true, v
		}
	case esi.SnapAssets:
		var v []esi.Asset
		if json.Unmarshal([]byte(payload), &v) == nil {
			b.assetsKnown, b.assets = true, v
		}
	case esi.SnapPlanets:
		var v []esi.Colony
		if json.Unmarshal([]byte(payload), &v) == nil {
			b.planetsKnown, b.colonies = true, v
		}
	case esi.SnapMailLabels:
		var v esi.MailLabels
		if json.Unmarshal([]byte(payload), &v) == nil {
			b.mailLabels = &v
		}
	case esi.SnapSkills:
		var v esi.Skills
		if json.Unmarshal([]byte(payload), &v) == nil {
			b.skillsKnown, b.skills = true, v
		}
	case esi.SnapMail:
		var v esi.MailHeaders
		if json.Unmarshal([]byte(payload), &v) == nil {
			b.mailKnown, b.mail = true, v
		}
	case esi.SnapCalendar:
		var v esi.CalendarEventSummaries
		if json.Unmarshal([]byte(payload), &v) == nil {
			b.calendarKnown, b.calendar = true, v
		}
	case esi.SnapOrdersHistory:
		var v esi.CharOrderHistory
		if json.Unmarshal([]byte(payload), &v) == nil {
			b.orderHistKnown, b.orderHist = true, v
		}
	}
}

// corpName resolves the character's corporation display name:
// profile's corporation id named by the character's own warmed
// corp_info snapshot, else the honest id fallback.
func (b *charSnaps) corpName() string {
	if b.profile != nil && b.profile.CorporationID > 0 {
		if b.corpInfo != nil && b.corpInfo.Name != "" {
			return b.corpInfo.Name
		}
		return fmt.Sprintf("Corporation #%d", b.profile.CorporationID)
	}
	return ""
}

func (b *charSnaps) corpID() int64 {
	if b.profile != nil {
		return b.profile.CorporationID
	}
	return 0
}

// currentEntry returns the queue's head entry (position 0) — the
// skill training right now — or nil.
func queueHead(queue esi.Skillqueue) *esi.SkillqueueEntry {
	for i := range queue {
		if queue[i].QueuePosition == 0 {
			return &queue[i]
		}
	}
	return nil
}

// queueState summarizes a character's training: known is false
// until the skillqueue snapshot exists; training is true when a
// future-dated entry remains; finishedAt is the last finish when
// everything in the queue already completed.
func queueState(queue esi.Skillqueue, now time.Time) (training bool, finishedAt time.Time) {
	for _, e := range queue {
		if t, err := time.Parse(time.RFC3339, e.FinishDate); err == nil {
			if t.After(now) {
				training = true
			}
			if t.After(finishedAt) {
				finishedAt = t
			}
		}
	}
	return training, finishedAt
}

// ---------------------------------------------------------------------------
// Widget view models.
// ---------------------------------------------------------------------------

type homeWidget struct {
	ID        string
	Title     string
	SpanClass string // solved grid span class: span3 | span6
	Size      string // catalog size class (flex modules get the resize toggle)
	SpanPref  string // user's span preference: "" (auto) | wide
	Customize bool   // rendering under /?customize=1 (form return targets)
	Fleet     *fleetWidget
	Attention *attentionWidget
	Briefing  *briefingWidget
	NetWorth  *netWorthWidget
	Industry  *industryWidget
	Market    *marketWidget
	Watchlist *watchlistWidget
	Skills    *skillsWidget
	Server    *serverStatusView
	PI        *piWidget
}

// homeView is the signed-in Home body (pageData.Home).
type homeView struct {
	Widgets    []homeWidget
	HasChars   bool
	Customize  *customizeView // non-nil in customize mode
	LoadFailed bool           // character list unreadable
}

// fleetRow is one line of the fleet overview.
type fleetRow struct {
	ID             int64
	Name           string
	PortraitURL    string
	CorpID         int64
	CorpName       string
	Tags           string
	SystemName     string
	SystemRef      placeRef // SystemName classified for the link policy
	DockedName     string
	DockedRef      placeRef // DockedName classified for the link policy
	ShipTypeName   string
	ShipTypeID     int64
	ShipName       string
	OnlineKnown    bool
	Online         bool
	LastLogin      string
	QueueKnown     bool
	NotTraining    bool
	Training       string // "Skill V", "" when idle
	TrainingFinish string // RFC3339 for the live countdown
	TrainingLeft   string
	WalletKnown    bool
	ISK            string
	UnreadKnown    bool  // mail label set has landed
	Unread         int64 // total unread mail (label set total)
	Relink         bool
	Search         string // lowercase filter haystack (name/tags/corp/system)
}

type fleetWidget struct {
	Rows []fleetRow
	Tags []string // every tag in use, for the filter chips
}

func splitTags(raw string) []string {
	return strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == ';'
	})
}

// attentionItem is one Needs-attention line. Rank orders the
// severities (fixed rule order, most actionable first); At breaks
// ties chronologically.
type attentionItem struct {
	Char string // character the item is about
	Text string
	Link string
	When string // short human timing, may be ""
	Rank int
	At   time.Time
}

type attentionWidget struct {
	Items []attentionItem
	More  int // items hidden past the cap
}

const attentionCap = 12

type netWorthWidget struct {
	Any             bool
	Total           string
	Wallet          string
	Assets          string
	AssetsKnown     bool   // an assets snapshot exists (the value is always real then)
	AssetsNote      string // partial-pricing coverage line, "" when everything priced
	Escrow          string
	AsOf            string
	History         *balanceChart // net worth over time, when 2+ sampled days exist
	HistoryBuilding bool          // history recording, not yet chartable
}

type industryRow struct {
	Char     string
	CharID   int64
	Activity string
	Name     string
	TypeID   int64
	Ends     string
	Left     string
}

type industryWidget struct {
	Rows       []industryRow
	ActiveJobs int
	ReadyJobs  int
}

type marketExpiry struct {
	Char    string
	CharID  int64
	Item    string
	TypeID  int64
	Expires string
	Left    string
}

// marketScopeOption is one choice of the orders widget's scope
// picker (All characters / one character / one tag).
type marketScopeOption struct {
	Value    string // "all" | "char:<id>" | "tag:<name>"
	Label    string
	Selected bool
}

// marketCharRow is one per-character breakdown row of the
// orders widget (merge mode "per-character", or one half of
// "both" when the expiring table isn't the breakdown).
type marketCharRow struct {
	Char     string
	CharID   int64
	Open     int
	Sell     string // "3 sell · 1,234,567 ISK", "" when none
	Buy      string // "1 buy · 234,567 ISK in escrow", "" when none
	NoOrders bool   // orders snapshot known, nothing open
	Soonest  string // soonest expiring order: "Tritanium (in 2d 3h)"
}

// watchlistWidget is the Home Market-watchlist module: the same
// rows the Market page computes, at a glance.
type watchlistWidget struct {
	Rows []watchlistRow
}

type marketWidget struct {
	// Scope controls (v0.3.04 widget config).
	ScopeOptions []marketScopeOption
	Merge        string // mergeBoth | mergeCombined | mergePerCharacter (render-resolved)
	ShowMerge    bool   // scope covers >1 character: the merge picker matters
	ScopeEmpty   string // scope selects nobody (unlinked character / unused tag)
	EmptyOrders  string // scoped characters known, nothing open

	AnyData     bool // at least one scoped character has an orders snapshot
	OrdersKnown int  // scoped characters with an orders snapshot
	Open        int
	SellCount   int
	SellValue   string
	BuyCount    int
	BuyValue    string
	Expiring    []marketExpiry
	PerChar     []marketCharRow
	Health      string // Phase 5 one-liner: undercuts + watchlist moves, "" when quiet
}

type skillFinish struct {
	Char    string
	CharID  int64
	Skill   string
	SkillID int64
	Level   string // roman level being finished
	Finish  string
	Left    string
}

type skillsWidget struct {
	Finishing   []skillFinish
	NotTraining int
}

// piExpiryLine is one extractor deadline in the PI widget.
type piExpiryLine struct {
	Char    string
	CharID  int64
	Planet  string
	Expires string
	Left    string // "in 2d 3h", "" once expired
	Expired bool
}

type piWidget struct {
	Any      bool // any colonies data at all
	Colonies int
	Expired  int
	Soonest  []piExpiryLine
}

// orderExpiry computes an order's expiry from ESI's issued +
// duration-days pair.
func orderExpiry(o esi.CharOrder) (time.Time, bool) {
	issued, err := time.Parse(time.RFC3339, o.Issued)
	if err != nil || o.Duration <= 0 {
		return time.Time{}, false
	}
	return issued.Add(time.Duration(o.Duration) * 24 * time.Hour), true
}

// parseRFC3339 parses a snapshot timestamp; ok=false for ""/junk.
func parseRFC3339(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, raw)
	return t, err == nil
}

// ---------------------------------------------------------------------------
// Widget builders. All pure reads over the decoded bundles; the
// only app state they touch is the type-name cache and the
// market-prices cache (both local, both cache-only).
// ---------------------------------------------------------------------------

func (app *Application) buildFleet(ctx context.Context, bundles []*charSnaps) *fleetWidget {
	tagSeen := map[string]bool{}
	var rows []fleetRow
	for _, b := range bundles {
		row := fleetRow{
			ID:          b.ch.CharacterID,
			Name:        b.ch.Name,
			PortraitURL: portraitURL(b.ch.CharacterID, 64),
			CorpID:      b.corpID(),
			CorpName:    b.corpName(),
			Tags:        b.ch.Tags,
			Relink:      b.ch.LinkState != "" && b.ch.LinkState != linkStateOK,
			WalletKnown: b.walletKnown,
		}
		for _, tag := range splitTags(b.ch.Tags) {
			tagSeen[tag] = true
		}
		if b.location != nil {
			row.SystemName = app.locationTitle(ctx, b.location.SolarSystemID, "solar_system")
			row.SystemRef = app.linkPlace(ctx, b.location.SolarSystemID, row.SystemName)
			if b.location.StationID > 0 {
				row.DockedName = app.locationTitle(ctx, b.location.StationID, "station")
				row.DockedRef = app.linkPlace(ctx, b.location.StationID, row.DockedName)
			} else if b.location.StructureID > 0 {
				row.DockedName = app.locationTitle(ctx, b.location.StructureID, "structure")
				row.DockedRef = app.linkPlace(ctx, b.location.StructureID, row.DockedName)
			}
		}
		if b.ship != nil {
			row.ShipTypeName = app.typeNameOrID(ctx, b.ship.ShipTypeID)
			row.ShipTypeID = b.ship.ShipTypeID
			row.ShipName = b.ship.ShipName
		}
		if b.online != nil {
			row.OnlineKnown = true
			row.Online = b.online.Online
			row.LastLogin = formatFinish(b.online.LastLogin)
		}
		if b.queueKnown {
			row.QueueKnown = true
			head := queueHead(b.queue)
			if head == nil {
				row.NotTraining = true
			} else {
				name := app.esi.CachedTypeName(ctx, head.SkillID)
				if name == "" {
					name = fmt.Sprintf("Type #%d", head.SkillID)
				}
				row.Training = fmt.Sprintf("%s %s", name, esi.RomanLevel(head.FinishedLevel))
				if t, ok := parseRFC3339(head.FinishDate); ok {
					row.TrainingFinish = t.UTC().Format(time.RFC3339)
					if t.After(time.Now()) {
						row.TrainingLeft = "in " + humanDuration(time.Until(t))
					} else {
						row.NotTraining = true
					}
				}
			}
		}
		if b.walletKnown {
			row.ISK = esi.FormatISK(b.wallet)
		}
		if b.mailLabels != nil {
			row.UnreadKnown = true
			row.Unread = b.mailLabels.TotalUnreadCount
		}
		row.Search = strings.ToLower(strings.Join([]string{
			row.Name, row.Tags, row.CorpName, row.SystemName,
		}, " "))
		rows = append(rows, row)
	}
	tags := make([]string, 0, len(tagSeen))
	for tag := range tagSeen {
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	return &fleetWidget{Rows: rows, Tags: tags}
}

// Attention rule ranks (lower = surfaces first). Fixed by rule,
// then chronological within a rank.
const (
	attentionRelink = iota
	attentionNotTraining
	attentionJobReady
	attentionOrderExpiring
	attentionContract
	attentionPI // Phase 2: expired/imminent extractors (appended; order preserved)
	attentionUndercut
	attentionMarketMove
)

func (app *Application) buildAttention(ctx context.Context, bundles []*charSnaps) *attentionWidget {
	now := time.Now()
	var items []attentionItem

	// Job/product names resolve in one batched name-cache pass.
	addJobItem := func(b *charSnaps, j esi.IndustryJob) {
		nameID := j.ProductTypeID
		if nameID == 0 {
			nameID = j.BlueprintTypeID
		}
		name := app.typeNameOrID(ctx, nameID)
		at, _ := parseRFC3339(j.CompletedDate)
		if at.IsZero() {
			at, _ = parseRFC3339(j.EndDate)
		}
		items = append(items, attentionItem{
			Char: b.ch.Name,
			Text: fmt.Sprintf("%s — job ready for delivery: %s (%s)", b.ch.Name, name, industryActivityLabel(j.ActivityID)),
			Link: fmt.Sprintf("/industry/?character=%d", b.ch.CharacterID),
			Rank: attentionJobReady,
			At:   at,
		})
	}

	for _, b := range bundles {
		// Re-link needed: the token is dead or the character
		// changed hands; everything else about them is frozen
		// until they sign in again, so this outranks all else.
		if b.ch.LinkState != "" && b.ch.LinkState != linkStateOK {
			reason := "sign-in expired"
			if b.ch.LinkState == linkStateOwnerChanged {
				reason = "character changed accounts"
			}
			items = append(items, attentionItem{
				Char: b.ch.Name,
				Text: fmt.Sprintf("%s needs re-linking — %s.", b.ch.Name, reason),
				Link: "/characters/",
				Rank: attentionRelink,
			})
		}

		// Not training: the queue snapshot exists and nothing in
		// it finishes in the future.
		if b.queueKnown {
			training, finishedAt := queueState(b.queue, now)
			if !training {
				item := attentionItem{
					Char: b.ch.Name,
					Text: fmt.Sprintf("%s isn't training — the queue is empty.", b.ch.Name),
					Link: fmt.Sprintf("/character/?character=%d", b.ch.CharacterID),
					Rank: attentionNotTraining,
					At:   finishedAt,
				}
				if !finishedAt.IsZero() {
					item.Text = fmt.Sprintf("%s isn't training — the queue finished %s.", b.ch.Name, formatFinish(b.queueLastFinish()))
					item.When = "finished " + finishedAt.UTC().Format("2006-01-02")
				}
				items = append(items, item)
			}
		}

		// Industry jobs ready for delivery.
		if b.jobsKnown {
			for _, j := range b.jobs {
				if j.Status == "ready" {
					addJobItem(b, j)
				}
			}
		}

		// Orders expiring within 24h.
		if b.ordersKnown {
			for _, o := range b.orders {
				expiry, ok := orderExpiry(o)
				if !ok || expiry.Before(now) || expiry.After(now.Add(24*time.Hour)) {
					continue
				}
				items = append(items, attentionItem{
					Char: b.ch.Name,
					Text: fmt.Sprintf("%s — %s order expires in %s.", b.ch.Name,
						app.typeNameOrID(ctx, o.TypeID), humanDuration(time.Until(expiry))),
					Link: fmt.Sprintf("/orders/?character=%d", b.ch.CharacterID),
					When: "expires " + expiry.UTC().Format("2006-01-02 15:04 UTC"),
					Rank: attentionOrderExpiring,
					At:   expiry,
				})
			}
		}

		// Contracts waiting on this character to accept/act.
		if b.contractsKnown {
			for _, c := range b.contracts {
				if c.Status != "outstanding" || c.AssigneeID != b.ch.CharacterID || c.AcceptorID != 0 {
					continue
				}
				label := c.Title
				if label == "" {
					label = c.Type + " contract"
				}
				at, _ := parseRFC3339(c.DateExpired)
				items = append(items, attentionItem{
					Char: b.ch.Name,
					Text: fmt.Sprintf("%s — contract waiting for you: %s.", b.ch.Name, label),
					Link: fmt.Sprintf("/contracts/?character=%d", b.ch.CharacterID),
					Rank: attentionContract,
					At:   at,
				})
			}
		}

		// Phase 2: extractors expired or running dry within a
		// day, from the warmed colony layouts (expiry fixed at
		// install time, so the countdown is real).
		if b.planetsKnown {
			for _, colony := range b.colonies {
				layout := b.layouts[colony.PlanetID]
				if layout == nil {
					continue
				}
				planet := app.planetDisplayName(ctx, colony.PlanetID)
				for _, ex := range layoutExtractors(*layout) {
					if !ex.ExpiryOK {
						continue
					}
					switch {
					case ex.Expiry.Before(now):
						items = append(items, attentionItem{
							Char: b.ch.Name,
							Text: fmt.Sprintf("%s — extractor on %s has expired.", b.ch.Name, planet),
							Link: fmt.Sprintf("/planets/?character=%d", b.ch.CharacterID),
							When: "expired " + formatFinish(ex.Expiry.UTC().Format(time.RFC3339)),
							Rank: attentionPI,
							At:   ex.Expiry,
						})
					case ex.Expiry.Before(now.Add(24 * time.Hour)):
						items = append(items, attentionItem{
							Char: b.ch.Name,
							Text: fmt.Sprintf("%s — extractor on %s runs dry in %s.", b.ch.Name, planet, humanDuration(time.Until(ex.Expiry))),
							Link: fmt.Sprintf("/planets/?character=%d", b.ch.CharacterID),
							When: "expires " + formatFinish(ex.Expiry.UTC().Format(time.RFC3339)),
							Rank: attentionPI,
							At:   ex.Expiry,
						})
					}
				}
			}
		}
	}

	// Phase 5: market health — undercut sell orders and
	// watchlist moves, from the worker's stored verdicts and
	// price history. Account-level (not per bundle), appended
	// after the extractor rules.
	items = append(items, app.attentionMarketItems(ctx, bundles)...)

	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Rank != items[j].Rank {
			return items[i].Rank < items[j].Rank
		}
		return items[i].At.Before(items[j].At)
	})
	w := &attentionWidget{}
	if len(items) > attentionCap {
		w.More = len(items) - attentionCap
		items = items[:attentionCap]
	}
	w.Items = items
	return w
}

// attentionMarketItems builds the Phase 5 Needs-attention lines
// for one account: undercut sell orders (one line each, folded
// into a single summary past three) and watchlist moves past the
// user's threshold. Everything reads the stored verdicts and
// history — a quiet market adds nothing.
func (app *Application) attentionMarketItems(ctx context.Context, bundles []*charSnaps) []attentionItem {
	if len(bundles) == 0 {
		return nil
	}
	userID := bundles[0].ch.UserID
	charNames := make(map[int64]string, len(bundles))
	for _, b := range bundles {
		charNames[b.ch.CharacterID] = b.ch.Name
	}

	var items []attentionItem
	health, err := app.queries.ListOrderHealthByUser(ctx, userID)
	if err != nil {
		logging.Errorf("home: attention: list order health for user %d: %v", userID, err)
	} else {
		var undercut []db.OrderHealth
		for _, h := range health {
			if h.Status == "undercut_station" || h.Status == "undercut_region" {
				undercut = append(undercut, h)
			}
		}
		switch {
		case len(undercut) > 3:
			items = append(items, attentionItem{
				Text: fmt.Sprintf("%d of your sell orders are undercut right now.", len(undercut)),
				Link: "/market/",
				Rank: attentionUndercut,
			})
		default:
			for _, h := range undercut {
				text, _ := orderHealthText(h.MyPrice, h.Status, h.StationBest, h.RegionBest)
				items = append(items, attentionItem{
					Char: charNames[h.CharacterID],
					Text: fmt.Sprintf("%s — %s sell order: %s.",
						charNames[h.CharacterID], app.typeNameOrID(ctx, h.TypeID), lowerFirst(text)),
					Link: "/market/",
					Rank: attentionUndercut,
					At:   h.ComputedAt,
				})
			}
		}
	}

	entries, err := app.queries.ListWatchlistByUser(ctx, userID)
	if err != nil {
		logging.Errorf("home: attention: list watchlist for user %d: %v", userID, err)
		return items
	}
	for _, e := range entries {
		rows := app.recentHistoryRows(ctx, e.RegionID, e.TypeID, markethistory.ChartRows)
		pct, ok := markethistory.ChangePct(rows, 7)
		if !ok || absFloat(pct) < e.ThresholdPct {
			continue
		}
		items = append(items, attentionItem{
			Text: fmt.Sprintf("%s %s over 7 days in %s.",
				app.typeNameOrID(ctx, e.TypeID), markethistory.ChangeDirection(pct),
				app.marketRegionLabel(ctx, e.RegionID)),
			Link: "/market/",
			Rank: attentionMarketMove,
		})
	}
	return items
}

// lowerFirst lowercases a string's first letter ("Undercut…" →
// "undercut…") for embedding mid-sentence.
func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// absFloat is |x| for the attention threshold comparison (keeps
// overview.go free of a math import it otherwise doesn't need).
func absFloat(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// queueLastFinish returns the latest finish_date in the queue, or
// "" when no entry carries one.
func (b *charSnaps) queueLastFinish() string {
	last := ""
	for _, e := range b.queue {
		if e.FinishDate > last {
			last = e.FinishDate
		}
	}
	return last
}

func (app *Application) buildNetWorth(ctx context.Context, userID int64, bundles []*charSnaps) *netWorthWidget {
	var walletSum, assetSum, escrowSum float64
	var walletOK, escrowOK bool
	var assetsSeen bool
	var pricedItems, totalItems int
	var asOf time.Time
	// Valuation prices: the live guide when a Market visit has
	// fetched it, else the worker-stored guide (guide_prices.go)
	// — the card always has prices to work with once the worker
	// has run, instead of waiting on Market activity.
	prices := app.valuationPrices(ctx)
	w := &netWorthWidget{}

	// The estimate is only as fresh as its stalest input.
	noteAsOf := func(b *charSnaps, kind string) {
		if t, ok := b.fetched[kind]; ok {
			if asOf.IsZero() || t.Before(asOf) {
				asOf = t
			}
		}
	}

	for _, b := range bundles {
		if b.walletKnown {
			walletSum += b.wallet
			walletOK = true
			noteAsOf(b, esi.SnapWallet)
		}
		// Assets price off the guide prices. Every stack counts
		// toward coverage: priced when the guide knows the type,
		// skipped honestly when it doesn't (the widget says how
		// much of the estate the number covers).
		if b.assetsKnown {
			assetsSeen = true
			for _, a := range b.assets {
				totalItems++
				if prices == nil {
					continue
				}
				p, ok := prices[a.TypeID]
				if !ok {
					continue
				}
				price := p.AveragePrice
				if price <= 0 {
					price = p.AdjustedPrice
				}
				if price <= 0 {
					continue
				}
				assetSum += float64(a.Quantity) * price
				pricedItems++
			}
			noteAsOf(b, esi.SnapAssets)
		}
		if b.ordersKnown {
			for _, o := range b.orders {
				if o.IsBuyOrder {
					escrowSum += o.Escrow
					escrowOK = true
				}
			}
			if escrowOK {
				noteAsOf(b, esi.SnapOrders)
			}
		}
	}

	total := walletSum + assetSum + escrowSum
	if walletOK || assetsSeen || escrowOK {
		w.Any = true
		w.Total = esi.FormatISK(total)
	}
	if walletOK {
		w.Wallet = esi.FormatISK(walletSum)
	}
	if assetsSeen {
		w.Assets = esi.FormatISK(assetSum)
		w.AssetsKnown = true
		if totalItems > 0 && pricedItems < totalItems {
			w.AssetsNote = fmt.Sprintf("Includes everything we could price — %d of %d items priced.", pricedItems, totalItems)
		}
	}
	if escrowOK {
		w.Escrow = esi.FormatISK(escrowSum)
	}
	if !asOf.IsZero() {
		w.AsOf = formatFinish(asOf.UTC().Format(time.RFC3339))
	}
	// Net worth over time from the daily sampler (schema 019):
	// a chart once two sampled days exist, an honest "building"
	// note before that. Stored rows only — no fetching here.
	app.attachNetWorthHistory(ctx, w, userID)
	return w
}

func (app *Application) buildIndustry(ctx context.Context, bundles []*charSnaps) *industryWidget {
	now := time.Now()
	// Product/blueprint names resolve in one batched pass.
	var ids []int64
	for _, b := range bundles {
		for _, j := range b.jobs {
			if j.ProductTypeID > 0 {
				ids = append(ids, j.ProductTypeID)
			}
			ids = append(ids, j.BlueprintTypeID)
		}
	}
	names := app.esi.CachedTypeNames(ctx, ids)
	nameFor := func(id int64) string {
		if name, ok := names[id]; ok && name != "" {
			return name
		}
		return fmt.Sprintf("Type #%d", id)
	}

	type pending struct {
		row industryRow
		end time.Time
	}
	var pendingRows []pending
	w := &industryWidget{}
	for _, b := range bundles {
		if !b.jobsKnown {
			continue
		}
		for _, j := range b.jobs {
			switch j.Status {
			case "ready":
				w.ReadyJobs++
			case "active", "paused":
				w.ActiveJobs++
				end, ok := parseRFC3339(j.EndDate)
				if !ok {
					continue
				}
				nameID := j.ProductTypeID
				if nameID == 0 {
					nameID = j.BlueprintTypeID
				}
				pendingRows = append(pendingRows, pending{
					row: industryRow{
						Char:     b.ch.Name,
						CharID:   b.ch.CharacterID,
						Activity: industryActivityLabel(j.ActivityID),
						Name:     nameFor(nameID),
						TypeID:   nameID,
						Ends:     formatFinish(j.EndDate),
						Left:     humanDuration(time.Until(end)),
					},
					end: end,
				})
			}
		}
	}
	sort.SliceStable(pendingRows, func(i, j int) bool {
		return pendingRows[i].end.Before(pendingRows[j].end)
	})
	for i, p := range pendingRows {
		if i >= 8 {
			break
		}
		_ = now
		w.Rows = append(w.Rows, p.row)
	}
	return w
}

// buildMarket assembles the orders widget over the characters
// its configuration scopes to (all / one character / one tag),
// rendered per the merge mode: combined totals, per-character
// rows, or both. The scope picker offers every linked character
// and every tag in use; a scope that selects nobody (unlinked
// character, tag no longer carried) renders its quiet line.
func (app *Application) buildMarket(ctx context.Context, bundles []*charSnaps, cfg ordersWidgetConfig) *marketWidget {
	now := time.Now()
	w := &marketWidget{Merge: cfg.Merge}

	// The picker: all characters, each character, each tag.
	selected := cfg.encoded()
	w.ScopeOptions = append(w.ScopeOptions, marketScopeOption{
		Value: scopeAll, Label: "All characters", Selected: selected == scopeAll,
	})
	for _, b := range bundles {
		value := "char:" + strconv.FormatInt(b.ch.CharacterID, 10)
		w.ScopeOptions = append(w.ScopeOptions, marketScopeOption{
			Value: value, Label: b.ch.Name, Selected: selected == value,
		})
	}
	var tagChars []db.Character
	for _, b := range bundles {
		tagChars = append(tagChars, b.ch)
	}
	for _, tag := range userTags(tagChars) {
		value := "tag:" + tag
		w.ScopeOptions = append(w.ScopeOptions, marketScopeOption{
			Value: value, Label: "Tag: " + tag, Selected: selected == value,
		})
	}

	scoped, label := scopeBundles(bundles, cfg)
	w.ShowMerge = len(scoped) > 1
	if !w.ShowMerge {
		// One character (or none) has nothing to merge: the
		// totals-plus-rows reading always applies.
		w.Merge = mergeBoth
	}
	if len(scoped) == 0 {
		switch cfg.ScopeType {
		case scopeCharacter:
			w.ScopeEmpty = "That character isn't linked anymore."
		case scopeTag:
			w.ScopeEmpty = "No characters carry that tag right now."
		default:
			w.ScopeEmpty = "No characters to show orders for yet."
		}
		return w
	}

	var sellSum, buySum float64
	type expiryRow struct {
		row marketExpiry
		at  time.Time
	}
	var expiring []expiryRow
	for _, b := range scoped {
		if !b.ordersKnown {
			continue
		}
		w.OrdersKnown++
		row := marketCharRow{Char: b.ch.Name, CharID: b.ch.CharacterID, NoOrders: true}
		var cSell, cBuy int
		var cSellSum, cBuySum float64
		var soonest *expiryRow
		for _, o := range b.orders {
			w.Open++
			row.Open++
			row.NoOrders = false
			if o.IsBuyOrder {
				w.BuyCount++
				buySum += o.Escrow
				cBuy++
				cBuySum += o.Escrow
			} else {
				w.SellCount++
				sellSum += o.Price * float64(o.VolumeRemain)
				cSell++
				cSellSum += o.Price * float64(o.VolumeRemain)
			}
			if expiry, ok := orderExpiry(o); ok && expiry.After(now) {
				er := expiryRow{
					row: marketExpiry{
						Char:    b.ch.Name,
						CharID:  b.ch.CharacterID,
						Item:    app.typeNameOrID(ctx, o.TypeID),
						TypeID:  o.TypeID,
						Expires: formatFinish(expiry.UTC().Format(time.RFC3339)),
						Left:    humanDuration(time.Until(expiry)),
					},
					at: expiry,
				}
				expiring = append(expiring, er)
				if soonest == nil || er.at.Before(soonest.at) {
					cp := er
					soonest = &cp
				}
			}
		}
		if w.Merge == mergePerCharacter {
			if cSell > 0 {
				row.Sell = fmt.Sprintf("%d sell · %s ISK", cSell, esi.FormatISK(cSellSum))
			}
			if cBuy > 0 {
				row.Buy = fmt.Sprintf("%d buy · %s ISK in escrow", cBuy, esi.FormatISK(cBuySum))
			}
			if soonest != nil {
				row.Soonest = fmt.Sprintf("%s (in %s)", soonest.row.Item, humanDuration(time.Until(soonest.at)))
			}
			w.PerChar = append(w.PerChar, row)
		}
	}
	w.AnyData = w.OrdersKnown > 0
	sort.SliceStable(expiring, func(i, j int) bool {
		return expiring[i].at.Before(expiring[j].at)
	})
	for i, e := range expiring {
		if i >= 5 {
			break
		}
		w.Expiring = append(w.Expiring, e.row)
	}
	if w.Open > 0 {
		w.SellValue = esi.FormatISK(sellSum)
		w.BuyValue = esi.FormatISK(buySum)
	} else if w.AnyData {
		switch cfg.ScopeType {
		case scopeCharacter:
			w.EmptyOrders = fmt.Sprintf("No open orders for %s right now.", label)
		case scopeTag:
			w.EmptyOrders = fmt.Sprintf("No open orders for your %s characters right now.", label)
		default:
			w.EmptyOrders = "No open orders across your characters."
		}
	}
	// Phase 5 health line: how many sell orders are undercut and
	// how many watched items are moving, from the same stored
	// verdicts the attention feed reads. Quiet when zero. It is
	// an account-wide summary, so it only reads under the
	// all-characters scope it describes.
	if cfg.ScopeType == scopeAll && len(bundles) > 0 {
		w.Health = app.marketHealthLine(ctx, bundles[0].ch.UserID)
	}
	return w
}

// buildWatchlistWidget assembles the Home watchlist module from
// the same stored rows the Market page renders: latest average,
// 7/30-day moves, and the moving flag past the user's line.
func (app *Application) buildWatchlistWidget(ctx context.Context, userID int64) *watchlistWidget {
	view := app.buildWatchlistView(ctx, userID, "")
	if view == nil {
		return &watchlistWidget{}
	}
	return &watchlistWidget{Rows: view.Rows}
}

// marketHealthLine sums the market widget's one-line health
// summary for one account: "2 orders undercut · watchlist: 1
// moving" (either half omitted when quiet).
func (app *Application) marketHealthLine(ctx context.Context, userID int64) string {
	var parts []string
	if userID > 0 {
		if health, err := app.queries.ListOrderHealthByUser(ctx, userID); err == nil {
			undercut := 0
			for _, h := range health {
				if h.Status == "undercut_station" || h.Status == "undercut_region" {
					undercut++
				}
			}
			if undercut > 0 {
				noun := "orders"
				if undercut == 1 {
					noun = "order"
				}
				parts = append(parts, fmt.Sprintf("%d %s undercut", undercut, noun))
			}
		} else {
			logging.Errorf("home: market widget: list order health for user %d: %v", userID, err)
		}
		if entries, err := app.queries.ListWatchlistByUser(ctx, userID); err == nil {
			moving := 0
			for _, e := range entries {
				rows := app.recentHistoryRows(ctx, e.RegionID, e.TypeID, markethistory.ChartRows)
				if pct, ok := markethistory.ChangePct(rows, 7); ok && absFloat(pct) >= e.ThresholdPct {
					moving++
				}
			}
			if moving > 0 {
				parts = append(parts, fmt.Sprintf("watchlist: %d moving", moving))
			}
		} else {
			logging.Errorf("home: market widget: list watchlist for user %d: %v", userID, err)
		}
	}
	return strings.Join(parts, " · ")
}

func (app *Application) buildSkills(ctx context.Context, bundles []*charSnaps) *skillsWidget {
	now := time.Now()
	type pending struct {
		row    skillFinish
		finish time.Time
	}
	var pendingRows []pending
	w := &skillsWidget{}
	for _, b := range bundles {
		if !b.queueKnown {
			continue
		}
		training, _ := queueState(b.queue, now)
		if !training {
			w.NotTraining++
			continue
		}
		head := queueHead(b.queue)
		if head == nil {
			continue
		}
		finish, ok := parseRFC3339(head.FinishDate)
		if !ok {
			continue
		}
		name := app.esi.CachedTypeName(ctx, head.SkillID)
		if name == "" {
			name = fmt.Sprintf("Type #%d", head.SkillID)
		}
		pendingRows = append(pendingRows, pending{
			row: skillFinish{
				Char:    b.ch.Name,
				CharID:  b.ch.CharacterID,
				Skill:   name,
				SkillID: head.SkillID,
				Level:   esi.RomanLevel(head.FinishedLevel),
				Finish:  formatFinish(head.FinishDate),
				Left:    humanDuration(time.Until(finish)),
			},
			finish: finish,
		})
	}
	sort.SliceStable(pendingRows, func(i, j int) bool {
		return pendingRows[i].finish.Before(pendingRows[j].finish)
	})
	for i, p := range pendingRows {
		if i >= 8 {
			break
		}
		w.Finishing = append(w.Finishing, p.row)
	}
	return w
}

// buildPI assembles the planetary-industry widget: colonies
// across the fleet, how many extractor heads have stopped, and
// the soonest extractor deadlines (expired first, most overdue
// leading). Deadlines come from layout expiry fields only —
// fixed at install time, the one PI number ESI keeps honest.
func (app *Application) buildPI(ctx context.Context, bundles []*charSnaps) *piWidget {
	now := time.Now()
	w := &piWidget{}
	type pending struct {
		row piExpiryLine
		at  time.Time
	}
	var pendingRows []pending
	for _, b := range bundles {
		if !b.planetsKnown {
			continue
		}
		w.Any = true
		w.Colonies += len(b.colonies)
		for _, colony := range b.colonies {
			layout := b.layouts[colony.PlanetID]
			if layout == nil {
				continue
			}
			planet := app.planetDisplayName(ctx, colony.PlanetID)
			for _, ex := range layoutExtractors(*layout) {
				if !ex.ExpiryOK {
					continue
				}
				row := piExpiryLine{
					Char:    b.ch.Name,
					CharID:  b.ch.CharacterID,
					Planet:  planet,
					Expires: formatFinish(ex.Expiry.UTC().Format(time.RFC3339)),
				}
				if ex.Expiry.Before(now) {
					row.Expired = true
					w.Expired++
				} else {
					row.Left = "in " + humanDuration(time.Until(ex.Expiry))
				}
				pendingRows = append(pendingRows, pending{row: row, at: ex.Expiry})
			}
		}
	}
	sort.SliceStable(pendingRows, func(i, j int) bool {
		return pendingRows[i].at.Before(pendingRows[j].at)
	})
	for i, p := range pendingRows {
		if i >= 5 {
			break
		}
		w.Soonest = append(w.Soonest, p.row)
	}
	return w
}

// ---------------------------------------------------------------------------
// Home assembly + layout handlers.
// ---------------------------------------------------------------------------

// buildHome assembles the signed-in Home body: the user's layout
// applied to freshly-decoded snapshot bundles. Customize mode
// builds the same widgets (they render live under the arrange
// controls) and adds the catalog state on top.
func (app *Application) buildHome(ctx context.Context, customize bool) *homeView {
	view := &homeView{}
	userID := int64(app.sessions.GetInt(ctx, sessionUserID))
	if userID == 0 {
		// Dev-login sessions carry no user: clean empty state.
		return view
	}

	chars, err := app.queries.ListCharactersByUser(ctx, userID)
	if err != nil {
		logging.Errorf("home: list characters for user %d: %v", userID, err)
		view.LoadFailed = true
		return view
	}
	view.HasChars = len(chars) > 0

	raw, err := app.queries.GetUserHomeLayout(ctx, userID)
	if err != nil {
		raw = ""
	}
	layout := parseHomeLayout(raw)

	if customize {
		view.Customize = buildCustomizeView(layout)
	}
	if !view.HasChars || len(layout) == 0 {
		return view
	}

	bundles := app.loadCharSnaps(ctx, userID, chars, layoutIDs(layout))

	// Phase 6: the briefing's "since you last looked" window,
	// resolved before the widgets build. The anchor only exists
	// once the module has rendered, so a home without it never
	// touches the anchor at all.
	now := time.Now()
	briefingSince := time.Time{}
	briefingIn := false
	for _, item := range layout {
		if item.ID == widgetBriefing {
			briefingIn = true
		}
	}
	if briefingIn {
		briefingSince = app.briefingWindowStart(ctx, userID, now)
	}

	spans := solveHomeSpans(layout, 6)
	for i, item := range layout {
		def, _ := widgetDefFor(item.ID)
		w := homeWidget{
			ID: item.ID, Title: def.Title,
			SpanClass: spanClass(spans[i]), Size: def.Size, SpanPref: item.Span,
			Customize: customize,
		}
		switch item.ID {
		case widgetBriefing:
			w.Briefing = app.buildBriefing(ctx, bundles, briefingSince, now)
		case widgetFleet:
			w.Fleet = app.buildFleet(ctx, bundles)
		case widgetAttention:
			w.Attention = app.buildAttention(ctx, bundles)
		case widgetNetWorth:
			w.NetWorth = app.buildNetWorth(ctx, userID, bundles)
		case widgetIndustry:
			w.Industry = app.buildIndustry(ctx, bundles)
		case widgetMarket:
			w.Market = app.buildMarket(ctx, bundles, app.ordersConfigFor(ctx, userID))
		case widgetWatchlist:
			w.Watchlist = app.buildWatchlistWidget(ctx, userID)
		case widgetSkills:
			w.Skills = app.buildSkills(ctx, bundles)
		case widgetPI:
			w.PI = app.buildPI(ctx, bundles)
		case widgetServer:
			if status, ok := app.loadServerStatus(ctx); ok {
				w.Server = status
			}
		}
		view.Widgets = append(view.Widgets, w)
	}

	// The anchor advances only after a real (non-Customize) home
	// render that included the briefing: looking at the digest
	// is what moves the window. A Customize pass arranges the
	// module; it does not count as having read it.
	if briefingIn && !customize {
		app.advanceBriefingAnchor(ctx, userID, now)
	}
	return view
}

// customizeEntry is one catalog row in Customize mode.
type customizeEntry struct {
	ID          string
	Title       string
	Description string
	Enabled     bool
	Position    int // 1-based among enabled; 0 when disabled
	CanUp       bool
	CanDown     bool
	Span        string // span preference ("wide" or "")
	IsFlex      bool   // flex modules offer the half/full resize control
}

type customizeView struct {
	Entries []customizeEntry
}

func buildCustomizeView(layout []homeLayoutItem) *customizeView {
	position := make(map[string]int, len(layout))
	spans := make(map[string]string, len(layout))
	for i, item := range layout {
		position[item.ID] = i + 1
		spans[item.ID] = item.Span
	}
	view := &customizeView{}
	for _, def := range homeWidgetCatalog {
		entry := customizeEntry{
			ID:          def.ID,
			Title:       def.Title,
			Description: def.Description,
			Enabled:     position[def.ID] > 0,
			Position:    position[def.ID],
			Span:        spans[def.ID],
			IsFlex:      def.Size == widgetSizeFlex,
		}
		if entry.Enabled {
			entry.CanUp = entry.Position > 1
			entry.CanDown = entry.Position < len(layout)
		}
		view.Entries = append(view.Entries, entry)
	}
	return view
}

// saveHomeLayout persists a normalized layout for the user.
func (app *Application) saveHomeLayout(ctx context.Context, userID int64, layout []homeLayoutItem) {
	err := app.queries.SetUserHomeLayout(ctx, db.SetUserHomeLayoutParams{
		HomeLayout: encodeHomeLayout(layout),
		ID:         userID,
	})
	if err != nil {
		logging.Errorf("home: save layout for user %d: %v", userID, err)
	}
}

// handleHomeLayout applies one layout change (POST /home/layout):
// toggle a widget on/off, move one up/down, flip a flex widget's
// span preference (action=span, span=auto|wide), reset to the
// default, or — from a drag on the customize view — accept the
// whole new order at once (action=order, ids comma-joined; span
// preferences ride along from the saved layout). Every control
// is a plain form, so arranging works with no JavaScript; the
// change saves immediately and bounces back to Customize. The
// drag/×/span/add-module enhancements POST with X-Requested-With
// and get a bare 200 instead of the redirect, so the page never
// navigates under the user's fingers.
func (app *Application) handleHomeLayout(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := int64(app.sessions.GetInt(ctx, sessionUserID))
	if userID == 0 {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	raw, err := app.queries.GetUserHomeLayout(ctx, userID)
	if err != nil {
		raw = ""
	}
	layout := parseHomeLayout(raw)

	if err := r.ParseForm(); err == nil {
		action := r.FormValue("action")
		widget := r.FormValue("widget")
		_, known := widgetDefFor(widget)
		switch action {
		case "reset":
			// Store the canonical default rather than "": the
			// intent (default set, auto spans) stays explicit.
			app.saveHomeLayout(ctx, userID, defaultHomeLayoutItems())
		case "toggle":
			if known {
				found := -1
				for i, item := range layout {
					if item.ID == widget {
						found = i
					}
				}
				if found >= 0 {
					layout = append(layout[:found], layout[found+1:]...)
				} else {
					layout = append(layout, homeLayoutItem{ID: widget})
				}
				app.saveHomeLayout(ctx, userID, layout)
			}
		case "up", "down":
			if known {
				for i, item := range layout {
					if item.ID != widget {
						continue
					}
					j := i - 1
					if action == "down" {
						j = i + 1
					}
					if j >= 0 && j < len(layout) {
						layout[i], layout[j] = layout[j], layout[i]
					}
					break
				}
				app.saveHomeLayout(ctx, userID, layout)
			}
		case "order":
			// The whole arrangement, as dragged. Unknown and
			// duplicate ids drop out and the submitted order
			// wins; span preferences carry over from the saved
			// layout (they are orthogonal to order), and an id
			// never seen before starts auto. An empty or
			// all-unknown list turns everything off, exactly
			// like toggling each widget off would.
			prefs := make(map[string]string, len(layout))
			for _, item := range layout {
				prefs[item.ID] = item.Span
			}
			next := make([]homeLayoutItem, 0, len(layout))
			seen := make(map[string]bool, len(layout))
			for _, id := range strings.Split(r.FormValue("ids"), ",") {
				id = strings.TrimSpace(id)
				if _, ok := widgetDefFor(id); !ok || seen[id] {
					continue
				}
				seen[id] = true
				next = append(next, homeLayoutItem{ID: id, Span: prefs[id]})
			}
			app.saveHomeLayout(ctx, userID, next)
		case "span":
			// Flip one widget's span preference; the id must
			// already be on the home and the value exactly
			// auto|wide — anything else is not a layout change.
			if known {
				pref, valid := spanAuto, true
				switch r.FormValue("span") {
				case spanWide:
					pref = spanWide
				case "auto":
					pref = spanAuto
				default:
					valid = false
				}
				if valid {
					for i := range layout {
						if layout[i].ID == widget {
							layout[i].Span = pref
							app.saveHomeLayout(ctx, userID, layout)
							break
						}
					}
				}
			}
		}
	}
	if r.Header.Get("X-Requested-With") == "XMLHttpRequest" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
		return
	}
	http.Redirect(w, r, "/?customize=1", http.StatusSeeOther)
}
