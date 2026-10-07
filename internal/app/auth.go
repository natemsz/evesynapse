package app

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/oauth2"
)

// Session keys. Values stored via scs are gob-encoded; keep them typed
// consistently (ints for IDs so GetInt works).
const (
	sessionAuthenticated = "authenticated"
	sessionUserID        = "user_id"
	sessionCharacterID   = "character_id"
	sessionCharacterName = "character_name"
	sessionOAuthState    = "oauth_state"
)

// sessionLifetime is how long a sign-in lasts. The session slides
// (see slideSession), so the 30 days only run out after a month
// away, not a day after signing in.
const sessionLifetime = 30 * 24 * time.Hour

// slideSession keeps signed-in sessions alive while the user is
// active: once more than a day of the session's lifetime has
// elapsed, the token is renewed, pushing the deadline back out to
// a full sessionLifetime. Renewing also rotates the cookie token,
// which is why it happens at most once a day rather than on every
// request. The renewed token's cookie is written by LoadAndSave
// when it commits the session at the end of the request.
func (app *Application) slideSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if app.sessions.GetBool(ctx, sessionAuthenticated) {
			if deadline := app.sessions.Deadline(ctx); !deadline.IsZero() &&
				time.Until(deadline) < app.sessions.Lifetime-24*time.Hour {
				if err := app.sessions.RenewToken(ctx); err != nil {
					log.Printf("session: sliding renewal: %v", err)
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// https://login.eveonline.com/.well-known/oauth-authorization-server
const (
	eveIssuer       = "https://login.eveonline.com" // required `iss` claim
	eveDiscoveryURL = "https://login.eveonline.com/.well-known/oauth-authorization-server"
)

// eveScopes is the single source of truth for the scopes requested at
// login: the full read ESI scope set plus two write scopes. The list is
// generated from the OAuth2 scope catalog in CCP's ESI OpenAPI document
// (GET https://esi.evetech.net/meta/openapi.json →
// components.securitySchemes), minus every mutating scope — anything
// whose name contains write_, send_, respond_, organize_, manage_ or
// open_window (5 scopes excluded: respond_calendar_events,
// write_contacts, write_fleet,
// open_window, write_waypoint; esi-planets.manage_planets.v1
// is the one deliberate manage exception — see below). The user's
// developer-portal application has the same scopes enabled, so
// SSO grants exactly what is requested here. Already-linked
// characters keep their previously granted scopes until re-linked.
//
// One deliberate write exception: esi-fittings.write_fittings.v1
// IS requested, powering the fitting editor's "Save to EVE" button
// (POST /characters/{id}/fittings/). Characters linked before this
// scope was added get a 403 on that endpoint, which the handler
// reports as "please sign in again" instead of failing silently.
//
// One deliberate exception (Phase 2): esi-planets.manage_planets.v1
// IS requested. CCP publishes no read scope for planetary industry —
// manage_planets is the only scope gating the two colony GETs
// (/characters/{id}/planets and /characters/{id}/planets/{planet_id}),
// and no write endpoint hangs off it in the current ESI surface.
// The app only ever GETs colonies; EVE's consent screen still words
// the grant "manage your planetary installations". Characters linked
// before this scope was added get a 403 on the colonies endpoint,
// which the worker records as "PI not enabled" until they re-link
// (see planets_worker.go).
var eveScopes = []string{
	"esi-alliances.read_contacts.v1",
	"esi-assets.read_assets.v1",
	"esi-assets.read_corporation_assets.v1",
	"esi-calendar.read_calendar_events.v1",
	"esi-characters.read_agents_research.v1",
	"esi-characters.read_blueprints.v1",
	"esi-characters.read_contacts.v1",
	"esi-characters.read_corporation_roles.v1",
	"esi-characters.read_fatigue.v1",
	"esi-characters.read_fw_stats.v1",
	"esi-characters.read_loyalty.v1",
	"esi-characters.read_medals.v1",
	"esi-characters.read_notifications.v1",
	"esi-characters.read_standings.v1",
	"esi-characters.read_titles.v1",
	"esi-clones.read_clones.v1",
	"esi-clones.read_implants.v1",
	"esi-contracts.read_character_contracts.v1",
	"esi-contracts.read_corporation_contracts.v1",
	"esi-corporations.read_blueprints.v1",
	"esi-corporations.read_contacts.v1",
	"esi-corporations.read_container_logs.v1",
	"esi-corporations.read_corporation_membership.v1",
	"esi-corporations.read_divisions.v1",
	"esi-corporations.read_facilities.v1",
	"esi-corporations.read_fw_stats.v1",
	"esi-corporations.read_medals.v1",
	"esi-corporations.read_standings.v1",
	"esi-corporations.read_starbases.v1",
	"esi-corporations.read_structures.v1",
	"esi-corporations.read_titles.v1",
	"esi-corporations.track_members.v1",
	"esi-fittings.read_fittings.v1",
	"esi-fittings.write_fittings.v1",
	"esi-fleets.read_fleet.v1",
	"esi-industry.read_character_jobs.v1",
	"esi-industry.read_character_mining.v1",
	"esi-industry.read_corporation_jobs.v1",
	"esi-industry.read_corporation_mining.v1",
	"esi-killmails.read_corporation_killmails.v1",
	"esi-killmails.read_killmails.v1",
	"esi-location.read_location.v1",
	"esi-location.read_online.v1",
	"esi-location.read_ship_type.v1",
	"esi-mail.read_mail.v1",
	// Issue 26: marking mail read needs organize_mail (PUT
	// /characters/{id}/mail/{mail_id}/). Characters linked before
	// this scope was added get a 403, reported as "sign in again".
	"esi-mail.organize_mail.v1",
	// Issue 27: composing/sending mail needs send_mail (POST
	// /characters/{id}/mail/). Same re-link note as above.
	"esi-mail.send_mail.v1",
	"esi-markets.read_character_orders.v1",
	"esi-markets.read_corporation_orders.v1",
	"esi-markets.structure_markets.v1",
	// The only "manage_" scope requested — see the eveScopes
	// comment: it gates the colony GETs and nothing else.
	"esi-planets.manage_planets.v1",
	"esi-planets.read_customs_offices.v1",
	"esi-search.search_structures.v1",
	"esi-skills.read_skillqueue.v1",
	"esi-skills.read_skills.v1",
	"esi-universe.read_structures.v1",
	"esi-wallet.read_character_wallet.v1",
	"esi-wallet.read_corporation_wallets.v1",
	"esi.activity.char:read",
	"esi.cosmetic.char:read",
}

// loginHTTPClient is shared by the SSO/JWKS calls (auth.go) and ESI.
var loginHTTPClient = &http.Client{Timeout: 10 * time.Second}

// eveOAuthConfig builds the EVE SSO OAuth2 config. The redirect URL must
// match the callback registered for the app at developers.eveonline.com
// character-for-character.
func eveOAuthConfig(cfg Config) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     cfg.eveClientID,
		ClientSecret: cfg.eveClientSecret,
		RedirectURL:  cfg.eveCallbackURL,
		Scopes:       eveScopes,
		Endpoint: oauth2.Endpoint{
			AuthURL:  "https://login.eveonline.com/v2/oauth/authorize",
			TokenURL: "https://login.eveonline.com/v2/oauth/token",
		},
	}
}

// handleEVELogin starts the EVE SSO flow: it stores a random state in
// the session and redirects to CCP's authorize page. Also used by the
// "Link another character" button — the callback attaches the character
// to whichever account the session already holds.
func (app *Application) handleEVELogin(w http.ResponseWriter, r *http.Request) {
	if !app.cfg.SSOConfigured() {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintln(w, "EVE SSO is not configured yet: set EVE_CLIENT_ID / EVE_CLIENT_SECRET (see .env.example).")
		return
	}

	state, err := newOAuthState()
	if err != nil {
		log.Printf("sso: generate state: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	app.sessions.Put(r.Context(), sessionOAuthState, state)
	http.Redirect(w, r, eveOAuthConfig(app.cfg).AuthCodeURL(state), http.StatusFound)
}

// newOAuthState returns 32 random bytes hex-encoded (64 chars).
func newOAuthState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// handleEVECallback completes the flow: state check, code exchange,
// JWT verification against CCP's JWKS, then account/character
// persistence and session sign-in. Failures bounce home with a short
// ?error= code that the home page turns into a friendly message; the
// real error goes to the server log only. Tokens, codes and the client
// secret are never logged and never shown to the browser.
func (app *Application) handleEVECallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()

	fail := func(code string) {
		http.Redirect(w, r, "/?error="+code, http.StatusSeeOther)
	}

	// CCP-side failure (e.g. the player clicked Cancel: error=
	// access_denied). Only the error code is logged, never the code.
	if cerr := q.Get("error"); cerr != "" {
		log.Printf("sso callback: CCP returned error=%s", cerr)
		fail("denied")
		return
	}
	if !app.cfg.SSOConfigured() {
		fail("unconfigured")
		return
	}

	// State is single-use: read it, clear it, then constant-time compare.
	want := app.sessions.GetString(ctx, sessionOAuthState)
	app.sessions.Remove(ctx, sessionOAuthState)
	got := q.Get("state")
	if want == "" || got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		log.Printf("sso callback: state mismatch")
		fail("state")
		return
	}

	code := q.Get("code")
	if code == "" {
		log.Printf("sso callback: missing authorization code")
		fail("denied")
		return
	}

	token, err := eveOAuthConfig(app.cfg).Exchange(ctx, code)
	if err != nil {
		log.Printf("sso callback: token exchange failed: %v", err)
		fail("exchange")
		return
	}
	// From here on `token` holds the access/refresh tokens: never log it.

	characterID, characterName, grantedScopes, ownerHash, err := app.verifyAccessToken(ctx, token.AccessToken)
	if err != nil {
		log.Printf("sso callback: access token verification failed: %v", err)
		fail("verify")
		return
	}

	// Land on the account this sign-in belongs to: the session's
	// account when it holds one ("link another character"), else
	// the account the character is already linked to, else a fresh
	// account — for a first-ever sign-in, and for a character that
	// changed EVE accounts since it was linked (its new owner must
	// never inherit the previous owner's account).
	userID, err := app.resolveSignInUser(ctx, int64(app.sessions.GetInt(ctx, sessionUserID)), characterID, ownerHash)
	if err != nil {
		switch {
		case errors.Is(err, errSignUpNotAllowed):
			log.Printf("sso callback: character %d (%s) is not on this instance's sign-up lists; no account created", characterID, characterName)
			fail("notallowed")
		case errors.Is(err, errSignUpUnverified):
			log.Printf("sso callback: sign-up check for character %d: %v", characterID, err)
			fail("allowcheck")
		default:
			log.Printf("sso callback: resolve account for character %d: %v", characterID, err)
			fail("save")
		}
		return
	}

	expiry := ""
	if !token.Expiry.IsZero() {
		expiry = token.Expiry.UTC().Format(time.RFC3339)
	}
	result, err := app.linkVerifiedCharacter(ctx, linkCharacterInput{
		UserID:       userID,
		CharacterID:  characterID,
		Name:         characterName,
		Scopes:       grantedScopes,
		OwnerHash:    ownerHash,
		AccessToken:  token.AccessToken,
		RefreshToken: token.RefreshToken,
		TokenExpiry:  sql.NullString{String: expiry, Valid: expiry != ""},
	})
	if err != nil {
		log.Printf("sso callback: store character %d: %v", characterID, err)
		fail("save")
		return
	}
	switch {
	case result.OwnerChanged && result.Moved:
		log.Printf("sso: character %d changed EVE accounts since it was linked; moved off its previous account to user %d", characterID, userID)
	case result.OwnerChanged:
		log.Printf("sso: character %d owner hash changed since the link was verified; flagged for re-verification", characterID)
	case result.Moved:
		log.Printf("sso: character %d moved to user %d (already linked elsewhere; fresh sign-in wins)", characterID, userID)
	}

	log.Printf("sso: signed in character %d (%s) on user %d", characterID, characterName, userID)

	// Warm this character first on the next worker cycle so its
	// pages are ready moments after login, not minutes later.
	app.markCharacterPriority(characterID)

	// Rotate the session token on privilege change, then sign in.
	if err := app.sessions.RenewToken(ctx); err != nil {
		log.Printf("sso callback: renew session token: %v", err)
	}
	app.sessions.Put(ctx, sessionAuthenticated, true)
	app.sessions.Put(ctx, sessionUserID, int(userID))
	app.sessions.Put(ctx, sessionCharacterID, int(characterID))
	app.sessions.Put(ctx, sessionCharacterName, characterName)

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleSignOut destroys the session server-side and expires the cookie.
func (app *Application) handleSignOut(w http.ResponseWriter, r *http.Request) {
	if err := app.sessions.Destroy(r.Context()); err != nil {
		log.Printf("sign out: destroy session: %v", err)
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// DevLoginEnabled reports whether the config opted into the
// dev-only sign-in stub (DEV_LOGIN=1). The /dev-login route itself
// lives in internal/devtools and is only registered by the dev
// entrypoint (cmd/evesynapse-dev); the release binary never
// registers it.
func (app *Application) DevLoginEnabled() bool {
	return app.cfg.devLogin
}

// DevSignIn marks the session authenticated as the dev stub user,
// without EVE SSO. Called only by the dev-only /dev-login route
// (see internal/devtools), which hands a signed-in session to
// anyone who asks — NEVER enable it on a deployment anyone else
// can reach.
func (app *Application) DevSignIn(ctx context.Context) {
	app.sessions.Put(ctx, sessionAuthenticated, true)
	app.sessions.Put(ctx, sessionCharacterName, "Dev Capsuleer (stub)")
}

// ---------------------------------------------------------------------------
// Access-token (JWT) verification against CCP's published keys.
// ---------------------------------------------------------------------------

// jwksCache lazily resolves CCP's JWKS URI from the discovery document
// and caches the RSA public keys by kid, refreshing when an unknown kid
// shows up (key rotation).
type jwksCache struct {
	mu      sync.Mutex
	jwksURI string
	keys    map[string]*rsa.PublicKey
}

func (c *jwksCache) keyFor(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if key, ok := c.keys[kid]; ok {
		return key, nil
	}
	if err := c.refreshLocked(ctx); err != nil {
		return nil, err
	}
	key, ok := c.keys[kid]
	if !ok {
		return nil, fmt.Errorf("no JWKS key for kid %q", kid)
	}
	return key, nil
}

// refreshLocked fetches the discovery document (once) and the JWKS.
// Callers must hold c.mu.
func (c *jwksCache) refreshLocked(ctx context.Context) error {
	if c.jwksURI == "" {
		uri, err := fetchJWKSURI(ctx)
		if err != nil {
			return err
		}
		c.jwksURI = uri
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.jwksURI, nil)
	if err != nil {
		return err
	}
	resp, err := loginHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetch JWKS: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch JWKS: status %d", resp.StatusCode)
	}

	var set struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
		return fmt.Errorf("decode JWKS: %w", err)
	}

	keys := make(map[string]*rsa.PublicKey, len(set.Keys))
	for _, k := range set.Keys {
		if k.Kty != "RSA" {
			continue
		}
		pub, err := publicKeyFromJWK(k.N, k.E)
		if err != nil {
			return fmt.Errorf("parse JWKS key %q: %w", k.Kid, err)
		}
		keys[k.Kid] = pub
	}
	if len(keys) == 0 {
		return errors.New("JWKS contained no RSA keys")
	}
	c.keys = keys
	return nil
}

func fetchJWKSURI(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, eveDiscoveryURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := loginHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch discovery document: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch discovery document: status %d", resp.StatusCode)
	}
	var doc struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return "", fmt.Errorf("decode discovery document: %w", err)
	}
	if doc.JWKSURI == "" {
		return "", errors.New("discovery document has no jwks_uri")
	}
	return doc.JWKSURI, nil
}

