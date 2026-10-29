package store

import (
	"context"
	"database/sql"
	_ "embed" // the schema steps below
	"fmt"
	"strings"
)

// The schema, one embedded file per step. schemaSteps lists them in
// order and migrateSchema applies whichever a database is missing,
// each in one transaction, recording it in schema_migrations. A new
// schema change is the next numbered file in schema_pg/, an embed
// here, and a line in schemaSteps.

// Step 001: the whole schema as of the move to Postgres.
//
//go:embed schema_pg/001_baseline.sql
var pgBaselineSchema string

// Step 002: the station leaderboard.
//
//go:embed schema_pg/002_station_leaderboard.sql
var pgStationLeaderboardSchema string

// Step 003: fitting metadata (is_public / is_draft on
// local_fittings).
//
//go:embed schema_pg/003_fit_metadata.sql
var pgFitMetadataSchema string

// Step 004: per-type market price TTL cache and industry
// cost index tracking.
//
//go:embed schema_pg/004_price_cache_costindex.sql
var pgPriceCacheSchema string

// Step 005: restock planner targets.
//
//go:embed schema_pg/005_restock.sql
var pgRestockSchema string

// Step 006: custom jump-clone names.
//
//go:embed schema_pg/006_clone_names.sql
var pgCloneNamesSchema string

// Step 007: foreign keys to users on the four per-user tables that
// lacked one. The first step applied purely by its record; steps
// 001–006 also carry a probe, for databases older than the record.
//
//go:embed schema_pg/007_user_foreign_keys.sql
var pgUserForeignKeysSchema string

// Step 008: the ETag each snapshot was stored with, so a refresh
// can ask ESI whether it changed instead of downloading it again.
//
//go:embed schema_pg/008_snapshot_etags.sql
var pgSnapshotETagsSchema string

// Steps 009–011: every time kept as TEXT becomes a timestamptz, one
// group of tables per step (accounts and snapshots; the market; the
// record and name caches).
//
//go:embed schema_pg/009_timestamps_accounts_snapshots.sql
var pgTimestampsAccountsSchema string

//go:embed schema_pg/010_timestamps_market.sql
var pgTimestampsMarketSchema string

//go:embed schema_pg/011_timestamps_records.sql
var pgTimestampsRecordsSchema string

//go:embed schema_pg/012_notifications.sql
var pgNotificationsSchema string

//go:embed schema_pg/013_push_subscriptions.sql
var pgPushSubscriptionsSchema string

//go:embed schema_pg/014_ops.sql
var pgOpsSchema string

//go:embed schema_pg/015_op_attendance.sql
var pgOpAttendanceSchema string

//go:embed schema_pg/016_discord_links.sql
var pgDiscordLinksSchema string

//go:embed schema_pg/017_discord_servers.sql
var pgDiscordServersSchema string

//go:embed schema_pg/018_discord_dm_status.sql
var pgDiscordDMStatusSchema string

//go:embed schema_pg/019_discord_role_rules.sql
var pgDiscordRoleRulesSchema string

//go:embed schema_pg/020_discord_auto_join.sql
var pgDiscordAutoJoinSchema string

//go:embed schema_pg/021_discord_kick.sql
var pgDiscordKickSchema string

//go:embed schema_pg/022_discord_channel_access.sql
var pgDiscordChannelAccessSchema string

//go:embed schema_pg/023_user_last_seen.sql
var pgUserLastSeenSchema string

//go:embed schema_pg/024_admin_owners.sql
var pgAdminOwnersSchema string

//go:embed schema_pg/025_srp.sql
var pgSRPSchema string

//go:embed schema_pg/026_doctrines.sql
var pgDoctrinesSchema string

//go:embed schema_pg/027_want_closeness.sql
var pgWantClosenessSchema string

// Step 028: typing-intent guesses get their own ring (below
// viewed, above orbit), and the history and type-detail queues
// gain the same priority the record queues already had.
//
//go:embed schema_pg/028_typing_guesses.sql
var pgTypingGuessesSchema string

// Step 029: foresight targets, moved from 027 after the
// want-closeness and typing rings claimed 027-028 on main.
//
//go:embed schema_pg/029_foresight.sql
var pgForesightSchema string

// Step 030: the shared cache across instances — this instance's
// identity, the peers it has heard of with their reputation
// ledger, staged entries waiting on a spot check, and the Sync
// page's counters.
//
//go:embed schema_pg/030_swarm.sql
var pgSwarmSchema string

// Step 031: the stored trade-ideas answer for the home page
// (one row, replaced per refresh; the widget renders stored
// text only, never waits on the model).
//
//go:embed schema_pg/031_trade_ideas.sql
var pgTradeIdeasSchema string

// Step 032: the corp pulse change log (seen member/structure
// IDs per corporation, plus recorded departures, arrivals and
// removals for the Corp pulse widget).
//
//go:embed schema_pg/032_pulse_events.sql
var pgPulseEventsSchema string

