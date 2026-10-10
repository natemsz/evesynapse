package app

import (
	"bytes"
	"context"
	"html/template"
	"net/http"
	"sync"

	"evesynapse/internal/logging"
)

// pageData is the view model shared by the templates.
type pageData struct {
	Version           string // footer product version ("v0.3.00.002"); filled by render
	AssetVersion      string // cache-busting token on stylesheet/script URLs (core_static.go); filled by render
	LoggedIn          bool
	IsAdmin           bool // one of the account's characters is in EVE_ADMIN_CHARACTER_IDS; filled by render
	CharacterName     string
	SSOConfigured     bool
	AutoRefresh       bool   // Sync page: app.js reloads on a timer (noscript meta-refresh fallback)
	NavState          string // saved nav layout ("rail" or "hidden") from the cookie; base.html puts it on <html>; filled by render
	Notice            string // a one-time message from the action that redirected here (see flash); shown above the page content
	Error             string // friendly, user-safe banner (never internals)
	Section           string // top-nav branch key (base.html); filled by render from the page when empty
	NavPage           string // template file rendered, for marking the exact nav link; filled by render
	SyncStatus        string // footer "ESI Health:" value ("OK", "limited"); filled by render
	Shopping          *shoppingView
	Home              *homeView
	CharChars         []assetCharLink
	CharacterPage     *characterView
	Fittings          *fittingsView
	FittingsChars     []assetCharLink
	FitEditor         *fitEditorView
	Killmails         *killmailsView
	KillmailChars     []assetCharLink
	Wallet            *walletView
	WalletChars       []assetCharLink
	Orders            *ordersView
	OrdersChars       []assetCharLink
	Contracts         *contractsView
	ContractsChars    []assetCharLink
	Industry          *industryView
	IndustryChars     []assetCharLink
	Planner           *plannerView
	IntelWars         *warsView
	IntelIncursions   *incursionsView
	IntelFW           *fwView
	ServerStatus      *serverStatusView
	Corps             []corpView
	CorpChars         []assetCharLink
	CorpMembers       *corpMembersView
	CorpWallets       *corpWalletsView
	CorpOrders        *corpOrdersView
	CorpAssets        *corpAssetsView
	CorpStructs       *corpStructuresView
	Assets            *assetsView
	AssetsChars       []assetCharLink
	AssetsSearch      *assetsSearchView
	Planets           *planetsView
	PlanetsChars      []assetCharLink
	Mail              *mailView
	MailChars         []assetCharLink
	MailCompose       *mailComposeView
	Calendar          *calendarView
	CalendarChars     []assetCharLink
	Contacts          *contactsView
	ContactsChars     []assetCharLink
	Market            *marketView
	MarketScanner     *scannerView
	MarketTradefinder *tradefinderView
	MarketLeaderboard *leaderboardView
	Restock           *restockView
	Items             *itemsView
	Skills            *skillsView
	SkillsChars       []assetCharLink
	SkillPlans        *skillPlansView
	Sync              *syncView
	Admin             *adminView

	// Header character switcher (base.html): every character
	// linked to the signed-in account, the acting one marked.
	// Filled by render; handlers never set it.
	Switcher []switcherEntry

	// ViewerChars is the set of the signed-in user's own linked
	// character IDs — the charLink/killCharLink template helpers
	// route own characters to their sheet and everyone else to
	// the public pilot page (or zKillboard in kill contexts).
	// Filled by render from Switcher; handlers never set it.
	ViewerChars map[int64]bool

	// Notify is the top-bar notifications icon and its short list
	// (notify_pages.go). Filled by render; handlers never set it.
	Notify *notifyBadge
	// NotifyPoll is how often, in seconds, the page asks whether the
	// icon has changed (NOTIFY_POLL_SECONDS); 0 means it does not ask.
	// Filled by render.
	NotifyPoll int

	// Notifications is the /notifications/ page.
	Notifications *notificationsView

	// NotifySettings is the /notifications/settings page.
	NotifySettings *notifySettingsView

	// DiscordServers is the /discord/servers page.
	DiscordServers *discordServersView

	// Groups is the /groups page.
	Groups *groupsView

	// SRP is the /srp/ page (corp_srp.go).
	SRP *srpView

	// Doctrines (corp_doctrines.go): the list, one doctrine's page, and
	// the corporation's fits on one page.
	Doctrines *doctrinesView
	Doctrine  *doctrineView
	CorpFits  *corpFitsView

	// PublicFits is the /fittings/public/ page (char_fit_public.go).
	PublicFits *publicFitsView

	// CorpSettings is the /corporations/settings/ page (corp_select.go).
	CorpSettings *corpSettingsView

	// Ops (ops.go): one op's page, and the form that creates or
	// changes one.
	Op     *opView
	OpForm *opFormView
	PAPs   *papsView // the attendance table (/ops/paps)

	// Public pilot page (/pilot/): a stranger's public record.
	Pilot *pilotView

	// Public corporation & alliance pages (/corporation/,
	// /alliance/): an organization's public record.
	Corporation *corporationPageView
	Alliance    *alliancePageView

	// Solar system & station pages (/system/, /station/): an SDE
	// place record.
	System  *systemPageView
	Station *stationPageView

	// Player structure page (/structure/): the local structure
	// record.
	Structure *structurePageView

	// Character management page (/characters/).
	CharactersPage *charactersView
}

