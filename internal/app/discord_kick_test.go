package app

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestDiscordRemovals: with removals switched on, everyone in the
// server who is owed no role by its rules is removed, whether or not
// they ever used EveSynapse; and nobody else. Bots, the owner, holders
// of a protected role and newcomers stay. It is off by default, cannot
// be switched on for a server with no rules, and the page says who
// would go before anything is switched on.
func TestDiscordRemovals(t *testing.T) {
	f := newNotifyFixture(t)
	fake := f.enableDiscord()
	cookie := sessionCookie(t, f.app, f.userID, f.ch.CharacterID, f.ch.Name)
	f.joinCorp(f.ch.CharacterID, discordCorp, "Director")
	cookie, _ = f.addServer(cookie, discordCorpOwner, "add-corp")
	cookie, _ = f.connect(cookie, "alice")
	now := time.Now().UTC()
	save := func(form url.Values) string {
		t.Helper()
		f.post(cookie, "/discord/servers/"+discordCorpGuild+"/removals", form)
		_, body := getPage(t, f.app, cookie, discordServersPath)
		return body
	}
	preview := func() string {
		t.Helper()
		f.post(cookie, "/discord/servers/"+discordCorpGuild+"/removals/preview", url.Values{})
		_, body := getPage(t, f.app, cookie, discordServersPath)
		return body
	}
	const (
		stranger = "100000000000000010" // never connected EveSynapse
		guest    = "100000000000000011" // holds the protected role
		helper   = "100000000000000012" // another bot
		newcomer = "100000000000000013" // joined an hour ago
		owner    = "100000000000000900"
	)
	fake.join(discordCorpGuild, discordAlice, discordRoleMod)
	fake.join(discordCorpGuild, discordBob) // connected, but in another corporation
	fake.join(discordCorpGuild, stranger)
	fake.join(discordCorpGuild, guest, discordRoleMod)
	fake.join(discordCorpGuild, helper)
	fake.join(discordCorpGuild, newcomer)
	fake.join(discordCorpGuild, owner)
	fake.mu.Lock()
	fake.bots[helper] = true
	fake.joined[discordCorpGuild+" "+newcomer] = now.Add(-time.Hour)
	fake.mu.Unlock()
	other, _ := f.q.CreateUser(f.ctx)
	seedCharacter(t, f.q, other.ID, fixtureCharB, "Fixture Other")
	f.joinCorp(fixtureCharB, discordOtherCorp)
	f.connect(sessionCookie(t, f.app, other.ID, fixtureCharB, "Fixture Other"), "bob")

	// Off to begin with: a pass removes nobody and asks nothing.
	calls := fake.asked()
	if n := f.app.discordKickPass(f.ctx, now); n != 0 || fake.asked() != calls {
		t.Fatalf("with removals off: %d removed, %d call(s)", n, fake.asked()-calls)
	}

	// No rules: it cannot be switched on, and the preview says why.
	if body := save(url.Values{"kick_enabled": {"1"}}); !strings.Contains(body, "Removals were not switched on") {
		t.Fatal("removals were switched on for a server with no rules")
	}
	if guild, _ := f.q.GetDiscordGuild(f.ctx, discordCorpGuild); guild.KickEnabled {
		t.Fatal("removals are on for a server with no rules")
	}
	mustContain(t, "preview with no rules", preview(), "has no role rules, so nobody would be removed")

	// A rule for members only. The preview names who would go, and
	// changes nothing.
	f.addRule(cookie, discordCorpGuild, ruleMember, discordRoleMember)
	body := preview()
	mustContain(t, "preview", body, "3 of 7 members would be removed from Server 1", "name-10", "name-11", "name-02")
	if strings.Contains(body, "name-12") || strings.Contains(body, "name-13") || strings.Contains(body, "name-01,") {
		t.Fatalf("the preview lists somebody who is protected: %s", body[strings.Index(body, "would be removed"):][:200])
	}
	if len(fake.kicked) != 0 {
		t.Fatalf("the preview removed %q", fake.kicked)
	}

	// Switched on, with the moderators' role protected.
	body = save(url.Values{"kick_enabled": {"1"}, "exempt": {discordRoleMod, "300000000000000777"}})
	mustContain(t, "after switching on", body, "Removals are on for Server 1.", `name="kick_enabled" value="1" checked>`)
	guild, _ := f.q.GetDiscordGuild(f.ctx, discordCorpGuild)
	if !guild.KickEnabled || guild.KickExemptRoles != discordRoleMod {
		t.Fatalf("stored %v, exempt %q; want on, with only the role that exists", guild.KickEnabled, guild.KickExemptRoles)
	}
	if n := f.app.discordKickPass(f.ctx, now); n != 2 {
		t.Fatalf("%d removed, want the stranger and the member of another corporation: %q", n, fake.kicked)
	}
	gone := strings.Join(fake.kicked, " ")
	for _, id := range []string{stranger, discordBob} {
		if !strings.Contains(gone, id) {
			t.Errorf("%s was not removed", id)
		}
	}
	for name, id := range map[string]string{"the director": discordAlice, "the guest with a protected role": guest, "a bot": helper, "a newcomer": newcomer, "the owner": owner} {
		if strings.Contains(gone, id) {
			t.Errorf("%s was removed", name)
		}
	}
	// Not gone through again for half an hour.
	calls = fake.asked()
	if n := f.app.discordKickPass(f.ctx, now.Add(5*time.Minute)); n != 0 || fake.asked() != calls {
		t.Fatalf("five minutes later: %d removed, %d call(s)", n, fake.asked()-calls)
	}

	// The newcomer's day runs out, and they still have not connected.
	if n := f.app.discordKickPass(f.ctx, now.Add(25*time.Hour)); n != 1 || !strings.Contains(strings.Join(fake.kicked, " "), newcomer) {
		t.Fatalf("after the newcomer's first day: %d removed, %q", n, fake.kicked)
	}

	// The director's character leaves the corporation: at the next
	// look they are owed nothing, and go too. (Their protected role
	// was the moderators'; take it away first.)
	fake.join(discordCorpGuild, discordAlice)
	f.joinCorp(f.ch.CharacterID, discordOtherCorp)
	if n := f.app.discordKickPass(f.ctx, now.Add(26*time.Hour)); n != 1 || !strings.Contains(strings.Join(fake.kicked, " "), discordAlice) {
		t.Fatalf("after leaving the corporation: %d removed, %q", n, fake.kicked)
	}

	// The member list cannot be read: nobody is removed.
	fake.join(discordCorpGuild, stranger)
	fake.mu.Lock()
	fake.noList = true
	fake.mu.Unlock()
	before := len(fake.kicked)
	if n := f.app.discordKickPass(f.ctx, now.Add(27*time.Hour)); n != 0 || len(fake.kicked) != before {
		t.Fatalf("with the member list refused: %d removed", n)
	}
}
