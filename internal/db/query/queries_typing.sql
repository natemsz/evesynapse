-- Typing-intent guesses (schema 028): what each search box's
-- latest keystroke guessed, so the next keystroke can replace it.
-- The queue rows carry the typing ring (worker_pagewants.go); this
-- table carries which box put them there, and whether the pilot
-- opened one within minutes of it being noted (the hit rate the
-- prefetch tuning reads later).

-- name: InsertTypingGuess :exec
INSERT INTO typing_guesses (box, kind, entity_id, region_id, noted_at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (box, kind, entity_id, region_id) DO UPDATE SET
    noted_at = excluded.noted_at;

-- name: DeleteTypingGuessesForBox :exec
DELETE FROM typing_guesses WHERE box = $1;

-- name: GetTypingGuess :one
SELECT box, kind, entity_id, region_id, noted_at, hit_at
FROM typing_guesses
WHERE box = $1 AND kind = $2 AND entity_id = $3 AND region_id = $4;

-- name: MarkTypingGuessHits :exec
UPDATE typing_guesses SET hit_at = sqlc.arg(now)::timestamptz
WHERE kind = $1 AND entity_id = $2 AND region_id = $3
  AND hit_at IS NULL AND noted_at > sqlc.arg(since)::timestamptz;

-- A box's next keystroke drops the queue rows its guesses are
-- still waiting in. Only rows still at the typing ring go: one
-- the pilot opened has been bumped to viewed, and one already
-- holding data is a record now, not a guess.

-- name: DeleteTypingPilotGuesses :exec
DELETE FROM pilot_records
WHERE priority = sqlc.arg(typing_priority)::bigint AND state = 'pending'
  AND character_id IN (SELECT entity_id FROM typing_guesses WHERE box = $1 AND kind = 'pilot');

-- name: DeleteTypingCorporationGuesses :exec
DELETE FROM corporation_records
WHERE priority = sqlc.arg(typing_priority)::bigint AND state = 'pending'
  AND corporation_id IN (SELECT entity_id FROM typing_guesses WHERE box = $1 AND kind = 'corporation');

-- name: DeleteTypingAllianceGuesses :exec
DELETE FROM alliance_records
WHERE priority = sqlc.arg(typing_priority)::bigint AND state = 'pending'
  AND alliance_id IN (SELECT entity_id FROM typing_guesses WHERE box = $1 AND kind = 'alliance');

-- name: DeleteTypingHistoryGuesses :exec
DELETE FROM market_history_wants
WHERE priority = sqlc.arg(typing_priority)::bigint
  AND (region_id, type_id) IN (SELECT region_id, entity_id FROM typing_guesses WHERE box = $1 AND kind = 'market_history');

-- name: DeleteTypingDetailGuesses :exec
DELETE FROM type_details
WHERE priority = sqlc.arg(typing_priority)::bigint AND fetched_at IS NULL
  AND type_id IN (SELECT entity_id FROM typing_guesses WHERE box = $1 AND kind = 'type_description');

-- The latest guess behind an entity's fetch: a row means the page
-- was warmed from a search box. Feeds the freshness line; no row
-- means the fetch was routine or foresight-driven (checked first).
-- name: GetTypingGuessNotedForEntity :one
SELECT box
FROM typing_guesses
WHERE kind = $1 AND entity_id = $2 AND region_id = $3 AND noted_at <= $4
ORDER BY noted_at DESC
LIMIT 1;

-- Hit rates by guess kind over a window: what the Sync page's
-- guess table shows beside the foresight one.
-- name: ListTypingGuessRates :many
SELECT kind, COUNT(*) AS queued, COUNT(hit_at) AS opened
FROM typing_guesses
WHERE noted_at >= $1
GROUP BY kind
ORDER BY kind;
