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

// TestSwarmSeedPeerShowsSeed proves a configured seed lists as
// a directory, not a source: State Seed even when approved, so
// the operator never mistakes it for an instance with data.
func TestSwarmSeedPeerShowsSeed(t *testing.T) {
	app, _ := swarmPageApp(t)
	app.cfg.swarm = swarmConfig{mode: swarmOpen, secret: swarmTestSecret, seeds: []string{"https://seed.example"}, gossipHours: defaultSwarmGossipHours}
	ctx := context.Background()
	now := time.Now().UTC()
	for _, peer := range []struct {
		id, url, source string
	}{
		{"seed-1", "https://seed.example", "seed"},
		{"seed", "https://directory.example", "gossip"},
		{"peer-data-1", "https://node.example", "gossip"},
	} {
		if _, err := app.queries.UpsertSwarmPeer(ctx, db.UpsertSwarmPeerParams{
			InstanceID: peer.id, BaseUrl: peer.url,
			PublicKey: strings.Repeat("A", 44), Source: peer.source, FirstSeen: now,
		}); err != nil {
			t.Fatalf("upsert %s: %v", peer.id, err)
		}
		if err := app.queries.SetSwarmPeerApproved(ctx, db.SetSwarmPeerApprovedParams{InstanceID: peer.id, Approved: true}); err != nil {
			t.Fatalf("approve %s: %v", peer.id, err)
		}
	}
	rows := app.loadSwarmPeers(ctx)
	byID := map[string]swarmPeerRow{}
	for _, row := range rows {
		byID[row.ID] = row
	}
	if byID["seed-1"].State != "Seed" {
		t.Errorf("seed state %q, want Seed", byID["seed-1"].State)
	}
	if byID["peer-data-1"].State != "Approved" {
		t.Errorf("instance state %q, want Approved", byID["peer-data-1"].State)
	}
	// Role is what the peer functions as, seen from here. No
	// instance ID is special: a configured seed address is a
	// Seed until a pull from it has staged something, and both
	// from then on; an instance that merely calls itself
	// "seed" is a cache like any other.
	if byID["seed-1"].Role != "Seed" {
		t.Errorf("configured seed role %q, want Seed", byID["seed-1"].Role)
	}
	if byID["seed"].Role != "Cache" {
		t.Errorf("an instance named seed has role %q, want Cache", byID["seed"].Role)
	}
	if err := app.queries.TouchSwarmPeerServed(ctx, db.TouchSwarmPeerServedParams{InstanceID: "seed-1", LastServed: timeSet(now)}); err != nil {
		t.Fatalf("mark served: %v", err)
	}
	for _, row := range app.loadSwarmPeers(ctx) {
		if row.ID == "seed-1" && row.Role != "Seed+Cache" {
			t.Errorf("serving seed role %q, want Seed+Cache", row.Role)
		}
	}
	if byID["peer-data-1"].Role != "Cache" {
		t.Errorf("instance role %q, want Cache", byID["peer-data-1"].Role)
	}
}

// TestSwarmAdminRoleColumn renders the Admin table over a
// directory-only seed, a serving seed and a plain cache: the
// heading and every role reach the page.
func TestSwarmAdminRoleColumn(t *testing.T) {
	app, cookie := swarmPageApp(t)
	app.cfg.swarm = swarmConfig{mode: swarmOpen, secret: swarmTestSecret, seeds: []string{"https://seed.example"}, gossipHours: defaultSwarmGossipHours}
	ctx := context.Background()
	now := time.Now().UTC()
	for _, peer := range []struct {
		id, url, source string
	}{
		{"seed", "https://directory.example", "gossip"},
		{"seed-1", "https://seed.example", "seed"},
		{"peer-data-1", "https://node.example", "gossip"},
	} {
		if _, err := app.queries.UpsertSwarmPeer(ctx, db.UpsertSwarmPeerParams{
			InstanceID: peer.id, BaseUrl: peer.url,
			PublicKey: strings.Repeat("A", 44), Source: peer.source, FirstSeen: now,
		}); err != nil {
			t.Fatalf("upsert %s: %v", peer.id, err)
		}
	}
	if err := app.queries.TouchSwarmPeerServed(ctx, db.TouchSwarmPeerServedParams{InstanceID: "seed-1", LastServed: timeSet(now)}); err != nil {
		t.Fatalf("mark served: %v", err)
	}
	code, body := getPage(t, app, cookie, "/admin/")
	if code != http.StatusOK {
		t.Fatalf("/admin/: status %d", code)
	}
	// html/template escapes + as &#43; in the cells; the row
	// test above pins the exact role values.
	mustContain(t, "/admin/", body, "Swarm Cache", "<th>Role</th>", "Seed&#43;Cache", ">Cache<")
}

