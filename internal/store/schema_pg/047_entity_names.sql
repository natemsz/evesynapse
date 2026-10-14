-- Schema step 047: entity_names, one row per known character,
-- corporation and alliance name.
--
-- Names were held in process memory (warmed by the worker) and inside
-- each public record's JSON payload, so a restart started cold and a
-- page listing hundreds of corporations read and decoded a record per
-- row. Names rarely change; this table keeps every one the app has
-- learned, loaded into memory at startup and written through as new
-- ones arrive. `evesynapse -populate-names` fills it in bulk.
--
-- Backfilled here from what the database already knows: public
-- corporation and alliance records, the member lists inside alliance
-- records, and the linked characters. Payloads that are not JSON
-- objects are skipped.

CREATE TABLE IF NOT EXISTS entity_names (
    kind       TEXT NOT NULL,
    id         BIGINT NOT NULL,
    name       TEXT NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (kind, id)
);

INSERT INTO entity_names (kind, id, name)
SELECT 'corporation', corporation_id, payload::jsonb -> 'corp' ->> 'name'
FROM corporation_records
WHERE state = 'ready' AND payload IS JSON OBJECT
  AND coalesce(payload::jsonb -> 'corp' ->> 'name', '') <> ''
ON CONFLICT DO NOTHING;

INSERT INTO entity_names (kind, id, name)
SELECT 'alliance', alliance_id, payload::jsonb -> 'alliance' ->> 'name'
FROM alliance_records
WHERE state = 'ready' AND payload IS JSON OBJECT
  AND coalesce(payload::jsonb -> 'alliance' ->> 'name', '') <> ''
ON CONFLICT DO NOTHING;

INSERT INTO entity_names (kind, id, name)
SELECT DISTINCT ON ((m ->> 'id')::bigint) 'corporation', (m ->> 'id')::bigint, m ->> 'name'
FROM alliance_records,
     LATERAL jsonb_array_elements(
         CASE WHEN jsonb_typeof(payload::jsonb -> 'corporations') = 'array'
              THEN payload::jsonb -> 'corporations' ELSE '[]'::jsonb END) AS m
WHERE state = 'ready' AND payload IS JSON OBJECT
  AND coalesce(m ->> 'name', '') <> '' AND (m ->> 'id') ~ '^[0-9]+$'
ON CONFLICT DO NOTHING;

INSERT INTO entity_names (kind, id, name)
SELECT 'character', character_id, name FROM characters WHERE name <> ''
ON CONFLICT DO NOTHING;