// Step 033: public discovery counters (views, stored price,
// rating aggregates) and the per-account fit ratings table.
//
//go:embed schema_pg/033_public_fits.sql
var pgPublicFitsSchema string

// Step 034: when a swarm peer last presented a key other than
// its pinned one (refused; shown to the operator).
//
//go:embed schema_pg/034_swarm_key_pin.sql
var pgSwarmKeyPinSchema string

// Step 035: provenance for applied swarm entries (source, signer,
// signed fetch time), so an excluded signer's data can be taken
// back; and the serving peer on staged entries.
//
//go:embed schema_pg/035_swarm_provenance.sql
var pgSwarmProvenanceSchema string

// Step 036: the next gossip time, drawn once a round and kept
// across restarts.
//
//go:embed schema_pg/036_swarm_next_gossip.sql
var pgSwarmNextGossipSchema string

// Step 037: how often a staged swarm entry has been deferred,
// so an undecidable one is dropped instead of waiting a week.
//
//go:embed schema_pg/037_swarm_staging_deferrals.sql
var pgSwarmStagingDeferralsSchema string

// Step 038: owner-signed peer addresses, and the pull rotation
// that replaces singling the seed out by a fixed ID.
//
//go:embed schema_pg/038_swarm_signed_addresses.sql
var pgSwarmSignedAddressesSchema string

// Step 039: the typing-guess ledger keeps its history (a guess
// is superseded, not deleted), so the hit rates read over a
// window see the whole window.
//
//go:embed schema_pg/039_typing_ledger.sql
var pgTypingLedgerSchema string

// Step 040: recount fit rating aggregates from fit_ratings (a
// doubled first rating was counted twice). Numbered clear of the
// steps other open branches carry.
//
//go:embed schema_pg/040_fit_rating_recount.sql
var pgFitRatingRecountSchema string

// Step 041: the public fit rank takes the global mean as an
// argument, computed once per query instead of once per row.
//
//go:embed schema_pg/041_public_fit_rank_once.sql
var pgPublicFitRankOnceSchema string

// Step 042: stored public fit pages (sections, simulation, EFT),
// built once per version of a fit instead of on every view.
//
//go:embed schema_pg/042_public_fit_pages.sql
var pgPublicFitPagesSchema string

// Step 043: the trade ideas' last ask and failure run, for the
// refresh backoff.
//
//go:embed schema_pg/043_trade_ideas_attempts.sql
var pgTradeIdeasAttemptsSchema string

// Step 044: swarm_staging drops checked_at, which was never set.
//
//go:embed schema_pg/044_swarm_staging_no_checked_at.sql
var pgSwarmStagingNoCheckedAtSchema string

// Step 045: every saved home layout goes back to the default once,
// for the overview's new grid.
//
//go:embed schema_pg/045_home_layout_reset.sql
var pgHomeLayoutResetSchema string

// Step 046: trade_ideas holds a second answer (row 2), the Trade
// ideas page's longer one.
//
//go:embed schema_pg/046_trade_ideas_page.sql
var pgTradeIdeasPageSchema string

// Step 047: entity_names, every known character, corporation and
// alliance name, backfilled from the stored records.
//
//go:embed schema_pg/047_entity_names.sql
var pgEntityNamesSchema string

//go:embed schema_pg/048_link_choices.sql
var pgLinkChoicesSchema string

// Step 049: trade_ideas rows, the scanner rows each stored
// trade-ideas answer was written from (see the step file).
//
//go:embed schema_pg/049_trade_ideas_rows.sql
var pgTradeIdeasRowsSchema string

// Step 050: the stored market book (top orders per side, kept by
// each completed region sweep), the sweep staging table's system
// id column, and site_settings for live operator switches.
//
//go:embed schema_pg/050_market_book_snapshot.sql
var pgMarketBookSnapshotSchema string

// Step 051: true values -- the worker's bottom-up build cost
// (and, for types with no real market, contract-estimated sell
// price) per manufacturable type, for the types CCP's adjusted
// price misprices (see the step file).
//
//go:embed schema_pg/051_true_values.sql
var pgTrueValuesSchema string

// Step 052: true-value evidence tiers -- true_values gains
// the provenance of its estimated sell (sell_source), and the
// contract crawl gains its longitudinal observation table,
// where a watched listing that vanishes before expiry turns
// into probable-sale evidence.
//
//go:embed schema_pg/052_true_value_evidence.sql
var pgTrueValueEvidenceSchema string

// Step 053: station-grain typical (median) prices on
// market_station_stats, distilled by the same sweeps that
// already compute them at region grain, so the spread scanner
// can price typical against typical instead of best extremes.
//
//go:embed schema_pg/053_station_typicals.sql
var pgStationTypicalsSchema string

// Step 054: the Fuzzwork fallback stats in their own table
// (never the sweep tables), with provenance per row, and the
// mirror's bookkeeping row.
//
//go:embed schema_pg/054_market_stats_fallback.sql
var pgMarketStatsFallbackSchema string

