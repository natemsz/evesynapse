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
		ClientID: "client", ClientSecret: "secret", BotToken: "bot-token", GuildID: guild,
		RedirectURL: "https://eve.example/discord/callback",
	}, &http.Client{Transport: f}, "https://discord.test/api")
}

func TestAuthURLAsksOnlyForIdentity(t *testing.T) {
	u, err := url.Parse(newClient(&fake{}).AuthURL("state-1"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Host != "discord.com" || q.Get("scope") != "identify" || q.Get("state") != "state-1" ||
		q.Get("client_id") != "client" || q.Get("redirect_uri") != "https://eve.example/discord/callback" || q.Get("response_type") != "code" {
		t.Fatalf("sign-in address %s", u)
	}
	if strings.Contains(u.String(), "secret") || strings.Contains(u.String(), "bot-token") {
		t.Fatal("the sign-in address carries a secret")
	}
}

func TestIdentifyReadsTheAccountAndNothingElse(t *testing.T) {
	f := &fake{answers: map[string][2]string{
		"POST /api/oauth2/token": {"200", `{"access_token":"user-token","token_type":"Bearer"}`},
		"GET /api/users/@me":     {"200", `{"id":"` + user + `","username":"pilot","global_name":"Pilot One"}`},
	}}
	got, err := newClient(f).Identify(context.Background(), "the-code")
	if err != nil {
		t.Fatal(err)
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
	if _, err := newClient(f).Identify(context.Background(), "c"); err == nil {
		t.Fatal("an account with no id was accepted")
	}
	f.answers["POST /api/oauth2/token"] = [2]string{"400", `{"message":"invalid_grant"}`}
	if _, err := newClient(f).Identify(context.Background(), "c"); !IsStatus(err, 400) {
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
	roles, err := c.MemberRoles(context.Background(), user)
	if err != nil || len(roles) != 2 || roles[0] != role {
		t.Fatalf("roles %v, %v", roles, err)
	}
	if err := c.SetRole(context.Background(), user, role, true); err != nil {
		t.Fatal(err)
	}
	if err := c.SetRole(context.Background(), user, role, false); err != nil {
		t.Fatal(err)
	}
	if got := f.calls[1].Method + " " + f.calls[2].Method; got != "PUT DELETE" {
		t.Fatalf("give and take were sent as %s", got)
	}
	// Not in the server.
	delete(f.answers, "GET "+member)
	if _, err := c.MemberRoles(context.Background(), user); !IsStatus(err, 404) {
		t.Fatalf("a user outside the server: %v", err)
	}
	// Asked to slow down: how long is passed on.
	f.answers["GET "+member] = [2]string{"429", `{"message":"You are being rate limited.","retry_after":1.5}`}
	_, err = c.MemberRoles(context.Background(), user)
	se, ok := err.(*StatusError)
	if !ok || se.Status != 429 || se.RetryAfter != 1500*time.Millisecond {
		t.Fatalf("rate limit: %v", err)
	}
	if strings.Contains(se.Error(), "bot-token") {
		t.Fatal("an error message carries the bot token")
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
