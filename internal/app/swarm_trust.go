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
	cryptorand "crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
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
)

// Staged entries older than this never apply: an entry nobody
// could verify within a week is stale, not evidence.
const maxSwarmStagingAge = 7 * 24 * time.Hour

// Sync-page counter names.
const (
	swarmCounterAppliedHistory = "applied_history"
	swarmCounterAppliedNames   = "applied_names"
	swarmCounterAppliedRecords = "applied_records"
	swarmCounterSpotPassed     = "spot_passed"
	swarmCounterSpotFailed     = "spot_failed"
	swarmCounterPullsOK        = "pulls_ok"
	swarmCounterPullsFailed    = "pulls_failed"
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

// swarmCycle is the worker's swarm pass: gossip when due, pull
// what is wanted, spot-check what is staged. Only the spot
// checks spend ESI calls, from the cycle's allowance; limited
// reports ESI's stop signal like every other pass.
func (app *Application) swarmCycle(ctx context.Context, allowance *fetchBudget, characters []db.Character, now time.Time) (applied int, limited bool) {
	if !app.cfg.swarm.enabled() || app.swarmID == nil || app.swarmCli == nil {
		return 0, false
	}
	app.swarmGossip(ctx, now)
	app.swarmPull(ctx, now)
	return app.swarmSpotCheck(ctx, allowance, characters, now)
}

// swarmGossipDue reports whether the membership round runs: the
// first run (bootstrap), or the interval since the last one,
// jittered ±25% so a whole swarm does not gossip in lockstep.
func (app *Application) swarmGossipDue(now time.Time) bool {
	app.swarmMu.Lock()
	defer app.swarmMu.Unlock()
	if app.swarmLastGossip.IsZero() {
		return true
	}
	interval := time.Duration(app.cfg.swarm.gossipHours) * time.Hour
	jitter := interval / 4
	next := app.swarmLastGossip.Add(interval - jitter + time.Duration(rand.Int64N(int64(2*jitter+1))))
	return !now.Before(next)
}

func (app *Application) swarmGossipDone(now time.Time) {
	app.swarmMu.Lock()
	defer app.swarmMu.Unlock()
	app.swarmLastGossip = now
}

// swarmGossip exchanges membership with seeds and known peers.
// Learned members are recorded (trust on first use, behind the
// shared secret); approval follows the mode — open trusts every
// secret-holder, allowlist waits for the operator. Excluded
// peers are neither contacted nor propagated.
func (app *Application) swarmGossip(ctx context.Context, now time.Time) {
	if !app.swarmGossipDue(now) {
		return
	}
	defer app.swarmGossipDone(now)
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
	learned := 0
	for _, c := range contacts {
		source := "gossip"
		if c.id == "" {
			source = "seed"
		}
		if app.swarmContactPeer(ctx, c.url, c.id, c.key, source, now) {
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
// here on. It reports whether anyone answered.
func (app *Application) swarmContactPeer(ctx context.Context, baseURL, peerID, peerKey, source string, now time.Time) bool {
	members, signer, key, err := app.swarmFetchMembers(ctx, baseURL, peerID, peerKey)
	if err != nil {
		return false
	}
	if peerID != "" && signer != peerID {
		return false
	}
	if err := app.swarmLearnMembers(ctx, members, signer, key, baseURL, source, now); err != nil {
		logging.Errorf("swarm: learn members via %s: %v", baseURL, err)
		return false
	}
	if err := app.queries.TouchSwarmPeerGossip(ctx, db.TouchSwarmPeerGossipParams{InstanceID: signer, LastGossip: timeSet(now)}); err != nil {
		logging.Errorf("swarm: touch gossip %s: %v", signer, err)
	}
	return true
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

// swarmLearnMembers records gossip-learned members. New IDs pin
// the key they arrived with; a changed key for a known ID is
// kept as-is (rotation goes through the operator: delete the
// peer row and the next gossip re-pins). Approval follows the
// mode; exclusion is never touched by gossip. The contacted
// signer is recorded under the URL that just answered when its
// own list does not name it — the dialed URL is known-good, so
// a lonely seed still introduces itself.
func (app *Application) swarmLearnMembers(ctx context.Context, members []swarmMember, signer, signerKey, dialURL, source string, now time.Time) error {
	if signer != "" && signerKey != "" {
		if peer, err := app.queries.GetSwarmPeer(ctx, signer); err != nil {
			// A signer never heard of is new, not broken:
			// anything else aborts the whole sighting.
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		} else if peer.PublicKey != signerKey {
			logging.Warnf("swarm: %s presents a changed key; keeping the pinned one until the operator re-admits it", signer)
		}
	}
	seen := make(map[string]bool, len(members)+1)
	for _, m := range members {
		seen[m.InstanceID] = true
	}
	if signer != "" && signerKey != "" && !seen[signer] {
		members = append(members, swarmMember{InstanceID: signer, BaseURL: dialURL, PublicKey: signerKey})
	}
	for _, m := range members {
		if m.InstanceID == "" || m.InstanceID == app.swarmID.id {
			continue
		}
		// The signer's own record may carry no address (no
		// public origin configured): the URL that just
		// answered is known-good and files it.
		if m.InstanceID == signer && m.BaseURL == "" {
			m.BaseURL = dialURL
		}
		if !validSwarmBaseURL(m.BaseURL) {
			continue
		}
		if raw, err := base64.StdEncoding.DecodeString(m.PublicKey); err != nil || len(raw) != 32 {
			continue
		}
		if err := app.queries.UpsertSwarmPeer(ctx, db.UpsertSwarmPeerParams{
			InstanceID: m.InstanceID, BaseUrl: m.BaseURL, PublicKey: m.PublicKey,
			Source: source, FirstSeen: now,
		}); err != nil {
			return err
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
// peers to serve before the worker spends ESI calls: due
// history pairs first, then pending organization records.
// Bounded, like every read that feeds a render or a fetch.
func (app *Application) swarmDemandKeys(ctx context.Context) []swarmPullKey {
	var keys []swarmPullKey
	if candidates, err := app.historyCandidates(ctx); err != nil {
		logging.Errorf("swarm: demand history: %v", err)
	} else {
		for _, key := range candidates {
			if len(keys) >= maxSwarmHistoryDemand {
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

// swarmPeerPullable reports whether one listed peer may serve:
// not the seed marker, and not under the floor once it has
// enough checks to judge. Shared by the cycle pull and the
// urgent single-key pull so both read the same gate.
func swarmPeerPullable(instanceID string, good, bad int64) bool {
	if instanceID == swarmSeedInstanceID {
		return false
	}
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
	peers, err := app.queries.ListSwarmPullPeers(ctx)
	if err != nil {
		logging.Errorf("swarm: pull peers: %v", err)
		return 0
	}
	staged := 0
	for _, p := range peers {
		if !swarmPeerPullable(p.InstanceID, p.GoodChecks, p.BadChecks) {
			continue
		}
		entries, err := app.swarmCli.pullEntries(ctx, p.BaseUrl, []swarmPullKey{{Kind: kind, Key: key}})
		if err != nil {
			app.addSwarmCounter(ctx, swarmCounterPullsFailed, 1)
			continue
		}
		app.addSwarmCounter(ctx, swarmCounterPullsOK, 1)
		now := time.Now()
		for _, entry := range entries {
			if entry.Signer != p.InstanceID {
				continue
			}
			if err := app.stageSwarmEntry(ctx, entry, p.InstanceID, now); err != nil {
				logging.Warnf("swarm: pull %s: %v", p.BaseUrl, err)
				continue
			}
			staged++
		}
		return staged
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
		if err := app.applySwarmEntry(ctx, row.Kind, row.Key, row.Payload, now); err != nil {
			logging.Errorf("swarm: apply %s %s: %v", row.Kind, row.Key, err)
			return false
		}
		return true
	}
	verdict, _ := app.swarmVerifyAgainstESI(ctx, &fetchBudget{left: 1}, nil, row.Kind, row.Key, row.Payload)
	switch verdict {
	case swarmCheckPass:
		if err := app.applySwarmEntry(ctx, row.Kind, row.Key, row.Payload, now); err != nil {
			logging.Errorf("swarm: apply %s %s: %v", row.Kind, row.Key, err)
			return false
		}
		app.countSwarmCheck(ctx, row.Signer, true)
		app.addSwarmCounter(ctx, swarmCounterSpotPassed, 1)
		return true
	case swarmCheckFail:
		app.discardSwarmEntry(ctx, row.Kind, row.Key, row.Signer)
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
// what comes back. A peer may only serve under its own pinned
// identity: entries signed by anyone else are skipped, so a
// forwarder cannot spend another signer's reputation (or its
// own, on another's lies). Seed-only peers are directories,
// not sources — the worker signs under its fixed "seed"
// identity and holds no data, so pulling it would spend one of
// the cycle's peer slots learning nothing. A directory that
// also serves under its own minted identity (a warmer acting
// as seed) stays pullable: only the protocol's seed marker
// skips, never the configured URL.
func (app *Application) swarmPull(ctx context.Context, now time.Time) {
	keys := app.swarmDemandKeys(ctx)
	if len(keys) == 0 {
		return
	}
	peers, err := app.queries.ListSwarmPullPeers(ctx)
	if err != nil {
		logging.Errorf("swarm: pull peers: %v", err)
		return
	}
	n := 0
	for _, p := range peers {
		if n >= maxSwarmPullPeersPerCycle {
			break
		}
		if !swarmPeerPullable(p.InstanceID, p.GoodChecks, p.BadChecks) {
			continue
		}
		n++
		entries, err := app.swarmCli.pullEntries(ctx, p.BaseUrl, keys)
		if err != nil {
			app.addSwarmCounter(ctx, swarmCounterPullsFailed, 1)
			continue
		}
		app.addSwarmCounter(ctx, swarmCounterPullsOK, 1)
		for _, entry := range entries {
			if entry.Signer != p.InstanceID {
				continue
			}
			if err := app.stageSwarmEntry(ctx, entry, p.InstanceID, now); err != nil {
				logging.Warnf("swarm: pull %s: %v", p.BaseUrl, err)
			}
		}
	}
}

// swarmSpotCheck verifies staged entries against ESI and applies
// the ones that pass — the only path anything staged reaches
// the real tables. At most a few ESI calls a cycle, from the
// shared allowance; ESI's stop signal pauses the pass like any
// other. Anything failing is discarded and counted against its
// signer; anything undecidable (no scoped character, no
// overlapping history, a transient error) waits for the next
// pass, up to the staging age cap.
func (app *Application) swarmSpotCheck(ctx context.Context, allowance *fetchBudget, characters []db.Character, now time.Time) (applied int, limited bool) {
	rows, err := app.queries.ListUncheckedStaging(ctx, 25)
	if err != nil {
		logging.Errorf("swarm: list staged: %v", err)
		return 0, false
	}
	checks := 0
	for _, row := range rows {
		if now.Sub(row.ReceivedAt) > maxSwarmStagingAge {
			app.discardSwarmEntry(ctx, row.Kind, row.Key, row.Signer)
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
			app.discardSwarmEntry(ctx, row.Kind, row.Key, row.Signer)
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
			if err := app.applySwarmEntry(ctx, row.Kind, row.Key, row.Payload, now); err != nil {
				logging.Errorf("swarm: apply %s %s: %v", row.Kind, row.Key, err)
				continue
			}
			applied++
			continue
		}
		if checks >= maxSwarmSpotPerCycle {
			break
		}
		checks++
		verdict, stop := app.swarmVerifyAgainstESI(ctx, allowance, characters, row.Kind, row.Key, row.Payload)
		if stop {
			return applied, true
		}
		switch verdict {
		case swarmCheckPass:
			if err := app.applySwarmEntry(ctx, row.Kind, row.Key, row.Payload, now); err != nil {
				logging.Errorf("swarm: apply %s %s: %v", row.Kind, row.Key, err)
				continue
			}
			app.countSwarmCheck(ctx, row.Signer, true)
			app.addSwarmCounter(ctx, swarmCounterSpotPassed, 1)
			applied++
		case swarmCheckFail:
			app.discardSwarmEntry(ctx, row.Kind, row.Key, row.Signer)
		default:
			// Deferred: no scoped character, no overlapping
			// history, a transient error — the row waits.
		}
	}
	return applied, false
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
// ESI and compares the slow-moving truth: exact overlap for
// history and names, identity fields for records. stop reports
// ESI's error-limit signal. Anything less than a clean fetch is
// deferred, never failed — only ESI's own answer convicts.
func (app *Application) swarmVerifyAgainstESI(ctx context.Context, allowance *fetchBudget, characters []db.Character, kind, key, payload string) (swarmCheckVerdict, bool) {
	switch kind {
	case swarmKindHistory:
		return app.swarmVerifyHistory(ctx, allowance, key, payload)
	case swarmKindTypeName:
		return app.swarmVerifyTypeName(ctx, allowance, key, payload)
	case swarmKindCorporation:
		return app.swarmVerifyCorporation(ctx, allowance, key, payload)
	case swarmKindAlliance:
		return app.swarmVerifyAlliance(ctx, allowance, key, payload)
	case swarmKindStructure:
		return app.swarmVerifyStructure(ctx, allowance, characters, key, payload)
	default:
		return swarmCheckFail, false
	}
}

// swarmTake budgets one spot-check call: false means the cycle
// is out of allowance and the row waits.
func (app *Application) swarmTake(allowance *fetchBudget) bool {
	return allowance.take()
}

func (app *Application) swarmVerifyHistory(ctx context.Context, allowance *fetchBudget, key, payload string) (swarmCheckVerdict, bool) {
	parts := strings.Split(key, "/")
	if len(parts) != 2 {
		return swarmCheckFail, false
	}
	regionID, ok := parseSwarmID(parts[0])
	if !ok {
		return swarmCheckFail, false
	}
	typeID, ok := parseSwarmID(parts[1])
	if !ok {
		return swarmCheckFail, false
	}
	var staged swarmHistoryPayload
	dec := json.NewDecoder(strings.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&staged); err != nil {
		return swarmCheckFail, false
	}
	if !app.swarmTake(allowance) {
		return swarmCheckDefer, false
	}
	// Past days are immutable: every staged day ESI still shows
	// must match exactly. Dates only the signer shows are its
	// newer fetch, not a lie; dates only ESI shows are days the
	// signer never stored. No overlap at all verifies nothing.
	rows, err := app.fetchMarketHistory(ctx, marketKey{RegionID: regionID, TypeID: typeID})
	if err != nil {
		if errors.Is(err, esi.ErrErrorLimit) {
			return swarmCheckDefer, true
		}
		return swarmCheckDefer, false
	}
	fresh := make(map[string]esi.MarketHistoryDay, len(rows))
	for _, row := range rows {
		fresh[row.Date] = row
	}
	overlap := 0
	for _, day := range staged.Days {
		got, ok := fresh[day.Date]
		if !ok {
			continue
		}
		overlap++
		if got.Average != day.Average || got.Highest != day.Highest || got.Lowest != day.Lowest ||
			got.Volume != day.Volume || got.OrderCount != day.OrderCount {
			return swarmCheckFail, false
		}
	}
	if overlap == 0 {
		return swarmCheckDefer, false
	}
	return swarmCheckPass, false
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
	var scoped *db.Character
	for i, ch := range characters {
		if characterSyncs(ch) && characterHasScope(ch, structureScope) {
			scoped = &characters[i]
			break
		}
	}
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
