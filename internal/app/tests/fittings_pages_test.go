package tests

// Fittings page split: /fittings/ is the fit simulator only — no
// saved-fits list underneath it — and /fittings/saved/ carries the
// character's EVE fittings list. Hermetic: fixture snapshots; the
// only outbound call is the warming path's single read-through.

import (
	"context"
	"strings"
	"testing"

	"evesynapse/internal/apptest"
	"evesynapse/internal/esi"
)

// A page draws from what is stored and fetches nothing, whether or not
// the data it wants has landed yet.
const pageCalls = 0

func TestFittingsPagesSplit(t *testing.T) {
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
	apptest.SeedCharacter(t, q, user.ID, apptest.FixtureCharB, "Beta Pilot")

	if _, err := conn.ExecContext(ctx,
		`INSERT INTO sde_types (type_id, name, group_id) VALUES (587, 'Rifter', 25), (3244, 'Medium Shield Extender I', 38)`); err != nil {
		t.Fatalf("seed sde: %v", err)
	}

	apptest.SeedSnapshot(t, q, apptest.FixtureCharA, esi.SnapFittings, esi.Fittings{
		{
			FittingID: 42, Name: "Alpha Rifter", ShipTypeID: 587,
			Items: []esi.FittingItem{
				{TypeID: 3244, Quantity: 1, Flag: "MedSlot0"},
			},
		},
	})
	// Beta Pilot deliberately has no fittings snapshot yet.

	cookie := apptest.SessionCookie(t, rig, user.ID, apptest.FixtureCharA, "Alpha Pilot")

	// The simulator page carries the editor chrome and links to
	// the saved-fits page, but embeds no saved-fits list.
	code, body := apptest.GetPage(t, rig, cookie, "/fittings/")
	if code != 200 {
		t.Fatalf("simulator status = %d", code)
	}
	apptest.MustContain(t, "/fittings/", body, "Build a fitting", "/fittings/saved/")
	if strings.Contains(body, `id="fit-esi-list"`) {
		t.Errorf("simulator page still embeds the saved-fits list")
	}
	if strings.Contains(body, "Alpha Rifter") {
		t.Errorf("simulator page leaks a saved fit name")
	}

	// The saved-fits page renders the list, with View-stats deep
	// links back at the simulator.
	code, body = apptest.GetPage(t, rig, cookie, "/fittings/saved/")
	if code != 200 {
		t.Fatalf("saved-fits status = %d", code)
	}
	apptest.MustContain(t, "/fittings/saved/", body,
		`id="fit-esi-list"`, "Alpha Rifter", "Rifter",
		"Medium Shield Extender I", "Medium slots",
		"esi=42#fit-editor", "Training plan for this fit",
	)
	if strings.Contains(body, `id="fit-editor"`) {
		t.Errorf("saved-fits page embeds the simulator")
	}

	// Warming state: Beta's snapshot hasn't landed yet. The page
	// says so and fetches nothing; the worker brings it in.
	before := transport.Calls.Load()
	code, body = apptest.GetPage(t, rig, cookie, "/fittings/saved/?character=90000002")
	if code != 200 {
		t.Fatalf("saved-fits warming status = %d", code)
	}
	apptest.MustContain(t, "/fittings/saved/?character=90000002", body, "Still warming up")
	if got := transport.Calls.Load() - before; got != pageCalls {
		t.Errorf("warming page made %d outbound calls, want %d", got, pageCalls)
	}

	if transport.Calls.Load() != pageCalls {
		t.Errorf("fittings pages made %d outbound calls, want %d", transport.Calls.Load(), pageCalls)
	}
}
