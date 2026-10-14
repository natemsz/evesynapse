package app

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
	swarmTrustPriorGood = 9
	swarmTrustPriorBad  = 1
	swarmTrustFloor     = 0.5
	swarmTrustMinChecks = 10

	swarmSampleMin = 0.05
	swarmSampleMax = 0.5

	swarmStreakWeightCap = 10
	swarmStreakExclude   = 5
)

// swarmStagedSuppressWindow bounds the coordination between
// the pull and the passes: a key with an unsettled staged
// entry newer than this skips its ESI refetch, because a spot
// decision is moments away. Older rows fall back to ESI: the
// spot pass evidently is not settling them, and pages must
// not rot behind a stuck row. Discards re-mark the key due by
// leaving fetch state untouched, so suppression can only
// delay a fetch, never cancel it.
const swarmStagedSuppressWindow = 10 * time.Minute

// swarmUrgentPullTimeout bounds one peer pull inside the
// urgent drain: a hung peer must not wedge the seconds-scale
// tick behind it (the drain holds the shared fetch lock).
const swarmUrgentPullTimeout = 3 * time.Second

// Per-cycle bounds: spot checks are ESI calls (from the shared
// allowance), pulls are peer HTTP (bounded anyway), and gossip
// touches a few peers a day.
const (
	maxSwarmSpotPerCycle      = 5
	maxSwarmPullPeersPerCycle = 3
	maxSwarmDemandKeys        = 50
	maxSwarmGossipPeers       = 10
	maxSwarmHistoryDemand     = 30
	maxSwarmOrgDemand         = 10
	maxSwarmNameDemand        = 5 // per name kind
)

// Staged entries older than this never apply: an entry nobody
// could verify within a week is stale, not evidence.
const maxSwarmStagingAge = 7 * 24 * time.Hour

// maxSwarmDeferrals is how many passes may fail to decide a
// staged entry before it is dropped.
const maxSwarmDeferrals = 3

// Sync-page counter names.
const (
	swarmCounterAppliedHistory   = "applied_history"
	swarmCounterAppliedNames     = "applied_names"
	swarmCounterAppliedRecords   = "applied_records"
	swarmCounterSpotPassed       = "spot_passed"
	swarmCounterSpotFailed       = "spot_failed"
	swarmCounterPullsOK          = "pulls_ok"
	swarmCounterPullsFailed      = "pulls_failed"
	swarmCounterRolledBack       = "rolled_back"
	swarmCounterDroppedUnchecked = "dropped_unchecked"
)

// swarmTrust scores a signer from its ledger: good over total
// with the prior, so the first checks move it little and
// sustained behavior moves it a lot.
func swarmTrust(good, bad int64) float64 {
	return float64(good+swarmTrustPriorGood) / float64(good+bad+swarmTrustPriorGood+swarmTrustPriorBad)
}

// addSwarmCounter bumps one Sync-page counter (best effort,
// like the snapshot fetch-state log).
func (app *Application) addSwarmCounter(ctx context.Context, name string, n int64) {
	if err := app.queries.AddSwarmCounter(ctx, db.AddSwarmCounterParams{Name: name, Value: n}); err != nil {
		logging.Errorf("swarm: counter %s: %v", name, err)
	}
}

// swarmCheckDraw reports whether one entry is ESI-verified:
// always while the signer has fewer than ten total checks,
// then with probability falling from half to five percent as
// trust rises. r must be uniform in [0,1); the caller draws it
// unpredictably per entry (swarmRandFloat), never per batch.
func swarmCheckDraw(trust float64, total int64, r float64) bool {
	if total < swarmTrustMinChecks {
		return true
	}
	p := 1 - trust
	if p < swarmSampleMin {
		p = swarmSampleMin
	}
	if p > swarmSampleMax {
		p = swarmSampleMax
	}
	return r < p
}

// swarmRandFloat draws one uniform float in [0,1) from the
// operating system's randomness: a malicious peer observing
// outcomes cannot predict the next draw and poison around it.
// A variable (not a call) so tests pin deterministic draws;
// production never reassigns it.
var swarmRandFloat = func() (float64, error) {
	var b [8]byte
	if _, err := cryptorand.Read(b[:]); err != nil {
		return 0, err
	}
	return float64(binary.BigEndian.Uint64(b[:])) / 18446744073709551616.0, nil
}

// swarmStreakBump advances a signer's consecutive-failure run:
// any success resets it to zero, any failure extends it and
// reports the ledger weight for this failure (doubling per
// consecutive failure to a cap). In-memory only, alongside the
// streak map the struct comment describes.
func (app *Application) swarmStreakBump(signer string, good bool) (streak int, weight int64) {
	app.swarmStreakMu.Lock()
	defer app.swarmStreakMu.Unlock()
	if app.swarmStreak == nil {
		app.swarmStreak = make(map[string]int)
	}
	if good {
		delete(app.swarmStreak, signer)
		return 0, 1
	}
	streak = app.swarmStreak[signer] + 1
	app.swarmStreak[signer] = streak
	shift := streak - 1
	if shift > swarmStreakWeightCap {
		shift = swarmStreakWeightCap
	}
	return streak, int64(1) << uint(shift)
}

// countSwarmCheck ledgers one ESI-verified data verdict (a
// spot check or an inline urgent one): bad verdicts weigh by
// consecutiveness, then exclusion delegates to
// swarmEnforceTrust with the streak, the same single
// implementation every ledger path shares.
func (app *Application) countSwarmCheck(ctx context.Context, signer string, good bool) {
	streak, weight := app.swarmStreakBump(signer, good)
	var goodN, badN int64
	if good {
		goodN = 1
	} else {
		badN = weight
	}
	if err := app.queries.AddSwarmPeerCheck(ctx, db.AddSwarmPeerCheckParams{InstanceID: signer, GoodChecks: goodN, BadChecks: badN}); err != nil {
		logging.Errorf("swarm: ledger check for %s: %v", signer, err)
		return
	}
	app.swarmEnforceTrust(ctx, signer, streak)
}

// countSwarmShapeReject ledgers one staging reject (a bad
// signature, a malformed payload, an unknown signer): flat,
// streak untouched. Rejects never applied anything, so they
// read as a broken peer, not a poisoning one.
func (app *Application) countSwarmShapeReject(ctx context.Context, signer string) {
	if err := app.queries.AddSwarmPeerCheck(ctx, db.AddSwarmPeerCheckParams{InstanceID: signer, GoodChecks: 0, BadChecks: 1}); err != nil {
		logging.Errorf("swarm: ledger reject for %s: %v", signer, err)
		return
	}
	app.swarmEnforceTrust(ctx, signer, 0)
}

