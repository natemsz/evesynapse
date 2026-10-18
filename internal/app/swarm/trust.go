package swarm

// ---------------------------------------------------------------------------
// Swarm trust: gossip, demand-driven pulls, and spot checks (the
// paper: "entries are spot-checked against ESI itself... one bad
// instance cannot poison the rest").
//
// The worker's swarm pass runs gossip (due about daily), pulls
// what this instance already wants from approved peers, and
// spot-checks staged entries against ESI. Peer-to-peer traffic
// spends no ESI budget; only the spot checks do, a few calls a
// cycle from the cycle's shared allowance — urgency reorders,
// never widens.
//
// Reputation is per signer, from this instance's own checks
// only: good/(good+bad) with a 9-to-1 prior, so a new instance
// starts at 0.9 and a sustained liar sinks. Ten checks under
// 0.5 excludes automatically; the operator can always exclude
// (or re-admit) by hand. Reputation never travels: gossip moves
// identity material (IDs, URLs, keys), never judgments, so a
// liar cannot talk others into distrusting someone.
// ---------------------------------------------------------------------------

import (
	"context"
	"crypto/ed25519"
	cryptorand "crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"evesynapse/internal/app/module"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// Trust arithmetic: a new signer starts at 0.9 (the 9-to-1
// prior), and ten checks under 0.5 excludes it. The floor needs
// the minimum so one unlucky check (a renamed structure, a
// deleted corporation) never exiles anyone.
//
// Verification is sampled, not exhaustive: the check rate
// falls as trust rises (half at 0.5, five percent at 0.99),
// so a trusted peer's entries mostly apply on signature and
// shape alone while a thin random sample keeps ESI honest.
// The draw is per entry and unpredictable; a peer that can
// predict the sample only has to poison the rest. Below ten
// total checks every entry is verified: trust is earned with
// evidence first, sampled after.
//
// Failure weight accelerates with consecutiveness: the first
// failure in a row counts once, the next twice, then four,
// doubling to a cap. A compromised high-trust peer is ejected
// on velocity, not just level: five consecutive failures
// exclude whatever the lifetime score says. Any success
// resets the streak. The streak lives in memory (it resets on
// restart); the ledger in the database does not. Shape
// rejects at staging (bad signatures, malformed payloads)
// look like brokenness, not malice, so they ledger flat and
// never touch the streak.
const (
	SwarmTrustPriorGood = 9
	SwarmTrustPriorBad  = 1
	SwarmTrustFloor     = 0.5
	SwarmTrustMinChecks = 10

	SwarmSampleMin = 0.05
	SwarmSampleMax = 0.5

	SwarmStreakWeightCap = 10
	SwarmStreakExclude   = 5
)

// SwarmStagedSuppressWindow bounds the coordination between
// the pull and the passes: a key with an unsettled staged
// entry newer than this skips its ESI refetch, because a spot
// decision is moments away. Older rows fall back to ESI: the
// spot pass evidently is not settling them, and pages must
// not rot behind a stuck row. Discards re-mark the key due by
// leaving fetch state untouched, so suppression can only
// delay a fetch, never cancel it.
const SwarmStagedSuppressWindow = 10 * time.Minute

// SwarmUrgentPullTimeout bounds one peer pull inside the
// urgent drain: a hung peer must not wedge the seconds-scale
// tick behind it (the drain holds the shared fetch lock).
const SwarmUrgentPullTimeout = 3 * time.Second

// Per-cycle bounds: spot checks are ESI calls (from the shared
// allowance), pulls are peer HTTP (bounded anyway), and gossip
// touches a few peers a day.
const (
	MaxSwarmSpotPerCycle      = 5
	MaxSwarmPullPeersPerCycle = 3
	MaxSwarmDemandKeys        = 50
	MaxSwarmGossipPeers       = 10
	MaxSwarmHistoryDemand     = 30
	MaxSwarmOrgDemand         = 10
	MaxSwarmNameDemand        = 5 // per name kind
	MaxSwarmContractDemand    = 10
)

// Staged entries older than this never apply: an entry nobody
// could verify within a week is stale, not evidence.
const MaxSwarmStagingAge = 7 * 24 * time.Hour

// MaxSwarmDeferrals is how many passes may fail to decide a
// staged entry before it is dropped.
const MaxSwarmDeferrals = 3

// Sync-page counter names.
const (
	SwarmCounterAppliedHistory   = "applied_history"
	SwarmCounterAppliedNames     = "applied_names"
	SwarmCounterAppliedRecords   = "applied_records"
	SwarmCounterAppliedContracts = "applied_contracts"
	SwarmCounterSpotPassed       = "spot_passed"
	SwarmCounterSpotFailed       = "spot_failed"
	SwarmCounterPullsOK          = "pulls_ok"
	SwarmCounterPullsFailed      = "pulls_failed"
	SwarmCounterPullsAuthSigned  = "pulls_auth_signed"
	SwarmCounterPullsAuthLegacy  = "pulls_auth_legacy"
	SwarmCounterRolledBack       = "rolled_back"
	SwarmCounterDroppedUnchecked = "dropped_unchecked"
)

// SwarmTrust scores a signer from its ledger: good over total
// with the prior, so the first checks move it little and
// sustained behavior moves it a lot.
func SwarmTrust(good, bad int64) float64 {
	return float64(good+SwarmTrustPriorGood) / float64(good+bad+SwarmTrustPriorGood+SwarmTrustPriorBad)
}

// AddSwarmCounter bumps one Sync-page counter (best effort,
// like the snapshot fetch-state log).
func (app *Mod) AddSwarmCounter(ctx context.Context, name string, n int64) {
	if err := app.Queries.AddSwarmCounter(ctx, db.AddSwarmCounterParams{Name: name, Value: n}); err != nil {
		logging.Errorf("swarm: counter %s: %v", name, err)
	}
}

// SwarmCheckDraw reports whether one entry is ESI-verified:
// always while the signer has fewer than ten total checks,
// then with probability falling from half to five percent as
// trust rises. r must be uniform in [0,1); the caller draws it
// unpredictably per entry (SwarmRandFloat), never per batch.
func SwarmCheckDraw(trust float64, total int64, r float64) bool {
	if total < SwarmTrustMinChecks {
		return true
	}
	p := 1 - trust
	if p < SwarmSampleMin {
		p = SwarmSampleMin
	}
	if p > SwarmSampleMax {
		p = SwarmSampleMax
	}
	return r < p
}

// SwarmRandFloat draws one uniform float in [0,1) from the
// operating system's randomness: a malicious peer observing
// outcomes cannot predict the next draw and poison around it.
// A variable (not a Call) so tests pin deterministic draws;
// production never reassigns it.
var SwarmRandFloat = func() (float64, error) {
	var b [8]byte
	if _, err := cryptorand.Read(b[:]); err != nil {
		return 0, err
	}
	return float64(binary.BigEndian.Uint64(b[:])) / 18446744073709551616.0, nil
}

// SwarmStreakBump advances a signer's consecutive-failure run:
// any success resets it to zero, any failure extends it and
// reports the ledger weight for this failure (doubling per
// consecutive failure to a cap). In-memory only, alongside the
// streak map the struct comment describes.
func (app *Mod) SwarmStreakBump(signer string, good bool) (streak int, weight int64) {
	app.SwarmStreakMu.Lock()
	defer app.SwarmStreakMu.Unlock()
	if app.SwarmStreak == nil {
		app.SwarmStreak = make(map[string]int)
	}
	if good {
		delete(app.SwarmStreak, signer)
		return 0, 1
	}
	streak = app.SwarmStreak[signer] + 1
	app.SwarmStreak[signer] = streak
	shift := streak - 1
	if shift > SwarmStreakWeightCap {
		shift = SwarmStreakWeightCap
	}
	return streak, int64(1) << uint(shift)
}

// CountSwarmCheck ledgers one ESI-verified data verdict (a
// spot check or an inline urgent one): bad verdicts weigh by
// consecutiveness, then exclusion delegates to
// SwarmEnforceTrust with the streak, the same single
// implementation every ledger path shares.
func (app *Mod) CountSwarmCheck(ctx context.Context, signer string, good bool) {
	streak, weight := app.SwarmStreakBump(signer, good)
	var goodN, badN int64
	if good {
		goodN = 1
	} else {
		badN = weight
	}
	if err := app.Queries.AddSwarmPeerCheck(ctx, db.AddSwarmPeerCheckParams{InstanceID: signer, GoodChecks: goodN, BadChecks: badN}); err != nil {
		logging.Errorf("swarm: ledger check for %s: %v", signer, err)
		return
	}
	app.SwarmEnforceTrust(ctx, signer, streak)
}

// CountSwarmShapeReject ledgers one staging reject (a bad
// signature, a malformed payload, an unknown signer): flat,
// streak untouched. Rejects never applied anything, so they
// read as a broken peer, not a poisoning one.
func (app *Mod) CountSwarmShapeReject(ctx context.Context, signer string) {
	if err := app.Queries.AddSwarmPeerCheck(ctx, db.AddSwarmPeerCheckParams{InstanceID: signer, GoodChecks: 0, BadChecks: 1}); err != nil {
		logging.Errorf("swarm: ledger reject for %s: %v", signer, err)
		return
	}
	app.SwarmEnforceTrust(ctx, signer, 0)
}

// SwarmEnforceTrust excludes a signer that earned it: five
// consecutive data failures whatever the lifetime score, or
// enough lifetime checks under the floor. Streak zero (shape
// rejects, or anything without velocity) can only trip the
// floor rule.
func (app *Mod) SwarmEnforceTrust(ctx context.Context, signer string, streak int) {
	peer, err := app.Queries.GetSwarmPeer(ctx, signer)
	if err != nil || peer.Excluded {
		return
	}
	if streak >= SwarmStreakExclude {
		note := fmt.Sprintf("auto: %d consecutive failures", streak)
		if err := app.Queries.SetSwarmPeerExcluded(ctx, db.SetSwarmPeerExcludedParams{InstanceID: signer, Excluded: true, ExcludeNote: note}); err != nil {
			logging.Errorf("swarm: auto-exclude %s: %v", signer, err)
			return
		}
		logging.Warnf("swarm: excluded %s (%s)", signer, note)
		return
	}
	total := peer.GoodChecks + peer.BadChecks
	if total >= SwarmTrustMinChecks && SwarmTrust(peer.GoodChecks, peer.BadChecks) < SwarmTrustFloor {
		note := fmt.Sprintf("auto: trust %.2f over %d checks below %.2f", SwarmTrust(peer.GoodChecks, peer.BadChecks), total, SwarmTrustFloor)
		if err := app.Queries.SetSwarmPeerExcluded(ctx, db.SetSwarmPeerExcludedParams{InstanceID: signer, Excluded: true, ExcludeNote: note}); err != nil {
			logging.Errorf("swarm: auto-exclude %s: %v", signer, err)
			return
		}
		logging.Warnf("swarm: excluded %s (%s)", signer, note)
	}
}

// SwarmFetchPhase is the worker's early swarm pass: gossip when
// due, then pull what is wanted. It spends no ESI calls (peer
// HTTP only), so the cycle runs it ahead of its own fetch
// passes: demand is genuine at pull time, and anything a peer
// serves settles the fetch state before the worker spends an
// ESI Call on the same datum. The spot checks stay in the later
// SwarmCycle — they spend ESI, so they wait behind the passes
// whose pages are watching.
func (app *Mod) SwarmFetchPhase(ctx context.Context, now time.Time) {
	if !app.Cfg.Enabled() || app.SwarmID == nil || app.SwarmCli == nil {
		return
	}
	app.SwarmGossip(ctx, now)
	app.SwarmRefreshCoverage(ctx, now)
	app.SwarmPruneAsked(now)
	app.SwarmPull(ctx, now)
}

// SwarmCycle is the worker's later swarm pass: take back what
// excluded signers supplied, then spot-check what is staged.
// Gossip and the pull belong to SwarmFetchPhase alone, once a
// cycle: a second round here would ask every peer the same
// questions again minutes later. Only the spot checks spend
// ESI calls, from the cycle's allowance; limited reports ESI's
// stop signal like every other pass.
func (app *Mod) SwarmCycle(ctx context.Context, allowance module.Budget, characters []db.Character, now time.Time) (applied int, limited bool) {
	if !app.Cfg.Enabled() || app.SwarmID == nil || app.SwarmCli == nil {
		return 0, false
	}
	// Before anything new applies, take back what excluded
	// signers already supplied (local writes, no ESI).
	app.SwarmRollBack(ctx)
	return app.SwarmSpotCheck(ctx, allowance, characters, now)
}

// SwarmGossipDue reports whether the membership round runs:
// when no round has run yet (bootstrap), or once the time drawn
// at the end of the last round has come. That time is read from
// the database on first use, so a restart keeps the schedule
// instead of gossiping at once.
func (app *Mod) SwarmGossipDue(ctx context.Context, now time.Time) bool {
	app.SwarmMu.Lock()
	defer app.SwarmMu.Unlock()
	if !app.SwarmNextLoaded {
		// No identity row, or no time drawn yet, reads as due.
		if next, err := app.Queries.GetSwarmNextGossip(ctx); err == nil && next.Valid {
			app.SwarmNextGossip = next.Time
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
			logging.Errorf("swarm: read next gossip time: %v", err)
		}
		app.SwarmNextLoaded = true
	}
	return app.SwarmNextGossip.IsZero() || !now.Before(app.SwarmNextGossip)
}

// SwarmGossipDone ends a round by drawing the next one's time:
// the interval, jittered ±25% so a whole swarm does not gossip
// in lockstep. The draw happens once, here, and the result is
// kept: drawing afresh on every check would fire at the first
// check to draw low, bunching every round near the early edge.
func (app *Mod) SwarmGossipDone(ctx context.Context, now time.Time) {
	interval := time.Duration(app.Cfg.GossipHours) * time.Hour
	jitter := interval / 4
	next := now.Add(interval - jitter + time.Duration(rand.Int64N(int64(2*jitter+1))))
	app.SwarmMu.Lock()
	app.SwarmNextGossip = next
	app.SwarmNextLoaded = true
	app.SwarmMu.Unlock()
	if err := app.Queries.SetSwarmNextGossip(ctx, timeSet(next)); err != nil {
		logging.Errorf("swarm: keep next gossip time: %v", err)
	}
}

// SwarmGossip exchanges membership with seeds and known peers.
// Learned members are recorded (trust on first use, behind the
// shared secret); approval follows the mode — open trusts every
// secret-holder, allowlist waits for the operator. Excluded
// peers are neither contacted nor propagated.
func (app *Mod) SwarmGossip(ctx context.Context, now time.Time) {
	if !app.SwarmGossipDue(ctx, now) {
		return
	}
	defer app.SwarmGossipDone(ctx, now)
	type contact struct {
		url, ID, key string
	}
	var contacts []contact
	for _, seed := range app.Cfg.Seeds {
		contacts = append(contacts, contact{url: seed})
	}
	peers, err := app.Queries.ListSwarmPeers(ctx)
	if err != nil {
		logging.Errorf("swarm: gossip peers: %v", err)
		return
	}
	n := 0
	for _, p := range peers {
		if p.Excluded || p.InstanceID == app.SwarmID.ID {
			continue
		}
		if n >= MaxSwarmGossipPeers {
			break
		}
		n++
		contacts = append(contacts, contact{url: p.BaseUrl, ID: p.InstanceID, key: p.PublicKey})
	}
	// The lists are fetched a few at a time under one deadline for
	// the round (swarm/fanout.go), then learned one at a time.
	type heard struct {
		members     []SwarmMember
		Signer, key string
		Err         error
	}
	results := make([]heard, len(contacts))
	SwarmFanOut(ctx, len(contacts), func(rctx context.Context, i int) {
		c, r := contacts[i], &results[i]
		r.members, r.Signer, r.key, r.Err = app.SwarmFetchMembers(rctx, c.url, c.ID, c.key)
	})
	learned := 0
	for i, c := range contacts {
		source := "gossip"
		if c.ID == "" {
			source = "seed"
		}
		r := results[i]
		if app.SwarmAbsorbMembers(ctx, c.url, c.ID, source, r.members, r.Signer, r.key, r.Err, now) == nil {
			learned++
		}
	}
	if learned > 0 {
		logging.Infof("swarm: gossip heard from %d peer(s)", learned)
	}
	// Every round ends with one heartbeat per seed, so the
	// directory keeps a fresh record of this instance even
	// when nobody has pulled its member list lately.
	app.SwarmAnnounceToSeeds(ctx, now)
	// Forgiveness on the gossip cadence: old bad marks fade
	// whether or not new checks land, so a fixed peer's score
	// recovers over weeks. Exclusion itself never clears here.
	if err := app.Queries.DecaySwarmBadChecks(ctx); err != nil {
		logging.Errorf("swarm: decay bad marks: %v", err)
	}
}

// SwarmContactPeer asks one address for its membership list and
// records what it learns: the one gossip round, shared by seeds,
// known peers, and operator-added addresses alike. A known peer
// must serve under its pinned identity; an address with no
// pinned key yet is trust-on-first-use — the shared secret
// already proved membership, and the key proves possession from
// here on. Nil means the address answered and its list was
// learned; ErrSwarmKeyChanged means it answered as an instance
// already pinned under another key, and taught nothing.
func (app *Mod) SwarmContactPeer(ctx context.Context, baseURL, peerID, peerKey, source string, now time.Time) error {
	members, signer, key, err := app.SwarmFetchMembers(ctx, baseURL, peerID, peerKey)
	return app.SwarmAbsorbMembers(ctx, baseURL, peerID, source, members, signer, key, err, now)
}

// SwarmAbsorbMembers records what one address answered a members
// request with (fetchErr when it did not answer): the second half of
// SwarmContactPeer, kept apart so the gossip round can fetch in
// parallel and record one at a time.
func (app *Mod) SwarmAbsorbMembers(ctx context.Context, baseURL, peerID, source string, members []SwarmMember, signer, key string, fetchErr error, now time.Time) error {
	if fetchErr != nil {
		return fetchErr
	}
	if peerID != "" && signer != peerID {
		return fmt.Errorf("swarm: %s answered as another instance", baseURL)
	}
	if err := app.SwarmLearnMembers(ctx, members, signer, key, baseURL, source, now); err != nil {
		if !errors.Is(err, ErrSwarmKeyChanged) {
			logging.Errorf("swarm: learn members via %s: %v", baseURL, err)
		}
		return err
	}
	if err := app.Queries.TouchSwarmPeerGossip(ctx, db.TouchSwarmPeerGossipParams{InstanceID: signer, LastGossip: timeSet(now)}); err != nil {
		logging.Errorf("swarm: touch gossip %s: %v", signer, err)
	}
	// It answered: if it is not known to serve data, the next
	// pull may probe it once more. Membership is slow, so this
	// costs a directory one pull a round, not one a cycle.
	if err := app.Queries.RearmSwarmPeerProbe(ctx, db.RearmSwarmPeerProbeParams{
		InstanceID: signer, ServedSince: now.Add(-SwarmServesWindow),
	}); err != nil {
		logging.Errorf("swarm: re-arm probe of %s: %v", ShortenSwarmID(signer), err)
	}
	return nil
}

// SwarmFetchMembers asks one URL for its membership list. A
// known peer must serve under its pinned key; a seed (no pinned
// key yet) is trust-on-first-use — the shared secret already
// proved membership, and the key proves possession from here on.
func (app *Mod) SwarmFetchMembers(ctx context.Context, baseURL, peerID, peerKey string) (members []SwarmMember, signer, key string, err error) {
	var body struct {
		Entry SwarmSignedEntry `json:"entry"`
	}
	if err := app.SwarmCli.AuthedGET(ctx, strings.TrimSuffix(baseURL, "/")+"/swarm/members", &body); err != nil {
		return nil, "", "", err
	}
	entry := body.Entry
	if entry.Kind != SwarmKindMembers || entry.Signer == "" {
		return nil, "", "", fmt.Errorf("swarm: %s served no member list", baseURL)
	}
	if peerKey != "" {
		if entry.Signer != peerID || !VerifySwarmEntry(entry, peerKey) {
			return nil, "", "", fmt.Errorf("swarm: %s served a member list that does not verify", baseURL)
		}
		return DecodeSwarmMembers(app.Cfg.Secret, entry.Payload, entry.Signer, peerKey)
	}
	// Trust on first use: the shared secret opens the list —
	// only a member could have sealed it — then the signer's
	// listed key must verify the signature, proving the key's
	// holder sent it.
	opened, err := OpenSwarmMembers(entry.Payload, app.Cfg.Secret)
	if err != nil {
		return nil, "", "", fmt.Errorf("swarm: %s served a member list that does not open", baseURL)
	}
	var list []SwarmMember
	if err := DecodeSwarmStrict(strings.NewReader(string(opened)), &list); err != nil {
		return nil, "", "", fmt.Errorf("swarm: %s served an unreadable member list", baseURL)
	}
	for _, m := range list {
		if m.InstanceID != entry.Signer {
			continue
		}
		if VerifySwarmEntry(entry, m.PublicKey) {
			return list, entry.Signer, m.PublicKey, nil
		}
	}
	return nil, "", "", fmt.Errorf("swarm: %s served a member list naming no verifiable signer", baseURL)
}

// DecodeSwarmMembers opens and strictly decodes a
// signature-verified member list.
func DecodeSwarmMembers(secret, payload, signer, key string) ([]SwarmMember, string, string, error) {
	opened, err := OpenSwarmMembers(payload, secret)
	if err != nil {
		return nil, "", "", fmt.Errorf("swarm: member list from %s does not open", signer)
	}
	var members []SwarmMember
	if err := DecodeSwarmStrict(strings.NewReader(string(opened)), &members); err != nil {
		return nil, "", "", fmt.Errorf("swarm: unreadable member list from %s", signer)
	}
	return members, signer, key, nil
}

// ErrSwarmKeyChanged reports a sighting whose sender presented a
// key other than the one pinned for it. Nothing it said is
// learned.
var ErrSwarmKeyChanged = errors.New("swarm: the sender's key is not the one pinned for it")

// SwarmFlagKeyConflict marks a refused key change on the peer
// it was aimed at and says so in the log. The pinned key stays;
// if the new one is real, the operator removes the peer and the
// next sighting pins it.
func (app *Mod) SwarmFlagKeyConflict(ctx context.Context, instanceID, heardFrom string, now time.Time) {
	logging.Warnf("swarm: refused a changed key for %s (heard from %s); the pinned key stays until the operator removes the instance",
		ShortenSwarmID(instanceID), ShortenSwarmID(heardFrom))
	if err := app.Queries.FlagSwarmPeerKeyConflict(ctx, db.FlagSwarmPeerKeyConflictParams{
		InstanceID: instanceID, KeyConflictAt: timeSet(now),
	}); err != nil {
		logging.Errorf("swarm: mark key conflict on %s: %v", ShortenSwarmID(instanceID), err)
	}
}

// SwarmLearnMembers records the members one sighting names.
//
// Keys: the first key seen for an instance ID is pinned. A
// sighting that names a known ID under another key is refused
// and flagged, never written (the upsert itself enforces it),
// and the only way to a new key is the operator removing the
// peer. A sender whose own key is not its pinned one teaches
// nothing at all (ErrSwarmKeyChanged).
//
// Addresses: an address is its owner's word. Each member tuple
// carries the owner's signature over its instance ID, address
// and issue time (url_sig), made with the key in the same tuple.
// A new member is stored only if that signature verifies; a
// known member's address moves only on a tuple whose signature
// verifies under the pinned key and which was issued later than
// the one held, whoever relayed it — an older tuple, however
// valid, is ignored, so a stale address cannot be replayed. The
// tuple is stored as it arrived, to be relayed unchanged. The one
// address taken without a signature is the one this instance
// dialed itself: a sender that signs no address of its own is
// filed under the URL that just answered, and such a row is
// never relayed.
//
// Approval follows the mode; exclusion is never touched.
func (app *Mod) SwarmLearnMembers(ctx context.Context, members []SwarmMember, signer, signerKey, dialURL, source string, now time.Time) error {
	if signer != "" && signerKey != "" {
		peer, err := app.Queries.GetSwarmPeer(ctx, signer)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// A sender never heard of is new, not broken.
		case err != nil:
			return err
		case peer.PublicKey != signerKey:
			app.SwarmFlagKeyConflict(ctx, signer, signer, now)
			return ErrSwarmKeyChanged
		}
	}
	seen := make(map[string]bool, len(members)+1)
	for _, m := range members {
		seen[m.InstanceID] = true
	}
	if signer != "" && signerKey != "" && !seen[signer] {
		members = append(members, SwarmMember{InstanceID: signer, PublicKey: signerKey})
	}
	for _, m := range members {
		if m.InstanceID == "" || m.InstanceID == app.SwarmID.ID {
			continue
		}
		if raw, err := base64.StdEncoding.DecodeString(m.PublicKey); err != nil || len(raw) != ed25519.PublicKeySize {
			continue
		}
		signed := m.BaseURL != "" && VerifySwarmMember(m)
		if !signed {
			// Only the sender itself, at the address this instance
			// dialed, may be filed without a signed address.
			if m.InstanceID != signer || m.PublicKey != signerKey || dialURL == "" {
				continue
			}
			m.BaseURL, m.URLSig, m.IssuedAt = dialURL, "", 0
		}
		if !ValidSwarmBaseURL(m.BaseURL) {
			continue
		}
		// A loopback address is only ever one this instance was
		// told to dial itself (a seed or a hand-added address in
		// local development). A member list cannot introduce one.
		if u, err := ParseSwarmURL(m.BaseURL); err == nil && SwarmHostIsLoopback(u.Host) &&
			strings.TrimSuffix(m.BaseURL, "/") != strings.TrimSuffix(dialURL, "/") {
			continue
		}
		written, err := app.Queries.UpsertSwarmPeer(ctx, db.UpsertSwarmPeerParams{
			InstanceID: m.InstanceID, BaseUrl: m.BaseURL, PublicKey: m.PublicKey,
			Source: source, FirstSeen: now,
			UrlSig: m.URLSig, UrlIssuedAt: m.IssuedAt, AddressSigned: signed,
		})
		if err != nil {
			return err
		}
		if written == 0 {
			// Known under another key: refused, and nothing
			// about it changes, approval included.
			app.SwarmFlagKeyConflict(ctx, m.InstanceID, signer, now)
			continue
		}
		if !app.Cfg.Allowlisted() {
			if err := app.Queries.SetSwarmPeerApproved(ctx, db.SetSwarmPeerApprovedParams{InstanceID: m.InstanceID, Approved: true}); err != nil {
				return err
			}
		}
	}
	return nil
}

