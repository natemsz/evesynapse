package app

import (
	"context"
	"evesynapse/internal/app/module"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	json "github.com/goccy/go-json"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
	"evesynapse/internal/tags"
)

// ---------------------------------------------------------------------------
// The signed-in Home is the account overview: a customizable set of
// widgets that read across every linked character, each rendered
// cache-only from worker-warmed snapshots. The character sheet itself
// is at /character/.
//
// Layout: which widgets are on, in what order, with a per-user span
// preference for flexible ones, stored per account as a JSON array on
// the user record. Unknown ids are ignored on load, so a layout saved
// by a newer build never breaks an older binary.
// ---------------------------------------------------------------------------

// Widget ids. These strings are persisted in users.home_layout;
// never rename one.
const (
	widgetBriefing    = "briefing"
	widgetFleet       = "fleet"
	widgetNetWorth    = "networth"
	widgetIndustry    = "industry"
	widgetProdLine    = "prodline"
	widgetMarket      = "market"
	widgetWatchlist   = "watchlist"
	widgetPI          = "pi"
	widgetTradeIdeas  = "tradeideas"
	widgetTraining    = "training"
	widgetPulse       = "pulse"
	widgetWalletFlow  = "walletflow"
	widgetUpkeep      = "upkeep"
	widgetGlance      = "glance"
	widgetProfitPulse = "profitpulse"
	widgetAgenda      = "agenda"
	widgetFleets      = "fleets"
	widgetKills       = "kills"
	widgetSRP         = "srp"
	widgetClones      = "clones"
	widgetMail        = "mail"
	widgetContracts   = "contracts"
	widgetLP          = "lp"
	widgetMovers      = "movers"
	widgetSov         = "sov"
	widgetLocal       = "local"
	widgetDoctrines   = "doctrines"
	// "server" (the Tranquility module) is retired: the player count
	// and ESI health ride the Overview bar now. A saved layout naming
	// it drops the entry on load; never reuse the id. "attention"
	// (Needs Attention) is retired the same way: every line it
	// listed is a Briefing line too. "skills" is retired as well:
	// Training Horizon at one row high is what it showed.
)

// clampGrid holds a stored size inside the grid; 0 (unset) stays 0.
func clampGrid(n, most int) int {
	switch {
	case n <= 0:
		return 0
	case n > most:
		return most
	}
	return n
}

// defaultHomeLayout is what accounts with no saved layout get, sized
// to tile the three-column grid with no gaps: the headline figures
// on top and the fleet right under them, the briefing as a band,
// then the three things an everyday pilot checks most (skill
// finishes, extractor timers, net worth) side by side at double
// height, and wallet flow at two by two with the industry and
// market checks stacked beside it. Sizes are explicit here rather
// than taken from the declarations, so the default tiles even where
// a widget's default size (what Add a widget gives it) differs.
// Accounts with a saved layout keep theirs.
var defaultHomeLayout = []homeLayoutItem{
	{ID: widgetBriefing, W: 3, H: 1},
	{ID: widgetGlance, W: 3, H: 1},
	{ID: widgetFleet, W: 3, H: 2},
	{ID: widgetAgenda, W: 1, H: 2},
	{ID: widgetNetWorth, W: 1, H: 2},
	{ID: widgetTraining, W: 1, H: 2},
	{ID: widgetWalletFlow, W: 2, H: 2},
	{ID: widgetPI, W: 1, H: 2},
	{ID: widgetIndustry, W: 2, H: 1},
	{ID: widgetMarket, W: 1, H: 1},
}

// widgetSize is the size a layout entry draws at: its stored size
// (the widget's default per unset axis), snapped to one the widget
// declares.
func widgetSize(item homeLayoutItem) (w, h int) {
	def, ok := widgetDefFor(item.ID)
	if !ok {
		return 1, 1
	}
	s := def.sizeOf(item)
	return s.W, s.H
}

// homeLayoutItem is one persisted layout entry: the widget and, once
// the user has resized it, its size in grid cells (0 = the catalog
// default).
type homeLayoutItem struct {
	ID  string `json:"id"`
	Key string `json:"key,omitempty"` // a copy's key, "id#2"; empty for the first card
	W   int    `json:"w,omitempty"`
	H   int    `json:"h,omitempty"`
}

// LayoutKey names the card: its copy key, else the widget id.
func (i homeLayoutItem) LayoutKey() string {
	if i.Key != "" {
		return i.Key
	}
	return i.ID
}