// Step 055: public contracts (the search corpus), the
// abyssal item corpus with its per-instance sighting
// history, and the mutaplasmid roll ranges the SDE importer
// keeps filled.
//
//go:embed schema_pg/055_public_contracts.sql
var pgPublicContractsSchema string

// Step 056: public contract appraisal results -- per-line
// price, tier and as-of on pub_contract_items, and the
// totals, coverage, tier mix and deal ratio on pub_contracts.
//
//go:embed schema_pg/056_pub_contract_appraisal.sql
var pgPublicContractAppraisalSchema string

// Step 057: abyssal estimate storage -- the scoring pass's
// results on abyssal_items (family, score, per-attribute
// percentiles, the estimate with its spread, basis and cap,
// and the recreation cross-check), read by the appraisal
// core and the contract pages.
//
//go:embed schema_pg/057_abyssal_estimates.sql
var pgAbyssalEstimatesSchema string

// Step 058: saved appraisals -- the appraisal tool's per-user
// snapshots (raw paste, its hash, the priced snapshot as shown,
// and a share token for the read-only public view).
//
//go:embed schema_pg/058_user_appraisals.sql
var pgUserAppraisalsSchema string

// Step 059: the audit's missing indexes -- the fallback stats
// by type, the contract search's sort/queue/filter indexes,
// the detail stores by character, the record drains, and a
// trigram index for entity-name search (with the pg_trgm
// extension it needs).
//
//go:embed schema_pg/059_audit_indexes.sql
var pgAuditIndexesSchema string

// Step 060: the public-contract item drain's failure
// bookkeeping -- an attempt counter and the latest failure's
// reason on pub_contracts, so a listing whose payload never
// parses can be parked instead of retried forever, and the
// index the drain's new newest-first walk reads by.
//
//go:embed schema_pg/060_pub_contract_items_parking.sql
var pgPubContractItemsParkingSchema string

// Step 061: blueprint copy medians -- the stored per-(type,
// runs) median ask of single-copy contracts in the public
// corpus, the one price a blueprint copy may take.
//
//go:embed schema_pg/061_blueprint_copy_medians.sql
var pgBlueprintCopyMediansSchema string

// Step 062: the build each swarm peer last claimed for
// itself, for the Admin page's Build column. Self-reported
// display metadata only; the trust model never reads it.
//
//go:embed schema_pg/062_swarm_peer_build.sql
var pgSwarmPeerBuildSchema string

// Step 063: the SDE's mutaplasmid applicability mappings
// (which base types a mutaplasmid can be applied to, and the
// type each becomes). The contract probe queue admits only
// instances of the resulting types.
//
//go:embed schema_pg/063_abyssal_mappings.sql
var pgAbyssalMappingsSchema string

// Step 064: appraisal lifecycle -- anonymous appraisal storage
// (shared anonymous owner, is_anonymous flag), last-viewed
// tracking for the 180-day anonymous expiry, and the secret
// that signs the anonymous-history cookie.
//
//go:embed schema_pg/064_appraisal_lifecycle.sql
var pgAppraisalLifecycleSchema string

// Step 065: remembered appraisal pricing basis per account.
//
//go:embed schema_pg/065_appraisal_preferences.sql
var pgAppraisalPreferencesSchema string

// Step 066: SDE reprocessing outputs, portion sizes, and the
// remembered refine rate in the appraisal preferences.
//
//go:embed schema_pg/066_reprocessing.sql
var pgReprocessingSchema string

// Step 067: anonymous fit sharing -- share tokens on
// local_fittings, the shared anonymous fit owner, and the
// fitting history cookie secret.
//
//go:embed schema_pg/067_anonymous_fits.sql
var pgAnonymousFitsSchema string

// Step 068: the public structures set the contract worker
// mirrors from ESI's public structures route, for the
// contract search's un-dockable-citadel filter.
//
//go:embed schema_pg/068_public_structures.sql
var pgPublicStructuresSchema string

// Step 069: abyssal sighting attribution -- what else shared a
// listing's contract and who listed it, stamped onto the sighting
// so the abyssal calibration corpus can attribute a contract's
// price to the item, and backfilled where the contract is stored.
//
//go:embed schema_pg/069_abyssal_attribution.sql
var pgAbyssalAttributionSchema string

// Step 070: the abyssal learned correction -- confirmed sales
// stamped onto sightings, each family's out-of-sample record,
// and which market adjustment an item's estimate carries.
//
//go:embed schema_pg/070_abyssal_learned.sql
var pgAbyssalLearnedSchema string

// Step 071: the abyssal ladder clamp -- every item priced past
// either end of its ladder, and every runaway stored ladder
// figure, queued first for rescoring under the clamp.
//
//go:embed schema_pg/071_abyssal_ladder_clamp.sql
var pgAbyssalLadderClampSchema string

// Step 072: the universe map -- system positions, constellations,
// security classes and factions, the stargate graph, and the map
// payload built from them.
//
//go:embed schema_pg/072_universe_map.sql
var pgUniverseMapSchema string