// SwarmDemandKeys gathers what this instance already wants, for
// peers to serve before the worker spends ESI calls: names
// first (type names readers found missing, structure names the
// resolver has due — few, and cheap to carry), then due history
// pairs, then pending organization records, then public
// contracts whose item lists are still pending. A pull stages only
// what it asked for, so every kind the swarm carries has to be
// asked for here. Bounded, like every read that feeds a render
// or a fetch.
func (app *Mod) SwarmDemandKeys(ctx context.Context) []SwarmPullKey {
	var keys []SwarmPullKey
	for _, id := range app.Deps.MissingTypeNames(MaxSwarmNameDemand) {
		keys = append(keys, SwarmPullKey{Kind: SwarmKindTypeName, Key: fmt.Sprintf("%d", id)})
	}
	now := time.Now().UTC()
	if ids, err := app.Queries.ListStructureResolutions(ctx, db.ListStructureResolutionsParams{
		ResolvedCutoff:  now.Add(-module.StructureRenameWindow),
		MissingCutoff:   now.Add(-module.StructureMissingWindow),
		ResolutionLimit: MaxSwarmNameDemand,
	}); err != nil {
		logging.Errorf("swarm: demand structure names: %v", err)
	} else {
		for _, id := range ids {
			keys = append(keys, SwarmPullKey{Kind: SwarmKindStructure, Key: fmt.Sprintf("%d", id)})
		}
	}
	names := len(keys)
	if candidates, err := app.Deps.HistoryCandidates(ctx); err != nil {
		logging.Errorf("swarm: demand history: %v", err)
	} else {
		for _, key := range candidates {
			if len(keys)-names >= MaxSwarmHistoryDemand {
				break
			}
			if app.Deps.MarketFetchDue(ctx, module.MarketFetchKind("history", key), module.HistoryRefetchGate) {
				keys = append(keys, SwarmPullKey{Kind: SwarmKindHistory,
					Key: fmt.Sprintf("%d/%d", key.RegionID, key.TypeID)})
			}
		}
	}
	if ids, err := app.Queries.ListSwarmCorpDemand(ctx, MaxSwarmOrgDemand); err != nil {
		logging.Errorf("swarm: demand corporations: %v", err)
	} else {
		for _, id := range ids {
			keys = append(keys, SwarmPullKey{Kind: SwarmKindCorporation, Key: fmt.Sprintf("%d", id)})
		}
	}
	if ids, err := app.Queries.ListSwarmAllianceDemand(ctx, MaxSwarmOrgDemand); err != nil {
		logging.Errorf("swarm: demand alliances: %v", err)
	} else {
		for _, id := range ids {
			keys = append(keys, SwarmPullKey{Kind: SwarmKindAlliance, Key: fmt.Sprintf("%d", id)})
		}
	}
	// Contracts whose headers this instance swept but whose item
	// lists it has not fetched: the one contract datum worth
	// asking a peer for, since the header alone cannot be pulled
	// by ID and is already here.
	if rows, err := app.Queries.ListPublicContractsPendingItems(ctx, MaxSwarmContractDemand); err != nil {
		logging.Errorf("swarm: demand contracts: %v", err)
	} else {
		for _, row := range rows {
			keys = append(keys, SwarmPullKey{Kind: SwarmKindContract, Key: fmt.Sprintf("%d", row.ContractID)})
		}
	}
	if len(keys) > MaxSwarmDemandKeys {
		keys = keys[:MaxSwarmDemandKeys]
	}
	return keys
}

