package app

// Tests for the swarm's trust rules (swarm/trust.go). Key
// pinning comes first: the key an instance ID was first seen
// with is the only one its entries are ever checked against, so
// every path that records a peer must leave it alone.

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"evesynapse/internal/app/swarm"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// pinHonest files an approved peer with a reputation in B: the
// instance a liar would want to become.
func pinHonest(t *testing.T, p *swarmPair, url string) *swarm.SwarmIdentity {
	t.Helper()
	ctx := context.Background()
	honest, err := swarm.MintSwarmIdentity()
	if err != nil {
		t.Fatalf("mint honest: %v", err)
	}
	honest.ID = "instance-honest"
	if _, err := p.b.queries.UpsertSwarmPeer(ctx, db.UpsertSwarmPeerParams{
		InstanceID: honest.ID, BaseUrl: url, PublicKey: honest.PublicString(),
		Source: "gossip", FirstSeen: time.Now().UTC().Add(-48 * time.Hour),
	}); err != nil {
		t.Fatalf("pin honest: %v", err)
	}
	if err := p.b.queries.SetSwarmPeerApproved(ctx, db.SetSwarmPeerApprovedParams{InstanceID: honest.ID, Approved: true}); err != nil {
		t.Fatalf("approve honest: %v", err)
	}
	if err := p.b.queries.AddSwarmPeerCheck(ctx, db.AddSwarmPeerCheckParams{InstanceID: honest.ID, GoodChecks: 40}); err != nil {
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
	p := newSwarmPair(t, swarm.SwarmOpen)
	ctx := context.Background()
	now := time.Now().UTC()
	p.pinA(t)
	honest := pinHonest(t, p, "https://honest.example")
	before, err := p.b.queries.GetSwarmPeer(ctx, honest.ID)
	if err != nil {
		t.Fatalf("read honest: %v", err)
	}

	// A's own table is what its member list serves: A names the
	// honest instance under the liar's key and address.
	liar, err := swarm.MintSwarmIdentity()
	if err != nil {
		t.Fatalf("mint liar: %v", err)
	}
	// A complete, self-consistent tuple: the liar signs the
	// address with its own key and claims the honest ID.
	liar.ID = honest.ID
	if _, err := p.a.queries.UpsertSwarmPeer(ctx, db.UpsertSwarmPeerParams{
		InstanceID: honest.ID, BaseUrl: "https://liar.example", PublicKey: liar.PublicString(),
		Source: "gossip", FirstSeen: now, UrlSig: swarm.SignSwarmMember(liar, "https://liar.example", testIssued), UrlIssuedAt: testIssued,
	}); err != nil {
		t.Fatalf("plant the lie in A: %v", err)
	}

	if err := swarm.StateFor(p.b).SwarmContactPeer(ctx, p.urlA, "instance-a", swarm.StateFor(p.a).SwarmID.PublicString(), "gossip", now); err != nil {
		t.Fatalf("gossip with A: %v", err)
	}

	after, err := p.b.queries.GetSwarmPeer(ctx, honest.ID)
	if err != nil {
		t.Fatalf("honest peer lost: %v", err)
	}
	if after.PublicKey != honest.PublicString() {
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
	entry, err := swarm.SignSwarmEntry(liar, honest.ID, swarm.SwarmKindTypeName, "34", `{"type_id":34,"name":"Tritanium"}`, now)
	if err != nil {
		t.Fatalf("sign as liar: %v", err)
	}
	if swarm.VerifySwarmEntry(entry, after.PublicKey) {
		t.Fatalf("an entry signed with the liar's key verifies as the honest peer")
	}
}

// testIssued is a fixed issue time for tuples the tests sign.
const testIssued = int64(1760000000)

// TestSwarmAddressMovesOnlyOnItsOwnersSignature: an address is
// its owner's word, and its newest word. A relayed tuple moves a
// known peer's address only when its signature verifies under
// the pinned key and it was issued later than the tuple held;
// any other tuple for that peer changes nothing.
func TestSwarmAddressMovesOnlyOnItsOwnersSignature(t *testing.T) {
	p := newSwarmPair(t, swarm.SwarmOpen)
	ctx := context.Background()
	now := time.Now().UTC()
	p.pinA(t)
	honest := pinHonest(t, p, "https://honest.example")
	relay := func(url, sig string, issued int64) {
		t.Helper()
		// A's table is what its member list serves. A fresh row
		// each time, so A relays exactly this tuple.
		if err := p.a.queries.DeleteSwarmPeer(ctx, honest.ID); err != nil {
			t.Fatalf("clear A: %v", err)
		}
		if _, err := p.a.queries.UpsertSwarmPeer(ctx, db.UpsertSwarmPeerParams{
			InstanceID: honest.ID, BaseUrl: url, PublicKey: honest.PublicString(),
			Source: "gossip", FirstSeen: now, UrlSig: sig, UrlIssuedAt: issued,
		}); err != nil {
			t.Fatalf("plant the tuple in A: %v", err)
		}
		if err := swarm.StateFor(p.b).SwarmContactPeer(ctx, p.urlA, "instance-a", swarm.StateFor(p.a).SwarmID.PublicString(), "gossip", now); err != nil {
			t.Fatalf("gossip with A: %v", err)
		}
	}
	read := func() db.SwarmPeer {
		t.Helper()
		peer, err := p.b.queries.GetSwarmPeer(ctx, honest.ID)
		if err != nil {
			t.Fatalf("honest peer lost: %v", err)
		}
		return peer
	}

	// Signed by somebody else's key over the same ID and a new address.
	other, err := swarm.MintSwarmIdentity()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	other.ID = honest.ID
	relay("https://elsewhere.example", swarm.SignSwarmMember(other, "https://elsewhere.example", testIssued), testIssued)
	if peer := read(); peer.BaseUrl != "https://honest.example" || peer.KeyConflictAt.Valid {
		t.Fatalf("a tuple its owner never signed changed the peer: %+v", peer)
	}
	// The owner's signature, but over a different address than the one relayed.
	relay("https://swapped.example", swarm.SignSwarmMember(honest, "https://honest.example", testIssued), testIssued)
	if peer := read(); peer.BaseUrl != "https://honest.example" {
		t.Fatalf("a signature for another address moved the peer to %q", peer.BaseUrl)
	}
	// The owner's signature, but the issue time relayed is not the one signed.
	relay("https://moved.example", swarm.SignSwarmMember(honest, "https://moved.example", testIssued), testIssued+500)
	if peer := read(); peer.BaseUrl != "https://honest.example" {
		t.Fatalf("a tuple with a doctored issue time moved the peer to %q", peer.BaseUrl)
	}
	// The owner's own signed tuple, relayed by a third party: it moves,
	// and the tuple is kept exactly as issued.
	moved := swarm.SignSwarmMember(honest, "https://moved.example", testIssued+100)
	relay("https://moved.example", moved, testIssued+100)
	if peer := read(); peer.BaseUrl != "https://moved.example" || peer.UrlSig != moved || peer.UrlIssuedAt != testIssued+100 {
		t.Fatalf("the owner's signed address was not taken up verbatim: %+v", peer)
	}
	// Replay: an older tuple the owner really signed does not move it back.
	relay("https://old.example", swarm.SignSwarmMember(honest, "https://old.example", testIssued+50), testIssued+50)
	if peer := read(); peer.BaseUrl != "https://moved.example" || peer.UrlIssuedAt != testIssued+100 {
		t.Fatalf("an older tuple replaced a newer one: %+v", peer)
	}
	// The owner's own heartbeat, issued now, moves it.
	code, ok := seedTestRegister(t, p.b, seedTestHeartbeat(t, honest, "https://again.example"), p.secret)
	if code != http.StatusOK || !ok {
		t.Fatalf("owner heartbeat: status %d ok %v", code, ok)
	}
	if peer := read(); peer.BaseUrl != "https://again.example" {
		t.Fatalf("the owner could not move its own address: %q", peer.BaseUrl)
	}
}

// TestSwarmSenderUnderAnotherKeyTeachesNothing: an address that
// answers as a known instance under a key that is not its
// pinned one is refused whole. Its own row is untouched and
// marked, and nobody its list names is learned.
func TestSwarmSenderUnderAnotherKeyTeachesNothing(t *testing.T) {
	p := newSwarmPair(t, swarm.SwarmOpen)
	ctx := context.Background()
	now := time.Now().UTC()

	// B knows "instance-a" under another key, at another address.
	pinned, err := swarm.MintSwarmIdentity()
	if err != nil {
		t.Fatalf("mint pinned: %v", err)
	}
	if _, err := p.b.queries.UpsertSwarmPeer(ctx, db.UpsertSwarmPeerParams{
		InstanceID: "instance-a", BaseUrl: "https://real-a.example", PublicKey: pinned.PublicString(),
		Source: "gossip", FirstSeen: now.Add(-48 * time.Hour),
	}); err != nil {
		t.Fatalf("pin the real A: %v", err)
	}
	// The server at urlA lists a third instance too.
	third, err := swarm.MintSwarmIdentity()
	if err != nil {
		t.Fatalf("mint third: %v", err)
	}
	third.ID = "instance-third"
	if _, err := p.a.queries.UpsertSwarmPeer(ctx, db.UpsertSwarmPeerParams{
		InstanceID: "instance-third", BaseUrl: "https://third.example", PublicKey: third.PublicString(),
		Source: "gossip", FirstSeen: now, UrlSig: swarm.SignSwarmMember(third, "https://third.example", testIssued), UrlIssuedAt: testIssued,
	}); err != nil {
		t.Fatalf("list a third instance in A: %v", err)
	}

	// Dialed with no pinned key, as a seed or a hand-typed
	// address is: trust on first use must not reach a known ID.
	err = swarm.StateFor(p.b).SwarmContactPeer(ctx, p.urlA, "", "", "manual", now)
	if !errors.Is(err, swarm.ErrSwarmKeyChanged) {
		t.Fatalf("contact: err = %v, want swarm.ErrSwarmKeyChanged", err)
	}
	peer, err := p.b.queries.GetSwarmPeer(ctx, "instance-a")
	if err != nil {
		t.Fatalf("pinned peer lost: %v", err)
	}
	if peer.PublicKey != pinned.PublicString() || peer.BaseUrl != "https://real-a.example" {
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
	impostor, err := swarm.MintSwarmIdentity()
	if err != nil {
		t.Fatalf("mint impostor: %v", err)
	}
	impostor.ID = "peer-5"
	code, ok := seedTestRegister(t, app, seedTestHeartbeat(t, impostor, "https://impostor.example"), swarmTestSecret)
	if code != http.StatusConflict || ok {
		t.Fatalf("impostor heartbeat: status %d ok %v, want 409", code, ok)
	}
	peer, err := app.queries.GetSwarmPeer(ctx, "peer-5")
	if err != nil {
		t.Fatalf("peer lost: %v", err)
	}
	if peer.PublicKey != pinned.PublicString() || peer.BaseUrl != "http://127.0.0.1:9" {
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
	rotated, err := swarm.MintSwarmIdentity()
	if err != nil {
		t.Fatalf("mint rotated: %v", err)
	}
	rotated.ID = "peer-6"
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
	if peer.PublicKey != rotated.PublicString() || peer.KeyConflictAt.Valid {
		t.Fatalf("re-pinned row wrong: %+v", peer)
	}
}

// signedBy signs one entry as kp, the way a peer serves it.
func signedBy(t *testing.T, kp *swarm.SwarmIdentity, kind, key, payload string, at time.Time) swarm.SwarmSignedEntry {
	t.Helper()
	entry, err := swarm.SignSwarmEntry(kp, kp.ID, kind, key, payload, at)
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

	asked := []swarm.SwarmPullKey{
		{Kind: swarm.SwarmKindTypeName, Key: "34"},
		{Kind: swarm.SwarmKindTypeName, Key: "36"},
	}
	entries := []swarm.SwarmSignedEntry{
		signedBy(t, peer, swarm.SwarmKindTypeName, "34", `{"type_id":34,"name":"Tritanium"}`, now),
		// Never asked for: another type, and another kind.
		signedBy(t, peer, swarm.SwarmKindTypeName, "35", `{"type_id":35,"name":"Pyerite"}`, now),
		signedBy(t, peer, swarm.SwarmKindCorporation, "98000001", `{"id":98000001,"payload":"{}"}`, now),
		// The asked key again, later and different: one answer per key.
		signedBy(t, peer, swarm.SwarmKindTypeName, "34", `{"type_id":34,"name":"Poison"}`, now.Add(time.Hour)),
		// An asked key under someone else's signature: skipped, no blame.
		signedBy(t, other, swarm.SwarmKindTypeName, "36", `{"type_id":36,"name":"Mexallon"}`, now),
	}
	if got := swarm.StateFor(app).SwarmStagePulled(ctx, peer.ID, "http://127.0.0.1:9", asked, entries, now); got != 1 {
		t.Fatalf("staged %d entries, want 1", got)
	}
	staged := stagedKeys(t, app)
	if len(staged) != 1 || staged[swarm.SwarmKindTypeName+"/34"] != `{"type_id":34,"name":"Tritanium"}` {
		t.Fatalf("staging holds %v, want only the first answer for type 34", staged)
	}
	row, err := app.queries.GetSwarmPeer(ctx, peer.ID)
	if err != nil {
		t.Fatalf("read peer: %v", err)
	}
	if row.BadChecks != 1 {
		t.Fatalf("serving peer has %d bad marks, want 1 for the response", row.BadChecks)
	}
	if bystander, err := app.queries.GetSwarmPeer(ctx, other.ID); err != nil || bystander.BadChecks != 0 {
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
	asked := []swarm.SwarmPullKey{{Kind: swarm.SwarmKindTypeName, Key: "34"}, {Kind: swarm.SwarmKindTypeName, Key: "35"}}
	entries := []swarm.SwarmSignedEntry{
		signedBy(t, peer, swarm.SwarmKindTypeName, "35", `{"type_id":35,"name":"Pyerite"}`, now),
		signedBy(t, peer, swarm.SwarmKindTypeName, "34", `{"type_id":34,"name":"Tritanium"}`, now),
	}
	if got := swarm.StateFor(app).SwarmStagePulled(ctx, peer.ID, "http://127.0.0.1:9", asked, entries, now); got != 2 {
		t.Fatalf("staged %d entries, want 2", got)
	}
	row, err := app.queries.GetSwarmPeer(ctx, peer.ID)
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
	peer, err := swarm.MintSwarmIdentity()
	if err != nil {
		t.Fatalf("mint peer: %v", err)
	}
	peer.ID = "peer-http"
	now := time.Now().UTC()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/swarm/pull" {
			http.NotFound(w, r)
			return
		}
		swarm.WriteSwarmJSON(w, map[string]any{"entries": []swarm.SwarmSignedEntry{
			signedBy(t, peer, swarm.SwarmKindTypeName, "34", `{"type_id":34,"name":"Tritanium"}`, now),
			signedBy(t, peer, swarm.SwarmKindTypeName, "35", `{"type_id":35,"name":"Pyerite"}`, now),
		}})
	}))
	t.Cleanup(srv.Close)
	if _, err := app.queries.UpsertSwarmPeer(ctx, db.UpsertSwarmPeerParams{
		InstanceID: peer.ID, BaseUrl: srv.URL, PublicKey: peer.PublicString(),
		Source: "manual", FirstSeen: now,
	}); err != nil {
		t.Fatalf("pin peer: %v", err)
	}
	if err := app.queries.SetSwarmPeerApproved(ctx, db.SetSwarmPeerApprovedParams{InstanceID: peer.ID, Approved: true}); err != nil {
		t.Fatalf("approve peer: %v", err)
	}
	if got := swarm.StateFor(app).SwarmPullOne(ctx, swarm.SwarmKindTypeName, "34"); got != 1 {
		t.Fatalf("pull staged %d entries, want 1", got)
	}
	staged := stagedKeys(t, app)
	if _, ok := staged[swarm.SwarmKindTypeName+"/35"]; ok || len(staged) != 1 {
		t.Fatalf("staging holds %v, want only type 34", staged)
	}
}

// orgEntry is a corporation or alliance record as it travels.
func orgEntry(t *testing.T, id int64, record string) string {
	t.Helper()
	return string(mustJSON(t, swarm.SwarmOrgPayload{ID: id, Payload: record}))
}

// TestSwarmApplyRecordsWhereItCameFrom: every applied entry
// leaves a provenance row naming the peer that served it, the
// instance that signed it and the fetch time the signature
// covers, with the stamp written into the live row.
func TestSwarmApplyRecordsWhereItCameFrom(t *testing.T) {
	ctx := context.Background()
	app := swarmTestApp(t)
	peer := swarmTestPeer(t, app, "peer-1")
	fetched := time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)
	now := time.Now().UTC()

	origin := swarm.SwarmOrigin{Source: "peer-served-it", Signer: peer.ID, FetchedAt: fetched, Verified: true}
	if err := swarm.StateFor(app).ApplySwarmEntry(ctx, swarm.SwarmKindCorporation, "98000001", orgEntry(t, 98000001, `{"corp":{"name":"Test Corp"}}`), origin, now); err != nil {
		t.Fatalf("apply corporation: %v", err)
	}
	got, err := app.queries.GetSwarmApplied(ctx, db.GetSwarmAppliedParams{Kind: swarm.SwarmKindCorporation, Key: "98000001"})
	if err != nil {
		t.Fatalf("no provenance row: %v", err)
	}
	if got.Source != "peer-served-it" || got.Signer != peer.ID || !got.FetchedAt.Equal(fetched) || !got.Verified {
		t.Fatalf("provenance wrong: %+v", got)
	}
	rec, err := app.queries.GetCorporationRecord(ctx, 98000001)
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	if !got.RowStamp.Valid || !rec.FetchedAt.Valid || !rec.FetchedAt.Time.Equal(got.RowStamp.Time) {
		t.Fatalf("row stamp %v does not match the live row at %v", got.RowStamp, rec.FetchedAt)
	}

	// History keeps the day range it supplied; an unchecked apply says so.
	history := `{"region_id":10000002,"type_id":34,"days":[` +
		`{"date":"2026-10-03","average":5,"highest":6,"lowest":4,"volume":10,"order_count":2},` +
		`{"date":"2026-10-01","average":5,"highest":6,"lowest":4,"volume":10,"order_count":2}]}`
	unchecked := swarm.SwarmOrigin{Signer: peer.ID, FetchedAt: fetched}
	if err := swarm.StateFor(app).ApplySwarmEntry(ctx, swarm.SwarmKindHistory, "10000002/34", history, unchecked, now); err != nil {
		t.Fatalf("apply history: %v", err)
	}
	hist, err := app.queries.GetSwarmApplied(ctx, db.GetSwarmAppliedParams{Kind: swarm.SwarmKindHistory, Key: "10000002/34"})
	if err != nil {
		t.Fatalf("no history provenance: %v", err)
	}
	if hist.FirstDate != "2026-10-01" || hist.LastDate != "2026-10-03" || hist.Verified {
		t.Fatalf("history provenance wrong: %+v", hist)
	}
	if hist.Source != peer.ID {
		t.Fatalf("an entry staged without a source should fall back to its signer, got %q", hist.Source)
	}
}

