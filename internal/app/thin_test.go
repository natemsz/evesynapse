package app

// Thin-client tests: the gate (sharing required), the dark
// machine surface (health plus swarm only), and a thin worker
// cycle that runs the public passes with no characters.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestThinConfigGate(t *testing.T) {
	var full Config
	if err := full.checkThinConfig(); err != nil {
		t.Fatalf("full client gated: %v", err)
	}
	off := Config{thin: true, swarm: swarmConfig{mode: swarmOff}}
	if err := off.checkThinConfig(); err == nil {
		t.Fatal("thin client without sharing allowed to start")
	}
	open := Config{thin: true, swarm: swarmConfig{mode: swarmOpen, secret: strings.Repeat("s", 32)}}
	if err := open.checkThinConfig(); err != nil {
		t.Fatalf("sharing thin client gated: %v", err)
	}
}

func thinServe(t *testing.T, h http.Handler, method, path, secret string) int {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestThinHandlerDark(t *testing.T) {
	transport := &countingTransport{}
	app, _, _ := buildCorpTestApp(t, transport)
	app.cfg.thin = true
	h := app.Handler()

	for _, path := range []string{"/", "/home", "/admin/", "/auth/eve", "/sync", "/static/app.js"} {
		if code := thinServe(t, h, http.MethodGet, path, ""); code != http.StatusNotFound {
			t.Fatalf("GET %s: status %d, want 404", path, code)
		}
	}
	if code := thinServe(t, h, http.MethodGet, "/healthz", ""); code != http.StatusOK {
		t.Fatalf("GET /healthz: status %d, want 200", code)
	}
	// Sharing off: the swarm API stays dark too.
	if code := thinServe(t, h, http.MethodGet, "/swarm/members", swarmTestSecret); code != http.StatusNotFound {
		t.Fatalf("GET /swarm/members while off: status %d, want 404", code)
	}
}

func TestThinHandlerServesSwarm(t *testing.T) {
	transport := &countingTransport{}
	app, _, _ := buildCorpTestApp(t, transport)
	app.cfg.thin = true
	app.cfg.swarm = swarmConfig{mode: swarmOpen, secret: swarmTestSecret, gossipHours: defaultSwarmGossipHours}
	id, err := mintSwarmIdentity()
	if err != nil {
		t.Fatalf("mint identity: %v", err)
	}
	id.id = "test-local"
	app.swarmID = id
	app.swarmCli = newSwarmClient(swarmTestSecret)
	h := app.Handler()

	if code := thinServe(t, h, http.MethodGet, "/", ""); code != http.StatusNotFound {
		t.Fatalf("GET /: status %d, want 404", code)
	}
	if code := thinServe(t, h, http.MethodGet, "/swarm/members", ""); code != http.StatusForbidden {
		t.Fatalf("GET /swarm/members without secret: status %d, want 403", code)
	}
	if code := thinServe(t, h, http.MethodGet, "/swarm/members", swarmTestSecret); code != http.StatusOK {
		t.Fatalf("GET /swarm/members with secret: status %d, want 200", code)
	}
}

func TestThinCycleRuns(t *testing.T) {
	transport := &countingTransport{}
	app, _, _ := buildCorpTestApp(t, transport)
	app.cfg.thin = true
	app.cfg.swarm = swarmConfig{mode: swarmOpen, secret: swarmTestSecret, gossipHours: defaultSwarmGossipHours}
	id, err := mintSwarmIdentity()
	if err != nil {
		t.Fatalf("mint identity: %v", err)
	}
	id.id = "test-local"
	app.swarmID = id
	app.swarmCli = newSwarmClient(swarmTestSecret)

	app.refreshThinCycle(context.Background())
	if got := app.snapshotWorkerStatus().Summary; got == "" {
		t.Fatal("thin cycle recorded no summary")
	}
}
