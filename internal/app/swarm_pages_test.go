package app

// Page tests for the swarm's Sync card and Admin section: the
// pages render with sharing off and on, and the
// approve/exclude/admit actions move peers.

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
)

func swarmPageApp(t *testing.T) (*Application, *http.Cookie) {
	t.Helper()
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	grantTestAdmin(app, fixtureCharA)
	return app, sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")
}

func TestSwarmSyncCardOff(t *testing.T) {
	app, cookie := swarmPageApp(t)
	code, body := getPage(t, app, cookie, "/sync/")
	if code != http.StatusOK {
		t.Fatalf("/sync/: status %d", code)
	}
	mustContain(t, "/sync/", body, "Shared cache", "Off")
}

func TestSwarmAdminSectionOff(t *testing.T) {
	app, cookie := swarmPageApp(t)
	code, body := getPage(t, app, cookie, "/admin/")
	if code != http.StatusOK {
		t.Fatalf("/admin/: status %d", code)
	}
	mustContain(t, "/admin/", body, "Shared cache", "Mode Off", "SHARED_CACHE")
	if strings.Contains(body, "SHARED_CACHE_SECRET=") {
		t.Fatalf("/admin/: page leaks the secret setting name with a value")
	}
}

func TestSwarmAdminActions(t *testing.T) {
	app, cookie := swarmPageApp(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := app.queries.UpsertSwarmPeer(ctx, db.UpsertSwarmPeerParams{
		InstanceID: "peer-action-1", BaseUrl: "http://127.0.0.1:9",
		PublicKey: strings.Repeat("A", 44), Source: "gossip", FirstSeen: now,
	}); err != nil {
		t.Fatalf("upsert peer: %v", err)
	}
	form := url.Values{"instance": {"peer-action-1"}}
	if code, _ := postForm(t, app, cookie, "/admin/swarm/approve", form); code != http.StatusSeeOther {
		t.Fatalf("approve: status %d, want 303", code)
	}
	peer, err := app.queries.GetSwarmPeer(ctx, "peer-action-1")
	if err != nil || !peer.Approved {
		t.Fatalf("approve: approved=%v err=%v", peer.Approved, err)
	}
	if code, _ := postForm(t, app, cookie, "/admin/swarm/exclude", form); code != http.StatusSeeOther {
		t.Fatalf("exclude: status %d, want 303", code)
	}
	peer, err = app.queries.GetSwarmPeer(ctx, "peer-action-1")
	if err != nil || !peer.Excluded || !strings.HasPrefix(peer.ExcludeNote, "manual:") {
		t.Fatalf("exclude: %+v err=%v", peer, err)
	}
	if code, _ := postForm(t, app, cookie, "/admin/swarm/admit", form); code != http.StatusSeeOther {
		t.Fatalf("admit: status %d, want 303", code)
	}
	peer, err = app.queries.GetSwarmPeer(ctx, "peer-action-1")
	if err != nil || peer.Excluded {
		t.Fatalf("admit: still excluded err=%v", err)
	}
	// Unknown instances change nothing, loudly.
	bad := url.Values{"instance": {"peer-nope"}}
	if code, _ := postForm(t, app, cookie, "/admin/swarm/exclude", bad); code != http.StatusSeeOther {
		t.Fatalf("exclude unknown: status %d, want 303", code)
	}
}

func TestSwarmSyncCardEnabled(t *testing.T) {
	app, cookie := swarmPageApp(t)
	app.cfg.swarm = swarmConfig{mode: swarmOpen, secret: swarmTestSecret, gossipHours: defaultSwarmGossipHours}
	id, err := mintSwarmIdentity()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	app.swarmID = id
	app.swarmCli = newSwarmClient(swarmTestSecret)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := app.queries.UpsertSwarmPeer(ctx, db.UpsertSwarmPeerParams{
		InstanceID: "peer-shown-12", BaseUrl: "http://127.0.0.1:9",
		PublicKey: strings.Repeat("A", 44), Source: "seed", FirstSeen: now,
	}); err != nil {
		t.Fatalf("upsert peer: %v", err)
	}
	if err := app.queries.SetSwarmPeerApproved(ctx, db.SetSwarmPeerApprovedParams{InstanceID: "peer-shown-12", Approved: true}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if err := app.queries.AddSwarmCounter(ctx, db.AddSwarmCounterParams{Name: swarmCounterAppliedHistory, Value: 7}); err != nil {
		t.Fatalf("counter: %v", err)
	}
	code, body := getPage(t, app, cookie, "/sync/")
	if code != http.StatusOK {
		t.Fatalf("/sync/: status %d", code)
	}
	mustContain(t, "/sync/", body, "Shared cache", "Open", "peer-sho", "ESI calls saved")
	code, body = getPage(t, app, cookie, "/admin/")
	if code != http.StatusOK {
		t.Fatalf("/admin/: status %d", code)
	}
	mustContain(t, "/admin/", body, "Mode Open", "peer-sho", "Approve", "Exclude")
}