// swarmEnforceTrust excludes a signer that earned it: five
// consecutive data failures whatever the lifetime score, or
// enough lifetime checks under the floor. Streak zero (shape
// rejects, or anything without velocity) can only trip the
// floor rule.
func (app *Application) swarmEnforceTrust(ctx context.Context, signer string, streak int) {
	peer, err := app.queries.GetSwarmPeer(ctx, signer)
	if err != nil || peer.Excluded {
		return
	}
	if streak >= swarmStreakExclude {
		note := fmt.Sprintf("auto: %d consecutive failures", streak)
		if err := app.queries.SetSwarmPeerExcluded(ctx, db.SetSwarmPeerExcludedParams{InstanceID: signer, Excluded: true, ExcludeNote: note}); err != nil {
			logging.Errorf("swarm: auto-exclude %s: %v", signer, err)
			return
		}
		logging.Warnf("swarm: excluded %s (%s)", signer, note)
		return
	}
	total := peer.GoodChecks + peer.BadChecks
	if total >= swarmTrustMinChecks && swarmTrust(peer.GoodChecks, peer.BadChecks) < swarmTrustFloor {
		note := fmt.Sprintf("auto: trust %.2f over %d checks below %.2f", swarmTrust(peer.GoodChecks, peer.BadChecks), total, swarmTrustFloor)
		if err := app.queries.SetSwarmPeerExcluded(ctx, db.SetSwarmPeerExcludedParams{InstanceID: signer, Excluded: true, ExcludeNote: note}); err != nil {
			logging.Errorf("swarm: auto-exclude %s: %v", signer, err)
			return
		}
		logging.Warnf("swarm: excluded %s (%s)", signer, note)
	}
}

// swarmFetchPhase is the worker's early swarm pass: gossip when
// due, then pull what is wanted. It spends no ESI calls (peer
// HTTP only), so the cycle runs it ahead of its own fetch
// passes: demand is genuine at pull time, and anything a peer
// serves settles the fetch state before the worker spends an
// ESI call on the same datum. The spot checks stay in the later
// swarmCycle — they spend ESI, so they wait behind the passes
// whose pages are watching.
func (app *Application) swarmFetchPhase(ctx context.Context, now time.Time) {
	if !app.cfg.swarm.enabled() || app.swarmID == nil || app.swarmCli == nil {
		return
	}
	app.swarmGossip(ctx, now)
	app.swarmPull(ctx, now)
}

// swarmCycle is the worker's later swarm pass: take back what
// excluded signers supplied, then spot-check what is staged.
// Gossip and the pull belong to swarmFetchPhase alone, once a
// cycle: a second round here would ask every peer the same
// questions again minutes later. Only the spot checks spend
// ESI calls, from the cycle's allowance; limited reports ESI's
// stop signal like every other pass.
func (app *Application) swarmCycle(ctx context.Context, allowance *fetchBudget, characters []db.Character, now time.Time) (applied int, limited bool) {
	if !app.cfg.swarm.enabled() || app.swarmID == nil || app.swarmCli == nil {
		return 0, false
	}
	// Before anything new applies, take back what excluded
	// signers already supplied (local writes, no ESI).
	app.swarmRollBack(ctx)
	return app.swarmSpotCheck(ctx, allowance, characters, now)
}

// swarmGossipDue reports whether the membership round runs:
// when no round has run yet (bootstrap), or once the time drawn
// at the end of the last round has come. That time is read from
// the database on first use, so a restart keeps the schedule
// instead of gossiping at once.
func (app *Application) swarmGossipDue(ctx context.Context, now time.Time) bool {
	app.swarmMu.Lock()
	defer app.swarmMu.Unlock()
	if !app.swarmNextLoaded {
		// No identity row, or no time drawn yet, reads as due.
		if next, err := app.queries.GetSwarmNextGossip(ctx); err == nil && next.Valid {
			app.swarmNextGossip = next.Time
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
			logging.Errorf("swarm: read next gossip time: %v", err)
		}
		app.swarmNextLoaded = true
	}
	return app.swarmNextGossip.IsZero() || !now.Before(app.swarmNextGossip)
}

// swarmGossipDone ends a round by drawing the next one's time:
// the interval, jittered ±25% so a whole swarm does not gossip
// in lockstep. The draw happens once, here, and the result is
// kept: drawing afresh on every check would fire at the first
// check to draw low, bunching every round near the early edge.
func (app *Application) swarmGossipDone(ctx context.Context, now time.Time) {
	interval := time.Duration(app.cfg.swarm.gossipHours) * time.Hour
	jitter := interval / 4
	next := now.Add(interval - jitter + time.Duration(rand.Int64N(int64(2*jitter+1))))
	app.swarmMu.Lock()
	app.swarmNextGossip = next
	app.swarmNextLoaded = true
	app.swarmMu.Unlock()
	if err := app.queries.SetSwarmNextGossip(ctx, timeSet(next)); err != nil {
		logging.Errorf("swarm: keep next gossip time: %v", err)
	}
}

