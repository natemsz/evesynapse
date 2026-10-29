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

	// The Android wrapper (core_assetlinks.go): the app's package and
	// the fingerprints of the certificates it may be signed with.
	// ANDROID_APP_PACKAGE and ANDROID_APP_FINGERPRINTS; either unset
	// means the site names no app.
	androidPackage      string
	androidFingerprints []string

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

	// Foresight (foresight.go): warming on game events. FORESIGHT=off
	// switches it off; FORESIGHT_FETCHES_PER_CYCLE bounds what it may
	// spend of a cycle's fetch allowance.
	foresightOff     bool
	foresightFetches int

	// anonMarketOff (MARKET_ANON=off) is the BOOT DEFAULT for the
	// anonymous-market switch (market_anon.go): while off, anonymous
	// visitors get the market order book from stored sweep data and
	// may queue market history wants. The Admin page flips the live
	// setting at runtime (persisted in site_settings); the stored
	// row wins over this default once it exists, so the env value
	// only decides a fresh install's starting state.
	anonMarketOff bool

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

	addr                     string         // listen address
	databaseURL              string         // Postgres connection URL (DATABASE_URL)
	eveClientID              string         // EVE SSO application client ID
	eveClientSecret          string         // EVE SSO application client secret
	eveCallbackURL           string         // OAuth2 redirect URI registered with CCP
	eveRevokeURL             string         // EVE SSO token revocation endpoint (EVE_SSO_REVOKE_URL)
	devLogin                 bool           // DEV_LOGIN=1: register the /dev-login route
	sdeBaseURL               string         // EVE SDE CSV dump base URL (Fuzzwork by default)
	fuzzworkBaseURL          string         // Fuzzwork market aggregate base URL (fallback prices)
	killfeedBaseURL          string         // zKillboard R2Z2 killmail stream base URL (killfeed.go)
	killfeedOff              bool           // KILLFEED=off: no public killmail stream
	zkillAPIBaseURL          string         // zKillboard history API base URL (pilot_activity.go)
	pilotBackfillOff         bool           // PILOT_BACKFILL=off: no pilot history backfills
	residueOff               bool           // RESIDUE_MODEL=off: no residue step (residue_model.go)
	residueModelPath         string         // RESIDUE_MODEL_PATH: the local reader's model file ("" = not installed, the step idles)
	residueModelBin          string         // RESIDUE_MODEL_BIN: the local reader's runtime binary
	residueModelAddr         string         // RESIDUE_MODEL_ADDR: where the local reader listens (loopback only)
	adminCharIDs             map[int64]bool // EVE_ADMIN_CHARACTER_IDS (comma-separated)
	tokenKey                 string         // TOKEN_ENCRYPTION_KEY: encrypts stored EVE tokens ("" = stored as they are)
	tokenKeyProvider         string         // TOKEN_KEY_PROVIDER: where the token key comes from — env, http, oci or none ("" = derive: a set TOKEN_ENCRYPTION_KEY means env, none set means none)
	tokenKeyWrapped          string         // TOKEN_KEY_WRAPPED: the token key wrapped by the key service (http provider) or the KMS key (oci provider)
	tokenKeyUnwrapURL        string         // TOKEN_KEY_UNWRAP_URL: the key service's unwrap endpoint (http provider)
	tokenKeyCred             string         // TOKEN_KEY_UNWRAP_CREDENTIAL: bearer credential for the unwrap endpoint (http provider)
	tokenKeyOCIKeyID         string         // TOKEN_KEY_OCI_KEY_ID: the OCI KMS key that wrapped the token key (oci provider)
	tokenKeyOCIEndpoint      string         // TOKEN_KEY_OCI_CRYPTO_ENDPOINT: the vault's cryptographic endpoint (oci provider)
	tokenKeyOCIMetadataURL   string         // TOKEN_KEY_OCI_METADATA_URL: the instance metadata base (oci provider; "" = the link-local default — an override for tests)
	tokenKeyOCIFederationURL string         // TOKEN_KEY_OCI_FEDERATION_URL: the region federation endpoint (oci provider; "" = derived from the crypto endpoint)
	tokenKeyOCISecretsURL    string         // TOKEN_KEY_OCI_SECRETS_URL: the OCI Secrets service base (oci provider; "" = derived from the crypto endpoint's region — an override for tests)

	// The working credentials' external sources (auth_credentials.go):
	// for each of EVE_CLIENT_SECRET, DISCORD_CLIENT_SECRET,
	// DISCORD_BOT_TOKEN and MODEL_API_KEY, either the OCI secret
	// holding it (<name>_OCI_SECRET_ID, oci provider) or its value
	// wrapped by the key service (<name>_WRAPPED, http provider).
	// Both empty means the credential is its plain env value, as
	// always. creds is where a fetched value lands at startup; the
	// accessors (EVEClientSecret and its siblings) read it ahead of
	// the plain fields below.
	creds                      *credentialStore
	eveClientSecretOCIID       string
	eveClientSecretWrapped     string
	discordClientSecretOCIID   string
	discordClientSecretWrapped string
	discordBotTokenOCIID       string
	discordBotTokenWrapped     string
	metaAIKeyOCIID             string
	metaAIKeyWrapped           string
	signUp                     signUpPolicy // EVE_ALLOWED_*_IDS: who may create an account (empty = anyone)
	esiContact                 string       // ESI_CONTACT: how CCP can reach the operator, sent in the User-Agent
	statusOrigin               string       // STATUS_PAGE_ORIGIN: the status page's origin, the one site a browser may read /status.json from (core_status.go)
	logLevel                   string       // LOG_LEVEL: debug, info (default), warn or error
	logFormat                  string       // LOG_FORMAT: text (default) or json

	// Trade ideas (tradeideas.go): a model asked about the Forge
	// spread-scanner rows every so often. Meta's Muse Wonder when
	// MODEL_API_KEY is set (tradeideas_meta.go), else Gemma on
	// Cloudflare Workers AI with the account ID and token. Neither
	// configured means the feature stays off; a key or token never
	// leaves the Authorization header or the logs.
	cloudflareAccountID string // CLOUDFLARE_ACCOUNT_ID
	cloudflareAuthToken string // CLOUDFLARE_AUTH_TOKEN
	metaAIKey           string // MODEL_API_KEY

	// noWorker builds the application without its background worker:
	// one-shot commands (-populate-names) that share the database
	// with a running server must not run a second worker beside it.
	noWorker bool
}

