-- Schema step 015: who attended an op (the PAP).
--
-- op_attendance is one row per character seen on an op. The usual
-- source is the fleet itself: while the op runs, the worker reads the
-- fleet commander's fleet from ESI and records its members. Where
-- that cannot be done (the commander is not linked here, is not the
-- fleet boss, or has not granted fleet access) a manager marks
-- attendance by hand instead. A row keeps where it came from; a hand
-- mark never overwrites what the fleet reported.
--
-- capture_status on the op says how the automatic capture is going,
-- so the op's page can explain why hand-marking is being offered.

CREATE TABLE IF NOT EXISTS op_attendance (
    op_id         BIGINT NOT NULL REFERENCES ops (id) ON DELETE CASCADE,
    character_id  BIGINT NOT NULL,
    ship_type_id  BIGINT NOT NULL DEFAULT 0,
    source        TEXT NOT NULL,
    first_seen_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (op_id, character_id)
);

CREATE INDEX IF NOT EXISTS op_attendance_character ON op_attendance (character_id);

ALTER TABLE ops ADD COLUMN IF NOT EXISTS capture_status TEXT NOT NULL DEFAULT '';
ALTER TABLE ops ADD COLUMN IF NOT EXISTS capture_checked_at timestamptz;
