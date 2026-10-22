package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// TestIntelChannelsOrgOwned: a corporation's intel channel is run like
// its other programmes and shared by the same rules as Discord roles.
// A director makes one for the corporation and it starts open to every
// member; an outsider never sees it; a role rule in place of the
// members rule follows the in-game role; and handing "intel channels"
// to that role in the corporation's settings lets its holders run the
// channel too, where before they could not.
func TestIntelChannelsOrgOwned(t *testing.T) {
	app, _, q := buildCorpTestApp(t, nil)
	ctx := context.Background()
	const corp, otherCorp = int64(98000201), int64(98000202)
	pilot := func(charID int64, name string, corpID int64, roles ...string) (int64, *http.Cookie) {
		t.Helper()
		u, err := q.CreateUser(ctx)
		if err != nil {
			t.Fatal(err)
		}
		seedCharacter(t, q, u.ID, charID, name)
		if err := q.UpsertCharacterCorporation(ctx, db.UpsertCharacterCorporationParams{CharacterID: charID, CorporationID: corpID, UpdatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		seedSnapshot(t, q, charID, esi.SnapCorpRoles, esi.CharacterRoles{Roles: roles})
		return u.ID, sessionCookie(t, app, u.ID, charID, name)
	}
	_, director := pilot(90000201, "The Director", corp, "Director")
	_, member := pilot(90000202, "A Member", corp)
	_, manager := pilot(90000203, "Station Boss", corp, "Station_Manager")
	_, outsider := pilot(90000204, "Somebody Else", otherCorp)

	send := func(c *http.Cookie, path string, form url.Values) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(c)
		app.Handler().ServeHTTP(httptest.NewRecorder(), req)
	}
	owner := "corporation:" + strconv.FormatInt(corp, 10)

	// The director is offered the corporation as an owner; a plain
	// member is not, and cannot make one for it anyway.
	if _, body := getPage(t, app, director, "/intel/channels/"); !strings.Contains(body, `value="`+owner+`"`) {
		t.Errorf("a director is not offered the corporation as a channel's owner")
	}
	if _, body := getPage(t, app, member, "/intel/channels/"); strings.Contains(body, `value="`+owner+`"`) {
		t.Errorf("a plain member is offered the corporation as a channel's owner")
	}
	send(member, "/intel/channels/create", url.Values{"name": {"Not allowed"}, "owner": {owner}})
	send(director, "/intel/channels/create", url.Values{"name": {"Corp intel"}, "owner": {owner}})
	chans, err := q.ListOrgIntelChannels(ctx, db.ListOrgIntelChannelsParams{OwnerKind: ownerCorporation, OwnerID: corp})
	if err != nil || len(chans) != 1 || chans[0].Name != "Corp intel" {
		t.Fatalf("the corporation's channels: %+v %v", chans, err)
	}
	cid := chans[0].ID
	base := "/intel/channels/" + strconv.FormatInt(cid, 10) + "/"

	// Open to members, read and post; never to an outsider.
	if code, body := getPage(t, app, member, base); code != http.StatusOK || !strings.Contains(body, `action="`+base+`report"`) {
		t.Errorf("a member cannot read and post in the corporation's channel: %d", code)
	}
	if code, _ := getPage(t, app, outsider, base); code != http.StatusNotFound {
		t.Errorf("an outsider sees the corporation's channel: %d", code)
	}

	// Only the in-game role, read only, in place of members.
	send(director, base+"grants/remove", url.Values{"kind": {"rule"}, "who": {ruleMember}})
	send(director, base+"grants", url.Values{"kind": {"rule"}, "who": {ruleEVERole + ":Station_Manager"}})
	if code, _ := getPage(t, app, member, base); code != http.StatusNotFound {
		t.Errorf("a member without the role still sees the channel: %d", code)
	}
	code, body := getPage(t, app, manager, base)
	if code != http.StatusOK || strings.Contains(body, `action="`+base+`report"`) {
		t.Errorf("the role's holder should read but not post: %d", code)
	}
	if strings.Contains(body, `id="access"`) {
		t.Errorf("the role's holder may manage the channel before it is handed to them")
	}
	// A member cannot change who sees it.
	send(member, base+"grants", url.Values{"kind": {"rule"}, "who": {ruleMember}, "post": {"1"}})
	if rules, _ := q.ListIntelChannelRules(ctx, cid); len(rules) != 1 || rules[0].RuleKind != ruleEVERole {
		t.Errorf("a non-manager changed the channel's rules: %+v", rules)
	}

	// Handed to the role in the corporation's settings, its holder runs it.
	send(director, "/corporations/settings/save", url.Values{"corporation": {strconv.FormatInt(corp, 10)}, "intel_who": {ruleEVERole + ":Station_Manager"}})
	if code, body := getPage(t, app, manager, base); code != http.StatusOK || !strings.Contains(body, `id="access"`) || !strings.Contains(body, `action="`+base+`report"`) {
		t.Errorf("the role's holder does not run the channel once it is handed to them: %d", code)
	}
	send(manager, base+"edit", url.Values{"name": {"Renamed intel"}})
	if c, _ := q.GetIntelChannel(ctx, cid); c.Name != "Renamed intel" {
		t.Errorf("the delegated manager could not rename the channel: %q", c.Name)
	}
}