// parseHomeLayout normalizes a persisted layout: unknown ids
// dropped, duplicates dropped, order preserved. Empty storage
// (or unparseable JSON) yields the default layout; a saved empty
// array is honored, the user turned everything off.
//
// Three storage formats load: v1, the bare id array (["fleet", ...]);
// v2, objects with a span preference ({"id":"market","span":"wide"}),
// where wide reads as the full grid width; and v3, objects with a
// size ({"id":"market","w":2,"h":1}), written now. Each entry is
// decoded on its own, so a corrupted entry drops out instead of
// taking the whole layout down; a size out of range is held to it.
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
		if !ok || seen[item.LayoutKey()] {
			continue
		}
		seen[item.LayoutKey()] = true
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
	// v2/v3: {"id": ..., "span": ...} or {"id": ..., "w": .., "h": ..}.
	var obj struct {
		ID   string `json:"id"`
		Key  string `json:"key"`
		Span string `json:"span"`
		W    int    `json:"w"`
		H    int    `json:"h"`
	}
	if err := json.Unmarshal(entry, &obj); err != nil {
		return homeLayoutItem{}, false
	}
	if _, ok := widgetDefFor(obj.ID); !ok {
		return homeLayoutItem{}, false
	}
	item := homeLayoutItem{ID: obj.ID, W: clampGrid(obj.W, homeGridCols), H: clampGrid(obj.H, homeGridRows)}
	if obj.Key != "" && obj.Key != obj.ID {
		// A copy: its key must name a copy of this widget.
		if def, ok := widgetDefForKey(obj.Key); !ok || def.ID != obj.ID {
			return homeLayoutItem{}, false
		}
		item.Key = obj.Key
	}
	if obj.Span == "wide" && item.W == 0 {
		item.W = homeGridCols
	}
	return item, true
}

// encodeHomeLayout serializes a normalized layout for storage,
// always in the v3 object format (size omitted while default).
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
	items := make([]homeLayoutItem, 0, len(defaultHomeLayout))
	for _, item := range defaultHomeLayout {
		// A widget whose module is not built in is not in the catalog.
		if _, ok := widgetDefFor(item.ID); ok {
			items = append(items, item)
		}
	}
	return items
}

// homeCap is how many rows or items a module shows before More, by
// its height: one cell holds a short list, every extra row holds
// five more.
func homeCap(h int) int { return 3 + 5*(h-1) }

// ---------------------------------------------------------------------------
// Snapshot store: one batched read of the kinds the visible
// widgets need, decoded per character on demand. Rendering never
// calls esi.Get/GetCached, a stale or missing snapshot dims its
// own widget, exactly like every other page.
// ---------------------------------------------------------------------------

// charSnaps is one character's decoded snapshot bundle. Fields
// stay nil/false when the snapshot is absent or undecodable,
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

	// Planetary industry (colonies list + whichever
	// per-planet layouts have warmed) and the mail label set
	// (the fleet's unread badge reads the total from it).
	planetsKnown bool
	colonies     []esi.Colony
	layouts      map[int64]*esi.PlanetLayout

	mailLabels *esi.MailLabels

	// Briefing: trained skills (industry slot math),
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

	journalKnown bool
	journal      esi.WalletJournal

	// Clones and Fatigue: jump clones and the home station, the
	// active implants, and jump fatigue.
	clones        *esi.Clones
	implantsKnown bool
	implants      esi.Implants
	fatigue       *esi.Fatigue

	// Loyalty Points: LP held per corporation.
	loyaltyKnown bool
	loyalty      esi.LoyaltyBalances

	// fetched records when each snapshot was fetched, so widgets
	// can date their data ("as of").
	fetched map[string]time.Time
}

// loadCharSnaps reads the snapshots of every linked character in
// one query (kinds restricted to the needs given: the union of the
// visible widgets' Needs) and decodes them into per-character bundles.
func (app *Application) loadCharSnaps(ctx context.Context, userID int64, chars []db.Character, needs []string) []*charSnaps {
	kindSet := map[string]bool{}
	needLayouts := false
	for _, kind := range needs {
		if kind == needPlanetLayouts {
			needLayouts = true
			continue
		}
		kindSet[kind] = true
	}
	kinds := make([]string, 0, len(kindSet))
	for kind := range kindSet {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)

	bundles := make(map[int64]*charSnaps, len(chars))
	out := make([]*charSnaps, 0, len(chars))
	for _, ch := range chars {
		b := &charSnaps{ch: ch}
		bundles[ch.CharacterID] = b
		out = append(out, b)
	}
	if len(kinds) > 0 {
		app.decodeUserSnaps(ctx, userID, kinds, bundles)
	}
	if needLayouts {
		app.loadPlanetLayouts(ctx, userID, bundles)
	}
	return out
}