// publicKeyFromJWK builds an RSA public key from the base64url modulus
// and exponent of a JWK.
func publicKeyFromJWK(nB64, eB64 string) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(nB64)
	if err != nil {
		return nil, fmt.Errorf("decode modulus: %w", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(eB64)
	if err != nil {
		return nil, fmt.Errorf("decode exponent: %w", err)
	}
	eBig := new(big.Int).SetBytes(eBytes)
	if !eBig.IsInt64() || eBig.Int64() <= 0 || eBig.Int64() > (1<<31) {
		return nil, errors.New("exponent out of range")
	}
	return &rsa.PublicKey{
		N: new(big.Int).SetBytes(nBytes),
		E: int(eBig.Int64()),
	}, nil
}

// verifyAccessToken validates an EVE SSO access token (RS256 JWT)
// against CCP's JWKS and extracts the character it belongs to. It
// returns the character ID, the character name from the `name` claim,
// the granted scopes (from the `scp` claim, falling back to the
// requested scopes), and the character owner hash from the `owner`
// claim (empty when CCP didn't send one).
func (app *Application) verifyAccessToken(ctx context.Context, accessToken string) (characterID int64, characterName, scopes, ownerHash string, err error) {
	claims := jwt.MapClaims{}
	_, err = jwt.ParseWithClaims(accessToken, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("unexpected signing method %v", t.Header["alg"])
		}
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, errors.New("token header has no kid")
		}
		return app.jwks.keyFor(ctx, kid)
	}, jwt.WithValidMethods([]string{"RS256"}))
	if err != nil {
		return 0, "", "", "", err
	}

	iss, err := claims.GetIssuer()
	if err != nil || iss != eveIssuer {
		return 0, "", "", "", fmt.Errorf("unexpected issuer %q", iss)
	}

	sub, err := claims.GetSubject()
	if err != nil {
		return 0, "", "", "", fmt.Errorf("read sub claim: %w", err)
	}
	// sub looks like "CHARACTER:EVE:123456789".
	parts := strings.Split(sub, ":")
	if len(parts) != 3 || parts[0] != "CHARACTER" || parts[1] != "EVE" {
		return 0, "", "", "", fmt.Errorf("unexpected sub claim format")
	}
	characterID, err = strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return 0, "", "", "", fmt.Errorf("parse character id from sub: %w", err)
	}

	characterName, _ = claims["name"].(string)
	// The owner hash identifies the EVE account currently owning
	// the character; it changes when the character is transferred.
	ownerHash, _ = claims["owner"].(string)

	scopes = strings.Join(eveScopes, " ")
	if scp, ok := claims["scp"].([]any); ok && len(scp) > 0 {
		granted := make([]string, 0, len(scp))
		for _, s := range scp {
			if str, ok := s.(string); ok {
				granted = append(granted, str)
			}
		}
		if len(granted) > 0 {
			scopes = strings.Join(granted, " ")
		}
	}
	return characterID, characterName, scopes, ownerHash, nil
}
