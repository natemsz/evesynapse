package app

// White-box tests for swarm member privacy: the member list
// seals under the shared secret (members alone decode), and
// pages alias gossip-learned members while hand-added and seed
// addresses show plainly.

import (
	"evesynapse/internal/app/swarm"
	"strings"
	"testing"
)

func TestSwarmMembersSealRoundtrip(t *testing.T) {
	secret := strings.Repeat("s", 40)
	raw := []byte(`[{"instance_id":"abc","base_url":"https://swarm.example.org/","public_key":"e30="}]`)
	sealed, err := swarm.SealSwarmMembers(raw, secret)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if !strings.HasPrefix(sealed, swarm.SwarmMembersPrefix) {
		t.Fatalf("sealed %q lacks the envelope prefix", sealed)
	}
	if strings.Contains(sealed, "swarm.example.org") {
		t.Fatalf("sealed list leaks the address in cleartext")
	}
	opened, err := swarm.OpenSwarmMembers(sealed, secret)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if string(opened) != string(raw) {
		t.Fatalf("open returned different bytes")
	}
}

func TestSwarmMembersSealWrongSecret(t *testing.T) {
	sealed, err := swarm.SealSwarmMembers([]byte(`[]`), strings.Repeat("s", 40))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := swarm.OpenSwarmMembers(sealed, strings.Repeat("x", 40)); err == nil {
		t.Fatalf("opened under the wrong secret")
	}
	if _, err := swarm.OpenSwarmMembers("swm:v1:!!!not-base64!!!", strings.Repeat("s", 40)); err == nil {
		t.Fatalf("opened garbage")
	}
	// Flipping a sealed byte fails closed (AES-GCM rejects it).
	tampered := sealed[:len(sealed)-1]
	if sealed[len(sealed)-1] == 'A' {
		tampered += "B"
	} else {
		tampered += "A"
	}
	if _, err := swarm.OpenSwarmMembers(tampered, strings.Repeat("s", 40)); err == nil {
		t.Fatalf("opened a tampered envelope")
	}
}

func TestSwarmAliasShape(t *testing.T) {
	secret := strings.Repeat("s", 40)
	first := swarm.SwarmAlias(secret, "peer-a")
	parts := strings.Split(first, ".")
	if len(parts) != 3 {
		t.Fatalf("alias %q is not adjective.noun.number", first)
	}
	inList := func(word string, list []string) bool {
		for _, w := range list {
			if w == word {
				return true
			}
		}
		return false
	}
	if !inList(parts[0], swarm.SwarmAliasAdjectives) || !inList(parts[1], swarm.SwarmAliasNouns) {
		t.Fatalf("alias %q uses words outside the pools", first)
	}
	if parts[2] == "" {
		t.Fatalf("alias %q has no number", first)
	}
	for _, c := range parts[2] {
		if c < '0' || c > '9' {
			t.Fatalf("alias %q has a non-numeric suffix", first)
		}
	}
	// Stable per swarm, distinct per id and per secret.
	if got := swarm.SwarmAlias(secret, "peer-a"); got != first {
		t.Fatalf("alias unstable: %q then %q", first, got)
	}
	if got := swarm.SwarmAlias(secret, "peer-b"); got == first {
		t.Fatalf("two ids share alias %q", first)
	}
	if got := swarm.SwarmAlias(strings.Repeat("x", 40), "peer-a"); got == first {
		t.Fatalf("two swarms share alias %q", first)
	}
}

func TestSwarmDisplayRule(t *testing.T) {
	app := swarmTestApp(t)
	swarm.StateFor(app).Cfg.Seeds = []string{"https://evesynapse.app"}
	gossip := swarm.StateFor(app).SwarmDisplayPeer("peer-gossip-1", "https://node7.example.net/", "gossip")
	if !strings.Contains(gossip, ".") || strings.Contains(gossip, "node7") {
		t.Fatalf("gossip peer shows %q, want the alias", gossip)
	}
	manual := swarm.StateFor(app).SwarmDisplayPeer("peer-manual-1", "https://friend.example.org/", "manual")
	if manual != "https://friend.example.org/" {
		t.Fatalf("hand-added peer shows %q, want the address", manual)
	}
	seed := swarm.StateFor(app).SwarmDisplayPeer("peer-seed-1", "https://evesynapse.app/", "seed")
	if seed != "https://evesynapse.app/" {
		t.Fatalf("configured seed shows %q, want the address", seed)
	}
	// A seed-sourced peer that is not itself a configured seed
	// still aliases: learning *through* evesynapse.app is not
	// being it.
	via := swarm.StateFor(app).SwarmDisplayPeer("peer-via-1", "https://node9.example.net/", "seed")
	if strings.Contains(via, "node9") {
		t.Fatalf("peer learned via a seed shows %q, want the alias", via)
	}
}