// decodeUserSnaps reads the account's snapshots of the given kinds in
// one batch and decodes each into its character's bundle.
func (app *Application) decodeUserSnaps(ctx context.Context, userID int64, kinds []string, bundles map[int64]*charSnaps) {
	rows, err := app.queries.ListSnapshotsForUser(ctx, db.ListSnapshotsForUserParams{
		UserID: userID,
		Kinds:  kinds,
	})
	if err != nil {
		// A store failure degrades the whole overview to
		// character shells (name/tags) plus warming notes,
		// never to an error page.
		logging.Errorf("home: list snapshots for user %d: %v", userID, err)
		return
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
}

// loadPlanetLayouts reads the per-planet colony layouts. They ride
// suffix-keyed snapshots, so they cannot join the batched kind list:
// a widget asks for them with needPlanetLayouts.
func (app *Application) loadPlanetLayouts(ctx context.Context, userID int64, bundles map[int64]*charSnaps) {
	layoutRows, err := app.queries.ListPlanetLayoutsForUser(ctx, userID)
	if err != nil {
		logging.Errorf("home: list planet layouts for user %d: %v", userID, err)
		return
	}
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
	case esi.SnapWalletJournal:
		var v esi.WalletJournal
		if json.Unmarshal([]byte(payload), &v) == nil {
			b.journalKnown, b.journal = true, v
		}
	case esi.SnapClones:
		var v esi.Clones
		if json.Unmarshal([]byte(payload), &v) == nil {
			b.clones = &v
		}
	case esi.SnapImplants:
		var v esi.Implants
		if json.Unmarshal([]byte(payload), &v) == nil {
			b.implantsKnown, b.implants = true, v
		}
	case esi.SnapFatigue:
		var v esi.Fatigue
		if json.Unmarshal([]byte(payload), &v) == nil {
			b.fatigue = &v
		}
	case esi.SnapLoyalty:
		var v esi.LoyaltyBalances
		if json.Unmarshal([]byte(payload), &v) == nil {
			b.loyaltyKnown, b.loyalty = true, v
		}
	}
}

// corpName resolves the character's corporation display name:
// profile's corporation id named by the character's own warmed
// corp_info snapshot, else the id fallback.
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

// currentEntry returns the queue's head entry (position 0), the
// skill training right now, or nil.
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

// homeView is the signed-in Home body (pageData.Home).
type homeView struct {
	Widgets    []homeWidget
	HasChars   bool
	Customize  *customizeView // non-nil in customize mode
	LoadFailed bool           // character list unreadable
	// CustomizePrompt is the one-time hint under the Customize
	// button (home_customize_prompt.go); nil once answered, in
	// Customize itself, and for the demo account.
	CustomizePrompt *customizePromptView
	// BriefingSince is the start of the Briefing's window in this
	// render (unix seconds; 0 without a Briefing). The page sends it
	// back with every card it fetches later (pinBriefingSince).
	BriefingSince int64
}

// fleetRow is one line of the fleet overview.
type fleetRow struct {
	ID          int64
	Name        string
	PortraitURL string
	CorpID      int64
	CorpName    string
	Tags        string
	SystemName  string
	SystemRef   placeRef // SystemName classified for the link policy
	// The location system's security badge, from the stored
	// sde_systems row (the fleet read carries it alongside the
	// title): text and tier by the shared secStatus rounding.
	SecKnown       bool
	SecText        string
	SecTier        int
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
	// Idle counts the characters whose queue is known and empty,
	// for the Not-training chip; TotalISK sums the known wallets.
	Idle     int
	TotalISK string
}

func splitTags(raw string) []string { return tags.Split(raw) }

type netWorthWidget struct {
	Any             bool
	Total           string
	Wallet          string
	Assets          string
	AssetsKnown     bool   // an assets snapshot exists (the value is always real then)
	AssetsNote      string // partial-pricing coverage line, "" when everything priced
	Escrow          string
	AsOf            string
	HistoryBuilding bool // history recording, not yet chartable
	// WalletPct is the wallets' share of the total, "48.1%", and
	// WalletBar the same share for a data-w bar; both empty when
	// the total is not positive.
	WalletPct string
	WalletBar string
	// AssetsPct/AssetsBar are the priced share for the Hangar KPI's
	// bar: items priced over items known, from the same stored-read
	// pass that totals the assets.
	AssetsPct string
	AssetsBar string
	// The net-worth sparkline (last 30 days of daily totals), as SVG
	// point strings; both empty under two sampled days.
	SparkPoints string
	SparkArea   string

	// Trends beside the At a Glance figures: each live figure against
	// the daily sample about a week back; nil without an older sample
	// or a change worth showing.
	TotalTrend  *kpiTrend
	WalletTrend *kpiTrend
	AssetsTrend *kpiTrend

	// The live figures the trends compare, and escrow on its own.
	totalNow, walletNow, assetsNow, escrowNow float64
	// pricedItems of totalItems asset stacks have a house price.
	pricedItems, totalItems int
	// days is the last month of sampled days, oldest first; chars is
	// each character's live total, in the bundles' order.
	days  []netWorthDay
	chars []netWorthChar
}

// netWorthChar is one character's live net worth.
type netWorthChar struct {
	ID    int64
	Name  string
	Total float64
}

// kpiTrend is one figure's direction and its change, for the arrow's
// title ("+3.2% over 7 days").
type kpiTrend struct {
	Up    bool
	Delta string // shown beside the arrow: "-1.74B (-1.2%)"
	Text  string
	Since string // the span compared: "7 days"
}

