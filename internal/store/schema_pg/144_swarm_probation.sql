-- Schema step 144: probation for a swarm signer ESI caught lying.
-- probation_checks is how many of its next entries are verified
-- before sampling resumes; probation_since is when the lie was
-- caught, so its unchecked applies from the days before can be
-- taken back.
ALTER TABLE swarm_peers ADD COLUMN IF NOT EXISTS probation_checks INT NOT NULL DEFAULT 0;
ALTER TABLE swarm_peers ADD COLUMN IF NOT EXISTS probation_since timestamptz;