// swarmGossip exchanges membership with seeds and known peers.
// Learned members are recorded (trust on first use, behind the
// shared secret); approval follows the mode — open trusts every
// secret-holder, allowlist waits for the operator. Excluded
// peers are neither contacted nor propagated.
func (app *Application) swarmGossip(ctx context.Context, now time.Time) {
	if !app.swarmGossipDue(ctx, now) {
		return
	}
	defer app.swarmGossipDone(ctx, now)
	type contact struct {
		url, id, key string
	}
	var contacts []contact
	for _, seed := range app.cfg.swarm.seeds {
		contacts = append(contacts, contact{url: seed})
	}
	peers, err := app.queries.ListSwarmPeers(ctx)
	if err != nil {
		logging.Errorf("swarm: gossip peers: %v", err)
		return
	}
	n := 0
	for _, p := range peers {
		if p.Excluded || p.InstanceID == app.swarmID.id {
			continue
		}
		if n >= maxSwarmGossipPeers {
			break
		}
		n++
		contacts = append(contacts, contact{url: p.BaseUrl, id: p.InstanceID, key: p.PublicKey})
	}
	// The lists are fetched a few at a time under one deadline for
	// the round (swarm_fanout.go), then learned one at a time.
	type heard struct {
		members     []swarmMember
		signer, key string
		err         error
	}
	results := make([]heard, len(contacts))
	swarmFanOut(ctx, len(contacts), func(rctx context.Context, i int) {
		c, r := contacts[i], &results[i]
		r.members, r.signer, r.key, r.err = app.swarmFetchMembers(rctx, c.url, c.id, c.key)
	})
	learned := 0
	for i, c := range contacts {
		source := "gossip"
		if c.id == "" {
			source = "seed"
		}
		r := results[i]
		if app.swarmAbsorbMembers(ctx, c.url, c.id, source, r.members, r.signer, r.key, r.err, now) == nil {
			learned++
		}
	}
	if learned > 0 {
		logging.Infof("swarm: gossip heard from %d peer(s)", learned)
	}
	// Every round ends with one heartbeat per seed, so the
	// directory keeps a fresh record of this instance even
	// when nobody has pulled its member list lately.
	app.swarmAnnounceToSeeds(ctx, now)
	// Forgiveness on the gossip cadence: old bad marks fade
	// whether or not new checks land, so a fixed peer's score
	// recovers over weeks. Exclusion itself never clears here.
	if err := app.queries.DecaySwarmBadChecks(ctx); err != nil {
		logging.Errorf("swarm: decay bad marks: %v", err)
	}
}

// swarmContactPeer asks one address for its membership list and
// records what it learns: the one gossip round, shared by seeds,
// known peers, and operator-added addresses alike. A known peer
// must serve under its pinned identity; an address with no
// pinned key yet is trust-on-first-use — the shared secret
// already proved membership, and the key proves possession from
// here on. Nil means the address answered and its list was
// learned; errSwarmKeyChanged means it answered as an instance
// already pinned under another key, and taught nothing.
func (app *Application) swarmContactPeer(ctx context.Context, baseURL, peerID, peerKey, source string, now time.Time) error {
	members, signer, key, err := app.swarmFetchMembers(ctx, baseURL, peerID, peerKey)
	return app.swarmAbsorbMembers(ctx, baseURL, peerID, source, members, signer, key, err, now)
}

// swarmAbsorbMembers records what one address answered a members
// request with (fetchErr when it did not answer): the second half of
// swarmContactPeer, kept apart so the gossip round can fetch in
// parallel and record one at a time.
func (app *Application) swarmAbsorbMembers(ctx context.Context, baseURL, peerID, source string, members []swarmMember, signer, key string, fetchErr error, now time.Time) error {
	if fetchErr != nil {
		return fetchErr
	}
	if peerID != "" && signer != peerID {
		return fmt.Errorf("swarm: %s answered as another instance", baseURL)
	}
	if err := app.swarmLearnMembers(ctx, members, signer, key, baseURL, source, now); err != nil {
		if !errors.Is(err, errSwarmKeyChanged) {
			logging.Errorf("swarm: learn members via %s: %v", baseURL, err)
		}
		return err
	}
	if err := app.queries.TouchSwarmPeerGossip(ctx, db.TouchSwarmPeerGossipParams{InstanceID: signer, LastGossip: timeSet(now)}); err != nil {
		logging.Errorf("swarm: touch gossip %s: %v", signer, err)
	}
	// It answered: if it is not known to serve data, the next
	// pull may probe it once more. Membership is slow, so this
	// costs a directory one pull a round, not one a cycle.
	if err := app.queries.RearmSwarmPeerProbe(ctx, db.RearmSwarmPeerProbeParams{
		InstanceID: signer, ServedSince: now.Add(-swarmServesWindow),
	}); err != nil {
		logging.Errorf("swarm: re-arm probe of %s: %v", shortenSwarmID(signer), err)
	}
	return nil
}

