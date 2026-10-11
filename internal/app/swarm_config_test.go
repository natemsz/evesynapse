package app

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
	s := loadSwarmConfig(swarmEnv(nil))
	if s.enabled() {
		t.Fatalf("empty env: enabled, want off")
	}
	if s.err != nil {
		t.Fatalf("empty env: err %v, want none", s.err)
	}
}

func TestSwarmUnknownModeStaysOff(t *testing.T) {
	s := loadSwarmConfig(swarmEnv(map[string]string{
		"SHARED_CACHE":        "on",
		"SHARED_CACHE_SECRET": strings.Repeat("s", 40),
	}))
	if s.enabled() {
		t.Fatalf("SHARED_CACHE=on: enabled, want off")
	}
	if s.err != nil {
		t.Fatalf("SHARED_CACHE=on: err %v, want none (off is safe)", s.err)
	}
}

func TestSwarmOnMeansOpen(t *testing.T) {
	s := loadSwarmConfig(swarmEnv(map[string]string{
		"SHARED_CACHE":        "on",
		"SHARED_CACHE_SECRET": strings.Repeat("s", 32),
	}))
	if !s.enabled() || s.allowlisted() || s.err != nil || s.mode != swarmOpen {
		t.Fatalf("on: enabled=%v allowlisted=%v mode=%q err=%v", s.enabled(), s.allowlisted(), s.mode, s.err)
	}
}

func TestSwarmNeedsSecret(t *testing.T) {
	for _, mode := range []string{"open", "allowlist", "OPEN", "on"} {
		s := loadSwarmConfig(swarmEnv(map[string]string{"SHARED_CACHE": mode}))
		if s.err == nil {
			t.Fatalf("SHARED_CACHE=%s without secret: no error, want refusal", mode)
		}
		s = loadSwarmConfig(swarmEnv(map[string]string{
			"SHARED_CACHE":        mode,
			"SHARED_CACHE_SECRET": "too-short",
		}))
		if s.err == nil {
			t.Fatalf("SHARED_CACHE=%s with short secret: no error, want refusal", mode)
		}
	}
}

func TestSwarmEnabledModes(t *testing.T) {
	open := loadSwarmConfig(swarmEnv(map[string]string{
		"SHARED_CACHE":        "open",
		"SHARED_CACHE_SECRET": strings.Repeat("s", 32),
	}))
	if !open.enabled() || open.allowlisted() || open.err != nil {
		t.Fatalf("open: enabled=%v allowlisted=%v err=%v", open.enabled(), open.allowlisted(), open.err)
	}
	allow := loadSwarmConfig(swarmEnv(map[string]string{
		"SHARED_CACHE":        "allowlist",
		"SHARED_CACHE_SECRET": strings.Repeat("s", 32),
	}))
	if !allow.enabled() || !allow.allowlisted() || allow.err != nil {
		t.Fatalf("allowlist: enabled=%v allowlisted=%v err=%v", allow.enabled(), allow.allowlisted(), allow.err)
	}
}

func TestSwarmSeeds(t *testing.T) {
	good := loadSwarmConfig(swarmEnv(map[string]string{
		"SHARED_CACHE":        "open",
		"SHARED_CACHE_SECRET": strings.Repeat("s", 32),
		"SHARED_CACHE_SEEDS":  "https://seed.example.org/, http://127.0.0.1:8080",
	}))
	if good.err != nil {
		t.Fatalf("good seeds: err %v", good.err)
	}
	if len(good.seeds) != 2 || good.seeds[0] != "https://seed.example.org" {
		t.Fatalf("good seeds: %q", good.seeds)
	}
	bad := loadSwarmConfig(swarmEnv(map[string]string{
		"SHARED_CACHE":        "open",
		"SHARED_CACHE_SECRET": strings.Repeat("s", 32),
		"SHARED_CACHE_SEEDS":  "https://seed.example.org/, http://seed.example.org/",
	}))
	if bad.err == nil {
		t.Fatalf("cleartext seed: no error, want refusal")
	}
}

func TestSwarmGossipHours(t *testing.T) {
	if got := parseSwarmGossipHours(""); got != defaultSwarmGossipHours {
		t.Fatalf("unset: %d, want %d", got, defaultSwarmGossipHours)
	}
	if got := parseSwarmGossipHours("20"); got != 20 {
		t.Fatalf("20: %d", got)
	}
	if got := parseSwarmGossipHours("1"); got != minSwarmGossipHours {
		t.Fatalf("1: %d, want floor %d", got, minSwarmGossipHours)
	}
	if got := parseSwarmGossipHours("9999"); got != maxSwarmGossipHours {
		t.Fatalf("9999: %d, want cap %d", got, maxSwarmGossipHours)
	}
}