// TestSwarmPairProvenanceEndToEnd: an entry pulled over HTTP,
// staged and checked carries its origin all the way to the row.
func TestSwarmPairProvenanceEndToEnd(t *testing.T) {
	p := newSwarmPair(t, swarm.SwarmOpen)
	ctx := context.Background()
	if err := p.a.queries.UpsertTypeName(ctx, db.UpsertTypeNameParams{TypeID: 34, Name: "Tritanium"}); err != nil {
		t.Fatalf("seed A: %v", err)
	}
	p.stubB.types = map[int64]string{34: `{"name":"Tritanium","group_id":18}`}
	p.pinA(t)
	p.pullStages(t, []swarm.SwarmPullKey{{Kind: swarm.SwarmKindTypeName, Key: "34"}})
	if got := p.spotRuns(t); got != 1 {
		t.Fatalf("applied %d, want 1", got)
	}
	row, err := p.b.queries.GetSwarmApplied(ctx, db.GetSwarmAppliedParams{Kind: swarm.SwarmKindTypeName, Key: "34"})
	if err != nil {
		t.Fatalf("no provenance after a pull and a check: %v", err)
	}
	if row.Source != "instance-a" || row.Signer != "instance-a" || !row.Verified {
		t.Fatalf("provenance wrong: %+v", row)
	}
	if time.Since(row.FetchedAt) > time.Hour || row.FetchedAt.After(time.Now().Add(time.Minute)) {
		t.Fatalf("fetched_at %v is not the time A signed", row.FetchedAt)
	}
}

