package app

// Build claim tests: the claim an instance makes for itself
// (the SHA-256 of its own binary), the members gossip
// carrying and filing claims, the Admin classification
// against the release library, and the invariant the feature
// stands on — a claim is display metadata, so trust persists
// across releases: changing a peer's claim (upgrade,
// downgrade, a mainline/fork flip) moves nothing in its
// trust row.

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"evesynapse/internal/app/swarm"
	"net/http"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
)

// Claims used across the tests: two hashes the release
// library names (the two architectures of v0.5.2.10), and
// one it does not.
const (
	claimKnownA  = "90e1321d7066de809fc143199a2a824b93c1a35e31a6bf45e83826fb4bc555d4" // v0.5.2.10 amd64
	claimKnownB  = "18139b81fb608b686e489762ed1d6d7342358ce4d9bb5f42684c85d1e4e1b734" // v0.5.2.10 arm64
	claimUnknown = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
)

// TestBuildClaimIsOwnHash: this instance's claim is the hash
// of the binary the test itself runs as — a well-formed
// SHA-256 hex string, and (the test binary being no release)
// a Fork by the library.
func TestBuildClaimIsOwnHash(t *testing.T) {
	claim := swarm.BuildClaim()
	if claim == "" {
		t.Fatal("no claim: the test binary's executable should be readable")
	}
	raw, err := hex.DecodeString(claim)
	if err != nil || len(raw) != 32 {
		t.Fatalf("claim %q is not a SHA-256 hex string", claim)
	}
	if got := swarm.ClassifyBuild(claim); got != swarm.BuildFork {
		t.Fatalf("own (unreleased) build classified %v, want Fork", got)
	}
}

// TestClassifyBuild pins the classifier against the release
// library: a seeded release hash is Mainline and renders
// with the release's label, any other hash is a Fork, and
// no claim — a peer that predates claims — is Unknown.
func TestClassifyBuild(t *testing.T) {
	if len(swarm.ReleaseLibrary) == 0 {
		t.Fatal("the release library is empty")
	}
	for _, sha := range []string{claimKnownA, claimKnownB} {
		if got := swarm.ClassifyBuild(sha); got != swarm.BuildMainline {
			t.Fatalf("library hash %s… classified %v, want Mainline", sha[:8], got)
		}
	}
	if got := swarm.ClassifyBuild(claimUnknown); got != swarm.BuildFork {
		t.Fatalf("unlisted hash classified %v, want Fork", got)
	}
	if got := swarm.ClassifyBuild(""); got != swarm.BuildUnknown {
		t.Fatalf("empty claim classified %v, want Unknown", got)
	}
	if got := swarm.BuildLabel(claimKnownA); got != "Mainline · v0.5.2.10" {
		t.Fatalf("mainline label %q, want the release label", got)
	}
	if got := swarm.BuildLabel(claimUnknown); got != "Fork" {
		t.Fatalf("fork label %q", got)
	}
	if got := swarm.BuildLabel(""); got != "—" {
		t.Fatalf("unknown label %q, want —", got)
	}
}

// TestClassifyBuildStorm: a storm-typed library entry — the
// kind the first storm release will add — classifies as
// Mainline and renders with the Storm product named, while an
// app entry beside it keeps its plain release rendering.
func TestClassifyBuildStorm(t *testing.T) {
	const stormHash = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if _, ok := swarm.ReleaseLibrary[stormHash]; ok {
		t.Fatal("fixture hash is already in the library; pick another")
	}
	swarm.ReleaseLibrary[stormHash] = swarm.ReleaseBuild{Kind: swarm.ReleaseStorm, Label: "v0.1.0"}
	defer delete(swarm.ReleaseLibrary, stormHash)
	if got := swarm.ClassifyBuild(stormHash); got != swarm.BuildMainline {
		t.Fatalf("storm hash classified %v, want Mainline", got)
	}
	if got := swarm.BuildLabel(stormHash); got != "Mainline · Storm v0.1.0" {
		t.Fatalf("storm label %q, want the storm rendering", got)
	}
	if got := swarm.BuildLabel(claimKnownA); got != "Mainline · v0.5.2.10" {
		t.Fatalf("app label beside a storm entry %q, want the app rendering", got)
	}
}

// TestSeedAnnounceCarriesBuild: the heartbeat an instance
// builds for the seeds seals its own build claim beside the
// address it signs.
func TestSeedAnnounceCarriesBuild(t *testing.T) {
	app := swarmTestApp(t)
	swarm.StateFor(app).Cfg = swarm.SwarmConfig{Mode: swarm.SwarmOpen, Secret: strings.Repeat("s", 32)}
	app.cfg.eveCallbackURL = "https://node.example.org/auth/callback"
	entry, ok := swarm.StateFor(app).BuildSeedAnnounce(time.Now())
	if !ok {
		t.Fatal("no announce built for a sharing instance with an origin")
	}
	var payload swarm.SeedAnnouncePayload
	if err := json.Unmarshal([]byte(entry.Payload), &payload); err != nil {
		t.Fatalf("announce payload is not JSON: %v", err)
	}
	if payload.Build != swarm.BuildClaim() {
		t.Fatalf("announce claim %q, want own claim %q", payload.Build, swarm.BuildClaim())
	}
}

