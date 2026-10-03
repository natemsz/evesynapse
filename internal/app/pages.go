package app

import (
	"bytes"
	"context"
	"html/template"
	"log"
	"net/http"

	db "evesynapse/internal/db/sqlc"
)

// pageData is the view model shared by the templates.
type pageData struct {
	LoggedIn        bool
	CharacterName   string
	SSOConfigured   bool
	AutoRefresh     bool   // base.html emits a meta-refresh (Sync page)
	Error           string // friendly, user-safe banner (never internals)
	Section         string // top-nav branch key (base.html); filled by render from the page when empty
	NavPage         string // template file rendered, for marking the exact nav link; filled by render
	Home            *homeView
	CharChars       []assetCharLink
	CharacterPage   *characterView
	Fittings        *fittingsView
	FittingsChars   []assetCharLink
	Killmails       *killmailsView
	KillmailChars   []assetCharLink
	Wallet          *walletView
	WalletChars     []assetCharLink
	Orders          *ordersView
	OrdersChars     []assetCharLink
	Contracts       *contractsView
	ContractsChars  []assetCharLink
	Industry        *industryView
	IndustryChars   []assetCharLink
	IntelWars       *warsView
	IntelIncursions *incursionsView
	IntelFW         *fwView
	ServerStatus    *serverStatusView
	Corps           []corpView
	CorpChars       []assetCharLink
	CorpMembers     *corpMembersView
	CorpWallets     *corpWalletsView
	CorpOrders      *corpOrdersView
	CorpAssets      *corpAssetsView
	CorpStructs     *corpStructuresView
	Assets          *assetsView
	AssetsChars     []assetCharLink
	Planets         *planetsView
	PlanetsChars    []assetCharLink
	Mail            *mailView
	MailChars       []assetCharLink
	Calendar        *calendarView
	CalendarChars   []assetCharLink
	Contacts        *contactsView
	ContactsChars   []assetCharLink
	Market          *marketView
	Items           *itemsView
	Skills          *skillsView
	SkillsChars     []assetCharLink
	Sync            *syncView
	Users           []db.User
	Characters      []db.Character
	Snapshots       []adminSnapshotRow
	WorkerStatus    string

	// Header character switcher (base.html): every character
	// linked to the signed-in account, the acting one marked.
	// Filled by render; handlers never set it.
	Switcher []switcherEntry

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
	Trained string // trained level, roman
	Active  string // active level, roman
	SP      string // formatted
}

// adminSnapshotRow is one line of the admin snapshots overview.
type adminSnapshotRow struct {
	CharacterID   int64
	CharacterName string
	Kind          string
	FetchedAt     string
	CachedUntil   string // "—" when unset
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
	case "character.html", "skills.html", "fittings.html", "killmails.html", "characters.html",
		"mail.html", "calendar.html", "contacts.html":
		return "character"
	case "assets.html", "industry.html", "planets.html":
		return "assets"
	case "market.html", "wallet.html", "orders.html", "contracts.html", "items.html":
		return "economy"
	case "corporations.html", "corp_members.html", "corp_wallets.html",
		"corp_orders.html", "corp_assets.html", "corp_structures.html":
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

func (app *Application) render(ctx context.Context, w http.ResponseWriter, status int, page string, data pageData) {
	if data.Section == "" {
		data.Section = sectionForPage(page)
	}
	if data.NavPage == "" {
		data.NavPage = page
	}
	if data.LoggedIn && data.Switcher == nil {
		data.Switcher = app.switcherEntries(ctx)
	}
	ts, err := template.New("base").ParseFS(templatesFS, "templates/base.html", "templates/"+page)
	if err != nil {
		log.Printf("parse template %s: %v", page, err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	buf := new(bytes.Buffer)
	if err := ts.ExecuteTemplate(buf, "base", data); err != nil {
		log.Printf("execute template %s: %v", page, err)
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
	default:
		return ""
	}
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

func (app *Application) handleAdmin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := pageData{
		LoggedIn:      true,
		CharacterName: app.sessions.GetString(ctx, sessionCharacterName),
		SSOConfigured: app.cfg.SSOConfigured(),
		WorkerStatus:  app.workerStatusText(),
	}

	users, err := app.queries.ListUsers(ctx)
	if err != nil {
		log.Printf("admin: list users: %v", err)
		data.Error = "Could not load admin data; check the server log."
	} else {
		data.Users = users
	}

	characters, err := app.queries.ListAllCharacters(ctx)
	if err != nil {
		log.Printf("admin: list characters: %v", err)
		data.Error = "Could not load admin data; check the server log."
	} else {
		data.Characters = characters
	}

	// Snapshot cache overview: per character, which ESI kinds are
	// cached and when each expires.
	for _, ch := range data.Characters {
		snaps, err := app.queries.ListSnapshotsByCharacter(ctx, ch.CharacterID)
		if err != nil {
			log.Printf("admin: list snapshots for character %d: %v", ch.CharacterID, err)
			continue
		}
		for _, snap := range snaps {
			until := "—"
			if snap.CachedUntil.Valid && snap.CachedUntil.String != "" {
				until = snap.CachedUntil.String
			}
			data.Snapshots = append(data.Snapshots, adminSnapshotRow{
				CharacterID:   ch.CharacterID,
				CharacterName: ch.Name,
				Kind:          snap.Kind,
				FetchedAt:     snap.FetchedAt,
				CachedUntil:   until,
			})
		}
	}

	app.render(ctx, w, http.StatusOK, "admin.html", data)
}
