package app

import (
	"context"
	"strings"
)

// scannerStationRef resolves one order location to its display
// title and link by the name policy: an NPC station the SDE
// knows links to the station page, a player structure with a
// resolved name links to the structure page, and a structure
// nobody has named yet reads "Player structure" as plain text
// -- never a bare number. Cache-only. The per-render memo keeps
// row-building loops (scanner, tradefinder, leaderboard) to one
// resolution per location per render.
func (app *Application) scannerStationRef(ctx context.Context, memo map[int64]placeRef, locationID int64) placeRef {
	if ref, ok := memo[locationID]; ok {
		return ref
	}
	ref := app.resolveScannerStationRef(ctx, locationID)
	memo[locationID] = ref
	return ref
}

func (app *Application) resolveScannerStationRef(ctx context.Context, locationID int64) placeRef {
	if locationID <= 0 {
		return placeRef{Name: "Player structure"}
	}
	if isStructureID(locationID) {
		if name := app.resolvedStructureTitle(ctx, locationID); name != "" {
			return app.linkPlace(ctx, locationID, name)
		}
		return placeRef{Name: "Player structure"}
	}
	title := app.econLocationTitle(ctx, locationID)
	if strings.HasPrefix(title, "Station #") || strings.HasPrefix(title, "Structure #") {
		// The SDE has no name for this place; keep the promise
		// that the scanner never shows a bare number.
		title = "Player structure"
		return placeRef{Name: title}
	}
	return app.linkPlace(ctx, locationID, title)
}
