-- Migration 027 (v0.3.14): the guide-price want marker. A kill
-- view that finds no prices anywhere leaves this singleton row
-- behind; the worker's urgent drain answers it with a guide
-- refresh when ESI's cache window allows, so killmail values
-- stop depending on someone having visited the Market page. One
-- row ever: the price guide is global, so one want covers every
-- viewer.

CREATE TABLE IF NOT EXISTS guide_price_wants (
    id        INTEGER PRIMARY KEY CHECK (id = 1),
    wanted_at TEXT NOT NULL DEFAULT '' -- RFC3339
);