// Step 073: the universe map's live layers payload (kills, jumps,
// sovereignty, Faction Warfare, incursions), built by the worker.
//
//go:embed schema_pg/073_map_layers.sql
var pgMapLayersSchema string

// Step 074: the killboard -- every killmail (zKillboard's public
// stream and our characters' own), a summary kept forever, attackers
// and the full killmail kept a year, and the stream's cursor.
//
//go:embed schema_pg/074_killboard.sql
var pgKillboardSchema string

// Step 075: place activity -- ESI's public hourly per-system
// kills/jumps aggregates as history, the distilled per-system and
// per-region figures the place pages read, current sovereignty
// holders, and the collector's cache-window gate.
//
//go:embed schema_pg/075_place_activity.sql
var pgPlaceActivitySchema string

// Step 076: the shared demo account -- the is_demo flag on users
// and the demo reset clock.
//
//go:embed schema_pg/076_demo_account.sql
var pgDemoAccountSchema string

// Step 077: pasted scans (d-scan, local, fleet, probe), unlisted
// and kept 90 days.
//
//go:embed schema_pg/077_scans.sql
var pgScansSchema string

// Step 078: intel channels -- owned streams of intel reports, shared
// by grant (a character, corporation, alliance or group; read or
// read and post), drawn on the map; reports kept 7 days.
//
//go:embed schema_pg/078_intel_channels.sql
var pgIntelChannelsSchema string

// Step 079: intel posting tokens for EveSynapse Axon, kept
// as hashes.
//
//go:embed schema_pg/079_intel_tokens.sql
var pgIntelTokensSchema string

// Step 080: known hostiles -- the pilots each intel channel watches.
//
//go:embed schema_pg/080_intel_watch.sql
var pgIntelWatchSchema string

// Step 081: Axon pairing codes and per-device signing keys on
// intel_tokens.
//
//go:embed schema_pg/081_axon_pairing.sql
var pgAxonPairingSchema string

// Step 082: intel channels owned by a corporation or alliance, and
// rule grants judged by the standing model.
//
//go:embed schema_pg/082_intel_channel_orgs.sql
var pgIntelChannelOrgsSchema string

// Step 083: what intel reports say (internal/intelanalyze), and the
// names and affiliations read from them.
//
//go:embed schema_pg/083_intel_analysis.sql
var pgIntelAnalysisSchema string

// Step 084: the release builds the swarm's learner has
// recorded from the owner's GitHub releases.
//
//go:embed schema_pg/084_release_builds.sql
var pgReleaseBuildsSchema string

// Step 085: the SDE's authored 2D layout (position2D) on
// sde_systems, the universe map's improved projection source.
//
//go:embed schema_pg/085_system_position2d.sql
var pgSystemPosition2DSchema string

// Step 086: scan groups -- a scans.group_id naming the founding
// scan of the group a paste belongs to.
//
//go:embed schema_pg/086_scan_groups.sql
var pgScanGroupsSchema string

// Step 087: intel report groups -- an intel_reports.group_id
// naming the founding report of the group a channel report
// belongs to, and the intel_group_composites table holding each
// group's fused picture.
//
//go:embed schema_pg/087_intel_report_groups.sql
var pgIntelReportGroupsSchema string

// Step 088: pilot activity profiles -- the stored aggregates of
// a pilot's public kill record (pilot_activity.go).
//
//go:embed schema_pg/088_pilot_activity.sql
var pgPilotActivitySchema string

// Step 089: tracked gangs -- the fusion layer over report
// groups: one body of pilots tracked across the groups that
// sight it, with its identity evidence and latest picture.
//
//go:embed schema_pg/089_intel_gangs.sql
var pgIntelGangsSchema string

// Step 090: celestials -- the SDE's named in-system places
// (schema 090), what a kill's location_id resolves against.
//
//go:embed schema_pg/090_sde_celestials.sql
var pgSDECelestialsSchema string

// Step 091: the residue step's store -- every completed pass
// over a settled reading's leftover spans, logged whole, and
// the notes the pass leaves on the reading (residue_model.go).
//
//go:embed schema_pg/091_intel_residue.sql
var pgIntelResidueSchema string

// Step 092: residue attempt accounting -- failed passes counted
// per report with a backoff gate and a dead-letter reason, and
// the report text hash on completed calls (residue_model.go).
//
//go:embed schema_pg/092_intel_residue_attempts.sql
var pgIntelResidueAttemptsSchema string

// Step 093: per-line kill prices, stored at ingest
// (killfeed.go).
//
//go:embed schema_pg/093_kill_line_prices.sql
var pgKillLinePricesSchema string

// Step 094: Axon submission labels -- a submission sent with an
// override, beside what the site detected (intel_preview.go).
//
//go:embed schema_pg/094_intel_submit_labels.sql
var pgIntelSubmitLabelsSchema string

// Step 095: intel threads -- report groups on one system or on
// neighbouring ones, close in time, linked as one engagement
// (intel_threads.go).
//
//go:embed schema_pg/095_intel_threads.sql
var pgIntelThreadsSchema string

