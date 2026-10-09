package app

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"evesynapse/internal/dotenv"
	"evesynapse/internal/logging"
	"evesynapse/internal/store"
)

// Config holds runtime configuration. Everything comes from the
// environment (after dotenv.Load has had a chance to fill gaps from a
// local .env); secrets are never hardcoded or logged.
type Config struct {
	// opsManagerRoles are the in-game corporation roles that may
	// create and change ops (ops.go). OPS_MANAGER_ROLES, comma
	// separated; Director when unset.
	opsManagerRoles []string

	// Browser push (notify_push.go): the server's VAPID key pair and
	// a contact address for the push services. All unset means push
	// is off.
	pushPublicKey, pushPrivateKey, pushSubject string

	// notifyPoll is how often, in seconds, an open page asks whether
	// its notifications icon has changed (NOTIFY_POLL_SECONDS). 0: it
	// does not ask, and the icon changes on page loads only.
	notifyPoll int

	// workerTiersOff (WORKER_TIERS=off) has the worker refresh every
	// character as often as ESI allows, whether or not anyone is
	// looking, as it did before tiers (worker_tiers.go).
	workerTiersOff bool

	// workerFetches is the worker's fetch allowance for one cycle
	// (WORKER_FETCHES_PER_CYCLE); 0 means the default.
	workerFetches int

	// workerLanes is how many characters the worker refreshes at once
	// (WORKER_LANES, worker_lanes.go). 0, which only a Config not read
	// from the environment has, means one.
	workerLanes int

	// dbConns is how many database connections the app may hold
	// (DB_MAX_CONNS); 0 means the store's default.
	dbConns int

	// Discord (discord_link.go). The client id and secret run the
	// "Connect Discord" sign-in; the bot token lets the bot message
	// and give roles in the servers it is added to. With none set
	// Discord is off. What the bot does in each server is not set
	// here: its directors choose that on the site (discord_servers.go).
	discordClientID, discordClientSecret string
	discordBotToken                      string

	addr            string         // listen address
	databaseURL     string         // Postgres connection URL (DATABASE_URL)
	eveClientID     string         // EVE SSO application client ID
	eveClientSecret string         // EVE SSO application client secret
	eveCallbackURL  string         // OAuth2 redirect URI registered with CCP
	devLogin        bool           // DEV_LOGIN=1: register the /dev-login route
	sdeBaseURL      string         // EVE SDE CSV dump base URL (Fuzzwork by default)
	adminCharIDs    map[int64]bool // EVE_ADMIN_CHARACTER_IDS (comma-separated)
	tokenKey        string         // TOKEN_ENCRYPTION_KEY: encrypts stored EVE tokens ("" = stored as they are)
	signUp          signUpPolicy   // EVE_ALLOWED_*_IDS: who may create an account (empty = anyone)
	esiContact      string         // ESI_CONTACT: how CCP can reach the operator, sent in the User-Agent
	logLevel        string         // LOG_LEVEL: debug, info (default), warn or error
	logFormat       string         // LOG_FORMAT: text (default) or json
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

// defaultSDEBaseURL is Fuzzwork's community SDE conversion, CSV tables
// under /dump/latest/csv/.
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

// LoadConfig loads the .env file (if present) and then reads the
// environment. A .env that exists but cannot be read is an error,
// not a silent fall back to defaults: the defaults point at a
// development database, and the wrong database is worse than no
// start at all.
func LoadConfig() (Config, error) {
	if err := loadEnvFile(); err != nil {
		return Config{}, err
	}
	notifyPoll, ok := parseNotifyPoll(os.Getenv("NOTIFY_POLL_SECONDS"))
	if !ok {
		logging.Warnf("evesynapse: NOTIFY_POLL_SECONDS=%q is not a whole number of seconds; using %d", os.Getenv("NOTIFY_POLL_SECONDS"), notifyPoll)
	}
	return Config{
		notifyPoll:          notifyPoll,
		workerTiersOff:      strings.EqualFold(strings.TrimSpace(os.Getenv("WORKER_TIERS")), "off"),
		workerFetches:       parseWorkerFetches(os.Getenv("WORKER_FETCHES_PER_CYCLE")),
		workerLanes:         parseWorkerLanes(os.Getenv("WORKER_LANES")),
		dbConns:             parseDBConns(os.Getenv("DB_MAX_CONNS")),
		discordClientID:     os.Getenv("DISCORD_CLIENT_ID"),
		discordClientSecret: os.Getenv("DISCORD_CLIENT_SECRET"),
		discordBotToken:     os.Getenv("DISCORD_BOT_TOKEN"),
		addr:                getenvDefault("ADDR", ":8080"),
		databaseURL:         getenvDefault("DATABASE_URL", "postgres://evesynapse@localhost:5432/evesynapse?sslmode=disable"),
		eveClientID:         os.Getenv("EVE_CLIENT_ID"),
		eveClientSecret:     os.Getenv("EVE_CLIENT_SECRET"),
		eveCallbackURL:      getenvDefault("EVE_CALLBACK_URL", "http://localhost:8080/auth/callback"),
		devLogin:            os.Getenv("DEV_LOGIN") == "1",
		sdeBaseURL:          getenvDefault("EVE_SDE_BASE_URL", defaultSDEBaseURL),
		adminCharIDs:        parseAdminCharIDs(os.Getenv("EVE_ADMIN_CHARACTER_IDS")),
		tokenKey:            os.Getenv("TOKEN_ENCRYPTION_KEY"),
		signUp:              loadSignUpPolicy(os.Getenv),
		esiContact:          os.Getenv("ESI_CONTACT"),
		logLevel:            os.Getenv("LOG_LEVEL"),
		logFormat:           os.Getenv("LOG_FORMAT"),
		pushPublicKey:       os.Getenv("VAPID_PUBLIC_KEY"),
		pushPrivateKey:      os.Getenv("VAPID_PRIVATE_KEY"),
		pushSubject:         os.Getenv("VAPID_SUBJECT"),
		opsManagerRoles:     parseRoleList(os.Getenv("OPS_MANAGER_ROLES")),
	}, nil
}

// loadEnvFile loads ./.env when the process started beside one,
// else the .env beside the binary. Services start away from their
// checkout (WorkingDirectory=/, or the binary's own directory),
// and reading only the working directory has pointed -refresh at
// the default database. The working directory wins when both
// exist, so `go run` from a checkout keeps working (its binary
// lives in a build cache with no .env beside it). Neither file
// existing is fine: configuration then comes from the environment.
func loadEnvFile() error {
	if _, err := os.Stat(".env"); err == nil {
		return dotenv.Load(".env")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf(".env: %w", err)
	}
	exe, err := os.Executable()
	if err != nil {
		return nil // no install directory to look beside
	}
	installEnv := filepath.Join(filepath.Dir(exe), ".env")
	if _, err := os.Stat(installEnv); err != nil {
		return nil // neither directory has one: environment only
	}
	return dotenv.Load(installEnv)
}

// esiUserAgent builds the User-Agent every ESI request carries:
// the product and its version, where the code lives, and — when
// the operator set ESI_CONTACT — how to reach whoever runs this
// instance. CCP asks for exactly that, so that a client causing
// trouble gets a message rather than a block.
func (c Config) esiUserAgent() string {
	ua := "EveSynapse/" + strings.TrimPrefix(appVersion, "v") + " (+https://github.com/natemsz/evesynapse"
	// A header value cannot carry control characters; anything of
	// the sort in the setting becomes a space rather than breaking
	// every request.
	contact := strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, c.esiContact))
	if len(contact) > 120 {
		contact = contact[:120]
	}
	if contact != "" {
		ua += "; " + contact
	}
	return ua + ")"
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
		// Strict whole-string parse: Sscanf's %d accepts "123abc"
		// as 123, silently granting (or denying) the wrong ID, and
		// ParseInt takes a leading "+", so digits only.
		if strings.IndexFunc(part, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
			continue
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil || id <= 0 {
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

func getenvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// SetupLogging applies LOG_LEVEL and LOG_FORMAT to the process. The
// entrypoints call it once, before anything is logged; an unknown
// level or format is an error rather than a silent fallback, so a
// typo cannot leave the log quieter or louder than intended.
func (c Config) SetupLogging() error {
	return logging.Setup(c.logLevel, c.logFormat, os.Stderr)
}

// defaultOpsManagerRole is who may manage ops when OPS_MANAGER_ROLES
// is not set.
const defaultOpsManagerRole = "Director"

// parseRoleList reads a comma-separated list of in-game corporation
// role names ("Director,Personnel_Manager"); empty means the default.
func parseRoleList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if role := strings.TrimSpace(part); role != "" {
			out = append(out, role)
		}
	}
	if len(out) == 0 {
		out = []string{defaultOpsManagerRole}
	}
	return out
}

