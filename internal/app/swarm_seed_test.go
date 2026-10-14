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

	db "evesynapse/internal/db/sqlc"
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
	// The payload is the member tuple a seed stores and relays.
	if !verifySwarmMember(swarmMember{InstanceID: app.swarmID.id, BaseURL: payload.BaseURL, PublicKey: payload.PublicKey, URLSig: payload.URLSig, IssuedAt: payload.IssuedAt}) {
		t.Fatal("announce carries no valid signature over its address")
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
	var gotAuth, gotType, gotBody string
	var gotEntry swarmSignedEntry
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotType = r.Header.Get("Content-Type")
		if r.URL.Path != "/swarm/seed/register" {
			t.Errorf("path %q", r.URL.Path)
		}
		raw, _ := io.ReadAll(io.LimitReader(r.Body, maxSwarmBodyBytes))
		gotBody = string(raw)
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
	// The bytes that went over the wire, not the entry as built.
	if gotBody == "" || strings.Contains(gotBody, secret) {
		t.Fatalf("secret leaked into the body, or no body arrived: %q", gotBody)
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

// TestSwarmGossipAnnouncesToSeeds: the membership round ends
// with one heartbeat per configured seed, carrying an entry
// that verifies under the instance key, and the next immediate
// round sends nothing (it is not due).
func TestSwarmGossipAnnouncesToSeeds(t *testing.T) {
	var hits int
	var gotAuth string
	var gotEntry swarmSignedEntry
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/swarm/seed/register" {
			http.NotFound(w, r)
			return
		}
		hits++
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(io.LimitReader(r.Body, maxSwarmBodyBytes))
		var body struct {
			Entry swarmSignedEntry `json:"entry"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("decode heartbeat: %v", err)
		}
		gotEntry = body.Entry
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	app := swarmTestApp(t)
	app.cfg.swarm.seeds = []string{srv.URL}
	app.cfg.eveCallbackURL = "https://node.example.org/auth/callback"
	now := time.Now()
	app.swarmGossip(context.Background(), now)
	if hits != 1 {
		t.Fatalf("heartbeats sent: %d, want 1", hits)
	}
	if gotAuth != "Bearer "+swarmTestSecret {
		t.Fatalf("auth header %q", gotAuth)
	}
	if gotEntry.Kind != seedAnnounceKind || gotEntry.Key != app.swarmID.id || gotEntry.Signer != app.swarmID.id {
		t.Fatalf("heartbeat identity wrong: %+v", gotEntry)
	}
	if !verifySwarmEntry(gotEntry, app.swarmID.publicString()) {
		t.Fatal("heartbeat does not verify under the instance key")
	}
	app.swarmGossip(context.Background(), now)
	if hits != 1 {
		t.Fatalf("second round re-announced: %d hits, want 1", hits)
	}
}

// TestSwarmGossipSkipsAnnounceWhenNothingToSay: no seeds and
// no dialable origin means the round runs but dials nothing.
func TestSwarmGossipSkipsAnnounceWhenNothingToSay(t *testing.T) {
	app := swarmTestApp(t)
	app.cfg.swarm.seeds = nil
	app.cfg.eveCallbackURL = ""
	now := time.Now()
	app.swarmGossip(context.Background(), now)
	if app.swarmGossipDue(context.Background(), now) {
		t.Fatal("gossip round did not complete")
	}
}

// seedTestHeartbeat signs one self-announce for an arbitrary
// identity, the way a remote instance would.
func seedTestHeartbeat(t *testing.T, kp *swarmIdentity, baseURL string) swarmSignedEntry {
	t.Helper()
	issued := time.Now().Unix()
	payload := string(mustJSON(t, seedAnnouncePayload{BaseURL: baseURL, PublicKey: kp.publicString(), URLSig: signSwarmMember(kp, baseURL, issued), IssuedAt: issued}))
	entry, err := signSwarmEntry(kp, kp.id, seedAnnounceKind, kp.id, payload, time.Now())
	if err != nil {
		t.Fatalf("sign heartbeat: %v", err)
	}
	return entry
}

func seedTestRegister(t *testing.T, app *Application, entry swarmSignedEntry, secret string) (int, bool) {
	t.Helper()
	raw := string(mustJSON(t, map[string]any{"entry": entry}))
	code, body := swarmServe(t, app, http.MethodPost, "/swarm/seed/register", raw, secret)
	var decoded struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		return code, false
	}
	return code, decoded.OK
}

// TestSwarmSeedRegisterAccepts: a verifiable heartbeat files
// the peer through the gossip path: approved in open mode,
// waiting in allowlist mode.
func TestSwarmSeedRegisterAccepts(t *testing.T) {
	ctx := context.Background()
	app := swarmTestApp(t)
	kp, err := mintSwarmIdentity()
	if err != nil {
		t.Fatalf("mint peer: %v", err)
	}
	kp.id = "peer-1"
	code, ok := seedTestRegister(t, app, seedTestHeartbeat(t, kp, "https://peer-1.example"), swarmTestSecret)
	if code != http.StatusOK || !ok {
		t.Fatalf("register: status %d ok %v", code, ok)
	}
	peer, err := app.queries.GetSwarmPeer(ctx, "peer-1")
	if err != nil {
		t.Fatalf("peer not filed: %v", err)
	}
	if peer.BaseUrl != "https://peer-1.example" || peer.PublicKey != kp.publicString() || peer.Source != "gossip" {
		t.Fatalf("peer row wrong: %+v", peer)
	}
	if !peer.Approved {
		t.Fatal("open mode did not approve the registered peer")
	}

	listed := swarmTestApp(t)
	listed.cfg.swarm.mode = swarmAllowlist
	kp2, err := mintSwarmIdentity()
	if err != nil {
		t.Fatalf("mint peer: %v", err)
	}
	kp2.id = "peer-2"
	code, ok = seedTestRegister(t, listed, seedTestHeartbeat(t, kp2, "https://peer-2.example"), swarmTestSecret)
	if code != http.StatusOK || !ok {
		t.Fatalf("register in allowlist mode: status %d ok %v", code, ok)
	}
	peer2, err := listed.queries.GetSwarmPeer(ctx, "peer-2")
	if err != nil {
		t.Fatalf("peer not filed in allowlist mode: %v", err)
	}
	if peer2.Approved {
		t.Fatal("allowlist mode approved a peer nobody approved")
	}
}

// TestSwarmSeedRegisterRejects: anything that does not verify
// as the sender's own self-signed heartbeat is refused, and a
// heartbeat from self is a no-op, never a peer row.
func TestSwarmSeedRegisterRejects(t *testing.T) {
	ctx := context.Background()
	app := swarmTestApp(t)
	kp, err := mintSwarmIdentity()
	if err != nil {
		t.Fatalf("mint peer: %v", err)
	}
	kp.id = "peer-9"
	good := seedTestHeartbeat(t, kp, "https://peer-9.example")

	badSig := good
	badSig.Payload += "x"
	if code, _ := seedTestRegister(t, app, badSig, swarmTestSecret); code != http.StatusBadRequest {
		t.Fatalf("tampered signature: status %d, want 400", code)
	}
	wrongKind, err := signSwarmEntry(kp, kp.id, swarmKindMembers, kp.id, good.Payload, time.Now())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if code, _ := seedTestRegister(t, app, wrongKind, swarmTestSecret); code != http.StatusBadRequest {
		t.Fatalf("wrong kind: status %d, want 400", code)
	}
	otherKey, err := signSwarmEntry(kp, kp.id, seedAnnounceKind, "someone-else", good.Payload, time.Now())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if code, _ := seedTestRegister(t, app, otherKey, swarmTestSecret); code != http.StatusBadRequest {
		t.Fatalf("key naming another instance: status %d, want 400", code)
	}
	if code, _ := seedTestRegister(t, app, good, "wrong-secret-0123456789abcdef"); code != http.StatusForbidden {
		t.Fatalf("wrong secret: status %d, want 403", code)
	}
	if code, _ := seedTestRegister(t, app, good, ""); code != http.StatusForbidden {
		t.Fatalf("missing secret: status %d, want 403", code)
	}
	if _, err := app.queries.GetSwarmPeer(ctx, "peer-9"); err == nil {
		t.Fatal("refused heartbeat filed a peer row")
	}

	self := seedTestHeartbeat(t, app.swarmID, "https://node.example.org")
	if code, ok := seedTestRegister(t, app, self, swarmTestSecret); code != http.StatusOK || !ok {
		t.Fatalf("self heartbeat: status %d ok %v", code, ok)
	}
	if _, err := app.queries.GetSwarmPeer(ctx, app.swarmID.id); err == nil {
		t.Fatal("self heartbeat filed a peer row for self")
	}
}

// TestSwarmSeedRegisterNeverForgives: in allowlist mode,
// re-registering cannot approve or re-admit an excluded peer;
// the owner's signed heartbeat only moves its address.
func TestSwarmSeedRegisterNeverForgives(t *testing.T) {
	ctx := context.Background()
	app := swarmTestApp(t)
	app.cfg.swarm.mode = swarmAllowlist
	kp := swarmTestPeer(t, app, "peer-7")
	if err := app.queries.SetSwarmPeerApproved(ctx, db.SetSwarmPeerApprovedParams{InstanceID: "peer-7", Approved: false}); err != nil {
		t.Fatalf("unapprove peer: %v", err)
	}
	if err := app.queries.SetSwarmPeerExcluded(ctx, db.SetSwarmPeerExcludedParams{InstanceID: "peer-7", Excluded: true, ExcludeNote: "manual test"}); err != nil {
		t.Fatalf("exclude peer: %v", err)
	}
	code, ok := seedTestRegister(t, app, seedTestHeartbeat(t, kp, "https://peer-7.example"), swarmTestSecret)
	if code != http.StatusOK || !ok {
		t.Fatalf("re-register: status %d ok %v", code, ok)
	}
	peer, err := app.queries.GetSwarmPeer(ctx, "peer-7")
	if err != nil {
		t.Fatalf("peer lost: %v", err)
	}
	if !peer.Excluded {
		t.Fatal("re-registering cleared an exclusion")
	}
	if peer.Approved {
		t.Fatal("re-registering approved a peer nobody approved")
	}
	if peer.BaseUrl != "https://peer-7.example" {
		t.Fatalf("the owner's signed heartbeat did not move its address: %q", peer.BaseUrl)
	}
}