type marketExpiry struct {
	Char    string
	CharID  int64
	Item    string
	TypeID  int64
	Expires string
	Left    string
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

type marketWidget struct {
	Merge       string // mergeBoth | mergeCombined | mergePerCharacter: the show setting, both for one character
	EmptyOrders string // scoped characters known, nothing open

	AnyData     bool // at least one scoped character has an orders snapshot
	OrdersKnown int  // scoped characters with an orders snapshot
	Open        int
	SellCount   int
	SellValue   string
	BuyCount    int
	BuyValue    string
	Expiring    []marketExpiry
	PerChar     []marketCharRow
	Health      string // One-liner: undercuts + watchlist moves, "" when quiet
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
func parseRFC3339(raw string) (time.Time, bool) { return module.ParseRFC3339(raw) }

// ---------------------------------------------------------------------------
// Widget builders. All pure reads over the decoded bundles; the
// only app state they touch is the type-name cache and the
// market-prices cache (both local, both cache-only).
// ---------------------------------------------------------------------------

func (app *Application) buildFleet(ctx context.Context, userID int64, bundles []*charSnaps) *fleetWidget {
	now := time.Now()
	if userID > 0 {
		if cached := app.fleetRollup(userID, now); cached != nil {
			return cached
		}
	}
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
			if sys, serr := app.queries.GetSDESystem(ctx, b.location.SolarSystemID); serr == nil {
				row.SecText, row.SecTier = secRound(sys.Security)
				row.SecKnown = true
			}
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
			head := queueHead(b.queue, time.Now())
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
				} else {
					// A paused queue's entries carry no finish
					// dates, so the head's does not parse: not
					// training, the same verdict queueState gives
					// the briefing on this snapshot, and the row
					// reads as an empty queue's does.
					row.Training = ""
					row.NotTraining = true
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
	w := &fleetWidget{Rows: rows, Tags: tags}
	var isk float64
	anyISK := false
	for _, b := range bundles {
		if b.walletKnown {
			isk += b.wallet
			anyISK = true
		}
	}
	for _, r := range rows {
		if r.QueueKnown && r.NotTraining {
			w.Idle++
		}
	}
	if anyISK {
		w.TotalISK = esi.FormatISK(isk)
	}
	if userID > 0 {
		app.storeFleetRollup(userID, now, w)
	}
	return w
}

// absFloat is |x| for the watchlist threshold comparison (keeps
// home_overview.go free of a math import it otherwise doesn't need).
func absFloat(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// charIDs narrows the stored history to those characters (a card scoped
// to some of them, whose bundles are already narrowed); nil reads the
// whole account.
func (app *Application) buildNetWorth(ctx context.Context, userID int64, bundles []*charSnaps, charIDs []int64) *netWorthWidget {
	var walletSum, assetSum, escrowSum float64
	var walletOK, escrowOK bool
	var assetsSeen bool
	var pricedItems, totalItems int
	var asOf time.Time
	// Valuation prices resolve in the house order
	// (houseValuations, the appraisal tool's order). The guide's
	// average prices nothing.
	values := app.houseValuations(ctx)
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
		var own float64
		if b.walletKnown {
			own += b.wallet
			walletSum += b.wallet
			walletOK = true
			noteAsOf(b, esi.SnapWallet)
		}
		// Assets price off the house order. Every stack counts
		// toward coverage: priced when any tier of the house
		// order knows the type, skipped when none does (the widget says how much of the estate the
		// number covers).
		if b.assetsKnown {
			assetsSeen = true
			for _, a := range b.assets {
				totalItems++
				price, ok := houseValueOf(values, a.TypeID)
				if !ok {
					continue
				}
				assetSum += float64(a.Quantity) * price
				own += float64(a.Quantity) * price
				pricedItems++
			}
			noteAsOf(b, esi.SnapAssets)
		}
		if b.ordersKnown {
			for _, o := range b.orders {
				if o.IsBuyOrder {
					escrowSum += o.Escrow
					own += o.Escrow
					escrowOK = true
				}
			}
			if escrowOK {
				noteAsOf(b, esi.SnapOrders)
			}
		}
		if b.walletKnown || b.assetsKnown || b.ordersKnown {
			w.chars = append(w.chars, netWorthChar{ID: b.ch.CharacterID, Name: b.ch.Name, Total: own})
		}
	}

	total := walletSum + assetSum + escrowSum
	w.totalNow, w.walletNow, w.assetsNow, w.escrowNow = total, walletSum, assetSum+escrowSum, escrowSum
	w.pricedItems, w.totalItems = pricedItems, totalItems
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
			w.AssetsNote = fmt.Sprintf("Includes everything we could price: %d of %d items priced.", pricedItems, totalItems)
		}
	}
	if escrowOK {
		w.Escrow = esi.FormatISK(escrowSum)
	}
	if walletOK && total > 0 {
		pct := 100 * walletSum / total
		w.WalletPct = fmt.Sprintf("%.1f%%", pct)
		w.WalletBar = fmt.Sprintf("%.1f", pct)
	}
	if assetsSeen && totalItems > 0 {
		pct := 100 * float64(pricedItems) / float64(totalItems)
		w.AssetsPct = fmt.Sprintf("%.1f%%", pct)
		w.AssetsBar = fmt.Sprintf("%.1f", pct)
	}
	if !asOf.IsZero() {
		w.AsOf = formatFinish(asOf.UTC().Format(time.RFC3339))
	}
	// Net worth over time from the daily sampler (schema 019):
	// the last month of days, a "building" note before two exist.
	// Stored rows only, no fetching here.
	app.attachNetWorthHistory(ctx, w, userID, charIDs)
	attachNetWorthSpark(w)
	return w
}