// swarmFetchMembers asks one URL for its membership list. A
// known peer must serve under its pinned key; a seed (no pinned
// key yet) is trust-on-first-use — the shared secret already
// proved membership, and the key proves possession from here on.
func (app *Application) swarmFetchMembers(ctx context.Context, baseURL, peerID, peerKey string) (members []swarmMember, signer, key string, err error) {
	var body struct {
		Entry swarmSignedEntry `json:"entry"`
	}
	if err := app.swarmCli.authedGET(ctx, strings.TrimSuffix(baseURL, "/")+"/swarm/members", &body); err != nil {
		return nil, "", "", err
	}
	entry := body.Entry
	if entry.Kind != swarmKindMembers || entry.Signer == "" {
		return nil, "", "", fmt.Errorf("swarm: %s served no member list", baseURL)
	}
	if peerKey != "" {
		if entry.Signer != peerID || !verifySwarmEntry(entry, peerKey) {
			return nil, "", "", fmt.Errorf("swarm: %s served a member list that does not verify", baseURL)
		}
		return decodeSwarmMembers(app.cfg.swarm.secret, entry.Payload, entry.Signer, peerKey)
	}
	// Trust on first use: the shared secret opens the list —
	// only a member could have sealed it — then the signer's
	// listed key must verify the signature, proving the key's
	// holder sent it.
	opened, err := openSwarmMembers(entry.Payload, app.cfg.swarm.secret)
	if err != nil {
		return nil, "", "", fmt.Errorf("swarm: %s served a member list that does not open", baseURL)
	}
	var list []swarmMember
	dec := json.NewDecoder(strings.NewReader(string(opened)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&list); err != nil {
		return nil, "", "", fmt.Errorf("swarm: %s served an unreadable member list", baseURL)
	}
	for _, m := range list {
		if m.InstanceID != entry.Signer {
			continue
		}
		if verifySwarmEntry(entry, m.PublicKey) {
			return list, entry.Signer, m.PublicKey, nil
		}
	}
	return nil, "", "", fmt.Errorf("swarm: %s served a member list naming no verifiable signer", baseURL)
}

// decodeSwarmMembers opens and strictly decodes a
// signature-verified member list.
func decodeSwarmMembers(secret, payload, signer, key string) ([]swarmMember, string, string, error) {
	opened, err := openSwarmMembers(payload, secret)
	if err != nil {
		return nil, "", "", fmt.Errorf("swarm: member list from %s does not open", signer)
	}
	dec := json.NewDecoder(strings.NewReader(string(opened)))
	dec.DisallowUnknownFields()
	var members []swarmMember
	if err := dec.Decode(&members); err != nil {
		return nil, "", "", fmt.Errorf("swarm: unreadable member list from %s", signer)
	}
	return members, signer, key, nil
}

// errSwarmKeyChanged reports a sighting whose sender presented a
// key other than the one pinned for it. Nothing it said is
// learned.
var errSwarmKeyChanged = errors.New("swarm: the sender's key is not the one pinned for it")

// swarmFlagKeyConflict marks a refused key change on the peer
// it was aimed at and says so in the log. The pinned key stays;
// if the new one is real, the operator removes the peer and the
// next sighting pins it.
func (app *Application) swarmFlagKeyConflict(ctx context.Context, instanceID, heardFrom string, now time.Time) {
	logging.Warnf("swarm: refused a changed key for %s (heard from %s); the pinned key stays until the operator removes the instance",
		shortenSwarmID(instanceID), shortenSwarmID(heardFrom))
	if err := app.queries.FlagSwarmPeerKeyConflict(ctx, db.FlagSwarmPeerKeyConflictParams{
		InstanceID: instanceID, KeyConflictAt: timeSet(now),
	}); err != nil {
		logging.Errorf("swarm: mark key conflict on %s: %v", shortenSwarmID(instanceID), err)
	}
}

// swarmLearnMembers records the members one sighting names.
//
// Keys: the first key seen for an instance ID is pinned. A
// sighting that names a known ID under another key is refused
// and flagged, never written (the upsert itself enforces it),
// and the only way to a new key is the operator removing the
// peer. A sender whose own key is not its pinned one teaches
// nothing at all (errSwarmKeyChanged).
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
func (app *Application) swarmLearnMembers(ctx context.Context, members []swarmMember, signer, signerKey, dialURL, source string, now time.Time) error {
	if signer != "" && signerKey != "" {
		peer, err := app.queries.GetSwarmPeer(ctx, signer)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// A sender never heard of is new, not broken.
		case err != nil:
			return err
		case peer.PublicKey != signerKey:
			app.swarmFlagKeyConflict(ctx, signer, signer, now)
			return errSwarmKeyChanged
		}
	}
	seen := make(map[string]bool, len(members)+1)
	for _, m := range members {
		seen[m.InstanceID] = true
	}
	if signer != "" && signerKey != "" && !seen[signer] {
		members = append(members, swarmMember{InstanceID: signer, PublicKey: signerKey})
	}
	for _, m := range members {
		if m.InstanceID == "" || m.InstanceID == app.swarmID.id {
			continue
		}
		if raw, err := base64.StdEncoding.DecodeString(m.PublicKey); err != nil || len(raw) != ed25519.PublicKeySize {
			continue
		}
		signed := m.BaseURL != "" && verifySwarmMember(m)
		if !signed {
			// Only the sender itself, at the address this instance
			// dialed, may be filed without a signed address.
			if m.InstanceID != signer || m.PublicKey != signerKey || dialURL == "" {
				continue
			}
			m.BaseURL, m.URLSig, m.IssuedAt = dialURL, "", 0
		}
		if !validSwarmBaseURL(m.BaseURL) {
			continue
		}
		// A loopback address is only ever one this instance was
		// told to dial itself (a seed or a hand-added address in
		// local development). A member list cannot introduce one.
		if u, err := parseSwarmURL(m.BaseURL); err == nil && swarmHostIsLoopback(u.host) &&
			strings.TrimSuffix(m.BaseURL, "/") != strings.TrimSuffix(dialURL, "/") {
			continue
		}
		written, err := app.queries.UpsertSwarmPeer(ctx, db.UpsertSwarmPeerParams{
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
			app.swarmFlagKeyConflict(ctx, m.InstanceID, signer, now)
			continue
		}
		if !app.cfg.swarm.allowlisted() {
			if err := app.queries.SetSwarmPeerApproved(ctx, db.SetSwarmPeerApprovedParams{InstanceID: m.InstanceID, Approved: true}); err != nil {
				return err
			}
		}
	}
	return nil
}

// swarmDemandKeys gathers what this instance already wants, for
// peers to serve before the worker spends ESI calls: names
// first (type names readers found missing, structure names the
// resolver has due — few, and cheap to carry), then due history
// pairs, then pending organization records. A pull stages only
// what it asked for, so every kind the swarm carries has to be
// asked for here. Bounded, like every read that feeds a render
// or a fetch.
func (app *Application) swarmDemandKeys(ctx context.Context) []swarmPullKey {
	var keys []swarmPullKey
	for _, id := range app.esi.MissingTypeNames(maxSwarmNameDemand) {
		keys = append(keys, swarmPullKey{Kind: swarmKindTypeName, Key: fmt.Sprintf("%d", id)})
	}
	now := time.Now().UTC()
	if ids, err := app.queries.ListStructureResolutions(ctx, db.ListStructureResolutionsParams{
		ResolvedCutoff:  now.Add(-structureRenameWindow),
		MissingCutoff:   now.Add(-structureMissingWindow),
		ResolutionLimit: maxSwarmNameDemand,
	}); err != nil {
		logging.Errorf("swarm: demand structure names: %v", err)
	} else {
		for _, id := range ids {
			keys = append(keys, swarmPullKey{Kind: swarmKindStructure, Key: fmt.Sprintf("%d", id)})
		}
	}
	names := len(keys)
	if candidates, err := app.historyCandidates(ctx); err != nil {
		logging.Errorf("swarm: demand history: %v", err)
	} else {
		for _, key := range candidates {
			if len(keys)-names >= maxSwarmHistoryDemand {
				break
			}
			if app.marketFetchDue(ctx, marketFetchKind("history", key), historyRefetchGate) {
				keys = append(keys, swarmPullKey{Kind: swarmKindHistory,
					Key: fmt.Sprintf("%d/%d", key.RegionID, key.TypeID)})
			}
		}
	}
	if ids, err := app.queries.ListSwarmCorpDemand(ctx, maxSwarmOrgDemand); err != nil {
		logging.Errorf("swarm: demand corporations: %v", err)
	} else {
		for _, id := range ids {
			keys = append(keys, swarmPullKey{Kind: swarmKindCorporation, Key: fmt.Sprintf("%d", id)})
		}
	}
	if ids, err := app.queries.ListSwarmAllianceDemand(ctx, maxSwarmOrgDemand); err != nil {
		logging.Errorf("swarm: demand alliances: %v", err)
	} else {
		for _, id := range ids {
			keys = append(keys, swarmPullKey{Kind: swarmKindAlliance, Key: fmt.Sprintf("%d", id)})
		}
	}
	if len(keys) > maxSwarmDemandKeys {
		keys = keys[:maxSwarmDemandKeys]
	}
	return keys
}

// swarmSampled reports whether one staged entry is ESI-verified
// this pass: the trust draw, defaulting to verified when the
// randomness fails (a missed draw must cost a call, never
// trust).
func (app *Application) swarmSampled(good, bad int64) bool {
	r, err := swarmRandFloat()
	if err != nil {
		logging.Errorf("swarm: sample randomness: %v", err)
		return true
	}
	return swarmCheckDraw(swarmTrust(good, bad), good+bad, r)
}

// swarmServesWindow is how recently a pull must have staged
// something from a peer for it to stay in the data rotation.
const swarmServesWindow = 7 * 24 * time.Hour

// swarmPeerPullable reports whether one listed peer may serve:
// not under the floor once it has enough checks to judge.
// Shared by the cycle pull and the urgent single-key pull so
// both read the same gate. A seed is a member like any other
// and is not singled out here: whether a peer serves data is
// learned from its pulls (ListSwarmPullPeers), not from its
// identity or from where it is configured.
func swarmPeerPullable(good, bad int64) bool {
	if total := good + bad; total >= swarmTrustMinChecks && swarmTrust(good, bad) < swarmTrustFloor {
		return false
	}
	return true
}

// swarmPullOne asks approved peers for one key and stages what
// verifies, for latency-sensitive callers (the urgent drain):
// freshest first under the caller's deadline, falling through
// to the next pullable peer when one errors, stopping at the
// first peer that answers. Returns how many entries staged.
// Failures stay silent per attempt; the ESI fallback covers
// them.
func (app *Application) swarmPullOne(ctx context.Context, kind, key string) int {
	if !app.cfg.swarm.enabled() || app.swarmID == nil || app.swarmCli == nil {
		return 0
	}
	peers, err := app.queries.ListSwarmPullPeers(ctx, time.Now().UTC().Add(-swarmServesWindow))
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
		if !swarmPeerPullable(p.GoodChecks, p.BadChecks) {
			continue
		}
		keys := []swarmPullKey{{Kind: kind, Key: key}}
		entries, err := app.swarmCli.pullEntries(ctx, p.BaseUrl, keys)
		if err != nil {
			app.addSwarmCounter(ctx, swarmCounterPullsFailed, 1)
			continue
		}
		app.addSwarmCounter(ctx, swarmCounterPullsOK, 1)
		return app.swarmStagePulled(ctx, p.InstanceID, p.BaseUrl, keys, entries, time.Now())
	}
	return 0
}