// TestSwarmExcludedSignerDataIsTakenBack: excluding a signer
// undoes what it supplied, one kind at a time, and nothing
// else: not another signer's rows, not a row this instance has
// fetched itself since, not history outside the entry's days.
func TestSwarmExcludedSignerDataIsTakenBack(t *testing.T) {
	ctx := context.Background()
	app := swarmTestApp(t)
	bad := swarmTestPeer(t, app, "peer-bad")
	good := swarmTestPeer(t, app, "peer-good")
	fetched := time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)
	now := time.Now().UTC()
	from := func(peer *swarm.SwarmIdentity) swarm.SwarmOrigin {
		return swarm.SwarmOrigin{Source: peer.ID, Signer: peer.ID, FetchedAt: fetched}
	}
	apply := func(kind, key, payload string, origin swarm.SwarmOrigin) {
		t.Helper()
		if err := swarm.StateFor(app).ApplySwarmEntry(ctx, kind, key, payload, origin, now); err != nil {
			t.Fatalf("apply %s %s: %v", kind, key, err)
		}
	}

	// A day this instance already held, outside the entry's range.
	if err := app.queries.UpsertMarketHistory(ctx, db.UpsertMarketHistoryParams{
		RegionID: 10000002, TypeID: 34, Date: "2026-09-15", Average: 5, Highest: 6, Lowest: 4, Volume: 9, OrderCount: 1,
	}); err != nil {
		t.Fatalf("seed own history: %v", err)
	}
	apply(swarm.SwarmKindCorporation, "98000001", orgEntry(t, 98000001, `{"corp":{"name":"Planted Corp"}}`), from(bad))
	apply(swarm.SwarmKindAlliance, "99000001", orgEntry(t, 99000001, `{"alliance":{"name":"Planted Alliance"}}`), from(bad))
	apply(swarm.SwarmKindHistory, "10000002/34", `{"region_id":10000002,"type_id":34,"days":[`+
		`{"date":"2026-10-02","average":999,"highest":999,"lowest":999,"volume":1,"order_count":1},`+
		`{"date":"2026-10-01","average":999,"highest":999,"lowest":999,"volume":1,"order_count":1}]}`, from(bad))
	apply(swarm.SwarmKindTypeName, "34", `{"type_id":34,"name":"Not Tritanium"}`, from(bad))
	apply(swarm.SwarmKindStructure, "1035000000001", `{"structure_id":1035000000001,"name":"Planted Keepstar"}`, from(bad))
	// One this instance has since fetched from ESI itself.
	apply(swarm.SwarmKindCorporation, "98000002", orgEntry(t, 98000002, `{"corp":{"name":"Planted Too"}}`), from(bad))
	if err := app.queries.SetCorporationRecord(ctx, db.SetCorporationRecordParams{
		CorporationID: 98000002, Payload: `{"corp":{"name":"Fetched Here"}}`, State: orgStateReady, FetchedAt: timeSet(now.Add(time.Hour)),
	}); err != nil {
		t.Fatalf("refetch corporation: %v", err)
	}
	apply(swarm.SwarmKindCorporation, "98000003", orgEntry(t, 98000003, `{"corp":{"name":"Honest Corp"}}`), from(good))

	if got := swarm.StateFor(app).SwarmRollBack(ctx); got != 0 {
		t.Fatalf("rolled back %d rows with nobody excluded", got)
	}
	if err := app.queries.SetSwarmPeerExcluded(ctx, db.SetSwarmPeerExcludedParams{InstanceID: bad.ID, Excluded: true, ExcludeNote: "manual test"}); err != nil {
		t.Fatalf("exclude: %v", err)
	}
	if got := swarm.StateFor(app).SwarmRollBack(ctx); got != 5 {
		t.Fatalf("rolled back %d rows, want 5 (corporation, alliance, history, type name, structure)", got)
	}

	if rec, err := app.queries.GetCorporationRecord(ctx, 98000001); err != nil || rec.State != orgStatePending || rec.Payload != "" || rec.FetchedAt.Valid {
		t.Fatalf("planted corporation still stands: %+v err=%v", rec, err)
	}
	if rec, err := app.queries.GetAllianceRecord(ctx, 99000001); err != nil || rec.State != orgStatePending || rec.Payload != "" || rec.FetchedAt.Valid {
		t.Fatalf("planted alliance still stands: %+v err=%v", rec, err)
	}
	days, err := app.queries.ListMarketHistory(ctx, db.ListMarketHistoryParams{RegionID: 10000002, TypeID: 34, RowLimit: 10})
	if err != nil || len(days) != 1 || days[0].Date != "2026-09-15" {
		t.Fatalf("history after rollback: %+v err=%v, want only the day this instance held itself", days, err)
	}
	if _, err := app.queries.GetMarketFetchState(ctx, marketFetchKind("history", marketKey{RegionID: 10000002, TypeID: 34})); err == nil {
		t.Fatalf("the pair still reads as fetched; the worker would not refetch it")
	}
	if name, err := app.queries.GetTypeName(ctx, 34); err == nil {
		t.Fatalf("planted type name still stands: %q", name)
	}
	if row, err := app.queries.GetStructureName(ctx, 1035000000001); err != nil || row.State != esi.StructurePending || row.Name != "" {
		t.Fatalf("planted structure name still stands: %+v err=%v", row, err)
	}
	if rec, err := app.queries.GetCorporationRecord(ctx, 98000002); err != nil || rec.State != orgStateReady || rec.Payload != `{"corp":{"name":"Fetched Here"}}` {
		t.Fatalf("a record this instance fetched itself was taken back: %+v err=%v", rec, err)
	}
	if rec, err := app.queries.GetCorporationRecord(ctx, 98000003); err != nil || rec.State != orgStateReady {
		t.Fatalf("another signer's record was taken back: %+v err=%v", rec, err)
	}
	if n, err := app.queries.CountSwarmAppliedBySigner(ctx, bad.ID); err != nil || n != 0 {
		t.Fatalf("%d provenance rows left under the excluded signer (err %v)", n, err)
	}
	if n, err := app.queries.CountSwarmAppliedBySigner(ctx, good.ID); err != nil || n != 1 {
		t.Fatalf("the honest signer has %d provenance rows, want 1 (err %v)", n, err)
	}
	if got := swarm.StateFor(app).SwarmCounterMap(ctx)[swarm.SwarmCounterRolledBack]; got != 5 {
		t.Fatalf("rolled-back counter = %d, want 5", got)
	}
	if got := swarm.StateFor(app).SwarmRollBack(ctx); got != 0 {
		t.Fatalf("a second pass rolled back %d more rows", got)
	}
}

