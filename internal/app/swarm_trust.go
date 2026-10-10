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
	"encoding/base64"
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
const (
	swarmTrustPriorGood = 9
	swarmTrustPriorBad  = 1
	swarmTrustFloor     = 0.5
	swarmTrustMinChecks = 10
)

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

// countSwarmCheck ledgers one check against a signer and
// auto-excludes on sustained distrust: enough checks under the
// floor. Exclusion never clears by itself — gossip must not
// forgive — but the operator can re-admit by hand.
func (app *Application) countSwarmCheck(ctx context.Context, signer string, good bool) {
	var goodN, badN int64
	if good {
		goodN = 1
	} else {
		badN = 1
	}
	if err := app.queries.AddSwarmPeerCheck(ctx, db.AddSwarmPeerCheckParams{InstanceID: signer, GoodChecks: goodN, BadChecks: badN}); err != nil {
		logging.Errorf("swarm: ledger check for %s: %v", signer, err)
		return
	}
	peer, err := app.queries.GetSwarmPeer(ctx, signer)
	if err != nil || peer.Excluded {
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
		members, signer, key, err := app.swarmFetchMembers(ctx, c.url, c.id, c.key)
		if err != nil {
			continue
		}
		// A known peer serves under its pinned identity; a seed
		// introduces itself (trust on first use — it holds the
		// secret, which is the membership proof).
		if c.id != "" && signer != c.id {
			continue
		}
		if err := app.swarmLearnMembers(ctx, members, signer, key, now); err != nil {
			logging.Errorf("swarm: learn members via %s: %v", c.url, err)
			continue
		}
		learned++
		if peer, err := app.queries.GetSwarmPeer(ctx, signer); err == nil {
			_ = peer
			if err := app.queries.TouchSwarmPeerGossip(ctx, db.TouchSwarmPeerGossipParams{InstanceID: signer, LastGossip: timeSet(now)}); err != nil {
				logging.Errorf("swarm: touch gossip %s: %v", signer, err)
			}
		}
	}
	if learned > 0 {
		logging.Infof("swarm: gossip heard from %d peer(s)", learned)
	}
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
		return decodeSwarmMembers(entry.Payload, entry.Signer, peerKey)
	}
	dec := json.NewDecoder(strings.NewReader(entry.Payload))
	dec.DisallowUnknownFields()
	var list []swarmMember
	if err := dec.Decode(&list); err != nil {
		return nil, "", "", fmt.Errorf("swarm: %s served an unreadable member list", baseURL)
	}
	for _, m := range list {
		if m.InstanceID != entry.Signer {
			continue
		}
		if verifySwarmEntry(entry, m.PublicKey) {
			return decodeSwarmMembers(entry.Payload, entry.Signer, m.PublicKey)
		}
	}
	return nil, "", "", fmt.Errorf("swarm: %s served a member list naming no verifiable signer", baseURL)
}

// decodeSwarmMembers strictly decodes a verified member list.
func decodeSwarmMembers(payload, signer, key string) ([]swarmMember, string, string, error) {
	dec := json.NewDecoder(strings.NewReader(payload))
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
// mode; exclusion is never touched by gossip.
func (app *Application) swarmLearnMembers(ctx context.Context, members []swarmMember, signer, signerKey string, now time.Time) error {
	// The signer introduces itself too: record it under the key
	// its list verified with.
	if signer != "" && signerKey != "" {
		if peer, err := app.queries.GetSwarmPeer(ctx, signer); err != nil {
			return err
		} else if peer.PublicKey != signerKey {
			logging.Warnf("swarm: %s presents a changed key; keeping the pinned one until the operator re-admits it", signer)
		}
	}
	for _, m := range members {
		if m.InstanceID == "" || m.InstanceID == app.swarmID.id {
			continue
		}
		if !validSwarmBaseURL(m.BaseURL) {
			continue
		}
		if raw, err := base64.StdEncoding.DecodeString(m.PublicKey); err != nil || len(raw) != 32 {
			continue
		}
		source := "gossip"
		if m.InstanceID == signer && signerKey != "" {
			source = "seed"
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

// swarmPull asks approved peers for the wanted keys and stages
// what comes back. A peer may only serve under its own pinned
// identity: entries signed by anyone else are skipped, so a
// forwarder cannot spend another signer's reputation (or its
// own, on another's lies).
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
		if total := p.GoodChecks + p.BadChecks; total >= swarmTrustMinChecks && swarmTrust(p.GoodChecks, p.BadChecks) < swarmTrustFloor {
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
			if err := app.stageSwarmEntry(ctx, entry, now); err != nil {
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