// attachNetWorthSpark fills the KPI sparkline from the daily totals the
// history read already holds (w.days, oldest first): no second query.
// Under two sampled days the spark stays empty and the KPI renders no
// line.
func attachNetWorthSpark(w *netWorthWidget) {
	totals := make([]float64, 0, len(w.days))
	for _, d := range w.days {
		totals = append(totals, d.Total())
	}
	if line, area, ok := sparklineSVG(totals); ok {
		w.SparkPoints, w.SparkArea = line, area
	}
}

// buildMarket assembles the orders widget over the characters its
// scope setting covers, read the way its show setting says: combined
// totals, a row per character, or both. One character has nothing to
// combine, so it always reads both ways.
func (app *Application) buildMarket(ctx context.Context, bundles []*charSnaps, show string, scope widgetScope, shared *homeMarketShared) *marketWidget {
	now := time.Now()
	w := &marketWidget{Merge: show}
	if len(bundles) < 2 {
		w.Merge = mergeBoth
	}

	var sellSum, buySum float64
	type expiryRow struct {
		row marketExpiry
		at  time.Time
	}
	var expiring []expiryRow
	for _, b := range bundles {
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
		switch {
		case scope.Char != "":
			w.EmptyOrders = fmt.Sprintf("No open orders for %s right now.", scope.Char)
		case scope.Tag != "":
			w.EmptyOrders = fmt.Sprintf("No open orders for your %s characters right now.", scope.Tag)
		default:
			w.EmptyOrders = "No open orders across your characters."
		}
	}
	// Health line: how many sell orders are undercut and
	// how many watched items are moving, from the same stored
	// verdicts the Briefing reads. Quiet when zero. It is
	// an account-wide summary, so it only reads under the
	// all-characters scope it describes.
	if scope.All() && len(bundles) > 0 {
		w.Health = app.marketHealthLine(ctx, shared)
	}
	return w
}

// marketHealthLine sums the market widget's one-line health
// summary for one account: "2 orders undercut · watchlist: 1
// moving" (either half omitted when quiet). The rows are the
// render's shared order-health and watchlist fetches, so the
// line always agrees with the Briefing and the watchlist widget
// built beside it.
func (app *Application) marketHealthLine(ctx context.Context, shared *homeMarketShared) string {
	health, entries := shared.orderHealth(ctx), shared.watchlist(ctx)
	var parts []string
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
	moving := 0
	for _, e := range entries {
		if m := shared.watchMove(ctx, e); m.Has7 && absFloat(m.Chg7) >= e.ThresholdPct {
			moving++
		}
	}
	if moving > 0 {
		parts = append(parts, fmt.Sprintf("watchlist: %d moving", moving))
	}
	return strings.Join(parts, " · ")
}

// ---------------------------------------------------------------------------
// Home assembly + layout handlers.
// ---------------------------------------------------------------------------

// buildHome assembles the signed-in Home body: the user's layout
// applied to freshly-decoded snapshot bundles. Customize mode
// builds the same widgets (they render live under the arrange
// controls) and adds the catalog state on top.
// homeMarketShared carries one home render's order-health and
// watchlist rows for the Briefing, the market widget and the
// watchlist widget: each table is fetched at most once, on first
// use, and a failed fetch reads as no rows for every consumer
// alike.
type homeMarketShared struct {
	app       *Application
	userID    int64
	health    []db.OrderHealth
	healthSet bool
	watch     []db.MarketWatchlist
	watchSet  bool
	moves     map[[2]int64]watchMove
}

// watchMove is one watchlist entry's stored price move, read at
// most once per render: the Briefing, the health line and the
// watchlist widget all judge "moving" on the same figures.
func (s *homeMarketShared) watchMove(ctx context.Context, e db.MarketWatchlist) watchMove {
	key := [2]int64{e.TypeID, e.RegionID}
	if m, ok := s.moves[key]; ok {
		return m
	}
	m := s.app.watchMove(ctx, e.TypeID, marketRegionForType(e.TypeID, e.RegionID))
	if s.moves == nil {
		s.moves = map[[2]int64]watchMove{}
	}
	s.moves[key] = m
	return m
}

