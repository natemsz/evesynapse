package app

// Hermetic tests for the player structure page (v0.3.16) and the
// structure leg of the name-link policy: the helper's policy, the
// page states (pending -> named with context, named without
// context, unknown id, settled-missing), context persistence from
// corporation structure snapshots (with provenance precedence),
// and the surfaces that now link resolved structure titles
// (assets, character docked-at) while unresolved ones stay text.
// Every render runs against the counting transport, which must
// stay at zero calls.

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

const fixtureStructureID = int64(1044752365771)
const fixtureStructureCorp = int64(777000)

// ---------------------------------------------------------------------------
// Helper policy: structures resolve to their page only once named.
// ---------------------------------------------------------------------------

func TestStructureLinkHelpers(t *testing.T) {
	cases := []struct {
		name string
		got  template.HTML
		want string
	}{
		{"structure", structureLink(fixtureStructureID, "Corp Home"), `<a href="/structure/?structure=1044752365771">Corp Home</a>`},
		{"structure no id", structureLink(0, "Corp Home"), `Corp Home`},
		{"structure no name", structureLink(fixtureStructureID, ""), ``},
		{"structure name escaped", structureLink(fixtureStructureID, `<b>Home</b>`), `<a href="/structure/?structure=1044752365771">&lt;b&gt;Home&lt;/b&gt;</a>`},
		{"ref structure", placeLink(placeRef{Name: "Corp Home", StructureID: fixtureStructureID}), `<a href="/structure/?structure=1044752365771">Corp Home</a>`},
		{"ref unnamed structure stays text", placeLink(placeRef{Name: "Structure #1044752365771"}), `Structure #1044752365771`},
		{"ref station still wins", placeLink(placeRef{Name: "Jita 4 - Moon 4", StationID: 60003760, StructureID: fixtureStructureID}), `<a href="/station/?station=60003760">Jita 4 - Moon 4</a>`},
	}
	for _, c := range cases {
		if string(c.got) != c.want {
			t.Errorf("%s: got %q, want %q", c.name, c.got, c.want)
		}
	}
}

// seedStructureContext plants the corp-snapshot facts for the
// fixture structure (owner, Jita system, Fixture Citadel type) and
// the SDE rows the page resolves names against.
func seedStructureContext(t *testing.T, app *Application, q *db.Queries) {
	t.Helper()
	ctx := context.Background()
	if err := q.SetStructureContext(ctx, db.SetStructureContextParams{
		StructureID:        fixtureStructureID,
		OwnerCorporationID: fixtureStructureCorp,
		SystemID:           30000142,
		TypeID:             35834,
		UpdatedAt:          time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed structure context: %v", err)
	}
	if _, err := app.db.ExecContext(ctx, `INSERT INTO sde_types (type_id, name, group_id, market_group_id, published) VALUES (35834, 'Fixture Citadel', 0, 0, 1)`); err != nil {
		t.Fatalf("seed sde type: %v", err)
	}
	app.esi.StoreCorpName(fixtureStructureCorp, "Fixture Owners")
}

// ---------------------------------------------------------------------------
// The page states.
// ---------------------------------------------------------------------------

func TestStructurePageStates(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")
	seedPlaceSDE(t, app)

	path := fmt.Sprintf("/structure/?structure=%d", fixtureStructureID)

	// Unknown id: the honest loading state, live-region wired,
	// and the visit itself queued the resolution want.
	code, body := getPage(t, app, cookie, path)
	if code != http.StatusOK {
		t.Fatalf("GET /structure/ unknown: status %d", code)
	}
	mustContain(t, "/structure/ unknown", body, "Looking up this structure",
		fmt.Sprintf(`data-poll-url="/structure/fragment?structure=%d"`, fixtureStructureID))
	if row, err := q.GetStructureName(ctx, fixtureStructureID); err != nil || row.State != esi.StructurePending {
		t.Fatalf("visit queued want: row=%+v err=%v, want pending", row, err)
	}

	// Context without a name: still loading, but the known facts
	// render (owner, system, type -- each linked).
	seedStructureContext(t, app, q)
	code, body = getPage(t, app, cookie, path)
	if code != http.StatusOK {
		t.Fatalf("GET /structure/ context: status %d", code)
	}
	mustContain(t, "/structure/ context", body, "Looking up this structure",
		`<a href="/items/type/35834/">Fixture Citadel</a>`,
		`<a href="/corporation/?corporation=777000">Fixture Owners</a>`,
		`<a href="/system/?system=30000142">Jita</a>`)

	// The name lands: ready state, name as the heading, context
	// still linked, poller disarmed.
	if err := q.SetStructureName(ctx, db.SetStructureNameParams{
		StructureID: fixtureStructureID, Name: "Corp Home", State: esi.StructureResolved,
		ResolvedAt: timeSet(time.Now().UTC()), Source: esi.StructureSourceCorp,
	}); err != nil {
		t.Fatalf("seed resolved name: %v", err)
	}
	code, body = getPage(t, app, cookie, path)
	if code != http.StatusOK {
		t.Fatalf("GET /structure/ ready: status %d", code)
	}
	mustContain(t, "/structure/ ready", body, "Corp Home", "player-built structure",
		`<a href="/corporation/?corporation=777000">Fixture Owners</a>`)
	if strings.Contains(body, "data-live-region") {
		t.Error("ready structure page still carries a live region")
	}

	// The fragment endpoint renders the same ready body for the
	// poller.
	code, body = getPage(t, app, cookie, fmt.Sprintf("/structure/fragment?structure=%d", fixtureStructureID))
	if code != http.StatusOK {
		t.Fatalf("GET /structure/fragment: status %d", code)
	}
	mustContain(t, "/structure/fragment", body, "Corp Home")

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handlers made %d outbound calls, want 0", got)
	}
}

