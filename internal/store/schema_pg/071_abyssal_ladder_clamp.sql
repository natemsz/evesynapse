-- Abyssal ladder clamp (schema 071).
--
-- An item past the top rung of its ladder used to extrapolate
-- along the ladder's last step, and a step fitted between two
-- rungs at nearly the same index could be almost vertical: some
-- stored ladder figures ran to 1e174 and Infinity. Recreation
-- cost capped every displayed estimate, but the stored figure
-- was what the market factor and the learned correction read.
-- The ladder now prices past the top rung at the top rung, and
-- below the bottom rung falls to at most a tenth of it.
--
-- Every item whose stored figure came from either end of its
-- ladder, and every runaway figure, is put at the front of the
-- scoring queue (the due list takes NULL scored_at first), so
-- the next passes rewrite them under the clamp. Until then the
-- corpus reads a runaway figure as missing.
UPDATE abyssal_items SET scored_at = NULL
WHERE est_scope IN ('above', 'below', 'single')
   OR ladder_value = 'NaN'::double precision
   OR ladder_value > 1e13
   OR (recreation_cost > 0 AND ladder_value > 10 * recreation_cost);