// SwarmSampled reports whether one staged entry is ESI-verified
// this pass: the trust draw, defaulting to verified when the
// randomness fails (a missed draw must cost a Call, never
// trust).
func (app *Mod) SwarmSampled(good, bad int64) bool {
	r, err := SwarmRandFloat()
	if err != nil {
		logging.Errorf("swarm: sample randomness: %v", err)
		return true
	}
	return SwarmCheckDraw(SwarmTrust(good, bad), good+bad, r)
}

// SwarmServesWindow is how recently a pull must have staged
// something from a peer for it to stay in the data rotation.
const SwarmServesWindow = 7 * 24 * time.Hour

// SwarmPeerPullable reports whether one listed peer may serve:
// not under the floor once it has enough checks to judge.
// Shared by the cycle pull and the urgent single-key pull so
// both read the same gate. A seed is a member like any other
// and is not singled out here: whether a peer serves data is
// learned from its pulls (ListSwarmPullPeers), not from its
// identity or from where it is configured.
func SwarmPeerPullable(good, bad int64) bool {
	if total := good + bad; total >= SwarmTrustMinChecks && SwarmTrust(good, bad) < SwarmTrustFloor {
		return false
	}
	return true
}

// SwarmPullOne asks approved peers for one key and stages what
// verifies, for latency-sensitive callers (the urgent drain):
// freshest first under the caller's deadline, falling through
// to the next pullable peer when one errors, stopping at the
// first peer that answers. Returns how many entries staged.
// Failures stay silent per attempt; the ESI fallback covers
// them.
func (app *Mod) SwarmPullOne(ctx context.Context, kind, key string) int {
	if !app.Cfg.Enabled() || app.SwarmID == nil || app.SwarmCli == nil {
		return 0
	}
	peers, err := app.Queries.ListSwarmPullPeers(ctx, time.Now().UTC().Add(-SwarmServesWindow))
	if err != nil {
		logging.Errorf("swarm: pull peers: %v", err)
		return 0
	}
	// One key, one deadline: ask whoever served something most
	// recently, not whoever is next in the rotation.
	sort.SliceStable(peers, func(i, j int) bool {
		a, b := peers[i].LastServed, peers[j].LastServed
		if a.Valid != b.Valid {
			return a.Valid
		}
		return a.Valid && a.Time.After(b.Time)
	})
	for _, p := range peers {
		if !SwarmPeerPullable(p.GoodChecks, p.BadChecks) {
			continue
		}
		keys := []SwarmPullKey{{Kind: kind, Key: key}}
		entries, err := app.SwarmCli.PullEntries(ctx, p.BaseUrl, keys)
		if err != nil {
			app.AddSwarmCounter(ctx, SwarmCounterPullsFailed, 1)
			continue
		}
		app.AddSwarmCounter(ctx, SwarmCounterPullsOK, 1)
		return app.SwarmStagePulled(ctx, p.InstanceID, p.BaseUrl, keys, entries, time.Now())
	}
	return 0
}

