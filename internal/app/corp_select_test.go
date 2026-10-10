package app

import (
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
)

// TestCorporationPagesShowOneCorporation: the doctrine pages show the
// corporation the address names, among the player corporations the
// account is in; settings are for directors and save in one go; the
// search boxes suggest only what the account may read; and the fit
// browser's chips filter.
func TestCorporationPagesShowOneCorporation(t *testing.T) {
	f := newNotifyFixture(t)
	director := sessionCookie(t, f.app, f.userID, f.ch.CharacterID, f.ch.Name)
	f.joinCorp(f.ch.CharacterID, discordCorp, "Director")
	seedCharacter(t, f.q, f.userID, 90000005, "Fixture Alt")
	f.joinCorp(90000005, discordOtherCorp)
	seedCharacter(t, f.q, f.userID, 90000006, "Fixture Rookie")
	f.joinCorp(90000006, 1000125)
	other, _ := f.q.CreateUser(f.ctx)
	seedCharacter(t, f.q, other.ID, 90000003, "Fixture Stranger")
	f.joinCorp(90000003, 98000009)
	stranger := sessionCookie(t, f.app, other.ID, 90000003, "Fixture Stranger")

	f.post(director, "/doctrines/create", url.Values{"corporation": {"98000001"}, "name": {"Shield Lokis"}, "category": {"Stratop"}, "tags": {"shield"}})
	mine := f.doctrines(discordCorp)[0]
	now := time.Now().UTC()
	if _, err := f.q.CreateDoctrine(f.ctx, db.CreateDoctrineParams{CorporationID: discordOtherCorp, Name: "Armor Abaddons", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	add := doctrineURL(mine.ID) + "/fits/add"
	guardian := f.savedFit(f.userID, "Fleet Guardian", 11987, false, `"logi"`)
	f.post(director, add, url.Values{"fit": {strconv.FormatInt(guardian, 10)}, "fleet_role": {"Logistics"}})
	f.savedFit(f.userID, "Mainline Loki", 29990, false, ``)
	f.post(director, add, url.Values{"fit_name": {"mainline loki"}, "fleet_role": {"DPS"}})
	f.post(director, add, url.Values{"fit_name": {"no such fit"}})
	if fits, _ := f.q.ListDoctrineFits(f.ctx, []int64{mine.ID}); len(fits) != 2 || fits[1].Name != "Mainline Loki" {
		t.Fatalf("doctrine fits %+v, want the picked one and the one named", fits)
	}
	f.savedFit(other.ID, "Public Rifter", 587, true, `"frigate"`)

	get := func(address string) string {
		_, body := getPage(t, f.app, director, address)
		return body
	}
	// One corporation at a time; the selector offers the player corporations only.
	home := get(doctrinesPath)
	mustContain(t, "doctrines, the character's corporation", home, "Shield Lokis", `href="?corporation=98000001"`, `href="?corporation=98000002"`)
	if strings.Contains(home, "Armor Abaddons") || strings.Contains(home, "corporation=1000125") {
		t.Fatal("the doctrines page shows a second corporation's doctrines, or offers an NPC corporation")
	}
	away := get(doctrinesPath + "?corporation=98000002")
	if !strings.Contains(away, "Armor Abaddons") || strings.Contains(away, "Shield Lokis") || strings.Contains(away, "New doctrine") {
		t.Fatal("the other corporation's page is wrong, or offers a member the keeper's form")
	}
	mustContain(t, "doctrines, a corporation the account is not in", get(doctrinesPath+"?corporation=98000009"), "Shield Lokis")

	// Settings: the corporations the account directs, saved together.
	settings := get(corpSettingsPath)
	mustContain(t, "corporation settings", settings, `name="srp_who"`, `name="doctrines_who"`, `name="srp_policy"`)
	if strings.Contains(settings, `href="?corporation=98000002"`) {
		t.Fatal("settings are offered for a corporation the account does not direct")
	}
	save := url.Values{"corporation": {"98000001"}, "srp_who": {"eve_role:Accountant"}, "doctrines_who": {"member"}, "srp_policy": {"Doctrine ships only."}}
	f.post(director, "/corporations/settings/save", save)
	if _, err := f.q.GetCorpPermission(f.ctx, db.GetCorpPermissionParams{CorporationID: discordCorp, Permission: permSRP}); err == nil {
		t.Fatal("settings were half saved though one choice could not be made")
	}
	save.Set("doctrines_who", "eve_role:Fitting_Manager")
	f.post(stranger, "/corporations/settings/save", save)
	f.post(director, "/corporations/settings/save", url.Values{"corporation": {"98000002"}, "srp_who": {"eve_role:Accountant"}})
	if _, err := f.q.GetCorpPermission(f.ctx, db.GetCorpPermissionParams{CorporationID: discordOtherCorp, Permission: permSRP}); err == nil {
		t.Fatal("somebody who is not a director changed a corporation's settings")
	}
	f.post(director, "/corporations/settings/save", save)
	srp, _ := f.q.GetCorpPermission(f.ctx, db.GetCorpPermissionParams{CorporationID: discordCorp, Permission: permSRP})
	keeps, _ := f.q.GetCorpPermission(f.ctx, db.GetCorpPermissionParams{CorporationID: discordCorp, Permission: permDoctrines})
	policy, _ := f.q.GetSRPSettings(f.ctx, discordCorp)
	if srp.Ref != "Accountant" || keeps.Ref != "Fitting_Manager" || policy.Policy != "Doctrine ships only." {
		t.Fatalf("saved settings: srp %+v, doctrines %+v, policy %q", srp, keeps, policy.Policy)
	}
	mustContain(t, "settings after saving", get(corpSettingsPath), `value="eve_role:Fitting_Manager" selected`, "Doctrine ships only.")
	if _, body := getPage(t, f.app, stranger, corpSettingsPath); !strings.Contains(body, "no settings for you to change") {
		t.Fatal("somebody who directs nothing is shown settings")
	}

	// Suggestions: a corporation's names go to its members only.
	mustContain(t, "doctrine suggestions", get("/doctrines/suggest?corporation=98000001&q=shi"), "Shield Lokis", doctrineURL(mine.ID), `"Tag"`)
	mustContain(t, "fit suggestions", get("/doctrines/suggest?scope=fits&corporation=98000001&q=guard"), "Fleet Guardian", "/fittings/?doctrine=")
	mustContain(t, "category suggestions", get("/doctrines/suggest?scope=categories&corporation=98000001&q=str"), "Stratop")
	mustContain(t, "saved fit suggestions", get("/doctrines/suggest?scope=myfits&q=main"), "Mainline Loki")
	mustContain(t, "public fit suggestions", get("/fittings/public/suggest?q=rif"), "Public Rifter", "/fittings/?public=")
	for _, address := range []string{"/doctrines/suggest?corporation=98000001&q=shi", "/doctrines/suggest?scope=fits&corporation=98000001&q=guard", "/doctrines/suggest?scope=myfits&q=main"} {
		if _, body := getPage(t, f.app, stranger, address); strings.TrimSpace(body) != "[]" {
			t.Fatalf("an outsider was suggested %s from %s", body, address)
		}
	}

	// The fit browser: chips with counts, each a filter.
	fits := get(doctrineFitsPath)
	mustContain(t, "corporate fits", fits, "Fleet Guardian", "Mainline Loki", "part=Logistics", "<small>1</small>", "2</strong> of 2 fits")
	logi := get(doctrineFitsPath + "?corporation=98000001&part=logistics")
	if !strings.Contains(logi, "Fleet Guardian") || strings.Contains(logi, `fit-editor">Mainline Loki`) || !strings.Contains(logi, "Part: logistics") {
		t.Fatal("the part chip does not filter the corporate fits")
	}
	if byName := get(doctrineFitsPath + "?sort=name"); strings.Index(byName, `fit-editor">Fleet Guardian`) > strings.Index(byName, `fit-editor">Mainline Loki`) {
		t.Fatal("corporate fits are not sorted by name")
	}
}