// TestSwarmOnePeerRoundPerCycle: across both of a cycle's swarm
// passes, a peer is asked for its member list once and for the
// wanted keys once. The later pass only settles what is staged.
func TestSwarmOnePeerRoundPerCycle(t *testing.T) {
	ctx := context.Background()
	app := swarmTestApp(t)
	peer, err := swarm.MintSwarmIdentity()
	if err != nil {
		t.Fatalf("mint peer: %v", err)
	}
	peer.ID = "peer-counted"
	now := time.Now().UTC()
	hits := map[string]int{}
	var hitsMu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsMu.Lock()
		hits[r.URL.Path]++
		hitsMu.Unlock()
		switch r.URL.Path {
		case "/swarm/pull":
			swarm.WriteSwarmJSON(w, map[string]any{"entries": []swarm.SwarmSignedEntry{}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	if _, err := app.queries.UpsertSwarmPeer(ctx, db.UpsertSwarmPeerParams{
		InstanceID: peer.ID, BaseUrl: srv.URL, PublicKey: peer.PublicString(),
		Source: "manual", FirstSeen: now,
	}); err != nil {
		t.Fatalf("pin peer: %v", err)
	}
	if err := app.queries.SetSwarmPeerApproved(ctx, db.SetSwarmPeerApprovedParams{InstanceID: peer.ID, Approved: true}); err != nil {
		t.Fatalf("approve peer: %v", err)
	}
	// Something to ask for: one pending corporation record.
	if err := app.queries.UpsertCorporationWant(ctx, db.UpsertCorporationWantParams{
		CorporationID: 98000001, Priority: wantViewed, NotedAt: timeSet(now),
	}); err != nil {
		t.Fatalf("queue a want: %v", err)
	}

	swarm.StateFor(app).SwarmFetchPhase(ctx, now)
	if _, limited := swarm.StateFor(app).SwarmCycle(ctx, &fetchBudget{left: 10}, nil, now); limited {
		t.Fatalf("the later pass reported the ESI limit with nothing staged")
	}
	if hits["/swarm/members"] != 1 {
		t.Fatalf("peer was asked for its member list %d times in one cycle, want 1", hits["/swarm/members"])
	}
	if hits["/swarm/pull"] != 1 {
		t.Fatalf("peer was pulled %d times in one cycle, want 1", hits["/swarm/pull"])
	}
}

// TestSwarmGossipTimeDrawnOnce: the next round's time is drawn
// when a round ends, inside the jitter window, and asking again
// does not move it. (Drawing per check fired at the first low
// draw, so rounds bunched at the early edge of the window.)
func TestSwarmGossipTimeDrawnOnce(t *testing.T) {
	ctx := context.Background()
	app := swarmTestApp(t)
	interval := time.Duration(swarm.StateFor(app).Cfg.GossipHours) * time.Hour
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	swarm.StateFor(app).SwarmGossipDone(ctx, now)

	swarm.StateFor(app).SwarmMu.Lock()
	next := swarm.StateFor(app).SwarmNextGossip
	swarm.StateFor(app).SwarmMu.Unlock()
	if next.Before(now.Add(interval*3/4)) || next.After(now.Add(interval*5/4)) {
		t.Fatalf("next round at %v, outside %v ±25%% of %v", next, interval, now)
	}
	// Anywhere inside the window the answer is settled by the one
	// draw: before it never, from it always, however often asked.
	for i := 0; i < 500; i++ {
		if swarm.StateFor(app).SwarmGossipDue(ctx, next.Add(-time.Second)) {
			t.Fatalf("check %d: due a second before the drawn time", i)
		}
		if !swarm.StateFor(app).SwarmGossipDue(ctx, next) {
			t.Fatalf("check %d: not due at the drawn time", i)
		}
	}
}

// TestSwarmGossipTimeSurvivesRestart: the drawn time is kept
// with the identity, so a restarted instance waits for it; a
// freshly minted identity starts due.
func TestSwarmGossipTimeSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	app := swarmTestApp(t)
	if err := app.queries.PutSwarmIdentity(ctx, db.PutSwarmIdentityParams{
		InstanceID: swarm.StateFor(app).SwarmID.ID, PublicKey: swarm.StateFor(app).SwarmID.PublicString(), SealedSeed: "sw:v1:test", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("store identity: %v", err)
	}
	now := time.Now().UTC()
	swarm.StateFor(app).SwarmGossipDone(ctx, now)

	// A restart forgets everything held in memory.
	swarm.StateFor(app).SwarmMu.Lock()
	swarm.StateFor(app).SwarmNextGossip, swarm.StateFor(app).SwarmNextLoaded = time.Time{}, false
	swarm.StateFor(app).SwarmMu.Unlock()
	if swarm.StateFor(app).SwarmGossipDue(ctx, now.Add(time.Hour)) {
		t.Fatalf("a restart made gossip due an hour after the last round")
	}
	if !swarm.StateFor(app).SwarmGossipDue(ctx, now.Add(200*time.Hour)) {
		t.Fatalf("gossip never comes due after a restart")
	}

	// A new identity (the secret changed) starts from scratch.
	if err := app.queries.PutSwarmIdentity(ctx, db.PutSwarmIdentityParams{
		InstanceID: "fresh-identity", PublicKey: swarm.StateFor(app).SwarmID.PublicString(), SealedSeed: "sw:v1:test", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("store fresh identity: %v", err)
	}
	swarm.StateFor(app).SwarmMu.Lock()
	swarm.StateFor(app).SwarmNextGossip, swarm.StateFor(app).SwarmNextLoaded = time.Time{}, false
	swarm.StateFor(app).SwarmMu.Unlock()
	if !swarm.StateFor(app).SwarmGossipDue(ctx, now.Add(time.Minute)) {
		t.Fatalf("a fresh identity did not start with gossip due")
	}
}

// TestSwarmAppliedRowKeepsSignedFetchTime: a row a peer supplied
// is stamped with the fetch time its signer signed, not the
// moment it arrived, so it is exactly as old here as at its
// source. A time ahead of this clock is held to now.
func TestSwarmAppliedRowKeepsSignedFetchTime(t *testing.T) {
	ctx := context.Background()
	app := swarmTestApp(t)
	peer := swarmTestPeer(t, app, "peer-1")
	now := time.Now().UTC()
	fetched := now.Add(-5 * time.Hour).Truncate(time.Second)
	origin := swarm.SwarmOrigin{Source: peer.ID, Signer: peer.ID, FetchedAt: fetched}

	if err := swarm.StateFor(app).ApplySwarmEntry(ctx, swarm.SwarmKindCorporation, "98000001", orgEntry(t, 98000001, `{"corp":{"name":"Test Corp"}}`), origin, now); err != nil {
		t.Fatalf("apply corporation: %v", err)
	}
	rec, err := app.queries.GetCorporationRecord(ctx, 98000001)
	if err != nil || !rec.FetchedAt.Valid || !rec.FetchedAt.Time.Equal(fetched) {
		t.Fatalf("corporation fetched_at = %v (err %v), want the signed %v", rec.FetchedAt, err, fetched)
	}
	if err := swarm.StateFor(app).ApplySwarmEntry(ctx, swarm.SwarmKindStructure, "1035000000001", `{"structure_id":1035000000001,"name":"Test Keepstar"}`, origin, now); err != nil {
		t.Fatalf("apply structure: %v", err)
	}
	if row, err := app.queries.GetStructureName(ctx, 1035000000001); err != nil || !row.ResolvedAt.Valid || !row.ResolvedAt.Time.Equal(fetched) {
		t.Fatalf("structure resolved_at = %v (err %v), want the signed %v", row.ResolvedAt, err, fetched)
	}
	history := `{"region_id":10000002,"type_id":34,"days":[{"date":"2026-10-01","average":5,"highest":6,"lowest":4,"volume":10,"order_count":2}]}`
	if err := swarm.StateFor(app).ApplySwarmEntry(ctx, swarm.SwarmKindHistory, "10000002/34", history, origin, now); err != nil {
		t.Fatalf("apply history: %v", err)
	}
	state, err := app.queries.GetMarketFetchState(ctx, marketFetchKind("history", marketKey{RegionID: 10000002, TypeID: 34}))
	if err != nil || !state.AttemptedAt.Equal(fetched) || state.Detail != swarm.SwarmFetchDetail {
		t.Fatalf("history fetch state = %+v (err %v), want the signed time %v marked as the swarm's", state, err, fetched)
	}
	prov, err := app.queries.GetSwarmApplied(ctx, db.GetSwarmAppliedParams{Kind: swarm.SwarmKindCorporation, Key: "98000001"})
	if err != nil || !prov.FetchedAt.Equal(fetched) || prov.AppliedAt.Before(now.Add(-time.Minute)) {
		t.Fatalf("provenance = %+v (err %v), want fetched %v and applied about now", prov, err, fetched)
	}

	// A signer's clock running ahead cannot date a row in the future.
	ahead := swarm.SwarmOrigin{Source: peer.ID, Signer: peer.ID, FetchedAt: now.Add(48 * time.Hour)}
	if err := swarm.StateFor(app).ApplySwarmEntry(ctx, swarm.SwarmKindAlliance, "99000001", orgEntry(t, 99000001, `{"alliance":{"name":"Test Alliance"}}`), ahead, now); err != nil {
		t.Fatalf("apply alliance: %v", err)
	}
	if rec, err := app.queries.GetAllianceRecord(ctx, 99000001); err != nil || !rec.FetchedAt.Valid || rec.FetchedAt.Time.After(now.Add(time.Second)) {
		t.Fatalf("alliance fetched_at = %v (err %v), dated ahead of this clock", rec.FetchedAt, err)
	}
}

// TestSwarmNeverServesWhatAPeerSupplied: a row a peer supplied
// is not served onward, so this instance never signs another's
// data as its own. Once its own worker has fetched the datum
// from ESI the row is its own, and serves under its own time.
func TestSwarmNeverServesWhatAPeerSupplied(t *testing.T) {
	ctx := context.Background()
	app := swarmTestApp(t)
	peer := swarmTestPeer(t, app, "peer-1")
	now := time.Now().UTC()
	origin := swarm.SwarmOrigin{Source: peer.ID, Signer: peer.ID, FetchedAt: now.Add(-time.Hour).Truncate(time.Second)}
	entries := []struct{ kind, key, payload string }{
		{swarm.SwarmKindCorporation, "98000001", orgEntry(t, 98000001, `{"corp":{"name":"Test Corp"}}`)},
		{swarm.SwarmKindAlliance, "99000001", orgEntry(t, 99000001, `{"alliance":{"name":"Test Alliance"}}`)},
		{swarm.SwarmKindTypeName, "34", `{"type_id":34,"name":"Tritanium"}`},
		{swarm.SwarmKindStructure, "1035000000001", `{"structure_id":1035000000001,"name":"Test Keepstar"}`},
		{swarm.SwarmKindHistory, "10000002/34", `{"region_id":10000002,"type_id":34,"days":[{"date":"2026-10-01","average":5,"highest":6,"lowest":4,"volume":10,"order_count":2}]}`},
	}
	var asked []swarm.SwarmPullKey
	for _, e := range entries {
		if err := swarm.StateFor(app).ApplySwarmEntry(ctx, e.kind, e.key, e.payload, origin, now); err != nil {
			t.Fatalf("apply %s %s: %v", e.kind, e.key, err)
		}
		if _, _, ok := swarm.StateFor(app).ServeSwarmKey(ctx, e.kind, e.key); ok {
			t.Fatalf("%s %s was supplied by a peer and is being served onward", e.kind, e.key)
		}
		asked = append(asked, swarm.SwarmPullKey{Kind: e.kind, Key: e.key})
	}
	// The same over the wire: a pull for all five comes back empty.
	code, body := swarmServe(t, app, http.MethodPost, "/swarm/pull", string(mustJSON(t, swarm.SwarmPullRequest{Keys: asked})), swarmTestSecret)
	if code != http.StatusOK || !strings.Contains(body, `"entries":[]`) {
		t.Fatalf("pull of peer-supplied rows: status %d body %s, want no entries", code, body)
	}

	// This instance fetches two of them itself: those are its own now.
	own := now.Add(time.Minute).Truncate(time.Second)
	if err := app.queries.SetCorporationRecord(ctx, db.SetCorporationRecordParams{
		CorporationID: 98000001, Payload: `{"corp":{"name":"Test Corp"}}`, State: orgStateReady, FetchedAt: timeSet(own),
	}); err != nil {
		t.Fatalf("own corporation fetch: %v", err)
	}
	if _, at, ok := swarm.StateFor(app).ServeSwarmKey(ctx, swarm.SwarmKindCorporation, "98000001"); !ok || !at.Equal(own) {
		t.Fatalf("own corporation fetch: served=%v at %v, want served at %v", ok, at, own)
	}
	app.recordMarketFetch(ctx, marketFetchKind("history", marketKey{RegionID: 10000002, TypeID: 34}), fetchStateOK, "")
	if _, _, ok := swarm.StateFor(app).ServeSwarmKey(ctx, swarm.SwarmKindHistory, "10000002/34"); !ok {
		t.Fatalf("history this instance fetched itself is not served")
	}
	// The alliance is still the peer's copy.
	if _, _, ok := swarm.StateFor(app).ServeSwarmKey(ctx, swarm.SwarmKindAlliance, "99000001"); ok {
		t.Fatalf("the alliance record is still the peer's copy and was served")
	}
}

// TestSwarmStagingHoldsFetchTimeToItsOwnClock: the signed fetch
// time is the signer's clock. Far ahead of this one it is a lie
// (refused, and it costs the signer); a little ahead is drift
// (held to now); very old is not staged at all, without blame.
// A far-future time once outranked every honest answer for its
// key and went on to date the applied row.
func TestSwarmStagingHoldsFetchTimeToItsOwnClock(t *testing.T) {
	ctx := context.Background()
	app := swarmTestApp(t)
	peer := swarmTestPeer(t, app, "peer-1")
	now := time.Now().UTC().Truncate(time.Second)
	name := func(id string, at time.Time) swarm.SwarmSignedEntry {
		return signedBy(t, peer, swarm.SwarmKindTypeName, id, `{"type_id":`+id+`,"name":"Type `+id+`"}`, at)
	}
	marks := func() int64 {
		row, err := app.queries.GetSwarmPeer(ctx, peer.ID)
		if err != nil {
			t.Fatalf("read peer: %v", err)
		}
		return row.BadChecks
	}

	if err := swarm.StateFor(app).StageSwarmEntry(ctx, name("34", now.Add(365*24*time.Hour)), peer.ID, now); err == nil {
		t.Fatalf("an entry fetched a year from now was staged")
	}
	if got := marks(); got != 1 {
		t.Fatalf("a far-future fetch time cost the signer %d marks, want 1", got)
	}
	if err := swarm.StateFor(app).StageSwarmEntry(ctx, name("35", now.Add(2*time.Minute)), peer.ID, now); err != nil {
		t.Fatalf("an entry two minutes ahead (clock drift) was refused: %v", err)
	}
	row, err := app.queries.GetStagingEntry(ctx, db.GetStagingEntryParams{Kind: swarm.SwarmKindTypeName, Key: "35"})
	if err != nil || row.FetchedAt.After(now) {
		t.Fatalf("drifted entry staged with fetched_at %v (err %v), want it held to %v", row.FetchedAt, err, now)
	}
	if err := swarm.StateFor(app).StageSwarmEntry(ctx, name("36", now.Add(-200*24*time.Hour)), peer.ID, now); err == nil {
		t.Fatalf("an entry fetched 200 days ago was staged")
	}
	if err := swarm.StateFor(app).StageSwarmEntry(ctx, name("37", now.Add(-3*time.Hour)), peer.ID, now); err != nil {
		t.Fatalf("an ordinary entry was refused: %v", err)
	}
	if got := marks(); got != 1 {
		t.Fatalf("drift, age and an ordinary entry changed the ledger: %d marks, want 1", got)
	}
	staged := stagedKeys(t, app)
	if len(staged) != 2 {
		t.Fatalf("staging holds %v, want types 35 and 37 only", staged)
	}
}

// historyDays builds a history payload for The Forge / Tritanium
// from dates, every day carrying the same figures ESI's canned
// truth below carries.
func historyDays(dates ...string) string {
	var b strings.Builder
	b.WriteString(`{"region_id":10000002,"type_id":34,"days":[`)
	for i, d := range dates {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"date":"` + d + `","average":5.5,"highest":6,"lowest":5,"volume":100,"order_count":10}`)
	}
	b.WriteString(`]}`)
	return b.String()
}

// esiHistory is the same days as ESI serves them.
func esiHistory(dates ...string) string {
	var rows []string
	for _, d := range dates {
		rows = append(rows, `{"date":"`+d+`","average":5.5,"highest":6,"lowest":5,"volume":100,"order_count":10}`)
	}
	return "[" + strings.Join(rows, ",") + "]"
}

// TestSwarmStagingRefusesImpossibleDays: a history entry whose
// days are not real dates, repeat, or fall after the fetch that
// is said to have returned them never stages, and costs its
// signer a mark each time.
func TestSwarmStagingRefusesImpossibleDays(t *testing.T) {
	ctx := context.Background()
	app := swarmTestApp(t)
	peer := swarmTestPeer(t, app, "peer-1")
	now := time.Now().UTC()
	fetched := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	if now.Before(fetched) {
		t.Skip("clock is before the fixture dates")
	}
	// Staging takes the clock as a parameter: keep it just after the fetch.
	at := fetched.Add(time.Hour)
	for name, payload := range map[string]string{
		"not a date":         historyDays("2026-13-40"),
		"another format":     historyDays("10/01/2026"),
		"a repeated day":     historyDays("2026-10-01", "2026-10-01"),
		"a day after fetch":  historyDays("2026-10-01", "2026-10-06"),
		"a day far in front": historyDays("2031-01-01"),
	} {
		entry := signedBy(t, peer, swarm.SwarmKindHistory, "10000002/34", payload, fetched)
		if err := swarm.StateFor(app).StageSwarmEntry(ctx, entry, peer.ID, at); err == nil {
			t.Fatalf("%s: the entry staged", name)
		}
	}
	if staged := stagedKeys(t, app); len(staged) != 0 {
		t.Fatalf("staging holds %v, want nothing", staged)
	}
	row, err := app.queries.GetSwarmPeer(ctx, peer.ID)
	if err != nil || row.BadChecks != 5 {
		t.Fatalf("signer has %d marks (err %v), want one per refused entry", row.BadChecks, err)
	}
	// The fetch day itself is fine.
	good := signedBy(t, peer, swarm.SwarmKindHistory, "10000002/34", historyDays("2026-10-05", "2026-10-04"), fetched)
	if err := swarm.StateFor(app).StageSwarmEntry(ctx, good, peer.ID, at); err != nil {
		t.Fatalf("an entry ending on its fetch day was refused: %v", err)
	}
}

// TestSwarmVerifyOrgRecordDialects: the corporation and
// alliance verifiers read the record in either dialect —
// this app's wrapped shape or a bare ESI record — through
// the tolerant decoders, and compare the fields actually
// sent. Matching records pass in both dialects, wrong
// identity fields fail in both, and a body naming nothing
// under either dialect is undecidable rather than a lie:
// it defers, so no shape bug can mint a data verdict.
func TestSwarmVerifyOrgRecordDialects(t *testing.T) {
	ctx := context.Background()
	p := newSwarmPair(t, swarm.SwarmOpen)
	const corpID = int64(98000001)
	const allyID = int64(99000001)
	corpTruth := `{"name":"Fixture Corp","ticker":"FIX","member_count":42,"ceo_id":1,"creator_id":1,"alliance_id":99000001,"home_station_id":60003760,"tax_rate":0.1,"date_founded":"2020-01-01T00:00:00Z","description":"","url":""}`
	allyTruth := `{"name":"Fixture Alliance","ticker":"FALL","creator_id":1,"creator_corporation_id":98000001,"executor_corporation_id":98000001,"date_founded":"2020-01-01T00:00:00Z"}`
	p.stubB.corps = map[int64]string{corpID: corpTruth}
	p.stubB.allies = map[int64]string{allyID: allyTruth}
	verify := func(kind, key, payload string) swarm.SwarmCheckVerdict {
		t.Helper()
		verdict, _, stop := swarm.StateFor(p.b).SwarmVerifyAgainstESI(ctx, &fetchBudget{left: 5}, nil, kind, key, payload)
		if stop {
			t.Fatalf("canned ESI reported the error limit")
		}
		return verdict
	}
	corpKey := fmt.Sprintf("%d", corpID)
	allyKey := fmt.Sprintf("%d", allyID)
	for name, tc := range map[string]struct {
		kind, key, payload string
		want               swarm.SwarmCheckVerdict
	}{
		"corporation bare, true":            {swarm.SwarmKindCorporation, corpKey, orgEntry(t, corpID, corpTruth), swarm.SwarmCheckPass},
		"corporation wrapped, true":         {swarm.SwarmKindCorporation, corpKey, orgEntry(t, corpID, fmt.Sprintf(`{"corp":%s,"alliance":{"name":"","ticker":""}}`, corpTruth)), swarm.SwarmCheckPass},
		"corporation bare, wrong name":      {swarm.SwarmKindCorporation, corpKey, orgEntry(t, corpID, strings.Replace(corpTruth, "Fixture Corp", "Liar Corp", 1)), swarm.SwarmCheckFail},
		"corporation wrapped, wrong ticker": {swarm.SwarmKindCorporation, corpKey, orgEntry(t, corpID, fmt.Sprintf(`{"corp":%s,"alliance":{"name":"","ticker":""}}`, strings.Replace(corpTruth, `"FIX"`, `"LIE"`, 1))), swarm.SwarmCheckFail},
		"corporation bare, wrong home":      {swarm.SwarmKindCorporation, corpKey, orgEntry(t, corpID, strings.Replace(corpTruth, `"alliance_id":99000001`, `"alliance_id":99000002`, 1)), swarm.SwarmCheckFail},
		"corporation names nothing":         {swarm.SwarmKindCorporation, corpKey, orgEntry(t, corpID, `{}`), swarm.SwarmCheckDefer},
		"corporation wrapped empty":         {swarm.SwarmKindCorporation, corpKey, orgEntry(t, corpID, `{"corp":{"name":""}}`), swarm.SwarmCheckDefer},
		"alliance bare, true":               {swarm.SwarmKindAlliance, allyKey, orgEntry(t, allyID, allyTruth), swarm.SwarmCheckPass},
		"alliance wrapped, true":            {swarm.SwarmKindAlliance, allyKey, orgEntry(t, allyID, fmt.Sprintf(`{"alliance":%s,"corporations":[]}`, allyTruth)), swarm.SwarmCheckPass},
		"alliance bare, wrong ticker":       {swarm.SwarmKindAlliance, allyKey, orgEntry(t, allyID, strings.Replace(allyTruth, `"FALL"`, `"LIES"`, 1)), swarm.SwarmCheckFail},
		"alliance names nothing":            {swarm.SwarmKindAlliance, allyKey, orgEntry(t, allyID, `{}`), swarm.SwarmCheckDefer},
	} {
		if got := verify(tc.kind, tc.key, tc.payload); got != tc.want {
			t.Fatalf("%s: verdict %d, want %d", name, got, tc.want)
		}
	}
}

// TestSwarmStagingRefusesNamelessOrgRecords: a corporation or
// alliance record body that names nothing under either dialect
// never stages — the shape gate binds the body to a record
// shape, not just to JSON. The refusal is the flat shape
// reject every malformed entry gets: one mark per refusal, no
// failure streak, and a peer in good standing stays admitted.
// Bodies naming a record in either dialect stage normally.
func TestSwarmStagingRefusesNamelessOrgRecords(t *testing.T) {
	ctx := context.Background()
	app := swarmTestApp(t)
	peer := swarmTestPeer(t, app, "peer-1")
	if err := app.queries.AddSwarmPeerCheck(ctx, db.AddSwarmPeerCheckParams{InstanceID: peer.ID, GoodChecks: 40}); err != nil {
		t.Fatalf("give the peer a record: %v", err)
	}
	now := time.Now().UTC()
	for name, entry := range map[string]swarm.SwarmSignedEntry{
		"corporation, empty object":      signedBy(t, peer, swarm.SwarmKindCorporation, "98000001", orgEntry(t, 98000001, `{}`), now),
		"corporation, wrapped but empty": signedBy(t, peer, swarm.SwarmKindCorporation, "98000001", orgEntry(t, 98000001, `{"corp":{"name":""}}`), now),
		"corporation, bare but empty":    signedBy(t, peer, swarm.SwarmKindCorporation, "98000001", orgEntry(t, 98000001, `{"name":""}`), now),
		"alliance, empty object":         signedBy(t, peer, swarm.SwarmKindAlliance, "99000001", orgEntry(t, 99000001, `{}`), now),
	} {
		if err := swarm.StateFor(app).StageSwarmEntry(ctx, entry, peer.ID, now); err == nil {
			t.Fatalf("%s: the entry staged", name)
		}
	}
	if staged := stagedKeys(t, app); len(staged) != 0 {
		t.Fatalf("staging holds %v, want nothing", staged)
	}
	row, err := app.queries.GetSwarmPeer(ctx, peer.ID)
	if err != nil || row.Excluded {
		t.Fatalf("peer excluded by shape rejects: %+v err %v", row, err)
	}
	if row.BadChecks != 4 {
		t.Fatalf("signer has %d marks, want one flat shape-reject mark per refusal (4)", row.BadChecks)
	}
	bare := signedBy(t, peer, swarm.SwarmKindCorporation, "98000001", orgEntry(t, 98000001, `{"name":"Fixture Corp","ticker":"FIX"}`), now)
	if err := swarm.StateFor(app).StageSwarmEntry(ctx, bare, peer.ID, now); err != nil {
		t.Fatalf("a bare record naming its corporation was refused: %v", err)
	}
	wrapped := signedBy(t, peer, swarm.SwarmKindAlliance, "99000001", orgEntry(t, 99000001, `{"alliance":{"name":"Fixture Alliance","ticker":"FALL"}}`), now)
	if err := swarm.StateFor(app).StageSwarmEntry(ctx, wrapped, peer.ID, now); err != nil {
		t.Fatalf("a wrapped record naming its alliance was refused: %v", err)
	}
	if staged := stagedKeys(t, app); len(staged) != 2 {
		t.Fatalf("staging holds %v, want both honest records", staged)
	}
}

// TestSwarmHistoryCheckCoversWholeSpan: the ESI check compares
// every day in the span the entry and ESI share. A made-up day,
// a dropped day or a changed figure inside it fails; days
// outside it cannot be checked and are left out of what
// applies; no shared span decides nothing.
func TestSwarmHistoryCheckCoversWholeSpan(t *testing.T) {
	ctx := context.Background()
	check := func(t *testing.T, truth, payload string) (swarm.SwarmCheckVerdict, string) {
		t.Helper()
		p := newSwarmPair(t, swarm.SwarmOpen)
		p.stubB.history = truth
		verdict, confirmed, stop := swarm.StateFor(p.b).SwarmVerifyAgainstESI(ctx, &fetchBudget{left: 5}, nil, swarm.SwarmKindHistory, "10000002/34", payload)
		if stop {
			t.Fatalf("canned ESI reported the error limit")
		}
		return verdict, confirmed
	}
	week := []string{"2026-10-01", "2026-10-02", "2026-10-03", "2026-10-04", "2026-10-05"}

	t.Run("honest and whole", func(t *testing.T) {
		verdict, confirmed := check(t, esiHistory(week...), historyDays("2026-10-04", "2026-10-03", "2026-10-02"))
		if verdict != swarm.SwarmCheckPass || strings.Count(confirmed, `"date"`) != 3 {
			t.Fatalf("verdict %d confirmed %s, want a pass with its three days", verdict, confirmed)
		}
	})
	t.Run("a made-up day inside the span", func(t *testing.T) {
		// ESI has no trades on the 3rd; the entry invents one.
		truth := esiHistory("2026-10-01", "2026-10-02", "2026-10-04", "2026-10-05")
		if verdict, _ := check(t, truth, historyDays(week...)); verdict != swarm.SwarmCheckFail {
			t.Fatalf("verdict %d, want a fail", verdict)
		}
	})
	t.Run("a dropped day inside the span", func(t *testing.T) {
		if verdict, _ := check(t, esiHistory(week...), historyDays("2026-10-01", "2026-10-02", "2026-10-04", "2026-10-05")); verdict != swarm.SwarmCheckFail {
			t.Fatalf("verdict %d, want a fail", verdict)
		}
	})
	t.Run("a changed figure", func(t *testing.T) {
		lie := strings.Replace(historyDays(week...), `"average":5.5`, `"average":999`, 1)
		if verdict, _ := check(t, esiHistory(week...), lie); verdict != swarm.SwarmCheckFail {
			t.Fatalf("verdict %d, want a fail", verdict)
		}
	})
	t.Run("days ESI cannot confirm are left out", func(t *testing.T) {
		// One day older than ESI still serves, one newer than it serves yet.
		entry := historyDays(append([]string{"2026-09-28"}, append(week, "2026-10-06")...)...)
		verdict, confirmed := check(t, esiHistory(week...), entry)
		if verdict != swarm.SwarmCheckPass {
			t.Fatalf("verdict %d, want a pass", verdict)
		}
		if strings.Count(confirmed, `"date"`) != 5 || strings.Contains(confirmed, "2026-09-28") || strings.Contains(confirmed, "2026-10-06") {
			t.Fatalf("confirmed %s, want only the five days ESI has", confirmed)
		}
	})
	t.Run("no shared span", func(t *testing.T) {
		if verdict, _ := check(t, esiHistory(week...), historyDays("2026-09-01")); verdict != swarm.SwarmCheckDefer {
			t.Fatalf("verdict %d, want a deferral", verdict)
		}
	})
}

// TestSwarmHistoryAppliesOnlyConfirmedDays: end to end, a
// checked entry writes the days ESI confirmed and no others.
func TestSwarmHistoryAppliesOnlyConfirmedDays(t *testing.T) {
	p := newSwarmPair(t, swarm.SwarmOpen)
	ctx := context.Background()
	p.pinA(t)
	// Recent days, so the entry is young enough to be checked.
	now := time.Now().UTC()
	day := func(ago int) string { return now.AddDate(0, 0, -ago).Format(time.DateOnly) }
	p.stubB.history = esiHistory(day(3), day(2), day(1))
	fetched := now.Add(-time.Minute)
	entry := signedBy(t, swarm.StateFor(p.a).SwarmID, swarm.SwarmKindHistory, "10000002/34",
		historyDays(day(15), day(3), day(2), day(1), fetched.Format(time.DateOnly)), fetched)
	asked := []swarm.SwarmPullKey{{Kind: swarm.SwarmKindHistory, Key: "10000002/34"}}
	if got := swarm.StateFor(p.b).SwarmStagePulled(ctx, "instance-a", p.urlA, asked, []swarm.SwarmSignedEntry{entry}, now); got != 1 {
		t.Fatalf("staged %d entries, want 1", got)
	}
	if got := p.spotRuns(t); got != 1 {
		t.Fatalf("applied %d entries, want 1", got)
	}
	days, err := p.b.queries.ListMarketHistory(ctx, db.ListMarketHistoryParams{RegionID: 10000002, TypeID: 34, RowLimit: 20})
	if err != nil || len(days) != 3 {
		t.Fatalf("history rows %+v (err %v), want the three days ESI confirmed", days, err)
	}
	for _, d := range days {
		if d.Date < day(3) || d.Date > day(1) {
			t.Fatalf("a day ESI never confirmed was written: %s", d.Date)
		}
	}
}

// TestSwarmHistoryApplyKeepsToItsWindow: an unchecked apply
// writes only days its fetch could have returned: none after
// it, none further back than one fetch reaches.
func TestSwarmHistoryApplyKeepsToItsWindow(t *testing.T) {
	ctx := context.Background()
	app := swarmTestApp(t)
	peer := swarmTestPeer(t, app, "peer-1")
	fetched := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	origin := swarm.SwarmOrigin{Source: peer.ID, Signer: peer.ID, FetchedAt: fetched}
	payload := historyDays("2024-01-01", "2026-10-04", "2026-10-05", "2026-10-09")
	if err := swarm.StateFor(app).ApplySwarmEntry(ctx, swarm.SwarmKindHistory, "10000002/34", payload, origin, fetched.Add(time.Hour)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	days, err := app.queries.ListMarketHistory(ctx, db.ListMarketHistoryParams{RegionID: 10000002, TypeID: 34, RowLimit: 20})
	if err != nil || len(days) != 2 {
		t.Fatalf("history rows %+v (err %v), want the 4th and 5th only", days, err)
	}
	prov, err := app.queries.GetSwarmApplied(ctx, db.GetSwarmAppliedParams{Kind: swarm.SwarmKindHistory, Key: "10000002/34"})
	if err != nil || prov.FirstDate != "2026-10-04" || prov.LastDate != "2026-10-05" {
		t.Fatalf("provenance range %q..%q (err %v), want the days written", prov.FirstDate, prov.LastDate, err)
	}
}

// ledgerOf reads a peer's marks.
func ledgerOf(t *testing.T, app *Application, id string) (good, bad int64) {
	t.Helper()
	row, err := app.queries.GetSwarmPeer(context.Background(), id)
	if err != nil {
		t.Fatalf("read peer %s: %v", id, err)
	}
	return row.GoodChecks, row.BadChecks
}

// TestSwarmAgedOutEntryIsDroppedNotFailed: an entry past the
// staging age cap was never compared with ESI. It leaves
// staging without a mark on its signer and without counting as
// a failed spot check.
func TestSwarmAgedOutEntryIsDroppedNotFailed(t *testing.T) {
	ctx := context.Background()
	app := swarmTestApp(t)
	peer := swarmTestPeer(t, app, "peer-1")
	now := time.Now().UTC()
	old := now.Add(-swarm.MaxSwarmStagingAge - time.Hour)
	entry := signedBy(t, peer, swarm.SwarmKindTypeName, "34", `{"type_id":34,"name":"Tritanium"}`, old)
	if err := swarm.StateFor(app).StageSwarmEntry(ctx, entry, peer.ID, old); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if applied, _ := swarm.StateFor(app).SwarmSpotCheck(ctx, &fetchBudget{left: 10}, nil, now); applied != 0 {
		t.Fatalf("an aged-out entry applied")
	}
	if staged := stagedKeys(t, app); len(staged) != 0 {
		t.Fatalf("staging still holds %v", staged)
	}
	if _, bad := ledgerOf(t, app, peer.ID); bad != 0 {
		t.Fatalf("an entry nobody checked cost its signer %d marks", bad)
	}
	counters := swarm.StateFor(app).SwarmCounterMap(ctx)
	if counters[swarm.SwarmCounterSpotFailed] != 0 || counters[swarm.SwarmCounterDroppedUnchecked] != 1 {
		t.Fatalf("counters %+v, want no failed check and one unchecked drop", counters)
	}
}

// TestSwarmUndecidableEntryIsDroppedAfterAFewPasses: a structure
// name cannot be checked without a character holding the scope.
// It defers a bounded number of times and is then dropped, with
// no mark and no ESI swarm.Call, instead of taking a check slot every
// cycle until the age cap.
func TestSwarmUndecidableEntryIsDroppedAfterAFewPasses(t *testing.T) {
	ctx := context.Background()
	app := swarmTestApp(t)
	peer := swarmTestPeer(t, app, "peer-1")
	now := time.Now().UTC()
	entry := signedBy(t, peer, swarm.SwarmKindStructure, "1035000000001", `{"structure_id":1035000000001,"name":"Test Keepstar"}`, now)
	if err := swarm.StateFor(app).StageSwarmEntry(ctx, entry, peer.ID, now); err != nil {
		t.Fatalf("stage: %v", err)
	}
	for pass := 1; pass < swarm.MaxSwarmDeferrals; pass++ {
		swarm.StateFor(app).SwarmSpotCheck(ctx, &fetchBudget{left: 10}, nil, now)
		if staged := stagedKeys(t, app); len(staged) != 1 {
			t.Fatalf("pass %d: the entry left staging early", pass)
		}
	}
	swarm.StateFor(app).SwarmSpotCheck(ctx, &fetchBudget{left: 10}, nil, now)
	if staged := stagedKeys(t, app); len(staged) != 0 {
		t.Fatalf("after %d passes the undecidable entry is still staged", swarm.MaxSwarmDeferrals)
	}
	if good, bad := ledgerOf(t, app, peer.ID); good != 0 || bad != 0 {
		t.Fatalf("an undecidable entry moved the ledger: good %d bad %d", good, bad)
	}
	counters := swarm.StateFor(app).SwarmCounterMap(ctx)
	if counters[swarm.SwarmCounterSpotFailed] != 0 || counters[swarm.SwarmCounterDroppedUnchecked] != 1 {
		t.Fatalf("counters %+v, want no failed check and one unchecked drop", counters)
	}
	if _, err := app.queries.GetStructureName(ctx, 1035000000001); err == nil {
		t.Fatalf("an undecidable entry was applied")
	}
}

// TestSwarmWaitingIsNotDeferring: a pass that has no calls left
// tries nothing, so nothing counts toward the deferral bound.
func TestSwarmWaitingIsNotDeferring(t *testing.T) {
	ctx := context.Background()
	app := swarmTestApp(t)
	peer := swarmTestPeer(t, app, "peer-1")
	now := time.Now().UTC()
	entry := signedBy(t, peer, swarm.SwarmKindTypeName, "34", `{"type_id":34,"name":"Tritanium"}`, now)
	if err := swarm.StateFor(app).StageSwarmEntry(ctx, entry, peer.ID, now); err != nil {
		t.Fatalf("stage: %v", err)
	}
	for pass := 0; pass < swarm.MaxSwarmDeferrals+2; pass++ {
		swarm.StateFor(app).SwarmSpotCheck(ctx, &fetchBudget{left: 0}, nil, now)
	}
	if staged := stagedKeys(t, app); len(staged) != 1 {
		t.Fatalf("an entry was dropped by passes that had no calls to check it with")
	}
}

// TestSwarmDeferredEntriesWaitBehindFreshOnes: the pass reads
// untried entries first, so one that keeps deferring cannot hold
// the front of the queue.
func TestSwarmDeferredEntriesWaitBehindFreshOnes(t *testing.T) {
	ctx := context.Background()
	app := swarmTestApp(t)
	peer := swarmTestPeer(t, app, "peer-1")
	now := time.Now().UTC()
	older := signedBy(t, peer, swarm.SwarmKindStructure, "1035000000001", `{"structure_id":1035000000001,"name":"Test Keepstar"}`, now.Add(-time.Hour))
	if err := swarm.StateFor(app).StageSwarmEntry(ctx, older, peer.ID, now.Add(-time.Hour)); err != nil {
		t.Fatalf("stage older: %v", err)
	}
	if _, err := app.queries.BumpStagingDeferral(ctx, db.BumpStagingDeferralParams{Kind: swarm.SwarmKindStructure, Key: "1035000000001"}); err != nil {
		t.Fatalf("defer older: %v", err)
	}
	fresh := signedBy(t, peer, swarm.SwarmKindTypeName, "34", `{"type_id":34,"name":"Tritanium"}`, now)
	if err := swarm.StateFor(app).StageSwarmEntry(ctx, fresh, peer.ID, now); err != nil {
		t.Fatalf("stage fresh: %v", err)
	}
	rows, err := app.queries.ListUncheckedStaging(ctx, 10)
	if err != nil || len(rows) != 2 || rows[0].Kind != swarm.SwarmKindTypeName {
		t.Fatalf("queue order %+v (err %v), want the untried entry first", rows, err)
	}
}

// TestSwarmLowTrustEntryIsDroppedWithoutAnotherMark: an entry
// from a signer already under the trust floor is not read. It
// was not checked either, so it adds nothing to the ledger.
func TestSwarmLowTrustEntryIsDroppedWithoutAnotherMark(t *testing.T) {
	ctx := context.Background()
	app := swarmTestApp(t)
	peer := swarmTestPeer(t, app, "peer-1")
	now := time.Now().UTC()
	if err := app.queries.AddSwarmPeerCheck(ctx, db.AddSwarmPeerCheckParams{InstanceID: peer.ID, BadChecks: 40}); err != nil {
		t.Fatalf("sink the signer: %v", err)
	}
	entry := signedBy(t, peer, swarm.SwarmKindTypeName, "34", `{"type_id":34,"name":"Tritanium"}`, now)
	if err := swarm.StateFor(app).StageSwarmEntry(ctx, entry, peer.ID, now); err != nil {
		t.Fatalf("stage: %v", err)
	}
	swarm.StateFor(app).SwarmSpotCheck(ctx, &fetchBudget{left: 10}, nil, now)
	if staged := stagedKeys(t, app); len(staged) != 0 {
		t.Fatalf("staging still holds %v", staged)
	}
	if _, bad := ledgerOf(t, app, peer.ID); bad != 40 {
		t.Fatalf("an unchecked entry moved the ledger to %d bad marks, want 40", bad)
	}
	if got := swarm.StateFor(app).SwarmCounterMap(ctx)[swarm.SwarmCounterSpotFailed]; got != 0 {
		t.Fatalf("an unchecked entry counted as %d failed checks", got)
	}
}

// hasKey reports whether a demand list asks for kind/key.
func hasKey(keys []swarm.SwarmPullKey, kind, key string) bool {
	for _, k := range keys {
		if k.Kind == kind && k.Key == key {
			return true
		}
	}
	return false
}

// TestSwarmDemandAsksForNames: a pull stages only what it asked
// for, so names have to be asked for. A type name a reader found
// missing and a structure name the resolver has due are both in
// the demand, ahead of the bulkier kinds.
func TestSwarmDemandAsksForNames(t *testing.T) {
	ctx := context.Background()
	app := swarmTestApp(t)
	const unknownType = 987654
	if name := app.esi.CachedTypeName(ctx, unknownType); name != "" {
		t.Fatalf("fixture type %d is already named %q", unknownType, name)
	}
	if err := app.queries.UpsertStructureSeen(ctx, 1035000000001); err != nil {
		t.Fatalf("note a structure: %v", err)
	}
	keys := swarm.StateFor(app).SwarmDemandKeys(ctx)
	if !hasKey(keys, swarm.SwarmKindTypeName, "987654") {
		t.Fatalf("demand %+v does not ask for the missing type name", keys)
	}
	if !hasKey(keys, swarm.SwarmKindStructure, "1035000000001") {
		t.Fatalf("demand %+v does not ask for the pending structure name", keys)
	}
	// A name learned settles its demand.
	app.esi.LearnTypeName(unknownType, "Test Type")
	if hasKey(swarm.StateFor(app).SwarmDemandKeys(ctx), swarm.SwarmKindTypeName, "987654") {
		t.Fatalf("a type name already learned is still being asked for")
	}
	if got := app.esi.CachedTypeName(ctx, unknownType); got != "Test Type" {
		t.Fatalf("learned name reads %q", got)
	}
}

// TestSwarmPairNamesFlowOnDemand: end to end. B misses a type
// name and has a structure pending; A holds both. One fetch
// phase asks A for them and stages A's answers, and the checked
// type name lands and stops being asked for.
func TestSwarmPairNamesFlowOnDemand(t *testing.T) {
	p := newSwarmPair(t, swarm.SwarmOpen)
	ctx := context.Background()
	now := time.Now().UTC()
	const unknownType = 987654
	if err := p.a.queries.UpsertTypeName(ctx, db.UpsertTypeNameParams{TypeID: unknownType, Name: "Test Type"}); err != nil {
		t.Fatalf("seed A type name: %v", err)
	}
	if err := p.a.queries.SetStructureName(ctx, db.SetStructureNameParams{
		StructureID: 1035000000001, Name: "Test Keepstar", State: esi.StructureResolved,
		ResolvedAt: timeSet(now), Source: esi.StructureSourceESI,
	}); err != nil {
		t.Fatalf("seed A structure name: %v", err)
	}
	p.stubB.types = map[int64]string{unknownType: `{"name":"Test Type","group_id":18}`}
	p.pinA(t)
	p.b.esi.CachedTypeName(ctx, unknownType) // a reader on B misses it
	if err := p.b.queries.UpsertStructureSeen(ctx, 1035000000001); err != nil {
		t.Fatalf("note the structure on B: %v", err)
	}

	swarm.StateFor(p.b).SwarmPull(ctx, now)
	staged := stagedKeys(t, p.b)
	if _, ok := staged[swarm.SwarmKindTypeName+"/987654"]; !ok {
		t.Fatalf("staging %v: the type name B asked for did not arrive", staged)
	}
	if _, ok := staged[swarm.SwarmKindStructure+"/1035000000001"]; !ok {
		t.Fatalf("staging %v: the structure name B asked for did not arrive", staged)
	}
	p.spotRuns(t)
	if got := p.b.esi.CachedTypeName(ctx, unknownType); got != "Test Type" {
		t.Fatalf("B reads the type as %q after the check, want the name A supplied", got)
	}
	if hasKey(swarm.StateFor(p.b).SwarmDemandKeys(ctx), swarm.SwarmKindTypeName, "987654") {
		t.Fatalf("B still asks for a type name it now holds")
	}
}

// TestSwarmRollbackForgetsACachedTypeName: taking a type name
// back removes it from memory too, not only from the table.
func TestSwarmRollbackForgetsACachedTypeName(t *testing.T) {
	ctx := context.Background()
	app := swarmTestApp(t)
	bad := swarmTestPeer(t, app, "peer-bad")
	now := time.Now().UTC()
	const unknownType = 987654
	origin := swarm.SwarmOrigin{Source: bad.ID, Signer: bad.ID, FetchedAt: now}
	if err := swarm.StateFor(app).ApplySwarmEntry(ctx, swarm.SwarmKindTypeName, "987654", `{"type_id":987654,"name":"Planted Name"}`, origin, now); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := app.esi.CachedTypeName(ctx, unknownType); got != "Planted Name" {
		t.Fatalf("applied name reads %q", got)
	}
	if err := app.queries.SetSwarmPeerExcluded(ctx, db.SetSwarmPeerExcludedParams{InstanceID: bad.ID, Excluded: true, ExcludeNote: "manual test"}); err != nil {
		t.Fatalf("exclude: %v", err)
	}
	if got := swarm.StateFor(app).SwarmRollBack(ctx); got != 1 {
		t.Fatalf("rolled back %d rows, want 1", got)
	}
	if got := app.esi.CachedTypeName(ctx, unknownType); got != "" {
		t.Fatalf("the planted name is still served from memory: %q", got)
	}
}

// TestSwarmMemberMessageVector pins the bytes a member signs to
// own its address, and one signature over them from a fixed key,
// for the seed worker to reproduce. If this changes, every
// instance and seed must change with it.
func TestSwarmMemberMessageVector(t *testing.T) {
	if got := string(swarm.SwarmMemberMessage("id-1", "https://node.example.org", testIssued)); got != "member\nid-1\nhttps://node.example.org\n1760000000" {
		t.Fatalf("member message %q", got)
	}
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	id := &swarm.SwarmIdentity{ID: "id-1", Public: priv.Public().(ed25519.PublicKey), Private: priv}
	const wantKey = "A6EHv/POEL4dcN0Y50vAmWfk1jCbpQ1fHdyGZBJVMbg="
	const wantSig = "RIsOrR2+TRyZ/VEgnPjXfCRbSClfr/gScvRKJj5It5KP7i9NoyAwbSf75pIBIlo+pUIZOr9Kg64XIjnV8hVhAg=="
	if id.PublicString() != wantKey {
		t.Fatalf("public key %s, want %s", id.PublicString(), wantKey)
	}
	if got := swarm.SignSwarmMember(id, "https://node.example.org", testIssued); got != wantSig {
		t.Fatalf("url_sig %s, want %s", got, wantSig)
	}
	tuple := swarm.SwarmMember{InstanceID: "id-1", BaseURL: "https://node.example.org", PublicKey: wantKey, URLSig: wantSig, IssuedAt: testIssued}
	if !swarm.VerifySwarmMember(tuple) {
		t.Fatalf("the pinned tuple does not verify")
	}
	tuple.IssuedAt++
	if swarm.VerifySwarmMember(tuple) {
		t.Fatalf("the signature verified over another issue time")
	}
	tuple.IssuedAt, tuple.BaseURL = testIssued, "https://other.example.org"
	if swarm.VerifySwarmMember(tuple) {
		t.Fatalf("the signature verified over another address")
	}
}

// TestSwarmMemberListCarriesOwnersSignatures: an instance signs
// its own entry and relays every other tuple exactly as it was
// stored. A peer it knows only by an address it dialed has no
// signed address and is not listed.
func TestSwarmMemberListCarriesOwnersSignatures(t *testing.T) {
	ctx := context.Background()
	app := swarmTestApp(t)
	app.cfg.eveCallbackURL = "https://node.example.org/auth/callback"
	now := time.Now().UTC()
	signedPeer, err := swarm.MintSwarmIdentity()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	signedPeer.ID = "peer-signed"
	sig := swarm.SignSwarmMember(signedPeer, "https://signed.example", testIssued)
	for _, row := range []db.UpsertSwarmPeerParams{
		{InstanceID: signedPeer.ID, BaseUrl: "https://signed.example", PublicKey: signedPeer.PublicString(), UrlSig: sig, UrlIssuedAt: testIssued},
		{InstanceID: "peer-dialed", BaseUrl: "https://dialed.example", PublicKey: strings.Repeat("A", 44)},
	} {
		row.Source, row.FirstSeen = "gossip", now
		if _, err := app.queries.UpsertSwarmPeer(ctx, row); err != nil {
			t.Fatalf("seed %s: %v", row.InstanceID, err)
		}
	}
	code, body := swarmServe(t, app, http.MethodGet, "/swarm/members", "", swarmTestSecret)
	if code != http.StatusOK {
		t.Fatalf("/swarm/members: status %d", code)
	}
	var envelope struct {
		Entry swarm.SwarmSignedEntry `json:"entry"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	members, _, _, err := swarm.DecodeSwarmMembers(swarmTestSecret, envelope.Entry.Payload, swarm.StateFor(app).SwarmID.ID, swarm.StateFor(app).SwarmID.PublicString())
	if err != nil {
		t.Fatalf("open member list: %v", err)
	}
	byID := map[string]swarm.SwarmMember{}
	for _, m := range members {
		byID[m.InstanceID] = m
	}
	self, ok := byID[swarm.StateFor(app).SwarmID.ID]
	if !ok || self.BaseURL != "https://node.example.org" || !swarm.VerifySwarmMember(self) {
		t.Fatalf("own entry %+v does not carry a valid address signature", self)
	}
	relayed, ok := byID[signedPeer.ID]
	if !ok || relayed.BaseURL != "https://signed.example" || relayed.PublicKey != signedPeer.PublicString() || relayed.URLSig != sig || relayed.IssuedAt != testIssued {
		t.Fatalf("relayed tuple %+v was not passed on as stored", relayed)
	}
	if _, listed := byID["peer-dialed"]; listed {
		t.Fatalf("a peer with no signed address was listed")
	}
}

// TestSwarmIntroductionNeedsItsOwnersSignature: a member list
// introduces a new instance only by a tuple that instance
// signed. A tuple with a made-up signature is not stored.
func TestSwarmIntroductionNeedsItsOwnersSignature(t *testing.T) {
	p := newSwarmPair(t, swarm.SwarmOpen)
	ctx := context.Background()
	now := time.Now().UTC()
	p.pinA(t)
	real, err := swarm.MintSwarmIdentity()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	real.ID = "instance-real"
	forged, err := swarm.MintSwarmIdentity()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	forged.ID = "instance-forged"
	someoneElse, err := swarm.MintSwarmIdentity()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	someoneElse.ID = forged.ID
	for _, row := range []db.UpsertSwarmPeerParams{
		{InstanceID: real.ID, BaseUrl: "https://real.example", PublicKey: real.PublicString(), UrlSig: swarm.SignSwarmMember(real, "https://real.example", testIssued), UrlIssuedAt: testIssued},
		// The key is the forged instance's; the signature is not made with it.
		{InstanceID: forged.ID, BaseUrl: "https://forged.example", PublicKey: forged.PublicString(), UrlSig: swarm.SignSwarmMember(someoneElse, "https://forged.example", testIssued), UrlIssuedAt: testIssued},
	} {
		row.Source, row.FirstSeen = "gossip", now
		if _, err := p.a.queries.UpsertSwarmPeer(ctx, row); err != nil {
			t.Fatalf("list %s in A: %v", row.InstanceID, err)
		}
	}
	if err := swarm.StateFor(p.b).SwarmContactPeer(ctx, p.urlA, "instance-a", swarm.StateFor(p.a).SwarmID.PublicString(), "gossip", now); err != nil {
		t.Fatalf("gossip with A: %v", err)
	}
	if peer, err := p.b.queries.GetSwarmPeer(ctx, real.ID); err != nil || peer.BaseUrl != "https://real.example" || peer.UrlSig == "" {
		t.Fatalf("a properly signed introduction was not stored: %+v err=%v", peer, err)
	}
	if _, err := p.b.queries.GetSwarmPeer(ctx, forged.ID); err == nil {
		t.Fatalf("an introduction its owner never signed was stored")
	}
}

// TestSwarmRegisterNeedsAnAddressSignature: a heartbeat without
// the sender's signature over its address is refused, however
// good its envelope signature.
func TestSwarmRegisterNeedsAnAddressSignature(t *testing.T) {
	app := swarmTestApp(t)
	kp, err := swarm.MintSwarmIdentity()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	kp.ID = "peer-unsigned"
	payload := string(mustJSON(t, swarm.SeedAnnouncePayload{BaseURL: "https://peer.example", PublicKey: kp.PublicString()}))
	entry := signedBy(t, kp, swarm.SeedAnnounceKind, kp.ID, payload, time.Now())
	if code, _ := seedTestRegister(t, app, entry, swarmTestSecret); code != http.StatusBadRequest {
		t.Fatalf("heartbeat with no url_sig: status %d, want 400", code)
	}
	if _, err := app.queries.GetSwarmPeer(context.Background(), kp.ID); err == nil {
		t.Fatalf("a heartbeat with no address signature filed a peer")
	}
}

// TestSwarmPullTakesPeersInTurn: with more unprobed peers than
// one cycle asks, the next cycle starts with the ones not yet
// asked, and each gets exactly its one probe.
func TestSwarmPullTakesPeersInTurn(t *testing.T) {
	ctx := context.Background()
	app := swarmTestApp(t)
	now := time.Now().UTC()
	hits := map[string]int{}
	var hitsMu sync.Mutex
	total := swarm.MaxSwarmPullPeersPerCycle + 1
	for i := 0; i < total; i++ {
		id := fmt.Sprintf("peer-%d", i)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hitsMu.Lock()
			hits[id]++
			hitsMu.Unlock()
			swarm.WriteSwarmJSON(w, map[string]any{"entries": []swarm.SwarmSignedEntry{}})
		}))
		t.Cleanup(srv.Close)
		if _, err := app.queries.UpsertSwarmPeer(ctx, db.UpsertSwarmPeerParams{
			InstanceID: id, BaseUrl: srv.URL, PublicKey: strings.Repeat("A", 44), Source: "manual", FirstSeen: now,
		}); err != nil {
			t.Fatalf("pin %s: %v", id, err)
		}
		if err := app.queries.SetSwarmPeerApproved(ctx, db.SetSwarmPeerApprovedParams{InstanceID: id, Approved: true}); err != nil {
			t.Fatalf("approve %s: %v", id, err)
		}
	}
	if err := app.queries.UpsertCorporationWant(ctx, db.UpsertCorporationWantParams{
		CorporationID: 98000001, Priority: wantViewed, NotedAt: timeSet(now),
	}); err != nil {
		t.Fatalf("queue a want: %v", err)
	}
	swarm.StateFor(app).SwarmPull(ctx, now)
	asked := 0
	for _, n := range hits {
		asked += n
	}
	if asked != swarm.MaxSwarmPullPeersPerCycle {
		t.Fatalf("first cycle asked %d peers, want %d", asked, swarm.MaxSwarmPullPeersPerCycle)
	}
	swarm.StateFor(app).SwarmPull(ctx, now.Add(time.Minute))
	for i := 0; i < total; i++ {
		if hits[fmt.Sprintf("peer-%d", i)] != 1 {
			t.Fatalf("after two cycles peer-%d has been asked %d times, want its one probe: %v", i, hits[fmt.Sprintf("peer-%d", i)], hits)
		}
	}
	// None of them served anything: none is asked for data again.
	swarm.StateFor(app).SwarmPull(ctx, now.Add(2*time.Minute))
	for id, n := range hits {
		if n != 1 {
			t.Fatalf("%s served nothing and was asked again (%d times)", id, n)
		}
	}
}

