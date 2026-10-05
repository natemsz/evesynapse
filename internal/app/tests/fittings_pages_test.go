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

	// Warming state: Beta's snapshot hasn't landed yet. The
	// missing snapshot triggers GetCached's single read-through
	// attempt (pre-existing behavior, shared with the old page);
	// it fails against the stub transport, so the page shows the
	// warming copy.
	before := transport.Calls.Load()
	code, body = apptest.GetPage(t, rig, cookie, "/fittings/saved/?character=90000002")
	if code != 200 {
		t.Fatalf("saved-fits warming status = %d", code)
	}
	apptest.MustContain(t, "/fittings/saved/?character=90000002", body, "Still warming up")
	if got := transport.Calls.Load() - before; got != 1 {
		t.Errorf("warming page made %d outbound calls, want 1 (the read-through)", got)
	}

	if transport.Calls.Load() != 1 {
		t.Errorf("fittings pages made %d outbound calls, want 1 (warming read-through only)", transport.Calls.Load())
	}
}
