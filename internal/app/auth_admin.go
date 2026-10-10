package app

import (
	"context"
	"sync"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// Administrator status is tied to who owns a character, not only to
// its id. EVE_ADMIN_CHARACTER_IDS names characters, and a character
// can be sold or transferred to another EVE account. The first time an
// admin character is seen, its owner hash is recorded (admin_owners);
// the status holds only while the hash is still that one. To grant it
// to a new owner on purpose, take the id out of the setting, restart,
// and put it back.

// adminOwners is the recorded owner of each admin character.
type adminOwners struct {
	mu     sync.Mutex
	loaded bool
	hash   map[int64]string
	warned map[int64]bool
}

// load reads the recorded owners, dropping those of characters the
// setting no longer names.
func (a *adminOwners) load(ctx context.Context, app *Application) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.loadLocked(ctx, app)
}

func (a *adminOwners) loadLocked(ctx context.Context, app *Application) {
	ids := make([]int64, 0, len(app.cfg.adminCharIDs))
	for id := range app.cfg.adminCharIDs {
		ids = append(ids, id)
	}
	if err := app.queries.DeleteAdminOwnersExcept(ctx, ids); err != nil {
		logging.Errorf("admin: forget owners of former admin characters: %v", err)
		return
	}
	rows, err := app.queries.ListAdminOwners(ctx)
	if err != nil {
		logging.Errorf("admin: read admin owners: %v", err)
		return
	}
	a.hash, a.warned = make(map[int64]string, len(rows)), map[int64]bool{}
	for _, row := range rows {
		a.hash[row.CharacterID] = row.OwnerHash
	}
	a.loaded = true
}

// recorded returns the owner hash recorded for an admin character.
// known is false when the record could not be read at all, in which
// case nobody is treated as an administrator.
func (a *adminOwners) recorded(app *Application, characterID int64) (hash string, pinned, known bool) {
	if !a.loaded {
		a.loadLocked(context.Background(), app)
	}
	if !a.loaded {
		return "", false, false
	}
	hash, pinned = a.hash[characterID]
	return hash, pinned, true
}

// adminHolds reports whether ch makes its account an administrator's:
// it is named in the setting, its link is not flagged owner_changed,
// and its owner is the one recorded for it.
func (app *Application) adminHolds(ch db.Character) bool {
	if !app.cfg.IsAdminCharacter(ch.CharacterID) || ch.LinkState == linkStateOwnerChanged {
		return false
	}
	a := &app.admins
	a.mu.Lock()
	defer a.mu.Unlock()
	hash, pinned, known := a.recorded(app, ch.CharacterID)
	switch {
	case !known:
		return false
	case pinned && ch.OwnerHash != hash:
		if !a.warned[ch.CharacterID] {
			a.warned[ch.CharacterID] = true
			logging.Warnf("admin: character %d is named in EVE_ADMIN_CHARACTER_IDS but has changed owner since; it is not treated as an administrator", ch.CharacterID)
		}
		return false
	case pinned || ch.OwnerHash == "":
		// An owner that was never recorded by CCP cannot be pinned yet.
		return true
	}
	if err := app.queries.InsertAdminOwner(context.Background(), db.InsertAdminOwnerParams{CharacterID: ch.CharacterID, OwnerHash: ch.OwnerHash}); err != nil {
		logging.Errorf("admin: record owner of admin character %d: %v", ch.CharacterID, err)
		return false
	}
	a.hash[ch.CharacterID] = ch.OwnerHash
	logging.Infof("admin: recorded the owner of admin character %d", ch.CharacterID)
	return true
}

// adminMaySignUp reports whether a sign-in by an admin character may
// skip the sign-up lists: only when the owner signing in is the one
// recorded, or none has been recorded yet.
func (app *Application) adminMaySignUp(characterID int64, ownerHash string) bool {
	if !app.cfg.IsAdminCharacter(characterID) {
		return false
	}
	a := &app.admins
	a.mu.Lock()
	defer a.mu.Unlock()
	hash, pinned, known := a.recorded(app, characterID)
	return known && (!pinned || hash == ownerHash)
}
