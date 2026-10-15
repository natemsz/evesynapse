package module

import (
	"context"
	"html/template"
	"net/http"

	"github.com/go-chi/chi/v5"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// NavEntry is one page in a section's nav: its link, its label, and
// the template files that count as being on it.
type NavEntry struct {
	Href  string
	Label string
	Pages []string
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

// WorkerJob represents a named pass for the worker cycle.
type WorkerJob struct {
	Name string
}

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
	Widgets       []Widget
	Suggest       []SuggestPool
	// Sitemap lists the module's public pages, so a build without the
	// module leaves them out of sitemap.xml.
	Sitemap []SitemapEntry
	Pages   []Page
}
