package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
)

// TestSwarmPublicIP pins which addresses count as public.
func TestSwarmPublicIP(t *testing.T) {
	for addr, want := range map[string]bool{
		"93.184.216.34":        true,
		"2606:2800:220:1::1":   true,
		"10.0.0.5":             false,
		"172.16.0.1":           false,
		"192.168.1.10":         false,
		"169.254.169.254":      false, // cloud metadata
		"100.64.0.1":           false, // carrier-grade NAT
		"127.0.0.1":            false,
		"0.0.0.0":              false,
		"224.0.0.1":            false,
		"::1":                  false,
		"fe80::1":              false,
		"fd00::1":              false,
		"::ffff:10.0.0.5":      false, // a private address in IPv6 clothing
		"::ffff:93.184.216.34": true,
	} {
		if got := swarmPublicIP(netip.MustParseAddr(addr)); got != want {
			t.Errorf("swarmPublicIP(%s) = %v, want %v", addr, got, want)
		}
	}
}

// TestSwarmDialControl: the connection check refuses private
// addresses outright and loopback unless the URL itself named
// loopback.
func TestSwarmDialControl(t *testing.T) {
	plain := context.Background()
	loopbackURL := context.WithValue(plain, swarmLoopbackKey{}, true)
	for _, tc := range []struct {
		name    string
		ctx     context.Context
		address string
		allowed bool
	}{
		{"public", plain, "93.184.216.34:443", true},
		{"private", plain, "10.0.0.5:443", false},
		{"metadata service", plain, "169.254.169.254:80", false},
		{"a name that resolved to loopback", plain, "127.0.0.1:443", false},
		{"a loopback URL", loopbackURL, "127.0.0.1:8080", true},
		{"a loopback URL does not unlock private space", loopbackURL, "192.168.1.10:8080", false},
		{"ipv6 loopback by name", plain, "[::1]:443", false},
	} {
		err := swarmDialControl(tc.ctx, "tcp", tc.address, nil)
		if (err == nil) != tc.allowed {
			t.Errorf("%s: dial %s err = %v, want allowed = %v", tc.name, tc.address, err, tc.allowed)
		}
	}
}

// TestSwarmClientStaysOnTheHostItAsked: a peer that answers with
// a redirect to another host gets no second request (and so no
// bearer) sent there; a redirect within the same host is fine.
func TestSwarmClientStaysOnTheHostItAsked(t *testing.T) {
	ctx := context.Background()
	var elsewhereHits int
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhereHits++
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(elsewhere.Close)
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/away":
			http.Redirect(w, r, elsewhere.URL+"/swarm/members", http.StatusFound)
		case "/moved":
			http.Redirect(w, r, "/here", http.StatusFound)
		case "/here":
			if r.Header.Get("Authorization") == "" {
				t.Errorf("the bearer was dropped on a same-host redirect")
			}
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(peer.Close)

	client := newSwarmClient(swarmTestSecret)
	var into struct{}
	err := client.authedGET(ctx, peer.URL+"/away", &into)
	if err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("redirect to another host: err = %v, want a refusal", err)
	}
	if elsewhereHits != 0 {
		t.Fatalf("the other host received %d requests, want none", elsewhereHits)
	}
	if err := client.authedGET(ctx, peer.URL+"/moved", &into); err != nil {
		t.Fatalf("redirect within the host: %v", err)
	}
}

// TestSwarmGossipCannotIntroduceALoopbackAddress: a member list
// naming an instance at a loopback address teaches nothing
// about it. Loopback is for addresses the operator dialed.
func TestSwarmGossipCannotIntroduceALoopbackAddress(t *testing.T) {
	p := newSwarmPair(t, swarmOpen)
	ctx := context.Background()
	now := time.Now().UTC()
	p.pinA(t)
	inward, err := mintSwarmIdentity()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	outward, err := mintSwarmIdentity()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	for id, row := range map[string]struct {
		url string
		key string
	}{
		"instance-inward":  {"http://127.0.0.1:5432", inward.publicString()},
		"instance-outward": {"https://outward.example", outward.publicString()},
	} {
		if _, err := p.a.queries.UpsertSwarmPeer(ctx, db.UpsertSwarmPeerParams{
			InstanceID: id, BaseUrl: row.url, PublicKey: row.key, Source: "gossip", FirstSeen: now,
		}); err != nil {
			t.Fatalf("list %s in A: %v", id, err)
		}
	}
	if err := p.b.swarmContactPeer(ctx, p.urlA, "instance-a", p.a.swarmID.publicString(), "gossip", now); err != nil {
		t.Fatalf("gossip with A: %v", err)
	}
	if _, err := p.b.queries.GetSwarmPeer(ctx, "instance-inward"); err == nil {
		t.Fatalf("gossip introduced a peer at a loopback address")
	}
	if _, err := p.b.queries.GetSwarmPeer(ctx, "instance-outward"); err != nil {
		t.Fatalf("an ordinary gossip-learned peer was dropped too: %v", err)
	}
	// A itself was dialed at a loopback address by this test, and stays.
	if peer, err := p.b.queries.GetSwarmPeer(ctx, "instance-a"); err != nil || peer.BaseUrl != p.urlA {
		t.Fatalf("the dialed peer lost its address: %+v err=%v", peer, err)
	}
}
