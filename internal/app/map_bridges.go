package app

// ---------------------------------------------------------------------------
// Ansiblex jump bridges, shared across an alliance.
//
// ESI lists a corporation's structures only to a director, so most
// pilots could never see their own alliance's bridges. When any
// corporation's structures list syncs, its Ansiblexes are stored with
// the corporation's alliance (alliance_bridges); a reader is shown the
// bridges of every alliance and corporation their syncing characters
// belong to. One director's sync serves the whole alliance, and no
// one outside it ever sees them.
// ---------------------------------------------------------------------------

import (
	"context"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// bridgesFreshFor is how long a corporation's stored bridges stand
// without a fresh structures list; past it they age out.
const bridgesFreshFor = 7 * 24 * time.Hour

// storeCorpBridges writes a corporation's Ansiblexes from the
// structures list just synced under ch.
func (app *Application) storeCorpBridges(ctx context.Context, ch db.Character, corpID int64) {
	var structures esi.CorpStructures
	if corpID == 0 || !app.loadCorpSnapshot(ctx, ch.CharacterID, esi.SnapCorpStructures, &structures) {
		return
	}
	p := db.ReplaceCorpBridgesParams{CorporationID: corpID, UpdatedAt: time.Now().UTC(),
		StructureIds: []int64{}, FromSystems: []int64{}, ToSystems: []int64{}, Names: []string{}}
	for _, s := range structures {
		if s.TypeID != ansiblexTypeID {
			continue
		}
		b, ok := app.ansiblexBridge(ctx, s.Name)
		if !ok {
			continue
		}
		p.StructureIds = append(p.StructureIds, s.StructureID)
		p.FromSystems = append(p.FromSystems, b.From)
		p.ToSystems = append(p.ToSystems, b.To)
		p.Names = append(p.Names, b.Name)
	}
	p.AllianceID, _ = app.corpAlliance(ctx, corpID)
	if err := app.queries.ReplaceCorpBridges(ctx, p); err != nil {
		logging.Errorf("worker: bridges of corporation %d: %v", corpID, err)
	}
}

// readerBridges lists the bridges a reader may see: those of the
// alliances and corporations of their characters that still sync (a
// parked login's corporation may be out of date).
func (app *Application) readerBridges(ctx context.Context, chars []db.Character) []mineBridge {
	out := []mineBridge{}
	seenCorp, seenAlliance := map[int64]bool{}, map[int64]bool{}
	var corps, alliances []int64
	for _, ch := range chars {
		if !app.characterMaySync(ctx, ch) {
			continue
		}
		row, err := app.queries.GetCharacterCorporation(ctx, ch.CharacterID)
		if err != nil || row.CorporationID == 0 || seenCorp[row.CorporationID] {
			continue
		}
		seenCorp[row.CorporationID] = true
		corps = append(corps, row.CorporationID)
		if a, _ := app.corpAlliance(ctx, row.CorporationID); a != 0 && !seenAlliance[a] {
			seenAlliance[a] = true
			alliances = append(alliances, a)
		}
	}
	if len(corps) == 0 {
		return out
	}
	if alliances == nil {
		alliances = []int64{}
	}
	rows, err := app.queries.ListBridgesForOrgs(ctx, db.ListBridgesForOrgsParams{
		AllianceIds: alliances, CorporationIds: corps, Since: time.Now().UTC().Add(-bridgesFreshFor)})
	if err != nil {
		logging.Warnf("map: bridges: %v", err)
		return out
	}
	for _, r := range rows {
		out = append(out, mineBridge{From: r.FromSystem, To: r.ToSystem, Name: r.Name})
	}
	return out
}