func (s *homeMarketShared) orderHealth(ctx context.Context) []db.OrderHealth {
	if !s.healthSet {
		s.healthSet = true
		rows, err := s.app.queries.ListOrderHealthByUser(ctx, s.userID)
		if err != nil {
			logging.Errorf("home: list order health for user %d: %v", s.userID, err)
		} else {
			s.health = rows
		}
	}
	return s.health
}

func (s *homeMarketShared) watchlist(ctx context.Context) []db.MarketWatchlist {
	if !s.watchSet {
		s.watchSet = true
		rows, err := s.app.queries.ListWatchlistByUser(ctx, s.userID)
		if err != nil {
			logging.Errorf("home: list watchlist for user %d: %v", s.userID, err)
		} else {
			s.watch = rows
		}
	}
	return s.watch
}

// effectiveHomeLayout is the layout a home render or a layout
// change works from: the account's own preset when nothing is
// saved, the saved layout otherwise. A saved empty array stays
// empty.
func (app *Application) effectiveHomeLayout(ctx context.Context, userID int64, raw string) []homeLayoutItem {
	var layout []homeLayoutItem
	if strings.TrimSpace(raw) == "" {
		layout = app.defaultLayoutFor(ctx, userID)
	} else {
		layout = parseHomeLayout(raw)
	}
	// The demo account shows the app's surfaces, not a digest of
	// demo-seed news: its briefing notification cards are out,
	// whether the layout came from the demo preset or a saved one.
	if app.isDemoUser(ctx, userID) {
		out := layout[:0]
		for _, item := range layout {
			if item.ID != widgetBriefing {
				out = append(out, item)
			}
		}
		layout = out
	}
	return layout
}

func (app *Application) buildHome(ctx context.Context, customize bool) *homeView {
	return app.buildHomeView(ctx, customize, !customize, "")
}

