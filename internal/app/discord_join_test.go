package app

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestDiscordAddsMembersToTheirServer: someone with a character in the
// corporation, who has connected Discord, is put in the corporation's
// server with their roles, without an invite. A stranger to the
// corporation is not, even when a rule there would give them a role.
// The directors can switch it off, and an account that took its
// permission back on Discord is left alone and asked to reconnect.
func TestDiscordAddsMembersToTheirServer(t *testing.T) {
	f := newNotifyFixture(t)
	fake := f.enableDiscord()
	cookie := sessionCookie(t, f.app, f.userID, f.ch.CharacterID, f.ch.Name)
	f.joinCorp(f.ch.CharacterID, discordCorp, "Director")
	cookie, _ = f.addServer(cookie, discordCorpOwner, "add-corp")
	// A role for everyone connected, and one for members.
	f.setServer(cookie, discordCorpGuild, discordRoleLinked, discordRoleMember, "")
	// A server adds its members unless its directors untick the box
	// (the helper above saves the form without it).
	if guild, _ := f.q.GetDiscordGuild(f.ctx, discordCorpGuild); guild.AutoJoin {
		t.Fatal("saving the form with the box unticked left the adding on")
	}
	f.post(cookie, "/discord/servers/"+discordCorpGuild+"/save", url.Values{"ops_channel": {""}, "auto_join": {"1"}})
	now := notifyT0

	// A member of the corporation connects: added, with both roles.
	cookie, _ = f.connect(cookie, "alice")
	if n := f.app.discordSyncRoles(f.ctx, now); n != 1 {
		t.Fatalf("%d change(s), want the member added", n)
	}
	if len(fake.added) != 1 || fake.added[0] != discordCorpGuild+" "+discordAlice {
		t.Fatalf("added %q, want the member put in the corporation's server", fake.added)
	}
	if got, want := fake.held(discordCorpGuild, discordAlice), discordRoleLinked+","+discordRoleMember; got != want {
		t.Fatalf("the added member holds %s, want %s", got, want)
	}
	// Once in, nothing more is asked.
	calls := fake.asked()
	if n := f.app.discordSyncRoles(f.ctx, now.Add(time.Minute)); n != 0 || fake.asked() != calls {
		t.Fatalf("after adding: %d change(s), %d call(s)", n, fake.asked()-calls)
	}

	// Somebody from another corporation connects. The "everyone
	// connected" rule would give them a role here, but they are not
	// put in a server that is not theirs.
	other, _ := f.q.CreateUser(f.ctx)
	seedCharacter(t, f.q, other.ID, fixtureCharB, "Fixture Other")
	f.joinCorp(fixtureCharB, discordOtherCorp)
	theirs := sessionCookie(t, f.app, other.ID, fixtureCharB, "Fixture Other")
	theirs, _ = f.connect(theirs, "bob")
	f.app.discordSyncRoles(f.ctx, now.Add(2*time.Minute))
	if len(fake.added) != 1 {
		t.Fatalf("a stranger to the corporation was added to its server: %q", fake.added)
	}
	// They join the corporation: now they are added, at the next pass.
	f.joinCorp(fixtureCharB, discordCorp)
	f.app.discordSyncRoles(f.ctx, now.Add(3*time.Minute))
	if len(fake.added) != 2 || fake.added[1] != discordCorpGuild+" "+discordBob {
		t.Fatalf("after joining the corporation: added %q", fake.added)
	}

	// Switched off by the directors: nobody new is added.
	_, body := getPage(t, f.app, cookie, discordServersPath)
	mustContain(t, "server settings", body, `name="auto_join" value="1" checked>`)
	f.post(cookie, "/discord/servers/"+discordCorpGuild+"/save", url.Values{"ops_channel": {""}})
	if guild, _ := f.q.GetDiscordGuild(f.ctx, discordCorpGuild); guild.AutoJoin {
		t.Fatal("unticking did not switch the adding off")
	}
	third, _ := f.q.CreateUser(f.ctx)
	seedCharacter(t, f.q, third.ID, 90000003, "Fixture Third")
	f.joinCorp(90000003, discordCorp)
	fake.mu.Lock()
	fake.codes["carol"] = "100000000000000003"
	fake.mu.Unlock()
	thirds := sessionCookie(t, f.app, third.ID, 90000003, "Fixture Third")
	f.connect(thirds, "carol")
	f.app.discordSyncRoles(f.ctx, now.Add(4*time.Minute))
	if len(fake.added) != 2 {
		t.Fatalf("someone was added with the adding switched off: %q", fake.added)
	}

	// Back on; but the bot is not allowed to add members in that
	// server. It is tried once, not on every pass.
	f.post(cookie, "/discord/servers/"+discordCorpGuild+"/save", url.Values{"ops_channel": {""}, "auto_join": {"1"}})
	if err := f.q.DeleteDiscordNonMemberGrants(f.ctx, "100000000000000003"); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.noJoin = true
	fake.mu.Unlock()
	f.app.discordSyncRoles(f.ctx, now.Add(5*time.Minute))
	calls = fake.asked()
	f.app.discordSyncRoles(f.ctx, now.Add(6*time.Minute))
	if fake.asked() != calls {
		t.Fatalf("a server that refuses new members was asked again a minute later (%d call(s))", fake.asked()-calls)
	}
	fake.mu.Lock()
	fake.noJoin = false
	fake.mu.Unlock()

	// An account whose token has run out has it renewed; one that took
	// its permission back on Discord is left alone, the stored token
	// is forgotten, and the settings page asks them to connect again.
	if _, err := f.app.db.ExecContext(f.ctx, `UPDATE discord_links SET token_expiry = $1 WHERE user_id = $2`, now.Add(-time.Hour), third.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.q.DeleteDiscordNonMemberGrants(f.ctx, "100000000000000003"); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.revoked = true
	fake.mu.Unlock()
	f.app.discordSyncRoles(f.ctx, now.Add(7*time.Minute))
	if len(fake.added) != 2 || fake.refreshes != 1 {
		t.Fatalf("with permission taken back: added %q after %d renewal(s)", fake.added, fake.refreshes)
	}
	link, err := f.q.GetDiscordLink(f.ctx, third.ID)
	if err != nil || link.AccessToken != "" || link.RefreshToken != "" {
		t.Fatalf("the unusable token was kept: %+v, %v", link, err)
	}
	_, body = getPage(t, f.app, thirds, discordSettingsPath)
	mustContain(t, "settings after permission was taken back", body, "Connect Discord again")
	if _, body = getPage(t, f.app, cookie, discordSettingsPath); strings.Contains(body, "Connect Discord again") {
		t.Fatal("an account with a working token is asked to reconnect")
	}

	// Reconnecting brings them in at the next pass.
	fake.mu.Lock()
	fake.revoked = false
	fake.mu.Unlock()
	f.connect(thirds, "carol")
	f.app.discordSyncRoles(f.ctx, now.Add(8*time.Minute))
	if len(fake.added) != 3 {
		t.Fatalf("after reconnecting: added %q", fake.added)
	}
	_ = theirs
}