// TestSwarmSeedRegisterFilesBuild: a heartbeat files the
// sender's claim on its peer row through the gossip path,
// beside the address its signature owns.
func TestSwarmSeedRegisterFilesBuild(t *testing.T) {
	ctx := context.Background()
	app := swarmTestApp(t)
	kp, err := swarm.MintSwarmIdentity()
	if err != nil {
		t.Fatalf("mint peer: %v", err)
	}
	kp.ID = "peer-build-1"
	issued := time.Now().Unix()
	payload := string(mustJSON(t, swarm.SeedAnnouncePayload{
		BaseURL: "https://peer-build-1.example", PublicKey: kp.PublicString(),
		URLSig:   swarm.SignSwarmMember(kp, "https://peer-build-1.example", issued),
		IssuedAt: issued, Build: claimUnknown,
	}))
	entry, err := swarm.SignSwarmEntry(kp, kp.ID, swarm.SeedAnnounceKind, kp.ID, payload, time.Now())
	if err != nil {
		t.Fatalf("sign heartbeat: %v", err)
	}
	if code, ok := seedTestRegister(t, app, entry, swarmTestSecret); code != http.StatusOK || !ok {
		t.Fatalf("register: status %d ok %v", code, ok)
	}
	peer, err := app.queries.GetSwarmPeer(ctx, "peer-build-1")
	if err != nil {
		t.Fatalf("peer not filed: %v", err)
	}
	if peer.Build != claimUnknown {
		t.Fatalf("filed claim %q, want %q", peer.Build, claimUnknown)
	}
}

// TestSwarmMembersCarryBuild: the members list an instance
// serves names its own claim and relays the newest claim it
// holds for each signed peer.
func TestSwarmMembersCarryBuild(t *testing.T) {
	app := swarmTestApp(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := app.queries.UpsertSwarmPeer(ctx, db.UpsertSwarmPeerParams{
		InstanceID: "peer-hidden-1", BaseUrl: "https://hidden-peer.example.org/",
		PublicKey: strings.Repeat("A", 44), Source: "gossip", FirstSeen: now,
		UrlSig: strings.Repeat("B", 88), Build: claimKnownA,
	}); err != nil {
		t.Fatalf("upsert peer: %v", err)
	}
	code, body := swarmServe(t, app, http.MethodGet, "/swarm/members", "", swarmTestSecret)
	if code != http.StatusOK {
		t.Fatalf("members: status %d, want 200", code)
	}
	var decoded struct {
		Entry swarm.SwarmSignedEntry `json:"entry"`
	}
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("members body: %v", err)
	}
	raw, err := swarm.OpenSwarmMembers(decoded.Entry.Payload, swarmTestSecret)
	if err != nil {
		t.Fatalf("open members: %v", err)
	}
	var members []swarm.SwarmMember
	if err := json.Unmarshal(raw, &members); err != nil {
		t.Fatalf("members list: %v", err)
	}
	byID := map[string]swarm.SwarmMember{}
	for _, m := range members {
		byID[m.InstanceID] = m
	}
	if got := byID["test-local"].Build; got != swarm.BuildClaim() {
		t.Fatalf("self claim on the wire %q, want %q", got, swarm.BuildClaim())
	}
	if got := byID["peer-hidden-1"].Build; got != claimKnownA {
		t.Fatalf("relayed claim %q, want %q", got, claimKnownA)
	}
}

// TestSwarmLearnMembersFilesBuild: a member list learned from
// a peer files each member's claim beside the address its own
// signature owns.
func TestSwarmLearnMembersFilesBuild(t *testing.T) {
	app := swarmTestApp(t)
	ctx := context.Background()
	sender := swarmTestPeer(t, app, "sender-1")
	memberKP, err := swarm.MintSwarmIdentity()
	if err != nil {
		t.Fatalf("mint member: %v", err)
	}
	memberKP.ID = "member-1"
	issued := time.Now().Unix()
	member := swarm.SwarmMember{
		InstanceID: "member-1", BaseURL: "https://member-1.example",
		PublicKey: memberKP.PublicString(), IssuedAt: issued, Build: claimKnownB,
	}
	member.URLSig = swarm.SignSwarmMember(memberKP, member.BaseURL, issued)
	if err := swarm.StateFor(app).SwarmLearnMembers(ctx, []swarm.SwarmMember{member}, sender.ID, sender.PublicString(), "", "gossip", time.Now()); err != nil {
		t.Fatalf("learn members: %v", err)
	}
	peer, err := app.queries.GetSwarmPeer(ctx, "member-1")
	if err != nil {
		t.Fatalf("member not filed: %v", err)
	}
	if peer.Build != claimKnownB {
		t.Fatalf("filed claim %q, want %q", peer.Build, claimKnownB)
	}
}

