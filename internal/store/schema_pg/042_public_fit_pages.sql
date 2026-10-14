-- Schema step 042: stored public fit pages.
--
-- A public fit page ran the fitting engine and wrote a view count on
-- every load, for anyone. The parts that cost work (the module
-- sections, the All-V simulation and the EFT text) are now built once
-- per version of the fit and stored here: when the owner saves it
-- public, and by the worker for any page missing or out of date.
-- fit_updated_at and sde_stamp say which version of the fit and of
-- the static data a page was built from; complete is false while a
-- name or the ship's data was still missing, so the worker builds it
-- again later.

CREATE TABLE IF NOT EXISTS public_fit_pages (
  fit_id BIGINT PRIMARY KEY REFERENCES local_fittings (id) ON DELETE CASCADE,
  fit_updated_at TIMESTAMPTZ NOT NULL,
  sde_stamp TEXT NOT NULL DEFAULT '',
  complete BOOLEAN NOT NULL DEFAULT TRUE,
  built_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  body JSONB NOT NULL
);
