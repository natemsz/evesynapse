package module

import (
	"context"
	"database/sql"
	"html/template"
	"net/http"
	"net/url"

	"github.com/alexedwards/scs/v2"

	"github.com/go-chi/chi/v5"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
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

// Widget represents a home dashboard widget contribution.
type Widget struct {
	ID    string
	Title string
}

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
	NotifySettings any
	NavLinks       map[string][]NavLink
	SectionTabs    []NavLink
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
	LoadCorpSnapshot(ctx context.Context, characterID int64, kind string, out any) bool
	PINotEnabled(ctx context.Context, ch db.Character) bool
	PublicShell(ctx context.Context) ShellData
	PickCharacter(ctx context.Context, r *http.Request, path string) ([]db.Character, db.Character, []CharLink, error)
	Render(ctx context.Context, w http.ResponseWriter, status int, page string, data any)
	AnonPages() func(http.Handler) http.Handler
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
	Nav           []NavSection
	Routes        func(r chi.Router, deps Deps)
	WorkerJobs    []WorkerJob
	// Deliverers are the module's notification channels (see Deliverer).
	Deliverers []Deliverer
	// NotifySettings contributes to the notification settings page.
	NotifySettings *SettingsSection
	Widgets        []Widget
	Suggest        []SuggestPool
	// Sitemap lists the module's public pages, so a build without the
	// module leaves them out of sitemap.xml.
	Sitemap []SitemapEntry
	Pages   []Page
}