// swarmStagePulled stages what one peer answered a pull with,
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
func (app *Application) swarmStagePulled(ctx context.Context, peerID, baseURL string, asked []swarmPullKey, entries []swarmSignedEntry, now time.Time) int {
	waiting := make(map[swarmPullKey]bool, len(asked))
	for _, k := range asked {
		waiting[k] = true
	}
	staged, unasked := 0, 0
	for _, entry := range entries {
		if entry.Signer != peerID {
			continue
		}
		k := swarmPullKey{Kind: entry.Kind, Key: entry.Key}
		if !waiting[k] {
			unasked++
			continue
		}
		delete(waiting, k)
		if err := app.stageSwarmEntry(ctx, entry, peerID, now); err != nil {
			logging.Warnf("swarm: pull %s: %v", baseURL, err)
			continue
		}
		staged++
	}
	if staged > 0 {
		if err := app.queries.TouchSwarmPeerServed(ctx, db.TouchSwarmPeerServedParams{InstanceID: peerID, LastServed: timeSet(now)}); err != nil {
			logging.Errorf("swarm: note %s served: %v", shortenSwarmID(peerID), err)
		}
	}
	if unasked > 0 {
		logging.Warnf("swarm: pull %s: dropped %d entries the pull did not ask for", baseURL, unasked)
		app.countSwarmShapeReject(ctx, peerID)
	}
	return staged
}

// swarmSettleUrgent settles one staged key inline for the
// urgent drain: the trust draw decides. Unchecked entries
// apply on reputation (a spot decision would reach the same
// row within a cycle or two, too late for the viewing page);
// drawn entries verify now against ESI on a one-call budget,
// spending what the direct fetch would have cost. Anything
// else reports unapplied and the caller fetches from ESI as
// before. Only the direct fetch's absence saves a call.
func (app *Application) swarmSettleUrgent(ctx context.Context, kind, key string) bool {
	row, err := app.queries.GetStagingEntry(ctx, db.GetStagingEntryParams{Kind: kind, Key: key})
	if err != nil {
		return false
	}
	peer, err := app.queries.GetSwarmPeer(ctx, row.Signer)
	if err != nil || peer.Excluded {
		return false
	}
	now := time.Now().UTC()
	if !app.swarmSampled(peer.GoodChecks, peer.BadChecks) {
		// Trust-earned, like the spot pass below: the entry
		// applies on reputation with the ledger untouched.
		// Only an ESI verdict counts for or against.
		if err := app.applySwarmEntry(ctx, row.Kind, row.Key, row.Payload, swarmOrigin{source: row.Source, signer: row.Signer, fetchedAt: row.FetchedAt, verified: false}, now); err != nil {
			logging.Errorf("swarm: apply %s %s: %v", row.Kind, row.Key, err)
			return false
		}
		return true
	}
	verdict, confirmed, _ := app.swarmVerifyAgainstESI(ctx, &fetchBudget{left: 1}, nil, row.Kind, row.Key, row.Payload)
	switch verdict {
	case swarmCheckPass:
		if err := app.applySwarmEntry(ctx, row.Kind, row.Key, confirmed, swarmOrigin{source: row.Source, signer: row.Signer, fetchedAt: row.FetchedAt, verified: true}, now); err != nil {
			logging.Errorf("swarm: apply %s %s: %v", row.Kind, row.Key, err)
			return false
		}
		app.countSwarmCheck(ctx, row.Signer, true)
		app.addSwarmCounter(ctx, swarmCounterSpotPassed, 1)
		return true
	case swarmCheckFail:
		app.discardSwarmEntry(ctx, row.Kind, row.Key, row.Signer, swarmDiscardMismatch)
		return false
	default:
		return false
	}
}

