package app

import (
	"context"
	"testing"
	"time"

	"evesynapse/internal/app/swarm"
	db "evesynapse/internal/db/sqlc"
)

// TestSwarmLegacyIdentityTakesItsDerivedID: an identity stored under a
// random instance ID (minted before IDs were derived from the key)
// loads with the same key under the ID the key certifies, and is
// stored that way.
func TestSwarmLegacyIdentityTakesItsDerivedID(t *testing.T) {
	app, _, _ := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	kp, err := swarm.MintSwarmIdentity()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	sealed, err := swarm.SealSwarmSeed(kp.Private.Seed(), swarmTestSecret)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if err := app.queries.PutSwarmIdentity(ctx, db.PutSwarmIdentityParams{
		InstanceID: "legacy-random-id", PublicKey: kp.PublicString(), SealedSeed: sealed, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("store legacy identity: %v", err)
	}
	id, err := swarm.LoadSwarmIdentity(ctx, app.queries, swarmTestSecret)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if id.ID != swarm.SwarmIDForKey(kp.Public) || id.PublicString() != kp.PublicString() {
		t.Fatalf("loaded %s with key %s, want the derived ID under the same key", id.ID, id.PublicString())
	}
	row, err := app.queries.GetSwarmIdentity(ctx)
	if err != nil || row.InstanceID != id.ID {
		t.Fatalf("stored identity %+v err %v, want the derived ID", row, err)
	}
}

// TestSwarmLegacyPeerRowMergesIntoKeyID: an instance first filed
// under an ID minted before IDs were derived from keys, then met
// under the ID its key certifies, is listed once, under the new ID,
// with the old row's approval and reputation carried over.
func TestSwarmLegacyPeerRowMergesIntoKeyID(t *testing.T) {
	app := swarmTestApp(t)
	ctx := context.Background()
	sender := swarmTestPeer(t, app, "sender-legacy")
	kp, err := swarm.MintSwarmIdentity()
	if err != nil {
		t.Fatalf("mint member: %v", err)
	}
	firstSeen := time.Now().Add(-30 * 24 * time.Hour).UTC()
	if _, err := app.pool.Exec(ctx, `INSERT INTO swarm_peers (instance_id, base_url, public_key, source, approved, good_checks, bad_checks, first_seen, last_seen)
		VALUES ('legacy-neuron', 'https://neuron.example', $1, 'gossip', true, 40, 2, $2, $2)`, kp.PublicString(), firstSeen); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	issued := time.Now().Unix()
	member := swarm.SwarmMember{InstanceID: kp.ID, BaseURL: "https://neuron.example", PublicKey: kp.PublicString(), IssuedAt: issued}
	member.URLSig = swarm.SignSwarmMember(kp, member.BaseURL, issued)
	if err := swarm.StateFor(app).SwarmLearnMembers(ctx, []swarm.SwarmMember{member}, sender.ID, sender.PublicString(), "", "gossip", time.Now()); err != nil {
		t.Fatalf("learn members: %v", err)
	}
	var rows int
	if err := app.pool.QueryRow(ctx, `SELECT COUNT(*) FROM swarm_peers WHERE public_key = $1`, kp.PublicString()).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("rows for one key = %d, want 1", rows)
	}
	peer, err := app.queries.GetSwarmPeer(ctx, kp.ID)
	if err != nil {
		t.Fatalf("key ID row missing: %v", err)
	}
	if !peer.Approved || peer.GoodChecks != 40 || peer.BadChecks != 2 || !peer.FirstSeen.Equal(firstSeen.Truncate(time.Microsecond)) {
		t.Fatalf("standing not carried: approved %v, checks %d/%d, first seen %v", peer.Approved, peer.GoodChecks, peer.BadChecks, peer.FirstSeen)
	}
}
