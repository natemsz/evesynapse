-- Abyssal sighting attribution (schema 069).
--
-- A sighting's price is its contract's price, which is the
-- abyssal's own ask only when nothing else shares the contract.
-- The calibration corpus needs to know which listings those are,
-- and the contract rows are deleted when a listing finishes, so
-- the facts are stamped onto the sighting while the contract is
-- stored (StampAbyssalSightingAttribution, run beside every
-- sighting write) and survive it:
--
--   issuer_id       who listed it (the corpus caps any one
--                   seller's say in a family)
--   abyssal_lines   offered lines that are known abyssal items
--   wanted_lines    lines the issuer asks for in exchange
--   other_lines     offered lines that are not abyssal items
--   other_unpriced  of those, the ones the appraisal has not priced
--   other_value     the appraised value of those lines
--
-- NULL attribution is a sighting stamped before this step whose
-- contract is already gone: the corpus reads it at a reduced say.
ALTER TABLE abyssal_sightings ADD COLUMN IF NOT EXISTS issuer_id BIGINT;
ALTER TABLE abyssal_sightings ADD COLUMN IF NOT EXISTS abyssal_lines BIGINT;
ALTER TABLE abyssal_sightings ADD COLUMN IF NOT EXISTS wanted_lines BIGINT;
ALTER TABLE abyssal_sightings ADD COLUMN IF NOT EXISTS other_lines BIGINT;
ALTER TABLE abyssal_sightings ADD COLUMN IF NOT EXISTS other_unpriced BIGINT;
ALTER TABLE abyssal_sightings ADD COLUMN IF NOT EXISTS other_value DOUBLE PRECISION;

-- Backfill every sighting whose contract is still stored.
UPDATE abyssal_sightings s SET
    issuer_id      = c.issuer_id,
    abyssal_lines  = m.abyssal_lines,
    wanted_lines   = m.wanted_lines,
    other_lines    = m.other_lines,
    other_unpriced = m.other_unpriced,
    other_value    = m.other_value
FROM pub_contracts c,
LATERAL (
    SELECT
        COUNT(*) FILTER (WHERE i.is_included AND a.item_id IS NOT NULL) AS abyssal_lines,
        COUNT(*) FILTER (WHERE NOT i.is_included) AS wanted_lines,
        COUNT(*) FILTER (WHERE i.is_included AND a.item_id IS NULL) AS other_lines,
        COUNT(*) FILTER (WHERE i.is_included AND a.item_id IS NULL AND i.line_value IS NULL) AS other_unpriced,
        COALESCE(SUM(i.line_value) FILTER (WHERE i.is_included AND a.item_id IS NULL), 0) AS other_value
    FROM pub_contract_items i
    LEFT JOIN abyssal_items a ON a.item_id = i.item_id
    WHERE i.contract_id = c.contract_id
) m
WHERE c.contract_id = s.contract_id;

-- The item-first estimate (schema 069): an abyssal module is
-- priced on its family's ladder of ordinary modules (Tech II,
-- faction, deadspace, officer -- the same SDE group as its
-- source, same size class), placed by the same weighted
-- performance its score uses and priced between the rungs it
-- falls between; recreation cost caps it, and clean contract
-- listings only nudge a bounded per-family factor.
--   ladder_value      the uncalibrated ladder figure (what the
--                     calibration compares listings against)
--   ladder_lo_type_id the rung just below the item (0: none)
--   ladder_hi_type_id the rung just above it (0: none)
--   market_calib      the bounded market factor applied
ALTER TABLE abyssal_items ADD COLUMN IF NOT EXISTS ladder_value DOUBLE PRECISION;
ALTER TABLE abyssal_items ADD COLUMN IF NOT EXISTS ladder_lo_type_id BIGINT NOT NULL DEFAULT 0;
ALTER TABLE abyssal_items ADD COLUMN IF NOT EXISTS ladder_hi_type_id BIGINT NOT NULL DEFAULT 0;
ALTER TABLE abyssal_items ADD COLUMN IF NOT EXISTS market_calib DOUBLE PRECISION NOT NULL DEFAULT 1;
