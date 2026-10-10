package app

// White-box tests for the swarm's HTTP API, staging gate, and
// trust ledger: bearer auth fails closed, bad signatures never
// stage, sustained lies auto-exclude, and gossip never forgives.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
)

const swarmTestSecret = "test-shared-secret-0123456789abcdef"

// swarmTestApp builds a fixture app with sharing enabled in
// open mode, holding a fresh identity.
func swarmTestApp(t *testing.T) *Application {
	t.Helper()
	transport := &countingTransport{}
	app, _, _ := buildCorpTestApp(t, transport)
	app.cfg.swarm = swarmConfig{mode: swarmOpen, secret: swarmTestSecret, gossipHours: defaultSwarmGossipHours}
	id, err := mintSwarmIdentity()
	if err != nil {
		t.Fatalf("mint identity: %v", err)
	}
	id.id = "test-local"
	app.swarmID = id
	app.swarmCli = newSwarmClient(swarmTestSecret)
	return app
}

// swarmTestPeer pins a peer identity the tests sign with.
func swarmTestPeer(t *testing.T, app *Application, id string) *swarmIdentity {
	t.Helper()
	kp, err := mintSwarmIdentity()
	if err != nil {
		t.Fatalf("mint peer: %v", err)
	}
	kp.id = id
	now := time.Now().UTC()
	if err := app.queries.UpsertSwarmPeer(context.Background(), db.UpsertSwarmPeerParams{
		InstanceID: id, BaseUrl: "http://127.0.0.1:9", PublicKey: kp.publicString(),
		Source: "manual", FirstSeen: now,
	}); err != nil {
		t.Fatalf("upsert peer: %v", err)
	}
	if err := app.queries.SetSwarmPeerApproved(context.Background(), db.SetSwarmPeerApprovedParams{InstanceID: id, Approved: true}); err != nil {
		t.Fatalf("approve peer: %v", err)
	}
	return kp
}

func swarmServe(t *testing.T, app *Application, method, path, body, secret string) (int, string) {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func TestSwarmAPIDarkWhenOff(t *testing.T) {
	transport := &countingTransport{}
	app, _, _ := buildCorpTestApp(t, transport)
	if code, _ := swarmServe(t, app, http.MethodGet, "/swarm/members", "", swarmTestSecret); code != http.StatusNotFound {
		t.Fatalf("members while off: status %d, want 404", code)
	}
	if code, _ := swarmServe(t, app, http.MethodPost, "/swarm/pull", `{"keys":[]}`, swarmTestSecret); code != http.StatusNotFound {
		t.Fatalf("pull while off: status %d, want 404", code)
	}
}

func TestSwarmAPIAuth(t *testing.T) {
	app := swarmTestApp(t)
	if code, _ := swarmServe(t, app, http.MethodGet, "/swarm/members", "", ""); code != http.StatusForbidden {
		t.Fatalf("members without bearer: status %d, want 403", code)
	}
	if code, _ := swarmServe(t, app, http.MethodGet, "/swarm/members", "", "wrong-secret-0123456789abcdef"); code != http.StatusForbidden {
		t.Fatalf("members with wrong bearer: status %d, want 403", code)
	}
	code, body := swarmServe(t, app, http.MethodGet, "/swarm/members", "", swarmTestSecret)
	if code != http.StatusOK {
		t.Fatalf("members with bearer: status %d, want 200", code)
	}
	var decoded struct {
		Entry swarmSignedEntry `json:"entry"`
	}
	dec := json.NewDecoder(strings.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&decoded); err != nil {
		t.Fatalf("members body: %v", err)
	}
	if decoded.Entry.Kind != swarmKindMembers || decoded.Entry.Signer != "test-local" {
		t.Fatalf("members entry: kind %q signer %q", decoded.Entry.Kind, decoded.Entry.Signer)
	}
	if !verifySwarmEntry(decoded.Entry, app.swarmID.publicString()) {
		t.Fatalf("members entry does not verify")
	}
}

func TestSwarmPullServesHeldNames(t *testing.T) {
	app := swarmTestApp(t)
	ctx := context.Background()
	if err := app.queries.UpsertTypeName(ctx, db.UpsertTypeNameParams{TypeID: 34, Name: "Tritanium"}); err != nil {
		t.Fatalf("seed name: %v", err)
	}
	code, body := swarmServe(t, app, http.MethodPost, "/swarm/pull",
		`{"keys":[{"kind":"type_name","key":"34"},{"kind":"nope","key":"1"},{"kind":"type_name","key":"35"}]}`,
		swarmTestSecret)
	if code != http.StatusOK {
		t.Fatalf("pull: status %d, want 200", code)
	}
	var decoded struct {
		Entries []swarmSignedEntry `json:"entries"`
	}
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("pull body: %v", err)
	}
	// Only the held name: the unknown kind and the missing key
	// are skipped, never reported.
	if len(decoded.Entries) != 1 {
		t.Fatalf("pull entries: %d, want 1", len(decoded.Entries))
	}
	entry := decoded.Entries[0]
	if !verifySwarmEntry(entry, app.swarmID.publicString()) {
		t.Fatalf("pulled entry does not verify")
	}
	if !strings.Contains(entry.Payload, "Tritanium") {
		t.Fatalf("pulled entry payload %q lacks the name", entry.Payload)
	}
	if code, _ := swarmServe(t, app, http.MethodPost, "/swarm/pull", `{"keys":[}`, swarmTestSecret); code != http.StatusBadRequest {
		t.Fatalf("malformed pull: status %d, want 400", code)
	}
}