// Step 096: public contract departures -- a completed sweep keeps
// the contracts that left, stamped with when and how, and prunes
// them after a retention window (pub_contracts.go).
//
//go:embed schema_pg/096_pub_contract_departures.sql
var pgPubContractDeparturesSchema string

// Step 097: the Admin analytics recorder's aggregate tables
// (per-minute rows, hourly rollups, cycle rows, the inventory
// snapshot) plus created_at indexes for the Activity counts.
//
//go:embed schema_pg/097_admin_stats.sql
var pgAdminStatsSchema string

// Step 098: the WebAuthn second factor's tables (credentials,
// one-time recovery codes, the audit trail).
//
//go:embed schema_pg/098_webauthn.sql
var pgWebAuthnSchema string

// Step 099: the public items route's final answer about a departed
// contract, kept on the abyssal sightings and the true-value
// observations, so a 403 settles the exit probe without counting as
// evidence either way.
//
//go:embed schema_pg/099_contract_exit_answers.sql
var pgContractExitAnswersSchema string

// Step 100: durable name misses, the IDs ESI has definitively said
// are not a character or a type, so a restart does not re-ask them.
//
//go:embed schema_pg/100_name_misses.sql
var pgNameMissesSchema string

// Step 101: the market stat pack: the full order-book statistics set
// and the trend figures on the per-(region, type) stats row, the same
// set on its daily copy, and the Fuzzwork mirror's full columns.
//
//go:embed schema_pg/101_market_stat_pack.sql
var pgMarketStatPackSchema string

// Step 102: the LP store browser: the mirrored NPC loyalty stores
// and each offer's value per swept hub region, from the stat pack.
//
//go:embed schema_pg/102_lp_stores.sql
var pgLPStoresSchema string

// Step 103: market baskets: presets and players' own lists of types,
// and each basket's stored value per day, from the stat pack.
//
//go:embed schema_pg/103_market_baskets.sql
var pgMarketBasketsSchema string

// Step 104: the profit ledger: every wallet journal row, transaction
// and delivered job ever seen, and the profit attribution written
// from them at ingest.
//
//go:embed schema_pg/104_profit_ledger.sql
var pgProfitLedgerSchema string

// Step 105: beaten orders: what beat each open order at its station,
// the suggested reprice and the margin at it, on order_lifecycle.
//
//go:embed schema_pg/105_beaten_orders.sql
var pgBeatenOrdersSchema string

// Step 106: the market change feed, written from the stat pack when a
// region's sweep completes.
//
//go:embed schema_pg/106_market_changes.sql
var pgMarketChangesSchema string

// Step 108: the market API: hashed read-only keys and their daily
// usage, abyssal items callers asked to have priced, and the
// researched-original blueprint medians.
//
//go:embed schema_pg/108_market_api.sql
var pgMarketAPISchema string

//go:embed schema_pg/110_watchlist_baskets.sql
var pgWatchlistBasketsSchema string

// Step 107: global notices, the banners an administrator posts to
// every visitor, their per-account dismissals and the audit trail.
//
//go:embed schema_pg/107_global_notices.sql
var pgGlobalNoticesSchema string

// Step 109: briefing dismissals, the briefing lines an account has
// hidden (they still render, in the dismissed list, while they
// apply).
//
//go:embed schema_pg/109_briefing_dismissals.sql
var pgBriefingDismissalsSchema string

// Step 111: visualization preferences, one JSON object of chart
// display switches on the account.
//
//go:embed schema_pg/111_viz_prefs.sql
var pgVizPrefsSchema string

// Step 112: per-item trading P&L on a pooled cost basis: account,
// character and corporation pools, paired sales tax, allocated broker
// fees, typed gaps, and the per-item daily and window rollups.
//
//go:embed schema_pg/112_profit_trading.sql
var pgProfitTradingSchema string

// Step 113: per-blueprint industry P&L: recipe facts on landed jobs,
// the materials each consumed from the pool's stock, what became of
// the output, shared recipe values at today's prices, and the
// per-blueprint summary.
//
//go:embed schema_pg/113_profit_industry.sql
var pgProfitIndustrySchema string

// Step 114: LP earnings per store: LP balance samples, purchases
// inferred from them with confidence tiers, what the units sold for,
// the per-store summary, and the LP delta on the daily rows.
//
//go:embed schema_pg/114_profit_lp.sql
var pgProfitLPSchema string

// schemaStep is one numbered file in schema_pg. Steps apply in
// version order, each exactly once per database; schema_migrations
// records which ones have landed.
type schemaStep struct {
	version int
	name    string
	script  string
	// legacyProbe counts the objects the step creates. It exists
	// for databases that predate schema_migrations: a step whose
	// objects are already there is recorded as applied instead of
	// being run again. Steps added from here on leave it empty —
	// the record alone decides.
	legacyProbe string
}

