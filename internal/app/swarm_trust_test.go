package app

// Tests for the swarm's trust rules (swarm_trust.go). Key
// pinning comes first: the key an instance ID was first seen
// with is the only one its entries are ever checked against, so
// every path that records a peer must leave it alone.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
)

// pinHonest files an approved peer with a reputation in B: the
// instance a liar would want to become.
func pinHonest(t *testing.T, p *swarmPair, url string) *swarmIdentity {
	t.Helper()
	ctx := context.Background()
	honest, err := mintSwarmIdentity()
	if err != nil {
		t.Fatalf("mint honest: %v", err)
	}
	honest.id = "instance-honest"
	if _, err := p.b.queries.UpsertSwarmPeer(ctx, db.UpsertSwarmPeerParams{
		InstanceID: honest.id, BaseUrl: url, PublicKey: honest.publicString(),
		Source: "gossip", FirstSeen: time.Now().UTC().Add(-48 * time.Hour),
	}); err != nil {
		t.Fatalf("pin honest: %v", err)
	}
	if err := p.b.queries.SetSwarmPeerApproved(ctx, db.SetSwarmPeerApprovedParams{InstanceID: honest.id, Approved: true}); err != nil {
		t.Fatalf("approve honest: %v", err)
	}
	if err := p.b.queries.AddSwarmPeerCheck(ctx, db.AddSwarmPeerCheckParams{InstanceID: honest.id, GoodChecks: 40}); err != nil {
		t.Fatalf("give honest a record: %v", err)
	}
	return honest
}

// TestSwarmGossipCannotReplacePinnedKey: a member in good
// standing lists an honest peer under a key and an address of
// its own choosing. The honest peer's row keeps its key, its
// address and its record; the attempt is marked on it. This is
// the takeover the pin exists to stop: with the key replaced,
// the liar's entries would verify as the honest peer's and
// inherit its trust.
func TestSwarmGossipCannotReplacePinnedKey(t *testing.T) {
	p := newSwarmPair(t, swarmOpen)
	ctx := context.Background()
	now := time.Now().UTC()
	p.pinA(t)
	honest := pinHonest(t, p, "https://honest.example")
	before, err := p.b.queries.GetSwarmPeer(ctx, honest.id)
	if err != nil {
		t.Fatalf("read honest: %v", err)
	}

	// A's own table is what its member list serves: A names the
	// honest instance under the liar's key and address.
	liar, err := mintSwarmIdentity()
	if err != nil {
		t.Fatalf("mint liar: %v", err)
	}
	if _, err := p.a.queries.UpsertSwarmPeer(ctx, db.UpsertSwarmPeerParams{
		InstanceID: honest.id, BaseUrl: "https://liar.example", PublicKey: liar.publicString(),
		Source: "gossip", FirstSeen: now,
	}); err != nil {
		t.Fatalf("plant the lie in A: %v", err)
	}

	if err := p.b.swarmContactPeer(ctx, p.urlA, "instance-a", p.a.swarmID.publicString(), "gossip", now); err != nil {
		t.Fatalf("gossip with A: %v", err)
	}

	after, err := p.b.queries.GetSwarmPeer(ctx, honest.id)
	if err != nil {
		t.Fatalf("honest peer lost: %v", err)
	}
	if after.PublicKey != honest.publicString() {
		t.Fatalf("gossip replaced the pinned key")
	}
	if after.BaseUrl != "https://honest.example" {
		t.Fatalf("gossip moved the address to %q", after.BaseUrl)
	}
	if after.GoodChecks != 40 || after.BadChecks != 0 || !after.Approved || after.Excluded {
		t.Fatalf("gossip touched the record: %+v", after)
	}
	if !after.LastSeen.Equal(before.LastSeen) {
		t.Fatalf("a refused sighting refreshed last_seen")
	}
	if !after.KeyConflictAt.Valid {
		t.Fatalf("the refused key change was not marked on the peer")
	}

	// The honest peer's own entries still verify, the liar's do not.
	entry, err := signSwarmEntry(liar, honest.id, swarmKindTypeName, "34", `{"type_id":34,"name":"Tritanium"}`, now)
	if err != nil {
		t.Fatalf("sign as liar: %v", err)
	}
	if verifySwarmEntry(entry, after.PublicKey) {
		t.Fatalf("an entry signed with the liar's key verifies as the honest peer")
	}
}

