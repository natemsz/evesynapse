package tests

// My Loadouts preview popover: the saved-fittings cards render
// their Modules and cargo lists in a hidden popover panel (a
// trigger button plus the grouped lists) instead of a <details>
// block that expands the card in place. The fitting JS opens the
// panel as a floating preview; these tests pin the markup it
// drives. Doctrine pages keep their in-place <details> block —
// that side is pinned in the doctrines test in internal/app.

import (
	"context"
	"strings"
	"testing"

	"evesynapse/internal/apptest"
	"evesynapse/internal/esi"
)

func TestSavedFittingsPreviewPopoverMarkup(t *testing.T) {
	transport := &apptest.CountingTransport{}
	rig := apptest.Build(t, transport)
	conn := rig.DB()
	q := rig.Queries()
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	apptest.SeedCharacter(t, q, user.ID, apptest.FixtureCharA, "Alpha Pilot")

	if _, err := conn.ExecContext(ctx,
		`INSERT INTO sde_types (type_id, name, group_id) VALUES (587, 'Rifter', 25), (3244, 'Medium Shield Extender I', 38), (34, 'Tritanium', 18)`); err != nil {
		t.Fatalf("seed sde: %v", err)
	}
	apptest.SeedSnapshot(t, q, apptest.FixtureCharA, esi.SnapFittings, esi.Fittings{
		{
			FittingID: 42, Name: "Alpha Rifter", ShipTypeID: 587,
			Items: []esi.FittingItem{
				{TypeID: 3244, Quantity: 1, Flag: "MedSlot0"},
				{TypeID: 34, Quantity: 500, Flag: "Cargo"},
			},
		},
	})

	cookie := apptest.SessionCookie(t, rig, user.ID, apptest.FixtureCharA, "Alpha Pilot")
	code, body := apptest.GetPage(t, rig, cookie, "/fittings/saved/")
	if code != 200 {
		t.Fatalf("saved-fits status = %d", code)
	}
	apptest.MustContain(t, "/fittings/saved/", body,
		// The popover structure: trigger + labelled hidden panel
		// carrying the same grouped lists the details block had.
		`<div class="loadout-preview">`,
		`<button type="button" class="loadout-preview-btn" aria-expanded="false">Modules and cargo</button>`,
		`<div class="loadout-preview-panel" role="group" aria-label="Modules and cargo for Alpha Rifter" hidden>`,
		`<div class="loadout-preview-group">`,
		"Medium slots", "Medium Shield Extender I",
		"Cargo hold", "Tritanium",
	)
	if strings.Contains(body, `<summary>Modules and cargo</summary>`) {
		t.Errorf("saved-fits page still expands Modules and cargo in place")
	}
	if strings.Contains(body, "doctrine-eft") {
		t.Errorf("saved-fits page still uses the shared doctrine-eft details block")
	}
}
