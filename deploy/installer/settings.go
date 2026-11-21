// The settings catalog: every key in .env.example, with what it
// does and what a valid value looks like, so the review screen can
// show and edit all of them. settings_test.go fails when the
// template gains a key this list does not have.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

type settingKind int

const (
	kindText settingKind = iota
	kindSecret
	kindURL
	kindInt
	kindChoice
	kindIDList
	kindHostPort
)

// setting describes one .env key.
type setting struct {
	Key       string
	Group     string
	Help      string
	Kind      settingKind
	Default   string   // what the app uses when the key is unset
	Choices   []string // kindChoice: the accepted values
	Min, Max  int      // kindInt: the accepted range
	Important bool     // shown before "show all" is on
	Generate  int      // bytes of random hex the G key fills in; 0 means none
}

var onOff = []string{"on", "off"}

// settingGroups is the display order of the review screen.
var settingGroups = []string{
	"EVE sign-in", "Database", "Network", "Token security", "Access",
	"Bot protection", "Performance", "Data sources", "Updates", "Logging",
	"Notifications", "Discord", "Models", "Shared cache", "Development",
}

var settingsCatalog = []setting{
	{Key: "EVE_CLIENT_ID", Group: "EVE sign-in", Important: true, Help: "Client ID of your application at developers.eveonline.com. Sign-in stays off without it."},
	{Key: "EVE_CLIENT_SECRET", Group: "EVE sign-in", Important: true, Kind: kindSecret, Help: "Client secret from the same application page."},
	{Key: "EVE_CALLBACK_URL", Group: "EVE sign-in", Important: true, Kind: kindURL, Default: "http://localhost:8080/auth/callback", Help: "Where CCP returns the browser after login. Must match your EVE application exactly."},
	{Key: "ESI_CONTACT", Group: "EVE sign-in", Important: true, Help: "How CCP can reach you: an email, Discord handle or character name. Sent with every ESI request."},
	{Key: "EVE_SSO_REVOKE_URL", Group: "EVE sign-in", Kind: kindURL, Default: "https://login.eveonline.com/v2/oauth/revoke", Help: "Token revocation endpoint. Only changed for tests."},

	{Key: "DATABASE_URL", Group: "Database", Important: true, Kind: kindSecret, Help: "PostgreSQL 16 or newer connection string."},
	{Key: "DB_MAX_CONNS", Group: "Database", Important: true, Kind: kindInt, Min: 5, Max: 500, Default: "20", Help: "Database connections the app may hold. Keep it below PostgreSQL's max_connections."},
	{Key: "LOW_RAM_MODE", Group: "Database", Important: true, Kind: kindChoice, Choices: []string{"1"}, Help: "Set to 1 to serve from PostgreSQL instead of memory. Saves roughly 600 to 700 MB."},

	{Key: "ADDR", Group: "Network", Important: true, Kind: kindHostPort, Default: ":8080", Help: "Listen address. Use 127.0.0.1:8080 behind a reverse proxy on this machine."},
	{Key: "TRUST_CF_CONNECTING_IP", Group: "Network", Important: true, Kind: kindChoice, Choices: []string{"off"}, Help: "Set to off when no Cloudflare tunnel fronts the app, so a local process cannot pick its own address."},
	{Key: "BOT_GUARD_ASN_HEADER", Group: "Network", Help: "Request header that carries the visitor's ASN from your edge."},
	{Key: "MARKET_API_HOST", Group: "Network", Help: "A separate host for the market API, such as api.example.com."},
	{Key: "STATUS_PAGE_ORIGIN", Group: "Network", Kind: kindURL, Help: "Origin of your public status page, the one site allowed to read /status.json."},

	{Key: "TOKEN_ENCRYPTION_KEY", Group: "Token security", Important: true, Kind: kindSecret, Generate: 32, Help: "Encrypts stored EVE tokens. Keep it with your backups: losing it signs every character out."},
	{Key: "TOKEN_KEY_PROVIDER", Group: "Token security", Important: true, Kind: kindChoice, Choices: []string{"env", "none", "http", "oci"}, Help: "Where the token key comes from. Use http or oci for any instance reachable from the web."},
	{Key: "TOKEN_KEY_WRAPPED", Group: "Token security", Kind: kindSecret, Help: "The wrapped token key, for the http and oci providers."},
	{Key: "TOKEN_KEY_UNWRAP_URL", Group: "Token security", Kind: kindURL, Help: "Key service address, for the http provider."},
	{Key: "TOKEN_KEY_UNWRAP_CREDENTIAL", Group: "Token security", Kind: kindSecret, Help: "Credential for the key service."},
	{Key: "TOKEN_KEY_OCI_KEY_ID", Group: "Token security", Help: "OCID of the OCI Vault key."},
	{Key: "TOKEN_KEY_OCI_CRYPTO_ENDPOINT", Group: "Token security", Kind: kindURL, Help: "Crypto endpoint of the OCI Vault."},
	{Key: "TOKEN_KEY_OCI_METADATA_URL", Group: "Token security", Kind: kindURL, Help: "Override for the OCI instance metadata address."},
	{Key: "TOKEN_KEY_OCI_FEDERATION_URL", Group: "Token security", Kind: kindURL, Help: "Override for the OCI federation endpoint."},
	{Key: "TOKEN_KEY_OCI_SECRETS_URL", Group: "Token security", Kind: kindURL, Help: "Override for the OCI Secrets service address."},
	{Key: "EVE_CLIENT_SECRET_OCI_SECRET_ID", Group: "Token security", Help: "OCI Secrets OCID holding the EVE client secret."},
	{Key: "DISCORD_CLIENT_SECRET_OCI_SECRET_ID", Group: "Token security", Help: "OCI Secrets OCID holding the Discord client secret."},
	{Key: "DISCORD_BOT_TOKEN_OCI_SECRET_ID", Group: "Token security", Help: "OCI Secrets OCID holding the Discord bot token."},
	{Key: "MODEL_API_KEY_OCI_SECRET_ID", Group: "Token security", Help: "OCI Secrets OCID holding the model API key."},
	{Key: "EVE_CLIENT_SECRET_WRAPPED", Group: "Token security", Kind: kindSecret, Help: "EVE client secret wrapped by the key service."},
	{Key: "DISCORD_CLIENT_SECRET_WRAPPED", Group: "Token security", Kind: kindSecret, Help: "Discord client secret wrapped by the key service."},
	{Key: "DISCORD_BOT_TOKEN_WRAPPED", Group: "Token security", Kind: kindSecret, Help: "Discord bot token wrapped by the key service."},
	{Key: "MODEL_API_KEY_WRAPPED", Group: "Token security", Kind: kindSecret, Help: "Model API key wrapped by the key service."},

	{Key: "EVE_ADMIN_CHARACTER_IDS", Group: "Access", Important: true, Kind: kindIDList, Help: "Character IDs that can open the Admin and Sync pages, comma separated. Unset means nobody can."},
	{Key: "EVE_ALLOWED_CHARACTER_IDS", Group: "Access", Important: true, Kind: kindIDList, Help: "Characters allowed to create an account. All three lists unset means anyone can sign up."},
	{Key: "EVE_ALLOWED_CORPORATION_IDS", Group: "Access", Important: true, Kind: kindIDList, Help: "Corporations whose members may create an account."},
	{Key: "EVE_ALLOWED_ALLIANCE_IDS", Group: "Access", Important: true, Kind: kindIDList, Help: "Alliances whose members may create an account."},
	{Key: "OPS_MANAGER_ROLES", Group: "Access", Default: "Director", Help: "In-game corporation roles that may manage calendar ops, comma separated."},

	{Key: "RANVIA_KEY", Group: "Bot protection", Important: true, Kind: kindSecret, Help: "Ranvia site key. Unset, the built-in bot guard runs."},
	{Key: "RANVIA_LOG_REFUSALS", Group: "Bot protection", Kind: kindChoice, Choices: []string{"1"}, Help: "Set to 1 to log each request Ranvia refuses, checks or slows."},
	{Key: "BOT_GUARD_SETTINGS", Group: "Bot protection", Help: "Path to a JSON file of your own rule overrides."},

	{Key: "WORKER_TIERS", Group: "Performance", Kind: kindChoice, Choices: onOff, Default: "on", Help: "Refresh recently used accounts more often. Off only suits a handful of characters."},
	{Key: "WORKER_FETCHES_PER_CYCLE", Group: "Performance", Important: true, Kind: kindInt, Min: 20, Max: 2000, Default: "480", Help: "ESI fetches per one-minute cycle for the whole server."},
	{Key: "WORKER_LANES", Group: "Performance", Important: true, Kind: kindInt, Min: 1, Max: 16, Default: "4", Help: "Characters refreshed at the same time."},
	{Key: "FORESIGHT", Group: "Performance", Kind: kindChoice, Choices: onOff, Default: "on", Help: "Fetch likely-next data ahead of game events such as docking at a hub."},
	{Key: "FORESIGHT_FETCHES_PER_CYCLE", Group: "Performance", Kind: kindInt, Min: 0, Max: 200, Default: "30", Help: "Most ESI calls a cycle may spend on fetching ahead."},
	{Key: "BATTLE_LOG_READERS", Group: "Performance", Kind: kindInt, Min: 1, Max: 32, Help: "Game logs read at once for battle reports. Unset means three eighths of the cores."},
	{Key: "NOTIFY_POLL_SECONDS", Group: "Performance", Kind: kindInt, Min: 0, Max: 3600, Default: "30", Help: "How often an open page checks for notifications. 0 turns it off."},

	{Key: "MARKET_ANON", Group: "Data sources", Kind: kindChoice, Choices: onOff, Default: "off", Help: "What anonymous visitors see on market pages. The Admin page can change it later."},
	{Key: "KILLFEED", Group: "Data sources", Kind: kindChoice, Choices: onOff, Default: "on", Help: "The public killmail stream the killboard and pilot activity read."},
	{Key: "KILLFEED_BASE_URL", Group: "Data sources", Kind: kindURL, Help: "Another address for the killmail stream."},
	{Key: "PILOT_BACKFILL", Group: "Data sources", Kind: kindChoice, Choices: onOff, Default: "on", Help: "Fill in a pilot's past activity when they first appear."},
	{Key: "EVE_SDE_BASE_URL", Group: "Data sources", Kind: kindURL, Default: "https://www.fuzzwork.co.uk/dump/latest/csv/", Help: "Where the EVE static data is downloaded from."},
	{Key: "EVE_FUZZWORK_BASE_URL", Group: "Data sources", Kind: kindURL, Help: "Address of the fallback market aggregate."},
	{Key: "ZKILL_API_BASE_URL", Group: "Data sources", Kind: kindURL, Help: "Address of zKillboard's history API."},
	{Key: "KILL_ARCHIVE_BASE_URL", Group: "Data sources", Default: "https://data.everef.net/killmails", Help: "Daily killmail archives. Set to off to turn the restore off."},

	{Key: "EVESYNAPSE_UPDATE_PROVIDER", Group: "Updates", Help: "Where updates come from: github, gitlab, forgejo, mirror, comma separated."},
	{Key: "EVESYNAPSE_UPDATE_PROJECT", Group: "Updates", Help: "GitLab project to update from. Only for a fork."},
	{Key: "EVESYNAPSE_UPDATE_API_URL", Group: "Updates", Kind: kindURL, Help: "API address for a self-hosted GitLab or GitHub Enterprise."},
	{Key: "EVESYNAPSE_UPDATE_MIRROR_URL", Group: "Updates", Kind: kindURL, Help: "Root of a release mirror served somewhere else."},

	{Key: "LOG_LEVEL", Group: "Logging", Kind: kindChoice, Choices: []string{"debug", "info", "warn", "error"}, Default: "info", Help: "The least severe kind of line written."},
	{Key: "LOG_FORMAT", Group: "Logging", Kind: kindChoice, Choices: []string{"text", "json"}, Default: "text", Help: "Log format. Use json for a log collector."},

	{Key: "VAPID_PUBLIC_KEY", Group: "Notifications", Help: "Browser push public key, from evesynapse -push-keys."},
	{Key: "VAPID_PRIVATE_KEY", Group: "Notifications", Kind: kindSecret, Help: "Browser push private key. Changing it makes every browser subscribe again."},
	{Key: "VAPID_SUBJECT", Group: "Notifications", Help: "Contact for push services, such as mailto:you@example.org."},
	{Key: "ANDROID_APP_PACKAGE", Group: "Notifications", Help: "The site's Android app package."},
	{Key: "ANDROID_APP_FINGERPRINTS", Group: "Notifications", Help: "SHA-256 signing certificate fingerprints, comma separated."},

	{Key: "DISCORD_CLIENT_ID", Group: "Discord", Important: true, Help: "Discord application ID. Turns on Connect Discord."},
	{Key: "DISCORD_CLIENT_SECRET", Group: "Discord", Important: true, Kind: kindSecret, Help: "Discord application secret."},
	{Key: "DISCORD_BOT_TOKEN", Group: "Discord", Important: true, Kind: kindSecret, Help: "Lets the bot be added to Discord servers."},

	{Key: "MODEL_API_KEY", Group: "Models", Kind: kindSecret, Help: "API key for the Trade ideas model."},
	{Key: "CLOUDFLARE_ACCOUNT_ID", Group: "Models", Help: "Cloudflare account for Workers AI, used when no model key is set."},
	{Key: "CLOUDFLARE_AUTH_TOKEN", Group: "Models", Kind: kindSecret, Help: "Cloudflare token for Workers AI."},
	{Key: "INTEL_TAGGER", Group: "Models", Kind: kindChoice, Choices: []string{"off"}, Help: "Set to off to read intel without the built-in classifier."},
	{Key: "INTEL_TAGGER_WEIGHTS", Group: "Models", Help: "A model file to try instead of the built-in classifier."},
	{Key: "RESIDUE_MODEL", Group: "Models", Kind: kindChoice, Choices: onOff, Default: "on", Help: "The local model that reads intel the classifier cannot."},
	{Key: "RESIDUE_MODEL_PATH", Group: "Models", Help: "Model file. Unset means not installed, and the step idles."},
	{Key: "RESIDUE_MODEL_BIN", Group: "Models", Help: "Runtime that serves the model."},
	{Key: "RESIDUE_MODEL_ADDR", Group: "Models", Kind: kindHostPort, Default: "127.0.0.1:18099", Help: "Where the model listens. Loopback only."},

	{Key: "SHARED_CACHE", Group: "Shared cache", Important: true, Kind: kindChoice, Choices: []string{"off", "on", "open", "allowlist"}, Default: "off", Help: "Share public ESI data with other instances."},
	{Key: "SHARED_CACHE_SECRET", Group: "Shared cache", Important: true, Kind: kindSecret, Generate: 32, Help: "The swarm's membership proof, at least 32 characters. Required when sharing is on."},
	{Key: "SHARED_CACHE_SEEDS", Group: "Shared cache", Help: "Instances a new one asks for the member list, comma separated."},
	{Key: "SHARED_CACHE_GOSSIP_HOURS", Group: "Shared cache", Kind: kindInt, Min: 12, Max: 168, Default: "18", Help: "How often membership is shared again, in hours."},
	{Key: "SHARED_CACHE_WARM", Group: "Shared cache", Help: "Run as a warming node: all, or a comma list of record kinds."},

	{Key: "DEV_LOGIN", Group: "Development", Kind: kindChoice, Choices: []string{"1"}, Help: "Instant fake sign-in for local development. Never enable in production."},
}