// TradeIdeasConfigured reports whether the trade-ideas refresh
// can run: a Meta API key, or both the Cloudflare account ID (the
// URL) and token (the Authorization header).
func (c Config) TradeIdeasConfigured() bool {
	return c.MetaAIKey() != "" || (c.cloudflareAccountID != "" && c.cloudflareAuthToken != "")
}

// tradeIdeasUsesMeta reports whether the refresh asks Muse Wonder
// (MODEL_API_KEY set) rather than Gemma.
func (c Config) tradeIdeasUsesMeta() bool { return c.MetaAIKey() != "" }

// SSOConfigured reports whether EVE SSO can run: it needs both the
// client ID (login redirect) and the secret (token exchange).
func (c Config) SSOConfigured() bool {
	return c.eveClientID != "" && c.EVEClientSecret() != ""
}

// The working credentials in force. Each accessor returns the
// value fetched from the credential's external source once one
// has landed (auth_credentials.go); until then — and for a
// credential with no external source — it is the plain env value
// the field has always held. Callers must read through these, not
// the fields, so a credential that arrives after boot takes
// effect without a restart.
func (c Config) EVEClientSecret() string { return c.credential(credEVEClientSecret, c.eveClientSecret) }

// DiscordClientSecret returns the Discord OAuth client secret in
// force (see EVEClientSecret).
func (c Config) DiscordClientSecret() string {
	return c.credential(credDiscordClientSecret, c.discordClientSecret)
}

// DiscordBotToken returns the Discord bot token in force (see
// EVEClientSecret).
func (c Config) DiscordBotToken() string { return c.credential(credDiscordBotToken, c.discordBotToken) }

// MetaAIKey returns the MODEL_API_KEY in force (see
// EVEClientSecret).
func (c Config) MetaAIKey() string { return c.credential(credModelAPIKey, c.metaAIKey) }

