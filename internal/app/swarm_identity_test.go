package app

// White-box tests for swarm identity and entry signatures: a
// sealed seed round-trips under the right secret only, entries
// verify under the pinned key only, and any tampering —
// payload, key, signer, signature — fails closed.

import (
	"strings"
	"testing"
	"time"
)

func TestSwarmSealRoundtrip(t *testing.T) {
	id, err := mintSwarmIdentity()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	secret := strings.Repeat("s", 40)
	sealed, err := sealSwarmSeed(id.private.Seed(), secret)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if !strings.HasPrefix(sealed, swarmSeedPrefix) {
		t.Fatalf("sealed %q lacks the envelope prefix", sealed)
	}
	opened, err := unsealSwarmSeed(sealed, secret)
	if err != nil {
		t.Fatalf("unseal: %v", err)
	}
	if string(opened) != string(id.private.Seed()) {
		t.Fatalf("unseal returned a different seed")
	}
}

func TestSwarmSealWrongSecret(t *testing.T) {
	id, err := mintSwarmIdentity()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	sealed, err := sealSwarmSeed(id.private.Seed(), strings.Repeat("s", 40))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := unsealSwarmSeed(sealed, strings.Repeat("x", 40)); err == nil {
		t.Fatalf("unseal with wrong secret succeeded")
	}
	if _, ok := openSwarmIdentity("some-id", id.publicString(), sealed, strings.Repeat("x", 40)); ok {
		t.Fatalf("open with wrong secret succeeded")
	}
}

func TestSwarmSignVerify(t *testing.T) {
	a, err := mintSwarmIdentity()
	if err != nil {
		t.Fatalf("mint a: %v", err)
	}
	b, err := mintSwarmIdentity()
	if err != nil {
		t.Fatalf("mint b: %v", err)
	}
	entry, err := signSwarmEntry(a, "instance-a", "market_history", "10000002/34", `[{"date":"2026-10-01"}]`, time.Now())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if !verifySwarmEntry(entry, a.publicString()) {
		t.Fatalf("fresh signature does not verify")
	}
	// Wrong key: B's key must not verify A's signature.
	if verifySwarmEntry(entry, b.publicString()) {
		t.Fatalf("signature verifies under another instance's key")
	}
	// Every tamper fails: payload, key, signer, and a flipped
	// signature byte.
	tampered := entry
	tampered.Payload = `[{"date":"2026-10-02"}]`
	if verifySwarmEntry(tampered, a.publicString()) {
		t.Fatalf("tampered payload verifies")
	}
	tampered = entry
	tampered.Key = "10000002/35"
	if verifySwarmEntry(tampered, a.publicString()) {
		t.Fatalf("tampered key verifies")
	}
	tampered = entry
	tampered.Signer = "instance-evil"
	if verifySwarmEntry(tampered, a.publicString()) {
		t.Fatalf("tampered signer verifies")
	}
	tampered = entry
	sig := tampered.Signature
	if sig[len(sig)-1] == 'A' {
		tampered.Signature = sig[:len(sig)-1] + "B"
	} else {
		tampered.Signature = sig[:len(sig)-1] + "A"
	}
	if verifySwarmEntry(tampered, a.publicString()) {
		t.Fatalf("flipped signature verifies")
	}
	// Malformed inputs fail, not panic.
	if verifySwarmEntry(swarmSignedEntry{}, a.publicString()) {
		t.Fatalf("empty entry verifies")
	}
	if verifySwarmEntry(entry, "not-base64!!") {
		t.Fatalf("entry verifies under garbage key")
	}
}

func TestSwarmSignPayloadCap(t *testing.T) {
	id, err := mintSwarmIdentity()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	big := strings.Repeat("x", maxSwarmPayloadBytes+1)
	if _, err := signSwarmEntry(id, "i", "market_history", "k", big, time.Now()); err == nil {
		t.Fatalf("oversize payload signed")
	}
	huge := swarmSignedEntry{Kind: "market_history", Key: "k", Payload: big, Signer: "i", Signature: "e30="}
	if verifySwarmEntry(huge, id.publicString()) {
		t.Fatalf("oversize payload verifies")
	}
}