// SwarmStagePulled stages what one peer answered a pull with,
// and only that: an entry is kept when it names a key this pull
// asked the peer for, is signed by the peer itself, and is the
// first answer for its key. Everything else in the response is
// dropped unread — data moves on request, so a peer cannot hand
// over records nobody asked it for. Entries under another
// signer are skipped without blame (a forwarder cannot spend
// another signer's reputation, or its own on another's lies);
// unasked-for or repeated keys are the serving peer's own doing
// and cost it one flat mark for the response. It returns how
// many entries staged.
func (app *Mod) SwarmStagePulled(ctx context.Context, peerID, baseURL string, asked []SwarmPullKey, entries []SwarmSignedEntry, now time.Time) int {
	waiting := make(map[SwarmPullKey]bool, len(asked))
	for _, k := range asked {
		waiting[k] = true
	}
	staged, unasked := 0, 0
	for _, entry := range entries {
		if entry.Signer != peerID {
			continue
		}
		k := SwarmPullKey{Kind: entry.Kind, Key: entry.Key}
		if !waiting[k] {
			unasked++
			continue
		}
		delete(waiting, k)
		if err := app.StageSwarmEntry(ctx, entry, peerID, now); err != nil {
			logging.Warnf("swarm: pull %s: %v", baseURL, err)
			continue
		}
		staged++
	}
	if staged > 0 {
		if err := app.Queries.TouchSwarmPeerServed(ctx, db.TouchSwarmPeerServedParams{InstanceID: peerID, LastServed: timeSet(now)}); err != nil {
			logging.Errorf("swarm: note %s served: %v", ShortenSwarmID(peerID), err)
		}
	}
	if unasked > 0 {
		logging.Warnf("swarm: pull %s: dropped %d entries the pull did not ask for", baseURL, unasked)
		app.CountSwarmShapeReject(ctx, peerID)
	}
	return staged
}

