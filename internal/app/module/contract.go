package module

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/alexedwards/scs/v2"

	"github.com/go-chi/chi/v5"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/fitview"
	"evesynapse/internal/standing"
)

// NavEntry is one page in a section's nav: its link, its label, and
// the template files that count as being on it.
type NavEntry struct {
	Href  string
	Label string
	Pages []string
	// After is the Href of the entry this one follows in its section;
	// empty (or not found) appends it. It keeps a module's page where
	// the menu always had it.
	After string
}

// NavSection associates nav entries with a category section key (e.g. "industry").
type NavSection struct {
	Section string
	Entries []NavEntry
}

// SuggestItem is one row of the shared autocomplete feed the front end
// reads (see FRONTEND.md); core's other search boxes answer with the
// same shape.
type SuggestItem struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Label   string `json:"label,omitempty"`
	Kind    string `json:"kind,omitempty"`
	URL     string `json:"url,omitempty"`
	Value   string `json:"value,omitempty"`
	Missing bool   `json:"missing,omitempty"`
}

// RegionOption is one market region the pages offer.
type RegionOption struct {
	ID   int64
	Name string
}

// RegionChoice is a region option as a page's filter shows it.
type RegionChoice struct {
	ID     int64
	Name   string
	Active bool
}

// QueryValue is the first value of a query parameter, trimmed.
func QueryValue(q map[string][]string, key string) string {
	if vs, ok := q[key]; ok && len(vs) > 0 {
		return strings.TrimSpace(vs[0])
	}
	return ""
}

// NamedChoice is one pickable thing a page offers: an id, its name,
// and whether it is the one chosen.
type NamedChoice struct {
	ID       int64
	Name     string
	Selected bool
}

// PublicFitsPath is where the public fits browser lives.
const PublicFitsPath = "/fittings/public/"

// PublicFitBody is what the fitting side derives from a public fit for
// its page. Sections and Sim are the fitting view types, carried
// opaquely: the template reads them, the module never does.
type PublicFitBody struct {
	PriceKnown bool
	PriceRaw   float64
	PriceTotal string
	Sections   any
	Sim        any
	EFT        string
	// Preparing: the stored page for this version of the fit is not
	// built yet (the worker builds it within a cycle or two).
	Preparing bool
}

// PublicFitCard is one fit on the browser, a ship page or the fit page
// header: stored aggregates only, never a per-render computation.
type PublicFitCard struct {
	ID          int64
	Name        string
	Ship        string
	ShipTypeID  int64
	Hull        string
	Author      string
	Mine        bool
	Stars       float64
	StarCount   int64
	Views       int64
	Price       float64
	PriceKnown  bool
	PriceLine   string
	Tags        []string
	Updated     time.Time
	UpdatedLine string
	FitURL      string
	// OpenToolURL opens the fit in the simulator (own fits via
	// ?local=, others via ?public=): the doctrine keepers' flow.
	OpenToolURL string
	// AddField names this card’s hidden input on the keeper’s
	// add-to-doctrine form: local for own fits, public otherwise.
	AddField string
}

// PublicFitCardOf renders one list row; tags decode from the stored
// document.
func PublicFitCardOf(row db.ListPublicFitsRow, userID int64) PublicFitCard {
	id := strconv.FormatInt(row.ID, 10)
	card := PublicFitCard{
		ID: row.ID, Name: row.Name, Ship: row.ShipName, ShipTypeID: row.ShipTypeID,
		Hull: row.Hull, Author: row.AuthorName, Mine: userID > 0 && row.UserID == userID,
		Views: row.ViewCount, Price: row.TotalPrice, PriceKnown: row.TotalPrice > 0,
		Updated: row.UpdatedAt, UpdatedLine: row.UpdatedAt.UTC().Format(time.RFC3339),
		FitURL: PublicFitsPath + "fit/" + id + "/",
	}
	if card.Mine {
		card.OpenToolURL = "/fittings/?local=" + id + "#fit-editor"
		card.AddField = "local"
	} else {
		card.OpenToolURL = "/fittings/?public=" + id + "#fit-editor"
		card.AddField = "public"
	}
	if card.PriceKnown {
		card.PriceLine = esi.FormatISK(row.TotalPrice) + " ISK"
	}
	if card.Ship == "" {
		card.Ship = "Type #" + strconv.FormatInt(row.ShipTypeID, 10)
	}
	if row.RatingCount > 0 {
		card.Stars = float64(row.RatingSum) / float64(row.RatingCount)
		card.StarCount = row.RatingCount
	}
	var doc struct {
		Tags []string `json:"tags,omitempty"`
	}
	if json.Unmarshal([]byte(row.ItemsJson), &doc) == nil {
		card.Tags = doc.Tags
	}
	return card
}

// WalletDivisionCount is how many wallet divisions a corporation has.
const WalletDivisionCount = 7

// WalletDivisionLabel names a wallet division the way the client does:
// division 1 is the master wallet.
func WalletDivisionLabel(division int64) string {
	if division == 1 {
		return "Master Wallet"
	}
	return fmt.Sprintf("Division %d", division)
}

// CorpSectionState is the empty-state triage every corporation section
// renders from: data loaded, still warming, refused for want of an
// in-game role, or never fed because the character granted none of the
// scopes that feed the section.
type CorpSectionState struct {
	Loaded      bool
	Warming     bool   // no snapshot yet and no refusal recorded
	Forbidden   bool   // ESI answered 403 on the last attempt
	RoleMissing string // role label when Forbidden named one ("" = label-less)
	// NoFeeder: no snapshot, no refusal, and the character granted none
	// of the scopes of the module that reads this kind, so no fetch
	// could ever feed it. AddScopesURL is the targeted re-link that
	// asks for that module's scopes (the role opt-in), the way locked
	// features offer theirs.
	NoFeeder     bool
	AddScopesURL string
}

