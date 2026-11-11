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