// TestSwarmGossipKeepsAddressUnlessOwnerMovesIt: with the right
// key, a third party's word about a known peer's address still
// changes nothing but last_seen; the peer's own self-signed
// heartbeat moves it.
func TestSwarmGossipKeepsAddressUnlessOwnerMovesIt(t *testing.T) {
	p := newSwarmPair(t, swarmOpen)
	ctx := context.Background()
	now := time.Now().UTC()
	p.pinA(t)
	honest := pinHonest(t, p, "https://honest.example")

	if _, err := p.a.queries.UpsertSwarmPeer(ctx, db.UpsertSwarmPeerParams{
		InstanceID: honest.id, BaseUrl: "https://elsewhere.example", PublicKey: honest.publicString(),
		Source: "gossip", FirstSeen: now,
	}); err != nil {
		t.Fatalf("plant the address in A: %v", err)
	}
	if err := p.b.swarmContactPeer(ctx, p.urlA, "instance-a", p.a.swarmID.publicString(), "gossip", now); err != nil {
		t.Fatalf("gossip with A: %v", err)
	}
	peer, err := p.b.queries.GetSwarmPeer(ctx, honest.id)
	if err != nil {
		t.Fatalf("honest peer lost: %v", err)
	}
	if peer.BaseUrl != "https://honest.example" {
		t.Fatalf("a third party moved the address to %q", peer.BaseUrl)
	}
	if peer.KeyConflictAt.Valid {
		t.Fatalf("a sighting under the pinned key was marked as a conflict")
	}
	if !peer.LastSeen.After(now.Add(-time.Hour)) {
		t.Fatalf("a matching sighting did not refresh last_seen: %v", peer.LastSeen)
	}

	// The owner's own heartbeat, under the pinned key, moves it.
	code, ok := seedTestRegister(t, p.b, seedTestHeartbeat(t, honest, "https://moved.example"), p.secret)
	if code != http.StatusOK || !ok {
		t.Fatalf("owner heartbeat: status %d ok %v", code, ok)
	}
	peer, err = p.b.queries.GetSwarmPeer(ctx, honest.id)
	if err != nil {
		t.Fatalf("honest peer lost: %v", err)
	}
	if peer.BaseUrl != "https://moved.example" {
		t.Fatalf("the owner could not move its own address: %q", peer.BaseUrl)
	}
}

// TestSwarmSenderUnderAnotherKeyTeachesNothing: an address that
// answers as a known instance under a key that is not its
// pinned one is refused whole. Its own row is untouched and
// marked, and nobody its list names is learned.
func TestSwarmSenderUnderAnotherKeyTeachesNothing(t *testing.T) {
	p := newSwarmPair(t, swarmOpen)
	ctx := context.Background()
	now := time.Now().UTC()

	// B knows "instance-a" under another key, at another address.
	pinned, err := mintSwarmIdentity()
	if err != nil {
		t.Fatalf("mint pinned: %v", err)
	}
	if _, err := p.b.queries.UpsertSwarmPeer(ctx, db.UpsertSwarmPeerParams{
		InstanceID: "instance-a", BaseUrl: "https://real-a.example", PublicKey: pinned.publicString(),
		Source: "gossip", FirstSeen: now.Add(-48 * time.Hour),
	}); err != nil {
		t.Fatalf("pin the real A: %v", err)
	}
	// The server at urlA lists a third instance too.
	third, err := mintSwarmIdentity()
	if err != nil {
		t.Fatalf("mint third: %v", err)
	}
	if _, err := p.a.queries.UpsertSwarmPeer(ctx, db.UpsertSwarmPeerParams{
		InstanceID: "instance-third", BaseUrl: "https://third.example", PublicKey: third.publicString(),
		Source: "gossip", FirstSeen: now,
	}); err != nil {
		t.Fatalf("list a third instance in A: %v", err)
	}

	// Dialed with no pinned key, as a seed or a hand-typed
	// address is: trust on first use must not reach a known ID.
	err = p.b.swarmContactPeer(ctx, p.urlA, "", "", "manual", now)
	if !errors.Is(err, errSwarmKeyChanged) {
		t.Fatalf("contact: err = %v, want errSwarmKeyChanged", err)
	}
	peer, err := p.b.queries.GetSwarmPeer(ctx, "instance-a")
	if err != nil {
		t.Fatalf("pinned peer lost: %v", err)
	}
	if peer.PublicKey != pinned.publicString() || peer.BaseUrl != "https://real-a.example" {
		t.Fatalf("the impostor rewrote the pinned peer: %+v", peer)
	}
	if !peer.KeyConflictAt.Valid {
		t.Fatalf("the refused sender was not marked")
	}
	if peer.Approved {
		t.Fatalf("a refused sighting approved the peer")
	}
	if _, err := p.b.queries.GetSwarmPeer(ctx, "instance-third"); err == nil {
		t.Fatalf("a refused sender still taught its member list")
	}
}

