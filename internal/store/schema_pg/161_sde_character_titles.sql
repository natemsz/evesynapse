-- Cosmetic character title definitions from chrTitles.csv (UUID -> title name).
CREATE TABLE IF NOT EXISTS sde_character_titles (
    title_id TEXT PRIMARY KEY,
    name     TEXT NOT NULL
);