func TestSwarmStagingGate(t *testing.T) {
	app := swarmTestApp(t)
	ctx := context.Background()
	peer := swarmTestPeer(t, app, "peer-a")
	now := time.Now().UTC()

	entry, err := signSwarmEntry(peer, "peer-a", swarmKindTypeName, "34", `{"type_id":34,"name":"Tritanium"}`, now)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := app.stageSwarmEntry(ctx, entry, "peer-a", now); err != nil {
		t.Fatalf("stage good entry: %v", err)
	}
	rows, err := app.queries.ListUncheckedStaging(ctx, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("staged rows: %d, err %v", len(rows), err)
	}

	// Unknown signer, served by peer-a: no pinned key, no
	// stage — and the serving peer is at fault, not the named
	// stranger (naming someone else must never frame them).
	other, err := mintSwarmIdentity()
	if err != nil {
		t.Fatalf("mint other: %v", err)
	}
	foreign, err := signSwarmEntry(other, "peer-evil", swarmKindTypeName, "35", `{"type_id":35,"name":"Pyroxeres"}`, now)
	if err != nil {
		t.Fatalf("sign foreign: %v", err)
	}
	if err := app.stageSwarmEntry(ctx, foreign, "peer-a", now); err == nil {
		t.Fatalf("staged an entry from an unknown signer")
	}

	// Wrong key: another instance's signature under peer-a's
	// name, served by peer-a — peer-a's ledger takes it.
	forged, err := signSwarmEntry(other, "peer-a", swarmKindTypeName, "36", `{"type_id":36,"name":"Mexallon"}`, now)
	if err != nil {
		t.Fatalf("sign forged: %v", err)
	}
	if err := app.stageSwarmEntry(ctx, forged, "peer-a", now); err == nil {
		t.Fatalf("staged a mis-signed entry")
	}
	got, err := app.queries.GetSwarmPeer(ctx, "peer-a")
	if err != nil {
		t.Fatalf("read peer: %v", err)
	}
	if got.BadChecks != 2 {
		t.Fatalf("peer-a bad checks: %d, want 2 (foreign + forged, both served by it)", got.BadChecks)
	}

	// Valid signature, lying content (payload names another
	// type): staged nowhere; the signer proved authorship, so
	// the signer takes this one.
	mismatched, err := signSwarmEntry(peer, "peer-a", swarmKindTypeName, "34", `{"type_id":99,"name":"Tritanium"}`, now)
	if err != nil {
		t.Fatalf("sign mismatched: %v", err)
	}
	if err := app.stageSwarmEntry(ctx, mismatched, "peer-a", now); err == nil {
		t.Fatalf("staged a key-mismatched entry")
	}
	got, err = app.queries.GetSwarmPeer(ctx, "peer-a")
	if err != nil || got.BadChecks != 3 {
		t.Fatalf("peer-a bad checks: %+v err %v, want 3", got, err)
	}
	rows, err = app.queries.ListUncheckedStaging(ctx, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("staged rows after rejects: %d, err %v (want the 1 good entry)", len(rows), err)
	}
}