// swarmUrgentHistory serves one due history want from peers
// before the urgent drain spends ESI: one freshest-peer pull
// under a short deadline, then an inline settle. True when the
// want applied (verified or trust-earned) and the ESI fetch
// may skip; false keeps the existing direct fetch. Peer
// silence costs milliseconds, never correctness.
func (app *Application) swarmUrgentHistory(ctx context.Context, key marketKey) bool {
	// Non-swarm instances pay nothing here: no staging
	// lookup, no pull, straight to the direct fetch. Same
	// gate as the fetch suppressor below.
	if !app.cfg.swarm.enabled() || app.swarmID == nil {
		return false
	}
	skey := fmt.Sprintf("%d/%d", key.RegionID, key.TypeID)
	pctx, cancel := context.WithTimeout(ctx, swarmUrgentPullTimeout)
	app.swarmPullOne(pctx, swarmKindHistory, skey)
	cancel()
	return app.swarmSettleUrgent(ctx, swarmKindHistory, skey)
}

// swarmSuppressFetch reports whether one due key skips its ESI
// refetch: an unsettled staged entry newer than the window
// means a spot decision is moments away, and refetching under
// it would spend the call the pull already covered. Older rows
// fall back to ESI (see the window's comment); discards leave
// fetch state untouched, so the key goes due again and the
// suppression can only delay a fetch, never cancel it.
func (app *Application) swarmSuppressFetch(ctx context.Context, kind, key string, now time.Time) bool {
	if !app.cfg.swarm.enabled() || app.swarmID == nil {
		return false
	}
	row, err := app.queries.GetStagingEntry(ctx, db.GetStagingEntryParams{Kind: kind, Key: key})
	if err != nil {
		return false
	}
	received := row.ReceivedAt
	return now.Sub(received) < swarmStagedSuppressWindow
}

// swarmPull asks approved peers for the wanted keys and stages
// what they answer with, through swarmStagePulled: only keys
// this pull asked for, only under the serving peer's own pinned
// identity. Only peers known to serve data are asked, in turn,
// a few a cycle; a peer not yet asked gets one probe first. A
// seed that is only a directory answers its probe with nothing
// and is then dialed for membership alone.
func (app *Application) swarmPull(ctx context.Context, now time.Time) {
	keys := app.swarmDemandKeys(ctx)
	if len(keys) == 0 {
		return
	}
	peers, err := app.queries.ListSwarmPullPeers(ctx, time.Now().UTC().Add(-swarmServesWindow))
	if err != nil {
		logging.Errorf("swarm: pull peers: %v", err)
		return
	}
	var picked []db.ListSwarmPullPeersRow
	for _, p := range peers {
		if len(picked) >= maxSwarmPullPeersPerCycle {
			break
		}
		if !swarmPeerPullable(p.GoodChecks, p.BadChecks) {
			continue
		}
		picked = append(picked, p)
		// Its turn is taken whether or not it answers, so a dead
		// peer goes to the back of the line like any other.
		if err := app.queries.TouchSwarmPeerPulled(ctx, db.TouchSwarmPeerPulledParams{InstanceID: p.InstanceID, LastPulled: timeSet(now)}); err != nil {
			logging.Errorf("swarm: note pull of %s: %v", shortenSwarmID(p.InstanceID), err)
		}
	}
	// Asked a few at a time under one deadline (swarm_fanout.go);
	// what they answer is staged one at a time, in rotation order.
	type answer struct {
		entries []swarmSignedEntry
		err     error
	}
	answers := make([]answer, len(picked))
	swarmFanOut(ctx, len(picked), func(rctx context.Context, i int) {
		answers[i].entries, answers[i].err = app.swarmCli.pullEntries(rctx, picked[i].BaseUrl, keys)
	})
	for i, p := range picked {
		if answers[i].err != nil {
			app.addSwarmCounter(ctx, swarmCounterPullsFailed, 1)
			continue
		}
		app.addSwarmCounter(ctx, swarmCounterPullsOK, 1)
		app.swarmStagePulled(ctx, p.InstanceID, p.BaseUrl, keys, answers[i].entries, now)
	}
}

// swarmSpotCheck verifies staged entries against ESI and applies
// the ones that pass — the only path anything staged reaches
// the real tables. At most a few ESI calls a cycle, from the
// shared allowance; ESI's stop signal pauses the pass like any
// other. Only an entry ESI contradicts is a failure: it is
// discarded and counted against its signer. Anything
// undecidable (no scoped character, no span shared with ESI's
// history, a transient error) waits for another pass, at most
// maxSwarmDeferrals times, and is then dropped; so is an entry
// past the staging age cap, or from a signer under the trust
// floor. A drop is neutral: no mark, no streak, its own counter.
func (app *Application) swarmSpotCheck(ctx context.Context, allowance *fetchBudget, characters []db.Character, now time.Time) (applied int, limited bool) {
	rows, err := app.queries.ListUncheckedStaging(ctx, 25)
	if err != nil {
		logging.Errorf("swarm: list staged: %v", err)
		return 0, false
	}
	checks := 0
	for _, row := range rows {
		if now.Sub(row.ReceivedAt) > maxSwarmStagingAge {
			app.dropSwarmEntry(ctx, row.Kind, row.Key, row.Signer, swarmDropAgedOut)
			continue
		}
		peer, err := app.queries.GetSwarmPeer(ctx, row.Signer)
		if err != nil || peer.Excluded {
			if err := app.queries.DeleteStagingEntry(ctx, db.DeleteStagingEntryParams{Kind: row.Kind, Key: row.Key}); err != nil {
				logging.Errorf("swarm: drop staged %s %s: %v", row.Kind, row.Key, err)
			}
			continue
		}
		if total := peer.GoodChecks + peer.BadChecks; total >= swarmTrustMinChecks && swarmTrust(peer.GoodChecks, peer.BadChecks) < swarmTrustFloor {
			app.dropSwarmEntry(ctx, row.Kind, row.Key, row.Signer, swarmDropLowTrust)
			continue
		}
		if !app.swarmSampled(peer.GoodChecks, peer.BadChecks) {
			// Trust-earned: signature and shape already
			// verified at staging, and the sample skipped
			// ESI. The entry applies on reputation, but an
			// unchecked apply is neither a good check nor a
			// bad one: only ESI verdicts move trust (or the
			// failure streak), so the ledger stays
			// untouched. The per-kind applied counters
			// inside applySwarmEntry record the served
			// volume.
			if err := app.applySwarmEntry(ctx, row.Kind, row.Key, row.Payload, swarmOrigin{source: row.Source, signer: row.Signer, fetchedAt: row.FetchedAt, verified: false}, now); err != nil {
				logging.Errorf("swarm: apply %s %s: %v", row.Kind, row.Key, err)
				continue
			}
			applied++
			continue
		}
		if !swarmCheckable(row.Kind, characters) {
			// Nobody here can check this kind right now (a
			// structure name with no character holding the
			// scope). Trying would decide nothing, so it is not
			// tried and takes none of the cycle's check slots:
			// it counts as a pass that could not decide, and
			// the slots go to entries that can be checked.
			app.swarmDeferStaged(ctx, row.Kind, row.Key, row.Signer)
			continue
		}
		if checks >= maxSwarmSpotPerCycle || allowance.exhausted() {
			// Out of checks or out of calls: the rest wait,
			// and waiting is not a deferral.
			break
		}
		checks++
		verdict, confirmed, stop := app.swarmVerifyAgainstESI(ctx, allowance, characters, row.Kind, row.Key, row.Payload)
		if stop {
			return applied, true
		}
		switch verdict {
		case swarmCheckPass:
			if err := app.applySwarmEntry(ctx, row.Kind, row.Key, confirmed, swarmOrigin{source: row.Source, signer: row.Signer, fetchedAt: row.FetchedAt, verified: true}, now); err != nil {
				logging.Errorf("swarm: apply %s %s: %v", row.Kind, row.Key, err)
				continue
			}
			app.countSwarmCheck(ctx, row.Signer, true)
			app.addSwarmCounter(ctx, swarmCounterSpotPassed, 1)
			applied++
		case swarmCheckFail:
			app.discardSwarmEntry(ctx, row.Kind, row.Key, row.Signer, swarmDiscardMismatch)
		default:
			// Deferred: no scoped character, no span shared
			// with ESI's history, a transient error. The row
			// waits for another pass, a few times; one that
			// still cannot be decided is dropped, with no
			// mark on its signer, rather than taking a check
			// slot every cycle until the age cap.
			app.swarmDeferStaged(ctx, row.Kind, row.Key, row.Signer)
		}
	}
	return applied, false
}

