-- Schema step 066: reprocessing outputs and refine settings.
--
-- sde_reprocessing is the SDE's per-type reprocessing bill:
-- one row per (type, output material) with the quantity one
-- portion yields before any refine rate. The portion size
-- itself rides on sde_type_physics from invTypes. The
-- appraisal's reprocess view reads these stored rows only.
--
-- The remembered refine rate joins the appraisal preferences
-- (schema 065), persisted the same way as the pricing basis.

CREATE TABLE IF NOT EXISTS sde_reprocessing (
    type_id         BIGINT NOT NULL,
    material_type_id BIGINT NOT NULL,
    quantity        BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (type_id, material_type_id)
);

ALTER TABLE sde_type_physics
    ADD COLUMN IF NOT EXISTS portion_size BIGINT NOT NULL DEFAULT 0;

ALTER TABLE user_appraisal_prefs
    ADD COLUMN IF NOT EXISTS refine_rate DOUBLE PRECISION NOT NULL DEFAULT 100;
ALTER TABLE user_appraisal_prefs
    ADD COLUMN IF NOT EXISTS prune_ores BOOLEAN NOT NULL DEFAULT true;
