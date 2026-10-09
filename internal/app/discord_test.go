package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/discord"
	"evesynapse/internal/esi"
)

// fakeDiscord stands in for Discord's API. It knows which account a
// sign-in code belongs to, which roles each member of the server
// holds, and records every message and role change the bot makes.
type fakeDiscord struct {
	mu          sync.Mutex
	codes       map[string]string   // sign-in code -> account id
	members     map[string][]string // account id -> roles held; absent: not in the server
	refuseDM    bool
	failMembers bool     // looking a member up fails outright
	messages    []string // "channel: text"
	changes     []string // "PUT user role" / "DELETE user role"
	calls       int
}

func (d *fakeDiscord) RoundTrip(req *http.Request) (*http.Response, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	respond := func(code int, body string) (*http.Response, error) {
		return &http.Response{StatusCode: code, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	}
	raw := ""
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		raw = string(b)
	}
	path := strings.TrimPrefix(req.URL.Path, "/api")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case path == "/oauth2/token":
		form, _ := url.ParseQuery(raw)
		if _, ok := d.codes[form.Get("code")]; !ok {
			return respond(400, `{"message":"invalid_grant"}`)
		}
		return respond(200, `{"access_token":"token-for-`+form.Get("code")+`"}`)
	case path == "/users/@me":
		code := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer token-for-")
		return respond(200, `{"id":"`+d.codes[code]+`","username":"pilot-`+code+`"}`)
	case path == "/users/@me/channels":
		if d.refuseDM {
			return respond(403, `{"message":"Cannot send messages to this user"}`)
		}
		id := raw[strings.Index(raw, `:"`)+2 : strings.LastIndex(raw, `"`)]
		return respond(200, `{"id":"9`+id[1:]+`"}`) // a conversation id made from the account's
	case len(parts) == 3 && parts[0] == "channels" && parts[2] == "messages":
		text := raw[strings.Index(raw, `"content":"`)+11:]
		text = text[:strings.Index(text, `"`)]
		d.messages = append(d.messages, parts[1]+": "+strings.ReplaceAll(text, `\n`, " | "))
		return respond(200, `{"id":"1"}`)
	case len(parts) == 4 && parts[0] == "guilds" && parts[2] == "members":
		if d.failMembers {
			return respond(500, `{"message":"Internal Server Error"}`)
		}
		roles, in := d.members[parts[3]]
		if !in {
			return respond(404, `{"message":"Unknown Member"}`)
		}
		return respond(200, `{"roles":["`+strings.Join(roles, `","`)+`"]}`)
	case len(parts) == 6 && parts[0] == "guilds" && parts[4] == "roles":
		user, role := parts[3], parts[5]
		d.changes = append(d.changes, req.Method+" "+user+" "+role)
		var kept []string
		for _, r := range d.members[user] {
			if r != role {
				kept = append(kept, r)
			}
		}
		if req.Method == http.MethodPut {
			kept = append(kept, role)
		}
		d.members[user] = kept
		return respond(204, ``)
	}
	return respond(404, `{"message":"unexpected `+path+`"}`)
}

const (
	discordGuild      = "200000000000000001"
	discordAlice      = "100000000000000001"
	discordBob        = "100000000000000002"
	discordRoleLinked = "300000000000000001"
	discordRoleCorp   = "300000000000000002"
	discordRoleOther  = "300000000000000003" // another corporation's
	discordRoleMod    = "300000000000000099" // not EveSynapse's to touch
	discordOpsChannel = "400000000000000001"
	discordCorp       = int64(98000001)
	discordOtherCorp  = int64(98000002)
)

