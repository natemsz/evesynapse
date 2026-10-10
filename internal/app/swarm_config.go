package app

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
	swarmOff       = "off"
	swarmOpen      = "open"
	swarmAllowlist = "allowlist"
)

// Defaults and bounds for the gossip rate: membership spreads
// peer to peer about once a day, jittered, so discovery traffic
// stays near zero. Below 12 hours is refused: gossip is for
// slow membership drift, not for data.
const (
	defaultSwarmGossipHours = 18
	minSwarmGossipHours     = 12
	maxSwarmGossipHours     = 168
)

// swarmSecretMinLength is the shortest SHARED_CACHE_SECRET
// accepted. It is a bearer credential for the whole swarm, not
// a password to remember; generate it like the token key
// (openssl rand -hex 32).
const swarmSecretMinLength = 32

// swarmConfig is the shared cache's configuration (SHARED_CACHE,
// SHARED_CACHE_SECRET, SHARED_CACHE_SEEDS,
// SHARED_CACHE_GOSSIP_HOURS). err is set when the operator asked
// for sharing but the settings cannot run it safely; New refuses
// to start on it, the way a broken sign-up restriction refuses.
type swarmConfig struct {
	mode        string // off, open, allowlist
	secret      string
	seeds       []string
	gossipHours int
	err         error
}

// enabled reports whether the instance shares anything at all.
func (s swarmConfig) enabled() bool { return s.mode == swarmOpen || s.mode == swarmAllowlist }

// allowlisted reports whether peers need approval before
// anything is read from them.
func (s swarmConfig) allowlisted() bool { return s.mode == swarmAllowlist }

func loadSwarmConfig(getenv func(string) string) swarmConfig {
	var s swarmConfig
	raw := strings.ToLower(strings.TrimSpace(getenv("SHARED_CACHE")))
	switch raw {
	case "", swarmOff:
		s.mode = swarmOff
	case swarmOpen, swarmAllowlist:
		s.mode = raw
	default:
		logging.Warnf("evesynapse: SHARED_CACHE=%q is not off, open or allowlist; the shared cache stays off", getenv("SHARED_CACHE"))
		s.mode = swarmOff
		return s
	}
	if !s.enabled() {
		return s
	}
	s.secret = strings.TrimSpace(getenv("SHARED_CACHE_SECRET"))
	if len(s.secret) < swarmSecretMinLength {
		if s.secret == "" {
			s.err = fmt.Errorf("SHARED_CACHE=%s needs a SHARED_CACHE_SECRET (at least %d random characters, for example: openssl rand -hex 32)", s.mode, swarmSecretMinLength)
		} else {
			s.err = fmt.Errorf("SHARED_CACHE_SECRET is %d characters long; it needs at least %d random ones (for example: openssl rand -hex 32)", len(s.secret), swarmSecretMinLength)
		}
		return s
	}
	seeds, err := parseSwarmSeeds(getenv("SHARED_CACHE_SEEDS"))
	if err != nil {
		s.err = err
		return s
	}
	s.seeds = seeds
	s.gossipHours = parseSwarmGossipHours(getenv("SHARED_CACHE_GOSSIP_HOURS"))
	return s
}

// parseSwarmSeeds reads the bootstrap/fallback directory nodes:
// the instances a new or lonely one asks for the membership
// list. Each must be an https URL, except loopback (local
// development and tests), where plain http never leaves the
// machine. Anything else refuses to start rather than gossip
// the shared secret somewhere unsafe.
func parseSwarmSeeds(raw string) ([]string, error) {
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
			if !swarmHostIsLoopback(u.Hostname()) {
				return nil, fmt.Errorf("SHARED_CACHE_SEEDS: %q must be https (http is only allowed on loopback)", part)
			}
		default:
			return nil, fmt.Errorf("SHARED_CACHE_SEEDS: %q must be https (http is only allowed on loopback)", part)
		}
		out = append(out, strings.TrimSuffix(part, "/"))
	}
	return out, nil
}

// swarmHostIsLoopback reports whether a hostname is this machine
// itself: localhost names or a loopback IP.
func swarmHostIsLoopback(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// parseSwarmGossipHours reads SHARED_CACHE_GOSSIP_HOURS: unset or
// unreadable means the default; anything outside 12–168 hours is
// kept in bounds (which the log says).
func parseSwarmGossipHours(raw string) int {
	return intSetting("SHARED_CACHE_GOSSIP_HOURS", raw, minSwarmGossipHours, maxSwarmGossipHours, defaultSwarmGossipHours)
}
