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
