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
	if _, err := app.queries.UpsertSwarmPeer(context.Background(), db.UpsertSwarmPeerParams{
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
	if code, _ := swarmServe(t, app, http.MethodPost, "/swarm/seed/register", `{"entry":{}}`, swarmTestSecret); code != http.StatusNotFound {
		t.Fatalf("seed register while off: status %d, want 404", code)
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

func TestSwarmMembersSealed(t *testing.T) {
	app := swarmTestApp(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := app.queries.UpsertSwarmPeer(ctx, db.UpsertSwarmPeerParams{
		InstanceID: "peer-hidden-1", BaseUrl: "https://hidden-peer.example.org/",
		PublicKey: strings.Repeat("A", 44), Source: "gossip", FirstSeen: now,
		// Only a peer with a signed address is relayed; the list
		// passes the stored signature on without judging it.
		UrlSig: strings.Repeat("B", 88),
	}); err != nil {
		t.Fatalf("upsert peer: %v", err)
	}
	code, body := swarmServe(t, app, http.MethodGet, "/swarm/members", "", swarmTestSecret)
	if code != http.StatusOK {
		t.Fatalf("members: status %d, want 200", code)
	}
	// The wire carries no cleartext topology: not the peer's
	// address, not even JSON framing around it.
	if strings.Contains(body, "hidden-peer") {
		t.Fatalf("member list leaks the peer address in cleartext")
	}
	var decoded struct {
		Entry swarmSignedEntry `json:"entry"`
	}
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("members body: %v", err)
	}
	if !verifySwarmEntry(decoded.Entry, app.swarmID.publicString()) {
		t.Fatalf("sealed member list does not verify")
	}
	opened, err := openSwarmMembers(decoded.Entry.Payload, swarmTestSecret)
	if err != nil {
		t.Fatalf("member list does not open under the secret: %v", err)
	}
	if !strings.Contains(string(opened), "hidden-peer") {
		t.Fatalf("opened member list lacks the peer: %s", opened)
	}
	// ...and stays shut under any other secret.
	if _, err := openSwarmMembers(decoded.Entry.Payload, strings.Repeat("x", 32)); err == nil {
		t.Fatalf("member list opened under the wrong secret")
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
	pull, err := app.queries.ListSwarmPullPeers(ctx, time.Now().Add(-swarmServesWindow))
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
	ctx := context.Background()
	if !app.swarmGossipDue(ctx, time.Now().UTC()) {
		t.Fatalf("first run: gossip not due")
	}
	app.swarmGossipDone(ctx, time.Now().UTC())
	if app.swarmGossipDue(ctx, time.Now().UTC()) {
		t.Fatalf("just gossiped: gossip due again")
	}
}

// TestSwarmCheckDraw pins the sampling probabilities: always
// verified under ten total checks, then half at half trust and
// five percent at the top, clamped both ends.
func TestSwarmCheckDraw(t *testing.T) {
	for _, tc := range []struct {
		trust float64
		total int64
		r     float64
		want  bool
	}{
		{0.9, 5, 0.0, true},  // volume gate: new signers always verify
		{0.9, 9, 0.99, true}, // still under the gate at nine
		{0.5, 10, 0.499, true}, {0.5, 10, 0.5, false},
		{0.99, 100, 0.049, true}, {0.99, 100, 0.05, false},
		{1.0, 50, 0.049, true}, {1.0, 50, 0.051, false}, // floor: five percent
		{0.0, 50, 0.499, true}, {0.0, 50, 0.5, false}, // cap: half
	} {
		if got := swarmCheckDraw(tc.trust, tc.total, tc.r); got != tc.want {
			t.Errorf("draw(%v, %d, %v) = %v, want %v", tc.trust, tc.total, tc.r, got, tc.want)
		}
	}
}

// TestSwarmTrustStreakDecaySuppress covers the ledger dynamics
// on one fixture: consecutive failures weigh doubling, any
// success resets the run, old bad marks decay on the gossip
// cadence, five in a row excludes whatever the lifetime score
// says, and fresh staged rows suppress refetches while old
// ones do not.
func TestSwarmTrustStreakDecaySuppress(t *testing.T) {
	ctx := context.Background()
	app := swarmTestApp(t)
	now := time.Now().UTC()

	swarmTestPeer(t, app, "peer-s1")
	for i := 0; i < 3; i++ {
		app.countSwarmCheck(ctx, "peer-s1", false)
	}
	got, err := app.queries.GetSwarmPeer(ctx, "peer-s1")
	if err != nil || got.BadChecks != 7 {
		t.Fatalf("escalation: bad %d, want 7 (1+2+4)", got.BadChecks)
	}
	if err := app.queries.DecaySwarmBadChecks(ctx); err != nil {
		t.Fatalf("decay: %v", err)
	}
	got, err = app.queries.GetSwarmPeer(ctx, "peer-s1")
	if err != nil || got.BadChecks != 6 {
		t.Fatalf("decay: bad %d, want 6 (7*7/8)", got.BadChecks)
	}
	app.countSwarmCheck(ctx, "peer-s1", true)
	app.countSwarmCheck(ctx, "peer-s1", false)
	got, err = app.queries.GetSwarmPeer(ctx, "peer-s1")
	if err != nil || got.BadChecks != 7 || got.GoodChecks != 1 {
		t.Fatalf("reset: %+v, want bad 7 good 1", got)
	}

	swarmTestPeer(t, app, "peer-s2")
	for i := 0; i < 30; i++ {
		app.countSwarmCheck(ctx, "peer-s2", true)
	}
	for i := 0; i < 5; i++ {
		app.countSwarmCheck(ctx, "peer-s2", false)
	}
	got, err = app.queries.GetSwarmPeer(ctx, "peer-s2")
	if err != nil {
		t.Fatalf("read peer: %v", err)
	}
	if !got.Excluded || !strings.Contains(got.ExcludeNote, "consecutive") {
		t.Fatalf("velocity exclusion: excluded %v note %q", got.Excluded, got.ExcludeNote)
	}

	kx := swarmTestPeer(t, app, "peer-x")
	payload := `{"region_id":10000002,"type_id":34,"days":[{"date":"2026-10-01","average":5.5,"highest":6.0,"lowest":5.0,"volume":100,"order_count":10}]}`
	fresh, err := signSwarmEntry(kx, "peer-x", swarmKindHistory, "10000002/34", payload, now)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := app.stageSwarmEntry(ctx, fresh, "peer-x", now); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if !app.swarmSuppressFetch(ctx, swarmKindHistory, "10000002/34", now) {
		t.Fatal("fresh staged row does not suppress its refetch")
	}
	stalePayload := `{"region_id":10000002,"type_id":35,"days":[{"date":"2026-10-01","average":5.5,"highest":6.0,"lowest":5.0,"volume":100,"order_count":10}]}`
	stale, err := signSwarmEntry(kx, "peer-x", swarmKindHistory, "10000002/35", stalePayload, now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := app.stageSwarmEntry(ctx, stale, "peer-x", now.Add(-time.Hour)); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if app.swarmSuppressFetch(ctx, swarmKindHistory, "10000002/35", now) {
		t.Fatal("hour-old staged row still suppresses its refetch")
	}

	// A single stale mark forgives fully in one round. 1*7/8
	// truncates to zero: deliberate, not an accident of the
	// integer math, so the cliff is pinned here.
	swarmTestPeer(t, app, "peer-s3")
	app.countSwarmCheck(ctx, "peer-s3", false)
	if err := app.queries.DecaySwarmBadChecks(ctx); err != nil {
		t.Fatalf("decay: %v", err)
	}
	got, err = app.queries.GetSwarmPeer(ctx, "peer-s3")
	if err != nil || got.BadChecks != 0 {
		t.Fatalf("decay cliff: bad %d, want 0 (1*7/8)", got.BadChecks)
	}
}

// TestSwarmTrustEarnedAppliesUnledgered pins the trust-earned
// path end to end: a high-trust peer's staged entry applies
// without ESI verification, spends zero ESI calls, and moves
// neither the good/bad ledger nor the failure streak. Only an
// ESI verdict touches trust, through the spot pass and the
// urgent settle alike.
func TestSwarmTrustEarnedAppliesUnledgered(t *testing.T) {
	ctx := context.Background()
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

	kx := swarmTestPeer(t, app, "peer-earned")
	for i := 0; i < 50; i++ {
		app.countSwarmCheck(ctx, "peer-earned", true)
	}
	app.countSwarmCheck(ctx, "peer-earned", false)
	now := time.Now().UTC()
	one, err := signSwarmEntry(kx, "peer-earned", swarmKindHistory, "10000002/34",
		`{"region_id":10000002,"type_id":34,"days":[{"date":"2026-10-01","average":5.5,"highest":6.0,"lowest":5.0,"volume":100,"order_count":10}]}`, now)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := app.stageSwarmEntry(ctx, one, "peer-earned", now); err != nil {
		t.Fatalf("stage: %v", err)
	}
	// Trust sits near 0.97: force the draw past any plausible
	// sample rate so the entry skips verification.
	oldRand := swarmRandFloat
	swarmRandFloat = func() (float64, error) { return 0.99, nil }
	defer func() { swarmRandFloat = oldRand }()

	applied, limited := app.swarmSpotCheck(ctx, &fetchBudget{left: 10}, nil, now)
	if limited || applied != 1 {
		t.Fatalf("spot: applied %d limited %v, want 1/false", applied, limited)
	}
	if n := transport.calls.Load(); n != 0 {
		t.Fatalf("spot: %d ESI calls, want none", n)
	}
	hist, err := app.queries.ListMarketHistory(ctx, db.ListMarketHistoryParams{RegionID: 10000002, TypeID: 34, RowLimit: 5})
	if err != nil || len(hist) == 0 {
		t.Fatalf("spot: entry never applied (rows %d err %v)", len(hist), err)
	}
	assertEarnedUntouched := func(where string) {
		t.Helper()
		got, err := app.queries.GetSwarmPeer(ctx, "peer-earned")
		if err != nil || got.GoodChecks != 50 || got.BadChecks != 1 {
			t.Fatalf("%s: ledger %+v err %v, want good 50 bad 1", where, got, err)
		}
		app.swarmStreakMu.Lock()
		streak := app.swarmStreak["peer-earned"]
		app.swarmStreakMu.Unlock()
		if streak != 1 {
			t.Fatalf("%s: failure streak %d, want 1 (untouched)", where, streak)
		}
		if n := transport.calls.Load(); n != 0 {
			t.Fatalf("%s: %d ESI calls, want none", where, n)
		}
	}
	assertEarnedUntouched("spot")

	two, err := signSwarmEntry(kx, "peer-earned", swarmKindHistory, "10000002/35",
		`{"region_id":10000002,"type_id":35,"days":[{"date":"2026-10-01","average":5.5,"highest":6.0,"lowest":5.0,"volume":100,"order_count":10}]}`, now)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := app.stageSwarmEntry(ctx, two, "peer-earned", now); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if !app.swarmSettleUrgent(ctx, swarmKindHistory, "10000002/35") {
		t.Fatal("urgent settle did not apply the trust-earned entry")
	}
	assertEarnedUntouched("settle")
}
