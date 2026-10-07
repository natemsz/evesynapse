-- Schema step 007: per-user tables that were created without the
-- foreign key to users every other per-user table has. Nothing
-- tied their rows to an account that exists, and nothing removed
-- them with it.
--
-- Rows left behind by an account that is gone are deleted first
-- (there is no account to show them to); the constraint then
-- keeps new ones from appearing. The whole step runs in one
-- transaction, and each constraint is dropped before it is added
-- so the step is harmless to run on a database that already has
-- it.

DELETE FROM local_fittings WHERE user_id NOT IN (SELECT id FROM users);
ALTER TABLE local_fittings DROP CONSTRAINT IF EXISTS local_fittings_user_id_fkey;
ALTER TABLE local_fittings
    ADD CONSTRAINT local_fittings_user_id_fkey
    FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE;

DELETE FROM restock_targets WHERE user_id NOT IN (SELECT id FROM users);
ALTER TABLE restock_targets DROP CONSTRAINT IF EXISTS restock_targets_user_id_fkey;
ALTER TABLE restock_targets
    ADD CONSTRAINT restock_targets_user_id_fkey
    FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE;

DELETE FROM clone_names WHERE user_id NOT IN (SELECT id FROM users);
ALTER TABLE clone_names DROP CONSTRAINT IF EXISTS clone_names_user_id_fkey;
ALTER TABLE clone_names
    ADD CONSTRAINT clone_names_user_id_fkey
    FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE;

DELETE FROM wallet_history WHERE user_id NOT IN (SELECT id FROM users);
ALTER TABLE wallet_history DROP CONSTRAINT IF EXISTS wallet_history_user_id_fkey;
ALTER TABLE wallet_history
    ADD CONSTRAINT wallet_history_user_id_fkey
    FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE;
