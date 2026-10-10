package app

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// savedFit stores a fit for an account, as the fitting tool would.
func (f *notifyFixture) savedFit(userID int64, name string, ship int64, public bool, tags string) int64 {
	f.t.Helper()
	now := time.Now().UTC()
	row, err := f.q.CreateLocalFitting(f.ctx, db.CreateLocalFittingParams{
		UserID: userID, Name: name, ShipTypeID: ship, IsPublic: public, CreatedAt: now, UpdatedAt: now,
		ItemsJson: `{"name":"` + name + `","tags":[` + tags + `],"shipTypeId":` + strconv.FormatInt(ship, 10) + `,"items":[{"typeId":2048,"qty":1}],"charges":{}}`,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return row.ID
}

func (f *notifyFixture) doctrines(corp int64) []db.Doctrine {
	f.t.Helper()
	rows, err := f.q.ListDoctrinesForCorporations(f.ctx, []int64{corp})
	if err != nil {
		f.t.Fatal(err)
	}
	return rows
}

// TestDoctrines: a corporation's doctrines are kept by its directors
// and whoever they hand it to, read by its members and nobody else;
// fits are copied in from saved and public fits; an op names one; the
// corporate fits page lists only doctrine fits and the public fits page
// only public ones.
func TestDoctrines(t *testing.T) {
	f := newNotifyFixture(t)
	director := sessionCookie(t, f.app, f.userID, f.ch.CharacterID, f.ch.Name)
	f.joinCorp(f.ch.CharacterID, discordCorp, "Director")
	mate, _ := f.q.CreateUser(f.ctx)
	seedCharacter(t, f.q, mate.ID, fixtureCharB, "Fixture Mate")
	f.joinCorp(fixtureCharB, discordCorp, "Accountant")
	member := sessionCookie(t, f.app, mate.ID, fixtureCharB, "Fixture Mate")
	other, _ := f.q.CreateUser(f.ctx)
	seedCharacter(t, f.q, other.ID, 90000003, "Fixture Stranger")
	f.joinCorp(90000003, discordOtherCorp, "Director")
	stranger := sessionCookie(t, f.app, other.ID, 90000003, "Fixture Stranger")

	mine := f.savedFit(f.userID, "Fleet Guardian", 11987, false, `"logi"`)
	theirs := f.savedFit(other.ID, "Secret Loki", 29990, false, ``)
	public := f.savedFit(other.ID, "Public Rifter", 587, true, `"frigate"`)

	// Only a keeper makes a doctrine, and only for their own corporation.
	create := url.Values{"corporation": {"98000001"}, "name": {" Shield   Lokis "}, "category": {"Stratop"}, "tags": {"shield, t3c, Shield"}}
	f.post(member, "/doctrines/create", create)
	f.post(stranger, "/doctrines/create", create)
	if len(f.doctrines(discordCorp)) != 0 {
		t.Fatal("somebody who does not keep the corporation's doctrines made one")
	}
	f.post(director, "/doctrines/create", create)
	f.post(director, "/doctrines/create", url.Values{"corporation": {"98000001"}, "name": {"shield lokis"}})
	made := f.doctrines(discordCorp)
	if len(made) != 1 || made[0].Name != "Shield Lokis" || made[0].Tags != "shield,t3c" {
		t.Fatalf("doctrines %+v, want the one, tidied", made)
	}
	d := made[0]
	page := doctrineURL(d.ID)

	// Fits: the keeper's own, a public one, never somebody's private one.
	add := page + "/fits/add"
	f.post(director, add, url.Values{"fit": {strconv.FormatInt(mine, 10)}, "fleet_role": {"Logistics"}, "note": {"anchor on the FC"}})
	f.post(director, add, url.Values{"fit": {strconv.FormatInt(theirs, 10)}})
	f.post(director, add, url.Values{"public": {strconv.FormatInt(theirs, 10)}})
	f.post(director, add, url.Values{"public": {strconv.FormatInt(public, 10)}, "fleet_role": {"nonsense"}})
	f.post(member, add, url.Values{"public": {strconv.FormatInt(public, 10)}})
	fits, _ := f.q.ListDoctrineFits(f.ctx, []int64{d.ID})
	if len(fits) != 2 || fits[0].Name != "Fleet Guardian" || fits[0].FleetRole != "Logistics" || fits[1].Name != "Public Rifter" || fits[1].FleetRole != "" {
		t.Fatalf("doctrine fits %+v", fits)
	}
	if in, _ := f.q.DoctrineHasShip(f.ctx, db.DoctrineHasShipParams{DoctrineID: d.ID, ShipTypeID: 11987}); !in {
		t.Fatal("the doctrine does not know its own ship")
	}

	// Members read it; the fitting tool opens its fits for them; outsiders get neither.
	_, body := getPage(t, f.app, member, page)
	mustContain(t, "doctrine page for a member", body, "Shield Lokis", "Fleet Guardian", "anchor on the FC", "/fittings/?doctrine="+strconv.FormatInt(fits[0].ID, 10))
	if strings.Contains(body, "Keep this doctrine") {
		t.Fatal("a member is offered the keeper's controls")
	}
	if _, body = getPage(t, f.app, stranger, page); strings.Contains(body, "Fleet Guardian") {
		t.Fatal("an outsider reads another corporation's doctrine")
	}
	if doc, ok := f.app.doctrineFitDoc(f.ctx, mate.ID, fits[0].ID); !ok || doc.ShipTypeID != 11987 {
		t.Fatalf("a member cannot open the doctrine's fit: %+v", doc)
	}
	if _, ok := f.app.doctrineFitDoc(f.ctx, other.ID, fits[0].ID); ok {
		t.Fatal("an outsider can open another corporation's doctrine fit")
	}

	// The list finds a doctrine by its fits' ships, its category and its tags.
	for _, query := range []string{"", "?q=guardian", "?category=stratop", "?tag=T3C"} {
		_, body = getPage(t, f.app, member, doctrinesPath+query)
		mustContain(t, "doctrines list "+query, body, `<a href="`+page+`">Shield Lokis</a>`)
	}
	if _, body = getPage(t, f.app, member, doctrinesPath+"?tag=armor"); strings.Contains(body, `<a href="`+page+`">`) {
		t.Fatal("a doctrine is listed under a tag it does not have")
	}

	// Corporate fits: the doctrines' fits only, by tag and doctrine, for members only.
	browse := func(cookie *http.Cookie, address string) string {
		_, body := getPage(t, f.app, cookie, address)
		return body
	}
	corporate := browse(member, doctrineFitsPath)
	mustContain(t, "corporate fits", corporate, "Fleet Guardian", "Public Rifter", `<a href="`+page+`">Shield Lokis</a>`)
	if strings.Contains(corporate, "Fixture Stranger") || strings.Contains(corporate, "Secret Loki") {
		t.Fatal("corporate fits lists a public fit's author, or a private fit")
	}
	for query, want := range map[string][2]string{
		"?tag=frigate": {"Public Rifter", "Fleet Guardian"},
		"?tag=LOGI":    {"Fleet Guardian", "Public Rifter"},
		"?q=guardian":  {"Fleet Guardian", "Public Rifter"},
		"?doctrine=" + strconv.FormatInt(d.ID+1000, 10): {"No fit matches", "fit-editor\">Fleet Guardian"},
	} {
		body := browse(member, doctrineFitsPath+query)
		if !strings.Contains(body, want[0]) || strings.Contains(body, want[1]) {
			t.Fatalf("corporate fits %s: want %q without %q", query, want[0], want[1])
		}
	}
	if body := browse(stranger, doctrineFitsPath); strings.Contains(body, "Fleet Guardian") || strings.Contains(body, "Public Rifter") {
		t.Fatal("corporate fits shows an outsider another corporation's fits")
	}

	// Public fits: everyone's public fits and nothing else, by name and tag.
	public0 := browse(member, publicFitsPath)
	mustContain(t, "public fits", public0, "Public Rifter", "Fixture Stranger", "/fittings/?public="+strconv.FormatInt(public, 10))
	if strings.Contains(public0, "Fleet Guardian") || strings.Contains(public0, "Secret Loki") || strings.Contains(public0, "Shield Lokis") {
		t.Fatal("public fits lists a corporate fit, a doctrine, or a private fit")
	}
	mustContain(t, "public fits by tag", browse(member, publicFitsPath+"?tag=FRIGATE"), "Public Rifter")
	if strings.Contains(browse(member, publicFitsPath+"?tag=logi"), "Public Rifter") || strings.Contains(browse(member, publicFitsPath+"?q=loki"), "Public Rifter") {
		t.Fatal("public fits ignores its filters")
	}
	mustContain(t, "public fits for their owner", browse(stranger, publicFitsPath), "Public Rifter", "/fittings/?local="+strconv.FormatInt(public, 10))

	// Only a keeper who came from a doctrine gets the add buttons, on either page.
	chosen := "?for=" + strconv.FormatInt(d.ID, 10)
	if strings.Contains(browse(member, doctrineFitsPath+chosen)+browse(member, publicFitsPath+chosen), "/fits/add") {
		t.Fatal("a member is offered the add-to-doctrine buttons")
	}
	mustContain(t, "corporate fits for a keeper", browse(director, doctrineFitsPath+chosen), page+"/fits/add", `name="copy"`)
	mustContain(t, "public fits for a keeper", browse(director, publicFitsPath+chosen), page+"/fits/add", `name="public"`)

	// An op names the doctrine; one of another corporation is refused.
	start := time.Now().UTC().Add(48 * time.Hour).Format(opDateTimeLayout)
	op := url.Values{"title": {"Stratop"}, "corporation": {"98000001"}, "fc": {"90000001"}, "starts_at": {start}, "duration": {"60"}}
	op.Set("doctrine_id", "999999")
	f.post(director, "/ops/save", op)
	op.Set("doctrine_id", strconv.FormatInt(d.ID, 10))
	f.post(director, "/ops/save", op)
	ops, _ := f.q.ListOpsForCorporationsBetween(f.ctx, db.ListOpsForCorporationsBetweenParams{
		CorporationIds: []int64{discordCorp}, FromTime: time.Now(), ToTime: time.Now().Add(72 * time.Hour),
	})
	if len(ops) != 1 || ops[0].DoctrineID != d.ID || ops[0].Doctrine != "Shield Lokis" {
		t.Fatalf("ops %+v, want the one naming the doctrine", ops)
	}
	_, body = getPage(t, f.app, member, opURL(ops[0].ID))
	mustContain(t, "op page", body, `<a href="`+page+`">Shield Lokis</a>`, "Bring", "Fleet Guardian")

	// Handing it on: an accountant keeps doctrines once the director says so.
	f.post(member, "/corporations/settings/save", url.Values{"corporation": {"98000001"}, "doctrines_who": {"eve_role:Accountant"}})
	f.post(member, page+"/save", url.Values{"name": {"Hijacked"}})
	f.post(member, page+"/fits/"+strconv.FormatInt(fits[0].ID, 10)+"/remove", url.Values{})
	f.post(member, page+"/delete", url.Values{})
	if kept := f.doctrines(discordCorp); len(kept) != 1 || kept[0].Name != "Shield Lokis" {
		t.Fatal("a member changed the doctrine, or who keeps it")
	}
	f.post(director, "/corporations/settings/save", url.Values{"corporation": {"98000001"}, "doctrines_who": {"eve_role:Accountant"}})
	f.post(member, page+"/save", url.Values{"name": {"Shield Lokis"}, "category": {"Roam"}})
	f.post(member, page+"/fits/"+strconv.FormatInt(fits[1].ID, 10)+"/remove", url.Values{})
	if kept := f.doctrines(discordCorp); kept[0].Category != "Roam" {
		t.Fatal("an accountant handed the doctrines could not change one")
	}
	if left, _ := f.q.ListDoctrineFits(f.ctx, []int64{d.ID}); len(left) != 1 {
		t.Fatalf("%d fit(s) left, want one", len(left))
	}
	f.post(stranger, page+"/delete", url.Values{})
	f.post(director, page+"/delete", url.Values{})
	if len(f.doctrines(discordCorp)) != 0 {
		t.Fatal("the director could not delete the doctrine")
	}
	if kept, _ := f.q.GetOp(f.ctx, ops[0].ID); kept.Doctrine != "Shield Lokis" {
		t.Fatal("the op lost its doctrine's name when the doctrine was deleted")
	}
}

// TestFitToolOpensLibraryFits: a pasted fit goes into a doctrine with
// what could not be read left out, and the fitting tool opens a
// doctrine's fit and a public fit, but not a doctrine fit of a
// corporation the account is not in.
func TestFitToolOpensLibraryFits(t *testing.T) {
	app, cookie := fitTestApp(t)
	ctx, q := context.Background(), app.queries
	inCorp := func(corp int64) {
		if err := q.UpsertCharacterCorporation(ctx, db.UpsertCharacterCorporationParams{CharacterID: fixtureCharA, CorporationID: corp, UpdatedAt: notifyT0}); err != nil {
			t.Fatal(err)
		}
	}
	inCorp(discordCorp)
	seedSnapshot(t, q, fixtureCharA, esi.SnapCorpRoles, esi.CharacterRoles{Roles: []string{"Director"}})

	postForm(t, app, cookie, "/doctrines/create", url.Values{"corporation": {"98000001"}, "name": {"Blaster boats"}})
	made, _ := q.ListDoctrinesForCorporations(ctx, []int64{discordCorp})
	if len(made) != 1 {
		t.Fatalf("%d doctrine(s), want one", len(made))
	}
	postForm(t, app, cookie, doctrineURL(made[0].ID)+"/fits/add", url.Values{
		"eft": {"[Fixture Frigate, Pasted Boat]\nFixture Blaster\nBogus Module X9\n"}, "fleet_role": {"DPS"},
	})
	postForm(t, app, cookie, doctrineURL(made[0].ID)+"/fits/add", url.Values{"eft": {"not a fit at all"}})
	fits, _ := q.ListDoctrineFits(ctx, []int64{made[0].ID})
	if len(fits) != 1 || fits[0].Name != "Pasted Boat" || fits[0].ShipTypeID != fitShipID || fits[0].FleetRole != "DPS" {
		t.Fatalf("pasted fits %+v", fits)
	}
	_, body := getPage(t, app, cookie, doctrineURL(made[0].ID))
	mustContain(t, "doctrine page", body, "[Fixture Frigate, Pasted Boat]", "Fixture Blaster", "Keep this doctrine")

	author, _ := q.CreateUser(ctx)
	now := time.Now().UTC()
	public, err := q.CreateLocalFitting(ctx, db.CreateLocalFittingParams{
		UserID: author.ID, Name: "Shared Boat", ShipTypeID: fitShipID, IsPublic: true, CreatedAt: now, UpdatedAt: now,
		ItemsJson: `{"name":"Shared Boat","shipTypeId":1001,"items":[{"typeId":3001,"qty":1}],"charges":{}}`,
	})
	if err != nil {
		t.Fatal(err)
	}

	open := "/fittings/?doctrine=" + strconv.FormatInt(fits[0].ID, 10)
	_, body = getPage(t, app, cookie, open)
	mustContain(t, "fitting tool on a doctrine fit", body, "Pasted Boat", "Fixture Blaster", "Damage per second")
	_, body = getPage(t, app, cookie, "/fittings/?public="+strconv.FormatInt(public.ID, 10))
	mustContain(t, "fitting tool on a public fit", body, "Shared Boat", "Fixture Blaster")

	inCorp(discordOtherCorp)
	if _, body = getPage(t, app, cookie, open); strings.Contains(body, "Pasted Boat") {
		t.Fatal("the fitting tool opened a doctrine fit of a corporation the account is not in")
	}
}
