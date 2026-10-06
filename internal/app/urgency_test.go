package app

// Hermetic tests for the current-page urgency pass:
//
//   - Contacts acceptance case: unresolved (including pre-90M)
//     character contacts enqueue viewed-priority pilot wants,
//     render a non-blank pending live region, and swap in the
//     resolved pilot link once the worker stores the name —
//     with zero outbound calls from pages, fragments, and the
//     status endpoint throughout.
//   - Contracts (the non-contacts page): an unresolved
//     counterparty label does the same via the generic label
//     fragment.
//   - The banner sync indicator's status endpoint reports the
//     page's pending count and settles to zero.
//   - Item descriptions come from the bulk SDE cache first;
//     the ESI type-detail queue is only the fallback.

import (
	"context"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// settlePilotName simulates the worker landing one pilot record.
func settlePilotName(t *testing.T, q *db.Queries, characterID int64, name string) {
	t.Helper()
	payload := `{"profile":{"name":"` + name + `"}}`
	if err := q.SetPilotRecord(context.Background(), db.SetPilotRecordParams{
		CharacterID: characterID, Payload: payload, State: pilotStateReady,
		FetchedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("set pilot record %d: %v", characterID, err)
	}
}

func TestContactsUnresolvedNamesEnqueueAndLiveFill(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	grantTestAdmin(app, fixtureCharA)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")

	// The Siberokk scenario: two pre-90M character contacts
	// whose names no local tier knows yet.
	seedSnapshot(t, q, fixtureCharA, esi.SnapContacts, []esi.Contact{
		{ContactID: 3018677, ContactType: "character", Standing: 5.0},
		{ContactID: 3018932, ContactType: "character", Standing: -5.0},
		{ContactID: fixtureCorpA, ContactType: "corporation", Standing: 0},
	})

	_, body := getPage(t, app, cookie, "/contacts/?character=90000001")
	// Non-blank pending state with the honest fallback visible.
	mustContain(t, "/contacts/", body,
		"Loading name for Character #3018677",
		"Loading name for Character #3018932",
		"/contacts/name-fragment?character=90000001&amp;contact=3018677",
		"data-poll-state=\"pending\"",
		// The global banner indicator ships on the page.
		"page-sync-indicator",
	)
	// The page left viewed-priority wants for both characters.
	for _, id := range []int64{3018677, 3018932} {
		rec, err := q.GetPilotRecord(ctx, id)
		if err != nil || rec.State != pilotStatePending {
			t.Fatalf("pilot want for %d: rec=%+v err=%v, want pending", id, rec, err)
		}
	}

	// The fragment is still pending and everything so far is
	// cache-only.
	_, frag := getPage(t, app, cookie, "/contacts/name-fragment?character=90000001&contact=3018677")
	mustContain(t, "contact name fragment (cold)", frag,
		"data-poll-state=\"pending\"", "Loading name for Character #3018677")

	// Status: the page is waiting on both names (plus the
	// unresolved corporation label).
	_, status := getPage(t, app, cookie, "/sync/page-status?page=%2Fcontacts%2F%3Fcharacter%3D90000001")
	mustContain(t, "page status (pending)", status, `"pending":3`, `"loading":true`)

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("transport calls = %d, want 0 (pages, fragments and status are cache-only)", got)
	}

	// The worker stores one name; the fragment swaps in the
	// resolved pilot link (stranger → /pilot/), and the status
	// count drops.
	settlePilotName(t, q, 3018677, "Siberokk Darkforge")
	_, frag = getPage(t, app, cookie, "/contacts/name-fragment?character=90000001&contact=3018677")
	mustContain(t, "contact name fragment (filled)", frag,
		"data-poll-state=\"ready\"",
		`<a href="/pilot/?character=3018677">Siberokk Darkforge</a>`)

	_, status = getPage(t, app, cookie, "/sync/page-status?page=%2Fcontacts%2F%3Fcharacter%3D90000001")
	mustContain(t, "page status (one name left)", status, `"pending":2`)

	// A fresh page render shows the resolved link inline and
	// only the other contact still pending.
	_, body = getPage(t, app, cookie, "/contacts/?character=90000001")
	mustContain(t, "/contacts/ (filled)", body,
		`<a href="/pilot/?character=3018677">Siberokk Darkforge</a>`,
		"Loading name for Character #3018932")

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("transport calls = %d, want 0", got)
	}
}

func TestContractsUnresolvedCounterpartyLiveFill(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	grantTestAdmin(app, fixtureCharA)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")

	const stranger = int64(93300099)
	seedSnapshot(t, q, fixtureCharA, esi.SnapContracts, []esi.Contract{{
		ContractID:  7001,
		IssuerID:    stranger,
		Type:        "item_exchange",
		Status:      "outstanding",
		Title:       "Bulk Tritanium",
		Price:       1000,
		DateIssued:  "2026-09-01T00:00:00Z",
		DateExpired: "2026-10-01T00:00:00Z",
	}})

	_, body := getPage(t, app, cookie, "/contracts/?character=90000001")
	mustContain(t, "/contracts/", body,
		"Loading name for Character #93300099",
		"/labels/character-fragment?id=93300099",
	)
	if _, err := q.GetPilotRecord(ctx, stranger); err != nil {
		t.Fatalf("contracts page left no pilot want for %d: %v", stranger, err)
	}

	_, frag := getPage(t, app, cookie, "/labels/character-fragment?id=93300099")
	mustContain(t, "label fragment (cold)", frag, "data-poll-state=\"pending\"")

	settlePilotName(t, q, stranger, "Counter Party")
	_, frag = getPage(t, app, cookie, "/labels/character-fragment?id=93300099")
	mustContain(t, "label fragment (filled)", frag,
		"data-poll-state=\"ready\"",
		`<a href="/pilot/?character=93300099">Counter Party</a>`)

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("transport calls = %d, want 0", got)
	}
}