// switcherEntry is one linked character in the header switcher.
type switcherEntry struct {
	ID          int64
	Name        string
	Tags        string
	PortraitURL string
	Active      bool
	Relink      bool // token_dead / owner_changed: needs a fresh sign-in
}

// skillRow is one line of the character-page skills table.

// skillRow is one line of the home-page skills table.
type skillRow struct {
	Name    string
	TypeID  int64
	Trained string // trained level, roman
	Active  string // active level, roman
	SP      string // formatted
	// TrainedLvl is the numeric trained level (0-5) driving the
	// in-game 5-box level indicator. NextLvl/NextState mark the
	// level currently training ("training") or sitting in the
	// queue ("queued"); NextState is "" when neither applies.
	TrainedLvl int
	NextLvl    int
	NextState  string
}

// sectionForPage maps a template file to the top-nav branch that
// contains it (see templates/base.html). Handlers whose page lives
// in a different branch than its template suggests — the corp
// killmails view reuses killmails.html — set pageData.Section
// explicitly and render leaves it alone.
func sectionForPage(page string) string {
	switch page {
	case "home.html":
		return "home"
	case "character.html", "characters.html", "skills.html", "skillplans.html",
		"mail.html", "calendar.html", "contacts.html", "pilot.html", "assets.html",
		"killmails.html", "notifications.html", "notification_settings.html", "op.html", "op_form.html", "op_paps.html":
		return "pilot"
	case "fittings.html", "fittings_saved.html", "fit_shopping.html", "fittings_public.html":
		return "fitting"
	case "industry.html", "planner.html", "planets.html":
		return "industry"
	case "market.html", "market_scanner.html", "market_tradefinder.html", "market_leaderboard.html",
		"market_restock.html",
		"wallet.html", "orders.html", "contracts.html", "system.html", "station.html", "structure.html":
		return "market"
	case "items.html":
		return "tools"
	case "corporations.html", "corp_members.html", "corp_wallets.html",
		"corp_orders.html", "corp_assets.html", "corp_structures.html",
		"corporation.html", "alliance.html", "discord_servers.html", "groups.html", "srp.html", "doctrines.html", "doctrine.html", "corp_fits.html", "corp_settings.html":
		return "corporation"
	case "intel_wars.html", "intel_incursions.html", "intel_fw.html":
		return "intel"
	case "sync.html":
		return "sync"
	case "admin.html":
		return "admin"
	}
	return ""
}

// Parsed template sets, one per page file. The templates are embedded
// in the binary and cannot change while it runs, so each set is parsed
// once, on first use, and shared by every request after that (a parsed
// set is safe to execute from many requests at once).
var (
	pageTemplates     sync.Map // page file -> *template.Template: base layout + partials + page
	fragmentTemplates sync.Map // page file -> *template.Template: partials + page (live-region fragments)
)

// pageSharedTemplates are the files every full page is parsed with:
// the layout and the partials any page may use. One list, so that
// nothing else parsing a page into the same cache can leave one out.
var pageSharedTemplates = []string{"templates/base.html", "templates/notifybell.html", "templates/balancechart.html", "templates/charselector.html", "templates/fitbrowser.html", "templates/locked.html"}

// parsedTemplate returns the cached set for page, parsing it (with
// the shared files first) the first time it is asked for.
func parsedTemplate(cache *sync.Map, name, page string, shared ...string) (*template.Template, error) {
	if ts, ok := cache.Load(page); ok {
		return ts.(*template.Template), nil
	}
	files := append(append([]string{}, shared...), "templates/"+page)
	ts, err := template.New(name).Funcs(linkFuncMap()).ParseFS(templatesFS, files...)
	if err != nil {
		return nil, err
	}
	// Two first requests may both parse; one result is kept.
	kept, _ := cache.LoadOrStore(page, ts)
	return kept.(*template.Template), nil
}