// CorpViewBase carries what every corporation subpage header renders:
// the viewing character, the corporation’s display title, and the ids
// the switcher and sub-nav links need. Embedded in each subpage’s view.
type CorpViewBase struct {
	CharacterName string
	CharacterID   int64 // active character (switcher + sub-nav links)
	CorpID        int64
	CorpTitle     string // "Name [TICKER]", or "Corporation #<id>" until corp_info lands
	MemberCount   string // member count from corp_info, "" when unknown
}

// CorpPageSelection is PickCorpPage’s result: the switcher state plus the
// selected character’s corporation.
type CorpPageSelection struct {
	Active db.Character
	Links  []CharLink
	Base   CorpViewBase
}

// Character link state and scopes the modules ask about.
const (
	LinkStateOK   = "ok"
	MailSendScope = "esi-mail.send_mail.v1"
)

// CharacterHasScope reports whether a linked character granted a scope.
func CharacterHasScope(ch db.Character, scope string) bool {
	for _, s := range strings.Fields(ch.Scopes) {
		if s == scope {
			return true
		}
	}
	return false
}

// Op timing the corporation modules match losses and fits to.
const (
	// OpCaptureLead and OpCaptureTail are how far around an op’s own
	// times a fleet or a loss still counts as the op’s.
	OpCaptureLead = 15 * time.Minute
	OpCaptureTail = 15 * time.Minute
	// OpMaxDuration is the longest an op can run, in minutes.
	OpMaxDuration = 24 * 60
	// AttendanceFleet is the attendance source for what the fleet reported.
	AttendanceFleet = "fleet"
)

// OpEnd is when an op is over.
func OpEnd(op db.Op) time.Time {
	return op.StartsAt.Add(time.Duration(op.DurationMinutes) * time.Minute)
}

// OpURL is an op’s page.
func OpURL(id int64) string { return fmt.Sprintf("/ops/%d", id) }

// CorpAddress is a corporation page’s address for one corporation.
func CorpAddress(path string, corp int64) string {
	if corp == 0 {
		return path
	}
	return path + "?corporation=" + strconv.FormatInt(corp, 10)
}

// CorpActor is how an account acts for a corporation: the character of
// its that is in the corporation, and one that can send EVE mail.
type CorpActor struct {
	CharacterID int64
	Mailer      *db.Character
}

// DoctrineFitView is one fit of a doctrine on a page.
type DoctrineFitView struct {
	ID         int64
	Name       string
	Ship       string
	ShipTypeID int64
	Role       string
	Note       string
	EFT        string // the fit as text the game imports; the doctrine's own page only
}

// DoctrineURL is a doctrine's page.
func DoctrineURL(id int64) string { return "/doctrines/" + strconv.FormatInt(id, 10) }

// EconSectionState is the empty-state triage the economy sections render
// from: data loaded, still warming, or why it is absent.
type EconSectionState struct {
	Loaded  bool
	Warming bool   // no snapshot yet and no failure recorded
	Note    string // why the data is absent, when not warming
}

// Scopes and op answers the pilot pages ask about.
const (
	MailOrganizeScope    = "esi-mail.organize_mail.v1"
	CalendarRespondScope = "esi-calendar.respond_calendar_events.v1"
	OpYes                = "yes"
	OpMaybe              = "maybe"
	OpNo                 = "no"
)

// ErrMailToken is what sending a mail answers when the character’s token
// could not be made good.
var ErrMailToken = errors.New("mail: no usable token")

// RelinkNotice is what a reader is told when a character lacks the scope
// an action needs.
func RelinkNotice(name, what string) string {
	return name + " was linked before EveSynapse asked for permission to " + what + " — sign in again to grant it."
}

// ParseRFC3339 reads an ESI timestamp; false when it is empty or not one.
func ParseRFC3339(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, raw)
	return t, err == nil
}

// OrderRangeLabel renders an order range readably: the ESI enum is
// "station", "solarsystem", "region" or a jump count.
func OrderRangeLabel(r string) string {
	switch r {
	case "station":
		return "Station"
	case "solarsystem":
		return "System"
	case "region":
		return "Region"
	case "":
		return ""
	default:
		return r + " jumps"
	}
}

// FetchGate is the library seam between the fetch pipeline and a module
// that can answer a fetch from elsewhere (the swarm). Core asks every
// registered gate before it spends ESI on a name, record, structure or
// market history. A gate only ever delays or replaces a fetch: it never
// cancels one, and any failure of a gate (an error, a panic) is read as
// "fetch normally" (core recovers and answers false).
type FetchGate interface {
	// Defer says this fetch should wait for a peer this time. kind is one
	// of the FetchKind values and key names the thing (an id, or
	// "region/type" for a market history).
	Defer(ctx context.Context, deps Deps, kind, key string, now time.Time) bool
	// UrgentHistory serves one due market history from elsewhere before
	// the urgent drain spends ESI; true means it was applied and the ESI
	// fetch may be skipped.
	UrgentHistory(ctx context.Context, deps Deps, regionID, typeID int64) bool
}

// What a fetch is of: the kinds a gate is asked about.
const (
	FetchKindHistory     = "market_history"
	FetchKindTypeName    = "type_name"
	FetchKindCorporation = "corporation_record"
	FetchKindAlliance    = "alliance_record"
	FetchKindStructure   = "structure_name"
)

// MarketKey is one (region, type) pair to warm.
type MarketKey struct {
	RegionID int64
	TypeID   int64
}

// MarketFetchKind is the fetch-state kind of one market key.
func MarketFetchKind(prefix string, key MarketKey) string {
	return prefix + "_" + strconv.FormatInt(key.RegionID, 10) + "_" + strconv.FormatInt(key.TypeID, 10)
}