// TestSwarmRegisterUnderAnotherKeyConflicts: a heartbeat for a
// known instance ID signed by a different key is a conflict,
// not a rotation.
func TestSwarmRegisterUnderAnotherKeyConflicts(t *testing.T) {
	ctx := context.Background()
	app := swarmTestApp(t)
	pinned := swarmTestPeer(t, app, "peer-5")
	impostor, err := mintSwarmIdentity()
	if err != nil {
		t.Fatalf("mint impostor: %v", err)
	}
	impostor.id = "peer-5"
	code, ok := seedTestRegister(t, app, seedTestHeartbeat(t, impostor, "https://impostor.example"), swarmTestSecret)
	if code != http.StatusConflict || ok {
		t.Fatalf("impostor heartbeat: status %d ok %v, want 409", code, ok)
	}
	peer, err := app.queries.GetSwarmPeer(ctx, "peer-5")
	if err != nil {
		t.Fatalf("peer lost: %v", err)
	}
	if peer.PublicKey != pinned.publicString() || peer.BaseUrl != "http://127.0.0.1:9" {
		t.Fatalf("the impostor rewrote the peer: %+v", peer)
	}
	if !peer.KeyConflictAt.Valid {
		t.Fatalf("the refused heartbeat was not marked")
	}
}

// TestSwarmRemovedPeerPinsAfresh: removing a peer is the one
// way its key changes. The next sighting pins what it brings,
// with no mark carried over.
func TestSwarmRemovedPeerPinsAfresh(t *testing.T) {
	ctx := context.Background()
	app := swarmTestApp(t)
	swarmTestPeer(t, app, "peer-6")
	rotated, err := mintSwarmIdentity()
	if err != nil {
		t.Fatalf("mint rotated: %v", err)
	}
	rotated.id = "peer-6"
	if code, _ := seedTestRegister(t, app, seedTestHeartbeat(t, rotated, "https://peer-6.example"), swarmTestSecret); code != http.StatusConflict {
		t.Fatalf("new key before removal: status %d, want 409", code)
	}
	if err := app.queries.DeleteSwarmPeer(ctx, "peer-6"); err != nil {
		t.Fatalf("remove peer: %v", err)
	}
	if code, ok := seedTestRegister(t, app, seedTestHeartbeat(t, rotated, "https://peer-6.example"), swarmTestSecret); code != http.StatusOK || !ok {
		t.Fatalf("new key after removal: status %d ok %v", code, ok)
	}
	peer, err := app.queries.GetSwarmPeer(ctx, "peer-6")
	if err != nil {
		t.Fatalf("peer not re-pinned: %v", err)
	}
	if peer.PublicKey != rotated.publicString() || peer.KeyConflictAt.Valid {
		t.Fatalf("re-pinned row wrong: %+v", peer)
	}
}

// signedBy signs one entry as kp, the way a peer serves it.
func signedBy(t *testing.T, kp *swarmIdentity, kind, key, payload string, at time.Time) swarmSignedEntry {
	t.Helper()
	entry, err := signSwarmEntry(kp, kp.id, kind, key, payload, at)
	if err != nil {
		t.Fatalf("sign %s %s: %v", kind, key, err)
	}
	return entry
}

// stagedKeys lists what sits in staging, as kind/key.
func stagedKeys(t *testing.T, app *Application) map[string]string {
	t.Helper()
	rows, err := app.queries.ListUncheckedStaging(context.Background(), 100)
	if err != nil {
		t.Fatalf("list staging: %v", err)
	}
	out := make(map[string]string, len(rows))
	for _, row := range rows {
		out[row.Kind+"/"+row.Key] = row.Payload
	}
	return out
}

