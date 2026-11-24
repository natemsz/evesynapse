-- Step 176: what a fleet asks of the pilots who sign up for it, apart
-- from who may: no sign-in to see where it forms up (none), an EVE
-- sign-in with no permissions (eve), or an EveSynapse character that
-- granted what the fleet tools read for a pilot (scopes). A fleet open
-- to "any EVE pilot" was the EVE sign-in under another name: it becomes
-- open to anyone with the EVE sign-in asked.
ALTER TABLE ops ADD COLUMN IF NOT EXISTS sign_in TEXT NOT NULL DEFAULT 'none';
ALTER TABLE ops DROP CONSTRAINT IF EXISTS ops_sign_in_check;
ALTER TABLE ops ADD CONSTRAINT ops_sign_in_check CHECK (sign_in IN ('none', 'eve', 'scopes'));
UPDATE ops SET join_gate = 'anyone', sign_in = 'eve' WHERE join_gate = 'verified';
