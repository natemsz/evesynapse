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
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
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

// orgEntry is a corporation or alliance record as it travels.
func orgEntry(t *testing.T, id int64, record string) string {
	t.Helper()
	return string(mustJSON(t, swarmOrgPayload{ID: id, Payload: record}))
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

	origin := swarmOrigin{source: "peer-served-it", signer: peer.id, fetchedAt: fetched, verified: true}
	if err := app.applySwarmEntry(ctx, swarmKindCorporation, "98000001", orgEntry(t, 98000001, `{"corp":{"name":"Test Corp"}}`), origin, now); err != nil {
		t.Fatalf("apply corporation: %v", err)
	}
	got, err := app.queries.GetSwarmApplied(ctx, db.GetSwarmAppliedParams{Kind: swarmKindCorporation, Key: "98000001"})
	if err != nil {
		t.Fatalf("no provenance row: %v", err)
	}
	if got.Source != "peer-served-it" || got.Signer != peer.id || !got.FetchedAt.Equal(fetched) || !got.Verified {
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
	unchecked := swarmOrigin{signer: peer.id, fetchedAt: fetched}
	if err := app.applySwarmEntry(ctx, swarmKindHistory, "10000002/34", history, unchecked, now); err != nil {
		t.Fatalf("apply history: %v", err)
	}
	hist, err := app.queries.GetSwarmApplied(ctx, db.GetSwarmAppliedParams{Kind: swarmKindHistory, Key: "10000002/34"})
	if err != nil {
		t.Fatalf("no history provenance: %v", err)
	}
	if hist.FirstDate != "2026-10-01" || hist.LastDate != "2026-10-03" || hist.Verified {
		t.Fatalf("history provenance wrong: %+v", hist)
	}
	if hist.Source != peer.id {
		t.Fatalf("an entry staged without a source should fall back to its signer, got %q", hist.Source)
	}
}

// TestSwarmPairProvenanceEndToEnd: an entry pulled over HTTP,
// staged and checked carries its origin all the way to the row.
func TestSwarmPairProvenanceEndToEnd(t *testing.T) {
	p := newSwarmPair(t, swarmOpen)
	ctx := context.Background()
	if err := p.a.queries.UpsertTypeName(ctx, db.UpsertTypeNameParams{TypeID: 34, Name: "Tritanium"}); err != nil {
		t.Fatalf("seed A: %v", err)
	}
	p.stubB.types = map[int64]string{34: `{"name":"Tritanium","group_id":18}`}
	p.pinA(t)
	p.pullStages(t, []swarmPullKey{{Kind: swarmKindTypeName, Key: "34"}})
	if got := p.spotRuns(t); got != 1 {
		t.Fatalf("applied %d, want 1", got)
	}
	row, err := p.b.queries.GetSwarmApplied(ctx, db.GetSwarmAppliedParams{Kind: swarmKindTypeName, Key: "34"})
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
	from := func(peer *swarmIdentity) swarmOrigin {
		return swarmOrigin{source: peer.id, signer: peer.id, fetchedAt: fetched}
	}
	apply := func(kind, key, payload string, origin swarmOrigin) {
		t.Helper()
		if err := app.applySwarmEntry(ctx, kind, key, payload, origin, now); err != nil {
			t.Fatalf("apply %s %s: %v", kind, key, err)
		}
	}

	// A day this instance already held, outside the entry's range.
	if err := app.queries.UpsertMarketHistory(ctx, db.UpsertMarketHistoryParams{
		RegionID: 10000002, TypeID: 34, Date: "2026-09-15", Average: 5, Highest: 6, Lowest: 4, Volume: 9, OrderCount: 1,
	}); err != nil {
		t.Fatalf("seed own history: %v", err)
	}
	apply(swarmKindCorporation, "98000001", orgEntry(t, 98000001, `{"corp":{"name":"Planted Corp"}}`), from(bad))
	apply(swarmKindAlliance, "99000001", orgEntry(t, 99000001, `{"alliance":{"name":"Planted Alliance"}}`), from(bad))
	apply(swarmKindHistory, "10000002/34", `{"region_id":10000002,"type_id":34,"days":[`+
		`{"date":"2026-10-02","average":999,"highest":999,"lowest":999,"volume":1,"order_count":1},`+
		`{"date":"2026-10-01","average":999,"highest":999,"lowest":999,"volume":1,"order_count":1}]}`, from(bad))
	apply(swarmKindTypeName, "34", `{"type_id":34,"name":"Not Tritanium"}`, from(bad))
	apply(swarmKindStructure, "1035000000001", `{"structure_id":1035000000001,"name":"Planted Keepstar"}`, from(bad))
	// One this instance has since fetched from ESI itself.
	apply(swarmKindCorporation, "98000002", orgEntry(t, 98000002, `{"corp":{"name":"Planted Too"}}`), from(bad))
	if err := app.queries.SetCorporationRecord(ctx, db.SetCorporationRecordParams{
		CorporationID: 98000002, Payload: `{"corp":{"name":"Fetched Here"}}`, State: orgStateReady, FetchedAt: timeSet(now.Add(time.Hour)),
	}); err != nil {
		t.Fatalf("refetch corporation: %v", err)
	}
	apply(swarmKindCorporation, "98000003", orgEntry(t, 98000003, `{"corp":{"name":"Honest Corp"}}`), from(good))

	if got := app.swarmRollBack(ctx); got != 0 {
		t.Fatalf("rolled back %d rows with nobody excluded", got)
	}
	if err := app.queries.SetSwarmPeerExcluded(ctx, db.SetSwarmPeerExcludedParams{InstanceID: bad.id, Excluded: true, ExcludeNote: "manual test"}); err != nil {
		t.Fatalf("exclude: %v", err)
	}
	if got := app.swarmRollBack(ctx); got != 5 {
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
	if n, err := app.queries.CountSwarmAppliedBySigner(ctx, bad.id); err != nil || n != 0 {
		t.Fatalf("%d provenance rows left under the excluded signer (err %v)", n, err)
	}
	if n, err := app.queries.CountSwarmAppliedBySigner(ctx, good.id); err != nil || n != 1 {
		t.Fatalf("the honest signer has %d provenance rows, want 1 (err %v)", n, err)
	}
	if got := app.swarmCounterMap(ctx)[swarmCounterRolledBack]; got != 5 {
		t.Fatalf("rolled-back counter = %d, want 5", got)
	}
	if got := app.swarmRollBack(ctx); got != 0 {
		t.Fatalf("a second pass rolled back %d more rows", got)
	}
}

// TestSwarmOnePeerRoundPerCycle: across both of a cycle's swarm
// passes, a peer is asked for its member list once and for the
// wanted keys once. The later pass only settles what is staged.
func TestSwarmOnePeerRoundPerCycle(t *testing.T) {
	ctx := context.Background()
	app := swarmTestApp(t)
	peer, err := mintSwarmIdentity()
	if err != nil {
		t.Fatalf("mint peer: %v", err)
	}
	peer.id = "peer-counted"
	now := time.Now().UTC()
	hits := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits[r.URL.Path]++
		switch r.URL.Path {
		case "/swarm/pull":
			writeSwarmJSON(w, map[string]any{"entries": []swarmSignedEntry{}})
		default:
			http.NotFound(w, r)
		}
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
	// Something to ask for: one pending corporation record.
	if err := app.queries.UpsertCorporationWant(ctx, db.UpsertCorporationWantParams{
		CorporationID: 98000001, Priority: wantViewed, NotedAt: timeSet(now),
	}); err != nil {
		t.Fatalf("queue a want: %v", err)
	}

	app.swarmFetchPhase(ctx, now)
	if _, limited := app.swarmCycle(ctx, &fetchBudget{left: 10}, nil, now); limited {
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
	interval := time.Duration(app.cfg.swarm.gossipHours) * time.Hour
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	app.swarmGossipDone(ctx, now)

	app.swarmMu.Lock()
	next := app.swarmNextGossip
	app.swarmMu.Unlock()
	if next.Before(now.Add(interval*3/4)) || next.After(now.Add(interval*5/4)) {
		t.Fatalf("next round at %v, outside %v ±25%% of %v", next, interval, now)
	}
	// Anywhere inside the window the answer is settled by the one
	// draw: before it never, from it always, however often asked.
	for i := 0; i < 500; i++ {
		if app.swarmGossipDue(ctx, next.Add(-time.Second)) {
			t.Fatalf("check %d: due a second before the drawn time", i)
		}
		if !app.swarmGossipDue(ctx, next) {
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
		InstanceID: app.swarmID.id, PublicKey: app.swarmID.publicString(), SealedSeed: "sw:v1:test", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("store identity: %v", err)
	}
	now := time.Now().UTC()
	app.swarmGossipDone(ctx, now)

	// A restart forgets everything held in memory.
	app.swarmMu.Lock()
	app.swarmNextGossip, app.swarmNextLoaded = time.Time{}, false
	app.swarmMu.Unlock()
	if app.swarmGossipDue(ctx, now.Add(time.Hour)) {
		t.Fatalf("a restart made gossip due an hour after the last round")
	}
	if !app.swarmGossipDue(ctx, now.Add(200*time.Hour)) {
		t.Fatalf("gossip never comes due after a restart")
	}

	// A new identity (the secret changed) starts from scratch.
	if err := app.queries.PutSwarmIdentity(ctx, db.PutSwarmIdentityParams{
		InstanceID: "fresh-identity", PublicKey: app.swarmID.publicString(), SealedSeed: "sw:v1:test", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("store fresh identity: %v", err)
	}
	app.swarmMu.Lock()
	app.swarmNextGossip, app.swarmNextLoaded = time.Time{}, false
	app.swarmMu.Unlock()
	if !app.swarmGossipDue(ctx, now.Add(time.Minute)) {
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
	origin := swarmOrigin{source: peer.id, signer: peer.id, fetchedAt: fetched}

	if err := app.applySwarmEntry(ctx, swarmKindCorporation, "98000001", orgEntry(t, 98000001, `{"corp":{"name":"Test Corp"}}`), origin, now); err != nil {
		t.Fatalf("apply corporation: %v", err)
	}
	rec, err := app.queries.GetCorporationRecord(ctx, 98000001)
	if err != nil || !rec.FetchedAt.Valid || !rec.FetchedAt.Time.Equal(fetched) {
		t.Fatalf("corporation fetched_at = %v (err %v), want the signed %v", rec.FetchedAt, err, fetched)
	}
	if err := app.applySwarmEntry(ctx, swarmKindStructure, "1035000000001", `{"structure_id":1035000000001,"name":"Test Keepstar"}`, origin, now); err != nil {
		t.Fatalf("apply structure: %v", err)
	}
	if row, err := app.queries.GetStructureName(ctx, 1035000000001); err != nil || !row.ResolvedAt.Valid || !row.ResolvedAt.Time.Equal(fetched) {
		t.Fatalf("structure resolved_at = %v (err %v), want the signed %v", row.ResolvedAt, err, fetched)
	}
	history := `{"region_id":10000002,"type_id":34,"days":[{"date":"2026-10-01","average":5,"highest":6,"lowest":4,"volume":10,"order_count":2}]}`
	if err := app.applySwarmEntry(ctx, swarmKindHistory, "10000002/34", history, origin, now); err != nil {
		t.Fatalf("apply history: %v", err)
	}
	state, err := app.queries.GetMarketFetchState(ctx, marketFetchKind("history", marketKey{RegionID: 10000002, TypeID: 34}))
	if err != nil || !state.AttemptedAt.Equal(fetched) || state.Detail != swarmFetchDetail {
		t.Fatalf("history fetch state = %+v (err %v), want the signed time %v marked as the swarm's", state, err, fetched)
	}
	prov, err := app.queries.GetSwarmApplied(ctx, db.GetSwarmAppliedParams{Kind: swarmKindCorporation, Key: "98000001"})
	if err != nil || !prov.FetchedAt.Equal(fetched) || prov.AppliedAt.Before(now.Add(-time.Minute)) {
		t.Fatalf("provenance = %+v (err %v), want fetched %v and applied about now", prov, err, fetched)
	}

	// A signer's clock running ahead cannot date a row in the future.
	ahead := swarmOrigin{source: peer.id, signer: peer.id, fetchedAt: now.Add(48 * time.Hour)}
	if err := app.applySwarmEntry(ctx, swarmKindAlliance, "99000001", orgEntry(t, 99000001, `{"alliance":{"name":"Test Alliance"}}`), ahead, now); err != nil {
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
	origin := swarmOrigin{source: peer.id, signer: peer.id, fetchedAt: now.Add(-time.Hour).Truncate(time.Second)}
	entries := []struct{ kind, key, payload string }{
		{swarmKindCorporation, "98000001", orgEntry(t, 98000001, `{"corp":{"name":"Test Corp"}}`)},
		{swarmKindAlliance, "99000001", orgEntry(t, 99000001, `{"alliance":{"name":"Test Alliance"}}`)},
		{swarmKindTypeName, "34", `{"type_id":34,"name":"Tritanium"}`},
		{swarmKindStructure, "1035000000001", `{"structure_id":1035000000001,"name":"Test Keepstar"}`},
		{swarmKindHistory, "10000002/34", `{"region_id":10000002,"type_id":34,"days":[{"date":"2026-10-01","average":5,"highest":6,"lowest":4,"volume":10,"order_count":2}]}`},
	}
	var asked []swarmPullKey
	for _, e := range entries {
		if err := app.applySwarmEntry(ctx, e.kind, e.key, e.payload, origin, now); err != nil {
			t.Fatalf("apply %s %s: %v", e.kind, e.key, err)
		}
		if _, _, ok := app.serveSwarmKey(ctx, e.kind, e.key); ok {
			t.Fatalf("%s %s was supplied by a peer and is being served onward", e.kind, e.key)
		}
		asked = append(asked, swarmPullKey{Kind: e.kind, Key: e.key})
	}
	// The same over the wire: a pull for all five comes back empty.
	code, body := swarmServe(t, app, http.MethodPost, "/swarm/pull", string(mustJSON(t, swarmPullRequest{Keys: asked})), swarmTestSecret)
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
	if _, at, ok := app.serveSwarmKey(ctx, swarmKindCorporation, "98000001"); !ok || !at.Equal(own) {
		t.Fatalf("own corporation fetch: served=%v at %v, want served at %v", ok, at, own)
	}
	app.recordMarketFetch(ctx, marketFetchKind("history", marketKey{RegionID: 10000002, TypeID: 34}), fetchStateOK, "")
	if _, _, ok := app.serveSwarmKey(ctx, swarmKindHistory, "10000002/34"); !ok {
		t.Fatalf("history this instance fetched itself is not served")
	}
	// The alliance is still the peer's copy.
	if _, _, ok := app.serveSwarmKey(ctx, swarmKindAlliance, "99000001"); ok {
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
	name := func(id string, at time.Time) swarmSignedEntry {
		return signedBy(t, peer, swarmKindTypeName, id, `{"type_id":`+id+`,"name":"Type `+id+`"}`, at)
	}
	marks := func() int64 {
		row, err := app.queries.GetSwarmPeer(ctx, peer.id)
		if err != nil {
			t.Fatalf("read peer: %v", err)
		}
		return row.BadChecks
	}

	if err := app.stageSwarmEntry(ctx, name("34", now.Add(365*24*time.Hour)), peer.id, now); err == nil {
		t.Fatalf("an entry fetched a year from now was staged")
	}
	if got := marks(); got != 1 {
		t.Fatalf("a far-future fetch time cost the signer %d marks, want 1", got)
	}
	if err := app.stageSwarmEntry(ctx, name("35", now.Add(2*time.Minute)), peer.id, now); err != nil {
		t.Fatalf("an entry two minutes ahead (clock drift) was refused: %v", err)
	}
	row, err := app.queries.GetStagingEntry(ctx, db.GetStagingEntryParams{Kind: swarmKindTypeName, Key: "35"})
	if err != nil || row.FetchedAt.After(now) {
		t.Fatalf("drifted entry staged with fetched_at %v (err %v), want it held to %v", row.FetchedAt, err, now)
	}
	if err := app.stageSwarmEntry(ctx, name("36", now.Add(-200*24*time.Hour)), peer.id, now); err == nil {
		t.Fatalf("an entry fetched 200 days ago was staged")
	}
	if err := app.stageSwarmEntry(ctx, name("37", now.Add(-3*time.Hour)), peer.id, now); err != nil {
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
		entry := signedBy(t, peer, swarmKindHistory, "10000002/34", payload, fetched)
		if err := app.stageSwarmEntry(ctx, entry, peer.id, at); err == nil {
			t.Fatalf("%s: the entry staged", name)
		}
	}
	if staged := stagedKeys(t, app); len(staged) != 0 {
		t.Fatalf("staging holds %v, want nothing", staged)
	}
	row, err := app.queries.GetSwarmPeer(ctx, peer.id)
	if err != nil || row.BadChecks != 5 {
		t.Fatalf("signer has %d marks (err %v), want one per refused entry", row.BadChecks, err)
	}
	// The fetch day itself is fine.
	good := signedBy(t, peer, swarmKindHistory, "10000002/34", historyDays("2026-10-05", "2026-10-04"), fetched)
	if err := app.stageSwarmEntry(ctx, good, peer.id, at); err != nil {
		t.Fatalf("an entry ending on its fetch day was refused: %v", err)
	}
}

// TestSwarmHistoryCheckCoversWholeSpan: the ESI check compares
// every day in the span the entry and ESI share. A made-up day,
// a dropped day or a changed figure inside it fails; days
// outside it cannot be checked and are left out of what
// applies; no shared span decides nothing.
func TestSwarmHistoryCheckCoversWholeSpan(t *testing.T) {
	ctx := context.Background()
	check := func(t *testing.T, truth, payload string) (swarmCheckVerdict, string) {
		t.Helper()
		p := newSwarmPair(t, swarmOpen)
		p.stubB.history = truth
		verdict, confirmed, stop := p.b.swarmVerifyAgainstESI(ctx, &fetchBudget{left: 5}, nil, swarmKindHistory, "10000002/34", payload)
		if stop {
			t.Fatalf("canned ESI reported the error limit")
		}
		return verdict, confirmed
	}
	week := []string{"2026-10-01", "2026-10-02", "2026-10-03", "2026-10-04", "2026-10-05"}

	t.Run("honest and whole", func(t *testing.T) {
		verdict, confirmed := check(t, esiHistory(week...), historyDays("2026-10-04", "2026-10-03", "2026-10-02"))
		if verdict != swarmCheckPass || strings.Count(confirmed, `"date"`) != 3 {
			t.Fatalf("verdict %d confirmed %s, want a pass with its three days", verdict, confirmed)
		}
	})
	t.Run("a made-up day inside the span", func(t *testing.T) {
		// ESI has no trades on the 3rd; the entry invents one.
		truth := esiHistory("2026-10-01", "2026-10-02", "2026-10-04", "2026-10-05")
		if verdict, _ := check(t, truth, historyDays(week...)); verdict != swarmCheckFail {
			t.Fatalf("verdict %d, want a fail", verdict)
		}
	})
	t.Run("a dropped day inside the span", func(t *testing.T) {
		if verdict, _ := check(t, esiHistory(week...), historyDays("2026-10-01", "2026-10-02", "2026-10-04", "2026-10-05")); verdict != swarmCheckFail {
			t.Fatalf("verdict %d, want a fail", verdict)
		}
	})
	t.Run("a changed figure", func(t *testing.T) {
		lie := strings.Replace(historyDays(week...), `"average":5.5`, `"average":999`, 1)
		if verdict, _ := check(t, esiHistory(week...), lie); verdict != swarmCheckFail {
			t.Fatalf("verdict %d, want a fail", verdict)
		}
	})
	t.Run("days ESI cannot confirm are left out", func(t *testing.T) {
		// One day older than ESI still serves, one newer than it serves yet.
		entry := historyDays(append([]string{"2026-09-28"}, append(week, "2026-10-06")...)...)
		verdict, confirmed := check(t, esiHistory(week...), entry)
		if verdict != swarmCheckPass {
			t.Fatalf("verdict %d, want a pass", verdict)
		}
		if strings.Count(confirmed, `"date"`) != 5 || strings.Contains(confirmed, "2026-09-28") || strings.Contains(confirmed, "2026-10-06") {
			t.Fatalf("confirmed %s, want only the five days ESI has", confirmed)
		}
	})
	t.Run("no shared span", func(t *testing.T) {
		if verdict, _ := check(t, esiHistory(week...), historyDays("2026-09-01")); verdict != swarmCheckDefer {
			t.Fatalf("verdict %d, want a deferral", verdict)
		}
	})
}

// TestSwarmHistoryAppliesOnlyConfirmedDays: end to end, a
// checked entry writes the days ESI confirmed and no others.
func TestSwarmHistoryAppliesOnlyConfirmedDays(t *testing.T) {
	p := newSwarmPair(t, swarmOpen)
	ctx := context.Background()
	p.pinA(t)
	// Recent days, so the entry is young enough to be checked.
	now := time.Now().UTC()
	day := func(ago int) string { return now.AddDate(0, 0, -ago).Format(time.DateOnly) }
	p.stubB.history = esiHistory(day(3), day(2), day(1))
	fetched := now.Add(-time.Minute)
	entry := signedBy(t, p.a.swarmID, swarmKindHistory, "10000002/34",
		historyDays(day(15), day(3), day(2), day(1), fetched.Format(time.DateOnly)), fetched)
	asked := []swarmPullKey{{Kind: swarmKindHistory, Key: "10000002/34"}}
	if got := p.b.swarmStagePulled(ctx, "instance-a", p.urlA, asked, []swarmSignedEntry{entry}, now); got != 1 {
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
	origin := swarmOrigin{source: peer.id, signer: peer.id, fetchedAt: fetched}
	payload := historyDays("2024-01-01", "2026-10-04", "2026-10-05", "2026-10-09")
	if err := app.applySwarmEntry(ctx, swarmKindHistory, "10000002/34", payload, origin, fetched.Add(time.Hour)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	days, err := app.queries.ListMarketHistory(ctx, db.ListMarketHistoryParams{RegionID: 10000002, TypeID: 34, RowLimit: 20})
	if err != nil || len(days) != 2 {
		t.Fatalf("history rows %+v (err %v), want the 4th and 5th only", days, err)
	}
	prov, err := app.queries.GetSwarmApplied(ctx, db.GetSwarmAppliedParams{Kind: swarmKindHistory, Key: "10000002/34"})
	if err != nil || prov.FirstDate != "2026-10-04" || prov.LastDate != "2026-10-05" {
		t.Fatalf("provenance range %q..%q (err %v), want the days written", prov.FirstDate, prov.LastDate, err)
	}
}