// SwarmSettleUrgent settles one staged key inline for the
// urgent drain: the trust draw decides. Unchecked entries
// apply on reputation (a spot decision would reach the same
// row within a cycle or two, too late for the viewing page);
// drawn entries verify now against ESI on a one-Call budget,
// spending what the direct fetch would have cost. Anything
// else reports unapplied and the caller fetches from ESI as
// before. Only the direct fetch's absence saves a Call.
func (app *Mod) SwarmSettleUrgent(ctx context.Context, kind, key string) bool {
	row, err := app.Queries.GetStagingEntry(ctx, db.GetStagingEntryParams{Kind: kind, Key: key})
	if err != nil {
		return false
	}
	peer, err := app.Queries.GetSwarmPeer(ctx, row.Signer)
	if err != nil || peer.Excluded {
		return false
	}
	now := time.Now().UTC()
	if !app.SwarmSampled(peer.GoodChecks, peer.BadChecks) {
		// Trust-earned, like the spot pass below: the entry
		// applies on reputation with the ledger untouched.
		// Only an ESI verdict counts for or against.
		if err := app.ApplySwarmEntry(ctx, row.Kind, row.Key, row.Payload, SwarmOrigin{Source: row.Source, Signer: row.Signer, FetchedAt: row.FetchedAt, Verified: false}, now); err != nil {
			logging.Errorf("swarm: apply %s %s: %v", row.Kind, row.Key, err)
			return false
		}
		return true
	}
	verdict, confirmed, _ := app.SwarmVerifyAgainstESI(ctx, &oneFetch{Left: 1}, nil, row.Kind, row.Key, row.Payload)
	switch verdict {
	case SwarmCheckPass:
		if err := app.ApplySwarmEntry(ctx, row.Kind, row.Key, confirmed, SwarmOrigin{Source: row.Source, Signer: row.Signer, FetchedAt: row.FetchedAt, Verified: true}, now); err != nil {
			logging.Errorf("swarm: apply %s %s: %v", row.Kind, row.Key, err)
			return false
		}
		app.CountSwarmCheck(ctx, row.Signer, true)
		app.AddSwarmCounter(ctx, SwarmCounterSpotPassed, 1)
		return true
	case SwarmCheckFail:
		app.DiscardSwarmEntry(ctx, row.Kind, row.Key, row.Signer, SwarmDiscardMismatch)
		return false
	default:
		return false
	}
}

// SwarmUrgentHistory serves one due history want from peers
// before the urgent drain spends ESI: one freshest-peer pull
// under a short deadline, then an inline settle. True when the
// want applied (verified or trust-earned) and the ESI fetch
// may skip; false keeps the existing direct fetch. Peer
// silence costs milliseconds, never correctness.
func (app *Mod) SwarmUrgentHistory(ctx context.Context, key module.MarketKey) bool {
	// Non-swarm instances pay nothing here: no staging
	// lookup, no pull, straight to the direct fetch. Same
	// gate as the fetch suppressor below.
	if !app.Cfg.Enabled() || app.SwarmID == nil {
		return false
	}
	skey := fmt.Sprintf("%d/%d", key.RegionID, key.TypeID)
	pctx, cancel := context.WithTimeout(ctx, SwarmUrgentPullTimeout)
	app.SwarmPullOne(pctx, SwarmKindHistory, skey)
	cancel()
	return app.SwarmSettleUrgent(ctx, SwarmKindHistory, skey)
}

// SwarmSuppressFetch reports whether one due key skips its ESI
// refetch: an unsettled staged entry newer than the window
// means a spot decision is moments away, and refetching under
// it would spend the Call the pull already covered. Older rows
// fall back to ESI (see the window's comment); discards leave
// fetch state untouched, so the key goes due again and the
// suppression can only delay a fetch, never cancel it.
func (app *Mod) SwarmSuppressFetch(ctx context.Context, kind, key string, now time.Time) bool {
	if !app.Cfg.Enabled() || app.SwarmID == nil {
		return false
	}
	row, err := app.Queries.GetStagingEntry(ctx, db.GetStagingEntryParams{Kind: kind, Key: key})
	if err != nil {
		return false
	}
	if now.Sub(row.ReceivedAt) >= SwarmStagedSuppressWindow {
		return false
	}
	// However often the entry is restaged, one key is held back at most
	// SwarmSuppressCap in all: then this instance fetches it itself.
	id := "suppress|" + kind + "|" + key
	app.SwarmCover.Mu.Lock()
	defer app.SwarmCover.Mu.Unlock()
	if app.SwarmCover.Asked == nil {
		app.SwarmCover.Asked = map[string]time.Time{}
	}
	first, seen := app.SwarmCover.Asked[id]
	if !seen {
		app.SwarmCover.Asked[id] = now
		return true
	}
	return now.Sub(first) < SwarmSuppressCap
}

