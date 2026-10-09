package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/discord"
	"evesynapse/internal/esi"
)

// fakeDiscord stands in for Discord's API. It knows which account a
// sign-in code belongs to and which server a bot install was approved
// for, each server's roles, channels and members, and records every
// message and role change the bot makes.
type fakeDiscord struct {
	mu            sync.Mutex
	codes         map[string]string              // sign-in code -> account id
	installs      map[string]string              // sign-in code -> server the bot was added to
	members       map[string]map[string][]string // server -> account -> roles held; absent: not a member
	refuseDM      bool
	failMembers   bool // looking a member up fails outright
	noJoin        bool // the bot may not add members
	revoked       bool // accounts have taken their permission back
	refreshes     int
	added         []string             // "server user", for members the bot added
	bots          map[string]bool      // accounts that are bots
	joined        map[string]time.Time // "server user" -> when they joined; long ago if absent
	owner         string               // the account that owns every server
	noList        bool                 // the member list may not be read
	kicked        []string             // "server user", for members the bot removed
	channelEdits  []string             // "allow|deny|clear channel target", in the order made
	noChannelEdit bool                 // the bot may not change channels
	left          []string             // servers the bot was told to leave
	messages      []string             // "channel: text"
	changes       []string             // "PUT server user role" / "DELETE server user role"
	calls         int
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
		if form.Get("grant_type") == "refresh_token" {
			d.refreshes++
			code := strings.TrimPrefix(form.Get("refresh_token"), "refresh-for-")
			if _, ok := d.codes[code]; !ok || d.revoked {
				return respond(400, `{"message":"invalid_grant"}`)
			}
			return respond(200, `{"access_token":"token-for-`+code+`","refresh_token":"refresh-for-`+code+`","expires_in":604800}`)
		}
		code := form.Get("code")
		if _, ok := d.codes[code]; !ok {
			return respond(400, `{"message":"invalid_grant"}`)
		}
		guild := ""
		if id := d.installs[code]; id != "" {
			guild = `,"guild":{"id":"` + id + `","name":"Server ` + id[len(id)-1:] + `"}`
		}
		return respond(200, `{"access_token":"token-for-`+code+`","refresh_token":"refresh-for-`+code+`","expires_in":604800`+guild+`}`)
	case path == "/users/@me" && strings.HasPrefix(req.Header.Get("Authorization"), "Bot "):
		return respond(200, `{"id":"`+discordBotUser+`","username":"EveSynapse","bot":true}`)
	case len(parts) == 4 && parts[0] == "channels" && parts[2] == "permissions":
		if d.noChannelEdit {
			return respond(403, `{"message":"Missing Permissions"}`)
		}
		set := "clear"
		if req.Method == http.MethodPut {
			set = "deny"
			if strings.Contains(raw, `"deny":"0"`) {
				set = "allow"
			}
		}
		d.channelEdits = append(d.channelEdits, set+" "+parts[1]+" "+parts[3])
		return respond(204, ``)
	case path == "/users/@me":
		code := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer token-for-")
		return respond(200, `{"id":"`+d.codes[code]+`","username":"pilot-`+code+`"}`)
	case path == "/users/@me/channels":
		if d.refuseDM {
			return respond(403, `{"message":"Cannot send messages to this user"}`)
		}
		id := raw[strings.Index(raw, `:"`)+2 : strings.LastIndex(raw, `"`)]
		return respond(200, `{"id":"9`+id[1:]+`"}`) // a conversation id made from the account's
	case len(parts) == 4 && parts[0] == "users" && parts[2] == "guilds" && req.Method == http.MethodDelete:
		d.left = append(d.left, parts[3])
		return respond(204, ``)
	case len(parts) == 3 && parts[0] == "channels" && parts[2] == "messages":
		text := raw[strings.Index(raw, `"content":"`)+11:]
		text = text[:strings.Index(text, `"`)]
		d.messages = append(d.messages, parts[1]+": "+strings.ReplaceAll(text, `\n`, " | "))
		return respond(200, `{"id":"1"}`)
	case len(parts) == 3 && parts[0] == "guilds" && parts[2] == "roles":
		if _, known := d.members[parts[1]]; !known {
			return respond(403, `{"message":"Missing Access"}`)
		}
		return respond(200, `[{"id":"`+parts[1]+`","name":"@everyone","position":0},`+
			`{"id":"`+discordRoleLinked+`","name":"Linked","position":1},{"id":"`+discordRoleMember+`","name":"Member","position":2},`+
			`{"id":"`+groupRoleCEO+`","name":"CEO","position":1},{"id":"`+groupRoleDirector+`","name":"Director","position":1},{"id":"`+groupRoleLogi+`","name":"Logi","position":1},`+
			`{"id":"`+discordRoleMod+`","name":"Moderator","position":3},{"id":"`+discordRoleBot+`","name":"EveSynapse","managed":true,"position":4}]`)
	case len(parts) == 3 && parts[0] == "guilds" && parts[2] == "channels":
		if _, known := d.members[parts[1]]; !known {
			return respond(403, `{"message":"Missing Access"}`)
		}
		return respond(200, `[{"id":"`+discordOpsChannel+`","name":"ops","type":0,"position":1},{"id":"`+discordVoiceChannel+`","name":"Voice","type":2,"position":2}]`)
	case len(parts) == 2 && parts[0] == "guilds":
		return respond(200, `{"id":"`+parts[1]+`","owner_id":"`+d.owner+`"}`)
	case len(parts) == 3 && parts[0] == "guilds" && parts[2] == "members":
		if d.noList {
			return respond(403, `{"message":"Missing Access"}`)
		}
		var ids []string
		for id := range d.members[parts[1]] {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		var out []string
		for _, id := range ids {
			joined, known := d.joined[parts[1]+" "+id]
			if !known {
				joined = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
			}
			bot := ""
			if d.bots[id] {
				bot = `,"bot":true`
			}
			out = append(out, `{"user":{"id":"`+id+`","username":"name-`+id[len(id)-2:]+`"`+bot+`},"roles":["`+strings.Join(d.members[parts[1]][id], `","`)+`"],"joined_at":"`+joined.Format(time.RFC3339)+`"}`)
		}
		return respond(200, `[`+strings.Join(out, ",")+`]`)
	case len(parts) == 4 && parts[0] == "guilds" && parts[2] == "members" && req.Method == http.MethodDelete:
		if _, in := d.members[parts[1]][parts[3]]; !in {
			return respond(404, `{"message":"Unknown Member"}`)
		}
		delete(d.members[parts[1]], parts[3])
		d.kicked = append(d.kicked, parts[1]+" "+parts[3])
		return respond(204, ``)
	case len(parts) == 4 && parts[0] == "guilds" && parts[2] == "members" && req.Method == http.MethodPut:
		// Adding a member: only with that account's own token.
		guild, user := parts[1], parts[3]
		token := raw[strings.Index(raw, `"access_token":"`)+16:]
		token = token[:strings.Index(token, `"`)]
		if d.codes[strings.TrimPrefix(token, "token-for-")] != user {
			return respond(403, `{"message":"Invalid OAuth2 access token"}`)
		}
		if d.noJoin {
			return respond(403, `{"message":"Missing Permissions"}`)
		}
		if _, in := d.members[guild][user]; in {
			return respond(204, ``)
		}
		var roles []string
		if list := raw[strings.Index(raw, `"roles":[`)+9:]; !strings.HasPrefix(list, "]") {
			roles = strings.Split(strings.ReplaceAll(list[:strings.Index(list, "]")], `"`, ""), ",")
		}
		d.members[guild][user] = roles
		d.added = append(d.added, guild+" "+user)
		return respond(201, `{}`)
	case len(parts) == 4 && parts[0] == "guilds" && parts[2] == "members":
		if d.failMembers {
			return respond(500, `{"message":"Internal Server Error"}`)
		}
		roles, in := d.members[parts[1]][parts[3]]
		if !in {
			return respond(404, `{"message":"Unknown Member"}`)
		}
		return respond(200, `{"roles":["`+strings.Join(roles, `","`)+`"]}`)
	case len(parts) == 6 && parts[0] == "guilds" && parts[4] == "roles":
		guild, user, role := parts[1], parts[3], parts[5]
		d.changes = append(d.changes, req.Method+" "+guild+" "+user+" "+role)
		var kept []string
		for _, r := range d.members[guild][user] {
			if r != role {
				kept = append(kept, r)
			}
		}
		if req.Method == http.MethodPut {
			kept = append(kept, role)
		}
		d.members[guild][user] = kept
		return respond(204, ``)
	}
	return respond(404, `{"message":"unexpected `+path+`"}`)
}