// TestSwarmDirectoryLeavesTheDataRotation: whether a peer serves
// data is learned from its pulls. A peer gets one probe; one
// that staged nothing is not asked for data again until a gossip
// round re-arms it, while a peer that served stays in the
// rotation cycle after cycle.
func TestSwarmDirectoryLeavesTheDataRotation(t *testing.T) {
	ctx := context.Background()
	app := swarmTestApp(t)
	now := time.Now().UTC()
	source, err := swarm.MintSwarmIdentity()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	source.ID = "peer-source"
	hits := map[string]int{}
	var hitsMu sync.Mutex
	serve := func(id string, entries func() []swarm.SwarmSignedEntry) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hitsMu.Lock()
			hits[id]++
			hitsMu.Unlock()
			swarm.WriteSwarmJSON(w, map[string]any{"entries": entries()})
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
	directoryURL := serve("peer-directory", func() []swarm.SwarmSignedEntry { return []swarm.SwarmSignedEntry{} })
	sourceURL := serve(source.ID, func() []swarm.SwarmSignedEntry {
		return []swarm.SwarmSignedEntry{signedBy(t, source, swarm.SwarmKindCorporation, "98000001", orgEntry(t, 98000001, `{"corp":{"name":"Test Corp"}}`), time.Now())}
	})
	for id, row := range map[string]struct{ url, key string }{
		"peer-directory": {directoryURL, strings.Repeat("A", 44)},
		source.ID:        {sourceURL, source.PublicString()},
	} {
		if _, err := app.queries.UpsertSwarmPeer(ctx, db.UpsertSwarmPeerParams{
			InstanceID: id, BaseUrl: row.url, PublicKey: row.key, Source: "manual", FirstSeen: now,
		}); err != nil {
			t.Fatalf("pin %s: %v", id, err)
		}
		if err := app.queries.SetSwarmPeerApproved(ctx, db.SetSwarmPeerApprovedParams{InstanceID: id, Approved: true}); err != nil {
			t.Fatalf("approve %s: %v", id, err)
		}
	}
	if err := app.queries.UpsertCorporationWant(ctx, db.UpsertCorporationWantParams{
		CorporationID: 98000001, Priority: wantViewed, NotedAt: timeSet(now),
	}); err != nil {
		t.Fatalf("queue a want: %v", err)
	}

	for cycle := 0; cycle < 4; cycle++ {
		swarm.StateFor(app).SwarmPull(ctx, now.Add(time.Duration(cycle)*time.Minute))
	}
	if hits["peer-directory"] != 1 {
		t.Fatalf("the directory was asked for data %d times in four cycles, want its one probe", hits["peer-directory"])
	}
	if hits[source.ID] != 4 {
		t.Fatalf("the peer that serves was asked %d times in four cycles, want 4", hits[source.ID])
	}

	// A gossip round that reaches the directory lets it be probed once more.
	if err := app.queries.RearmSwarmPeerProbe(ctx, db.RearmSwarmPeerProbeParams{InstanceID: "peer-directory", ServedSince: now.Add(-swarm.SwarmServesWindow)}); err != nil {
		t.Fatalf("re-arm: %v", err)
	}
	// Re-arming does nothing to a peer already known to serve.
	if err := app.queries.RearmSwarmPeerProbe(ctx, db.RearmSwarmPeerProbeParams{InstanceID: source.ID, ServedSince: now.Add(-swarm.SwarmServesWindow)}); err != nil {
		t.Fatalf("re-arm: %v", err)
	}
	swarm.StateFor(app).SwarmPull(ctx, now.Add(10*time.Minute))
	swarm.StateFor(app).SwarmPull(ctx, now.Add(11*time.Minute))
	if hits["peer-directory"] != 2 {
		t.Fatalf("after a re-arm the directory was asked %d times in all, want 2", hits["peer-directory"])
	}
	if hits[source.ID] != 6 {
		t.Fatalf("the serving peer was asked %d times in six cycles, want 6 (re-arming must not disturb it)", hits[source.ID])
	}
}