func TestSwarmSyncCardOff(t *testing.T) {
	app, cookie := swarmPageApp(t)
	code, body := getPage(t, app, cookie, "/sync/")
	if code != http.StatusOK {
		t.Fatalf("/sync/: status %d", code)
	}
	mustContain(t, "/sync/", body, "Swarm Cache", "Off")
}

func TestSwarmAdminSectionOff(t *testing.T) {
	app, cookie := swarmPageApp(t)
	code, body := getPage(t, app, cookie, "/admin/")
	if code != http.StatusOK {
		t.Fatalf("/admin/: status %d", code)
	}
	mustContain(t, "/admin/", body, "Swarm Cache", "Mode Off", "SHARED_CACHE")
	if strings.Contains(body, "SHARED_CACHE_SECRET=") {
		t.Fatalf("/admin/: page leaks the secret setting name with a value")
	}
}

func TestSwarmAdminActions(t *testing.T) {
	app, cookie := swarmPageApp(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := app.queries.UpsertSwarmPeer(ctx, db.UpsertSwarmPeerParams{
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
	right := url.Values{"instance": {"peer-action-1"}, "confirm": {app.swarmDisplayPeer("peer-action-1", "http://127.0.0.1:9", "gossip")}}
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
	if _, err := app.queries.UpsertSwarmPeer(ctx, db.UpsertSwarmPeerParams{
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
	// The seed-sourced peer shows under its alias, not its
	// address: only hand-added and configured seeds show
	// plainly.
	alias := swarmAlias(swarmTestSecret, "peer-shown-12")
	if strings.Contains(alias, "peer-shown") {
		t.Fatalf("alias %q leaks the instance id", alias)
	}
	code, body := getPage(t, app, cookie, "/sync/")
	if code != http.StatusOK {
		t.Fatalf("/sync/: status %d", code)
	}
	mustContain(t, "/sync/", body, "Swarm Cache", "Open", alias, "ESI calls saved")
	if strings.Contains(body, "127.0.0.1:9") {
		t.Fatalf("/sync/: page leaks the peer address")
	}
	code, body = getPage(t, app, cookie, "/admin/")
	if code != http.StatusOK {
		t.Fatalf("/admin/: status %d", code)
	}
	mustContain(t, "/admin/", body, "Mode Open", alias, "Approve", "Exclude")
	if strings.Contains(body, "127.0.0.1:9") {
		t.Fatalf("/admin/: page leaks the peer address")
	}
}

// TestSwarmAdminShowsRefusedKey: a peer that had another key
// refused says so on its Admin row, so the operator knows which
// instance to look at.
func TestSwarmAdminShowsRefusedKey(t *testing.T) {
	app, cookie := swarmPageApp(t)
	app.cfg.swarm = swarmConfig{mode: swarmOpen, secret: swarmTestSecret, gossipHours: defaultSwarmGossipHours}
	ctx := context.Background()
	now := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	if _, err := app.queries.UpsertSwarmPeer(ctx, db.UpsertSwarmPeerParams{
		InstanceID: "peer-flagged-1", BaseUrl: "https://node.example",
		PublicKey: strings.Repeat("A", 44), Source: "manual", FirstSeen: now,
	}); err != nil {
		t.Fatalf("upsert peer: %v", err)
	}
	_, body := getPage(t, app, cookie, "/admin/")
	if strings.Contains(body, "another key was refused") {
		t.Fatalf("/admin/ shows a refused key before one was refused")
	}
	if err := app.queries.FlagSwarmPeerKeyConflict(ctx, db.FlagSwarmPeerKeyConflictParams{
		InstanceID: "peer-flagged-1", KeyConflictAt: timeSet(now),
	}); err != nil {
		t.Fatalf("flag peer: %v", err)
	}
	_, body = getPage(t, app, cookie, "/admin/")
	mustContain(t, "/admin/", body, "another key was refused on 2026-10-13")
}
