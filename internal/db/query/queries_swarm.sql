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

-- A gossip or seed sighting records (or refreshes) a peer's
-- address and key and when it was last seen. It never touches
-- approval, exclusion, or the reputation counters: hearing
-- about an instance must not approve it or forgive it.
-- name: UpsertSwarmPeer :exec
INSERT INTO swarm_peers (instance_id, base_url, public_key, source, first_seen, last_seen)
VALUES ($1, $2, $3, $4, $5, $5)
ON CONFLICT (instance_id) DO UPDATE SET
    base_url = excluded.base_url,
    public_key = excluded.public_key,
    last_seen = excluded.last_seen;

-- name: GetSwarmPeer :one
SELECT instance_id, base_url, public_key, source, approved, excluded,
    exclude_note, good_checks, bad_checks, first_seen, last_seen, last_gossip
FROM swarm_peers
WHERE instance_id = $1;

-- name: ListSwarmPeers :many
SELECT instance_id, base_url, public_key, source, approved, excluded,
    exclude_note, good_checks, bad_checks, first_seen, last_seen, last_gossip
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

-- name: AddSwarmPeerCheck :exec
UPDATE swarm_peers
SET good_checks = good_checks + $2, bad_checks = bad_checks + $3
WHERE instance_id = $1;

-- A pulled entry stages at most once per key; a newer fetch of
-- the same key replaces it, an older one never overwrites.
-- Staging is not serving: renders read the real tables only.
-- name: StageSwarmEntry :exec
INSERT INTO swarm_staging (kind, key, payload, signer, signature, fetched_at, received_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (kind, key) DO UPDATE SET
    payload = excluded.payload,
    signer = excluded.signer,
    signature = excluded.signature,
    fetched_at = excluded.fetched_at,
    received_at = excluded.received_at,
    checked_at = NULL
WHERE excluded.fetched_at > swarm_staging.fetched_at;

-- name: ListUncheckedStaging :many
SELECT kind, key, payload, signer, signature, fetched_at, received_at
FROM swarm_staging
WHERE checked_at IS NULL
ORDER BY received_at
LIMIT $1;

-- name: MarkStagingChecked :exec
UPDATE swarm_staging SET checked_at = $3
WHERE kind = $1 AND key = $2;

-- name: DeleteStagingEntry :exec
DELETE FROM swarm_staging WHERE kind = $1 AND key = $2;

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