// credential resolves one working credential: the fetched value
// when the store holds one, else the plain env value.
func (c Config) credential(name, plain string) string {
	if c.creds != nil {
		if v, ok := c.creds.get(name); ok {
			return v
		}
	}
	return plain
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

// defaultFuzzworkBaseURL is Fuzzwork's market aggregate service:
// the orderset poll and the bulk aggregate CSV the fallback
// price mirror reads.
const defaultFuzzworkBaseURL = "https://market.fuzzwork.co.uk/"

// defaultEveRevokeURL is CCP's OAuth2 token revocation endpoint
// (RFC 7009): unlinking a character can ask CCP to revoke its
// refresh token there, not just delete the local copy.
const defaultEveRevokeURL = "https://login.eveonline.com/v2/oauth/revoke"

// EveRevokeURL returns the EVE SSO token-revocation endpoint.
func (c Config) EveRevokeURL() string {
	if c.eveRevokeURL != "" {
		return c.eveRevokeURL
	}
	return defaultEveRevokeURL
}

// FuzzworkBaseURL returns the base URL the fallback price
// mirror polls and downloads from, always with a trailing
// slash.
func (c Config) FuzzworkBaseURL() string {
	base := c.fuzzworkBaseURL
	if base == "" {
		base = defaultFuzzworkBaseURL
	}
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	return base
}

// defaultKillfeedBaseURL is zKillboard's R2Z2 killmail stream: one
// JSON file per killmail, numbered by a global sequence.
const defaultKillfeedBaseURL = "https://r2z2.zkillboard.com/ephemeral/"

// KillfeedBaseURL returns the killmail stream's base URL, always
// with a trailing slash.
func (c Config) KillfeedBaseURL() string {
	base := c.killfeedBaseURL
	if base == "" {
		base = defaultKillfeedBaseURL
	}
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	return base
}

// KillfeedEnabled reports whether the public killmail stream runs:
// KILLFEED=off turns it off, and a Config not built by LoadConfig
// (the tests') never reaches the real stream.
func (c Config) KillfeedEnabled() bool { return !c.killfeedOff && c.killfeedBaseURL != "" }

// defaultZKillAPIBaseURL is zKillboard's history API: filtered
// killmail queries, a pilot's kills and losses among them.
const defaultZKillAPIBaseURL = "https://zkillboard.com/api/"

// ZKillAPIBaseURL returns the history API's base URL, always with
// a trailing slash.
func (c Config) ZKillAPIBaseURL() string {
	base := c.zkillAPIBaseURL
	if base == "" {
		base = defaultZKillAPIBaseURL
	}
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	return base
}

// PilotBackfillEnabled reports whether pilot activity backfills
// run: PILOT_BACKFILL=off turns them off, and a Config not built
// by LoadConfig (the tests') never reaches the real API.
func (c Config) PilotBackfillEnabled() bool { return !c.pilotBackfillOff && c.zkillAPIBaseURL != "" }

// ResidueModelEnabled reports whether the residue step runs
// (residue_model.go): RESIDUE_MODEL=off turns it off, and with
// no model file configured there is nothing to run, so a Config
// not built by LoadConfig (the tests') never starts it either.
func (c Config) ResidueModelEnabled() bool { return !c.residueOff && c.residueModelPath != "" }

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
		notifyPoll:                 notifyPoll,
		workerTiersOff:             strings.EqualFold(strings.TrimSpace(os.Getenv("WORKER_TIERS")), "off"),
		workerFetches:              parseWorkerFetches(os.Getenv("WORKER_FETCHES_PER_CYCLE")),
		workerLanes:                parseWorkerLanes(os.Getenv("WORKER_LANES")),
		foresightOff:               strings.EqualFold(strings.TrimSpace(os.Getenv("FORESIGHT")), "off"),
		anonMarketOff:              strings.EqualFold(strings.TrimSpace(os.Getenv("MARKET_ANON")), "off"),
		foresightFetches:           parseForesightFetches(os.Getenv("FORESIGHT_FETCHES_PER_CYCLE")),
		dbConns:                    parseDBConns(os.Getenv("DB_MAX_CONNS")),
		discordClientID:            os.Getenv("DISCORD_CLIENT_ID"),
		discordClientSecret:        os.Getenv("DISCORD_CLIENT_SECRET"),
		discordBotToken:            os.Getenv("DISCORD_BOT_TOKEN"),
		addr:                       getenvDefault("ADDR", ":8080"),
		databaseURL:                getenvDefault("DATABASE_URL", "postgres://evesynapse@localhost:5432/evesynapse?sslmode=disable"),
		eveClientID:                os.Getenv("EVE_CLIENT_ID"),
		eveClientSecret:            os.Getenv("EVE_CLIENT_SECRET"),
		eveCallbackURL:             getenvDefault("EVE_CALLBACK_URL", "http://localhost:8080/auth/callback"),
		eveRevokeURL:               getenvDefault("EVE_SSO_REVOKE_URL", defaultEveRevokeURL),
		devLogin:                   os.Getenv("DEV_LOGIN") == "1",
		sdeBaseURL:                 getenvDefault("EVE_SDE_BASE_URL", defaultSDEBaseURL),
		fuzzworkBaseURL:            getenvDefault("EVE_FUZZWORK_BASE_URL", defaultFuzzworkBaseURL),
		killfeedBaseURL:            getenvDefault("KILLFEED_BASE_URL", defaultKillfeedBaseURL),
		killfeedOff:                strings.EqualFold(strings.TrimSpace(os.Getenv("KILLFEED")), "off"),
		zkillAPIBaseURL:            getenvDefault("ZKILL_API_BASE_URL", defaultZKillAPIBaseURL),
		pilotBackfillOff:           strings.EqualFold(strings.TrimSpace(os.Getenv("PILOT_BACKFILL")), "off"),
		residueOff:                 strings.EqualFold(strings.TrimSpace(os.Getenv("RESIDUE_MODEL")), "off"),
		residueModelPath:           os.Getenv("RESIDUE_MODEL_PATH"),
		residueModelBin:            getenvDefault("RESIDUE_MODEL_BIN", "llama-server"),
		residueModelAddr:           getenvDefault("RESIDUE_MODEL_ADDR", "127.0.0.1:18099"),
		adminCharIDs:               parseAdminCharIDs(os.Getenv("EVE_ADMIN_CHARACTER_IDS")),
		tokenKey:                   os.Getenv("TOKEN_ENCRYPTION_KEY"),
		tokenKeyProvider:           os.Getenv("TOKEN_KEY_PROVIDER"),
		tokenKeyWrapped:            os.Getenv("TOKEN_KEY_WRAPPED"),
		tokenKeyUnwrapURL:          os.Getenv("TOKEN_KEY_UNWRAP_URL"),
		tokenKeyCred:               os.Getenv("TOKEN_KEY_UNWRAP_CREDENTIAL"),
		tokenKeyOCIKeyID:           os.Getenv("TOKEN_KEY_OCI_KEY_ID"),
		tokenKeyOCIEndpoint:        os.Getenv("TOKEN_KEY_OCI_CRYPTO_ENDPOINT"),
		tokenKeyOCIMetadataURL:     os.Getenv("TOKEN_KEY_OCI_METADATA_URL"),
		tokenKeyOCIFederationURL:   os.Getenv("TOKEN_KEY_OCI_FEDERATION_URL"),
		tokenKeyOCISecretsURL:      os.Getenv("TOKEN_KEY_OCI_SECRETS_URL"),
		creds:                      newCredentialStore(),
		eveClientSecretOCIID:       os.Getenv("EVE_CLIENT_SECRET_OCI_SECRET_ID"),
		eveClientSecretWrapped:     os.Getenv("EVE_CLIENT_SECRET_WRAPPED"),
		discordClientSecretOCIID:   os.Getenv("DISCORD_CLIENT_SECRET_OCI_SECRET_ID"),
		discordClientSecretWrapped: os.Getenv("DISCORD_CLIENT_SECRET_WRAPPED"),
		discordBotTokenOCIID:       os.Getenv("DISCORD_BOT_TOKEN_OCI_SECRET_ID"),
		discordBotTokenWrapped:     os.Getenv("DISCORD_BOT_TOKEN_WRAPPED"),
		metaAIKeyOCIID:             os.Getenv("MODEL_API_KEY_OCI_SECRET_ID"),
		metaAIKeyWrapped:           os.Getenv("MODEL_API_KEY_WRAPPED"),
		signUp:                     loadSignUpPolicy(os.Getenv),
		esiContact:                 os.Getenv("ESI_CONTACT"),
		statusOrigin:               parseStatusOrigin(os.Getenv("STATUS_PAGE_ORIGIN")),
		cloudflareAccountID:        strings.TrimSpace(os.Getenv("CLOUDFLARE_ACCOUNT_ID")),
		cloudflareAuthToken:        strings.TrimSpace(os.Getenv("CLOUDFLARE_AUTH_TOKEN")),
		metaAIKey:                  strings.TrimSpace(os.Getenv("MODEL_API_KEY")),
		logLevel:                   os.Getenv("LOG_LEVEL"),
		logFormat:                  os.Getenv("LOG_FORMAT"),
		pushPublicKey:              os.Getenv("VAPID_PUBLIC_KEY"),
		pushPrivateKey:             os.Getenv("VAPID_PRIVATE_KEY"),
		pushSubject:                os.Getenv("VAPID_SUBJECT"),
		androidPackage:             strings.TrimSpace(os.Getenv("ANDROID_APP_PACKAGE")),
		androidFingerprints:        parseAndroidFingerprints(os.Getenv("ANDROID_APP_FINGERPRINTS")),
		opsManagerRoles:            parseRoleList(os.Getenv("OPS_MANAGER_ROLES")),
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
	return intSetting("DB_MAX_CONNS", raw, minDBConns, mostDBConns, 0)
}

// intSetting reads a whole-number setting: unset when raw is blank or
// not a positive whole number (which the log says), else the number
// kept between lo and hi.
func intSetting(name, raw string, lo, hi, unset int) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return unset
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		logging.Warnf("evesynapse: %s=%q is not a whole number; using the default", name, raw)
		return unset
	}
	return min(max(n, lo), hi)
}
