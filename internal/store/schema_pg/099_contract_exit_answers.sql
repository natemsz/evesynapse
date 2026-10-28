-- The public items route's final answer about a departed contract
-- (schema 099, pub_contracts_sales.go), kept as the status ESI gave:
-- 204, 403, 404, or 200 when it was still listed. A 403 ("Contract
-- not public") is final but says nothing about a sale, so it settles
-- the probe without becoming evidence; the stored status is what a
-- later reading of 403 would be backfilled from.
ALTER TABLE abyssal_sightings ADD COLUMN IF NOT EXISTS sale_probe_status BIGINT;
ALTER TABLE true_value_contract_observations ADD COLUMN IF NOT EXISTS exit_status BIGINT;
ALTER TABLE true_value_contract_observations ADD COLUMN IF NOT EXISTS exit_probed_at TIMESTAMPTZ;
