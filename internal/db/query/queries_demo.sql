-- ---------------------------------------------------------------------
-- The shared demo account (schema 076). One user row carries the
-- is_demo flag; these are the reads the sign-in route, the write
-- gates and the worker's eligibility check key on, the reset clock,
-- and the reset's wipe of everything the demo account owns. The
-- wipe is what makes re-seeding idempotent: seedDemoData clears
-- the demo-owned rows, then writes the seed state in full.
-- ---------------------------------------------------------------------

-- name: GetDemoUser :one
SELECT * FROM users WHERE is_demo <> 0 ORDER BY id LIMIT 1;

-- name: MarkUserDemo :exec
UPDATE users SET is_demo = 1 WHERE id = $1;

-- name: GetDemoLastReset :one
SELECT last_reset_at FROM demo_state WHERE id = 1;

-- name: StampDemoReset :exec
INSERT INTO demo_state (id, last_reset_at) VALUES (1, $1)
ON CONFLICT (id) DO UPDATE SET last_reset_at = EXCLUDED.last_reset_at;

-- name: DeleteDemoSnapshots :exec
DELETE FROM character_snapshots WHERE character_id = $1;

-- name: DeleteDemoCharacterCorporation :exec
DELETE FROM character_corporations WHERE character_id = $1;

-- name: DeleteDemoFittings :exec
DELETE FROM local_fittings WHERE user_id = $1;

-- name: DeleteDemoSkillPlans :exec
DELETE FROM skill_plans WHERE user_id = $1;

-- name: DeleteDemoNotifications :exec
DELETE FROM notifications WHERE user_id = $1;

-- name: DeleteDemoNotificationSeen :exec
DELETE FROM notification_seen WHERE user_id = $1;

-- name: DeleteDemoWidgetConfigs :exec
DELETE FROM widget_configs WHERE user_id = $1;

-- name: DeleteDemoAppraisals :exec
DELETE FROM user_appraisals WHERE user_id = $1;

-- name: DeleteDemoWalletHistory :exec
DELETE FROM wallet_history WHERE character_id = $1;

-- name: DeleteDemoOrderHealth :exec
DELETE FROM order_health WHERE character_id = $1;

-- name: DeleteDemoOrderLifecycle :exec
DELETE FROM order_lifecycle WHERE character_id = $1;

-- name: DeleteDemoContractDetails :exec
DELETE FROM contract_details WHERE character_id = $1;

-- name: DeleteDemoKillmailDetails :exec
DELETE FROM killmail_details WHERE character_id = $1;

-- name: DeleteDemoWatchlist :exec
DELETE FROM market_watchlist WHERE user_id = $1;

-- name: DeleteDemoLinkChoices :exec
DELETE FROM account_link_choices WHERE user_id = $1;

-- name: DeleteDemoPushSubscriptions :exec
DELETE FROM push_subscriptions WHERE user_id = $1;

-- name: ResetDemoHomeLayout :exec
UPDATE users SET home_layout = '' WHERE id = $1;

-- name: DeleteDemoBlueprintPins :exec
DELETE FROM blueprint_pins WHERE user_id = $1;

-- name: DeleteDemoRestockTargets :exec
DELETE FROM restock_targets WHERE user_id = $1;

-- name: DeleteDemoBaskets :exec
-- The demo account's own baskets; their items and value history
-- go with them (ON DELETE CASCADE). Presets have no user.
DELETE FROM market_baskets WHERE user_id = $1;

-- name: DeleteDemoDoctrines :exec
-- The demo corporation's doctrines; their fits cascade.
DELETE FROM doctrines WHERE corporation_id = $1;

-- name: DeleteDemoOps :exec
-- The demo corporation's ops; SRP claims filed against them cascade.
DELETE FROM ops WHERE corporation_id = $1;

-- name: DeleteDemoOrgGroups :exec
-- The demo corporation's groups; their members cascade.
DELETE FROM org_groups WHERE owner_kind = 'corporation' AND owner_id = $1;

-- name: DeleteDemoBattleFights :exec
-- The demo account's battle reports; their series, parties,
-- weapons, tackles, kills, moments and findings go with them
-- (ON DELETE CASCADE). The seed's fight times move with every
-- seed, so without this the reports would stack up.
DELETE FROM battle_fights WHERE user_id = $1;

-- The kills the demo seed's battle reports join to, in the
-- shared kill tables. A stored kill keeps its first copy's time
-- and attackers, and the seed replants its killmail details with
-- fresh times on every seed, so the wipe clears the battles'
-- mails for the battles step to merge the fresh copies back in.
-- name: DeleteDemoBattleKills :exec
DELETE FROM kills WHERE killmail_id = ANY(sqlc.arg(killmail_ids)::bigint[]);

-- name: DeleteDemoBattleKillAttackers :exec
DELETE FROM kill_attackers WHERE killmail_id = ANY(sqlc.arg(killmail_ids)::bigint[]);

-- name: DeleteDemoBattleKillDetails :exec
DELETE FROM kill_details WHERE killmail_id = ANY(sqlc.arg(killmail_ids)::bigint[]);

-- name: DeleteDemoBattleKillPrices :exec
DELETE FROM kill_line_prices WHERE killmail_id = ANY(sqlc.arg(killmail_ids)::bigint[]);

-- The demo cast's pilot activity profiles, folded by the seed
-- from its planted fights. The fold's applied markers would
-- otherwise hold the first seed's aggregates while the fights
-- are replanted with fresh times, so a reseed clears the fold
-- for the cast and folds it again.
-- name: DeleteDemoPilotActivityProfiles :exec
DELETE FROM pilot_activity_profiles WHERE character_id = ANY(sqlc.arg(character_ids)::bigint[]);

-- name: DeleteDemoPilotActivityShips :exec
DELETE FROM pilot_activity_ships WHERE character_id = ANY(sqlc.arg(character_ids)::bigint[]);

-- name: DeleteDemoPilotActivitySystems :exec
DELETE FROM pilot_activity_systems WHERE character_id = ANY(sqlc.arg(character_ids)::bigint[]);

-- name: DeleteDemoPilotActivityHours :exec
DELETE FROM pilot_activity_hours WHERE character_id = ANY(sqlc.arg(character_ids)::bigint[]);

-- name: DeleteDemoPilotActivityApplied :exec
DELETE FROM pilot_activity_applied WHERE character_id = ANY(sqlc.arg(character_ids)::bigint[]);
