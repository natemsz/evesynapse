package app

import (
	"testing"
	"time"

	discordmod "evesynapse/internal/app/discord"
	db "evesynapse/internal/db/sqlc"
)

// TestDiscordSyncRolesWithoutRules: a server with no role rules
// owes nobody anything, and the pass still settles a member who
// is due — one written down holding a role, checked long ago —
// by taking the role back; a member written down holding
// nothing and checked recently costs the pass no Discord calls
// at all.
func TestDiscordSyncRolesWithoutRules(t *testing.T) {
	f := newNotifyFixture(t)
	fake := f.enableDiscord()
	cookie := sessionCookie(t, f.app, f.userID, f.ch.CharacterID, f.ch.Name)
	f.joinCorp(f.ch.CharacterID, discordCorp, "Director")
	cookie, _ = f.addServer(cookie, discordCorpOwner, "add-corp")
	f.connect(cookie, "alice")
	now := notifyT0

	// No rules are ever added to this server. Alice is in it
	// holding a role, and what she was last given was written
	// down long past the periodic recheck.
	fake.join(discordCorpGuild, discordAlice, discordRoleMember)
	if err := f.q.UpsertDiscordRoleGrant(f.ctx, db.UpsertDiscordRoleGrantParams{
		DiscordID: discordAlice, GuildID: discordCorpGuild,
		Roles: discordRoleMember, Wanted: "", IsMember: true,
		CheckedAt: now.Add(-24 * time.Hour),
	}); err != nil {
		t.Fatalf("seed grant: %v", err)
	}

	if n := discordmod.SyncRoles(f.ctx, f.app, now); n != 1 {
		t.Fatalf("sync with no rules changed %d account(s), want 1", n)
	}
	if got := fake.held(discordCorpGuild, discordAlice); got != "" {
		t.Fatalf("after the pass she holds %q, want nothing", got)
	}

	// Written down holding nothing and freshly checked: the next
	// pass asks Discord nothing about her.
	calls := fake.asked()
	discordmod.SyncRoles(f.ctx, f.app, now.Add(time.Minute))
	if fake.asked() != calls {
		t.Fatalf("a settled member of a rule-less server cost %d call(s)", fake.asked()-calls)
	}
}
