package main

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"log"
	"net/http"

	db "evesynapse/internal/db/sqlc"
)

// pageData is the view model shared by the templates.
type pageData struct {
	LoggedIn      bool
	CharacterName string
	SSOConfigured bool
	Error         string // friendly, user-safe banner (never internals)
	Character     *characterSheet
	Users         []db.User
	Characters    []db.Character
	WorkerStatus  string
}

// characterSheet is what the home page shows for the signed-in
// character: identity from the session/DB, live data from ESI.
type characterSheet struct {
	Name            string
	PortraitURL     string
	CorporationName string
	Birthday        string // YYYY-MM-DD
	SecurityStatus  float64
	Fetched         bool // true when the live ESI data loaded
	Unavailable     bool // signed in, but ESI couldn't be reached
}

func (app *application) render(w http.ResponseWriter, status int, page string, data pageData) {
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

func (app *application) handleHome(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := pageData{
		LoggedIn:      app.sessions.GetBool(ctx, sessionAuthenticated),
		CharacterName: app.sessions.GetString(ctx, sessionCharacterName),
		SSOConfigured: app.cfg.ssoConfigured(),
		Error:         friendlyLoginError(r.URL.Query().Get("error")),
	}
	if data.LoggedIn {
		data.Character = app.loadCharacterSheet(ctx)
	}
	app.render(w, http.StatusOK, "home.html", data)
}

// loadCharacterSheet assembles the home-page character block.
//
// NOTE: ESI data is fetched live on every page load. The characters
// table has a cached_until column for exactly this, but ESI caching is
// per-endpoint and the sheet will fan out to several endpoints — proper
// caching belongs with the worker's refresh scheduler. Future work.
func (app *application) loadCharacterSheet(ctx context.Context) *characterSheet {
	sheet := &characterSheet{
		Name: app.sessions.GetString(ctx, sessionCharacterName),
	}

	characterID := int64(app.sessions.GetInt(ctx, sessionCharacterID))
	if characterID == 0 {
		// Dev-login or pre-SSO session: name only, nothing to fetch.
		return sheet
	}
	sheet.PortraitURL = fmt.Sprintf("https://images.evetech.net/characters/%d/portrait?size=128", characterID)

	character, err := app.queries.GetCharacter(ctx, characterID)
	if err != nil {
		log.Printf("home: load character %d: %v", characterID, err)
		sheet.Unavailable = true
		return sheet
	}
	if character.Name != "" {
		sheet.Name = character.Name
	}

	var pub esiCharacter
	if err := esiGet(ctx, character.AccessToken, fmt.Sprintf("/characters/%d/", characterID), &pub); err != nil {
		// Most likely cause over time: the stored access token expired
		// (~20 min lifetime). Token refresh via the worker is future
		// work; degrade to identity-only instead of failing the page.
		log.Printf("home: ESI character fetch for %d failed: %v", characterID, err)
		sheet.Unavailable = true
		return sheet
	}
	if pub.Name != "" {
		sheet.Name = pub.Name
	}
	sheet.SecurityStatus = pub.SecurityStatus
	if len(pub.Birthday) >= 10 {
		sheet.Birthday = pub.Birthday[:10]
	}

	// Corporation name is a separate public endpoint; a failure here
	// just leaves the field blank, the rest of the sheet still renders.
	var corp esiCorporation
	if err := esiGet(ctx, "", fmt.Sprintf("/corporations/%d/", pub.CorporationID), &corp); err == nil {
		sheet.CorporationName = corp.Name
	} else {
		log.Printf("home: ESI corporation fetch for %d failed: %v", pub.CorporationID, err)
	}

	sheet.Fetched = true
	return sheet
}

func (app *application) handleAdmin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := pageData{
		LoggedIn:      true,
		CharacterName: app.sessions.GetString(ctx, sessionCharacterName),
		SSOConfigured: app.cfg.ssoConfigured(),
		// Placeholder until the worker reports real job state.
		WorkerStatus: "heartbeat running (see server log)",
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

	app.render(w, http.StatusOK, "admin.html", data)
}
