package app

// The character Assets page: a ship's cargo is filed under the
// station the ship is docked in, nested beneath the ship; the search
// covers the whole account and says whose each hit is and what it is
// inside; and the search box suggests only what the account owns.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

const (
	nestStation  = int64(60003760)
	nestStationB = int64(60008494)
	typeLoki     = int64(29990)
	typeBox      = int64(3467)
	typeAmmo     = int64(210)
	typeTrit     = int64(34)
	typeRifter   = int64(587)
)

// assetsFixture is an app with one account and its first character.
type assetsFixture struct {
	app    *Application
	q      *db.Queries
	userID int64
	ch     db.Character
	calls  *countingTransport
}

func newAssetsFixture(t *testing.T) *assetsFixture {
	t.Helper()
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	user, err := q.CreateUser(context.Background())
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return &assetsFixture{app: app, q: q, userID: user.ID, calls: transport,
		ch: seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")}
}

// seedNestedAssets gives Fixture Ceo a Loki docked in Jita holding
// ammunition and a container (which holds Tritanium), plus loose
// Tritanium in the same station; and Fixture Alt a Rifter and some
// Tritanium in Amarr.
func seedNestedAssets(t *testing.T, f *assetsFixture) {
	t.Helper()
	if _, err := f.app.db.ExecContext(context.Background(),
		`INSERT INTO sde_types (type_id, name, group_id) VALUES
			(29990, 'Loki', 963), (3467, 'Small Secure Container', 340), (210, 'Scourge Heavy Missile', 385),
			(34, 'Tritanium', 18), (587, 'Rifter', 25)`); err != nil {
		t.Fatalf("seed types: %v", err)
	}
	f.app.esi.StorePlaceName(nestStation, "Jita IV - Moon 4 - Caldari Navy Assembly Plant")
	f.app.esi.StorePlaceName(nestStationB, "Amarr VIII (Oris) - Emperor Family Academy")

	seedSnapshot(t, f.q, fixtureCharA, esi.SnapAssets, []esi.Asset{
		{ItemID: 104, TypeID: typeTrit, Quantity: 5000, LocationID: nestStation, LocationType: "station", LocationFlag: "Hangar"},
		{ItemID: 100, TypeID: typeLoki, Quantity: 1, IsSingleton: true, LocationID: nestStation, LocationType: "station", LocationFlag: "Hangar"},
		{ItemID: 101, TypeID: typeAmmo, Quantity: 500, LocationID: 100, LocationType: "item", LocationFlag: "Cargo"},
		{ItemID: 102, TypeID: typeBox, Quantity: 1, IsSingleton: true, LocationID: 100, LocationType: "item", LocationFlag: "Cargo"},
		{ItemID: 103, TypeID: typeTrit, Quantity: 1000, LocationID: 102, LocationType: "item", LocationFlag: "Unlocked"},
	})
	alt := seedCharacter(t, f.q, f.userID, fixtureCharB, "Fixture Alt")
	seedSnapshot(t, f.q, alt.CharacterID, esi.SnapAssets, []esi.Asset{
		{ItemID: 200, TypeID: typeTrit, Quantity: 10, LocationID: nestStationB, LocationType: "station", LocationFlag: "Hangar"},
		{ItemID: 201, TypeID: typeRifter, Quantity: 1, IsSingleton: true, LocationID: nestStationB, LocationType: "station", LocationFlag: "Hangar"},
	})
}

func TestAssetsNestUnderWhereTheShipIsDocked(t *testing.T) {
	f := newAssetsFixture(t)
	seedNestedAssets(t, f)
	cookie := sessionCookie(t, f.app, f.userID, f.ch.CharacterID, f.ch.Name)

	code, body := getPage(t, f.app, cookie, "/assets/?character=90000001")
	if code != http.StatusOK {
		t.Fatalf("GET /assets/ = %d", code)
	}
	// One place: the station. The ship is not a place of its own.
	if strings.Contains(body, "Inside: ") {
		t.Fatal("a ship or container is still shown as a location of its own")
	}
	if n := strings.Count(body, `<section class="foldable"`); n != 1 {
		t.Fatalf("%d location blocks, want 1 (everything is in the one station)", n)
	}
	mustContain(t, "/assets/", body,
		"Jita IV - Moon 4 - Caldari Navy Assembly Plant",
		"· 5 stacks",
		"<summary>3 stacks inside Loki</summary>",
		"<summary>1 stack inside Small Secure Container</summary>",
		"5 stacks · 6502 items · 1 locations",
	)
	// What holds things comes first: the Loki above the loose
	// Tritanium, though that is the bigger stack.
	loki, loose := strings.Index(body, ">Loki<"), strings.Index(body, "<td>5,000</td>")
	if loki < 0 || loose < 0 || loki > loose {
		t.Fatalf("the Loki (at %d) should come before the loose Tritanium (at %d)", loki, loose)
	}
	// And its contents are inside its row's block, the container's
	// inside that.
	open := strings.Index(body, "<summary>3 stacks inside Loki</summary>")
	ammo := strings.Index(body, "Scourge Heavy Missile")
	box := strings.Index(body, "<summary>1 stack inside Small Secure Container</summary>")
	inner := strings.Index(body, "<td>1,000</td>")
	if !(open < ammo && open < box && box < inner) {
		t.Fatalf("nesting order wrong: Loki opens at %d, ammo %d, container opens at %d, its Tritanium %d", open, ammo, box, inner)
	}
}

func TestAssetSearchCoversTheAccountAndSaysWhere(t *testing.T) {
	f := newAssetsFixture(t)
	seedNestedAssets(t, f)
	cookie := sessionCookie(t, f.app, f.userID, f.ch.CharacterID, f.ch.Name)

	code, body := getPage(t, f.app, cookie, "/assets/?q=trit")
	if code != http.StatusOK {
		t.Fatalf("search = %d", code)
	}
	mustContain(t, "search", body,
		"3 stacks matching “trit” across 2 characters",
		`<a href="/assets/?character=90000001">Fixture Ceo</a>`,
		`<a href="/assets/?character=90000002">Fixture Alt</a>`,
		// The Tritanium in the container in the Loki is listed under
		// the station, and says what it is inside.
		"<small>in Loki › Small Secure Container</small>",
		"Jita IV - Moon 4 - Caldari Navy Assembly Plant",
		"Amarr VIII (Oris) - Emperor Family Academy",
		`id="assets-only"> Only Fixture Ceo`,
	)
	if strings.Contains(body, "Inside: ") {
		t.Fatal("a search hit is filed under a ship instead of the station")
	}

	// Ticked, the search is the acting character's alone.
	_, body = getPage(t, f.app, cookie, "/assets/?q=trit&only=1")
	mustContain(t, "search only", body,
		"2 stacks matching “trit” on this character",
		`id="assets-only" checked> Only Fixture Ceo`)
	if strings.Contains(body, `<h2><a href="/assets/?character=90000002">`) || strings.Contains(body, "Amarr VIII") {
		t.Fatal("the narrowed search still shows another character's assets")
	}
	_, body = getPage(t, f.app, cookie, "/assets/?q=rifter&only=1")
	mustContain(t, "search only, no hit", body, "No stacks matching “rifter” on this character.")
}

func TestAssetSuggestionsAreWhatTheAccountOwns(t *testing.T) {
	f := newAssetsFixture(t)
	seedNestedAssets(t, f)
	cookie := sessionCookie(t, f.app, f.userID, f.ch.CharacterID, f.ch.Name)
	suggest := func(query string) []suggestItem {
		t.Helper()
		code, body := getPage(t, f.app, cookie, "/assets/suggest?"+query)
		if code != http.StatusOK {
			t.Fatalf("suggest %s = %d", query, code)
		}
		var rows []suggestItem
		if err := json.Unmarshal([]byte(body), &rows); err != nil {
			t.Fatalf("suggest %s: %v in %q", query, err, body)
		}
		return rows
	}

	// Owned by both characters: says so.
	if rows := suggest("q=trit"); len(rows) != 1 || rows[0].Name != "Tritanium" || rows[0].Label != "2 characters" || rows[0].ID != typeTrit {
		t.Fatalf("trit: %+v", rows)
	}
	// Owned by one: names them. Items inside a ship count as owned.
	if rows := suggest("q=scourge"); len(rows) != 1 || rows[0].Label != "Fixture Ceo" {
		t.Fatalf("scourge: %+v", rows)
	}
	if rows := suggest("q=rif"); len(rows) != 1 || rows[0].Label != "Fixture Alt" {
		t.Fatalf("rif: %+v", rows)
	}
	// Narrowed to the acting character, the alt's Rifter is not offered.
	if rows := suggest("q=rif&only=1"); len(rows) != 0 {
		t.Fatalf("rif, only: %+v", rows)
	}
	if rows := suggest("q=trit&only=1"); len(rows) != 1 || rows[0].Label != "Fixture Ceo" {
		t.Fatalf("trit, only: %+v", rows)
	}
	// Names that start with the query come first; nothing the account
	// does not own is ever offered; one letter is not a query.
	if rows := suggest("q=s"); len(rows) != 0 {
		t.Fatalf("one letter: %+v", rows)
	}
	if rows := suggest("q=sec"); len(rows) != 1 || rows[0].Name != "Small Secure Container" {
		t.Fatalf("sec: %+v", rows)
	}
	if rows := suggest("q=veldspar"); len(rows) != 0 {
		t.Fatalf("something not owned: %+v", rows)
	}
	if got := f.calls.calls.Load(); got != 0 {
		t.Fatalf("%d outbound call(s) from the assets page", got)
	}
}

// TestAssetNestingSurvivesBadData: a snapshot in which two assets
// each claim to be inside the other, or one inside itself, must not
// hang the page.
func TestAssetNestingSurvivesBadData(t *testing.T) {
	f := newAssetsFixture(t)
	seedSnapshot(t, f.q, fixtureCharA, esi.SnapAssets, []esi.Asset{
		{ItemID: 1, TypeID: typeTrit, Quantity: 1, LocationID: 2, LocationType: "item"},
		{ItemID: 2, TypeID: typeTrit, Quantity: 1, LocationID: 1, LocationType: "item"},
		{ItemID: 3, TypeID: typeTrit, Quantity: 7, LocationID: 3, LocationType: "item"},
		{ItemID: 4, TypeID: typeTrit, Quantity: 9, LocationID: nestStation, LocationType: "station"},
	})
	cookie := sessionCookie(t, f.app, f.userID, f.ch.CharacterID, f.ch.Name)
	for _, path := range []string{"/assets/?character=90000001", "/assets/?q=type", "/assets/suggest?q=type"} {
		if code, _ := getPage(t, f.app, cookie, path); code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, code)
		}
	}
}

