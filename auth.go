package main

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

	db "evesynapse/internal/db/sqlc"
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

// EVE SSO (OAuth2 + OpenID Connect) endpoints. Discovery document:
// https://login.eveonline.com/.well-known/oauth-authorization-server
const (
	eveIssuer       = "login.eveonline.com" // required `iss` claim
	eveDiscoveryURL = "https://login.eveonline.com/.well-known/oauth-authorization-server"
)

// eveScopes is the single source of truth for the scopes requested at
// login — one per ESI data type EveSynapse reads.
var eveScopes = []string{
	"esi-skills.read_skills_v1",
	"esi-skills.read_skillqueue_v1",
	"esi-wallet.read_character_wallet_v1",
	"esi-assets.read_assets_v1",
}

// loginHTTPClient is shared by the SSO/JWKS calls (auth.go) and ESI.
var loginHTTPClient = &http.Client{Timeout: 10 * time.Second}

// eveOAuthConfig builds the EVE SSO OAuth2 config. The redirect URL must
// match the callback registered for the app at developers.eveonline.com
// character-for-character.
func eveOAuthConfig(cfg config) *oauth2.Config {
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
func (app *application) handleEVELogin(w http.ResponseWriter, r *http.Request) {
	if !app.cfg.ssoConfigured() {
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
func (app *application) handleEVECallback(w http.ResponseWriter, r *http.Request) {
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
	if !app.cfg.ssoConfigured() {
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

	characterID, characterName, grantedScopes, err := app.verifyAccessToken(ctx, token.AccessToken)
	if err != nil {
		log.Printf("sso callback: access token verification failed: %v", err)
		fail("verify")
		return
	}

	// Attach to the account this session already holds; create one on
	// first login. ("Link another character" reuses the same path.)
	userID := int64(app.sessions.GetInt(ctx, sessionUserID))
	if userID == 0 {
		user, err := app.queries.CreateUser(ctx)
		if err != nil {
			log.Printf("sso callback: create user: %v", err)
			fail("save")
			return
		}
		userID = user.ID
	}

	expiry := ""
	if !token.Expiry.IsZero() {
		expiry = token.Expiry.UTC().Format(time.RFC3339)
	}
	if _, err := app.queries.UpsertCharacter(ctx, db.UpsertCharacterParams{
		CharacterID:  characterID,
		UserID:       userID,
		Name:         characterName,
		AccessToken:  token.AccessToken,
		RefreshToken: token.RefreshToken,
		TokenExpiry:  sql.NullString{String: expiry, Valid: expiry != ""},
		Scopes:       grantedScopes,
		// CachedUntil stays NULL for now: ESI caching is per-endpoint,
		// while this column models the (single) character-sheet cache.
		// The live-per-page-load sheet fetch doesn't populate it yet.
	}); err != nil {
		log.Printf("sso callback: store character %d: %v", characterID, err)
		fail("save")
		return
	}

	log.Printf("sso: signed in character %d (%s) on user %d", characterID, characterName, userID)

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
func (app *application) handleSignOut(w http.ResponseWriter, r *http.Request) {
	if err := app.sessions.Destroy(r.Context()); err != nil {
		log.Printf("sign out: destroy session: %v", err)
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleDevLogin is a DEV-ONLY stub that marks the session authenticated
// so the admin can be exercised without EVE SSO.
//
// !!! The /dev-login route is ONLY registered when DEV_LOGIN=1 (see
// routes() in main.go). It hands a signed-in session to anyone who asks
// — NEVER enable it on a deployment anyone else can reach. !!!
func (app *application) handleDevLogin(w http.ResponseWriter, r *http.Request) {
	app.sessions.Put(r.Context(), sessionAuthenticated, true)
	app.sessions.Put(r.Context(), sessionCharacterName, "Dev Capsuleer (stub)")
	http.Redirect(w, r, "/admin/", http.StatusSeeOther)
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
// and the granted scopes (from the `scp` claim, falling back to the
// requested scopes).
func (app *application) verifyAccessToken(ctx context.Context, accessToken string) (characterID int64, characterName, scopes string, err error) {
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
		return 0, "", "", err
	}

	iss, err := claims.GetIssuer()
	if err != nil || iss != eveIssuer {
		return 0, "", "", fmt.Errorf("unexpected issuer %q", iss)
	}

	sub, err := claims.GetSubject()
	if err != nil {
		return 0, "", "", fmt.Errorf("read sub claim: %w", err)
	}
	// sub looks like "CHARACTER:EVE:123456789".
	parts := strings.Split(sub, ":")
	if len(parts) != 3 || parts[0] != "CHARACTER" || parts[1] != "EVE" {
		return 0, "", "", fmt.Errorf("unexpected sub claim format")
	}
	characterID, err = strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return 0, "", "", fmt.Errorf("parse character id from sub: %w", err)
	}

	characterName, _ = claims["name"].(string)

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
	return characterID, characterName, scopes, nil
}