// swarmDeferStaged notes one pass that could not decide a staged
// entry, and drops the entry (neutrally) once it has had
// maxSwarmDeferrals of them.
func (app *Application) swarmDeferStaged(ctx context.Context, kind, key, signer string) {
	n, err := app.queries.BumpStagingDeferral(ctx, db.BumpStagingDeferralParams{Kind: kind, Key: key})
	if err != nil {
		logging.Errorf("swarm: defer %s %s: %v", kind, key, err)
		return
	}
	if n >= maxSwarmDeferrals {
		app.dropSwarmEntry(ctx, kind, key, signer, swarmDropUndecidable)
	}
}

// swarmStructureChecker picks a character that can re-fetch a
// structure name: one that syncs and holds the structure scope.
// Nil when there is none.
func swarmStructureChecker(characters []db.Character) *db.Character {
	for i, ch := range characters {
		if characterSyncs(ch) && characterHasScope(ch, structureScope) {
			return &characters[i]
		}
	}
	return nil
}

// swarmCheckable reports whether this instance could check an
// entry of this kind against ESI at all right now. Everything
// public always can; a structure name needs a scoped character.
func swarmCheckable(kind string, characters []db.Character) bool {
	return kind != swarmKindStructure || swarmStructureChecker(characters) != nil
}

// swarmCheckVerdict is what re-fetching against ESI says about
// one staged entry.
type swarmCheckVerdict int

const (
	swarmCheckDefer swarmCheckVerdict = iota
	swarmCheckPass
	swarmCheckFail
)

// swarmVerifyAgainstESI re-fetches one staged entry's datum from
// ESI and compares the slow-moving truth: the whole shared span
// for history, the name for names, identity fields for records.
// On a pass, confirmed is the payload to apply: the entry's own
// for most kinds, and for history only the days ESI confirmed.
// stop reports ESI's error-limit signal. Anything less than a
// clean fetch is deferred, never failed — only ESI's own answer
// convicts.
func (app *Application) swarmVerifyAgainstESI(ctx context.Context, allowance *fetchBudget, characters []db.Character, kind, key, payload string) (verdict swarmCheckVerdict, confirmed string, stop bool) {
	switch kind {
	case swarmKindHistory:
		return app.swarmVerifyHistory(ctx, allowance, key, payload)
	case swarmKindTypeName:
		verdict, stop = app.swarmVerifyTypeName(ctx, allowance, key, payload)
	case swarmKindCorporation:
		verdict, stop = app.swarmVerifyCorporation(ctx, allowance, key, payload)
	case swarmKindAlliance:
		verdict, stop = app.swarmVerifyAlliance(ctx, allowance, key, payload)
	case swarmKindStructure:
		verdict, stop = app.swarmVerifyStructure(ctx, allowance, characters, key, payload)
	default:
		return swarmCheckFail, "", false
	}
	return verdict, payload, stop
}

// swarmTake budgets one spot-check call: false means the cycle
// is out of allowance and the row waits.
func (app *Application) swarmTake(allowance *fetchBudget) bool {
	return allowance.take()
}