// Fetch bookkeeping constants shared by the data feed and the modules.
const (
	FetchStateOK           = "ok"
	OrgStateReady          = "ready"
	HistoryRefetchGate     = 20 * time.Hour
	StructureRenameWindow  = 30 * 24 * time.Hour
	StructureMissingWindow = 24 * time.Hour
	StructureScope         = "esi-universe.read_structures.v1"
	// WantViewed is the priority of a want a page view noted.
	WantViewed int64 = 3
)

// CorporationRecordPayload is the stored public record behind a corporation
// page: the corporation profile, with the alliance's name and ticker baked
// in when the corporation belongs to one.
type CorporationRecordPayload struct {
	Corp     esi.Corporation `json:"corp"`
	Alliance esi.Alliance    `json:"alliance"`
}

// AllianceMemberCorp is one member corporation of an alliance; Name is
// baked when the drain could resolve it, "" until then.
type AllianceMemberCorp struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// AllianceRecordPayload is the stored public record behind an alliance
// page: the alliance profile plus its member corporations.
type AllianceRecordPayload struct {
	Alliance     esi.Alliance         `json:"alliance"`
	Corporations []AllianceMemberCorp `json:"corporations"`
}

// DecodeCorpPayload reads a stored corporation record: the app's
// {"corp":{...},"alliance":{...}}, or a bare ESI corporation a peer sent.
func DecodeCorpPayload(raw string) (CorporationRecordPayload, bool) {
	var p CorporationRecordPayload
	if err := json.Unmarshal([]byte(raw), &p); err == nil && p.Corp.Name != "" {
		return p, true
	}
	var bare esi.Corporation
	if err := json.Unmarshal([]byte(raw), &bare); err == nil && bare.Name != "" {
		return CorporationRecordPayload{Corp: bare}, true
	}
	return CorporationRecordPayload{}, false
}

// decodeAlliancePayload is decodeCorpPayload for alliance records: the
// app's {"alliance":{...},"corporations":[...]}, or a bare ESI alliance.
func DecodeAlliancePayload(raw string) (AllianceRecordPayload, bool) {
	var p AllianceRecordPayload
	if err := json.Unmarshal([]byte(raw), &p); err == nil && p.Alliance.Name != "" {
		return p, true
	}
	var bare esi.Alliance
	if err := json.Unmarshal([]byte(raw), &bare); err == nil && bare.Name != "" {
		return AllianceRecordPayload{Alliance: bare}, true
	}
	return AllianceRecordPayload{}, false
}

// StructureIDThreshold is where player-structure ids begin: far above the
// station and system ranges.
const StructureIDThreshold = int64(1_000_000_000_000)

// IsStructureID reports whether a bare location id is in the
// player-structure range.
func IsStructureID(id int64) bool { return id > StructureIDThreshold }

// StructureSourceRank orders name provenance: ESI truth always outranks
// the community tier, so a lower-trust name never overwrites a better one.
func StructureSourceRank(source string) int {
	switch source {
	case esi.StructureSourceESI:
		return 3
	case esi.StructureSourceCorp:
		return 2
	case esi.StructureSourceCommunity:
		return 1
	}
	return 0
}

// WidgetDecl declares a home widget the module owns: core lists and lays
// it out only when the module is built in. Feature is the manifest feature
// whose scopes power it (the module's own id when empty); After names the
// widget it follows in the catalog.
type WidgetDecl struct {
	ID          string
	Title       string
	Description string
	W, H        int
	Feature     string
	After       string
}

// BriefingTopicDecl declares a family of briefing lines the module
// owns: core's briefing builder produces those lines only when the
// module is built in. The builder stays core's, reading the shared
// snapshots; the declaration is only the gate.
type BriefingTopicDecl struct {
	ID    string
	Title string
}

// NotifyKindDecl declares a kind of notification the module owns: core
// offers it in the settings, produces it and shows it in the bell only when
// the module is built in. After names the kind it follows in the settings.
type NotifyKindDecl struct {
	ID       string
	Title    string
	Standing bool
	Account  bool
	Feature  string
	After    string
}

// Cycle is what a worker-cycle job is given: the fetch allowance left,
// the characters this cycle works, and the time.
type Cycle struct {
	Budget     Budget
	Characters []db.Character
	Now        time.Time
}

// Budget is the cycle's fetch allowance.
type Budget interface {
	// Take spends one fetch, reporting whether it was available.
	Take() bool
	// Exhausted reports that none is left.
	Exhausted() bool
}

// CyclePhase is where in the worker cycle a CycleJob runs.
type CyclePhase string

const (
	// BeforePublicData runs the job ahead of the public-data passes.
	BeforePublicData CyclePhase = "before public data"
	// AfterPublicData runs it right after them.
	AfterPublicData CyclePhase = "after public data"
)

// CycleJob is a pass that runs inside the worker cycle under the cycle’s
// own accounting: it is timed under its Name, guarded like a pass, and
// reports what it stored and whether ESI limited it. Describe is how a
// limit is worded in the log.
type CycleJob struct {
	Name     string
	Describe string
	At       CyclePhase
	Run      func(ctx context.Context, deps Deps, c Cycle) (stored int, limited bool)
}

// PageSection is a block a module contributes to a core page (the admin
// page, the sync page): rendered to HTML, with an anchor and a title for
// the page’s own jump links.
type PageSection struct {
	Anchor string
	Title  string
	Render func(ctx context.Context, deps Deps) template.HTML
}

// Page declares a template file rendered by the module, its default section,
// and any client scripts that must be loaded for it.
type Page struct {
	Template string
	Section  string
	Scripts  []string
}

// PlaceRef is a place's title with the ids it can link by (see the
// core link policy: station, then structure, then system, otherwise
// plain text).
type PlaceRef struct {
	Name        string
	StationID   int64
	StructureID int64
	SystemID    int64
}