// assetNamesESI stands in for ESI's asset-names call: it records the
// item ids it is asked about and answers with the names it was given.
type assetNamesESI struct {
	status int
	answer string
	asked  [][]int64
}

func (s *assetNamesESI) RoundTrip(req *http.Request) (*http.Response, error) {
	respond := func(code int, body string) (*http.Response, error) {
		return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(body))}, nil
	}
	if req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/characters/90000001/assets/names/") {
		return respond(http.StatusNotFound, `{"error":"unexpected `+req.Method+" "+req.URL.Path+`"}`)
	}
	var ids []int64
	if err := json.NewDecoder(req.Body).Decode(&ids); err != nil {
		return respond(http.StatusBadRequest, `{"error":"bad body"}`)
	}
	s.asked = append(s.asked, ids)
	return respond(s.status, s.answer)
}

// TestShipsShowTheNamesTheirOwnersGaveThem: the worker asks ESI for
// the names of ships and of anything with contents, once, and the
// page, the search and the suggestions use them.
func TestShipsShowTheNamesTheirOwnersGaveThem(t *testing.T) {
	names := &assetNamesESI{status: http.StatusOK, answer: `[{"item_id":100,"name":"Zoom Zoom"},{"item_id":102,"name":"None"}]`}
	app, _, q := buildCorpTestApp(t, names)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f := &assetsFixture{app: app, q: q, userID: user.ID, ch: seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")}
	seedNestedAssets(t, f)
	if _, err := app.db.ExecContext(ctx, `INSERT INTO sde_groups (group_id, name, category_id) VALUES (963, 'Strategic Cruiser', 6), (340, 'Secure Cargo Container', 2), (18, 'Mineral', 4), (385, 'Heavy Missile', 8)`); err != nil {
		t.Fatalf("seed groups: %v", err)
	}

	// Asked about: the Loki (a ship) and the container (it has
	// something inside). Not the ammunition or the Tritanium.
	stored, limited := app.warmCharacterAssetNames(ctx, f.ch)
	if stored != 1 || limited {
		t.Fatalf("stored %d name(s), limited=%v; want 1 (the container has none)", stored, limited)
	}
	if len(names.asked) != 1 || len(names.asked[0]) != 2 || names.asked[0][0] != 100 || names.asked[0][1] != 102 {
		t.Fatalf("asked ESI about %v, want one call for items 100 and 102", names.asked)
	}
	// Nothing new to name: no second call.
	if app.warmCharacterAssetNames(ctx, f.ch); len(names.asked) != 1 {
		t.Fatalf("asked again with nothing new: %v", names.asked)
	}

	cookie := sessionCookie(t, app, f.userID, f.ch.CharacterID, f.ch.Name)
	_, body := getPage(t, app, cookie, "/assets/?character=90000001")
	mustContain(t, "/assets/", body,
		">Zoom Zoom</a> <small>Loki</small>",
		"<summary>3 stacks inside Zoom Zoom</summary>",
		// The unnamed container keeps its type name.
		"<summary>1 stack inside Small Secure Container</summary>")

	// Searching by the given name finds the ship; a hit inside it
	// says so by that name.
	_, body = getPage(t, app, cookie, "/assets/?q=zoom")
	mustContain(t, "search zoom", body, "1 stack matching “zoom”", ">Zoom Zoom</a> <small>Loki</small>")
	_, body = getPage(t, app, cookie, "/assets/?q=trit&only=1")
	mustContain(t, "search trit", body, "<small>in Zoom Zoom › Small Secure Container</small>")

	_, body = getPage(t, app, cookie, "/assets/suggest?q=zoo")
	var rows []suggestItem
	if err := json.Unmarshal([]byte(body), &rows); err != nil || len(rows) != 1 || rows[0].Name != "Zoom Zoom" || rows[0].Label != "Loki · Fixture Ceo" {
		t.Fatalf("suggest zoo: %+v (%v)", rows, err)
	}
	// Searching by type still finds it.
	_, body = getPage(t, app, cookie, "/assets/?q=loki&only=1")
	mustContain(t, "search loki", body, ">Zoom Zoom</a> <small>Loki</small>")
}

// TestAssetNamesBackOffAfterAFailure: ESI refusing the names call is
// recorded, and the worker does not ask again a minute later.
func TestAssetNamesBackOffAfterAFailure(t *testing.T) {
	names := &assetNamesESI{status: http.StatusNotFound, answer: `{"error":"Invalid IDs in the request"}`}
	app, _, q := buildCorpTestApp(t, names)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f := &assetsFixture{app: app, q: q, userID: user.ID, ch: seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")}
	seedNestedAssets(t, f)

	for i := 0; i < 3; i++ {
		if stored, _ := app.warmCharacterAssetNames(ctx, f.ch); stored != 0 {
			t.Fatalf("stored %d name(s) from a refused call", stored)
		}
	}
	if len(names.asked) != 1 {
		t.Fatalf("ESI was asked %d times after refusing; want once, then a wait", len(names.asked))
	}
	// The page is unaffected: type names, as before.
	cookie := sessionCookie(t, app, f.userID, f.ch.CharacterID, f.ch.Name)
	_, body := getPage(t, app, cookie, "/assets/?character=90000001")
	mustContain(t, "/assets/", body, "<summary>3 stacks inside Loki</summary>")
}
