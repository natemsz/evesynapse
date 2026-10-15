package swarm

// ---------------------------------------------------------------------------
// Shared-cache configuration (the paper's "shared cache across
// instances" section). Self-hosted instances share public ESI data
// (market history, type names, corporation and alliance records)
// so the network asks ESI for each record once instead of once
// per instance. Private data never leaves the instance.
//
// Off unless the operator says otherwise: SHARED_CACHE is empty
// (or anything unrecognized) means off. "open" shares with every
// instance that holds the secret; "allowlist" additionally waits
// for the operator's approval per instance. Enabling without a
// SHARED_CACHE_SECRET, or with a weak one, refuses to start:
// an open door must never come from a typo.
// ---------------------------------------------------------------------------

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"evesynapse/internal/logging"
)

// The shared-cache modes (SHARED_CACHE).
const (
	SwarmOff       = "off"
	SwarmOpen      = "open"
	SwarmAllowlist = "allowlist"
)

// Defaults and bounds for the gossip rate: membership spreads
// peer to peer about once a day, jittered, so discovery traffic
// stays near zero. Below 12 hours is refused: gossip is for
// slow membership drift, not for data.
const (
	DefaultSwarmGossipHours = 18
	MinSwarmGossipHours     = 12
	MaxSwarmGossipHours     = 168
)

// SwarmSecretMinLength is the shortest SHARED_CACHE_SECRET
// accepted. It is a bearer credential for the whole swarm, not
// a password to remember; generate it like the token key
// (openssl rand -hex 32).
const SwarmSecretMinLength = 32

// SwarmConfig is the shared cache's configuration (SHARED_CACHE,
// SHARED_CACHE_SECRET, SHARED_CACHE_SEEDS,
// SHARED_CACHE_GOSSIP_HOURS). err is set when the operator asked
// for sharing but the settings cannot run it safely; New refuses
// to start on it, the way a broken sign-up restriction refuses.
type SwarmConfig struct {
	Mode        string // off, open, allowlist
	Secret      string
	Seeds       []string
	GossipHours int
	// warms are the shared kinds this instance fetches for the
	// swarm as a dedicated warming node (SHARED_CACHE_WARM,
	// swarm/warm.go); empty on an ordinary instance.
	Warms map[string]bool
	Err   error
}

// Enabled reports whether the instance shares anything at all.
func (s SwarmConfig) Enabled() bool { return s.Mode == SwarmOpen || s.Mode == SwarmAllowlist }

// Allowlisted reports whether peers need approval before
// anything is read from them.
func (s SwarmConfig) Allowlisted() bool { return s.Mode == SwarmAllowlist }

func LoadSwarmConfig(getenv func(string) string) SwarmConfig {
	var s SwarmConfig
	raw := strings.ToLower(strings.TrimSpace(getenv("SHARED_CACHE")))
	switch raw {
	case "", SwarmOff:
		s.Mode = SwarmOff
	case SwarmOpen, "on":
		// "on" is the plain-English spelling of open.
		s.Mode = SwarmOpen
	case SwarmAllowlist:
		s.Mode = raw
	default:
		logging.Warnf("evesynapse: SHARED_CACHE=%q is not off, open or allowlist; the shared cache stays off", getenv("SHARED_CACHE"))
		s.Mode = SwarmOff
		return s
	}
	if !s.Enabled() {
		return s
	}
	s.Secret = strings.TrimSpace(getenv("SHARED_CACHE_SECRET"))
	if len(s.Secret) < SwarmSecretMinLength {
		if s.Secret == "" {
			s.Err = fmt.Errorf("SHARED_CACHE=%s needs a SHARED_CACHE_SECRET (at least %d random characters, for example: openssl rand -hex 32)", s.Mode, SwarmSecretMinLength)
		} else {
			s.Err = fmt.Errorf("SHARED_CACHE_SECRET is %d characters long; it needs at least %d random ones (for example: openssl rand -hex 32)", len(s.Secret), SwarmSecretMinLength)
		}
		return s
	}
	seeds, err := ParseSwarmSeeds(getenv("SHARED_CACHE_SEEDS"))
	if err != nil {
		s.Err = err
		return s
	}
	s.Seeds = seeds
	s.GossipHours = ParseSwarmGossipHours(getenv("SHARED_CACHE_GOSSIP_HOURS"))
	s.Warms = ParseSwarmWarm(getenv("SHARED_CACHE_WARM"))
	return s
}

// ParseSwarmSeeds reads the bootstrap/fallback directory nodes:
// the instances a new or lonely one asks for the membership
// list. Each must be an https URL, except loopback (local
// development and tests), where plain http never leaves the
// machine. Anything else refuses to start rather than gossip
// the shared secret somewhere unsafe.
func ParseSwarmSeeds(raw string) ([]string, error) {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		u, err := url.Parse(part)
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("SHARED_CACHE_SEEDS: %q is not a URL", part)
		}
		switch strings.ToLower(u.Scheme) {
		case "https":
		case "http":
			if !SwarmHostIsLoopback(u.Hostname()) {
				return nil, fmt.Errorf("SHARED_CACHE_SEEDS: %q must be https (http is only allowed on loopback)", part)
			}
		default:
			return nil, fmt.Errorf("SHARED_CACHE_SEEDS: %q must be https (http is only allowed on loopback)", part)
		}
		out = append(out, strings.TrimSuffix(part, "/"))
	}
	return out, nil
}

// SwarmHostIsLoopback reports whether a hostname is this machine
// itself: localhost names or a loopback IP.
func SwarmHostIsLoopback(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ParseSwarmGossipHours reads SHARED_CACHE_GOSSIP_HOURS: unset or
// unreadable means the default; anything outside 12–168 hours is
// kept in bounds (which the log says).
func ParseSwarmGossipHours(raw string) int {
	return intSetting("SHARED_CACHE_GOSSIP_HOURS", raw, MinSwarmGossipHours, MaxSwarmGossipHours, DefaultSwarmGossipHours)
}