// schemaSteps lists every schema step in order. A new schema
// change is a new numbered file in schema_pg, an embed for it
// above, and one line here (no probe).
func schemaSteps() []schemaStep {
	return []schemaStep{
		// The baseline carries the sessions table too (pgxstore
		// does not create it).
		{1, "baseline", pgBaselineSchema,
			`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'users'`},
		{2, "station_leaderboard", pgStationLeaderboardSchema,
			`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'market_station_leaderboard'`},
		{3, "fit_metadata", pgFitMetadataSchema,
			`SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'local_fittings' AND column_name = 'is_public'`},
		{4, "price_cache_costindex", pgPriceCacheSchema,
			`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'market_type_prices'`},
		{5, "restock", pgRestockSchema,
			`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'restock_targets'`},
		{6, "clone_names", pgCloneNamesSchema,
			`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'clone_names'`},
		// From here on the record alone decides: no probes.
		{7, "user_foreign_keys", pgUserForeignKeysSchema, ""},
		{8, "snapshot_etags", pgSnapshotETagsSchema, ""},
		{9, "timestamps_accounts_snapshots", pgTimestampsAccountsSchema, ""},
		{10, "timestamps_market", pgTimestampsMarketSchema, ""},
		{11, "timestamps_records", pgTimestampsRecordsSchema, ""},
		{12, "notifications", pgNotificationsSchema, ""},
		{13, "push_subscriptions", pgPushSubscriptionsSchema, ""},
		{14, "ops", pgOpsSchema, ""},
		{15, "op_attendance", pgOpAttendanceSchema, ""},
		{16, "discord_links", pgDiscordLinksSchema, ""},
		{17, "discord_servers", pgDiscordServersSchema, ""},
		{18, "discord_dm_status", pgDiscordDMStatusSchema, ""},
		{19, "discord_role_rules", pgDiscordRoleRulesSchema, ""},
		{20, "discord_auto_join", pgDiscordAutoJoinSchema, ""},
		{21, "discord_kick", pgDiscordKickSchema, ""},
		{22, "discord_channel_access", pgDiscordChannelAccessSchema, ""},
		{23, "user_last_seen", pgUserLastSeenSchema, ""},
		{24, "admin_owners", pgAdminOwnersSchema, ""},
		{25, "srp", pgSRPSchema, ""},
		{26, "doctrines", pgDoctrinesSchema, ""},
		{27, "want_closeness", pgWantClosenessSchema, ""},
		{28, "typing_guesses", pgTypingGuessesSchema, ""},
		{29, "foresight", pgForesightSchema, ""},
		{30, "swarm", pgSwarmSchema, ""},
		{31, "trade_ideas", pgTradeIdeasSchema, ""},
		{32, "pulse_events", pgPulseEventsSchema, ""},
		{33, "public_fits", pgPublicFitsSchema, ""},
		{34, "swarm_key_pin", pgSwarmKeyPinSchema, ""},
		{35, "swarm_provenance", pgSwarmProvenanceSchema, ""},
		{36, "swarm_next_gossip", pgSwarmNextGossipSchema, ""},
		{37, "swarm_staging_deferrals", pgSwarmStagingDeferralsSchema, ""},
		{38, "swarm_signed_addresses", pgSwarmSignedAddressesSchema, ""},
		{39, "typing_ledger", pgTypingLedgerSchema, ""},
		{40, "fit_rating_recount", pgFitRatingRecountSchema, ""},
		{41, "public_fit_rank_once", pgPublicFitRankOnceSchema, ""},
		{42, "public_fit_pages", pgPublicFitPagesSchema, ""},
		{43, "trade_ideas_attempts", pgTradeIdeasAttemptsSchema, ""},
		{44, "swarm_staging_no_checked_at", pgSwarmStagingNoCheckedAtSchema, ""},
		{45, "home_layout_reset", pgHomeLayoutResetSchema, ""},
		{46, "trade_ideas_page", pgTradeIdeasPageSchema, ""},
		{47, "entity_names", pgEntityNamesSchema, ""},
		{48, "link_choices", pgLinkChoicesSchema, ""},
		{49, "trade_ideas_rows", pgTradeIdeasRowsSchema, ""},
		{50, "market_book_snapshot", pgMarketBookSnapshotSchema, ""},
		{51, "true_values", pgTrueValuesSchema, ""},
		{52, "true_value_evidence", pgTrueValueEvidenceSchema, ""},
		{53, "station_typicals", pgStationTypicalsSchema, ""},
		{54, "market_stats_fallback", pgMarketStatsFallbackSchema, ""},
		{55, "public_contracts", pgPublicContractsSchema, ""},
		{56, "public_contract_appraisal", pgPublicContractAppraisalSchema, ""},
		{57, "abyssal_estimates", pgAbyssalEstimatesSchema, ""},
		{58, "user_appraisals", pgUserAppraisalsSchema, ""},
		{59, "audit_indexes", pgAuditIndexesSchema, ""},
		{60, "pub_contract_items_parking", pgPubContractItemsParkingSchema, ""},
		{61, "blueprint_copy_medians", pgBlueprintCopyMediansSchema, ""},
		{62, "swarm_peer_build", pgSwarmPeerBuildSchema, ""},
		{63, "abyssal_mappings", pgAbyssalMappingsSchema, ""},
		{64, "appraisal_lifecycle", pgAppraisalLifecycleSchema, ""},
		{65, "appraisal_preferences", pgAppraisalPreferencesSchema, ""},
		{66, "reprocessing", pgReprocessingSchema, ""},
		{67, "anonymous_fits", pgAnonymousFitsSchema, ""},
		{68, "public_structures", pgPublicStructuresSchema, ""},
		{69, "abyssal_attribution", pgAbyssalAttributionSchema, ""},
		{70, "abyssal_learned", pgAbyssalLearnedSchema, ""},
		{71, "abyssal_ladder_clamp", pgAbyssalLadderClampSchema, ""},
		{72, "universe_map", pgUniverseMapSchema, ""},
		{73, "map_layers", pgMapLayersSchema, ""},
		{74, "killboard", pgKillboardSchema, ""},
		{75, "place_activity", pgPlaceActivitySchema, ""},
		{76, "demo_account", pgDemoAccountSchema, ""},
		{77, "scans", pgScansSchema, ""},
		{78, "intel_channels", pgIntelChannelsSchema, ""},
		{79, "intel_tokens", pgIntelTokensSchema, ""},
		{80, "intel_watch", pgIntelWatchSchema, ""},
		{81, "axon_pairing", pgAxonPairingSchema, ""},
		{82, "intel_channel_orgs", pgIntelChannelOrgsSchema, ""},
		{83, "intel_analysis", pgIntelAnalysisSchema, ""},
		{84, "release_builds", pgReleaseBuildsSchema, ""},
		{85, "system_position2d", pgSystemPosition2DSchema, ""},
		{86, "scan_groups", pgScanGroupsSchema, ""},
		{87, "intel_report_groups", pgIntelReportGroupsSchema, ""},
		{88, "pilot_activity", pgPilotActivitySchema, ""},
		{89, "intel_gangs", pgIntelGangsSchema, ""},
		{90, "sde_celestials", pgSDECelestialsSchema, ""},
		{91, "intel_residue", pgIntelResidueSchema, ""},
		{92, "intel_residue_attempts", pgIntelResidueAttemptsSchema, ""},
		{93, "kill_line_prices", pgKillLinePricesSchema, ""},
		{94, "intel_submit_labels", pgIntelSubmitLabelsSchema, ""},
		{95, "intel_threads", pgIntelThreadsSchema, ""},
		{96, "pub_contract_departures", pgPubContractDeparturesSchema, ""},
		{97, "admin_stats", pgAdminStatsSchema, ""},
		{98, "webauthn", pgWebAuthnSchema, ""},
		{99, "contract_exit_answers", pgContractExitAnswersSchema, ""},
		{100, "name_misses", pgNameMissesSchema, ""},
		{101, "market_stat_pack", pgMarketStatPackSchema, ""},
		{102, "lp_stores", pgLPStoresSchema, ""},
		{103, "market_baskets", pgMarketBasketsSchema, ""},
		{104, "profit_ledger", pgProfitLedgerSchema, ""},
		{105, "beaten_orders", pgBeatenOrdersSchema, ""},
		{106, "market_changes", pgMarketChangesSchema, ""},
		{107, "global_notices", pgGlobalNoticesSchema, ""},
		{108, "market_api", pgMarketAPISchema, ""},
		{109, "briefing_dismissals", pgBriefingDismissalsSchema, ""},
		{110, "watchlist_baskets", pgWatchlistBasketsSchema, ""},
		{111, "viz_prefs", pgVizPrefsSchema, ""},
		{112, "profit_trading", pgProfitTradingSchema, ""},
		{113, "profit_industry", pgProfitIndustrySchema, ""},
		{114, "profit_lp", pgProfitLPSchema, ""},
	}
}