// enableDiscord gives the fixture's app a Discord client over the
// stand-in, with the sign-in and the bot both set up.
func (f *notifyFixture) enableDiscord() *fakeDiscord {
	f.t.Helper()
	fake := &fakeDiscord{codes: map[string]string{"alice": discordAlice, "bob": discordBob}, members: map[string][]string{}}
	f.app.cfg.eveCallbackURL = "https://eve.example/auth/callback"
	f.app.cfg.discordRoleLinked = discordRoleLinked
	f.app.cfg.discordCorpRoles = map[int64]string{discordCorp: discordRoleCorp, discordOtherCorp: discordRoleOther}
	f.app.cfg.discordOpsChannels = map[int64]string{discordCorp: discordOpsChannel}
	f.app.discord = discord.New(discord.Config{
		ClientID: "client", ClientSecret: "secret", BotToken: "bot-token", GuildID: discordGuild,
		RedirectURL: "https://eve.example/discord/callback",
	}, &http.Client{Transport: fake}, "https://discord.test/api")
	return fake
}

// connect runs the whole "Connect Discord" round trip for a cookie,
// with the sign-in code Discord would hand back.
func (f *notifyFixture) connect(cookie *http.Cookie, code string) (*http.Cookie, string) {
	f.t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/discord/connect", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	f.app.Handler().ServeHTTP(rec, req)
	to, err := url.Parse(rec.Header().Get("Location"))
	if rec.Code != http.StatusFound || err != nil || to.Host != "discord.com" {
		f.t.Fatalf("connect: %d to %q", rec.Code, rec.Header().Get("Location"))
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == cookie.Name {
			cookie = c
		}
	}
	back := httptest.NewRequest(http.MethodGet, "/discord/callback?code="+code+"&state="+url.QueryEscape(to.Query().Get("state")), nil)
	back.AddCookie(cookie)
	rec = httptest.NewRecorder()
	f.app.Handler().ServeHTTP(rec, back)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != discordSettingsPath {
		f.t.Fatalf("callback: %d to %q", rec.Code, rec.Header().Get("Location"))
	}
	_, body := getPage(f.t, f.app, cookie, discordSettingsPath)
	return cookie, body
}

func (f *notifyFixture) joinCorp(characterID, corporationID int64) {
	f.t.Helper()
	if err := f.q.UpsertCharacterCorporation(f.ctx, db.UpsertCharacterCorporationParams{
		CharacterID: characterID, CorporationID: corporationID, UpdatedAt: notifyT0,
	}); err != nil {
		f.t.Fatal(err)
	}
}