func TestPageSyncStatusClearWhenNothingPending(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	grantTestAdmin(app, fixtureCharA)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")

	_, status := getPage(t, app, cookie, "/sync/page-status?page=%2Fsync%2F")
	mustContain(t, "page status (clear)", status, `"pending":0`, `"loading":false`)
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("transport calls = %d, want 0", got)
	}
}

func TestSDEDescriptionLocalFirstAndFallback(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	seed34SDEType(t, app)
	cookie := seedItemPage(t, app, q)

	// Type 34 carries bulk-cached invTypes text after the
	// schema-018 backfill; the page reads it locally.
	if _, err := app.db.ExecContext(ctx,
		`UPDATE sde_types SET description = 'A bulk cached mineral.' WHERE type_id = 34`); err != nil {
		t.Fatalf("seed SDE description: %v", err)
	}
	_, body := getPage(t, app, cookie, "/items/type/34/")
	mustContain(t, "/items/type/34/", body, "A bulk cached mineral.")
	if _, err := q.GetTypeDetail(ctx, 34); err == nil {
		t.Fatal("type detail want noted for a type the SDE already describes")
	}

	// Type 35 has no SDE text: the page queues the ESI fallback
	// and shows the filling-in state.
	if _, err := app.db.ExecContext(ctx,
		`INSERT INTO sde_types (type_id, name, group_id, market_group_id, published, description) VALUES (35, 'Fallback Rock', 18, 1, 1, '')`); err != nil {
		t.Fatalf("seed fallback type: %v", err)
	}
	_, body = getPage(t, app, cookie, "/items/type/35/")
	mustContain(t, "/items/type/35/", body, "This description is queued")
	td, err := q.GetTypeDetail(ctx, 35)
	if err != nil || td.FetchedAt != "" {
		t.Fatalf("fallback want: td=%+v err=%v, want unfilled want row", td, err)
	}

	// The fallback lands; the page serves it.
	if err := q.SetTypeDetail(ctx, db.SetTypeDetailParams{
		TypeID: 35, Description: "Text from the fallback fetch.",
		FetchedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("set type detail: %v", err)
	}
	_, body = getPage(t, app, cookie, "/items/type/35/")
	mustContain(t, "/items/type/35/ (fallback landed)", body, "Text from the fallback fetch.")

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("transport calls = %d, want 0", got)
	}
}

func TestSDEImportParsesAndStoresDescriptions(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()

	withDesc := "\"typeID\",\"typeName\",\"description\",\"groupID\",\"marketGroupID\",\"published\"\n" +
		"\"34\",\"Tritanium\",\"A basic mineral.\",\"18\",\"4\",\"1\"\n"
	var parsed parsedSDE
	if err := parseSDEFile("invTypes.csv", strings.NewReader(withDesc), &parsed); err != nil {
		t.Fatalf("parse invTypes with description: %v", err)
	}
	if len(parsed.types) != 1 || parsed.types[0].description != "A basic mineral." {
		t.Fatalf("parsed types = %+v, want description carried", parsed.types)
	}

	// A dump without the column still parses (empty description).
	withoutDesc := "\"typeID\",\"typeName\",\"groupID\",\"marketGroupID\",\"published\"\n" +
		"\"34\",\"Tritanium\",\"18\",\"4\",\"1\"\n"
	var parsed2 parsedSDE
	if err := parseSDEFile("invTypes.csv", strings.NewReader(withoutDesc), &parsed2); err != nil {
		t.Fatalf("parse invTypes without description: %v", err)
	}
	if len(parsed2.types) != 1 || parsed2.types[0].description != "" {
		t.Fatalf("parsed types = %+v, want empty description", parsed2.types)
	}

	// The store round-trips the text and stamps import version 6.
	if _, err := app.storeSDE(ctx, "fixture", &parsed); err != nil {
		t.Fatalf("store SDE: %v", err)
	}
	row, err := q.GetSDEType(ctx, 34)
	if err != nil || row.Description != "A basic mineral." {
		t.Fatalf("stored type = %+v err=%v, want description", row, err)
	}
	if ver, _ := app.sdeMeta(ctx, "sde_import_version"); ver != "7" {
		t.Fatalf("sde_import_version = %q, want 7", ver)
	}
	if state, html := app.itemDescription(ctx, 34); state != "ready" || !strings.Contains(string(html), "A basic mineral.") {
		t.Fatalf("itemDescription = %q %q, want local ready text", state, html)
	}
}