// schemaMigrationsDDL is the record of applied steps. It lives
// here rather than in schema_pg because it has to exist before any
// step can be recorded.
const schemaMigrationsDDL = `CREATE TABLE IF NOT EXISTS schema_migrations (
	version    INTEGER PRIMARY KEY,
	name       TEXT NOT NULL,
	applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`

// migrateSchema applies every schema step the database is missing,
// in order.
func migrateSchema(ctx context.Context, conn *sql.DB) error {
	if _, err := conn.ExecContext(ctx, schemaMigrationsDDL); err != nil {
		return fmt.Errorf("schema: create schema_migrations: %w", err)
	}
	for _, step := range schemaSteps() {
		if err := applySchemaStep(ctx, conn, step); err != nil {
			return fmt.Errorf("schema step %03d (%s): %w", step.version, step.name, err)
		}
	}
	return nil
}

// applySchemaStep lands one step atomically: its statements and
// its schema_migrations row commit together or not at all (DDL is
// transactional in Postgres). A step that fails halfway therefore
// leaves nothing behind for the next boot to mistake for a
// finished schema — it simply runs again. The advisory lock keeps
// two processes starting at the same moment (the server and a
// maintenance run) from applying the same step twice; it is
// released with the transaction.
func applySchemaStep(ctx context.Context, conn *sql.DB, step schemaStep) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // no-op once committed

	// 1165387091 is "EveS" in ASCII (0x45766553): an arbitrary
	// key this app alone takes.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(1165387091)`); err != nil {
		return err
	}
	var recorded int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_migrations WHERE version = $1`, step.version,
	).Scan(&recorded); err != nil {
		return err
	}
	if recorded > 0 {
		return tx.Commit()
	}

	present := 0
	if step.legacyProbe != "" {
		if err := tx.QueryRowContext(ctx, step.legacyProbe).Scan(&present); err != nil {
			return err
		}
	}
	if present == 0 {
		for _, stmt := range splitSchemaStatements(step.script) {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`, step.version, step.name,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// splitSchemaStatements cuts a schema script into its statements (the
