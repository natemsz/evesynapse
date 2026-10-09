package app

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

const (
	groupRoleCEO      = "300000000000000011"
	groupRoleDirector = "300000000000000012"
	groupRoleLogi     = "300000000000000013"
)

// ceoOf records a character as the CEO of a corporation, in the
// corporation's stored record.
func (f *notifyFixture) ceoOf(corporationID, characterID int64) {
	f.t.Helper()
	payload, _ := json.Marshal(corporationRecordPayload{Corp: esi.Corporation{Name: "Corp", CEOID: characterID}})
	if err := f.q.SetCorporationRecord(f.ctx, db.SetCorporationRecordParams{
		CorporationID: corporationID, Payload: string(payload), State: orgStateReady, FetchedAt: timeSet(notifyT0),
	}); err != nil {
		f.t.Fatal(err)
	}
}

// group returns the id of an owner's group by name.
func (f *notifyFixture) group(name string) int64 {
	f.t.Helper()
	groups, err := f.q.ListOrgGroupsForOwner(f.ctx, db.ListOrgGroupsForOwnerParams{OwnerKind: ownerCorporation, OwnerID: discordCorp})
	if err != nil {
		f.t.Fatal(err)
	}
	for _, g := range groups {
		if g.Name == name {
			return g.ID
		}
	}
	return 0
}

// TestGroupsAreKeptByDirectors: a director makes groups for the
// corporation and puts its linked characters in them; nobody else can,
// and nobody from outside the corporation can be put in.
func TestGroupsAreKeptByDirectors(t *testing.T) {
	f := newNotifyFixture(t)
	cookie := sessionCookie(t, f.app, f.userID, f.ch.CharacterID, f.ch.Name)
	f.joinCorp(f.ch.CharacterID, discordCorp)

	// A member is offered nothing and can make nothing.
	_, body := getPage(t, f.app, cookie, groupsPath)
	mustContain(t, "groups page for a member", body, "None of your characters is a director")
	f.post(cookie, "/groups/create", url.Values{"owner": {discordCorpOwner}, "name": {"Sneaky"}})
	if f.group("Sneaky") != 0 {
		t.Fatal("somebody who is not a director made a group")
	}

	f.joinCorp(f.ch.CharacterID, discordCorp, "Director")
	// Another account's character in the corporation, and one outside it.
	mate, _ := f.q.CreateUser(f.ctx)
	seedCharacter(t, f.q, mate.ID, fixtureCharB, "Fixture Mate")
	f.joinCorp(fixtureCharB, discordCorp)
	stranger, _ := f.q.CreateUser(f.ctx)
	seedCharacter(t, f.q, stranger.ID, 90000003, "Fixture Stranger")
	f.joinCorp(90000003, discordOtherCorp, "Director")

	f.post(cookie, "/groups/create", url.Values{"owner": {discordCorpOwner}, "name": {"  Logistics   wing "}, "description": {"Guardians and friends"}})
	logi := f.group("Logistics wing")
	if logi == 0 {
		t.Fatal("the group was not made (or its name was not tidied)")
	}
	// The same name again, whatever the case, is refused; so is no name.
	f.post(cookie, "/groups/create", url.Values{"owner": {discordCorpOwner}, "name": {"logistics WING"}})
	f.post(cookie, "/groups/create", url.Values{"owner": {discordCorpOwner}, "name": {"   "}})
	f.post(cookie, "/groups/create", url.Values{"owner": {"corporation:98000002"}, "name": {"Not mine"}})
	if groups, _ := f.q.ListOrgGroupsForOwner(f.ctx, db.ListOrgGroupsForOwnerParams{OwnerKind: ownerCorporation, OwnerID: discordCorp}); len(groups) != 1 {
		t.Fatalf("%d group(s), want the one", len(groups))
	}
	if groups, _ := f.q.ListOrgGroupsForOwner(f.ctx, db.ListOrgGroupsForOwnerParams{OwnerKind: ownerCorporation, OwnerID: discordOtherCorp}); len(groups) != 0 {
		t.Fatal("a group was made for a corporation the account does not direct")
	}

	// Members: the corporation's linked characters, and nobody else.
	_, body = getPage(t, f.app, cookie, groupsPath)
	mustContain(t, "groups page", body, "Logistics wing", "Guardians and friends", `value="90000002"> <span class="charselector-name">Fixture Mate</span>`)
	if strings.Contains(body, "Fixture Stranger") {
		t.Fatal("a character of another corporation is offered")
	}
	add := "/groups/" + strconv.FormatInt(logi, 10) + "/members/add"
	f.post(cookie, add, url.Values{"character": {"90000002", "90000003", "95000001", "junk"}})
	members, _ := f.q.ListOrgGroupMembers(f.ctx, logi)
	if len(members) != 1 || members[0].CharacterID != fixtureCharB {
		t.Fatalf("members %+v, want only the corporation's own character", members)
	}

	// An outsider, director of another corporation, can do none of it.
	theirs := sessionCookie(t, f.app, stranger.ID, 90000003, "Fixture Stranger")
	f.post(theirs, add, url.Values{"character": {"90000003"}})
	f.post(theirs, "/groups/"+strconv.FormatInt(logi, 10)+"/members/remove", url.Values{"character": {"90000002"}})
	f.post(theirs, "/groups/"+strconv.FormatInt(logi, 10)+"/delete", url.Values{})
	if members, _ = f.q.ListOrgGroupMembers(f.ctx, logi); len(members) != 1 || f.group("Logistics wing") == 0 {
		t.Fatal("an outsider changed another corporation's group")
	}
	if _, page := getPage(t, f.app, theirs, groupsPath); strings.Contains(page, "Guardians and friends") {
		t.Fatal("another corporation's group is shown to an outsider")
	}

	// A member who leaves the corporation stays listed, marked.
	f.joinCorp(fixtureCharB, discordOtherCorp)
	_, body = getPage(t, f.app, cookie, groupsPath)
	mustContain(t, "after a member left", body, "Fixture Mate <small>no longer in the corporation")

	f.post(cookie, "/groups/"+strconv.FormatInt(logi, 10)+"/members/remove", url.Values{"character": {"90000002"}})
	if members, _ = f.q.ListOrgGroupMembers(f.ctx, logi); len(members) != 0 {
		t.Fatal("the member was not removed")
	}
}

