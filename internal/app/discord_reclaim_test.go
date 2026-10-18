package app

import (
	"testing"

	db "evesynapse/internal/db/sqlc"
)

// TestDiscordServerReclaimClearsPreviousOwnersRules: a server the
// bot is added to again for a DIFFERENT owner starts clean — the
// recorded owner's role rules, channel access, auto-join and
// removals do not stay in force under the new owner. A re-add by
// the same owner changes nothing.
func TestDiscordServerReclaimClearsPreviousOwnersRules(t *testing.T) {
	f := newNotifyFixture(t)
	fake := f.enableDiscord()
	cookie := sessionCookie(t, f.app, f.userID, f.ch.CharacterID, f.ch.Name)

	f.joinCorp(f.ch.CharacterID, discordCorp, "Director")
	cookie, _ = f.addServer(cookie, discordCorpOwner, "add-corp")

	// The first owner configures the server: role settings and an
	// ops channel through the page; channel access, auto-join and
	// the removals switch seeded directly.
	f.setServer(cookie, discordCorpGuild, discordRoleLinked, discordRoleMember, discordOpsChannel)
	if err := f.q.InsertDiscordChannelAccess(f.ctx, db.InsertDiscordChannelAccessParams{
		GuildID: discordCorpGuild, ChannelID: discordVoiceChannel, RoleID: discordRoleMember,
		SetBy: f.userID, SetAt: notifyT0,
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.q.SetDiscordGuildAutoJoin(f.ctx, db.SetDiscordGuildAutoJoinParams{GuildID: discordCorpGuild, AutoJoin: true}); err != nil {
		t.Fatal(err)
	}
	if err := f.q.SetDiscordGuildKick(f.ctx, db.SetDiscordGuildKickParams{
		GuildID: discordCorpGuild, KickEnabled: true, KickExemptRoles: discordRoleMod,
	}); err != nil {
		t.Fatal(err)
	}

	configured := func() db.DiscordGuild {
		t.Helper()
		guild, err := f.q.GetDiscordGuild(f.ctx, discordCorpGuild)
		if err != nil {
			t.Fatal(err)
		}
		rules, err := f.q.ListDiscordRoleRulesForGuild(f.ctx, discordCorpGuild)
		if err != nil {
			t.Fatal(err)
		}
		access, err := f.q.ListDiscordChannelAccess(f.ctx, discordCorpGuild)
		if err != nil {
			t.Fatal(err)
		}
		if len(rules) == 0 || len(access) == 0 {
			t.Fatalf("seeded configuration missing: %d rules, %d channel access rows", len(rules), len(access))
		}
		return guild
	}
	if g := configured(); !g.AutoJoin || !g.KickEnabled || g.OpsChannel == "" {
		t.Fatalf("seeded server not configured: %+v", g)
	}

	// The same owner adds the bot again: nothing is cleared.
	f.addServer(cookie, discordCorpOwner, "add-corp")
	if g := configured(); !g.AutoJoin || !g.KickEnabled || g.OpsChannel == "" {
		t.Fatalf("same-owner re-add cleared the server: %+v", g)
	}

	// A director of another corporation claims the same server
	// for their corporation: the claim stands, the previous
	// owner's configuration does not.
	other, err := f.q.CreateUser(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	seedCharacter(t, f.q, other.ID, fixtureCharB, "Fixture Other")
	f.joinCorp(fixtureCharB, discordOtherCorp, "Director")
	theirs := sessionCookie(t, f.app, other.ID, fixtureCharB, "Fixture Other")
	fake.codes["reclaim"] = discordBob
	fake.installs["reclaim"] = discordCorpGuild
	f.addServer(theirs, "corporation:98000002", "reclaim")

	guild, err := f.q.GetDiscordGuild(f.ctx, discordCorpGuild)
	if err != nil {
		t.Fatal(err)
	}
	if guild.OwnerKind != ownerCorporation || guild.OwnerID != discordOtherCorp || guild.AddedBy != other.ID {
		t.Fatalf("claimed server %+v", guild)
	}
	if rules, err := f.q.ListDiscordRoleRulesForGuild(f.ctx, discordCorpGuild); err != nil || len(rules) != 0 {
		t.Fatalf("role rules after re-claim: %d, %v", len(rules), err)
	}
	if access, err := f.q.ListDiscordChannelAccess(f.ctx, discordCorpGuild); err != nil || len(access) != 0 {
		t.Fatalf("channel access after re-claim: %d, %v", len(access), err)
	}
	if guild.AutoJoin || guild.KickEnabled || guild.KickExemptRoles != "" {
		t.Fatalf("auto-join/removals survived the re-claim: %+v", guild)
	}
	if guild.OpsChannel != "" {
		t.Fatalf("ops channel survived the re-claim: %+v", guild)
	}
}