// swarmVerifyHistory checks one staged history entry against a
// fresh ESI fetch, over the whole span the two share — not just
// the days that happen to appear in both. Inside that span the
// day sets must be identical and every figure equal: a staged
// day ESI does not have was made up, and a day ESI has that the
// entry leaves out was dropped. Days outside the span (ones ESI
// has since let go of, or has not served this instance yet)
// cannot be checked, so they are not applied: the payload
// handed back holds only the days ESI confirmed. No shared span
// verifies nothing and the entry waits.
func (app *Application) swarmVerifyHistory(ctx context.Context, allowance *fetchBudget, key, payload string) (swarmCheckVerdict, string, bool) {
	parts := strings.Split(key, "/")
	if len(parts) != 2 {
		return swarmCheckFail, "", false
	}
	regionID, ok := parseSwarmID(parts[0])
	if !ok {
		return swarmCheckFail, "", false
	}
	typeID, ok := parseSwarmID(parts[1])
	if !ok {
		return swarmCheckFail, "", false
	}
	var staged swarmHistoryPayload
	dec := json.NewDecoder(strings.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&staged); err != nil || len(staged.Days) == 0 {
		return swarmCheckFail, "", false
	}
	if !app.swarmTake(allowance) {
		return swarmCheckDefer, "", false
	}
	rows, err := app.fetchMarketHistory(ctx, marketKey{RegionID: regionID, TypeID: typeID})
	if err != nil {
		if errors.Is(err, esi.ErrErrorLimit) {
			return swarmCheckDefer, "", true
		}
		return swarmCheckDefer, "", false
	}
	if len(rows) == 0 {
		return swarmCheckDefer, "", false
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
		return swarmCheckDefer, "", false
	}
	confirmed := swarmHistoryPayload{RegionID: staged.RegionID, TypeID: staged.TypeID}
	for _, day := range staged.Days {
		if day.Date < lo || day.Date > hi {
			continue
		}
		got, ok := fresh[day.Date]
		if !ok {
			logging.Warnf("swarm: check %s %s: the entry has %s, which ESI does not", swarmKindHistory, key, day.Date)
			return swarmCheckFail, "", false
		}
		if got.Average != day.Average || got.Highest != day.Highest || got.Lowest != day.Lowest ||
			got.Volume != day.Volume || got.OrderCount != day.OrderCount {
			logging.Warnf("swarm: check %s %s: figures for %s differ from ESI", swarmKindHistory, key, day.Date)
			return swarmCheckFail, "", false
		}
		confirmed.Days = append(confirmed.Days, day)
		delete(fresh, day.Date)
	}
	for date := range fresh {
		if date >= lo && date <= hi {
			logging.Warnf("swarm: check %s %s: the entry leaves out %s, which ESI has", swarmKindHistory, key, date)
			return swarmCheckFail, "", false
		}
	}
	raw, err := json.Marshal(confirmed)
	if err != nil {
		return swarmCheckDefer, "", false
	}
	return swarmCheckPass, string(raw), false
}

func (app *Application) swarmVerifyTypeName(ctx context.Context, allowance *fetchBudget, key, payload string) (swarmCheckVerdict, bool) {
	typeID, ok := parseSwarmID(key)
	if !ok {
		return swarmCheckFail, false
	}
	var staged swarmTypeNamePayload
	dec := json.NewDecoder(strings.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&staged); err != nil || staged.TypeID != typeID {
		return swarmCheckFail, false
	}
	if !app.swarmTake(allowance) {
		return swarmCheckDefer, false
	}
	var t esi.Type
	if err := app.esi.Get(ctx, "", fmt.Sprintf("/universe/types/%d/", typeID), &t); err != nil {
		if errors.Is(err, esi.ErrErrorLimit) {
			return swarmCheckDefer, true
		}
		if code, has := esi.StatusCode(err); has && code == 404 {
			return swarmCheckFail, false
		}
		return swarmCheckDefer, false
	}
	if t.Name != staged.Name {
		return swarmCheckFail, false
	}
	return swarmCheckPass, false
}

func (app *Application) swarmVerifyCorporation(ctx context.Context, allowance *fetchBudget, key, payload string) (swarmCheckVerdict, bool) {
	id, ok := parseSwarmID(key)
	if !ok {
		return swarmCheckFail, false
	}
	var staged swarmOrgPayload
	dec := json.NewDecoder(strings.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&staged); err != nil || staged.ID != id {
		return swarmCheckFail, false
	}
	var body corporationRecordPayload
	if err := json.Unmarshal([]byte(staged.Payload), &body); err != nil {
		return swarmCheckFail, false
	}
	if !app.swarmTake(allowance) {
		return swarmCheckDefer, false
	}
	// Identity fields only: names, tickers and alliance homes
	// move slowly; member counts and descriptions drift daily
	// and are never evidence.
	var corp esi.Corporation
	if err := app.esi.Get(ctx, "", fmt.Sprintf("/corporations/%d/", id), &corp); err != nil {
		if errors.Is(err, esi.ErrErrorLimit) {
			return swarmCheckDefer, true
		}
		if code, has := esi.StatusCode(err); has && code == 404 {
			return swarmCheckFail, false
		}
		return swarmCheckDefer, false
	}
	if corp.Name != body.Corp.Name || corp.Ticker != body.Corp.Ticker || corp.AllianceID != body.Corp.AllianceID {
		return swarmCheckFail, false
	}
	return swarmCheckPass, false
}

func (app *Application) swarmVerifyAlliance(ctx context.Context, allowance *fetchBudget, key, payload string) (swarmCheckVerdict, bool) {
	id, ok := parseSwarmID(key)
	if !ok {
		return swarmCheckFail, false
	}
	var staged swarmOrgPayload
	dec := json.NewDecoder(strings.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&staged); err != nil || staged.ID != id {
		return swarmCheckFail, false
	}
	var body allianceRecordPayload
	if err := json.Unmarshal([]byte(staged.Payload), &body); err != nil {
		return swarmCheckFail, false
	}
	if !app.swarmTake(allowance) {
		return swarmCheckDefer, false
	}
	var alliance esi.Alliance
	if err := app.esi.Get(ctx, "", fmt.Sprintf("/alliances/%d/", id), &alliance); err != nil {
		if errors.Is(err, esi.ErrErrorLimit) {
			return swarmCheckDefer, true
		}
		if code, has := esi.StatusCode(err); has && code == 404 {
			return swarmCheckFail, false
		}
		return swarmCheckDefer, false
	}
	if alliance.Name != body.Alliance.Name || alliance.Ticker != body.Alliance.Ticker {
		return swarmCheckFail, false
	}
	return swarmCheckPass, false
}

func (app *Application) swarmVerifyStructure(ctx context.Context, allowance *fetchBudget, characters []db.Character, key, payload string) (swarmCheckVerdict, bool) {
	id, ok := parseSwarmID(key)
	if !ok {
		return swarmCheckFail, false
	}
	var staged swarmStructurePayload
	dec := json.NewDecoder(strings.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&staged); err != nil || staged.StructureID != id {
		return swarmCheckFail, false
	}
	// Structures need a scoped character to re-fetch; with none
	// linked the row waits rather than applying blind.
	scoped := swarmStructureChecker(characters)
	if scoped == nil {
		return swarmCheckDefer, false
	}
	if !app.swarmTake(allowance) {
		return swarmCheckDefer, false
	}
	// A structure this instance cannot see is not evidence of a
	// lie (access differs per character): only a different name
	// convicts.
	live, err := app.esi.FetchStructure(ctx, *scoped, id)
	if err != nil {
		if errors.Is(err, esi.ErrErrorLimit) {
			return swarmCheckDefer, true
		}
		return swarmCheckDefer, false
	}
	if live.Name != staged.Name {
		return swarmCheckFail, false
	}
	return swarmCheckPass, false
}