// TestDiscordManyRoles: a server takes as many rules as its directors
// write, and a member gets every role they qualify for: CEO, an
// in-game role, a group, on top of member. Each goes when what it
// rests on goes, without touching the others.
func TestDiscordManyRoles(t *testing.T) {
	f := newNotifyFixture(t)
	fake := f.enableDiscord()
	cookie := sessionCookie(t, f.app, f.userID, f.ch.CharacterID, f.ch.Name)
	f.joinCorp(f.ch.CharacterID, discordCorp, "Director", "Accountant")
	f.ceoOf(discordCorp, f.ch.CharacterID)
	cookie, _ = f.addServer(cookie, discordCorpOwner, "add-corp")
	f.post(cookie, "/groups/create", url.Values{"owner": {discordCorpOwner}, "name": {"Logistics"}})
	logi := f.group("Logistics")
	logiRef := "group:" + strconv.FormatInt(logi, 10)

	// A second account in the corporation: a plain member, in the group.
	mate, _ := f.q.CreateUser(f.ctx)
	seedCharacter(t, f.q, mate.ID, fixtureCharB, "Fixture Mate")
	f.joinCorp(fixtureCharB, discordCorp)
	f.connect(sessionCookie(t, f.app, mate.ID, fixtureCharB, "Fixture Mate"), "bob")
	f.post(cookie, "/groups/"+strconv.FormatInt(logi, 10)+"/members/add", url.Values{"character": {"90000002"}})

	f.addRule(cookie, discordCorpGuild, ruleMember, discordRoleMember)
	f.addRule(cookie, discordCorpGuild, ruleCEO, groupRoleCEO)
	f.addRule(cookie, discordCorpGuild, "eve_role:Director", groupRoleDirector)
	body := f.addRule(cookie, discordCorpGuild, logiRef, groupRoleLogi)
	mustContain(t, "the rules", body, "<td>CEO</td>", "<td>In-game role: Director</td>", "<td>Group: Logistics</td>",
		`<option value="`+logiRef+`">Group: Logistics</option>`)
	cookie, _ = f.connect(cookie, "alice")
	fake.join(discordCorpGuild, discordAlice, discordRoleMod)
	fake.join(discordCorpGuild, discordBob)
	now := notifyT0

	f.app.discordSyncRoles(f.ctx, now)
	if got, want := fake.held(discordCorpGuild, discordAlice), groupRoleCEO+","+groupRoleDirector+","+discordRoleMember+","+discordRoleMod; got != sorted(want) {
		t.Fatalf("the CEO and director holds %s, want %s", got, sorted(want))
	}
	if got, want := fake.held(discordCorpGuild, discordBob), discordRoleMember+","+groupRoleLogi; got != sorted(want) {
		t.Fatalf("the member in the group holds %s, want %s", got, sorted(want))
	}

	// The in-game Director role is taken away: that role goes, the
	// others stay. (CEO is read from the corporation, not from roles.)
	seedSnapshot(t, f.q, f.ch.CharacterID, esi.SnapCorpRoles, esi.CharacterRoles{Roles: []string{"Accountant"}})
	f.app.discordSyncRoles(f.ctx, now.Add(60e9))
	if got, want := fake.held(discordCorpGuild, discordAlice), groupRoleCEO+","+discordRoleMember+","+discordRoleMod; got != sorted(want) {
		t.Fatalf("after losing Director: %s, want %s", got, sorted(want))
	}
	// Somebody else becomes CEO.
	f.ceoOf(discordCorp, 95000001)
	f.app.discordSyncRoles(f.ctx, now.Add(120e9))
	if got, want := fake.held(discordCorpGuild, discordAlice), discordRoleMember+","+discordRoleMod; got != sorted(want) {
		t.Fatalf("after the CEO changed: %s, want %s", got, sorted(want))
	}

	// Out of the group: the group's role goes.
	f.joinCorp(f.ch.CharacterID, discordCorp, "Director")
	f.post(cookie, "/groups/"+strconv.FormatInt(logi, 10)+"/members/remove", url.Values{"character": {"90000002"}})
	f.app.discordSyncRoles(f.ctx, now.Add(180e9))
	if got := fake.held(discordCorpGuild, discordBob); got != discordRoleMember {
		t.Fatalf("after leaving the group: %s, want the member role only", got)
	}
	// Back in, then the group is deleted: its rule goes with it, and
	// so does the role.
	f.post(cookie, "/groups/"+strconv.FormatInt(logi, 10)+"/members/add", url.Values{"character": {"90000002"}})
	f.app.discordSyncRoles(f.ctx, now.Add(240e9))
	if got, want := fake.held(discordCorpGuild, discordBob), discordRoleMember+","+groupRoleLogi; got != sorted(want) {
		t.Fatalf("back in the group: %s, want %s", got, sorted(want))
	}
	if code, _ := f.post(cookie, "/groups/"+strconv.FormatInt(logi, 10)+"/delete", url.Values{}); code != http.StatusSeeOther {
		t.Fatalf("delete group: %d", code)
	}
	rules, _ := f.q.ListDiscordRoleRulesForGuild(f.ctx, discordCorpGuild)
	for _, rule := range rules {
		if rule.Kind == ruleGroup {
			t.Fatal("a rule about a deleted group is still there")
		}
	}
	f.app.discordSyncRoles(f.ctx, now.Add(300e9))
	if got := fake.held(discordCorpGuild, discordBob); got != discordRoleMember {
		t.Fatalf("after the group was deleted: %s, want the member role only", got)
	}

	// A member of the group who moves to another corporation keeps the
	// listing but not the role.
	for _, change := range fake.changes {
		if strings.HasSuffix(change, discordRoleMod) {
			t.Fatalf("the bot touched a role that is not its own: %s", change)
		}
	}
}

// sorted returns a comma-separated list in sorted order.
func sorted(list string) string {
	parts := strings.Split(list, ",")
	for i := 1; i < len(parts); i++ {
		for j := i; j > 0 && parts[j] < parts[j-1]; j-- {
			parts[j], parts[j-1] = parts[j-1], parts[j]
		}
	}
	return strings.Join(parts, ",")
}
