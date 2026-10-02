package main

import (
	"fmt"
	"net/http"

	"golang.org/x/oauth2"
)

// eveOAuthConfig builds the EVE SSO OAuth2 config from the environment.
// EVE SSO v2 endpoints; scopes are left empty in the skeleton and get
// filled in as ESI features land (one scope per data type).
func eveOAuthConfig(cfg config) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     cfg.eveClientID,
		ClientSecret: cfg.eveClientSecret,
		RedirectURL:  "http://localhost:8080/auth/callback",
		Scopes:       []string{},
		Endpoint: oauth2.Endpoint{
			AuthURL:  "https://login.eveonline.com/v2/oauth/authorize",
			TokenURL: "https://login.eveonline.com/v2/oauth/token",
		},
	}
}

// handleEVELoginStub is the placeholder behind the "Log in with EVE SSO"
// button. The real handler will redirect to conf.AuthCodeURL(state) and
// a /auth/callback handler will exchange the code and store the tokens.
func (app *application) handleEVELoginStub(w http.ResponseWriter, r *http.Request) {
	conf := eveOAuthConfig(app.cfg)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if conf.ClientID == "" {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintln(w, "EVE SSO is not configured yet: set EVE_CLIENT_ID (see .env.example).")
		return
	}
	w.WriteHeader(http.StatusNotImplemented)
	fmt.Fprintln(w, "EVE SSO login is stubbed: the OAuth2 config is ready, the redirect/callback wiring comes next.")
}

// handleDevLogin is a DEV-ONLY stub that marks the session authenticated
// so the admin placeholder can be exercised before EVE SSO lands.
// Remove before deploying anywhere public.
func (app *application) handleDevLogin(w http.ResponseWriter, r *http.Request) {
	app.sessions.Put(r.Context(), "authenticated", true)
	app.sessions.Put(r.Context(), "character_name", "Dev Capsuleer (stub)")
	http.Redirect(w, r, "/admin/", http.StatusSeeOther)
}
