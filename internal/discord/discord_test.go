package discord

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// call is one request the stand-in for Discord received.
type call struct {
	Method, Path, Auth, Body string
}

// fake answers as Discord would, from a table of "METHOD /path" to a
// status and body, and records what it was asked.
type fake struct {
	answers map[string][2]string // key -> {status, body}
	calls   []call
}

func (f *fake) RoundTrip(req *http.Request) (*http.Response, error) {
	body := ""
	if req.Body != nil {
		raw, _ := io.ReadAll(req.Body)
		body = string(raw)
	}
	f.calls = append(f.calls, call{req.Method, req.URL.Path, req.Header.Get("Authorization"), body})
	status, text := "404", `{"message":"Unknown"}`
	if a, ok := f.answers[req.Method+" "+req.URL.Path]; ok {
		status, text = a[0], a[1]
	}
	code := 0
	for _, ch := range status {
		code = code*10 + int(ch-'0')
	}
	return &http.Response{StatusCode: code, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(text)), Request: req}, nil
}

const (
	user  = "111111111111111111"
	guild = "222222222222222222"
	role  = "333333333333333333"
	dm    = "444444444444444444"
)

func newClient(f *fake) *Client {
	return New(Config{
		ClientID: "client", ClientSecret: "secret", BotToken: "bot-token",
		RedirectURL: "https://eve.example/discord/callback",
	}, &http.Client{Transport: f}, "https://discord.test/api")
}