func settingFor(key string) (setting, bool) {
	for _, s := range settingsCatalog {
		if s.Key == key {
			return s, true
		}
	}
	return setting{}, false
}

// isSecretKey reports whether a key's value must never be shown or
// kept in memory after reading the existing file.
func isSecretKey(key string) bool {
	s, ok := settingFor(key)
	return ok && s.Kind == kindSecret
}

// validate checks a value for this setting. A blank value is always
// valid: it means the key is unset.
func (s setting) validate(v string) error {
	if v == "" {
		return nil
	}
	if strings.ContainsAny(v, "\r\n") {
		return fmt.Errorf("a value cannot contain a line break")
	}
	if strings.Contains(v, "'") && strings.ContainsAny(v, "\"\\") {
		return fmt.Errorf("a value cannot mix a single quote with a double quote or backslash")
	}
	switch s.Kind {
	case kindURL:
		u, err := url.Parse(v)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("enter a full http:// or https:// address")
		}
	case kindInt:
		n, err := strconv.Atoi(v)
		if err != nil || n < s.Min || n > s.Max {
			return fmt.Errorf("enter a whole number from %d to %d", s.Min, s.Max)
		}
	case kindChoice:
		for _, c := range s.Choices {
			if v == c {
				return nil
			}
		}
		return fmt.Errorf("choose one of: %s", strings.Join(s.Choices, ", "))
	case kindIDList:
		for _, part := range strings.Split(v, ",") {
			if n, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64); err != nil || n <= 0 {
				return fmt.Errorf("enter EVE IDs as numbers, separated by commas")
			}
		}
	case kindHostPort:
		_, port, err := net.SplitHostPort(v)
		if err != nil {
			return fmt.Errorf("enter an address like 127.0.0.1:8080")
		}
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("the port must be from 1 to 65535")
		}
	}
	if s.Key == "DATABASE_URL" && !strings.HasPrefix(v, "postgres://") && !strings.HasPrefix(v, "postgresql://") {
		return fmt.Errorf("a connection string starts with postgres://")
	}
	if s.Key == "SHARED_CACHE_SECRET" && len(v) < 32 {
		return fmt.Errorf("the secret must be at least 32 characters")
	}
	return nil
}

// generateValue makes a random hex value of n bytes.
func generateValue(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