// holdsOpsManagerRole reports whether one of a character's roles is a
// manager role. Role names are compared as ESI writes them, ignoring
// case.
func (c Config) holdsOpsManagerRole(roles []string) bool {
	managers := c.opsManagerRoles
	if len(managers) == 0 {
		managers = []string{defaultOpsManagerRole}
	}
	for _, role := range roles {
		for _, want := range managers {
			if strings.EqualFold(role, want) {
				return true
			}
		}
	}
	return false
}

// opsManagerRolesText names the manager roles for a message.
func (c Config) opsManagerRolesText() string {
	managers := c.opsManagerRoles
	if len(managers) == 0 {
		managers = []string{defaultOpsManagerRole}
	}
	return strings.ReplaceAll(strings.Join(managers, " or "), "_", " ")
}

// Bounds on DB_MAX_CONNS.
const (
	minDBConns  = 5
	mostDBConns = 500
)

// parseDBConns reads DB_MAX_CONNS: 0 (use the default) when unset or
// unreadable, else the number kept in bounds.
func parseDBConns(raw string) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		logging.Warnf("evesynapse: DB_MAX_CONNS=%q is not a whole number; using %d", raw, store.DefaultPoolSize)
		return 0
	}
	if n < minDBConns {
		return minDBConns
	}
	if n > mostDBConns {
		return mostDBConns
	}
	return n
}