// SwarmSuppressCap bounds how long staged peer entries can hold back
// one key's own fetch, restaged or not.
const SwarmSuppressCap = 2 * time.Minute

// SwarmPull asks approved peers for the wanted keys and stages
// what they answer with, through SwarmStagePulled: only keys
// this pull asked for, only under the serving peer's own pinned
// identity. Only peers known to serve data are asked, in turn,
// a few a cycle; a peer not yet asked gets one probe first. A
// seed that is only a directory answers its probe with nothing
// and is then dialed for membership alone.
func (app *Mod) SwarmPull(ctx context.Context, now time.Time) {
	keys := app.SwarmDemandKeys(ctx)
	if len(keys) == 0 {
		return
	}
	peers, err := app.Queries.ListSwarmPullPeers(ctx, time.Now().UTC().Add(-SwarmServesWindow))
	if err != nil {
		logging.Errorf("swarm: pull peers: %v", err)
		return
	}
	// Dedicated warming nodes are asked every cycle on top of the
	// rotation: the pull is how demand reaches them, and how what
	// they fetched comes back (swarm/warm.go).
	warm := app.SwarmWarmPeerIDs(now)
	// A dedicated node is in the pull set whether or not it served
	// lately: its first answers are empty (it is taking the demand),
	// and dropping it then would strand what it fetches. Its trust
	// was checked when it was asked what it warms.
	listed := map[string]bool{}
	for _, p := range peers {
		listed[p.InstanceID] = true
	}
	for id, base := range app.SwarmWarmPeerURLs(now) {
		if !listed[id] {
			peers = append(peers, db.ListSwarmPullPeersRow{InstanceID: id, BaseUrl: base})
		}
	}
	var picked []db.ListSwarmPullPeersRow
	rotation, warmTaken := 0, 0
	for _, p := range peers {
		if !SwarmPeerPullable(p.GoodChecks, p.BadChecks) {
			continue
		}
		if warm[p.InstanceID] {
			if warmTaken >= MaxSwarmWarmPeers {
				continue
			}
			warmTaken++
		} else {
			if rotation >= MaxSwarmPullPeersPerCycle {
				continue
			}
			rotation++
		}
		picked = append(picked, p)
		// Its turn is taken whether or not it answers, so a dead
		// peer goes to the back of the line like any other.
		if err := app.Queries.TouchSwarmPeerPulled(ctx, db.TouchSwarmPeerPulledParams{InstanceID: p.InstanceID, LastPulled: timeSet(now)}); err != nil {
			logging.Errorf("swarm: note pull of %s: %v", ShortenSwarmID(p.InstanceID), err)
		}
	}
	// Asked a few at a time under one deadline (swarm/fanout.go);
	// what they answer is staged one at a time, in rotation order.
	type answer struct {
		entries []SwarmSignedEntry
		Err     error
	}
	answers := make([]answer, len(picked))
	SwarmFanOut(ctx, len(picked), func(rctx context.Context, i int) {
		answers[i].entries, answers[i].Err = app.SwarmCli.PullEntries(rctx, picked[i].BaseUrl, keys)
	})
	for i, p := range picked {
		if answers[i].Err != nil {
			app.AddSwarmCounter(ctx, SwarmCounterPullsFailed, 1)
			continue
		}
		app.AddSwarmCounter(ctx, SwarmCounterPullsOK, 1)
		app.SwarmStagePulled(ctx, p.InstanceID, p.BaseUrl, keys, answers[i].entries, now)
	}
}

// SwarmSpotCheck verifies staged entries against ESI and applies
// the ones that pass — the only path anything staged reaches
// the real tables. At most a few ESI calls a cycle, from the
// shared allowance; ESI's stop signal pauses the pass like any
// other. Only an entry ESI contradicts is a failure: it is
// discarded and counted against its signer. Anything
// undecidable (no scoped character, no span shared with ESI's
// history, a transient error) waits for another pass, at most
// MaxSwarmDeferrals times, and is then dropped; so is an entry
// past the staging age cap, or from a signer under the trust
// floor. A drop is neutral: no mark, no streak, its own counter.
func (app *Mod) SwarmSpotCheck(ctx context.Context, allowance module.Budget, characters []db.Character, now time.Time) (applied int, limited bool) {
	rows, err := app.Queries.ListUncheckedStaging(ctx, 25)
	if err != nil {
		logging.Errorf("swarm: list staged: %v", err)
		return 0, false
	}
	checks := 0
	for _, row := range rows {
		if now.Sub(row.ReceivedAt) > MaxSwarmStagingAge {
			app.DropSwarmEntry(ctx, row.Kind, row.Key, row.Signer, SwarmDropAgedOut)
			continue
		}
		peer, err := app.Queries.GetSwarmPeer(ctx, row.Signer)
		if err != nil || peer.Excluded {
			if err := app.Queries.DeleteStagingEntry(ctx, db.DeleteStagingEntryParams{Kind: row.Kind, Key: row.Key}); err != nil {
				logging.Errorf("swarm: drop staged %s %s: %v", row.Kind, row.Key, err)
			}
			continue
		}
		if total := peer.GoodChecks + peer.BadChecks; total >= SwarmTrustMinChecks && SwarmTrust(peer.GoodChecks, peer.BadChecks) < SwarmTrustFloor {
			app.DropSwarmEntry(ctx, row.Kind, row.Key, row.Signer, SwarmDropLowTrust)
			continue
		}
		if !app.SwarmSampled(peer.GoodChecks, peer.BadChecks) {
			// Trust-earned: signature and shape already
			// verified at staging, and the sample skipped
			// ESI. The entry applies on reputation, but an
			// unchecked apply is neither a good check nor a
			// bad one: only ESI verdicts move trust (or the
			// failure streak), so the ledger stays
			// untouched. The per-kind applied counters
			// inside ApplySwarmEntry record the served
			// volume.
			if err := app.ApplySwarmEntry(ctx, row.Kind, row.Key, row.Payload, SwarmOrigin{Source: row.Source, Signer: row.Signer, FetchedAt: row.FetchedAt, Verified: false}, now); err != nil {
				logging.Errorf("swarm: apply %s %s: %v", row.Kind, row.Key, err)
				continue
			}
			applied++
			continue
		}
		if !SwarmCheckable(row.Kind, characters) {
			// Nobody here can check this kind right now (a
			// structure name with no character holding the
			// scope). Trying would decide nothing, so it is not
			// tried and takes none of the cycle's check slots:
			// it counts as a pass that could not decide, and
			// the slots go to entries that can be checked.
			app.SwarmDeferStaged(ctx, row.Kind, row.Key, row.Signer)
			continue
		}
		if checks >= MaxSwarmSpotPerCycle || allowance.Exhausted() {
			// Out of checks or out of calls: the rest wait,
			// and waiting is not a deferral.
			break
		}
		checks++
		verdict, confirmed, stop := app.SwarmVerifyAgainstESI(ctx, allowance, characters, row.Kind, row.Key, row.Payload)
		if stop {
			return applied, true
		}
		switch verdict {
		case SwarmCheckPass:
			if err := app.ApplySwarmEntry(ctx, row.Kind, row.Key, confirmed, SwarmOrigin{Source: row.Source, Signer: row.Signer, FetchedAt: row.FetchedAt, Verified: true}, now); err != nil {
				logging.Errorf("swarm: apply %s %s: %v", row.Kind, row.Key, err)
				continue
			}
			app.CountSwarmCheck(ctx, row.Signer, true)
			app.AddSwarmCounter(ctx, SwarmCounterSpotPassed, 1)
			applied++
		case SwarmCheckFail:
			app.DiscardSwarmEntry(ctx, row.Kind, row.Key, row.Signer, SwarmDiscardMismatch)
		default:
			// Deferred: no scoped character, no span shared
			// with ESI's history, a transient error. The row
			// waits for another pass, a few times; one that
			// still cannot be decided is dropped, with no
			// mark on its signer, rather than taking a check
			// slot every cycle until the age cap.
			app.SwarmDeferStaged(ctx, row.Kind, row.Key, row.Signer)
		}
	}
	return applied, false
}

