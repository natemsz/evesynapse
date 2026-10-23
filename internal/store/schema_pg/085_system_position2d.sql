-- The map's authored 2D layout (schema 085): mapSolarSystems.csv
-- also carries position2Dx and position2Dy, CCP's own flat-map
-- coordinates for each system -- the positions the game's 2D map
-- draws. sde_systems gains them so the universe map's improved
-- projection can read the authored layout from stored rows.
-- Values stay 0 until the next import (sde_import_version 12
-- forces one).
ALTER TABLE sde_systems ADD COLUMN IF NOT EXISTS position2d_x DOUBLE PRECISION NOT NULL DEFAULT 0;
ALTER TABLE sde_systems ADD COLUMN IF NOT EXISTS position2d_y DOUBLE PRECISION NOT NULL DEFAULT 0;
