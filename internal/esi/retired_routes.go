package esi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
)

// CCP moved the game's static data out of ESI into the SDE API: from
// compatibility date 2026-11-19 the /universe routes that served it
// are gone, apart from the ones listed in keptUniverseRoutes. The app
// reads that data from its own sde_* tables (imported from the SDE
// API), so a request for a retired route is answered here as not
// found, without a network call. Callers already treat a 404 as a
// settled miss, which is what a static-data miss is.
var keptUniverseRoutes = []string{
	"/universe/ids/",
	"/universe/names/",
	"/universe/structures/",
	"/universe/system_jumps/",
	"/universe/system_kills/",
}

// retiredStaticRoute reports whether path is a /universe route ESI no
// longer serves.
func retiredStaticRoute(path string) bool {
	if !strings.HasPrefix(path, "/universe/") {
		return false
	}
	for _, kept := range keptUniverseRoutes {
		if strings.HasPrefix(path, kept) {
			return false
		}
	}
	return true
}

// retiredRouteError is the answer to a retired route: not found.
func retiredRouteError(method, path string) error {
	return &StatusError{Method: method, Path: path, Code: http.StatusNotFound, Detail: "static data route; read from the SDE"}
}

// staticMiss is the answer when the static data has no such id: not
// found, the same settled miss ESI's 404 was.
func staticMiss(kind string, id int64) error {
	return &StatusError{Method: "SDE", Path: fmt.Sprintf("%s %d", kind, id), Code: http.StatusNotFound, Detail: "not in the static data"}
}

// storeFactionsSnapshot writes the GlobalFactions snapshot from the
// static data, in the shape its readers decode (Factions). With no
// factions imported yet it fails and the stored snapshot stays.
func (c *Client) storeFactionsSnapshot(ctx context.Context) error {
	rows, err := c.queries.ListSDEFactions(ctx)
	if err != nil {
		return fmt.Errorf("read factions: %w", err)
	}
	if len(rows) == 0 {
		return errors.New("no factions in the static data yet")
	}
	list := make(Factions, len(rows))
	for i, r := range rows {
		list[i] = Faction{FactionID: r.FactionID, Name: r.Name, CorporationID: r.CorporationID}
	}
	body, err := json.Marshal(list)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if err := c.queries.UpsertGlobalSnapshot(ctx, db.UpsertGlobalSnapshotParams{
		Kind: GlobalFactions, Payload: string(body), FetchedAt: now, CachedUntil: now.Add(6 * time.Hour),
	}); err != nil {
		return fmt.Errorf("store global snapshot %s: %w", GlobalFactions, err)
	}
	return nil
}
