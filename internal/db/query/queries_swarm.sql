-- Shared cache across instances (schema 030): this
-- instance's identity, the peers it has heard of with their
-- reputation ledger, staged entries waiting on a spot check,
-- and the Sync page's counters. Private data never appears
-- here: only public kinds (market history, type names,
-- corporation and alliance records) are staged or counted.

-- name: GetSwarmIdentity :one
SELECT instance_id, public_key, sealed_seed, created_at
FROM swarm_identity
LIMIT 1;

-- name: PutSwarmIdentity :exec
INSERT INTO swarm_identity (id, instance_id, public_key, sealed_seed, created_at)
VALUES (1, $1, $2, $3, $4)
ON CONFLICT (id) DO UPDATE SET
    instance_id = excluded.instance_id,
    public_key = excluded.public_key,
    sealed_seed = excluded.sealed_seed,
    created_at = excluded.created_at;

-- A sighting records a peer once and pins the key it arrived
-- with. The first key seen for an instance ID is the one kept:
-- a later sighting under another key writes nothing (zero rows,
-- which the caller flags), so no sighting can replace a pinned
-- key. The address moves only on the peer's own word
-- (own_sighting: the sighting was signed by that peer's pinned
-- key); anyone else's mention refreshes last_seen and nothing
-- more. Approval, exclusion and the reputation counters are
-- never touched: hearing about an instance must not approve it
-- or forgive it.
-- name: UpsertSwarmPeer :execrows
INSERT INTO swarm_peers (instance_id, base_url, public_key, source, first_seen, last_seen)
VALUES ($1, $2, $3, $4, $5, $5)
ON CONFLICT (instance_id) DO UPDATE SET
    base_url = CASE WHEN sqlc.arg(own_sighting)::boolean THEN excluded.base_url ELSE swarm_peers.base_url END,
    last_seen = excluded.last_seen
WHERE swarm_peers.public_key = excluded.public_key;

-- A refused key change is marked on the peer it was aimed at,
-- for the operator to see. Nothing else about the peer changes.
-- name: FlagSwarmPeerKeyConflict :exec
UPDATE swarm_peers SET key_conflict_at = $2 WHERE instance_id = $1;

-- name: GetSwarmPeer :one
SELECT instance_id, base_url, public_key, source, approved, excluded,
    exclude_note, good_checks, bad_checks, first_seen, last_seen, last_gossip,
    key_conflict_at
FROM swarm_peers
WHERE instance_id = $1;

-- name: ListSwarmPeers :many
SELECT instance_id, base_url, public_key, source, approved, excluded,
    exclude_note, good_checks, bad_checks, first_seen, last_seen, last_gossip,
    key_conflict_at
FROM swarm_peers
ORDER BY last_seen DESC
LIMIT 100;

-- Peers a pull may read from: approved, not excluded. Trust is
-- scored in Go from the counters; the floor keeps gossip noise
-- out of the pull set without another round trip.
-- name: ListSwarmPullPeers :many
SELECT instance_id, base_url, public_key, good_checks, bad_checks
FROM swarm_peers
WHERE approved AND NOT excluded
ORDER BY last_seen DESC
LIMIT 25;

-- name: SetSwarmPeerApproved :exec
UPDATE swarm_peers SET approved = $2 WHERE instance_id = $1;

-- name: SetSwarmPeerExcluded :exec
UPDATE swarm_peers SET excluded = $2, exclude_note = $3 WHERE instance_id = $1;

-- name: TouchSwarmPeerGossip :exec
UPDATE swarm_peers SET last_gossip = $2 WHERE instance_id = $1;

-- Removing a peer forgets everything: address, pinned key,
-- approval, exclusion, the ledger, and any key-conflict mark.
-- It is the only way a new key is accepted for an instance ID:
-- the operator removes the peer and the next sighting pins from
-- scratch. It is also how a dead instance leaves the list. Staged entries under its name
-- stay staged: they still face their own spot checks.
-- name: DeleteSwarmPeer :exec
DELETE FROM swarm_peers WHERE instance_id = $1;

-- name: AddSwarmPeerCheck :exec
UPDATE swarm_peers
SET good_checks = good_checks + $2, bad_checks = bad_checks + $3
WHERE instance_id = $1;

-- A pulled entry stages at most once per key; a newer fetch of
-- the same key replaces it, an older one never overwrites.
-- Staging is not serving: renders read the real tables only.
-- name: StageSwarmEntry :exec
INSERT INTO swarm_staging (kind, key, payload, signer, signature, fetched_at, received_at, source)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (kind, key) DO UPDATE SET
    payload = excluded.payload,
    signer = excluded.signer,
    signature = excluded.signature,
    fetched_at = excluded.fetched_at,
    received_at = excluded.received_at,
    source = excluded.source,
    checked_at = NULL
WHERE excluded.fetched_at > swarm_staging.fetched_at;

-- name: ListUncheckedStaging :many
SELECT kind, key, payload, signer, signature, fetched_at, received_at, source
FROM swarm_staging
WHERE checked_at IS NULL
ORDER BY received_at
LIMIT $1;

-- name: MarkStagingChecked :exec
UPDATE swarm_staging SET checked_at = $3
WHERE kind = $1 AND key = $2;