// TestBuildClaimChangeLeavesTrustAlone is the invariant: a
// peer's claim changing — here from one official release to
// a build no release matches, the full classification flip —
// updates the label on its row and nothing else. Approval,
// exclusion, the check counters and the key-conflict mark
// are exactly what they were; only the address tuple's
// freshness fields (the signature and issue time the new
// claim arrived with) move.
func TestBuildClaimChangeLeavesTrustAlone(t *testing.T) {
	app := swarmTestApp(t)
	ctx := context.Background()
	kp := swarmTestPeer(t, app, "peer-flip")
	if err := app.queries.AddSwarmPeerCheck(ctx, db.AddSwarmPeerCheckParams{InstanceID: "peer-flip", GoodChecks: 7, BadChecks: 2}); err != nil {
		t.Fatalf("record checks: %v", err)
	}
	mod := swarm.StateFor(app)
	learn := func(build string, issued int64) {
		t.Helper()
		m := swarm.SwarmMember{
			InstanceID: "peer-flip", BaseURL: "https://flip.example",
			PublicKey: kp.PublicString(), IssuedAt: issued, Build: build,
		}
		m.URLSig = swarm.SignSwarmMember(kp, m.BaseURL, issued)
		if err := mod.SwarmLearnMembers(ctx, []swarm.SwarmMember{m}, kp.ID, kp.PublicString(), "", "gossip", time.Now()); err != nil {
			t.Fatalf("learn %q: %v", build, err)
		}
	}
	base := time.Now().Unix()
	learn(claimKnownA, base)
	before, err := app.queries.GetSwarmPeer(ctx, "peer-flip")
	if err != nil {
		t.Fatalf("peer after first claim: %v", err)
	}
	if before.Build != claimKnownA {
		t.Fatalf("first claim not filed: %q", before.Build)
	}
	learn(claimUnknown, base+60)
	after, err := app.queries.GetSwarmPeer(ctx, "peer-flip")
	if err != nil {
		t.Fatalf("peer after claim change: %v", err)
	}
	if after.Build != claimUnknown {
		t.Fatalf("changed claim not filed: %q", after.Build)
	}
	if swarm.ClassifyBuild(before.Build) == swarm.ClassifyBuild(after.Build) {
		t.Fatal("the two claims classify alike; the test no longer spans the mainline/fork flip")
	}
	if after.Approved != before.Approved || after.Excluded != before.Excluded ||
		after.GoodChecks != before.GoodChecks || after.BadChecks != before.BadChecks ||
		after.ExcludeNote != before.ExcludeNote || after.KeyConflictAt.Valid != before.KeyConflictAt.Valid ||
		after.LastPulled.Valid != before.LastPulled.Valid || after.LastServed.Valid != before.LastServed.Valid {
		t.Fatalf("a claim change moved trust state:\nbefore %+v\nafter  %+v", before, after)
	}
	// A claim arriving with an older tuple than the one held
	// is as stale as its address: it files nothing.
	learn(claimKnownB, base)
	stale, err := app.queries.GetSwarmPeer(ctx, "peer-flip")
	if err != nil {
		t.Fatalf("peer after stale claim: %v", err)
	}
	if stale.Build != claimUnknown {
		t.Fatalf("stale claim overwrote the filed one: %q", stale.Build)
	}
}

// TestSwarmAdminBuildColumn renders the Admin table with a
// mainline peer, a fork peer, and a peer that never claimed:
// the heading and all three cells reach the page.
func TestSwarmAdminBuildColumn(t *testing.T) {
	app, cookie := swarmPageApp(t)
	swarm.StateFor(app).Cfg = swarm.SwarmConfig{Mode: swarm.SwarmOpen, Secret: swarmTestSecret, GossipHours: swarm.DefaultSwarmGossipHours}
	ctx := context.Background()
	now := time.Now().UTC()
	for _, peer := range []struct{ id, url, build string }{
		{"peer-main-1", "https://mainline.example", claimKnownA},
		{"peer-fork-1", "https://fork.example", claimUnknown},
		{"peer-quiet-1", "https://quiet.example", ""},
	} {
		if _, err := app.queries.UpsertSwarmPeer(ctx, db.UpsertSwarmPeerParams{
			InstanceID: peer.id, BaseUrl: peer.url,
			PublicKey: strings.Repeat("A", 44), Source: "gossip", FirstSeen: now,
			Build: peer.build,
		}); err != nil {
			t.Fatalf("upsert %s: %v", peer.id, err)
		}
	}
	rows := swarm.StateFor(app).LoadSwarmPeers(ctx)
	byID := map[string]swarm.SwarmPeerRow{}
	for _, row := range rows {
		byID[row.ID] = row
	}
	if got := byID["peer-main-1"].Build; got != "Mainline · v0.5.2.10" {
		t.Errorf("mainline peer build cell %q", got)
	}
	if got := byID["peer-fork-1"].Build; got != "Fork" {
		t.Errorf("fork peer build cell %q", got)
	}
	if got := byID["peer-quiet-1"].Build; got != "—" {
		t.Errorf("claimless peer build cell %q, want —", got)
	}
	code, body := getPage(t, app, cookie, "/admin/")
	if code != http.StatusOK {
		t.Fatalf("/admin/: status %d", code)
	}
	mustContain(t, "/admin/", body, "<th>Build</th>", "Mainline · v0.5.2.10", "<td>Fork</td>")
}