// buildHomeView builds the signed-in home. advance says whether this
// render counts as the reader looking at the Briefing: a full home
// render does; Customize and a live refresh of the cards do not.
// since, when set, is the Briefing window the page was drawn with
// (pinBriefingSince).
func (app *Application) buildHomeView(ctx context.Context, customize, advance bool, since string) *homeView {
	view := &homeView{}
	userID := app.userID(ctx)
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
	layout := app.effectiveHomeLayout(ctx, userID, raw)

	if customize {
		view.Customize = buildCustomizeView(layout)
		// Opening Customize answers the hint: the reader has
		// found the thing it points at.
		app.markCustomizePromptSeen(ctx, userID)
	} else if view.HasChars {
		view.CustomizePrompt = app.customizePromptFor(ctx, userID)
	}
	if !view.HasChars || len(layout) == 0 {
		return view
	}

	// One batched snapshot read: the union of the visible widgets'
	// needs.
	var needs []string
	defs := make([]*homeWidgetDef, len(layout))
	for i, item := range layout {
		if def, ok := widgetDefFor(item.ID); ok {
			defs[i] = def
			needs = append(needs, def.Needs...)
		}
	}
	shared := newHomeShared(ctx, app, userID, chars, needs)
	if since != "" {
		shared.pinBriefingSince(since)
	}
	viewer := viewerChars(chars)
	if view.Customize != nil {
		for i := range view.Customize.Entries {
			e := &view.Customize.Entries[i]
			if def, ok := widgetDefFor(e.ID); ok && e.Enabled {
				opts := def.optionsFrom(shared.Config(ctx, def.ID), chars)
				e.Settings = def.settingsView(def.ID, opts, chars)
				shared.fillCorpChoices(ctx, e.Settings, opts)
			}
		}
	}
	ts, err := homeTemplateSet()
	if err != nil {
		logging.Errorf("home: parse templates: %v", err)
	}

	briefingIn := false
	for i, item := range layout {
		def := defs[i]
		if def == nil {
			continue
		}
		if def.ID == widgetBriefing {
			briefingIn = true
		}
		if w, ok := app.buildHomeWidget(ctx, ts, def, item, shared, customize, false, viewer); ok {
			view.Widgets = append(view.Widgets, w)
		}
	}

	// The anchor advances only after a real (non-Customize) home
	// render that included the briefing: looking at the digest
	// is what moves the window. A Customize pass arranges the
	// module; it does not count as having read it.
	if briefingIn {
		view.BriefingSince = shared.BriefingSince(ctx).Unix()
	}
	if briefingIn && advance {
		app.advanceBriefingAnchor(ctx, userID, shared.now)
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
	Size        wSize               // current size (on the home, or the default)
	Default     wSize               // what Add a widget gives it
	Multi       bool                // Add a widget can add another copy while one stands
	Sizes       []customizeSize     // every declared size, for the no-script picker
	Settings    *widgetSettingsView // the no-script settings form, for a widget on the home with options
}

// customizeSize is one size a widget declares, as the no-script size
// picker offers it.
type customizeSize struct {
	Value    string // "2x1"
	Label    string // "2 by 1: the total beside its chart."
	Selected bool
}

// customizeGroup is one category of the Add a widget dialog.
type customizeGroup struct {
	Title   string
	Entries []customizeEntry
}

type customizeView struct {
	Entries []customizeEntry // the catalog, by category then title
	Groups  []customizeGroup // the same entries, grouped by category
}

// widgetCategoryTitle is a category's heading in the Add a widget dialog.
var widgetCategoryTitle = map[string]string{
	wcatOverview: "Overview", wcatPilots: "Pilots", wcatMoney: "Money", wcatIndustry: "Industry",
	wcatMarket: "Market", wcatCorp: "Corporation", wcatFleets: "Fleets", wcatIntel: "Intel",
}

func buildCustomizeView(layout []homeLayoutItem) *customizeView {
	position := make(map[string]int, len(layout))
	sizes := make(map[string]homeLayoutItem, len(layout))
	for i, item := range layout {
		position[item.ID] = i + 1
		sizes[item.ID] = item
	}
	view := &customizeView{}
	for _, def := range homeWidgetCatalog() {
		entry := customizeEntry{
			ID:          def.ID,
			Title:       def.Title,
			Description: def.Description,
			Enabled:     position[def.ID] > 0,
			Position:    position[def.ID],
			Default:     def.Default,
			Multi:       def.Multi,
		}
		entry.Size.W, entry.Size.H = widgetSize(homeLayoutItem{ID: def.ID, W: sizes[def.ID].W, H: sizes[def.ID].H})
		for _, sz := range def.Sizes() {
			entry.Sizes = append(entry.Sizes, customizeSize{
				Value:    sz.String(),
				Label:    fmt.Sprintf("%d by %d: %s", sz.W, sz.H, lowerFirst(def.FormFor(sz).Says)),
				Selected: sz == entry.Size,
			})
		}
		if entry.Enabled {
			entry.CanUp = entry.Position > 1
			entry.CanDown = entry.Position < len(layout)
		}
		view.Entries = append(view.Entries, entry)
		title := widgetCategoryTitle[def.Category]
		if n := len(view.Groups); n == 0 || view.Groups[n-1].Title != title {
			view.Groups = append(view.Groups, customizeGroup{Title: title})
		}
		g := &view.Groups[len(view.Groups)-1]
		g.Entries = append(g.Entries, entry)
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
// toggle a widget, move one up or down, resize one (action=size,
// w and h in grid cells), reset to the default, accept a whole new
// order from a drag (action=order, layout keys comma-joined), add a
// copy of a widget that allows copies (action=copy), or
// resize several cards at once (action=sizes, "key:WxH" comma-joined,
// the gaps Fill gaps closes), or set the whole arrangement, order and
// sizes (action=arrange, "key:WxH" comma-joined: undo, redo, swaps). A card is named by key (its layout key,
// "id#2" for a copy), defaulting to widget. Every control is a plain
// form, so arranging works with no JavaScript: the change saves and
// bounces back to Customize. Requests with X-Requested-With get JSON
// instead of the redirect.
func (app *Application) handleHomeLayout(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	if userID == 0 {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	scripted := r.Header.Get("X-Requested-With") == "XMLHttpRequest"
	raw, err := app.queries.GetUserHomeLayout(ctx, userID)
	if err != nil {
		raw = ""
	}
	// The change applies to the layout the account's home actually
	// shows: with nothing saved that is the account's preset, so a
	// widget standing only through the preset is found and resized
	// here instead of the change silently going nowhere.
	layout := app.effectiveHomeLayout(ctx, userID, raw)
	find := func(key string) int {
		for i, item := range layout {
			if item.LayoutKey() == key {
				return i
			}
		}
		return -1
	}
	refuse := func() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"ok":false}`)
	}
	reply := `{"ok":true}`

	if err := r.ParseForm(); err == nil {
		action := r.FormValue("action")
		key := r.FormValue("key")
		if key == "" {
			key = r.FormValue("widget")
		}
		def, known := widgetDefForKey(key)
		switch action {
		case "reset":
			// Store the account's own preset rather than "": the
			// intent (default set, auto spans) stays explicit.
			app.saveHomeLayout(ctx, userID, app.defaultLayoutFor(ctx, userID))
		case "toggle":
			if known {
				if i := find(key); i >= 0 {
					layout = append(layout[:i], layout[i+1:]...)
				} else if key == def.ID {
					layout = append(layout, homeLayoutItem{ID: def.ID})
				}
				app.saveHomeLayout(ctx, userID, layout)
			}
		case "copy":
			// Another card of a widget that allows copies, at its
			// default size, under the first free key.
			added := ""
			if known && def.Multi {
				for n := 1; n <= homeMaxCopies && added == ""; n++ {
					if k := copyKey(def.ID, n); find(k) < 0 {
						added = k
						item := homeLayoutItem{ID: def.ID}
						if n > 1 {
							item.Key = k
						}
						layout = append(layout, item)
					}
				}
			}
			if added == "" {
				if scripted {
					refuse()
					return
				}
				break
			}
			// A removed copy keeps its settings for undo; a new copy
			// under the same key starts at the defaults.
			if err := app.queries.DeleteWidgetConfig(ctx, db.DeleteWidgetConfigParams{UserID: userID, WidgetID: added}); err != nil {
				logging.Errorf("home: clear settings of %s for user %d: %v", added, userID, err)
			}
			app.saveHomeLayout(ctx, userID, layout)
			reply = `{"ok":true,"key":` + strconv.Quote(added) + `}`
		case "up", "down":
			if known {
				if i := find(key); i >= 0 {
					j := i - 1
					if action == "down" {
						j = i + 1
					}
					if j >= 0 && j < len(layout) {
						layout[i], layout[j] = layout[j], layout[i]
					}
				}
				app.saveHomeLayout(ctx, userID, layout)
			}
		case "order":
			// The whole arrangement, as dragged. Unknown and
			// duplicate keys drop out and the submitted order
			// wins; sizes carry over from the saved layout (they
			// are orthogonal to order), and a key never seen before
			// starts at its default size. An empty or all-unknown
			// list turns everything off, exactly like toggling each
			// widget off would.
			prefs := make(map[string]homeLayoutItem, len(layout))
			for _, item := range layout {
				prefs[item.LayoutKey()] = item
			}
			next := make([]homeLayoutItem, 0, len(layout))
			seen := make(map[string]bool, len(layout))
			for _, k := range strings.Split(r.FormValue("ids"), ",") {
				k = strings.TrimSpace(k)
				d, ok := widgetDefForKey(k)
				if !ok || seen[k] {
					continue
				}
				seen[k] = true
				item := homeLayoutItem{ID: d.ID, W: prefs[k].W, H: prefs[k].H}
				if k != d.ID {
					item.Key = k
				}
				next = append(next, item)
			}
			app.saveHomeLayout(ctx, userID, next)
		case "size":
			// Resize one card; the size must be one the widget
			// declares, else nothing changes, and a scripted caller
			// is told so, so it can put the size back. The
			// no-script picker sends one field, size=2x1.
			wv, werr := strconv.Atoi(r.FormValue("w"))
			hv, herr := strconv.Atoi(r.FormValue("h"))
			if sz, ok := parseWSize(r.FormValue("size")); ok {
				wv, hv, werr, herr = sz.W, sz.H, nil, nil
			}
			if known && werr == nil && herr == nil && def.Allows(wSize{wv, hv}) {
				// A card not on the home changes nothing.
				if i := find(key); i >= 0 {
					layout[i].W, layout[i].H = wv, hv
					app.saveHomeLayout(ctx, userID, layout)
				}
			} else if scripted {
				refuse()
				return
			}
		case "arrange":
			// The whole layout in one change, "key:WxH" comma-joined
			// in order: undo and redo put back an arrangement this
			// way, and a swap moves two cards and trades their sizes
			// at once. Unknown and repeated keys drop out; a size the
			// widget does not declare falls back to its default. A
			// card left out leaves the home and keeps its settings, as
			// a remove does.
			next := make([]homeLayoutItem, 0, len(layout))
			seen := make(map[string]bool, len(layout))
			for _, pair := range strings.Split(r.FormValue("layout"), ",") {
				k, raw, _ := strings.Cut(strings.TrimSpace(pair), ":")
				d, ok := widgetDefForKey(k)
				if !ok || seen[k] {
					continue
				}
				seen[k] = true
				item := homeLayoutItem{ID: d.ID}
				if k != d.ID {
					item.Key = k
				}
				if sz, sok := parseWSize(raw); sok && d.Allows(sz) {
					item.W, item.H = sz.W, sz.H
				}
				next = append(next, item)
			}
			app.saveHomeLayout(ctx, userID, next)
		case "sizes":
			// Several cards at once: each "key:WxH" pair applies
			// when the card is on the home and the widget declares
			// the size; the rest are skipped.
			changed := false
			for _, pair := range strings.Split(r.FormValue("sizes"), ",") {
				k, raw, ok := strings.Cut(strings.TrimSpace(pair), ":")
				sz, sok := parseWSize(raw)
				d, dok := widgetDefForKey(k)
				i := find(k)
				if !ok || !sok || !dok || i < 0 || !d.Allows(sz) {
					continue
				}
				layout[i].W, layout[i].H = sz.W, sz.H
				changed = true
			}
			if changed {
				app.saveHomeLayout(ctx, userID, layout)
			}
		}
	}
	if scripted {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, reply)
		return
	}
	http.Redirect(w, r, "/?customize=1", http.StatusSeeOther)
}
