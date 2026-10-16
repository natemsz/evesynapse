package tests

// My Loadouts unification + public-fit owner editing: the saved
// page lists the user's EveSynapse fits beside the character's
// EVE fittings with a source filter, and a public fit opened by
// its owner loads as their saved fit (edits land in place),
// while another user's public fit still opens unsaved (a copy).

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"evesynapse/internal/apptest"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

func seedLocalFit(t *testing.T, q *db.Queries, userID int64, name string, isPublic, isDraft bool) db.CreateLocalFittingRow {
	t.Helper()
	doc := fmt.Sprintf(`{"name":%q,"shipTypeId":587,"items":[{"typeId":3244,"qty":1}],"charges":{}}`, name)
	row, err := q.CreateLocalFitting(context.Background(), db.CreateLocalFittingParams{
		UserID: userID, Name: name, ShipTypeID: 587, ItemsJson: doc,
		IsPublic: isPublic, IsDraft: isDraft,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create local fit %q: %v", name, err)
	}
	return row
}

func TestMyLoadoutsSourcesAndOwnerEdit(t *testing.T) {
	transport := &apptest.CountingTransport{}
	rig := apptest.Build(t, transport)
	conn := rig.DB()
	q := rig.Queries()
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	other, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create other user: %v", err)
	}
	apptest.SeedCharacter(t, q, user.ID, apptest.FixtureCharA, "Alpha Pilot")
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO sde_types (type_id, name, group_id) VALUES (587, 'Rifter', 25), (3244, 'Medium Shield Extender I', 38)`); err != nil {
		t.Fatalf("seed sde: %v", err)
	}
	apptest.SeedSnapshot(t, q, apptest.FixtureCharA, esi.SnapFittings, esi.Fittings{
		{FittingID: 42, Name: "Alpha Rifter", ShipTypeID: 587,
			Items: []esi.FittingItem{{TypeID: 3244, Quantity: 1, Flag: "MedSlot0"}}},
	})

	publicFit := seedLocalFit(t, q, user.ID, "Harbor Public Rifter", true, false)
	seedLocalFit(t, q, user.ID, "Harbor Private Rifter", false, false)
	seedLocalFit(t, q, user.ID, "Harbor Draft Rifter", false, true)
	strangerFit := seedLocalFit(t, q, other.ID, "Stranger Public Rifter", true, false)

	cookie := apptest.SessionCookie(t, rig, user.ID, apptest.FixtureCharA, "Alpha Pilot")

	// All: every source together, drafts excluded, chips counted.
	_, body := apptest.GetPage(t, rig, cookie, "/fittings/saved/")
	apptest.MustContain(t, "/fittings/saved/", body,
		"Harbor Public Rifter", "Harbor Private Rifter", "Alpha Rifter",
		"Public <small>1</small>", "EveSynapse <small>2</small>", "EVE account <small>1</small>",
		fmt.Sprintf("/fittings/?local=%d#fit-editor", publicFit.ID),
		fmt.Sprintf("/fittings/public/fit/%d/", publicFit.ID),
		// The training-plan glyph rides on both fit sources.
		fmt.Sprintf("/skills/plans/fit?character=90000001&amp;local=%d", publicFit.ID),
		"/skills/plans/fit?character=90000001&amp;fitting=42",
	)
	if strings.Contains(body, "Harbor Draft Rifter") {
		t.Errorf("saved page lists the autosave draft")
	}

	// The source filters narrow to exactly their bucket.
	_, body = apptest.GetPage(t, rig, cookie, "/fittings/saved/?source=public")
	apptest.MustContain(t, "source=public", body, "Harbor Public Rifter")
	if strings.Contains(body, "Harbor Private Rifter") || strings.Contains(body, "Alpha Rifter") {
		t.Errorf("source=public leaked another bucket")
	}
	_, body = apptest.GetPage(t, rig, cookie, "/fittings/saved/?source=evesynapse")
	apptest.MustContain(t, "source=evesynapse", body, "Harbor Public Rifter", "Harbor Private Rifter")
	if strings.Contains(body, "Alpha Rifter") {
		t.Errorf("source=evesynapse leaked the EVE fit")
	}
	_, body = apptest.GetPage(t, rig, cookie, "/fittings/saved/?source=eve")
	apptest.MustContain(t, "source=eve", body, "Alpha Rifter")
	if strings.Contains(body, "Harbor Public Rifter") || strings.Contains(body, "Harbor Private Rifter") {
		t.Errorf("source=eve leaked a local fit")
	}

	// The owner opens their own public fit as their saved fit:
	// the editor carries its id, so saving edits it in place.
	_, body = apptest.GetPage(t, rig, cookie, fmt.Sprintf("/fittings/?public=%d", publicFit.ID))
	apptest.MustContain(t, "own public fit in the editor", body,
		fmt.Sprintf(`id="fit-local-id" value="%d"`, publicFit.ID))
	// Somebody else's public fit still opens unsaved.
	_, body = apptest.GetPage(t, rig, cookie, fmt.Sprintf("/fittings/?public=%d", strangerFit.ID))
	apptest.MustContain(t, "stranger public fit in the editor", body,
		`id="fit-local-id" value="0"`)

	// The public page tells the owner it is theirs, with an edit
	// path, and tells a visitor the copy story instead.
	_, body = apptest.GetPage(t, rig, cookie, fmt.Sprintf("/fittings/public/fit/%d/", publicFit.ID))
	apptest.MustContain(t, "own public page", body,
		"This is your fit", fmt.Sprintf("/fittings/?local=%d#fit-editor", publicFit.ID))
	_, body = apptest.GetPage(t, rig, cookie, fmt.Sprintf("/fittings/public/fit/%d/", strangerFit.ID))
	apptest.MustContain(t, "stranger public page", body, "make your own copy")
}
