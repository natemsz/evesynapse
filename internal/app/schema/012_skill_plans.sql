-- Migration 012 (Phase 4): skill plans. Two halves:
--
--   1. The dogma skill graph (sde_skill_meta, sde_requirements),
--      imported from the Fuzzwork dump's dgmTypeAttributes.csv by
--      the SDE importer: per-skill rank and primary/secondary
--      attributes, plus every type's required-skill rows (the
--      skill prerequisites the plan engine expands, and the fit
--      requirements the plan-from-fit feature resolves).
--
--   2. The user's own skill plans (skill_plans,
--      skill_plan_items): a named, per-character list of
--      (skill, target level) in intent order. The engine re-sorts
--      topologically at render; position only records which
--      skills the user asked for first.
--
-- Unlinking a character deletes its plans with it (the characters
-- row is the anchor for everything a plan means).

CREATE TABLE IF NOT EXISTS sde_skill_meta (
    type_id       INTEGER PRIMARY KEY, -- EVE type ID of the skill
    rank          REAL    NOT NULL DEFAULT 1, -- skillTimeConstant (attribute 275)
    primary_attr  INTEGER NOT NULL DEFAULT 0, -- attribute 180: dogma attribute id (164-168)
    secondary_attr INTEGER NOT NULL DEFAULT 0 -- attribute 181
);

CREATE TABLE IF NOT EXISTS sde_requirements (
    type_id       INTEGER NOT NULL, -- the type that needs skills (module, ship, skill, ...)
    skill_type_id INTEGER NOT NULL, -- the required skill
    level         INTEGER NOT NULL, -- required trained level (1-5)
    PRIMARY KEY (type_id, skill_type_id)
);

-- Requirement lookups run both ways: a plan expands what a type
-- needs, and "what needs this skill" stays cheap to ask.
CREATE INDEX IF NOT EXISTS idx_sde_requirements_skill ON sde_requirements (skill_type_id);

CREATE TABLE IF NOT EXISTS skill_plans (
    id           INTEGER PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    character_id INTEGER NOT NULL REFERENCES characters (character_id) ON DELETE CASCADE,
    name         TEXT    NOT NULL,
    created_at   TEXT    NOT NULL -- RFC3339
);

CREATE INDEX IF NOT EXISTS idx_skill_plans_user_char ON skill_plans (user_id, character_id);

CREATE TABLE IF NOT EXISTS skill_plan_items (
    plan_id       INTEGER NOT NULL REFERENCES skill_plans (id) ON DELETE CASCADE,
    skill_type_id INTEGER NOT NULL,
    target_level  INTEGER NOT NULL, -- 1-5
    position      INTEGER NOT NULL, -- intent order within the plan
    PRIMARY KEY (plan_id, skill_type_id)
);