// SwarmDeferStaged notes one pass that could not decide a staged
// entry, and drops the entry (neutrally) once it has had
// MaxSwarmDeferrals of them.
func (app *Mod) SwarmDeferStaged(ctx context.Context, kind, key, signer string) {
	n, err := app.Queries.BumpStagingDeferral(ctx, db.BumpStagingDeferralParams{Kind: kind, Key: key})
	if err != nil {
		logging.Errorf("swarm: defer %s %s: %v", kind, key, err)
		return
	}
	if n >= MaxSwarmDeferrals {
		app.DropSwarmEntry(ctx, kind, key, signer, SwarmDropUndecidable)
	}
}

// SwarmStructureChecker picks a character that can re-fetch a
// structure name: one that syncs and holds the structure scope.
// Nil when there is none.
func SwarmStructureChecker(characters []db.Character) *db.Character {
	for i, ch := range characters {
		if characterSyncs(ch) && module.CharacterHasScope(ch, module.StructureScope) {
			return &characters[i]
		}
	}
	return nil
}

// SwarmCheckable reports whether this instance could check an
// entry of this kind against ESI at all right now. Everything
// public always can; a structure name needs a scoped character.
func SwarmCheckable(kind string, characters []db.Character) bool {
	return kind != SwarmKindStructure || SwarmStructureChecker(characters) != nil
}

// SwarmCheckVerdict is what re-fetching against ESI says about
// one staged entry.
type SwarmCheckVerdict int

const (
	SwarmCheckDefer SwarmCheckVerdict = iota
	SwarmCheckPass
	SwarmCheckFail
)

// SwarmVerifyAgainstESI re-fetches one staged entry's datum from
// ESI and compares the slow-moving truth: the whole shared span
// for history, the name for names, identity fields for records.
// On a pass, confirmed is the payload to apply: the entry's own
// for most kinds, and for history only the days ESI confirmed.
// stop reports ESI's error-limit signal. Anything less than a
// clean fetch is deferred, never failed — only ESI's own answer
// convicts.
func (app *Mod) SwarmVerifyAgainstESI(ctx context.Context, allowance module.Budget, characters []db.Character, kind, key, payload string) (verdict SwarmCheckVerdict, confirmed string, stop bool) {
	switch kind {
	case SwarmKindHistory:
		return app.SwarmVerifyHistory(ctx, allowance, key, payload)
	case SwarmKindTypeName:
		verdict, stop = app.SwarmVerifyTypeName(ctx, allowance, key, payload)
	case SwarmKindCorporation:
		verdict, stop = app.SwarmVerifyCorporation(ctx, allowance, key, payload)
	case SwarmKindAlliance:
		verdict, stop = app.SwarmVerifyAlliance(ctx, allowance, key, payload)
	case SwarmKindStructure:
		verdict, stop = app.SwarmVerifyStructure(ctx, allowance, characters, key, payload)
	case SwarmKindContract:
		verdict, stop = app.SwarmVerifyContract(ctx, allowance, key, payload)
	default:
		return SwarmCheckFail, "", false
	}
	return verdict, payload, stop
}

// SwarmTake budgets one spot-check Call: false means the cycle
// is out of allowance and the row waits.
func (app *Mod) SwarmTake(allowance module.Budget) bool {
	return allowance.Take()
}

// SwarmVerifyHistory checks one staged history entry against a
// fresh ESI fetch, over the whole span the two share — not just
// the days that happen to appear in both. Inside that span the
// day sets must be identical and every figure equal: a staged
// day ESI does not have was made up, and a day ESI has that the
// entry leaves out was dropped. Days outside the span (ones ESI
// has since let go of, or has not served this instance yet)
// cannot be checked, so they are not applied: the payload
// handed back holds only the days ESI confirmed. No shared span
// verifies nothing and the entry waits.
func (app *Mod) SwarmVerifyHistory(ctx context.Context, allowance module.Budget, key, payload string) (SwarmCheckVerdict, string, bool) {
	parts := strings.Split(key, "/")
	if len(parts) != 2 {
		return SwarmCheckFail, "", false
	}
	regionID, ok := ParseSwarmID(parts[0])
	if !ok {
		return SwarmCheckFail, "", false
	}
	typeID, ok := ParseSwarmID(parts[1])
	if !ok {
		return SwarmCheckFail, "", false
	}
	var staged SwarmHistoryPayload
	if err := DecodeSwarmStrict(strings.NewReader(payload), &staged); err != nil || len(staged.Days) == 0 {
		return SwarmCheckFail, "", false
	}
	if !app.SwarmTake(allowance) {
		return SwarmCheckDefer, "", false
	}
	rows, err := app.Deps.FetchMarketHistory(ctx, module.MarketKey{RegionID: regionID, TypeID: typeID})
	if err != nil {
		if errors.Is(err, esi.ErrErrorLimit) {
			return SwarmCheckDefer, "", true
		}
		return SwarmCheckDefer, "", false
	}
	if len(rows) == 0 {
		return SwarmCheckDefer, "", false
	}
	// Dates are YYYY-MM-DD (checked at staging), so they order as text.
	fresh := make(map[string]esi.MarketHistoryDay, len(rows))
	lo, hi := rows[0].Date, rows[0].Date
	for _, row := range rows {
		fresh[row.Date] = row
		lo, hi = min(lo, row.Date), max(hi, row.Date)
	}
	stagedLo, stagedHi := staged.Days[0].Date, staged.Days[0].Date
	for _, day := range staged.Days {
		stagedLo, stagedHi = min(stagedLo, day.Date), max(stagedHi, day.Date)
	}
	lo, hi = max(lo, stagedLo), min(hi, stagedHi)
	if lo > hi {
		return SwarmCheckDefer, "", false
	}
	confirmed := SwarmHistoryPayload{RegionID: staged.RegionID, TypeID: staged.TypeID}
	for _, day := range staged.Days {
		if day.Date < lo || day.Date > hi {
			continue
		}
		got, ok := fresh[day.Date]
		if !ok {
			logging.Warnf("swarm: check %s %s: the entry has %s, which ESI does not", SwarmKindHistory, key, day.Date)
			return SwarmCheckFail, "", false
		}
		if got.Average != day.Average || got.Highest != day.Highest || got.Lowest != day.Lowest ||
			got.Volume != day.Volume || got.OrderCount != day.OrderCount {
			logging.Warnf("swarm: check %s %s: figures for %s differ from ESI", SwarmKindHistory, key, day.Date)
			return SwarmCheckFail, "", false
		}
		confirmed.Days = append(confirmed.Days, day)
		delete(fresh, day.Date)
	}
	for date := range fresh {
		if date >= lo && date <= hi {
			logging.Warnf("swarm: check %s %s: the entry leaves out %s, which ESI has", SwarmKindHistory, key, date)
			return SwarmCheckFail, "", false
		}
	}
	raw, err := json.Marshal(confirmed)
	if err != nil {
		return SwarmCheckDefer, "", false
	}
	return SwarmCheckPass, string(raw), false
}

func (app *Mod) SwarmVerifyTypeName(ctx context.Context, allowance module.Budget, key, payload string) (SwarmCheckVerdict, bool) {
	typeID, ok := ParseSwarmID(key)
	if !ok {
		return SwarmCheckFail, false
	}
	var staged SwarmTypeNamePayload
	if err := DecodeSwarmStrict(strings.NewReader(payload), &staged); err != nil || staged.TypeID != typeID {
		return SwarmCheckFail, false
	}
	if !app.SwarmTake(allowance) {
		return SwarmCheckDefer, false
	}
	var t esi.Type
	if err := app.Deps.ESIGet(ctx, fmt.Sprintf("/universe/types/%d/", typeID), &t); err != nil {
		if errors.Is(err, esi.ErrErrorLimit) {
			return SwarmCheckDefer, true
		}
		if code, has := esi.StatusCode(err); has && code == 404 {
			return SwarmCheckFail, false
		}
		return SwarmCheckDefer, false
	}
	if t.Name != staged.Name {
		return SwarmCheckFail, false
	}
	return SwarmCheckPass, false
}