func TestAuthURLAsksForIdentityAndJoining(t *testing.T) {
	u, err := url.Parse(newClient(&fake{}).AuthURL("state-1"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Host != "discord.com" || q.Get("scope") != "identify guilds.join" || q.Get("state") != "state-1" ||
		q.Get("client_id") != "client" || q.Get("redirect_uri") != "https://eve.example/discord/callback" || q.Get("response_type") != "code" {
		t.Fatalf("sign-in address %s", u)
	}
	if strings.Contains(u.String(), "secret") || strings.Contains(u.String(), "bot-token") {
		t.Fatal("the sign-in address carries a secret")
	}
}

func TestIdentifyReadsTheAccountAndNothingElse(t *testing.T) {
	f := &fake{answers: map[string][2]string{
		"POST /api/oauth2/token": {"200", `{"access_token":"user-token","token_type":"Bearer","refresh_token":"user-refresh","expires_in":604800}`},
		"GET /api/users/@me":     {"200", `{"id":"` + user + `","username":"pilot","global_name":"Pilot One"}`},
	}}
	got, token, err := newClient(f).Identify(context.Background(), "the-code")
	if err != nil {
		t.Fatal(err)
	}
	if token.Access != "user-token" || token.Refresh != "user-refresh" || time.Until(token.Expiry) < 6*24*time.Hour {
		t.Fatalf("token %+v", token)
	}
	if got.ID != user || got.DisplayName() != "Pilot One" {
		t.Fatalf("identified %+v", got)
	}
	if len(f.calls) != 2 {
		t.Fatalf("%d calls, want the exchange and the lookup: %+v", len(f.calls), f.calls)
	}
	exchange, lookup := f.calls[0], f.calls[1]
	form, _ := url.ParseQuery(exchange.Body)
	if !strings.HasPrefix(exchange.Auth, "Basic ") || form.Get("code") != "the-code" || form.Get("grant_type") != "authorization_code" {
		t.Fatalf("exchange %+v", exchange)
	}
	if lookup.Auth != "Bearer user-token" {
		t.Fatalf("lookup authorised as %q, want the user's own token", lookup.Auth)
	}

	// An account with no id, or a refused code, is an error.
	f.answers["GET /api/users/@me"] = [2]string{"200", `{"username":"pilot"}`}
	if _, _, err := newClient(f).Identify(context.Background(), "c"); err == nil {
		t.Fatal("an account with no id was accepted")
	}
	f.answers["POST /api/oauth2/token"] = [2]string{"400", `{"message":"invalid_grant"}`}
	if _, _, err := newClient(f).Identify(context.Background(), "c"); !IsStatus(err, 400) {
		t.Fatalf("refused code: %v", err)
	}
}

func TestSendDMPingsNobody(t *testing.T) {
	f := &fake{answers: map[string][2]string{
		"POST /api/users/@me/channels":           {"200", `{"id":"` + dm + `"}`},
		"POST /api/channels/" + dm + "/messages": {"200", `{"id":"1"}`},
	}}
	long := strings.Repeat("x", 3000)
	if err := newClient(f).SendDM(context.Background(), user, Message{Content: "@everyone hello " + long}); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 2 || f.calls[0].Auth != "Bot bot-token" || !strings.Contains(f.calls[0].Body, user) {
		t.Fatalf("calls %+v", f.calls)
	}
	var sent struct {
		Content         string `json:"content"`
		AllowedMentions struct {
			Parse []string `json:"parse"`
		} `json:"allowed_mentions"`
	}
	if err := json.Unmarshal([]byte(f.calls[1].Body), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.AllowedMentions.Parse == nil || len(sent.AllowedMentions.Parse) != 0 {
		t.Fatalf("mentions are not switched off: %s", f.calls[1].Body[:120])
	}
	if n := utf8.RuneCountInString(sent.Content); n != 2000 {
		t.Fatalf("a %d character message was sent; want it cut to Discord's 2000", n)
	}

	// Discord refusing the conversation is passed on as its status.
	f.answers["POST /api/users/@me/channels"] = [2]string{"403", `{"message":"Cannot send messages to this user"}`}
	if err := newClient(f).SendDM(context.Background(), user, Message{Content: "hi"}); !IsStatus(err, 403) {
		t.Fatalf("refused conversation: %v", err)
	}
	// Something that is not an id never reaches a request path.
	before := len(f.calls)
	if err := newClient(f).SendChannel(context.Background(), "../guilds", Message{Content: "hi"}); err == nil || len(f.calls) != before {
		t.Fatal("a channel id that is not an id was used")
	}
}

func TestRoles(t *testing.T) {
	member := "/api/guilds/" + guild + "/members/" + user
	f := &fake{answers: map[string][2]string{
		"GET " + member:                       {"200", `{"roles":["` + role + `","555555555555555555"]}`},
		"PUT " + member + "/roles/" + role:    {"204", ``},
		"DELETE " + member + "/roles/" + role: {"204", ``},
	}}
	c := newClient(f)
	roles, err := c.MemberRoles(context.Background(), guild, user)
	if err != nil || len(roles) != 2 || roles[0] != role {
		t.Fatalf("roles %v, %v", roles, err)
	}
	if err := c.SetRole(context.Background(), guild, user, role, true); err != nil {
		t.Fatal(err)
	}
	if err := c.SetRole(context.Background(), guild, user, role, false); err != nil {
		t.Fatal(err)
	}
	if got := f.calls[1].Method + " " + f.calls[2].Method; got != "PUT DELETE" {
		t.Fatalf("give and take were sent as %s", got)
	}
	// Not in the server.
	delete(f.answers, "GET "+member)
	if _, err := c.MemberRoles(context.Background(), guild, user); !IsStatus(err, 404) {
		t.Fatalf("a user outside the server: %v", err)
	}
	// Asked to slow down: how long is passed on.
	f.answers["GET "+member] = [2]string{"429", `{"message":"You are being rate limited.","retry_after":1.5}`}
	_, err = c.MemberRoles(context.Background(), guild, user)
	se, ok := err.(*StatusError)
	if !ok || se.Status != 429 || se.RetryAfter != 1500*time.Millisecond {
		t.Fatalf("rate limit: %v", err)
	}
	if strings.Contains(se.Error(), "bot-token") {
		t.Fatal("an error message carries the bot token")
	}
}

// TestInstall: adding the bot asks for giving roles and sending
// messages and nothing more, and which server it was added to is what
// Discord says in its own answer.
func TestInstall(t *testing.T) {
	f := &fake{answers: map[string][2]string{
		"POST /api/oauth2/token": {"200", `{"access_token":"user-token","guild":{"id":"` + guild + `","name":"Home"}}`},
		"GET /api/users/@me":     {"200", `{"id":"` + user + `","username":"pilot"}`},
	}}
	c := newClient(f)
	u, err := url.Parse(c.InstallURL("state-2"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("scope") != "bot identify guilds.join" || q.Get("state") != "state-2" {
		t.Fatalf("install address %s", u)
	}
	// Manage Roles, View Channels, Send Messages, Create Invite, Kick
	// Members: 1<<28 | 1<<10 | 1<<11 | 1<<0 | 1<<1.
	if q.Get("permissions") != "268438531" {
		t.Fatalf("the bot asks for permissions %s, want only roles, sending, adding and removing members (268438531)", q.Get("permissions"))
	}
	who, where, err := c.IdentifyInstall(context.Background(), "code")
	if err != nil || who.ID != user || where.ID != guild || where.Name != "Home" {
		t.Fatalf("install identified %+v in %+v, %v", who, where, err)
	}
	// A plain sign-in adds the bot nowhere.
	f.answers["POST /api/oauth2/token"] = [2]string{"200", `{"access_token":"user-token"}`}
	if _, where, err = c.IdentifyInstall(context.Background(), "code"); err != nil || where.ID != "" {
		t.Fatalf("a plain sign-in reported server %+v, %v", where, err)
	}
}

func TestGuildRolesAndChannels(t *testing.T) {
	f := &fake{answers: map[string][2]string{
		"GET /api/guilds/" + guild + "/roles":    {"200", `[{"id":"` + guild + `","name":"@everyone","position":0},{"id":"` + role + `","name":"Member","position":2},{"id":"555555555555555555","name":"Bot","managed":true,"position":5}]`},
		"GET /api/guilds/" + guild + "/channels": {"200", `[{"id":"666666666666666661","name":"voice","type":2,"position":0},{"id":"666666666666666662","name":"ops","type":0,"position":3},{"id":"666666666666666663","name":"news","type":5,"position":1}]`},
		"DELETE /api/users/@me/guilds/" + guild:  {"204", ``},
	}}
	c := newClient(f)
	roles, err := c.GuildRoles(context.Background(), guild)
	if err != nil || len(roles) != 2 || roles[0].Name != "Bot" || !roles[0].Managed || roles[1].ID != role {
		t.Fatalf("roles %+v, %v; want the two real roles, highest first, without @everyone", roles, err)
	}
	channels, err := c.GuildTextChannels(context.Background(), guild)
	if err != nil || len(channels) != 2 || channels[0].Name != "news" || channels[1].Name != "ops" {
		t.Fatalf("channels %+v, %v; want the two a message can go in, in order", channels, err)
	}
	if err := c.LeaveGuild(context.Background(), guild); err != nil {
		t.Fatal(err)
	}
}

// TestAddMemberAndRefresh: adding an account to a server goes out as
// the bot, carrying the account's own token and the roles to give;
// 201 is "added" and 204 "was there already". A token is renewed with
// the application's own credentials.
func TestAddMemberAndRefresh(t *testing.T) {
	member := "/api/guilds/" + guild + "/members/" + user
	f := &fake{answers: map[string][2]string{
		"PUT " + member:          {"201", `{"user":{"id":"` + user + `"}}`},
		"POST /api/oauth2/token": {"200", `{"access_token":"new-token","refresh_token":"new-refresh","expires_in":600}`},
	}}
	c := newClient(f)
	added, err := c.AddMember(context.Background(), guild, user, "user-token", []string{role})
	if err != nil || !added {
		t.Fatalf("added=%v, %v", added, err)
	}
	sent := f.calls[0]
	if sent.Auth != "Bot bot-token" || !strings.Contains(sent.Body, `"access_token":"user-token"`) || !strings.Contains(sent.Body, `"roles":["`+role+`"]`) {
		t.Fatalf("the request: %+v", sent)
	}
	f.answers["PUT "+member] = [2]string{"204", ``}
	if added, err = c.AddMember(context.Background(), guild, user, "user-token", nil); err != nil || added {
		t.Fatalf("someone already in the server: added=%v, %v", added, err)
	}
	if !strings.Contains(f.calls[1].Body, `"roles":[]`) {
		t.Fatalf("no roles should be sent as an empty list: %s", f.calls[1].Body)
	}
	before := len(f.calls)
	if _, err = c.AddMember(context.Background(), guild, user, "", nil); err == nil || len(f.calls) != before {
		t.Fatal("a member was added with no token")
	}

	token, err := c.Refresh(context.Background(), "old-refresh")
	if err != nil || token.Access != "new-token" || token.Refresh != "new-refresh" {
		t.Fatalf("refreshed %+v, %v", token, err)
	}
	form, _ := url.ParseQuery(f.calls[len(f.calls)-1].Body)
	if form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "old-refresh" || !strings.HasPrefix(f.calls[len(f.calls)-1].Auth, "Basic ") {
		t.Fatalf("the refresh request: %+v", f.calls[len(f.calls)-1])
	}
	f.answers["POST /api/oauth2/token"] = [2]string{"400", `{"message":"invalid_grant"}`}
	if _, err = c.Refresh(context.Background(), "revoked"); !IsStatus(err, 400) {
		t.Fatalf("a revoked refresh token: %v", err)
	}
}

// TestMembersOwnerAndKick: the member list is read page by page to its
// end, a list cut short is reported as incomplete, and a removal is a
// DELETE of that member with the reason for the audit log.
func TestMembersOwnerAndKick(t *testing.T) {
	member := func(id string) string {
		return `{"user":{"id":"` + id + `","username":"u` + id[len(id)-2:] + `"},"roles":["` + role + `"],"joined_at":"2026-01-02T03:04:05.000000+00:00"}`
	}
	f := &fake{answers: map[string][2]string{
		"GET /api/guilds/" + guild + "/members":            {"200", `[` + member(user) + `,{"user":{"id":"777777777777777777","username":"helper","bot":true},"roles":[],"joined_at":"2026-01-02T03:04:05+00:00"}]`},
		"GET /api/guilds/" + guild:                         {"200", `{"id":"` + guild + `","owner_id":"` + user + `"}`},
		"DELETE /api/guilds/" + guild + "/members/" + user: {"204", ``},
	}}
	c := newClient(f)
	members, complete, err := c.ListMembers(context.Background(), guild, 5000)
	if err != nil || !complete || len(members) != 2 || members[0].User.ID != user || !members[1].User.Bot || members[0].JoinedAt.Year() != 2026 {
		t.Fatalf("members %+v, complete=%v, %v", members, complete, err)
	}
	owner, err := c.GuildOwner(context.Background(), guild)
	if err != nil || owner != user {
		t.Fatalf("owner %q, %v", owner, err)
	}
	if err := c.Kick(context.Background(), guild, user, "left the corporation"); err != nil {
		t.Fatal(err)
	}
	last := f.calls[len(f.calls)-1]
	if last.Method != "DELETE" || last.Auth != "Bot bot-token" || last.Path != "/api/guilds/"+guild+"/members/"+user {
		t.Fatalf("the removal: %+v", last)
	}
	// Not allowed to read the list (no Server Members intent).
	f.answers["GET /api/guilds/"+guild+"/members"] = [2]string{"403", `{"message":"Missing Access"}`}
	if _, _, err = c.ListMembers(context.Background(), guild, 5000); !IsStatus(err, 403) {
		t.Fatalf("a refused member list: %v", err)
	}
}

func TestValidID(t *testing.T) {
	for id, want := range map[string]bool{
		user: true, "12345": true, "": false, "1234": false, "abc": false, "123/456": false, "../x": false, " 123456": false,
	} {
		if ValidID(id) != want {
			t.Errorf("ValidID(%q) = %v, want %v", id, !want, want)
		}
	}
}