// TestSwarmPullStagesOnlyWhatWasAsked: a peer answers a pull
// with the key asked for and a good deal more, all correctly
// signed. Only the asked key stages, with its first answer; the
// rest is dropped and the response costs the peer one mark.
func TestSwarmPullStagesOnlyWhatWasAsked(t *testing.T) {
	ctx := context.Background()
	app := swarmTestApp(t)
	peer := swarmTestPeer(t, app, "peer-1")
	other := swarmTestPeer(t, app, "peer-2")
	now := time.Now().UTC()

	asked := []swarmPullKey{
		{Kind: swarmKindTypeName, Key: "34"},
		{Kind: swarmKindTypeName, Key: "36"},
	}
	entries := []swarmSignedEntry{
		signedBy(t, peer, swarmKindTypeName, "34", `{"type_id":34,"name":"Tritanium"}`, now),
		// Never asked for: another type, and another kind.
		signedBy(t, peer, swarmKindTypeName, "35", `{"type_id":35,"name":"Pyerite"}`, now),
		signedBy(t, peer, swarmKindCorporation, "98000001", `{"id":98000001,"payload":"{}"}`, now),
		// The asked key again, later and different: one answer per key.
		signedBy(t, peer, swarmKindTypeName, "34", `{"type_id":34,"name":"Poison"}`, now.Add(time.Hour)),
		// An asked key under someone else's signature: skipped, no blame.
		signedBy(t, other, swarmKindTypeName, "36", `{"type_id":36,"name":"Mexallon"}`, now),
	}
	if got := app.swarmStagePulled(ctx, peer.id, "http://127.0.0.1:9", asked, entries, now); got != 1 {
		t.Fatalf("staged %d entries, want 1", got)
	}
	staged := stagedKeys(t, app)
	if len(staged) != 1 || staged[swarmKindTypeName+"/34"] != `{"type_id":34,"name":"Tritanium"}` {
		t.Fatalf("staging holds %v, want only the first answer for type 34", staged)
	}
	row, err := app.queries.GetSwarmPeer(ctx, peer.id)
	if err != nil {
		t.Fatalf("read peer: %v", err)
	}
	if row.BadChecks != 1 {
		t.Fatalf("serving peer has %d bad marks, want 1 for the response", row.BadChecks)
	}
	if bystander, err := app.queries.GetSwarmPeer(ctx, other.id); err != nil || bystander.BadChecks != 0 {
		t.Fatalf("the signer named by a forwarded entry was blamed: %+v err=%v", bystander, err)
	}
}

// TestSwarmPullCleanAnswerCostsNothing: an answer holding only
// what was asked stages it all and leaves the ledger alone.
func TestSwarmPullCleanAnswerCostsNothing(t *testing.T) {
	ctx := context.Background()
	app := swarmTestApp(t)
	peer := swarmTestPeer(t, app, "peer-1")
	now := time.Now().UTC()
	asked := []swarmPullKey{{Kind: swarmKindTypeName, Key: "34"}, {Kind: swarmKindTypeName, Key: "35"}}
	entries := []swarmSignedEntry{
		signedBy(t, peer, swarmKindTypeName, "35", `{"type_id":35,"name":"Pyerite"}`, now),
		signedBy(t, peer, swarmKindTypeName, "34", `{"type_id":34,"name":"Tritanium"}`, now),
	}
	if got := app.swarmStagePulled(ctx, peer.id, "http://127.0.0.1:9", asked, entries, now); got != 2 {
		t.Fatalf("staged %d entries, want 2", got)
	}
	row, err := app.queries.GetSwarmPeer(ctx, peer.id)
	if err != nil || row.BadChecks != 0 {
		t.Fatalf("a clean answer marked the peer: %+v err=%v", row, err)
	}
}

// TestSwarmPullOverHTTPDropsUnasked: the same rule end to end.
// A peer's /swarm/pull hands back the asked record and one
// nobody asked for; the urgent single-key pull stages one.
func TestSwarmPullOverHTTPDropsUnasked(t *testing.T) {
	ctx := context.Background()
	app := swarmTestApp(t)
	peer, err := mintSwarmIdentity()
	if err != nil {
		t.Fatalf("mint peer: %v", err)
	}
	peer.id = "peer-http"
	now := time.Now().UTC()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/swarm/pull" {
			http.NotFound(w, r)
			return
		}
		writeSwarmJSON(w, map[string]any{"entries": []swarmSignedEntry{
			signedBy(t, peer, swarmKindTypeName, "34", `{"type_id":34,"name":"Tritanium"}`, now),
			signedBy(t, peer, swarmKindTypeName, "35", `{"type_id":35,"name":"Pyerite"}`, now),
		}})
	}))
	t.Cleanup(srv.Close)
	if _, err := app.queries.UpsertSwarmPeer(ctx, db.UpsertSwarmPeerParams{
		InstanceID: peer.id, BaseUrl: srv.URL, PublicKey: peer.publicString(),
		Source: "manual", FirstSeen: now,
	}); err != nil {
		t.Fatalf("pin peer: %v", err)
	}
	if err := app.queries.SetSwarmPeerApproved(ctx, db.SetSwarmPeerApprovedParams{InstanceID: peer.id, Approved: true}); err != nil {
		t.Fatalf("approve peer: %v", err)
	}
	if got := app.swarmPullOne(ctx, swarmKindTypeName, "34"); got != 1 {
		t.Fatalf("pull staged %d entries, want 1", got)
	}
	staged := stagedKeys(t, app)
	if _, ok := staged[swarmKindTypeName+"/35"]; ok || len(staged) != 1 {
		t.Fatalf("staging holds %v, want only type 34", staged)
	}
}
