-- Schema step 059: the indexes the October 2026 audit found
-- missing. Indexes only -- no table or column changes. Each one
-- is named for the stored query it serves; without them those
-- queries sequential-scan tables that grow with the corpus.

-- pg_trgm backs the entity_names search index below. It is a
-- stock contrib extension, created here because nothing else in
-- the schema set has needed an extension before.
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- ListMarketStatsFallbackByType reads by type alone, across
-- every region in region order; the table's primary key leads
-- with region_id, so the read scanned the whole mirror.
CREATE INDEX IF NOT EXISTS market_stats_fallback_type_idx
    ON market_stats_fallback (type_id, region_id);

-- The contract search's standing final sort (its named sorts
-- all tie-break here, and the default view sorts by it alone).
CREATE INDEX IF NOT EXISTS pub_contracts_issued_idx
    ON pub_contracts (date_issued DESC, contract_id DESC);

-- ListPublicContractsPendingItems drains the item-fetch queue
-- oldest first; only pending rows ever sit in it, so the index
-- holds just those.
CREATE INDEX IF NOT EXISTS pub_contracts_pending_items_idx
    ON pub_contracts (first_seen, contract_id)
    WHERE items_state = 'pending';

-- Contract reads grouped or filtered by the issuing
-- corporation (the search's issuer filter resolves names to
-- corporation ids and matches on this column) and by start
-- location (courier and sighting reads).
CREATE INDEX IF NOT EXISTS pub_contracts_issuer_corp_idx
    ON pub_contracts (issuer_corporation_id);
CREATE INDEX IF NOT EXISTS pub_contracts_start_location_idx
    ON pub_contracts (start_location_id);

-- The per-character detail reads (ListKillmailDetailsByCharacter,
-- ListContractDetailIDsByCharacter) and the account-delete
-- cascade both filter on character_id; the primary keys lead
-- with the detail id, so both scanned the full store.
CREATE INDEX IF NOT EXISTS killmail_details_character_idx
    ON killmail_details (character_id);
CREATE INDEX IF NOT EXISTS contract_details_character_idx
    ON contract_details (character_id);

-- The record drains (ListPilotDrains and its corporation and
-- alliance fellows) pick due rows by state, then highest
-- priority, then most recently noted. Column order and
-- directions mirror that ORDER BY.
CREATE INDEX IF NOT EXISTS pilot_records_drain_idx
    ON pilot_records (state, priority DESC, noted_at DESC NULLS LAST);
CREATE INDEX IF NOT EXISTS corporation_records_drain_idx
    ON corporation_records (state, priority DESC, noted_at DESC NULLS LAST);
CREATE INDEX IF NOT EXISTS alliance_records_drain_idx
    ON alliance_records (state, priority DESC, noted_at DESC NULLS LAST);

-- The contract search resolves issuer names with a substring
-- ILIKE over entity_names; a trigram index is what makes a
-- '%...%' pattern an index lookup instead of a scan.
CREATE INDEX IF NOT EXISTS entity_names_name_trgm_idx
    ON entity_names USING gin (name gin_trgm_ops);