// TestDiscordConnect: the sign-in stores the Discord account's id and
// name against the EveSynapse account and nothing else; a forged or
// replayed return is refused; a Discord account belongs to whoever
// connected it last; and it can be disconnected.
func TestDiscordConnect(t *testing.T) {
	f := newNotifyFixture(t)
	cookie := sessionCookie(t, f.app, f.userID, f.ch.CharacterID, f.ch.Name)

	// Not set up: nothing is offered.
	_, body := getPage(t, f.app, cookie, discordSettingsPath)
	if strings.Contains(body, "Connect Discord") {
		t.Fatal("Discord is offered on a server that has not set it up")
	}
	fake := f.enableDiscord()
	_, body = getPage(t, f.app, cookie, discordSettingsPath)
	mustContain(t, "settings before connecting", body, `<a class="btn" href="/discord/connect">Connect Discord</a>`, "Only your Discord name and id are read.")

	// A return nobody started, or with the wrong state, changes nothing.
	for _, path := range []string{"/discord/callback?code=alice&state=made-up", "/discord/callback?code=alice"} {
		if code, _ := getPage(t, f.app, cookie, path); code != http.StatusSeeOther {
			t.Fatalf("%s: %d", path, code)
		}
	}
	if _, err := f.q.GetDiscordLink(f.ctx, f.userID); err == nil {
		t.Fatal("a return with no matching state linked an account")
	}
	if fake.calls != 0 {
		t.Fatalf("Discord was asked %d time(s) for a return that was refused", fake.calls)
	}

	cookie, body = f.connect(cookie, "alice")
	mustContain(t, "settings after connecting", body, "Discord connected as pilot-alice.", "Connected as <strong>pilot-alice</strong>.", `action="/discord/disconnect"`)
	link, err := f.q.GetDiscordLink(f.ctx, f.userID)
	if err != nil || link.DiscordID != discordAlice || link.Username != "pilot-alice" || link.DmNotifications {
		t.Fatalf("stored link %+v, %v", link, err)
	}

	// Cancelled on Discord's side, or a code Discord refuses: no change.
	if _, page := f.connect(cookie, "nobody"); !strings.Contains(page, "could not confirm the account") {
		t.Fatal("a refused sign-in code was not reported")
	}
	if link, _ = f.q.GetDiscordLink(f.ctx, f.userID); link.DiscordID != discordAlice {
		t.Fatalf("a refused sign-in changed the link to %q", link.DiscordID)
	}

	// The same Discord account connected by another EveSynapse
	// account moves to it.
	other, err := f.q.CreateUser(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	seedCharacter(t, f.q, other.ID, fixtureCharB, "Fixture Alt")
	f.connect(sessionCookie(t, f.app, other.ID, fixtureCharB, "Fixture Alt"), "alice")
	if _, err := f.q.GetDiscordLink(f.ctx, f.userID); err == nil {
		t.Fatal("one Discord account is linked to two EveSynapse accounts")
	}
	if link, err = f.q.GetDiscordLink(f.ctx, other.ID); err != nil || link.DiscordID != discordAlice {
		t.Fatalf("the second account's link: %+v, %v", link, err)
	}

	// Disconnecting.
	cookie, _ = f.connect(cookie, "bob")
	if code, where := f.post(cookie, "/discord/disconnect", url.Values{}); code != http.StatusSeeOther || where != discordSettingsPath {
		t.Fatalf("disconnect: %d to %q", code, where)
	}
	if _, err := f.q.GetDiscordLink(f.ctx, f.userID); err == nil {
		t.Fatal("still linked after disconnecting")
	}
	// Signed out: nothing.
	if code, _, _ := doReq(t, f.app, http.MethodGet, "/discord/connect", nil); code != http.StatusSeeOther {
		t.Fatalf("signed-out connect: %d", code)
	}
}

// TestDiscordDirectMessages: with the box ticked, what the worker
// announces also goes to the account as a direct message, with a link
// back to the site; with it unticked, nothing is sent.
func TestDiscordDirectMessages(t *testing.T) {
	f := newNotifyFixture(t)
	fake := f.enableDiscord()
	cookie := sessionCookie(t, f.app, f.userID, f.ch.CharacterID, f.ch.Name)
	cookie, _ = f.connect(cookie, "alice")
	fake.members[discordAlice] = nil
	mail := func(id int64, subject string, at time.Time) {
		seedSnapshot(t, f.q, f.ch.CharacterID, esi.SnapMail, esi.MailHeaders{{MailID: id, From: 555, Subject: subject, Timestamp: rfc(at)}})
	}
	now := notifyT0
	seedSnapshot(t, f.q, f.ch.CharacterID, esi.SnapMail, esi.MailHeaders{})
	f.pass(now)

	// Connected, box not ticked: the notification is made, no message.
	mail(1, "Before ticking", now)
	if n := f.pass(now.Add(time.Minute)); n != 1 || len(fake.messages) != 0 {
		t.Fatalf("%d notification(s), messages %q; want one and none", n, fake.messages)
	}

	if code, _ := f.post(cookie, "/discord/settings", url.Values{"dm": {"1"}}); code != http.StatusSeeOther {
		t.Fatalf("save: %d", code)
	}
	_, body := getPage(t, f.app, cookie, discordSettingsPath)
	mustContain(t, "settings with messages on", body, `name="dm" value="1" checked>`)
	mail(2, "Fleet tonight", now)
	f.pass(now.Add(2 * time.Minute))
	if len(fake.messages) != 1 || !strings.Contains(fake.messages[0], "Fixture Ceo has new mail: Fleet tonight") ||
		!strings.Contains(fake.messages[0], "https://eve.example/mail/?character=90000001") {
		t.Fatalf("messages %q, want the new mail with its link", fake.messages)
	}

	// The test button, and what it says when Discord will not deliver.
	f.post(cookie, "/discord/test", url.Values{})
	if len(fake.messages) != 2 || !strings.Contains(fake.messages[1], "EveSynapse can reach you here") {
		t.Fatalf("after the test: %q", fake.messages)
	}
	fake.refuseDM = true
	f.post(cookie, "/discord/test", url.Values{})
	_, body = getPage(t, f.app, cookie, discordSettingsPath)
	mustContain(t, "refused test", body, "Discord would not deliver the message.")
	// A refusal loses nothing: the notification is still made.
	mail(3, "While unreachable", now)
	if n := f.pass(now.Add(3 * time.Minute)); n != 1 {
		t.Fatalf("with Discord refusing: %d notification(s), want 1", n)
	}

	// Unticked again.
	fake.refuseDM = false
	f.post(cookie, "/discord/settings", url.Values{})
	mail(4, "After unticking", now)
	f.pass(now.Add(4 * time.Minute))
	if len(fake.messages) != 2 {
		t.Fatalf("a message was sent after unticking: %q", fake.messages)
	}
}

// TestDiscordAnnouncesNewOps: an op is posted once in its
// corporation's channel; an op with no channel is not asked about
// again; old ops are not posted when the bot is first turned on.
func TestDiscordAnnouncesNewOps(t *testing.T) {
	f := newNotifyFixture(t)
	fake := f.enableDiscord()
	now := notifyT0
	plan := func(corp int64, title string, created time.Time) int64 {
		t.Helper()
		id, err := f.q.CreateOp(f.ctx, db.CreateOpParams{
			CorporationID: corp, Title: title, StartsAt: time.Date(2026, 10, 12, 19, 0, 0, 0, time.UTC), DurationMinutes: 60,
			Doctrine: "Ferox", FcCharacterID: f.ch.CharacterID, CreatedByCharacter: f.ch.CharacterID, CreatedAt: created,
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	op := plan(discordCorp, "Structure @everyone bash", now)
	plan(discordCorp, "Planned last week", now.Add(-7*24*time.Hour))
	plan(discordOtherCorp, "No channel for this corporation", now)

	if n := f.app.discordAnnounceOps(f.ctx, now.Add(time.Minute)); n != 1 {
		t.Fatalf("posted %d, want 1: %q", n, fake.messages)
	}
	if len(fake.messages) != 1 {
		t.Fatalf("messages %q", fake.messages)
	}
	mustContain(t, "the post", fake.messages[0],
		discordOpsChannel+": ", "Structure @everyone bash", "Monday, Oct 12 19:00 EVE time", "Ferox",
		"https://eve.example"+opURL(op))
	calls := fake.calls
	if n := f.app.discordAnnounceOps(f.ctx, now.Add(2*time.Minute)); n != 0 || fake.calls != calls {
		t.Fatalf("second look posted %d and made %d more call(s)", n, fake.calls-calls)
	}
	// Nothing set up: nothing happens.
	f.app.cfg.discordOpsChannels = nil
	plan(discordCorp, "With no channels set", now)
	if n := f.app.discordAnnounceOps(f.ctx, now.Add(3*time.Minute)); n != 0 || fake.calls != calls {
		t.Fatal("posted with no channel configured")
	}
}

// TestDiscordRoles: a connected account gets the role for being
// connected and the roles of its characters' corporations, loses a
// corporation's role when its character leaves, and has every role
// the bot did not give left alone. Discord is only asked when
// something has changed.
func TestDiscordRoles(t *testing.T) {
	f := newNotifyFixture(t)
	fake := f.enableDiscord()
	cookie := sessionCookie(t, f.app, f.userID, f.ch.CharacterID, f.ch.Name)
	cookie, _ = f.connect(cookie, "alice")
	f.joinCorp(f.ch.CharacterID, discordCorp)
	held := func() string {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		roles := append([]string(nil), fake.members[discordAlice]...)
		sortStrings(roles)
		return strings.Join(roles, ",")
	}
	now := notifyT0

	// Not in the server yet: nothing to give.
	if n := f.app.discordSyncRoles(f.ctx, now); n != 0 || len(fake.changes) != 0 {
		t.Fatalf("for a user outside the server: %d changed, %q", n, fake.changes)
	}

	// Joined, already holding a role a moderator gave.
	fake.members[discordAlice] = []string{discordRoleMod}
	if n := f.app.discordSyncRoles(f.ctx, now.Add(7*time.Hour)); n != 1 {
		t.Fatalf("%d account(s) changed, want 1: %q", n, fake.changes)
	}
	if got, want := held(), discordRoleLinked+","+discordRoleCorp+","+discordRoleMod; got != want {
		t.Fatalf("roles held %s, want %s", got, want)
	}

	// Nothing changed: Discord is not asked.
	calls := fake.calls
	if n := f.app.discordSyncRoles(f.ctx, now.Add(7*time.Hour+time.Minute)); n != 0 || fake.calls != calls {
		t.Fatalf("an unchanged account cost %d call(s)", fake.calls-calls)
	}

	// The character moves corporation: one role goes, another comes.
	f.joinCorp(f.ch.CharacterID, discordOtherCorp)
	f.app.discordSyncRoles(f.ctx, now.Add(7*time.Hour+2*time.Minute))
	if got, want := held(), discordRoleLinked+","+discordRoleOther+","+discordRoleMod; got != want {
		t.Fatalf("after moving corporation: %s, want %s", got, want)
	}

	// The character's link to EVE goes bad: it earns nothing any more,
	// and with no working character left the account does not even
	// keep the role for being connected.
	if _, err := f.app.db.ExecContext(f.ctx, `UPDATE characters SET link_state = 'revoked' WHERE character_id = $1`, f.ch.CharacterID); err != nil {
		t.Fatal(err)
	}
	f.app.discordSyncRoles(f.ctx, now.Add(7*time.Hour+3*time.Minute))
	if got := held(); got != discordRoleMod {
		t.Fatalf("with the only character's link revoked: %s, want only the moderator's role", got)
	}
	if _, err := f.app.db.ExecContext(f.ctx, `UPDATE characters SET link_state = 'ok' WHERE character_id = $1`, f.ch.CharacterID); err != nil {
		t.Fatal(err)
	}
	f.app.discordSyncRoles(f.ctx, now.Add(7*time.Hour+210*time.Second))
	if got, want := held(), discordRoleLinked+","+discordRoleOther+","+discordRoleMod; got != want {
		t.Fatalf("with the link working again: %s, want %s", got, want)
	}

	// A role taken away by hand comes back at the periodic check.
	fake.members[discordAlice] = []string{discordRoleMod}
	f.app.discordSyncRoles(f.ctx, now.Add(7*time.Hour+4*time.Minute))
	if got := held(); got != discordRoleMod {
		t.Fatalf("checked again too soon: %s", got)
	}
	f.app.discordSyncRoles(f.ctx, now.Add(14*time.Hour))
	if got, want := held(), discordRoleLinked+","+discordRoleOther+","+discordRoleMod; got != want {
		t.Fatalf("after the periodic check: %s, want %s", got, want)
	}

	// Disconnecting takes back what was given, and only that.
	f.post(cookie, "/discord/disconnect", url.Values{})
	if got := held(); got != discordRoleMod {
		t.Fatalf("after disconnecting: %s, want only the moderator's role", got)
	}
	for _, change := range fake.changes {
		if strings.HasSuffix(change, discordRoleMod) {
			t.Fatalf("the bot touched a role that is not its own: %s", change)
		}
	}
}

// TestDiscordRolesAreTakenBackWhenTheAccountGoes: what the bot gave is
// taken back when the EveSynapse account is deleted, however that was
// done, and when it connects a different Discord account. If Discord
// cannot be reached at that moment it is tried again, not forgotten.
func TestDiscordRolesAreTakenBackWhenTheAccountGoes(t *testing.T) {
	f := newNotifyFixture(t)
	fake := f.enableDiscord()
	cookie := sessionCookie(t, f.app, f.userID, f.ch.CharacterID, f.ch.Name)
	cookie, _ = f.connect(cookie, "alice")
	f.joinCorp(f.ch.CharacterID, discordCorp)
	held := func(id string) string {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		roles := append([]string(nil), fake.members[id]...)
		sortStrings(roles)
		return strings.Join(roles, ",")
	}
	now := notifyT0
	fake.members[discordAlice] = []string{discordRoleMod}
	fake.members[discordBob] = nil
	f.app.discordSyncRoles(f.ctx, now)
	if got, want := held(discordAlice), discordRoleLinked+","+discordRoleCorp+","+discordRoleMod; got != want {
		t.Fatalf("to begin with: %s, want %s", got, want)
	}

	// The same account connects a different Discord account: the
	// first one loses what it was given, the second gains it.
	cookie, _ = f.connect(cookie, "bob")
	f.app.discordSyncRoles(f.ctx, now.Add(time.Minute))
	if got := held(discordAlice); got != discordRoleMod {
		t.Fatalf("the Discord account that was swapped out still holds %s", got)
	}
	if got, want := held(discordBob), discordRoleLinked+","+discordRoleCorp; got != want {
		t.Fatalf("the Discord account swapped in holds %s, want %s", got, want)
	}

	// The EveSynapse account is deleted outright, behind the app's
	// back, while Discord is refusing the bot. Nothing is forgotten.
	if _, err := f.app.db.ExecContext(f.ctx, `DELETE FROM users WHERE id = $1`, f.userID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.q.GetDiscordLink(f.ctx, f.userID); err == nil {
		t.Fatal("the link outlived the account")
	}
	member := fake.members[discordBob]
	fake.mu.Lock()
	delete(fake.members, discordBob) // the member cannot be looked up: tried again later
	fake.failMembers = true
	fake.mu.Unlock()
	f.app.discordSyncRoles(f.ctx, now.Add(2*time.Minute))
	fake.mu.Lock()
	fake.members[discordBob], fake.failMembers = member, false
	fake.mu.Unlock()
	if got := held(discordBob); got == "" {
		t.Fatal("the test did not leave the roles in place while Discord was failing")
	}
	if n := f.app.discordSyncRoles(f.ctx, now.Add(3*time.Minute)); n != 1 {
		t.Fatalf("after the account was deleted: %d change(s), want the roles taken back", n)
	}
	if got := held(discordBob); got != "" {
		t.Fatalf("a deleted account's Discord member still holds %s", got)
	}
	// And then there is nothing left to do.
	calls := fake.calls
	if n := f.app.discordSyncRoles(f.ctx, now.Add(4*time.Minute)); n != 0 || fake.calls != calls {
		t.Fatalf("with nothing left to take back: %d change(s), %d call(s)", n, fake.calls-calls)
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func TestDiscordIDSettings(t *testing.T) {
	got := discordIDMap("TEST", " 98000001=300000000000000002 , *=300000000000000003, nope, 5=abc, =300000000000000004,98000009 = 300000000000000005")
	want := map[int64]string{98000001: "300000000000000002", 0: "300000000000000003", 98000009: "300000000000000005"}
	if len(got) != len(want) {
		t.Fatalf("read %v, want %v", got, want)
	}
	for corp, id := range want {
		if got[corp] != id {
			t.Errorf("corporation %d: %q, want %q", corp, got[corp], id)
		}
	}
	if discordIDValue("TEST", " 300000000000000001 ") != "300000000000000001" || discordIDValue("TEST", "@everyone") != "" || discordIDValue("TEST", "") != "" {
		t.Fatal("a single id setting was read wrongly")
	}
}