// pgx stdlib driver executes one statement at a time). The split is
// lexically aware: semicolons inside string literals, quoted
// identifiers, comments, and dollar-quoted function bodies do not end a
// statement, and comments are stripped.
func splitSchemaStatements(script string) []string {
	var stmts []string
	var cur strings.Builder
	flush := func() {
		if stmt := strings.TrimSpace(cur.String()); stmt != "" {
			stmts = append(stmts, stmt)
		}
		cur.Reset()
	}
	i, n := 0, len(script)
	for i < n {
		c := script[i]
		switch {
		case c == ';':
			flush()
			i++
		case c == '\'':
			i = copyQuoted(&cur, script, i, '\'', isEscapeStringPrefix(script, i))
		case c == '"':
			i = copyQuoted(&cur, script, i, '"', false)
		case c == '-' && i+1 < n && script[i+1] == '-':
			// Line comment to the end of the line; the newline that
			// ends it is not skipped, so the tokens around it cannot fuse.
			j := i + 2
			for j < n && script[j] != '\n' {
				j++
			}
			i = j
		case c == '/' && i+1 < n && script[i+1] == '*':
			// Block comment, which nests in Postgres; a space
			// stays behind for the same reason as above.
			depth := 1
			j := i + 2
			for j < n && depth > 0 {
				if script[j] == '/' && j+1 < n && script[j+1] == '*' {
					depth++
					j += 2
				} else if script[j] == '*' && j+1 < n && script[j+1] == '/' {
					depth--
					j += 2
				} else {
					j++
				}
			}
			cur.WriteByte(' ')
			i = j
		case c == '$':
			if end, ok := scanDollarQuote(script, i); ok {
				cur.WriteString(script[i:end])
				i = end
			} else {
				cur.WriteByte(c) // a $1 parameter, not a quote
				i++
			}
		default:
			cur.WriteByte(c)
			i++
		}
	}
	flush()
	return stmts
}

// copyQuoted copies a '- or "-quoted span starting at script[i]
// (the opening quote) into cur and returns the index past it. A
// doubled quote is an escaped quote; with escapes (E'...' strings)
// a backslash escapes the next byte too. An unterminated span
// copies to the end and lets Postgres report the error.
func copyQuoted(cur *strings.Builder, script string, i int, quote byte, escapes bool) int {
	j, n := i+1, len(script)
	for j < n {
		if escapes && script[j] == '\\' {
			j += 2
			continue
		}
		if script[j] == quote {
			if j+1 < n && script[j+1] == quote {
				j += 2
				continue
			}
			j++
			break
		}
		j++
	}
	if j > n {
		j = n
	}
	cur.WriteString(script[i:j])
	return j
}

// isEscapeStringPrefix reports whether the quote at script[i] opens
// an E'...' escape string: an E immediately before it that is not
// itself part of a longer identifier.
func isEscapeStringPrefix(script string, i int) bool {
	if i == 0 || (script[i-1] != 'e' && script[i-1] != 'E') {
		return false
	}
	if i >= 2 && isIdentByte(script[i-2]) {
		return false
	}
	return true
}

func isIdentByte(c byte) bool {
	return c == '_' || c == '$' ||
		(c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// scanDollarQuote matches the dollar-quoted body starting at
// script[i] (the opening $) and returns the index past its closing
// tag. It reports false when the $ opens no quote (a $1 parameter)
// or the body never closes (copied literally; Postgres reports it).
func scanDollarQuote(script string, i int) (int, bool) {
	j, n := i+1, len(script)
	if j < n && script[j] >= '0' && script[j] <= '9' {
		return 0, false // $1: a parameter (tags never start with a digit)
	}
	for j < n && script[j] != '$' {
		if !isDollarTagByte(script[j]) {
			return 0, false
		}
		j++
	}
	if j >= n {
		return 0, false
	}
	tag := script[i : j+1]
	end := strings.Index(script[j+1:], tag)
	if end < 0 {
		return 0, false
	}
	return j + 1 + end + len(tag), true
}

func isDollarTagByte(c byte) bool {
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
