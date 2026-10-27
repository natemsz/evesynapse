-- Scans containing a pilot (schema 094, pilot_intel.go):
-- ListScansWithPilot casts every local scan's result to jsonb
-- and tests containment per row. Measured on production
-- 2026-10-26 (EXPLAIN ANALYZE, pilot 2122755002, scans at
-- 1,765 rows): 64 ms sequential scan, 60-180x the cost of the
-- other measured lookups, user-facing on the pilot page, and
-- growing linearly with every scan pasted. The GIN expression
-- below is exactly the query's containment operand, partial on
-- kind = 'local' as the query filters. Plain CREATE INDEX, not
-- CONCURRENTLY: schema steps run in one transaction each, and
-- at the measured table size the build is sub-second.
CREATE INDEX IF NOT EXISTS scans_local_pilots_gin
    ON scans USING gin ((result::jsonb -> 'local' -> 'pilots'))
    WHERE kind = 'local';