func TestStructurePageNamedWithoutContextAndMissing(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")

	// A name with no context rows: ready, heading only, no facts
	// table invented.
	const namedID = int64(1044752366001)
	if err := q.SetStructureName(ctx, db.SetStructureNameParams{
		StructureID: namedID, Name: "Lonely Rock", State: esi.StructureResolved,
		ResolvedAt: timeSet(time.Now().UTC()), Source: esi.StructureSourceESI,
	}); err != nil {
		t.Fatalf("seed resolved name: %v", err)
	}
	code, body := getPage(t, app, cookie, fmt.Sprintf("/structure/?structure=%d", namedID))
	if code != http.StatusOK {
		t.Fatalf("GET /structure/ named: status %d", code)
	}
	mustContain(t, "/structure/ named", body, "Lonely Rock")
	if strings.Contains(body, "<th>Owner</th>") || strings.Contains(body, "<th>System</th>") || strings.Contains(body, "<th>Type</th>") {
		t.Error("named-without-context page invented a facts table")
	}

	// A settled 'missing' answer: the honest unavailable line,
	// and no live region (nothing to poll for).
	const missingID = int64(1044752366002)
	if err := q.SetStructureName(ctx, db.SetStructureNameParams{
		StructureID: missingID, Name: "", State: esi.StructureMissing,
		ResolvedAt: timeSet(time.Now().UTC()), Source: esi.StructureSourceESI,
	}); err != nil {
		t.Fatalf("seed missing name: %v", err)
	}
	code, body = getPage(t, app, cookie, fmt.Sprintf("/structure/?structure=%d", missingID))
	if code != http.StatusOK {
		t.Fatalf("GET /structure/ missing: status %d", code)
	}
	mustContain(t, "/structure/ missing", body, "no longer be standing")
	if strings.Contains(body, "data-live-region") {
		t.Error("missing structure page still carries a live region")
	}

	// Non-structure ids and unparseable ones bounce home.
	for _, path := range []string{"/structure/", "/structure/?structure=abc", "/structure/?structure=60003760", "/structure/?structure=-4"} {
		if code, _ := getPage(t, app, cookie, path); code != http.StatusSeeOther {
			t.Errorf("GET %s: status %d, want %d (redirect)", path, code, http.StatusSeeOther)
		}
	}

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handlers made %d outbound calls, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// Context persistence from corp snapshots (+ provenance guard).
// ---------------------------------------------------------------------------

func TestPersistStructureContexts(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	app.persistStructureContexts(ctx, esi.CorpStructures{
		{StructureID: fixtureStructureID, CorporationID: fixtureStructureCorp, SystemID: 30000142, TypeID: 35834, Name: "Corp Home"},
		{StructureID: 1044752366003, CorporationID: fixtureStructureCorp, SystemID: 30000143, TypeID: 35832, Name: ""},
	})

	sc, err := q.GetStructureContext(ctx, fixtureStructureID)
	if err != nil {
		t.Fatalf("get context: %v", err)
	}
	if sc.OwnerCorporationID != fixtureStructureCorp || sc.SystemID != 30000142 || sc.TypeID != 35834 {
		t.Fatalf("context row: %+v, want owner %d system 30000142 type 35834", sc, fixtureStructureCorp)
	}
	row, err := q.GetStructureName(ctx, fixtureStructureID)
	if err != nil || row.State != esi.StructureResolved || row.Name != "Corp Home" || row.Source != esi.StructureSourceCorp {
		t.Fatalf("name row after persist: %+v err=%v", row, err)
	}
	// The unnamed entry persisted its context without naming it.
	if sc, err := q.GetStructureContext(ctx, 1044752366003); err != nil || sc.TypeID != 35832 {
		t.Fatalf("unnamed context: %+v err=%v", sc, err)
	}
	if _, err := q.GetStructureName(ctx, 1044752366003); err == nil {
		t.Error("unnamed structure gained a name row from an empty snapshot name")
	}

	// Provenance: an ESI-resolved name is never demoted by a
	// later corp-list write (nor is the corp name lost when it is
	// the same structure re-persisted under ESI truth).
	stamp := time.Now().UTC()
	if err := q.SetStructureName(ctx, db.SetStructureNameParams{
		StructureID: fixtureStructureID, Name: "ESI Truth", State: esi.StructureResolved,
		ResolvedAt: timeSet(stamp), Source: esi.StructureSourceESI,
	}); err != nil {
		t.Fatalf("seed esi name: %v", err)
	}
	app.persistStructureContexts(ctx, esi.CorpStructures{
		{StructureID: fixtureStructureID, CorporationID: fixtureStructureCorp, SystemID: 30000142, TypeID: 35834, Name: "Corp Home"},
	})
	row, err = q.GetStructureName(ctx, fixtureStructureID)
	if err != nil || row.Name != "ESI Truth" || row.Source != esi.StructureSourceESI {
		t.Fatalf("provenance after re-persist: %+v err=%v, want ESI Truth/esi", row, err)
	}
}

// ---------------------------------------------------------------------------
// Swept surfaces: resolved titles link, unresolved stay text.
// ---------------------------------------------------------------------------

func TestStructureLinksOnAssetsAndCharacter(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")
	seedPlaceSDE(t, app)

	seedSnapshot(t, q, fixtureCharA, esi.SnapAssets, []esi.Asset{
		{ItemID: 41, TypeID: 34, Quantity: 100, LocationID: fixtureStructureID, LocationType: "structure"},
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapLocation, esi.Location{SolarSystemID: 30000142, StructureID: fixtureStructureID})
	seedSnapshot(t, q, fixtureCharA, esi.SnapOnline, esi.Online{})
	seedSnapshot(t, q, fixtureCharA, esi.SnapShip, esi.Ship{})
	seedSnapshot(t, q, fixtureCharA, esi.SnapFatigue, esi.Fatigue{})
	seedSnapshot(t, q, fixtureCharA, esi.SnapImplants, esi.Implants{})
	seedSnapshot(t, q, fixtureCharA, esi.SnapClones, esi.Clones{})

	// Unresolved: both surfaces show the honest fallback as text.
	code, body := getPage(t, app, cookie, "/assets/?character=90000001")
	if code != http.StatusOK {
		t.Fatalf("GET /assets/: status %d", code)
	}
	mustContain(t, "/assets/ unresolved", body, fmt.Sprintf("Structure #%d", fixtureStructureID))
	if strings.Contains(body, "/structure/?structure=") {
		t.Error("unresolved structure title linked from assets")
	}
	code, body = getPage(t, app, cookie, "/character/")
	if code != http.StatusOK {
		t.Fatalf("GET /character/: status %d", code)
	}
	mustContain(t, "/character/ unresolved", body, fmt.Sprintf("Structure #%d", fixtureStructureID))
	if strings.Contains(body, "/structure/?structure=") {
		t.Error("unresolved structure title linked from character page")
	}

	// Resolved: both surfaces link the name to the structure page.
	if err := q.SetStructureName(ctx, db.SetStructureNameParams{
		StructureID: fixtureStructureID, Name: "Corp Home", State: esi.StructureResolved,
		ResolvedAt: timeSet(time.Now().UTC()), Source: esi.StructureSourceCorp,
	}); err != nil {
		t.Fatalf("seed resolved name: %v", err)
	}
	want := `<a href="/structure/?structure=1044752365771">Corp Home</a>`
	code, body = getPage(t, app, cookie, "/assets/?character=90000001")
	if code != http.StatusOK {
		t.Fatalf("GET /assets/ resolved: status %d", code)
	}
	mustContain(t, "/assets/ resolved", body, want)
	code, body = getPage(t, app, cookie, "/character/")
	if code != http.StatusOK {
		t.Fatalf("GET /character/ resolved: status %d", code)
	}
	mustContain(t, "/character/ resolved", body, want)

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handlers made %d outbound calls, want 0", got)
	}
}
