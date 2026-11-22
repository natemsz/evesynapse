package app

import (
	"context"
	json "github.com/goccy/go-json"
	"strconv"
	"time"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/fitview"
	"evesynapse/internal/logging"
)

// What the other modules ask of an owner's doctrines: who can read one,
// which doctrine a keeper is choosing fits for (public fits), a doctrine
// fit read as a fit document (the fitting tool), and the copy of a
// doctrine a fleet keeps from its start. The access rule, an owner's
// doctrine permission, is core's.

// doctrineOwner is who a doctrine belongs to.
func doctrineOwner(d db.Doctrine) orgOwner { return orgOwner{Kind: d.OwnerKind, ID: d.OwnerID} }

// doctrineChosenFor reads the "for" of an address: the doctrine a
// keeper is choosing fits for. Nil unless the account keeps it.
func (app *Application) doctrineChosenFor(ctx context.Context, userID int64, raw string) *opChoice {
	id, _ := strconv.ParseInt(raw, 10, 64)
	if id <= 0 || userID == 0 {
		return nil
	}
	d, err := app.queries.GetDoctrine(ctx, id)
	if err != nil {
		return nil
	}
	st, err := app.standingFor(ctx, userID)
	if err != nil || !app.ownerPermits(ctx, st, doctrineOwner(d), permDoctrines) {
		return nil
	}
	return &opChoice{ID: d.ID, Name: d.Name}
}

// readsDoctrine reports whether the session can read a doctrine. A
// verified pilot reads its communities' doctrines; its public
// corporation and alliance count for joining a fleet and nothing else.
func (app *Application) readsDoctrine(ctx context.Context, userID int64, owner orgOwner, visibility string) bool {
	if visibility == module.DoctrinePublic {
		return true
	}
	if userID != 0 {
		st, err := app.standingFor(ctx, userID)
		return err == nil && st.Belongs(owner)
	}
	if owner.Kind != ownerCommunity {
		return false
	}
	pilot, ok := app.VerifiedPilot(ctx)
	if !ok {
		return false
	}
	communities, err := app.queries.ListPilotCommunities(ctx, pilot.CharacterID)
	if err != nil {
		logging.Errorf("doctrines: communities of pilot %d: %v", pilot.CharacterID, err)
		return false
	}
	for _, id := range communities {
		if id == owner.ID {
			return true
		}
	}
	return false
}

// readsDoctrine is the same answer for a viewer already loaded.
func (v fleetViewer) readsDoctrine(owner orgOwner, visibility string) bool {
	if visibility == module.DoctrinePublic {
		return true
	}
	if v.Pilot != nil {
		return owner.Kind == ownerCommunity && v.Pilot.Communities[owner.ID]
	}
	return v.UserID != 0 && v.Standing.Belongs(owner)
}

// ReadsDoctrine is readsDoctrine for the modules.
func (app *Application) ReadsDoctrine(ctx context.Context, userID int64, owner orgOwner, visibility string) bool {
	return app.readsDoctrine(ctx, userID, owner, visibility)
}

// doctrineFitDoc reads a doctrine's fit for a session that can read the
// doctrine.
func (app *Application) doctrineFitDoc(ctx context.Context, userID, fitID int64) (*fitview.FitDoc, bool) {
	row, err := app.queries.GetDoctrineFit(ctx, fitID)
	if err != nil || !app.readsDoctrine(ctx, userID, orgOwner{Kind: row.OwnerKind, ID: row.OwnerID}, row.Visibility) {
		return nil, false
	}
	var doc fitview.FitDoc
	if json.Unmarshal([]byte(row.ItemsJson), &doc) != nil {
		return nil, false
	}
	doc.Name = row.Name
	return &doc, true
}