// Scope is one ESI scope a module wants, with what granting it adds,
// in words the player reads on the sign-up and re-link screens.
type Scope struct {
	Scope   string
	Unlocks string
}

// SitemapRow is one stored page a module lists in sitemap.xml.
type SitemapRow struct {
	Path       string
	LastMod    string // 2006-01-02, "" when unknown
	ChangeFreq string
	Priority   string
}

// SitemapEntry is one public page a module adds to sitemap.xml.
type SitemapEntry struct {
	Path       string // e.g. "/industry/pi/"
	ChangeFreq string
	Priority   string
}

// JobPhase is where in the worker cycle a module job runs: the cycle's
// structure is core's, and a job names the pass it follows.
type JobPhase string

const (
	// AfterMarket runs the job after the market data pass.
	AfterMarket JobPhase = "market"
	// AfterGuidePrices runs it right after the market guide refresh,
	// ahead of the character pass.
	AfterGuidePrices JobPhase = "guide"
	// AfterNotifications runs it last, with everything the cycle
	// fetched already stored and this cycle's notifications made.
	AfterNotifications JobPhase = "notifications"
)

// WorkerJob is a pass a module runs in the worker cycle, under its own
// name and its own panic guard (one job failing leaves the others
// running). Start is called once per cycle at its phase. A job with
// slow work that must not hold the cycle starts it with Deps.Background
// and returns; a job that is meant to run in the cycle, as a bounded
// pass does, simply does its work before it returns.
type WorkerJob struct {
	Name  string
	After JobPhase // AfterMarket when empty
	Start func(ctx context.Context, deps Deps)
}

// NotifyEvent is one notification offered for delivery.
type NotifyEvent struct {
	Kind        string
	CharacterID int64 // 0 for an account-level event
	Title       string
	URL         string
}

// NotifyMessage is a notification as it is sent outside the site: the
// notifier has already condensed a burst (a few are sent one by one,
// many as a single line with a count).
type NotifyMessage struct {
	Title string
	Body  string
	URL   string // a site path; Deps.SiteURL makes it a full address
}

// Notification is one batch of new notifications for one account.
type Notification struct {
	Events   []NotifyEvent
	Messages []NotifyMessage
}

// Deliverer sends a Notification to the account through one channel.
//
// Delivery is a fan-out: the notifier offers every notification to
// every registered Deliverer, in registration order. Each Deliverer
// honors the account's own preferences for its channel (an account that
// has not asked for it gets nothing from it). The result says whether
// THIS channel delivered it; it is information for logs and
// diagnostics. A true never stops the others, and nothing is
// first-claim-wins: the top bar, browser push and every module channel
// each deliver the same notification independently. A failure loses
// nothing, because the notification is already stored in the top bar.
type Deliverer func(ctx context.Context, deps Deps, userID int64, n Notification) (delivered bool)

// SettingsSection is a module's contribution to the notification
// settings page.
type SettingsSection struct {
	// Channel renders inside the settings form, under the per-kind
	// table (an "also send these by <channel>" box). Nil means none.
	Channel func(ctx context.Context, deps Deps, r *http.Request, userID int64) template.HTML
	// Account renders below the form as a section of its own (connect
	// or disconnect the channel's account). Nil means none.
	Account func(ctx context.Context, deps Deps, r *http.Request, userID int64) template.HTML
	// Save is called when the form is saved, with the submitted values;
	// an error makes the save report failure.
	Save func(ctx context.Context, deps Deps, userID int64, form url.Values) error
}

// WhoChoice is one thing a rule can be about: Value is "kind" or
// "kind:ref".
type WhoChoice struct {
	Value, Label string
}

// OwnerOption is one corporation or alliance in an owner chooser.
type OwnerOption struct {
	ID     int64
	Name   string
	Active bool
	// Param and Value are what the option's link sets in the address.
	Param, Value string
}

// DiscordConfig is the Discord access an install has: the sign-in
// application and the bot. HTTP and BaseURL are test injections.
type DiscordConfig struct {
	ClientID     string
	ClientSecret string
	BotToken     string
	HTTP         *http.Client
	BaseURL      string
}

// ModelConfig is the access a module has to the configured language
// model (Meta's Muse Wonder with an API key, else Gemma on Cloudflare
// Workers AI). HTTP and MetaAPIBase are test injections; nil and empty
// mean the real endpoints.
type ModelConfig struct {
	MetaKey             string
	CloudflareAccountID string
	CloudflareToken     string
	HTTP                *http.Client
	MetaAPIBase         string
}

// Configured reports whether a model can be asked at all: a Meta API
// key, or both Cloudflare credentials.
func (c ModelConfig) Configured() bool {
	return c.MetaKey != "" || (c.CloudflareAccountID != "" && c.CloudflareToken != "")
}

// UsesMeta reports whether Muse Wonder (MODEL_API_KEY set) answers
// rather than Gemma.
func (c ModelConfig) UsesMeta() bool { return c.MetaKey != "" }

// SuggestPool represents a search or quick-jump suggestion pool.
type SuggestPool struct {
	Name string
}

// NavLink is one rendered nav link for category menus and section tab strips.
type NavLink struct {
	Href   string
	Label  string
	Active bool
}

// SwitcherEntry is one linked character in the header switcher.
type SwitcherEntry struct {
	ID          int64
	Name        string
	Tags        string
	PortraitURL string
	Active      bool
	Relink      bool
}

// CharLink is one entry in a character picker or switcher.
type CharLink struct {
	ID     int64
	Name   string
	Active bool
	Tags   string
}

