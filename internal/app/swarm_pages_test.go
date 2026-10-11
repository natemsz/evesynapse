package app

// Page tests for the swarm's Sync card and Admin section: the
// pages render with sharing off and on, and the
// approve/exclude/admit actions move peers.

import (
	"context"
	"net/http"
	"net/http/httptest"
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
	// Removing asks twice: the row, and the short id typed out.
	// A wrong confirmation removes nothing.
	wrong := url.Values{"instance": {"peer-action-1"}, "confirm": {"nope"}}
	if code, _ := postForm(t, app, cookie, "/admin/swarm/remove", wrong); code != http.StatusSeeOther {
		t.Fatalf("remove wrong confirm: status %d, want 303", code)
	}
	if _, err := app.queries.GetSwarmPeer(ctx, "peer-action-1"); err != nil {
		t.Fatalf("wrong confirm removed the peer: %v", err)
	}
	right := url.Values{"instance": {"peer-action-1"}, "confirm": {"peer-act"}}
	if code, _ := postForm(t, app, cookie, "/admin/swarm/remove", right); code != http.StatusSeeOther {
		t.Fatalf("remove: status %d, want 303", code)
	}
	if _, err := app.queries.GetSwarmPeer(ctx, "peer-action-1"); err == nil {
		t.Fatalf("remove: peer still known")
	}
	// Unknown instances change nothing, loudly.
	bad := url.Values{"instance": {"peer-nope"}}
	if code, _ := postForm(t, app, cookie, "/admin/swarm/exclude", bad); code != http.StatusSeeOther {
		t.Fatalf("exclude unknown: status %d, want 303", code)
	}
}

func TestSwarmManualAdd(t *testing.T) {
	app, cookie := swarmPageApp(t)
	app.cfg.swarm = swarmConfig{mode: swarmOpen, secret: swarmTestSecret, gossipHours: defaultSwarmGossipHours}
	local, err := mintSwarmIdentity()
	if err != nil {
		t.Fatalf("mint local: %v", err)
	}
	local.id = "local-manual"
	app.swarmID = local
	app.swarmCli = newSwarmClient(swarmTestSecret)

	transport := &countingTransport{}
	peer, _, _ := buildCorpTestApp(t, transport)
	peer.cfg.swarm = swarmConfig{mode: swarmOpen, secret: swarmTestSecret, gossipHours: defaultSwarmGossipHours}
	remote, err := mintSwarmIdentity()
	if err != nil {
		t.Fatalf("mint peer: %v", err)
	}
	remote.id = "peer-manual"
	peer.swarmID = remote
	peer.swarmCli = newSwarmClient(swarmTestSecret)
	srv := httptest.NewServer(peer.Handler())
	t.Cleanup(srv.Close)

	// Cleartext off-machine is refused before anything dials.
	if code, _ := postForm(t, app, cookie, "/admin/swarm/add", url.Values{"url": {"http://example.com/"}}); code != http.StatusSeeOther {
		t.Fatalf("add cleartext: status %d, want 303", code)
	}
	if rows, err := app.queries.ListSwarmPeers(context.Background()); err != nil || len(rows) != 0 {
		t.Fatalf("cleartext add learned %d peers, want 0", len(rows))
	}
	// A answering address is learned under trust on first use,
	// approved outright in open mode.
	if code, _ := postForm(t, app, cookie, "/admin/swarm/add", url.Values{"url": {srv.URL}}); code != http.StatusSeeOther {
		t.Fatalf("add: status %d, want 303", code)
	}
	got, err := app.queries.GetSwarmPeer(context.Background(), "peer-manual")
	if err != nil {
		t.Fatalf("manual add did not learn the peer: %v", err)
	}
	if got.Source != "manual" || !got.Approved || got.Excluded {
		t.Fatalf("learned peer: %+v", got)
	}
	if got.PublicKey != remote.publicString() {
		t.Fatalf("learned key is not the peer's")
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