// held lists the roles an account holds in a server, sorted.
func (d *fakeDiscord) held(guild, user string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	roles := append([]string(nil), d.members[guild][user]...)
	sort.Strings(roles)
	return strings.Join(roles, ",")
}

func (d *fakeDiscord) join(guild, user string, roles ...string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.members[guild][user] = roles
}

func (d *fakeDiscord) asked() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

const (
	discordCorpGuild     = "200000000000000001" // the corporation's server
	discordAllianceGuild = "200000000000000002" // the alliance's server
	discordAlice         = "100000000000000001"
	discordBob           = "100000000000000002"
	discordRoleLinked    = "300000000000000001"
	discordRoleMember    = "300000000000000002"
	discordRoleMod       = "300000000000000098" // a server's own: not EveSynapse's to touch
	discordRoleBot       = "300000000000000099" // the bot's own: cannot be given
	discordOpsChannel    = "400000000000000001"
	discordVoiceChannel  = "400000000000000009"
	discordBotUser       = "100000000000000999"
	discordCorp          = int64(98000001)
	discordOtherCorp     = int64(98000002)
	discordAlliance      = int64(99000001)
	discordCorpOwner     = "corporation:98000001"
	discordAllianceOwner = "alliance:99000001"
)

// enableDiscord gives the fixture's app a Discord client over the
// stand-in, with the sign-in and the bot both set up, and two servers
// the bot can be added to.
func (f *notifyFixture) enableDiscord() *fakeDiscord {
	f.t.Helper()
	fake := &fakeDiscord{
		codes:    map[string]string{"alice": discordAlice, "bob": discordBob, "add-corp": discordAlice, "add-alliance": discordAlice},
		installs: map[string]string{"add-corp": discordCorpGuild, "add-alliance": discordAllianceGuild},
		members:  map[string]map[string][]string{discordCorpGuild: {}, discordAllianceGuild: {}},
		bots:     map[string]bool{},
		joined:   map[string]time.Time{},
		owner:    "100000000000000900",
	}
	f.app.cfg.eveCallbackURL = "https://eve.example/auth/callback"
	f.app.discord = discord.New(discord.Config{
		ClientID: "client", ClientSecret: "secret", BotToken: "bot-token",
		RedirectURL: "https://eve.example/discord/callback",
	}, &http.Client{Transport: fake}, "https://discord.test/api")
	return fake
}