// ShellData holds all the layout, session, nav, and shell state
// needed by base.html and shared partials.
type ShellData struct {
	Version        string
	AssetVersion   string
	LoggedIn       bool
	IsAdmin        bool
	CharacterName  string
	SSOConfigured  bool
	AutoRefresh    bool
	NavState       string
	Notice         string
	Error          string
	Section        string
	NavPage        string
	Scripts        []string
	HeadMeta       template.HTML
	Switcher       []SwitcherEntry
	ViewerChars    map[int64]bool
	Notify         any
	NotifyPoll     int
	NotifyPrompt   any
	NotifySettings any
	NavLinks       map[string][]NavLink
	SectionTabs    []NavLink
	SectionSubTabs []NavLink
	NavCharacter   int64
}

// Shell returns a pointer to the embedded ShellData so that
// any struct embedding ShellData satisfies the shellProvider interface.
func (s *ShellData) Shell() *ShellData {
	return s
}

// Deps is the narrow foundation interface consumed by module packages.
// Modules consume this interface, never the concrete Application struct.
type Deps interface {
	DB() *db.Queries
	ValuationPrices(ctx context.Context) map[int64]esi.MarketPrice
	// TypicalSells is The Forge's swept typical sell per type —
	// the first tier of the house price order.
	TypicalSells(ctx context.Context) map[int64]float64
	// TrueValues is the stored true-value estimated sell per
	// type (thin types only) — the order's second tier.
	TrueValues(ctx context.Context) map[int64]float64
	LoadCorpSnapshot(ctx context.Context, characterID int64, kind string, out any) bool
	PINotEnabled(ctx context.Context, ch db.Character) bool
	PublicShell(ctx context.Context) ShellData
	PickCharacter(ctx context.Context, r *http.Request, path string) ([]db.Character, db.Character, []CharLink, error)
	Render(ctx context.Context, w http.ResponseWriter, status int, page string, data any)
	AnonPages() func(http.Handler) http.Handler
	// AnonSim is the tighter per-address allowance for pages whose
	// result can be large (the simulator, the build planner).
	AnonSim() func(http.Handler) http.Handler
	// RequireAuth guards a route group for signed-in accounts.
	RequireAuth() func(http.Handler) http.Handler

	// UserID is the signed-in user's id, 0 when anonymous.
	UserID(ctx context.Context) int64
	// LoadGlobalSnapshot reads one public-data snapshot (wars,
	// incursions, ...) the worker keeps warm; false until it exists.
	LoadGlobalSnapshot(ctx context.Context, kind string, out any) bool
	// UserCorporationIDs is every corporation the user's characters
	// belong to.
	UserCorporationIDs(ctx context.Context, userID int64) map[int64]bool
	// OrgLink renders an alliance or corporation as a link to its page.
	OrgLink(ctx context.Context, corporationID, allianceID int64) template.HTML
	// SystemRef is a solar system's title with its link ids.
	SystemRef(ctx context.Context, systemID int64) PlaceRef
	// SystemSecurity is a solar system's stored security status the
	// way pages show it: the one-decimal text and the color tier of
	// the .sec-pill (the fleet overview's location badge). known is
	// false when the SDE has no row for the system.
	SystemSecurity(ctx context.Context, systemID int64) (text string, tier int, known bool)
	// ConstellationName reads the worker-warmed constellation name cache.
	ConstellationName(id int64) (string, bool)
	// FormatFinish writes an ESI timestamp the way pages show times.
	FormatFinish(ts string) string
	// HumanizeEnum turns an ESI snake_case enum into words.
	HumanizeEnum(s string) string
	// WarDetailWindow is how many of the newest wars the worker keeps
	// details warm for.
	WarDetailWindow() int

	// ModelConfig is the configured language model's access.
	ModelConfig() ModelConfig
	// Background runs fn in a goroutine the application waits for on
	// shutdown.
	Background(fn func())
	// TypeName is an item's name from the name caches, falling back to
	// the local static data; "Type #id" when unknown.
	TypeName(ctx context.Context, typeID int64) string
	// StationRef resolves an order location to its title and link ids
	// (memo keeps a render to one resolution per location).
	StationRef(ctx context.Context, memo map[int64]PlaceRef, locationID int64) PlaceRef
	// SpreadDefaults are the core-owned default minimum spread (percent)
	// and row cap for reading the stored spread statistics: shared data,
	// the same numbers whichever module or page reads them.
	SpreadDefaults() (minSpreadPct float64, rowCap int)

	// --- names, sessions and shared feeds (the build planner reads these) ---

	// CachedTypeNames and CachedPlaceName are cache-only name reads.
	CachedTypeNames(ctx context.Context, ids []int64) map[int64]string
	CachedPlaceName(ctx context.Context, id int64) (string, bool)
	// NotePlaceWant asks the worker to resolve a place the page lacked.
	NotePlaceWant(ctx context.Context, id int64)
	// SessionCharacterID is the session's acting character, 0 when none.
	SessionCharacterID(ctx context.Context) int64
	// SlotSkillIDs resolves the industry slot skills' type ids by name.
	SlotSkillIDs(ctx context.Context) map[string]int64
	// WriteSuggest answers with the shared autocomplete shape.
	WriteSuggest(w http.ResponseWriter, rows []SuggestItem)
	// ServeCSV writes a CSV download the way every export does.
	ServeCSV(w http.ResponseWriter, filename string, header []string, rows [][]string)

	// --- skills and fragments (the skill plans module reads these) ---------

	// SkillsViewOpened tells the foresight that this account opened the
	// character's skills views, so their snapshots stay warm.
	SkillsViewOpened(ctx context.Context, userID, characterID int64)
	// LiveSkillqueue is the queue entries still to train at now.
	LiveSkillqueue(queue esi.Skillqueue, now time.Time) esi.Skillqueue
	// RenderFragmentStatus answers with one named template of a page
	// file (the live-region swap), at the given status.
	RenderFragmentStatus(w http.ResponseWriter, status int, page, define string, data any)

	// --- market data the market pages share ---------------------------------

	// MarketPrices is the cached market guide (average and adjusted prices).
	MarketPrices(ctx context.Context) (map[int64]esi.MarketPrice, error)
	// MarketRegions is the regions the market pages offer, in menu order;
	// DefaultMarketRegion is the one a page starts on, MarketRegionName
	// names any region the market knows.
	MarketRegions() []RegionOption
	DefaultMarketRegion() int64
	MarketRegionName(id int64) (string, bool)
	// QueueHistoryWant asks the worker for a type's price history in a
	// region at the page-want priority; nothing is fetched here.
	QueueHistoryWant(ctx context.Context, regionID, typeID int64) error
	// AgeText says how long ago a stored row was written: "12 minutes ago".
	AgeText(t time.Time) string
	// TypeNameOrID is an item's name from the name caches, "Type #id"
	// when unknown (and the page wants it resolved).
	TypeNameOrID(ctx context.Context, typeID int64) string

	// --- stored state and the game's API, for the fitting tool ----------------

	// StoredSnapshot reads one stored snapshot of a character; an error
	// when none is stored.
	StoredSnapshot(ctx context.Context, ch db.Character, kind string, out any) error
	// RenderFragment answers with one named template of a page file.
	RenderFragment(w http.ResponseWriter, page, define string, data any)
	// SuggestTypes is the shared item search over the stored types; pool
	// narrows it ("market" for anything sold).
	SuggestTypes(ctx context.Context, query, pool string, limit int64) []SuggestItem
	// AccessToken is a valid access token for one of the account's
	// characters, refreshed if it was about to lapse.
	AccessToken(ctx context.Context, ch db.Character) (string, error)
	// PostESI posts JSON to an authenticated game API path and decodes
	// the answer into out; a refusal is an *esi.StatusError.
	PostESI(ctx context.Context, token, path string, body, out any) error
	// RefreshSnapshot fetches and stores one snapshot of a character now.
	RefreshSnapshot(ctx context.Context, ch db.Character, kind string) error

	// ModuleEnabled reports whether another module is compiled in, for the
	// surfaces that only make sense with it.
	ModuleEnabled(id string) bool
	// StorePublicFitPageNow builds a public fit’s stored page now, on its
	// owner’s request.
	StorePublicFitPageNow(ctx context.Context, fitID int64)
	// DoctrineFitDoc reads one of a corporation’s doctrine fits the
	// account may use, as a fit document.
	DoctrineFitDoc(ctx context.Context, userID, fitID int64) (*fitview.FitDoc, bool)

	// --- the corporation foundation (core keeps the access model and the feed) ---

	// CorpSection resolves the display state of one corporation snapshot
	// kind: loaded, still warming, or refused for want of an in-game role.
	CorpSection(ctx context.Context, characterID int64, kind string, out any) CorpSectionState
	// PickCorpPage resolves a corporation sub-page: the account’s active
	// character, its switcher, and its corporation’s display identity.
	// ok=false means the session has no user or no characters yet.
	PickCorpPage(ctx context.Context, r *http.Request) (CorpPageSelection, bool, error)
	// CorpStructureNames maps structure ids to the names in the stored
	// structures snapshot.
	CorpStructureNames(ctx context.Context, characterID int64) map[int64]string
	// CorpLocationTitle titles an order or asset location.
	CorpLocationTitle(ctx context.Context, locationID int64, structureNames map[int64]string) string
	// LinkPlaceMemo links a location by the core link policy, memoised
	// per render.
	LinkPlaceMemo(ctx context.Context, memo map[int64]PlaceRef, locationID int64, title string) PlaceRef
	// ResolvedStructureTitle is a player structure’s resolved name, "".
	ResolvedStructureTitle(ctx context.Context, structureID int64) string
	// TxnCounterparty names the other side of a wallet transaction.
	TxnCounterparty(ctx context.Context, id int64) (name string, isChar, isCorp bool)
	// AssetLocations groups assets by location for the assets pages; the
	// result is the pages’ own location view, read by the template only.
	AssetLocations(ctx context.Context, items []esi.Asset, extraTitles, nameOverrides map[int64]string) any
	// OrderHealthText says how an order’s price stands against the market.
	OrderHealthText(myPrice float64, status string, stationBest, regionBest float64) (string, bool)
	// CorpDisplayName, PlayerCorps, PickCorporation and CorpPermits are
	// the access model the corporation modules ask.
	CorpDisplayName(ctx context.Context, corpID int64) string
	CorpPermits(ctx context.Context, st standing.Standing, corp int64, permission string) bool
	PlayerCorps(ctx context.Context, userID int64) []int64
	PickCorporation(ctx context.Context, r *http.Request, offered []int64) (int64, []OwnerOption)
	// OwnerCorporations lists the corporations an owner covers.
	OwnerCorporations(ctx context.Context, o standing.Owner) []int64
	// ResolvedAllianceName, LocationTitle: cache-only names.
	ResolvedAllianceName(ctx context.Context, allianceID int64) (string, bool)
	LocationTitle(ctx context.Context, id int64, locType string) string

	// DoctrineFitViews reads the fits of some doctrines, by doctrine id.
	DoctrineFitViews(ctx context.Context, doctrineIDs []int64) map[int64][]DoctrineFitView
	// --- the pilot pages: economy and comms ----------------------------------

	// EconSection resolves one stored economy section’s display state: a
	// decoded snapshot wins; otherwise the recorded fetch state decides
	// between the warming note and the failure note.
	EconSection(ctx context.Context, characterID int64, kind string, out any) EconSectionState
	// OpCorps lists the corporations an account has a character in
	// (managedOnly: those where it may manage ops).
	OpCorps(ctx context.Context, userID int64, managedOnly bool) []int64
	// RelinkURL is the address that re-links a character for one module’s
	// scopes.
	RelinkURL(moduleID string, characterID int64) string
	// CharLinkHTML links a character by the core link policy.
	CharLinkHTML(ctx context.Context, characterID int64, name string) template.HTML
	// NoteCharacterWant asks the worker to resolve a character’s name.
	NoteCharacterWant(ctx context.Context, characterID int64)
	// MarkMailReadLocally records a mail as read in the stored headers.
	MarkMailReadLocally(ctx context.Context, characterID, mailID int64)
	// AttachWalletGraph and WalletFlow build the wallet page’s balance
	// history and the home wallet-flow cells for one character; the results
	// are read by the wallet template only.
	AttachWalletGraph(ctx context.Context, userID, characterID int64, journal esi.WalletJournal, journalLoaded bool) any
	WalletFlow(ctx context.Context, ch db.Character) any
	// EmptyWalletGraph is the settled-empty balance history.
	EmptyWalletGraph() any
	AllianceDisplayName(ctx context.Context, allianceID int64) string
	// ResolvedCharacterName is a character’s name from every local tier;
	// settled says the local tiers have a final answer.
	ResolvedCharacterName(ctx context.Context, characterID int64) (name string, settled bool)
	CharacterLabelPending(ctx context.Context, characterID int64) bool
	CachedCharacterName(id int64) (string, bool)
	CachedCorpName(id int64) (string, bool)
	// LinkPlace links a location by the core link policy.
	LinkPlace(ctx context.Context, locationID int64, title string) PlaceRef
	EconLocationTitle(ctx context.Context, locationID int64) string
	// RewriteSnapshot edits one stored snapshot in place (what an action
	// that succeeded in the game changes locally, ahead of the next fetch).
	RewriteSnapshot(ctx context.Context, characterID int64, kind string, edit func(payload string) (string, bool))
	// PutESI and PostESIPublic write to the game’s API: an authenticated
	// PUT, and an unauthenticated POST (name lookups).
	PutESI(ctx context.Context, token, path string, body any) error
	PostESIPublic(ctx context.Context, path string, body, out any) error
	// --- the fetch seam: what a module that shares fetched data (the swarm) may ask ---

	// MarketFetchDue, RecordMarketFetchAt, HistoryCandidates and
	// FetchMarketHistory are the market history bookkeeping; NoteStructureIDs,
	// WantCorporation and WantAlliance queue what pages want resolved.
	MarketFetchDue(ctx context.Context, kind string, gate time.Duration) bool
	RecordMarketFetchAt(ctx context.Context, kind, state, detail string, at time.Time)
	HistoryCandidates(ctx context.Context) ([]MarketKey, error)
	FetchMarketHistory(ctx context.Context, key MarketKey) ([]esi.MarketHistoryDay, error)
	NoteStructureIDs(ctx context.Context, ids ...int64)
	WantCorporation(ctx context.Context, id, closeness int64) error
	WantAlliance(ctx context.Context, id, closeness int64) error
	// ESIGet reads a public ESI path (unauthenticated) into out;
	// FetchStructure reads a structure with a character’s token.
	ESIGet(ctx context.Context, path string, out any) error
	FetchStructure(ctx context.Context, ch db.Character, id int64) (esi.UniverseStructure, error)
	// The type-name cache the swarm keeps in step with its peers.
	LearnTypeName(id int64, name string)
	ForgetTypeName(id int64)
	MissingTypeNames(limit int) []int64
	NoteTypeNameMiss(id int64)
	// PublicOrigin is this install’s public address, "" when it has none.
	PublicOrigin() string
	// RequireAdmin guards a route group for administrators.
	RequireAdmin() func(http.Handler) http.Handler

	// CorpActorFor is how an account acts for a corporation; UserCorps the
	// corporations it has a character in, in the order of its characters.
	CorpActorFor(ctx context.Context, userID, corp int64) CorpActor
	UserCorps(ctx context.Context, userID int64) []int64
	// SendEVEMail sends an in-game mail from one of the account’s
	// characters; ESIRefusalDetail says why the game refused one.
	SendEVEMail(ctx context.Context, from db.Character, recipientID int64, recipientType, subject, body string, approvedCost int) error
	ESIRefusalDetail(err error) string

	// --- the fit foundation -------------------------------------------------

	// FitBuilder builds fit views (document, EFT, simulation) from the
	// foundation package, over core's stored state.
	FitBuilder() *fitview.Builder

	// --- public fit upkeep: the work that keeps anonymous renders stored-only ---

	// RefreshPublicFitPages builds the stored pages that are missing or
	// out of date and reports how many it built; FlushFitViews writes the
	// views counted since the last flush. Both are the fitting side’s to
	// do, run by the public fits module’s worker jobs.
	RefreshPublicFitPages(ctx context.Context) int
	FlushFitViews(ctx context.Context)

	// --- public fits (the fitting side keeps the fit-derived parts) ---------

	// DoctrineChosenFor reads the "for" of an address: the doctrine a
	// keeper is choosing fits for, nil unless the account keeps it.
	DoctrineChosenFor(ctx context.Context, userID int64, raw string) *NamedChoice
	// FleetRoles is the fleet-role taxonomy a fit’s tags are matched to.
	FleetRoles() []string
	// FitViewsPending counts a view of a public fit (a prefetch is not a
	// view) and returns the views not yet written to its row.
	FitViewsPending(ctx context.Context, fitID int64) int64
	// PublicFitBody is the fit-derived part of a public fit’s page: its
	// price, and the stored module groups, read-only simulation and EFT.
	PublicFitBody(ctx context.Context, fitID int64, updatedAt time.Time, itemsJSON string, shipTypeID int64) PublicFitBody
	// PublicPageURL makes a site path an absolute address, "" when the
	// install has no public address.
	PublicPageURL(path string) string
	// HeadMeta composes a PageMeta into the escaped head block
	// (description, canonical, Open Graph, twitter card, any
	// structured data) via the one shared mechanism.
	HeadMeta(meta PageMeta) template.HTML

	// --- the session, pages and the site ---------------------------------

	// Sessions is the session manager (OAuth state, flash messages).
	Sessions() *scs.SessionManager
	// Flash leaves a message for the next page this session renders.
	Flash(ctx context.Context, msg string)
	// FlashBack is how a form handler answers: leave a message and go
	// back to path.
	FlashBack(w http.ResponseWriter, r *http.Request, path string) func(string)
	// SiteURL makes a site path a full address for a message read
	// outside the site.
	SiteURL(path string) string
	// PageShell is the shell state of a signed-in page.
	PageShell(ctx context.Context) ShellData
	// Fragment renders one named template of a page file to HTML, for a
	// section a module contributes to a core page.
	Fragment(page, define string, data any) (template.HTML, error)
	// SQLDB is the database handle, for the few places that need a
	// transaction.
	SQLDB() *sql.DB
	// SealToken and OpenToken encrypt and decrypt a stored token.
	SealToken(plain string, userID int64, field string) (string, error)
	OpenToken(sealed string, userID int64, field string) (string, error)
	// DiscordConfig is this install's Discord access.
	DiscordConfig() DiscordConfig

	// --- standing: the corporation/alliance foundation --------------------

	// StandingFor works an account's standing out from stored data.
	StandingFor(ctx context.Context, userID int64) (standing.Standing, error)
	// ManageableOwners lists the corporations and alliances an account
	// may manage things for (it directs them).
	ManageableOwners(ctx context.Context, userID int64) []standing.Owner
	// OwnerName is how an owner is shown.
	OwnerName(ctx context.Context, o standing.Owner) string
	// CorpAlliance is a corporation's alliance and that alliance's
	// executor corporation (0, 0 when none or not fetched yet).
	CorpAlliance(ctx context.Context, corpID int64) (allianceID, executorCorpID int64)
	// WhoChoices lists what a rule of an owner's can be about, RuleWho
	// says in words who a stored rule is about, ParseRuleWho checks a
	// submitted choice against the owner.
	WhoChoices(ctx context.Context, owner standing.Owner) []WhoChoice
	RuleWho(ctx context.Context, owner standing.Owner, rule standing.Rule) string
	ParseRuleWho(ctx context.Context, owner standing.Owner, raw string) (kind, ref string, ok bool)
	// PickOwner chooses which of the offered owners a page is about
	// (from the address), with the chooser's options.
	PickOwner(ctx context.Context, r *http.Request, offered []standing.Owner) (standing.Owner, []OwnerOption, bool)
	// ResolvedCorpName and DisplayCharacter are cache-only names.
	ResolvedCorpName(ctx context.Context, corpID int64) (string, bool)
	DisplayCharacter(ctx context.Context, characterID int64) string
}

