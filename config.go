package main

import (
	"bufio"
	"os"
	"strings"
)

// config holds runtime configuration. Everything comes from the
// environment (after loadDotEnv has had a chance to fill gaps from a
// local .env); secrets are never hardcoded or logged.
type config struct {
	addr            string // listen address
	dbPath          string // SQLite database file
	eveClientID     string // EVE SSO application client ID
	eveClientSecret string // EVE SSO application client secret
	eveCallbackURL  string // OAuth2 redirect URI registered with CCP
	sessionKey      string // reserved for cookie signing hardening
	devLogin        bool   // DEV_LOGIN=1: register the /dev-login route
}

// ssoConfigured reports whether EVE SSO can run: it needs both the
// client ID (login redirect) and the secret (token exchange).
func (c config) ssoConfigured() bool {
	return c.eveClientID != "" && c.eveClientSecret != ""
}

// loadConfig loads ./.env (if present) and then reads the environment.
func loadConfig() config {
	loadDotEnv(".env")
	return config{
		addr:            getenvDefault("ADDR", ":8080"),
		dbPath:          getenvDefault("DB_PATH", "evesynapse.db"),
		eveClientID:     os.Getenv("EVE_CLIENT_ID"),
		eveClientSecret: os.Getenv("EVE_CLIENT_SECRET"),
		eveCallbackURL:  getenvDefault("EVE_CALLBACK_URL", "http://localhost:8080/auth/callback"),
		sessionKey:      os.Getenv("SESSION_KEY"),
		devLogin:        os.Getenv("DEV_LOGIN") == "1",
	}
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
