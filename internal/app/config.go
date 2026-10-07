package app

import (
	"bufio"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// Config holds runtime configuration. Everything comes from the
// environment (after loadDotEnv has had a chance to fill gaps from a
// local .env); secrets are never hardcoded or logged.
type Config struct {
	addr            string         // listen address
	databaseURL     string         // Postgres connection URL (DATABASE_URL)
	eveClientID     string         // EVE SSO application client ID
	eveClientSecret string         // EVE SSO application client secret
	eveCallbackURL  string         // OAuth2 redirect URI registered with CCP
	sessionKey      string         // reserved for cookie signing hardening
	devLogin        bool           // DEV_LOGIN=1: register the /dev-login route
	sdeBaseURL      string         // EVE SDE CSV dump base URL (Fuzzwork by default)
	adminCharIDs    map[int64]bool // EVE_ADMIN_CHARACTER_IDS (comma-separated)
	tokenKey        string         // TOKEN_ENCRYPTION_KEY: encrypts stored EVE tokens ("" = stored as they are)
	signUp          signUpPolicy   // EVE_ALLOWED_*_IDS: who may create an account (empty = anyone)
}

// SSOConfigured reports whether EVE SSO can run: it needs both the
// client ID (login redirect) and the secret (token exchange).
func (c Config) SSOConfigured() bool {
	return c.eveClientID != "" && c.eveClientSecret != ""
}

// Addr returns the configured HTTP listen address.
func (c Config) Addr() string { return c.addr }

// DatabaseURL returns the Postgres connection URL the app opens.
func (c Config) DatabaseURL() string { return c.databaseURL }

// DatabaseLabel is the log-safe name of the database target:
// scheme, host, and database name with any credentials removed,
// so startup logs can say where the app connected without ever
// printing the password out of DATABASE_URL.
func (c Config) DatabaseLabel() string {
	u := c.databaseURL
	if i := strings.Index(u, "://"); i >= 0 {
		rest := u[i+3:]
		if j := strings.LastIndex(rest, "@"); j >= 0 {
			rest = rest[j+1:]
		}
		if j := strings.Index(rest, "?"); j >= 0 {
			rest = rest[:j]
		}
		return "postgres://" + rest
	}
	return "postgres"
}

// publicOrigin returns the scheme://host[:port] the site is served
// at. The one place the operator states the site's public address
// is EVE_CALLBACK_URL — it has to match what is registered with
// CCP character for character — so that is where it is read from.
// "" when the URL cannot be read as an http(s) address.
func (c Config) publicOrigin() string {
	u, err := url.Parse(c.eveCallbackURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// servedOverTLS reports whether the site's public address is https.
func (c Config) servedOverTLS() bool {
	return strings.HasPrefix(c.publicOrigin(), "https://")
}

// publicHostIsLocal reports whether the site's public address is
// this machine itself (local development), where plain http never
// leaves it.
func (c Config) publicHostIsLocal() bool {
	u, err := url.Parse(c.eveCallbackURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// defaultSDEBaseURL is Fuzzwork's community SDE conversion, CSV
// tables under /dump/latest/csv/ (verified live 2026-10-02; the
// dump previously lived directly under /dump/latest/ as .csv.bz2).
const defaultSDEBaseURL = "https://www.fuzzwork.co.uk/dump/latest/csv/"

// SDEBaseURL returns the base URL the SDE importer downloads the
// CSV tables from, always with a trailing slash.
func (c Config) SDEBaseURL() string {
	base := c.sdeBaseURL
	if base == "" {
		base = defaultSDEBaseURL
	}
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	return base
}

// loadConfig loads ./.env (if present) and then reads the environment.
func LoadConfig() Config {
	loadDotEnv(".env")
	return Config{
		addr:            getenvDefault("ADDR", ":8080"),
		databaseURL:     getenvDefault("DATABASE_URL", "postgres://evesynapse@localhost:5432/evesynapse?sslmode=disable"),
		eveClientID:     os.Getenv("EVE_CLIENT_ID"),
		eveClientSecret: os.Getenv("EVE_CLIENT_SECRET"),
		eveCallbackURL:  getenvDefault("EVE_CALLBACK_URL", "http://localhost:8080/auth/callback"),
		sessionKey:      os.Getenv("SESSION_KEY"),
		devLogin:        os.Getenv("DEV_LOGIN") == "1",
		sdeBaseURL:      getenvDefault("EVE_SDE_BASE_URL", defaultSDEBaseURL),
		adminCharIDs:    parseAdminCharIDs(os.Getenv("EVE_ADMIN_CHARACTER_IDS")),
		tokenKey:        os.Getenv("TOKEN_ENCRYPTION_KEY"),
		signUp:          loadSignUpPolicy(os.Getenv),
	}
}

// signUpPolicy is who may create an account on this instance. All
// three lists empty means anyone who can reach the site and sign in
// with EVE; otherwise a new account needs a character that is
// listed itself, or whose corporation or alliance is.
type signUpPolicy struct {
	characterIDs   map[int64]bool // EVE_ALLOWED_CHARACTER_IDS
	corporationIDs map[int64]bool // EVE_ALLOWED_CORPORATION_IDS
	allianceIDs    map[int64]bool // EVE_ALLOWED_ALLIANCE_IDS
	// err is a list that was set but could not be read. A
	// restriction the operator asked for must never quietly turn
	// into "anyone", so New refuses to start on it.
	err error
}

// restricted reports whether any list limits who may sign up.
func (p signUpPolicy) restricted() bool {
	return len(p.characterIDs)+len(p.corporationIDs)+len(p.allianceIDs) > 0
}

func loadSignUpPolicy(getenv func(string) string) signUpPolicy {
	var p signUpPolicy
	for _, list := range []struct {
		name string
		into *map[int64]bool
	}{
		{"EVE_ALLOWED_CHARACTER_IDS", &p.characterIDs},
		{"EVE_ALLOWED_CORPORATION_IDS", &p.corporationIDs},
		{"EVE_ALLOWED_ALLIANCE_IDS", &p.allianceIDs},
	} {
		ids, err := parseIDList(getenv(list.name))
		if err != nil && p.err == nil {
			p.err = fmt.Errorf("%s: %w", list.name, err)
		}
		*list.into = ids
	}
	return p
}

// parseIDList parses a comma-separated list of EVE IDs strictly:
// every entry must be a positive whole number. Unlike the admin
// list, where a bad entry merely means one admin fewer, a bad entry
// here is an error for the caller to act on.
func parseIDList(raw string) (map[int64]bool, error) {
	out := map[int64]bool{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("%q is not an EVE ID (use positive numbers separated by commas)", part)
		}
		out[id] = true
	}
	return out, nil
}

// parseAdminCharIDs parses a comma-separated list of EVE character
// IDs into a set. Empty or malformed entries are skipped; an empty
// list means nobody is admin.
func parseAdminCharIDs(raw string) map[int64]bool {
	out := map[int64]bool{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		var id int64
		if _, err := fmt.Sscanf(part, "%d", &id); err != nil || id <= 0 {
			continue
		}
		out[id] = true
	}
	return out
}

// IsAdminCharacter reports whether the given EVE character ID is an
// administrator (Issues 23/24). Admin is tied to specific EVE SSO
// characters via EVE_ADMIN_CHARACTER_IDS, not to login alone.
func (c Config) IsAdminCharacter(id int64) bool {
	return c.adminCharIDs[id]
}

// loadDotEnv is a small hand-rolled .env loader. Format: KEY=VALUE per
// line, blank lines and #-comments skipped, an optional leading
// "export " tolerated, optional surrounding quotes stripped. A key
// already present in the real environment always wins — the file only
// fills in gaps.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return // no .env file: configuration comes from the environment only
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if len(value) >= 2 {
			first, last := value[0], value[len(value)-1]
			if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		if key == "" {
			continue
		}
		if _, present := os.LookupEnv(key); present {
			continue // real environment wins
		}
		_ = os.Setenv(key, value)
	}
}

func getenvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