// Module is the registration contract exposed by every build-selectable module.
//
// A module declares what it needs from the build and from the player,
// all five explicitly (an empty list is written as an empty list, not
// left out): ID and Title, Requires (the modules it depends on),
// Scopes (the ESI scopes it cannot function without) and EnhanceScopes
// (optional scopes, each with the extra feature it unlocks). The build
// tool reads Requires; sign-up, the locked-state notices and the
// scope manifest read Scopes and EnhanceScopes, so no scope list lives
// anywhere else.
type Module struct {
	ID            string
	Title         string
	Requires      []string
	Scopes        []Scope
	EnhanceScopes []Scope
	// Activities names the kinds of play the module serves ("pilots",
	// "market", "industry", "corporation", "fitting"): the sign-up asks
	// players what they want to do and turns the answers into modules
	// from these declarations, with no list of its own to drift.
	Activities []string
	Nav        []NavSection
	Routes     func(r chi.Router, deps Deps)
	WorkerJobs []WorkerJob
	// Deliverers are the module's notification channels (see Deliverer).
	Deliverers []Deliverer
	// NotifySettings contributes to the notification settings page.
	NotifySettings *SettingsSection
	Suggest        []SuggestPool
	// Sitemap lists the module's public pages, so a build without the
	// module leaves them out of sitemap.xml.
	Sitemap []SitemapEntry
	// Widgets and NotifyKinds are what the module adds to the home page and
	// to the notifications; the builders and producers stay core's, reading
	// the shared snapshots, but nothing is shown for a module not built in.
	Widgets     []WidgetDecl
	NotifyKinds []NotifyKindDecl
	// BriefingTopics are the briefing line families the module owns;
	// core's briefing builder reads them from this declaration.
	BriefingTopics []BriefingTopicDecl
	// FetchGates are asked before the fetch pipeline spends ESI.
	FetchGates []FetchGate
	// CycleJobs run inside the worker cycle at a named phase.
	CycleJobs []CycleJob
	// AdminSections and SyncSections add blocks to those two pages.
	AdminSections []PageSection
	SyncSections  []PageSection
	// LinkExtras are optional permissions the module offers at sign-up
	// only when its own settings call for them (the shared cache offers
	// the structures permission only on a server that has the cache on).
	// They are never a feature of their own.
	LinkExtras func(deps Deps) []Scope
	// Init runs once the application is built and its store is open; an
	// error stops the start (a module whose settings are wrong says so
	// before serving anything).
	Init func(deps Deps) error
	// Snapshots lists the snapshot kinds (or kind prefixes) the module
	// reads, so the worker and the sync page can tie a stored kind to the
	// module that shows it.
	Snapshots []string
	// SitemapRows adds pages read from the database (the top public
	// fits, for one); called when the sitemap is built.
	SitemapRows func(ctx context.Context, deps Deps) ([]SitemapRow, error)
	Pages       []Page
}