// TestSwarmUpgradeDropsTheLegacySeedRow: the schema step that
// brings signed addresses removes the peer every seed used to
// share, so nobody has to clear it by hand.
func TestSwarmUpgradeDropsTheLegacySeedRow(t *testing.T) {
	ctx := context.Background()
	app := swarmTestApp(t)
	now := time.Now().UTC()
	for _, id := range []string{"seed", "seed-like-but-real"} {
		if _, err := app.queries.UpsertSwarmPeer(ctx, db.UpsertSwarmPeerParams{
			InstanceID: id, BaseUrl: "https://" + id + ".example", PublicKey: strings.Repeat("A", 44), Source: "seed", FirstSeen: now,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	// The step's own statements, as a database that already held
	// the row would run them.
	script, err := os.ReadFile("../store/schema_pg/038_swarm_signed_addresses.sql")
	if err != nil {
		t.Fatalf("read schema step: %v", err)
	}
	if _, err := app.db.ExecContext(ctx, string(script)); err != nil {
		t.Fatalf("re-run schema step: %v", err)
	}
	if _, err := app.queries.GetSwarmPeer(ctx, "seed"); err == nil {
		t.Fatalf("the legacy seed row survived the upgrade step")
	}
	if _, err := app.queries.GetSwarmPeer(ctx, "seed-like-but-real"); err != nil {
		t.Fatalf("a real member was removed: %v", err)
	}
}

// TestSwarmUncheckableEntriesTakeNoCheckSlot: structure names
// cannot be checked here without a scoped character. A queue
// headed by a full cycle's worth of them still spends every
// check slot on entries that can be checked, and makes no ESI
// swarm.Call for the ones that cannot.
func TestSwarmUncheckableEntriesTakeNoCheckSlot(t *testing.T) {
	p := newSwarmPair(t, swarm.SwarmOpen)
	ctx := context.Background()
	p.pinA(t)
	now := time.Now().UTC()
	p.stubB.types = map[int64]string{}
	// Older, so they head the queue.
	for i := 0; i < swarm.MaxSwarmSpotPerCycle; i++ {
		id := fmt.Sprintf("%d", 1035000000001+i)
		entry := signedBy(t, swarm.StateFor(p.a).SwarmID, swarm.SwarmKindStructure, id, `{"structure_id":`+id+`,"name":"Structure `+id+`"}`, now.Add(-2*time.Hour))
		if err := swarm.StateFor(p.b).StageSwarmEntry(ctx, entry, "instance-a", now.Add(-time.Hour)); err != nil {
			t.Fatalf("stage structure: %v", err)
		}
	}
	for i := 0; i < swarm.MaxSwarmSpotPerCycle; i++ {
		typeID := int64(987000 + i)
		id := fmt.Sprintf("%d", typeID)
		p.stubB.types[typeID] = `{"name":"Type ` + id + `","group_id":18}`
		entry := signedBy(t, swarm.StateFor(p.a).SwarmID, swarm.SwarmKindTypeName, id, `{"type_id":`+id+`,"name":"Type `+id+`"}`, now)
		if err := swarm.StateFor(p.b).StageSwarmEntry(ctx, entry, "instance-a", now); err != nil {
			t.Fatalf("stage type name: %v", err)
		}
	}
	if got := p.spotRuns(t); got != swarm.MaxSwarmSpotPerCycle {
		t.Fatalf("one pass applied %d entries, want all %d checkable ones", got, swarm.MaxSwarmSpotPerCycle)
	}
	if p.stubB.calls != swarm.MaxSwarmSpotPerCycle {
		t.Fatalf("the pass made %d ESI calls, want %d (none for the uncheckable entries)", p.stubB.calls, swarm.MaxSwarmSpotPerCycle)
	}
	rows, err := p.b.queries.ListUncheckedStaging(ctx, 50)
	if err != nil || len(rows) != swarm.MaxSwarmSpotPerCycle {
		t.Fatalf("staging holds %d rows (err %v), want the %d structure names", len(rows), err, swarm.MaxSwarmSpotPerCycle)
	}
	for _, row := range rows {
		if row.Kind != swarm.SwarmKindStructure || row.Deferrals != 1 {
			t.Fatalf("left in staging: %s %s with %d deferrals, want a structure name deferred once", row.Kind, row.Key, row.Deferrals)
		}
	}
	if _, bad := ledgerOf(t, p.b, "instance-a"); bad != 0 {
		t.Fatalf("uncheckable entries cost the signer %d marks", bad)
	}
}