// roundTrip starts a Discord sign-in at start (a GET, or a POST when
// form is given), follows it to Discord and back with the code Discord
// would hand over, and returns the cookie and the page landed on.
func (f *notifyFixture) roundTrip(cookie *http.Cookie, start string, form url.Values, code, landing string) (*http.Cookie, string) {
	f.t.Helper()
	var req *http.Request
	if form == nil {
		req = httptest.NewRequest(http.MethodGet, start, nil)
	} else {
		req = httptest.NewRequest(http.MethodPost, start, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	f.app.Handler().ServeHTTP(rec, req)
	to, err := url.Parse(rec.Header().Get("Location"))
	if rec.Code != http.StatusFound || err != nil || to.Host != "discord.com" {
		f.t.Fatalf("%s: %d to %q, want to be sent to Discord", start, rec.Code, rec.Header().Get("Location"))
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
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != landing {
		f.t.Fatalf("callback: %d to %q, want %q", rec.Code, rec.Header().Get("Location"), landing)
	}
	_, body := getPage(f.t, f.app, cookie, landing)
	return cookie, body
}

// connect links the cookie's account to the Discord account behind a
// sign-in code.
func (f *notifyFixture) connect(cookie *http.Cookie, code string) (*http.Cookie, string) {
	f.t.Helper()
	return f.roundTrip(cookie, "/discord/connect", nil, code, discordSettingsPath)
}

// addServer adds the bot to the server behind an install code, for an
// owner ("corporation:98000001").
func (f *notifyFixture) addServer(cookie *http.Cookie, owner, code string) (*http.Cookie, string) {
	f.t.Helper()
	return f.roundTrip(cookie, "/discord/servers/add", url.Values{"owner": {owner}}, code, discordServersPath)
}

func (f *notifyFixture) joinCorp(characterID, corporationID int64, roles ...string) {
	f.t.Helper()
	if err := f.q.UpsertCharacterCorporation(f.ctx, db.UpsertCharacterCorporationParams{
		CharacterID: characterID, CorporationID: corporationID, UpdatedAt: notifyT0,
	}); err != nil {
		f.t.Fatal(err)
	}
	seedSnapshot(f.t, f.q, characterID, esi.SnapCorpRoles, esi.CharacterRoles{Roles: roles})
}

// inAlliance stores what EveSynapse knows of a corporation: that it is
// in the alliance, and which corporation runs the alliance.
func (f *notifyFixture) inAlliance(corporationID, allianceID, executor int64) {
	f.t.Helper()
	payload, _ := json.Marshal(corporationRecordPayload{
		Corp:     esi.Corporation{Name: "Corp", AllianceID: allianceID},
		Alliance: esi.Alliance{Name: "The Alliance", ExecutorCorporationID: executor},
	})
	if err := f.q.SetCorporationRecord(f.ctx, db.SetCorporationRecordParams{
		CorporationID: corporationID, Payload: string(payload), State: orgStateReady, FetchedAt: timeSet(notifyT0),
	}); err != nil {
		f.t.Fatal(err)
	}
}

// addRule adds one role rule to a server through the page.
func (f *notifyFixture) addRule(cookie *http.Cookie, guild, who, role string) string {
	f.t.Helper()
	f.post(cookie, "/discord/servers/"+guild+"/rules/add", url.Values{"who": {who}, "role": {role}})
	_, body := getPage(f.t, f.app, cookie, discordServersPath)
	return body
}

// setServer makes a server's rules exactly a role for everyone
// connected and a role for members (either may be empty), and sets its
// ops channel, through the page. The form is sent with "add members
// automatically" unticked, so these tests decide for themselves who is
// in a server; discord_join_test.go covers the adding.
func (f *notifyFixture) setServer(cookie *http.Cookie, guild, linked, member, channel string) string {
	f.t.Helper()
	if err := f.q.DeleteDiscordRoleRulesForGuild(f.ctx, guild); err != nil {
		f.t.Fatal(err)
	}
	if linked != "" {
		f.addRule(cookie, guild, ruleLinked, linked)
	}
	if member != "" {
		f.addRule(cookie, guild, ruleMember, member)
	}
	f.post(cookie, "/discord/servers/"+guild+"/save", url.Values{"ops_channel": {channel}})
	_, body := getPage(f.t, f.app, cookie, discordServersPath)
	return body
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
		t.Fatal("Discord is offered on a site that has not set it up")
	}
	fake := f.enableDiscord()
	_, body = getPage(t, f.app, cookie, discordSettingsPath)
	mustContain(t, "settings before connecting", body, `<a class="btn btn-discord" href="/discord/connect"><svg class="discord-glyph" viewBox="0 0 24 24" aria-hidden="true">`, ` Connect Discord</a>`, "it can do nothing else with your account")

	// A return nobody started, or with the wrong state, changes nothing.
	for _, path := range []string{"/discord/callback?code=alice&state=made-up", "/discord/callback?code=alice"} {
		if code, _ := getPage(t, f.app, cookie, path); code != http.StatusSeeOther {
			t.Fatalf("%s: %d", path, code)
		}
	}
	if _, err := f.q.GetDiscordLink(f.ctx, f.userID); err == nil {
		t.Fatal("a return with no matching state linked an account")
	}
	if fake.asked() != 0 {
		t.Fatalf("Discord was asked %d time(s) for a return that was refused", fake.asked())
	}

	cookie, body = f.connect(cookie, "alice")
	mustContain(t, "settings after connecting", body, "Discord connected as pilot-alice.", "Connected as <strong>pilot-alice</strong>.", `action="/discord/disconnect"`)
	link, err := f.q.GetDiscordLink(f.ctx, f.userID)
	if err != nil || link.DiscordID != discordAlice || link.Username != "pilot-alice" || link.DmNotifications {
		t.Fatalf("stored link %+v, %v", link, err)
	}

	// A code Discord refuses: no change.
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

	// The switch is saved by the page's own Save button, with the rest.
	settings := func(dm bool) url.Values {
		form := url.Values{"discord_shown": {"1"}}
		for _, kind := range notifyKinds {
			form.Add("kind", kind.ID)
			if !kind.Account {
				form.Add("char."+kind.ID, "90000001")
			}
		}
		if dm {
			form.Set("discord_dm", "1")
		}
		return form
	}
	_, body := getPage(t, f.app, cookie, discordSettingsPath)
	mustContain(t, "settings before ticking", body, `<input type="checkbox" name="discord_dm" value="1"> Also send these to me on Discord`)
	if code, _ := f.post(cookie, "/notifications/settings", settings(true)); code != http.StatusSeeOther {
		t.Fatalf("save: %d", code)
	}
	_, body = getPage(t, f.app, cookie, discordSettingsPath)
	mustContain(t, "settings with messages on", body, `name="discord_dm" value="1" checked>`, "Notifications by direct message are <strong>on</strong>")
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
	// A refusal loses nothing: the notification is still made. And
	// the person is told on the settings page, not only the log.
	mail(3, "While unreachable", now)
	if n := f.pass(now.Add(3 * time.Minute)); n != 1 {
		t.Fatalf("with Discord refusing: %d notification(s), want 1", n)
	}
	_, body = getPage(t, f.app, cookie, discordSettingsPath)
	mustContain(t, "after a refused notification", body, `<p class="err">Discord refused to deliver your last notification.`)
	// The next one that goes through clears it.
	fake.refuseDM = false
	mail(5, "Reachable again", now)
	f.pass(now.Add(210 * time.Second))
	if _, body = getPage(t, f.app, cookie, discordSettingsPath); strings.Contains(body, "Discord refused to deliver") {
		t.Fatal("the refusal is still shown after a message went through")
	}
	sentSoFar := len(fake.messages)

	// Unticked again.
	f.post(cookie, "/notifications/settings", settings(false))
	mail(4, "After unticking", now)
	f.pass(now.Add(4 * time.Minute))
	if len(fake.messages) != sentSoFar {
		t.Fatalf("a message was sent after unticking: %q", fake.messages)
	}
}

// TestDiscordServersAreSetUpByDirectors: only a director can add the
// bot for a corporation, and only a director of the executor
// corporation for an alliance; which server it was added to is what
// Discord says; settings are saved only as roles and channels of that
// server; and nobody else can change them.
func TestDiscordServersAreSetUpByDirectors(t *testing.T) {
	f := newNotifyFixture(t)
	fake := f.enableDiscord()
	cookie := sessionCookie(t, f.app, f.userID, f.ch.CharacterID, f.ch.Name)

	// A member who is not a director is offered nothing and cannot
	// start the install.
	f.joinCorp(f.ch.CharacterID, discordCorp)
	_, body := getPage(t, f.app, cookie, discordServersPath)
	mustContain(t, "servers page for a member", body, "None of your characters is a director")
	if code, _ := f.post(cookie, "/discord/servers/add", url.Values{"owner": {discordCorpOwner}}); code != http.StatusSeeOther {
		t.Fatalf("a member's add: %d, want to be sent back", code)
	}
	if fake.asked() != 0 {
		t.Fatal("Discord was asked on behalf of someone who is not a director")
	}

	// A director adds the bot. The server recorded is the one Discord
	// names in its own answer.
	f.joinCorp(f.ch.CharacterID, discordCorp, "Director")
	cookie, body = f.addServer(cookie, discordCorpOwner, "add-corp")
	mustContain(t, "after adding", body, "The bot was added to Server 1.", `action="/discord/servers/`+discordCorpGuild+`/rules/add"`,
		`<option value="`+discordRoleMember+`">Member</option>`, `<option value="`+discordOpsChannel+`">#ops</option>`,
		`<option value="member">Members of the corporation</option>`, `<option value="ceo">CEO</option>`,
		`<option value="eve_role:Director">In-game role: Director</option>`)
	if strings.Contains(body, discordRoleBot) {
		t.Fatal("the bot's own role, which cannot be given, is offered")
	}
	if strings.Contains(body, `<option value="`+discordVoiceChannel+`">#`) {
		t.Fatal("a voice channel is offered for posting ops")
	}
	guild, err := f.q.GetDiscordGuild(f.ctx, discordCorpGuild)
	if err != nil || guild.OwnerKind != ownerCorporation || guild.OwnerID != discordCorp || guild.AddedBy != f.userID {
		t.Fatalf("stored server %+v, %v", guild, err)
	}
	// An ordinary "Connect Discord" return adds no server.
	cookie, _ = f.connect(cookie, "alice")
	if guilds, _ := f.q.ListDiscordGuilds(f.ctx); len(guilds) != 1 {
		t.Fatalf("%d server(s) after a plain connect, want 1", len(guilds))
	}

	// Rules: a role of that server that can be given is taken;
	// anything else is refused and nothing is stored. The same for
	// the ops channel.
	rules := func() string {
		t.Helper()
		rows, err := f.q.ListDiscordRoleRulesForGuild(f.ctx, discordCorpGuild)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, row := range rows {
			out = append(out, row.Kind+":"+row.Ref+"="+row.RoleID)
		}
		return strings.Join(out, " ")
	}
	body = f.setServer(cookie, discordCorpGuild, discordRoleLinked, discordRoleMember, discordOpsChannel)
	mustContain(t, "after saving", body, "Settings saved for Server 1.",
		"<td>Everyone who has connected EveSynapse</td><td>Linked</td>", "<td>Members of the corporation</td><td>Member</td>")
	want := "linked:=" + discordRoleLinked + " member:=" + discordRoleMember
	if rules() != want {
		t.Fatalf("rules %q, want %q", rules(), want)
	}
	for name, bad := range map[string][2]string{
		"a role of no server":         {ruleLinked, "300000000000000777"},
		"the bot's own role":          {ruleMember, discordRoleBot},
		"@everyone":                   {ruleLinked, discordCorpGuild},
		"not an id":                   {ruleLinked, "@everyone"},
		"a kind that does not exist":  {"admin", discordRoleMember},
		"an in-game role made up":     {"eve_role:Emperor", discordRoleMember},
		"another owner's corporation": {"corp:98000002", discordRoleMember},
		"a group that does not exist": {"group:999", discordRoleMember},
	} {
		f.addRule(cookie, discordCorpGuild, bad[0], bad[1])
		if rules() != want {
			t.Errorf("%s was accepted: rules %q", name, rules())
			_ = f.q.DeleteDiscordRoleRulesForGuild(f.ctx, discordCorpGuild)
			f.setServer(cookie, discordCorpGuild, discordRoleLinked, discordRoleMember, discordOpsChannel)
		}
	}
	for name, bad := range map[string]string{"a channel of no server": "400000000000000777", "a voice channel": discordVoiceChannel} {
		f.post(cookie, "/discord/servers/"+discordCorpGuild+"/save", url.Values{"ops_channel": {bad}})
		if guild, _ = f.q.GetDiscordGuild(f.ctx, discordCorpGuild); guild.OpsChannel != discordOpsChannel {
			t.Errorf("%s was accepted as the ops channel", name)
		}
	}
	// The same rule twice is one rule.
	f.addRule(cookie, discordCorpGuild, ruleMember, discordRoleMember)
	if rules() != want {
		t.Fatalf("a repeated rule was stored twice: %q", rules())
	}

	// Somebody else, a director of another corporation, cannot touch it.
	other, err := f.q.CreateUser(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	seedCharacter(t, f.q, other.ID, fixtureCharB, "Fixture Other")
	f.joinCorp(fixtureCharB, discordOtherCorp, "Director")
	theirs := sessionCookie(t, f.app, other.ID, fixtureCharB, "Fixture Other")
	_, body = getPage(t, f.app, theirs, discordServersPath)
	if strings.Contains(body, discordCorpGuild) {
		t.Fatal("another corporation's server is shown to an outsider")
	}
	f.post(theirs, "/discord/servers/"+discordCorpGuild+"/save", url.Values{"ops_channel": {""}})
	f.post(theirs, "/discord/servers/"+discordCorpGuild+"/rules/add", url.Values{"who": {ruleLinked}, "role": {discordRoleMod}})
	f.post(theirs, "/discord/servers/"+discordCorpGuild+"/rules/1/remove", url.Values{})
	f.post(theirs, "/discord/servers/"+discordCorpGuild+"/remove", url.Values{})
	if guild, err = f.q.GetDiscordGuild(f.ctx, discordCorpGuild); err != nil || guild.OpsChannel != discordOpsChannel || rules() != want {
		t.Fatalf("an outsider changed the server: %+v, rules %s, %v", guild, rules(), err)
	}
	if code, _ := f.post(theirs, "/discord/servers/add", url.Values{"owner": {discordCorpOwner}}); code != http.StatusSeeOther {
		t.Fatalf("an outsider's add for the corporation: %d", code)
	}

	// An alliance's server: only a director of the executor corporation.
	f.inAlliance(discordCorp, discordAlliance, discordOtherCorp) // the other corporation runs the alliance
	f.inAlliance(discordOtherCorp, discordAlliance, discordOtherCorp)
	if code, _ := f.post(cookie, "/discord/servers/add", url.Values{"owner": {discordAllianceOwner}}); code != http.StatusSeeOther {
		t.Fatalf("a member corporation's director adding for the alliance: %d, want to be sent back", code)
	}
	if guilds, _ := f.q.ListDiscordGuilds(f.ctx); len(guilds) != 1 {
		t.Fatal("a director of a member corporation added an alliance server")
	}
	_, body = f.addServer(theirs, discordAllianceOwner, "add-alliance")
	mustContain(t, "the alliance's server", body, "The bot was added to Server 2.", `<option value="member">Members of the alliance</option>`,
		`<option value="ceo">CEOs of its corporations</option>`, `<option value="corp:98000001">Members of `)

	// A director loses the role: the actions go with it.
	f.joinCorp(f.ch.CharacterID, discordCorp)
	f.post(cookie, "/discord/servers/"+discordCorpGuild+"/save", url.Values{"ops_channel": {""}})
	f.post(cookie, "/discord/servers/"+discordCorpGuild+"/rules/add", url.Values{"who": {ruleLinked}, "role": {discordRoleMod}})
	if guild, _ = f.q.GetDiscordGuild(f.ctx, discordCorpGuild); guild.OpsChannel != discordOpsChannel || rules() != want {
		t.Fatal("a former director changed the server")
	}
}

// TestDiscordAnnouncesNewOps: an op is posted once in the channel of
// its corporation's server; in its alliance's server only when the
// corporation's director has agreed; and old ops are not posted to a
// server that has just been set up.
func TestDiscordAnnouncesNewOps(t *testing.T) {
	f := newNotifyFixture(t)
	fake := f.enableDiscord()
	cookie := sessionCookie(t, f.app, f.userID, f.ch.CharacterID, f.ch.Name)
	f.joinCorp(f.ch.CharacterID, discordCorp, "Director")
	f.inAlliance(discordCorp, discordAlliance, discordCorp) // this corporation runs the alliance
	cookie, _ = f.addServer(cookie, discordCorpOwner, "add-corp")
	cookie, _ = f.addServer(cookie, discordAllianceOwner, "add-alliance")
	f.setServer(cookie, discordCorpGuild, "", "", discordOpsChannel)
	f.setServer(cookie, discordAllianceGuild, "", "", discordOpsChannel)
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
	plan(discordOtherCorp, "A corporation with no server", now)

	// Not shared with the alliance: the corporation's own server only.
	if n := f.app.discordAnnounceOps(f.ctx, now.Add(time.Minute)); n != 1 || len(fake.messages) != 1 {
		t.Fatalf("posted %d, messages %q; want the one op in the corporation's server", n, fake.messages)
	}
	mustContain(t, "the post", fake.messages[0],
		discordOpsChannel+": ", "Structure @everyone bash", "Monday, Oct 12 19:00 EVE time", "Ferox",
		"https://eve.example"+opURL(op))
	calls := fake.asked()
	if n := f.app.discordAnnounceOps(f.ctx, now.Add(2*time.Minute)); n != 0 || fake.asked() != calls {
		t.Fatalf("second look posted %d and made %d more call(s)", n, fake.asked()-calls)
	}

	// The director agrees to share: the next op goes to both.
	_, body := getPage(t, f.app, cookie, discordServersPath)
	mustContain(t, "share offered", body, `name="corporation" value="98000001"`)
	f.post(cookie, "/discord/servers/share", url.Values{"corporation": {"98000001"}, "share": {"1"}})
	plan(discordCorp, "Shared op", now)
	if n := f.app.discordAnnounceOps(f.ctx, now.Add(3*time.Minute)); n != 2 {
		t.Fatalf("a shared op was posted %d time(s), want in both servers", n)
	}
	// Only that corporation's director decides it.
	other, _ := f.q.CreateUser(f.ctx)
	seedCharacter(t, f.q, other.ID, fixtureCharB, "Fixture Other")
	f.joinCorp(fixtureCharB, discordOtherCorp, "Director")
	f.post(sessionCookie(t, f.app, other.ID, fixtureCharB, "Fixture Other"), "/discord/servers/share", url.Values{"corporation": {"98000001"}})
	if _, err := f.q.GetDiscordOpsShare(f.ctx, discordCorp); err != nil {
		t.Fatal("an outsider switched off another corporation's sharing")
	}
}

// TestDiscordRoles: in a server, a connected account gets the linked
// role, and the member role when a working character of theirs is in
// the server's corporation or alliance. Roles follow the character,
// roles the bot did not give are never touched, and Discord is only
// asked when something has changed.
func TestDiscordRoles(t *testing.T) {
	f := newNotifyFixture(t)
	fake := f.enableDiscord()
	cookie := sessionCookie(t, f.app, f.userID, f.ch.CharacterID, f.ch.Name)
	f.joinCorp(f.ch.CharacterID, discordCorp, "Director")
	f.inAlliance(discordCorp, discordAlliance, discordCorp)
	cookie, _ = f.addServer(cookie, discordCorpOwner, "add-corp")
	cookie, _ = f.addServer(cookie, discordAllianceOwner, "add-alliance")
	f.setServer(cookie, discordCorpGuild, discordRoleLinked, discordRoleMember, "")
	f.setServer(cookie, discordAllianceGuild, "", discordRoleMember, "")
	cookie, _ = f.connect(cookie, "alice")
	now := notifyT0

	// In neither server yet: nothing to give.
	if n := f.app.discordSyncRoles(f.ctx, now); n != 0 || len(fake.changes) != 0 {
		t.Fatalf("for a user in no server: %d changed, %q", n, fake.changes)
	}
	// Looked for, not found, and not looked for again every minute.
	calls := fake.asked()
	f.app.discordSyncRoles(f.ctx, now.Add(time.Minute))
	if fake.asked() != calls {
		t.Fatalf("someone who is not in the servers cost %d call(s) a pass", fake.asked()-calls)
	}

	// Joins both, already holding a role a moderator gave. The button
	// brings the roles at once.
	fake.join(discordCorpGuild, discordAlice, discordRoleMod)
	fake.join(discordAllianceGuild, discordAlice)
	f.post(cookie, "/discord/roles/refresh", url.Values{})
	if got, want := fake.held(discordCorpGuild, discordAlice), discordRoleLinked+","+discordRoleMember+","+discordRoleMod; got != want {
		t.Fatalf("in the corporation's server: %s, want %s", got, want)
	}
	if got := fake.held(discordAllianceGuild, discordAlice); got != discordRoleMember {
		t.Fatalf("in the alliance's server: %s, want the member role only (no linked role is set there)", got)
	}

	// Nothing changed: Discord is not asked.
	calls = fake.asked()
	if n := f.app.discordSyncRoles(f.ctx, now.Add(2*time.Minute)); n != 0 || fake.asked() != calls {
		t.Fatalf("an unchanged account cost %d call(s)", fake.asked()-calls)
	}

	// The character moves to a corporation outside the alliance: the
	// member roles go, the linked role stays.
	f.joinCorp(f.ch.CharacterID, discordOtherCorp)
	f.app.discordSyncRoles(f.ctx, now.Add(3*time.Minute))
	if got, want := fake.held(discordCorpGuild, discordAlice), discordRoleLinked+","+discordRoleMod; got != want {
		t.Fatalf("after leaving, in the corporation's server: %s, want %s", got, want)
	}
	if got := fake.held(discordAllianceGuild, discordAlice); got != "" {
		t.Fatalf("after leaving, in the alliance's server: %s, want nothing", got)
	}

	// Back in, then the character's link to EVE goes bad: with no
	// working character the account earns nothing, not even linked.
	f.joinCorp(f.ch.CharacterID, discordCorp)
	f.app.discordSyncRoles(f.ctx, now.Add(4*time.Minute))
	if _, err := f.app.db.ExecContext(f.ctx, `UPDATE characters SET link_state = 'revoked' WHERE character_id = $1`, f.ch.CharacterID); err != nil {
		t.Fatal(err)
	}
	f.app.discordSyncRoles(f.ctx, now.Add(5*time.Minute))
	if got := fake.held(discordCorpGuild, discordAlice); got != discordRoleMod {
		t.Fatalf("with the only character's link revoked: %s, want only the moderator's role", got)
	}
	if _, err := f.app.db.ExecContext(f.ctx, `UPDATE characters SET link_state = 'ok' WHERE character_id = $1`, f.ch.CharacterID); err != nil {
		t.Fatal(err)
	}
	f.app.discordSyncRoles(f.ctx, now.Add(6*time.Minute))

	// A role taken away by hand comes back at the periodic check.
	fake.join(discordCorpGuild, discordAlice, discordRoleMod)
	f.app.discordSyncRoles(f.ctx, now.Add(7*time.Minute))
	if got := fake.held(discordCorpGuild, discordAlice); got != discordRoleMod {
		t.Fatalf("checked again too soon: %s", got)
	}
	f.app.discordSyncRoles(f.ctx, now.Add(7*time.Hour))
	if got, want := fake.held(discordCorpGuild, discordAlice), discordRoleLinked+","+discordRoleMember+","+discordRoleMod; got != want {
		t.Fatalf("after the periodic check: %s, want %s", got, want)
	}

	// The directors change which role is the member role: the old one
	// is taken back, although it is no longer in the settings.
	f.joinCorp(f.ch.CharacterID, discordCorp, "Director")
	f.setServer(cookie, discordCorpGuild, discordRoleLinked, "", "")
	f.app.discordSyncRoles(f.ctx, now.Add(7*time.Hour+time.Minute))
	if got, want := fake.held(discordCorpGuild, discordAlice), discordRoleLinked+","+discordRoleMod; got != want {
		t.Fatalf("after the member role was unset: %s, want %s", got, want)
	}

	// Disconnecting takes back what was given, everywhere, and only that.
	f.post(cookie, "/discord/disconnect", url.Values{})
	if got := fake.held(discordCorpGuild, discordAlice); got != discordRoleMod {
		t.Fatalf("after disconnecting: %s, want only the moderator's role", got)
	}
	if got := fake.held(discordAllianceGuild, discordAlice); got != "" {
		t.Fatalf("after disconnecting, in the alliance's server: %s", got)
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
	f.joinCorp(f.ch.CharacterID, discordCorp, "Director")
	cookie, _ = f.addServer(cookie, discordCorpOwner, "add-corp")
	f.setServer(cookie, discordCorpGuild, discordRoleLinked, discordRoleMember, "")
	cookie, _ = f.connect(cookie, "alice")
	now := notifyT0
	fake.join(discordCorpGuild, discordAlice, discordRoleMod)
	fake.join(discordCorpGuild, discordBob)
	f.app.discordSyncRoles(f.ctx, now)
	if got, want := fake.held(discordCorpGuild, discordAlice), discordRoleLinked+","+discordRoleMember+","+discordRoleMod; got != want {
		t.Fatalf("to begin with: %s, want %s", got, want)
	}

	// The same account connects a different Discord account: the
	// first one loses what it was given, the second gains it.
	f.connect(cookie, "bob")
	f.app.discordSyncRoles(f.ctx, now.Add(time.Minute))
	if got := fake.held(discordCorpGuild, discordAlice); got != discordRoleMod {
		t.Fatalf("the Discord account that was swapped out still holds %s", got)
	}
	if got, want := fake.held(discordCorpGuild, discordBob), discordRoleLinked+","+discordRoleMember; got != want {
		t.Fatalf("the Discord account swapped in holds %s, want %s", got, want)
	}

	// The EveSynapse account is deleted outright, behind the app's
	// back, while Discord is failing. Nothing is forgotten.
	if _, err := f.app.db.ExecContext(f.ctx, `DELETE FROM users WHERE id = $1`, f.userID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.q.GetDiscordLink(f.ctx, f.userID); err == nil {
		t.Fatal("the link outlived the account")
	}
	fake.mu.Lock()
	fake.failMembers = true
	fake.mu.Unlock()
	f.app.discordSyncRoles(f.ctx, now.Add(2*time.Minute))
	fake.mu.Lock()
	fake.failMembers = false
	fake.mu.Unlock()
	if got := fake.held(discordCorpGuild, discordBob); got == "" {
		t.Fatal("the test did not leave the roles in place while Discord was failing")
	}
	if n := f.app.discordSyncRoles(f.ctx, now.Add(3*time.Minute)); n != 1 {
		t.Fatalf("after the account was deleted: %d change(s), want the roles taken back", n)
	}
	if got := fake.held(discordCorpGuild, discordBob); got != "" {
		t.Fatalf("a deleted account's Discord member still holds %s", got)
	}
	// And then there is nothing left to do.
	calls := fake.asked()
	if n := f.app.discordSyncRoles(f.ctx, now.Add(4*time.Minute)); n != 0 || fake.asked() != calls {
		t.Fatalf("with nothing left to take back: %d change(s), %d call(s)", n, fake.asked()-calls)
	}
}

// TestDiscordServerRemoval: removing a server first takes back the
// roles the bot gave there, and only then forgets it and has the bot
// leave.
func TestDiscordServerRemoval(t *testing.T) {
	f := newNotifyFixture(t)
	fake := f.enableDiscord()
	cookie := sessionCookie(t, f.app, f.userID, f.ch.CharacterID, f.ch.Name)
	f.joinCorp(f.ch.CharacterID, discordCorp, "Director")
	cookie, _ = f.addServer(cookie, discordCorpOwner, "add-corp")
	f.setServer(cookie, discordCorpGuild, discordRoleLinked, discordRoleMember, discordOpsChannel)
	cookie, _ = f.connect(cookie, "alice")
	fake.join(discordCorpGuild, discordAlice, discordRoleMod)
	now := notifyT0
	f.app.discordSyncRoles(f.ctx, now)

	f.post(cookie, "/discord/servers/"+discordCorpGuild+"/remove", url.Values{})
	_, body := getPage(t, f.app, cookie, discordServersPath)
	mustContain(t, "first press", body, "is switched off. The bot is taking back the roles it gave there (1 member(s) to go)")
	if len(fake.left) != 0 {
		t.Fatal("the bot left a server while members still held its roles")
	}
	f.app.discordSyncRoles(f.ctx, now.Add(time.Minute))
	if got := fake.held(discordCorpGuild, discordAlice); got != discordRoleMod {
		t.Fatalf("after switching off: %s, want only the moderator's role", got)
	}
	f.post(cookie, "/discord/servers/"+discordCorpGuild+"/remove", url.Values{})
	if _, err := f.q.GetDiscordGuild(f.ctx, discordCorpGuild); err == nil {
		t.Fatal("the server is still recorded after removal")
	}
	if len(fake.left) != 1 || fake.left[0] != discordCorpGuild {
		t.Fatalf("the bot left %q, want the removed server", fake.left)
	}
}