func TestSwarmAutoExclude(t *testing.T) {
	app := swarmTestApp(t)
	ctx := context.Background()
	swarmTestPeer(t, app, "peer-liar")
	for i := 0; i < swarmTrustMinChecks; i++ {
		app.countSwarmCheck(ctx, "peer-liar", false)
	}
	got, err := app.queries.GetSwarmPeer(ctx, "peer-liar")
	if err != nil {
		t.Fatalf("read peer: %v", err)
	}
	if !got.Excluded {
		t.Fatalf("sustained liar not excluded after %d bad checks", swarmTrustMinChecks)
	}
	if !strings.Contains(got.ExcludeNote, "auto:") {
		t.Fatalf("exclude note %q lacks the auto marker", got.ExcludeNote)
	}
	// Excluded peers leave the pull set.
	pull, err := app.queries.ListSwarmPullPeers(ctx)
	if err != nil {
		t.Fatalf("pull peers: %v", err)
	}
	for _, p := range pull {
		if p.InstanceID == "peer-liar" {
			t.Fatalf("excluded peer still in the pull set")
		}
	}
}

func TestSwarmGoodRecordNeverExcludes(t *testing.T) {
	app := swarmTestApp(t)
	ctx := context.Background()
	swarmTestPeer(t, app, "peer-good")
	for i := 0; i < 3*swarmTrustMinChecks; i++ {
		app.countSwarmCheck(ctx, "peer-good", true)
	}
	got, err := app.queries.GetSwarmPeer(ctx, "peer-good")
	if err != nil {
		t.Fatalf("read peer: %v", err)
	}
	if got.Excluded {
		t.Fatalf("good peer excluded")
	}
	// 30 good checks: (30+9)/(30+10) = 0.975 — the prior
	// keeps a good peer near the top without ever calling it
	// perfect.
	if trust := swarmTrust(got.GoodChecks, got.BadChecks); trust < 0.975 {
		t.Fatalf("good peer trust %.3f, want >= 0.975", trust)
	}
	// One unlucky check does not exile: new peers start near
	// the top and a single failure barely moves them.
	swarmTestPeer(t, app, "peer-new")
	app.countSwarmCheck(ctx, "peer-new", false)
	got, err = app.queries.GetSwarmPeer(ctx, "peer-new")
	if err != nil {
		t.Fatalf("read peer: %v", err)
	}
	if got.Excluded {
		t.Fatalf("single failure excluded a new peer")
	}
}

func TestSwarmBaseURLRule(t *testing.T) {
	for _, raw := range []string{
		"https://swarm.example.org/",
		"https://swarm.example.org:8443/base",
		"http://127.0.0.1:8080",
		"http://localhost:8080/",
		"http://[::1]:8080/",
	} {
		if !validSwarmBaseURL(raw) {
			t.Fatalf("valid URL refused: %q", raw)
		}
	}
	for _, raw := range []string{
		"",
		"not a url",
		"http://swarm.example.org/",
		"http://192.168.1.10:8080/",
		"ftp://swarm.example.org/",
		"gopher://swarm.example.org/",
	} {
		if validSwarmBaseURL(raw) {
			t.Fatalf("unsafe URL dialable: %q", raw)
		}
	}
}

func TestSwarmServeMissesWhenEmpty(t *testing.T) {
	app := swarmTestApp(t)
	ctx := context.Background()
	for _, k := range []swarmPullKey{
		{Kind: swarmKindHistory, Key: "10000002/34"},
		{Kind: swarmKindTypeName, Key: "34"},
		{Kind: swarmKindCorporation, Key: "98000001"},
		{Kind: swarmKindAlliance, Key: "99000001"},
		{Kind: swarmKindStructure, Key: "1044752365771"},
	} {
		if _, _, ok := app.serveSwarmKey(ctx, k.Kind, k.Key); ok {
			t.Fatalf("served %s %s from empty tables", k.Kind, k.Key)
		}
	}
}

func TestSwarmGossipDue(t *testing.T) {
	app := swarmTestApp(t)
	if !app.swarmGossipDue(time.Now().UTC()) {
		t.Fatalf("first run: gossip not due")
	}
	app.swarmGossipDone(time.Now().UTC())
	if app.swarmGossipDue(time.Now().UTC()) {
		t.Fatalf("just gossiped: gossip due again")
	}
}