func (app *Application) render(ctx context.Context, w http.ResponseWriter, status int, page string, data pageData) {
	if data.Version == "" {
		data.Version = appVersion
	}
	if data.AssetVersion == "" {
		data.AssetVersion = staticAssetVersion()
	}
	if data.Section == "" {
		data.Section = sectionForPage(page)
	}
	if data.NavState == "" {
		data.NavState = navStateFrom(ctx)
	}
	if data.Notice == "" {
		data.Notice = app.sessions.PopString(ctx, sessionFlash)
	}
	if data.NavPage == "" {
		data.NavPage = page
	}
	// Sync status for the footer. Cheap: ESI error budget
	// is in-memory; sweep state is a single cached query.
	if data.SyncStatus == "" {
		data.SyncStatus = app.syncStatusString(ctx)
	}
	// One read of the account's characters serves both the header
	// switcher and the admin check behind the Admin/Sync nav links.
	if data.LoggedIn && (data.Switcher == nil || !data.IsAdmin) {
		characters := app.sessionCharacters(ctx)
		if data.Switcher == nil {
			data.Switcher = app.switcherEntriesFor(ctx, characters)
		}
		if !data.IsAdmin {
			data.IsAdmin = app.adminAmong(characters)
		}
	}
	if data.LoggedIn && data.Notify == nil {
		data.Notify = app.notifyBadgeFor(ctx, app.userID(ctx))
	}
	if data.LoggedIn {
		data.NotifyPoll = app.cfg.notifyPoll
	}
	if data.LoggedIn && data.ViewerChars == nil {
		data.ViewerChars = make(map[int64]bool, len(data.Switcher))
		for _, entry := range data.Switcher {
			data.ViewerChars[entry.ID] = true
		}
	}
	ts, err := parsedTemplate(&pageTemplates, "base", page, pageSharedTemplates...)
	if err != nil {
		logging.Errorf("parse template %s: %v", page, err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	buf := new(bytes.Buffer)
	if err := ts.ExecuteTemplate(buf, "base", data); err != nil {
		logging.Errorf("execute template %s: %v", page, err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

// friendlyLoginError maps the ?error= codes produced by the SSO
// callback to safe, human messages. Unknown codes render nothing — the
// raw parameter is never echoed back to the page.
func friendlyLoginError(code string) string {
	switch code {
	case "denied":
		return "EVE login was cancelled or denied. You can try again whenever you're ready."
	case "state":
		return "That login attempt couldn't be verified — it may have expired. Please try logging in again."
	case "exchange", "verify":
		return "EVE login failed while completing sign-in. Please try again."
	case "save":
		return "You signed in at EVE, but saving your character failed here. Please try again."
	case "unconfigured":
		return "EVE SSO is not configured on this server yet."
	case "notallowed":
		return "This EveSynapse is limited to particular characters, corporations or alliances, and that character isn't among them. If you already have an account here, sign in with a character linked to it."
	case "allowcheck":
		return "EveSynapse couldn't check whether that character may sign up here, because EVE's API didn't answer. Please try again in a minute."
	default:
		return ""
	}
}

// syncStatusString is the footer's "ESI Health:" value: "OK", or
// "limited" when ESI's error budget is low.
func (app *Application) syncStatusString(ctx context.Context) string {
	if app.esi != nil && app.esi.ErrorBudgetLow() {
		return "limited"
	}
	return "OK"
}

func (app *Application) handleHome(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	loggedIn := app.sessions.GetBool(ctx, sessionAuthenticated)
	data := pageData{
		LoggedIn:      loggedIn,
		CharacterName: app.sessions.GetString(ctx, sessionCharacterName),
		SSOConfigured: app.cfg.SSOConfigured(),
	}
	// The ?error= banner explains a failed sign-in attempt; it is
	// only meaningful signed out. A stale or duplicate SSO callback
	// can bounce a signed-in user back here with an error code, and
	// showing "that login attempt couldn't be verified" above a
	// working character sheet is just confusing.
	if !loggedIn {
		data.Error = friendlyLoginError(r.URL.Query().Get("error"))
	}
	if data.LoggedIn {
		data.Home = app.buildHome(ctx, r.URL.Query().Get("customize") == "1")
	}
	// Tranquility status line: from the worker-warmed global
	// store only; absent until the first intel pass lands it.
	if status, ok := app.loadServerStatus(ctx); ok {
		data.ServerStatus = status
	}
	app.render(ctx, w, http.StatusOK, "home.html", data)
}

// page is the data every signed-in page starts from.
func (app *Application) page(ctx context.Context) pageData {
	return pageData{
		LoggedIn:      true,
		CharacterName: app.sessions.GetString(ctx, sessionCharacterName),
		SSOConfigured: app.cfg.SSOConfigured(),
	}
}

// userID is the signed-in account's id; 0 when nobody is signed in.
func (app *Application) userID(ctx context.Context) int64 {
	return int64(app.sessions.GetInt(ctx, sessionUserID))
}
