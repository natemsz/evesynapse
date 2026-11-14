-- Ansiblex jump bridges by alliance (map_bridges.go). Written when a
-- corporation's structures sync: each Ansiblex the list names, read
-- off its name (the two systems it joins), with the corporation's alliance at that
-- time (0 for a corporation outside any alliance). The map shows a
-- reader the bridges of their own alliances and corporations, so one
-- director's sync serves every member of the alliance.
CREATE TABLE IF NOT EXISTS alliance_bridges (
    structure_id   BIGINT PRIMARY KEY,
    corporation_id BIGINT NOT NULL,
    alliance_id    BIGINT NOT NULL DEFAULT 0,
    from_system    BIGINT NOT NULL,
    to_system      BIGINT NOT NULL,
    name           TEXT NOT NULL DEFAULT '',
    updated_at     TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS alliance_bridges_alliance ON alliance_bridges (alliance_id) WHERE alliance_id <> 0;
CREATE INDEX IF NOT EXISTS alliance_bridges_corporation ON alliance_bridges (corporation_id);