-- name: DeleteStagingEntry :exec
DELETE FROM swarm_staging WHERE kind = $1 AND key = $2;

-- A staged entry is unsettled until a spot decision deletes
-- it, so any present row is one the passes may defer to. The
-- urgent drain settles it inline; the history pass skips the
-- ESI refetch while it is fresh.
-- name: GetStagingEntry :one
SELECT kind, key, payload, signer, signature, fetched_at, received_at, source
FROM swarm_staging
WHERE kind = $1 AND key = $2;

-- Forgiveness on a slow cadence: old bad marks fade even when
-- no new checks land, so a fixed peer's score recovers over
-- weeks. Exclusion itself never clears here; the operator
-- re-admits, and the decayed score is the fresh start.
-- name: DecaySwarmBadChecks :exec
UPDATE swarm_peers SET bad_checks = (bad_checks * 7) / 8 WHERE bad_checks > 0;

-- Pending organization records someone followed a name to: what a
-- pull asks peers for before the worker spends ESI calls on it.
-- name: ListSwarmCorpDemand :many
SELECT corporation_id
FROM corporation_records
WHERE state = 'pending'
ORDER BY noted_at NULLS FIRST, corporation_id
LIMIT $1;

-- name: ListSwarmAllianceDemand :many
SELECT alliance_id
FROM alliance_records
WHERE state = 'pending'
ORDER BY noted_at NULLS FIRST, alliance_id
LIMIT $1;

-- name: AddSwarmCounter :exec
INSERT INTO swarm_counters (name, value)
VALUES ($1, $2)
ON CONFLICT (name) DO UPDATE SET value = swarm_counters.value + excluded.value;

-- name: GetSwarmCounters :many
SELECT name, value FROM swarm_counters;

-- Provenance (schema 035): one row per applied key, replaced
-- when the key is applied again. source is the instance the
-- entry was pulled from, signer the instance whose key signed
-- it, fetched_at the fetch time that signature covers.
-- row_stamp is the timestamp written into the live row, where
-- the kind has one, so a rollback can tell the swarm's copy
-- from one this instance has fetched itself since.
-- name: PutSwarmApplied :exec
INSERT INTO swarm_applied (kind, key, source, signer, fetched_at, applied_at, verified, row_stamp, first_date, last_date)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (kind, key) DO UPDATE SET
    source = excluded.source,
    signer = excluded.signer,
    fetched_at = excluded.fetched_at,
    applied_at = excluded.applied_at,
    verified = excluded.verified,
    row_stamp = excluded.row_stamp,
    first_date = excluded.first_date,
    last_date = excluded.last_date;

-- name: GetSwarmApplied :one
SELECT kind, key, source, signer, fetched_at, applied_at, verified, row_stamp, first_date, last_date
FROM swarm_applied
WHERE kind = $1 AND key = $2;

-- name: DeleteSwarmApplied :exec
DELETE FROM swarm_applied WHERE kind = $1 AND key = $2;

-- What is still in the tables from signers that have since been
-- excluded: the rollback pass takes these back, oldest first, a
-- bounded batch at a time.
-- name: ListSwarmAppliedToRollBack :many
SELECT a.kind, a.key, a.source, a.signer, a.fetched_at, a.applied_at, a.verified, a.row_stamp, a.first_date, a.last_date
FROM swarm_applied a
JOIN swarm_peers p ON p.instance_id = a.signer
WHERE p.excluded
ORDER BY a.applied_at, a.kind, a.key
LIMIT $1;

-- name: CountSwarmAppliedBySigner :one
SELECT COUNT(*) FROM swarm_applied WHERE signer = $1;

-- The rollbacks. Each touches the live row only while it still
-- carries the stamp the swarm apply wrote: a row this instance
-- has fetched from ESI since is its own and stays.

-- name: RollBackSwarmCorporation :execrows
UPDATE corporation_records SET payload = '', state = 'pending', fetched_at = NULL
WHERE corporation_id = $1 AND fetched_at = sqlc.arg(row_stamp)::timestamptz;

-- name: RollBackSwarmAlliance :execrows
UPDATE alliance_records SET payload = '', state = 'pending', fetched_at = NULL
WHERE alliance_id = $1 AND fetched_at = sqlc.arg(row_stamp)::timestamptz;

-- name: RollBackSwarmStructure :execrows
UPDATE structure_names SET name = '', state = 'pending', resolved_at = NULL, source = 'esi'
WHERE structure_id = $1 AND source = 'community' AND resolved_at = sqlc.arg(row_stamp)::timestamptz;

-- Type names carry no timestamp to tell copies apart, so the
-- cached name simply goes: the next reader resolves it afresh.
-- name: RollBackSwarmTypeName :execrows
DELETE FROM type_names WHERE type_id = $1;

-- History goes by the day range the entry supplied, and only
-- while the pair's fetch state still says the swarm supplied it.
-- name: RollBackSwarmHistoryDays :execrows
DELETE FROM market_history
WHERE region_id = $1 AND type_id = $2
  AND date >= sqlc.arg(first_date)::text AND date <= sqlc.arg(last_date)::text;

-- name: RollBackSwarmHistoryState :execrows
DELETE FROM market_fetch_state
WHERE kind = $1 AND detail = 'swarm' AND attempted_at = sqlc.arg(row_stamp)::timestamptz;
