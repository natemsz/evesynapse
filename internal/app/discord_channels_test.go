package app

import (
	"net/url"
	"strings"
	"testing"
)

// TestDiscordChannelAccess: a director picks a channel and the roles
// that may open it, and the bot makes it private to them, letting
// itself and the roles in before shutting everyone else out. Saving
// again replaces the list; saving with nothing ticked stops managing
// the channel without reopening it. Only that server's own channels
// and roles are taken, and only its directors can do it.
func TestDiscordChannelAccess(t *testing.T) {
	f := newNotifyFixture(t)
	fake := f.enableDiscord()
	cookie := sessionCookie(t, f.app, f.userID, f.ch.CharacterID, f.ch.Name)
	f.joinCorp(f.ch.CharacterID, discordCorp, "Director")
	cookie, _ = f.addServer(cookie, discordCorpOwner, "add-corp")
	save := func(_ any, form url.Values) string {
		t.Helper()
		f.post(cookie, "/discord/servers/"+discordCorpGuild+"/channels", form)
		_, body := getPage(t, f.app, cookie, discordServersPath)
		return body
	}
	edits := func() string {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		out := strings.Join(fake.channelEdits, "; ")
		fake.channelEdits = nil
		return out
	}
	stored := func() string {
		t.Helper()
		rows, err := f.q.ListDiscordChannelAccess(f.ctx, discordCorpGuild)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, row := range rows {
			out = append(out, row.ChannelID+"="+row.RoleID)
		}
		return strings.Join(out, " ")
	}
	ch := discordOpsChannel

	_, body := getPage(t, f.app, cookie, discordServersPath)
	mustContain(t, "the form", body, `action="/discord/servers/`+discordCorpGuild+`/channels"`,
		`<option value="`+ch+`">#ops</option>`, `<option value="`+discordVoiceChannel+`">Voice: Voice</option>`,
		"The bot is not keeping any channel private here.")

	// Two roles let in: the bot first, then the roles, then everyone
	// else shut out.
	body = save(nil, url.Values{"channel": {ch}, "role": {discordRoleMember, discordRoleLinked}})
	mustContain(t, "after saving", body, "#ops is now private to the roles you ticked.", "<td>#ops</td><td>Linked, Member</td>")
	want := "allow " + ch + " " + discordBotUser + "; allow " + ch + " " + discordRoleLinked + "; allow " + ch + " " + discordRoleMember + "; deny " + ch + " " + discordCorpGuild
	if got := edits(); got != want {
		t.Fatalf("changes made:\n %s\nwant:\n %s", got, want)
	}
	if got, want := stored(), ch+"="+discordRoleLinked+" "+ch+"="+discordRoleMember; got != want {
		t.Fatalf("recorded %q, want %q", got, want)
	}

	// Saved again with one role: the other's entry is taken away.
	save(nil, url.Values{"channel": {ch}, "role": {discordRoleMember}})
	if got := edits(); !strings.HasSuffix(got, "; clear "+ch+" "+discordRoleLinked) || strings.Contains(got, "clear "+ch+" "+discordRoleMember) {
		t.Fatalf("changes made on the second save: %s", got)
	}
	if got := stored(); got != ch+"="+discordRoleMember {
		t.Fatalf("recorded %q after the second save", got)
	}

	// What is not this server's is refused, and nothing is changed.
	for name, form := range map[string]url.Values{
		"a channel of no server": {"channel": {"400000000000000777"}, "role": {discordRoleMember}},
		"a role of no server":    {"channel": {ch}, "role": {"300000000000000777"}},
		"the bot's own role":     {"channel": {ch}, "role": {discordRoleBot}},
		"not an id":              {"channel": {"../guilds"}, "role": {discordRoleMember}},
	} {
		save(nil, form)
		if got := edits(); got != "" || stored() != ch+"="+discordRoleMember {
			t.Errorf("%s: changes %q, recorded %q", name, got, stored())
		}
	}

	// Discord refuses: nothing is recorded, and the director is told.
	fake.mu.Lock()
	fake.noChannelEdit = true
	fake.mu.Unlock()
	body = save(nil, url.Values{"channel": {discordVoiceChannel}, "role": {discordRoleMember}})
	mustContain(t, "refused by Discord", body, "Discord would not let the bot change Voice: Voice.")
	if strings.Contains(stored(), discordVoiceChannel) {
		t.Fatal("a change Discord refused was recorded as made")
	}
	fake.mu.Lock()
	fake.noChannelEdit = false
	fake.mu.Unlock()
	edits()

	// An outsider cannot.
	other, _ := f.q.CreateUser(f.ctx)
	seedCharacter(t, f.q, other.ID, fixtureCharB, "Fixture Other")
	f.joinCorp(fixtureCharB, discordOtherCorp, "Director")
	f.post(sessionCookie(t, f.app, other.ID, fixtureCharB, "Fixture Other"), "/discord/servers/"+discordCorpGuild+"/channels",
		url.Values{"channel": {ch}, "role": {discordRoleLinked}})
	if got := edits(); got != "" || stored() != ch+"="+discordRoleMember {
		t.Fatalf("an outsider changed a channel: %q, recorded %q", got, stored())
	}

	// Nothing ticked: the bot stops managing the channel. Its role's
	// entry goes; everyone else stays shut out.
	body = save(nil, url.Values{"channel": {ch}})
	mustContain(t, "after unticking everything", body, "#ops is no longer managed.", "The bot is not keeping any channel private here.")
	if got := edits(); got != "clear "+ch+" "+discordRoleMember {
		t.Fatalf("changes made when unticking everything: %q; want only the role's entry removed", got)
	}
	if stored() != "" {
		t.Fatalf("still recorded: %q", stored())
	}
}