// DoctrineOwners lists the owners an account belongs to that can have
// doctrines: its player corporations, alliances and communities.
func (app *Application) DoctrineOwners(ctx context.Context, userID int64) []orgOwner {
	st, characters, err := app.standingWithCharacters(ctx, userID)
	if err != nil {
		logging.Errorf("doctrines: standing of user %d: %v", userID, err)
		return nil
	}
	v := fleetViewer{UserID: userID, Standing: st, Characters: characters}
	var out []orgOwner
	for _, owner := range v.owners() {
		if owner.Kind != ownerCorporation || !npcCorporation(owner.ID) {
			out = append(out, owner)
		}
	}
	return out
}

// HoldsProgramme is holdsProgramme for the modules.
func (app *Application) HoldsProgramme(ctx context.Context, st orgStanding, owner orgOwner, permission string) bool {
	return app.holdsProgramme(ctx, st, owner, permission)
}

// OwnerActorFor is the account's character that acts for an owner.
func (app *Application) OwnerActorFor(ctx context.Context, userID int64, owner orgOwner) int64 {
	if owner.Kind == ownerCorporation {
		return app.CorpActorFor(ctx, userID, owner.ID).CharacterID
	}
	characters, err := app.accountCharacters(ctx, userID)
	if err != nil {
		logging.Errorf("doctrines: characters of user %d: %v", userID, err)
		return 0
	}
	for _, c := range characters {
		if c.Standing.Belongs(owner) {
			return c.CharacterID
		}
	}
	return 0
}

// doctrineSnapshot is the copy of a doctrine a fleet keeps from its
// start (ops.doctrine_snapshot).
type doctrineSnapshot struct {
	Fits []doctrineSnapshotFit `json:"fits"`
}

type doctrineSnapshotFit struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	ShipTypeID int64  `json:"ship_type_id"`
	Role       string `json:"role"`
}

// Each query stores a batch; a run stops after this many batches and
// the next worker cycle carries on.
const (
	fleetSnapshotBatch = 100
	fleetSnapshotRuns  = 20
)

// snapshotFleetDoctrines stores the doctrine on fleets that have
// started, of one doctrine or (0) of any.
func (app *Application) snapshotFleetDoctrines(ctx context.Context, doctrineID int64, now time.Time) (int, error) {
	total := 0
	for range fleetSnapshotRuns {
		n, err := app.queries.SnapshotStartedFleetDoctrines(ctx, db.SnapshotStartedFleetDoctrinesParams{TakenAt: now, OnlyDoctrine: doctrineID})
		total += int(n)
		if err != nil || n < fleetSnapshotBatch {
			return total, err
		}
	}
	return total, nil
}

// SnapshotFleetsOfDoctrine runs before a doctrine changes, so a fleet
// that started since the last worker cycle keeps the doctrine it flew.
func (app *Application) SnapshotFleetsOfDoctrine(ctx context.Context, doctrineID int64) error {
	_, err := app.snapshotFleetDoctrines(ctx, doctrineID, time.Now().UTC())
	return err
}

// doctrineFitView, doctrineURL and doctrineFitViews: a doctrine's fits, read by the fleet
// page (core) and the Doctrines module alike.
type doctrineFitView = module.DoctrineFitView

func doctrineURL(id int64) string { return module.DoctrineURL(id) }

func (app *Application) doctrineFitViews(ctx context.Context, doctrineIDs []int64) map[int64][]doctrineFitView {
	out := map[int64][]doctrineFitView{}
	if len(doctrineIDs) == 0 {
		return out
	}
	fits, err := app.queries.ListDoctrineFits(ctx, doctrineIDs)
	if err != nil {
		logging.Errorf("doctrines: fits: %v", err)
		return out
	}
	for _, fit := range fits {
		out[fit.DoctrineID] = append(out[fit.DoctrineID], doctrineFitView{
			ID: fit.ID, Name: fit.Name, Ship: app.typeNameOrID(ctx, fit.ShipTypeID), ShipTypeID: fit.ShipTypeID,
			Role: fit.FleetRole, Note: fit.Note,
		})
	}
	return out
}

func (app *Application) DoctrineFitViews(ctx context.Context, doctrineIDs []int64) map[int64][]module.DoctrineFitView {
	return app.doctrineFitViews(ctx, doctrineIDs)
}
