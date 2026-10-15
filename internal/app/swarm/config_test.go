package swarm

// White-box tests for the shared-cache configuration: off by
// default, explicit modes, and fail-closed validation —
// enabling without a strong secret, or with an unsafe seed,
// refuses to start rather than sharing in the clear.

import (
	"strings"
	"testing"
)

func swarmEnv(vars map[string]string) func(string) string {
	return func(key string) string { return vars[key] }
}

func TestSwarmOffByDefault(t *testing.T) {
	s := LoadSwarmConfig(swarmEnv(nil))
	if s.Enabled() {
		t.Fatalf("empty env: Enabled, want off")
	}
	if s.Err != nil {
		t.Fatalf("empty env: err %v, want none", s.Err)
	}
}

func TestSwarmUnknownModeStaysOff(t *testing.T) {
	s := LoadSwarmConfig(swarmEnv(map[string]string{
		"SHARED_CACHE":        "yes",
		"SHARED_CACHE_SECRET": strings.Repeat("s", 40),
	}))
	if s.Enabled() {
		t.Fatalf("SHARED_CACHE=yes: Enabled, want off")
	}
	if s.Err != nil {
		t.Fatalf("SHARED_CACHE=yes: err %v, want none (off is safe)", s.Err)
	}
}

func TestSwarmOnMeansOpen(t *testing.T) {
	s := LoadSwarmConfig(swarmEnv(map[string]string{
		"SHARED_CACHE":        "on",
		"SHARED_CACHE_SECRET": strings.Repeat("s", 32),
	}))
	if !s.Enabled() || s.Allowlisted() || s.Err != nil || s.Mode != SwarmOpen {
		t.Fatalf("on: Enabled=%v Allowlisted=%v mode=%q err=%v", s.Enabled(), s.Allowlisted(), s.Mode, s.Err)
	}
}

func TestSwarmNeedsSecret(t *testing.T) {
	for _, mode := range []string{"open", "allowlist", "OPEN", "on"} {
		s := LoadSwarmConfig(swarmEnv(map[string]string{"SHARED_CACHE": mode}))
		if s.Err == nil {
			t.Fatalf("SHARED_CACHE=%s without secret: no error, want refusal", mode)
		}
		s = LoadSwarmConfig(swarmEnv(map[string]string{
			"SHARED_CACHE":        mode,
			"SHARED_CACHE_SECRET": "too-short",
		}))
		if s.Err == nil {
			t.Fatalf("SHARED_CACHE=%s with short secret: no error, want refusal", mode)
		}
	}
}

func TestSwarmEnabledModes(t *testing.T) {
	open := LoadSwarmConfig(swarmEnv(map[string]string{
		"SHARED_CACHE":        "open",
		"SHARED_CACHE_SECRET": strings.Repeat("s", 32),
	}))
	if !open.Enabled() || open.Allowlisted() || open.Err != nil {
		t.Fatalf("open: Enabled=%v Allowlisted=%v err=%v", open.Enabled(), open.Allowlisted(), open.Err)
	}
	allow := LoadSwarmConfig(swarmEnv(map[string]string{
		"SHARED_CACHE":        "allowlist",
		"SHARED_CACHE_SECRET": strings.Repeat("s", 32),
	}))
	if !allow.Enabled() || !allow.Allowlisted() || allow.Err != nil {
		t.Fatalf("allowlist: Enabled=%v Allowlisted=%v err=%v", allow.Enabled(), allow.Allowlisted(), allow.Err)
	}
}

func TestSwarmSeeds(t *testing.T) {
	good := LoadSwarmConfig(swarmEnv(map[string]string{
		"SHARED_CACHE":        "open",
		"SHARED_CACHE_SECRET": strings.Repeat("s", 32),
		"SHARED_CACHE_SEEDS":  "https://seed.example.org/, Http://127.0.0.1:8080",
	}))
	if good.Err != nil {
		t.Fatalf("good seeds: err %v", good.Err)
	}
	if len(good.Seeds) != 2 || good.Seeds[0] != "https://seed.example.org" {
		t.Fatalf("good seeds: %q", good.Seeds)
	}
	bad := LoadSwarmConfig(swarmEnv(map[string]string{
		"SHARED_CACHE":        "open",
		"SHARED_CACHE_SECRET": strings.Repeat("s", 32),
		"SHARED_CACHE_SEEDS":  "https://seed.example.org/, Http://seed.example.org/",
	}))
	if bad.Err == nil {
		t.Fatalf("cleartext seed: no error, want refusal")
	}
}

func TestSwarmGossipHours(t *testing.T) {
	if got := ParseSwarmGossipHours(""); got != DefaultSwarmGossipHours {
		t.Fatalf("unset: %d, want %d", got, DefaultSwarmGossipHours)
	}
	if got := ParseSwarmGossipHours("20"); got != 20 {
		t.Fatalf("20: %d", got)
	}
	if got := ParseSwarmGossipHours("1"); got != MinSwarmGossipHours {
		t.Fatalf("1: %d, want floor %d", got, MinSwarmGossipHours)
	}
	if got := ParseSwarmGossipHours("9999"); got != MaxSwarmGossipHours {
		t.Fatalf("9999: %d, want cap %d", got, MaxSwarmGossipHours)
	}
}
