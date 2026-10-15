package app

import "testing"

// TestOrgPayloadShapes: a stored record reads in the app's shape and as
// the bare ESI object a swarm peer can send; neither shape naming the
// organization is unreadable.
func TestOrgPayloadShapes(t *testing.T) {
	if p, ok := decodeCorpPayload(`{"corp":{"name":"Late Corp","ticker":"LATE"},"alliance":{}}`); !ok || p.Corp.Name != "Late Corp" {
		t.Fatalf("app shape: %+v %v", p, ok)
	}
	if p, ok := decodeCorpPayload(`{"name":"Effervescent Blue","ticker":"-E B-","member_count":10}`); !ok || p.Corp.Name != "Effervescent Blue" || p.Corp.Ticker != "-E B-" {
		t.Fatalf("bare ESI shape: %+v %v", p, ok)
	}
	if _, ok := decodeCorpPayload(`{"corp":{}}`); ok {
		t.Fatal("a record naming no corporation read as one")
	}
	if p, ok := decodeAlliancePayload(`{"name":"Goonswarm Federation","ticker":"CONDI"}`); !ok || p.Alliance.Name != "Goonswarm Federation" {
		t.Fatalf("bare alliance: %+v %v", p, ok)
	}
}