func (app *Mod) SwarmVerifyCorporation(ctx context.Context, allowance module.Budget, key, payload string) (SwarmCheckVerdict, bool) {
	id, ok := ParseSwarmID(key)
	if !ok {
		return SwarmCheckFail, false
	}
	var staged SwarmOrgPayload
	if err := DecodeSwarmStrict(strings.NewReader(payload), &staged); err != nil || staged.ID != id {
		return SwarmCheckFail, false
	}
	// The record travels in either dialect — this app's wrapped
	// shape or a bare ESI corporation — so it is read through
	// the same tolerant decoder the apply path uses, and the
	// fields compared are the ones actually sent. A body naming
	// no corporation under either dialect is a shape problem,
	// not a lie ESI can contradict: it waits and drops without
	// a mark rather than failing its signer.
	body, ok := module.DecodeCorpPayload(staged.Payload)
	if !ok {
		return SwarmCheckDefer, false
	}
	if !app.SwarmTake(allowance) {
		return SwarmCheckDefer, false
	}
	// Identity fields only: names, tickers and alliance homes
	// move slowly; member counts and descriptions drift daily
	// and are never evidence.
	var corp esi.Corporation
	if err := app.Deps.ESIGet(ctx, fmt.Sprintf("/corporations/%d/", id), &corp); err != nil {
		if errors.Is(err, esi.ErrErrorLimit) {
			return SwarmCheckDefer, true
		}
		if code, has := esi.StatusCode(err); has && code == 404 {
			return SwarmCheckFail, false
		}
		return SwarmCheckDefer, false
	}
	if corp.Name != body.Corp.Name || corp.Ticker != body.Corp.Ticker || corp.AllianceID != body.Corp.AllianceID {
		return SwarmCheckFail, false
	}
	return SwarmCheckPass, false
}

func (app *Mod) SwarmVerifyAlliance(ctx context.Context, allowance module.Budget, key, payload string) (SwarmCheckVerdict, bool) {
	id, ok := ParseSwarmID(key)
	if !ok {
		return SwarmCheckFail, false
	}
	var staged SwarmOrgPayload
	if err := DecodeSwarmStrict(strings.NewReader(payload), &staged); err != nil || staged.ID != id {
		return SwarmCheckFail, false
	}
	// Either dialect, as for corporations: read through the
	// tolerant decoder, and a body naming no alliance under
	// either dialect defers rather than fails.
	body, ok := module.DecodeAlliancePayload(staged.Payload)
	if !ok {
		return SwarmCheckDefer, false
	}
	if !app.SwarmTake(allowance) {
		return SwarmCheckDefer, false
	}
	var alliance esi.Alliance
	if err := app.Deps.ESIGet(ctx, fmt.Sprintf("/alliances/%d/", id), &alliance); err != nil {
		if errors.Is(err, esi.ErrErrorLimit) {
			return SwarmCheckDefer, true
		}
		if code, has := esi.StatusCode(err); has && code == 404 {
			return SwarmCheckFail, false
		}
		return SwarmCheckDefer, false
	}
	if alliance.Name != body.Alliance.Name || alliance.Ticker != body.Alliance.Ticker {
		return SwarmCheckFail, false
	}
	return SwarmCheckPass, false
}

func (app *Mod) SwarmVerifyStructure(ctx context.Context, allowance module.Budget, characters []db.Character, key, payload string) (SwarmCheckVerdict, bool) {
	id, ok := ParseSwarmID(key)
	if !ok {
		return SwarmCheckFail, false
	}
	var staged SwarmStructurePayload
	if err := DecodeSwarmStrict(strings.NewReader(payload), &staged); err != nil || staged.StructureID != id {
		return SwarmCheckFail, false
	}
	// Structures need a scoped character to re-fetch; with none
	// linked the row waits rather than applying blind.
	scoped := SwarmStructureChecker(characters)
	if scoped == nil {
		return SwarmCheckDefer, false
	}
	if !app.SwarmTake(allowance) {
		return SwarmCheckDefer, false
	}
	// A structure this instance cannot see is not evidence of a
	// lie (access differs per character): only a different name
	// convicts.
	live, err := app.Deps.FetchStructure(ctx, *scoped, id)
	if err != nil {
		if errors.Is(err, esi.ErrErrorLimit) {
			return SwarmCheckDefer, true
		}
		return SwarmCheckDefer, false
	}
	if live.Name != staged.Name {
		return SwarmCheckFail, false
	}
	return SwarmCheckPass, false
}

// swarmPtrEqual reports whether two optional blueprint figures
// agree: both absent, or both present with the same value.
func swarmPtrEqual(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// SwarmVerifyContract checks one staged contract entry against
// the public items route. Only the items are compared — they
// are the contents this kind exists to move. The header fields
// are deliberately not compared: the demanding instance already
// holds the header from its own sweep (that is what made the
// contract pending and worth demanding), and the signer's copy
// of it adds nothing ESI could settle here.
//
// The item sets must match exactly. A 404 defers — the contract
// ended, and an honest entry cannot be convicted by its expiry —
// while a 400 fails: contents the public route refuses (courier
// cargo claimed as an item list, say) are not public data, and
// an entry carrying them is wrong however it was signed.
func (app *Mod) SwarmVerifyContract(ctx context.Context, allowance module.Budget, key, payload string) (SwarmCheckVerdict, bool) {
	id, ok := ParseSwarmID(key)
	if !ok {
		return SwarmCheckFail, false
	}
	var staged SwarmContractPayload
	if err := DecodeSwarmStrict(strings.NewReader(payload), &staged); err != nil || staged.ContractID != id {
		return SwarmCheckFail, false
	}
	if !app.SwarmTake(allowance) {
		return SwarmCheckDefer, false
	}
	var live []esi.PublicContractItem
	if err := app.Deps.ESIGet(ctx, fmt.Sprintf("/contracts/public/items/%d/", id), &live); err != nil {
		if errors.Is(err, esi.ErrErrorLimit) {
			return SwarmCheckDefer, true
		}
		if code, has := esi.StatusCode(err); has {
			switch code {
			case 404:
				return SwarmCheckDefer, false
			case 400:
				return SwarmCheckFail, false
			}
		}
		return SwarmCheckDefer, false
	}
	if len(live) != len(staged.Items) {
		return SwarmCheckFail, false
	}
	byRecord := make(map[int64]esi.PublicContractItem, len(live))
	for _, item := range live {
		byRecord[item.RecordID] = item
	}
	if len(byRecord) != len(live) {
		return SwarmCheckFail, false
	}
	for _, item := range staged.Items {
		got, ok := byRecord[item.RecordID]
		if !ok {
			return SwarmCheckFail, false
		}
		// ESI's item_id 0 and the wire's null are the same
		// fact: no singleton ID.
		if item.ItemID == nil {
			if got.ItemID != 0 {
				return SwarmCheckFail, false
			}
		} else if *item.ItemID != got.ItemID {
			return SwarmCheckFail, false
		}
		if got.TypeID != item.TypeID || got.Quantity != item.Quantity ||
			got.IsIncluded != item.IsIncluded || got.IsBlueprintCopy != item.IsBlueprintCopy ||
			!swarmPtrEqual(got.Runs, item.Runs) ||
			!swarmPtrEqual(got.MaterialEfficiency, item.MaterialEfficiency) ||
			!swarmPtrEqual(got.TimeEfficiency, item.TimeEfficiency) {
			return SwarmCheckFail, false
		}
	}
	return SwarmCheckPass, false
}
