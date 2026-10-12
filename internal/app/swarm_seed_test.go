package app

// Seed announcement tests: the heartbeat entry verifies under
// the instance key, carries the dialable origin, stays silent
// without one, and the sender speaks the worker's strict shape
// with the secret in the bearer header only.

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestSeedAnnounceVerifies builds a heartbeat the way the
// worker will check it: kind/key/signer coherence and an
// Ed25519 signature over the canonical envelope.
func TestSeedAnnounceVerifies(t *testing.T) {
	app := swarmTestApp(t)
	app.cfg.swarm = swarmConfig{mode: swarmOpen, secret: strings.Repeat("s", 32)}
	app.cfg.eveCallbackURL = "https://node.example.org/auth/callback"
	entry, ok := app.buildSeedAnnounce(time.Now())
	if !ok {
		t.Fatal("no announce built for a sharing instance with an origin")
	}
	if entry.Kind != seedAnnounceKind || entry.Key != app.swarmID.id || entry.Signer != app.swarmID.id {
		t.Fatalf("announce identity wrong: %+v", entry)
	}
	if !verifySwarmEntry(entry, app.swarmID.publicString()) {
		t.Fatal("announce does not verify under the instance key")
	}
	var payload seedAnnouncePayload
	if err := json.Unmarshal([]byte(entry.Payload), &payload); err != nil {
		t.Fatalf("announce payload is not JSON: %v", err)
	}
	if payload.BaseURL != "https://node.example.org" || payload.PublicKey != app.swarmID.publicString() {
		t.Fatalf("announce payload wrong: %+v", payload)
	}
}

// TestSeedAnnounceSilent covers the stays-dark cases: sharing
// off, no public address, or a non-https origin nobody could
// dial anyway.
func TestSeedAnnounceSilent(t *testing.T) {
	app := swarmTestApp(t)
	app.cfg.swarm = swarmConfig{mode: swarmOff, secret: strings.Repeat("s", 32)}
	app.cfg.eveCallbackURL = "https://node.example.org/auth/callback"
	if _, ok := app.buildSeedAnnounce(time.Now()); ok {
		t.Fatal("announce built while sharing is off")
	}
	app.cfg.swarm = swarmConfig{mode: swarmOpen, secret: strings.Repeat("s", 32)}
	app.cfg.eveCallbackURL = ""
	if _, ok := app.buildSeedAnnounce(time.Now()); ok {
		t.Fatal("announce built with no public origin")
	}
	app.cfg.eveCallbackURL = "http://localhost:8080/auth/callback"
	if _, ok := app.buildSeedAnnounce(time.Now()); ok {
		t.Fatal("announce built for an undialable origin")
	}
}

// TestSeedAnnounceSender posts the heartbeat the way the
// worker expects it: bearer secret, strict entry shape, and an
// {ok} answer required.
func TestSeedAnnounceSender(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	var gotAuth, gotType string
	var gotEntry swarmSignedEntry
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotType = r.Header.Get("Content-Type")
		if r.URL.Path != "/swarm/seed/register" {
			t.Errorf("path %q", r.URL.Path)
		}
		raw, _ := io.ReadAll(io.LimitReader(r.Body, maxSwarmBodyBytes))
		var body struct {
			Entry swarmSignedEntry `json:"entry"`
		}
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			t.Errorf("strict decode: %v", err)
		}
		gotEntry = body.Entry
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	kp, err := mintSwarmIdentity()
	if err != nil {
		t.Fatalf("mint identity: %v", err)
	}
	kp.id = "id-1"
	entry, _ := signSwarmEntry(kp, "id-1", seedAnnounceKind, "id-1", `{"base_url":"https://x.example","public_key":"k"}`, time.Now())
	cli := newSwarmClient(secret)
	if err := cli.sendSeedAnnounce(context.Background(), srv.URL, entry); err != nil {
		t.Fatalf("send: %v", err)
	}
	if gotAuth != "Bearer "+secret {
		t.Fatalf("auth header %q", gotAuth)
	}
	if gotType != "application/json" {
		t.Fatalf("content type %q", gotType)
	}
	if gotEntry.Kind != seedAnnounceKind || gotEntry.Signer != "id-1" {
		t.Fatalf("entry mangled: %+v", gotEntry)
	}
	if strings.Contains(string(mustJSON(t, entry)), secret) {
		t.Fatal("secret leaked into the body")
	}
}

// TestSeedMembersKeyVector pins the member-list sealing key
// against an independently computed HKDF-SHA-256 vector (.NET
// HMAC-SHA256, extract key=salt then expand info||0x01). The
// worker implements the same parameters in WebCrypto; if either
// side's salt, info, or length drifts, sealed lists stop
// opening and this goes red first.
func TestSeedMembersKeyVector(t *testing.T) {
	key, err := swarmMembersKey("test-secret-for-seed-worker-0001")
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	const want = "0c03a1ede796e347591d73795627def713853f58b946d14723f754621fa46b30"
	if got := hex.EncodeToString(key); got != want {
		t.Fatalf("sealing key %s, want %s", got, want)
	}
}

// TestSeedEnvelopeBytes pins Go's half of the signing bytes the
// worker must reproduce: struct field order, compact form, and
// <>& escaping. The worker's goString implements the same
// rules; any drift breaks verification and lands here first.
func TestSeedEnvelopeBytes(t *testing.T) {
	doc, err := json.Marshal(swarmEnvelope{V: 1, Kind: "announce", Key: "id-1", Payload: `{"a":"<b>&"}`, FetchedAt: 1700000000, Signer: "id-1"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const want = `{"v":1,"kind":"announce","key":"id-1","payload":"{\"a\":\"\u003cb\u003e\u0026\"}","fetched_at":1700000000,"signer":"id-1"}`
	if string(doc) != want {
		t.Fatalf("envelope bytes:\n got %s\nwant %s", doc, want)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}
