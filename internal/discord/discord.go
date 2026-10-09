// Package discord is the little of Discord's HTTP API that EveSynapse
// uses, on the standard library alone.
//
// Two different credentials are involved, and they do different jobs:
//
//   - The application's client id and secret run the "Connect Discord"
//     sign-in: a user proves which Discord account is theirs, the
//     account's id and name are read once, and the user's token is
//     thrown away. Nothing here can act as that user afterwards.
//
//   - The bot token acts as the bot, in whichever servers it has been
//     invited to: it sends messages and gives or takes roles. In each
//     it can do only what that server's settings let the bot's own
//     role do.
//
// Nothing is ever read from a channel: the bot does not connect to
// Discord's gateway and has no message handler.
package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is Discord's API root.
const DefaultBaseURL = "https://discord.com/api/v10"

// authorizeURL is where a user is sent to approve the sign-in.
const authorizeURL = "https://discord.com/oauth2/authorize"

// Config is what an install is given. Any part may be empty: the
// sign-in needs the client id and secret, the bot calls the token.
type Config struct {
	ClientID     string
	ClientSecret string
	BotToken     string
	RedirectURL  string // this site's /discord/callback
}

// CanLink reports whether the "Connect Discord" sign-in can run.
func (c Config) CanLink() bool {
	return c.ClientID != "" && c.ClientSecret != "" && c.RedirectURL != ""
}

// HasBot reports whether the bot can act at all.
func (c Config) HasBot() bool { return c.BotToken != "" }

// CanInstall reports whether the bot can be added to a server from
// this site: that takes the sign-in and the bot both.
func (c Config) CanInstall() bool { return c.CanLink() && c.HasBot() }

// Client talks to Discord.
type Client struct {
	cfg     Config
	http    *http.Client
	baseURL string
}

// New returns a client. httpClient may be nil; baseURL is
// DefaultBaseURL unless a test points it elsewhere.
func New(cfg Config, httpClient *http.Client, baseURL string) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{cfg: cfg, http: httpClient, baseURL: strings.TrimRight(baseURL, "/")}
}

// Config returns what the client was made with.
func (c *Client) Config() Config { return c.cfg }

// snowflake is the shape of every Discord id: a decimal number.
var snowflake = regexp.MustCompile(`^[0-9]{5,25}$`)

// ValidID reports whether s looks like a Discord id. Ids go into
// request paths, so nothing else is ever put there.
func ValidID(s string) bool { return snowflake.MatchString(s) }

// StatusError is an answer from Discord that was not a success.
type StatusError struct {
	Method, Path string
	Status       int
	Message      string // Discord's own "message", when it sent one
	// RetryAfter is how long Discord asked to wait (a 429), else 0.
	RetryAfter time.Duration
}

func (e *StatusError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("discord %s %s: HTTP %d: %s", e.Method, e.Path, e.Status, e.Message)
	}
	return fmt.Sprintf("discord %s %s: HTTP %d", e.Method, e.Path, e.Status)
}

// IsStatus reports whether err is Discord answering with status.
func IsStatus(err error, status int) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Status == status
}

// AuthURL is where to send a user to connect their Discord account.
// Two things are asked for: "identify", the account's id and name, and
// "guilds.join", so that the bot may add the account to a server it is
// in (Discord words it "join servers for you").
func (c *Client) AuthURL(state string) string {
	q := url.Values{}
	q.Set("client_id", c.cfg.ClientID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", c.cfg.RedirectURL)
	q.Set("scope", "identify guilds.join")
	q.Set("state", state)
	q.Set("prompt", "consent")
	return authorizeURL + "?" + q.Encode()
}

// User is a Discord account.
type User struct {
	ID         string `json:"id"`
	Username   string `json:"username"`
	GlobalName string `json:"global_name"`
}

// DisplayName is the name to show for the account.
func (u User) DisplayName() string {
	if u.GlobalName != "" {
		return u.GlobalName
	}
	return u.Username
}

// Guild is a Discord server.
type Guild struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// botPermissions is what the bot asks for when it is added to a
// server: Manage Roles (1<<28), View Channels (1<<10), Send Messages
// (1<<11), and Create Invite (1<<0), which is what Discord requires of
// a bot that adds members. Nothing that reads messages or manages
// channels.
const botPermissions = 1<<28 | 1<<10 | 1<<11 | 1<<0

// InstallURL is where to send someone to add the bot to a server of
// theirs. Discord only lets a person who may manage a server add a bot
// to it, and shows them the permissions asked for.
func (c *Client) InstallURL(state string) string {
	q := url.Values{}
	q.Set("client_id", c.cfg.ClientID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", c.cfg.RedirectURL)
	q.Set("scope", "bot identify guilds.join")
	q.Set("permissions", strconv.Itoa(botPermissions))
	q.Set("state", state)
	return authorizeURL + "?" + q.Encode()
}

// Token is an account's Discord token, as far as it was asked for: it
// can read the account's name and add the account to servers the bot
// is in.
type Token struct {
	Access  string
	Refresh string
	Expiry  time.Time
}

// Identify finishes a sign-in: it trades the code Discord sent back
// for a token and reads whose account it is.
func (c *Client) Identify(ctx context.Context, code string) (User, Token, error) {
	user, _, token, err := c.identify(ctx, code)
	return user, token, err
}

// IdentifyInstall is Identify for a return from InstallURL: it also
// reports the server the bot was just added to, as Discord itself
// states it in its answer (never as the browser claims it). The
// server is empty when the sign-in added the bot nowhere.
func (c *Client) IdentifyInstall(ctx context.Context, code string) (User, Guild, error) {
	user, guild, _, err := c.identify(ctx, code)
	if err == nil && guild.ID != "" && !ValidID(guild.ID) {
		return User{}, Guild{}, errors.New("discord: the sign-in returned a server with no id")
	}
	return user, guild, err
}

func (c *Client) identify(ctx context.Context, code string) (User, Guild, Token, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", c.cfg.RedirectURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return User{}, Guild{}, Token{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(c.cfg.ClientID, c.cfg.ClientSecret)
	var token tokenAnswer
	if _, err := c.do(req, "/oauth2/token", &token); err != nil {
		return User{}, Guild{}, Token{}, err
	}
	if token.AccessToken == "" {
		return User{}, Guild{}, Token{}, errors.New("discord: the sign-in returned no token")
	}

	req, err = http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/users/@me", nil)
	if err != nil {
		return User{}, Guild{}, Token{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token.AccessToken)
	var user User
	if _, err := c.do(req, "/users/@me", &user); err != nil {
		return User{}, Guild{}, Token{}, err
	}
	if !ValidID(user.ID) {
		return User{}, Guild{}, Token{}, errors.New("discord: the sign-in returned no account id")
	}
	return user, token.Guild, token.token(time.Now()), nil
}

// tokenAnswer is Discord's answer to a code or a refresh token.
type tokenAnswer struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Guild        Guild  `json:"guild"`
}

func (a tokenAnswer) token(now time.Time) Token {
	return Token{Access: a.AccessToken, Refresh: a.RefreshToken, Expiry: now.Add(time.Duration(a.ExpiresIn) * time.Second)}
}

// Refresh trades a refresh token for a new token. Discord answers 400
// when the account has taken its permission back.
func (c *Client) Refresh(ctx context.Context, refresh string) (Token, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refresh)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return Token{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(c.cfg.ClientID, c.cfg.ClientSecret)
	var answer tokenAnswer
	if _, err := c.do(req, "/oauth2/token", &answer); err != nil {
		return Token{}, err
	}
	if answer.AccessToken == "" {
		return Token{}, errors.New("discord: the refresh returned no token")
	}
	return answer.token(time.Now()), nil
}

// AddMember puts an account into a server, with roles, on the
// strength of the account's own token (it agreed to "join servers for
// you"). added is false when the account was in the server already,
// in which case nothing about it is changed.
func (c *Client) AddMember(ctx context.Context, guildID, userID, userToken string, roles []string) (added bool, err error) {
	if !ValidID(guildID) || !ValidID(userID) || userToken == "" {
		return false, errors.New("discord: not a server or user id, or no token")
	}
	for _, role := range roles {
		if !ValidID(role) {
			return false, errors.New("discord: not a role id")
		}
	}
	if roles == nil {
		roles = []string{}
	}
	status, err := c.botStatus(ctx, http.MethodPut, "/guilds/"+guildID+"/members/"+userID,
		map[string]any{"access_token": userToken, "roles": roles}, nil)
	return err == nil && status == http.StatusCreated, err
}

// bot makes a call as the bot. payload and out may be nil.
func (c *Client) bot(ctx context.Context, method, path string, payload, out any) error {
	_, err := c.botStatus(ctx, method, path, payload, out)
	return err
}

// botStatus is bot, also reporting which success status came back.
func (c *Client) botStatus(ctx context.Context, method, path string, payload, out any) (int, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bot "+c.cfg.BotToken)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.do(req, path, out)
}

func (c *Client) do(req *http.Request, path string, out any) (int, error) {
	// The form Discord asks of every bot: DiscordBot (url, version).
	req.Header.Set("User-Agent", "DiscordBot (https://github.com/natemsz/evesynapse, 1)")
	resp, err := c.http.Do(req)
	if err != nil {
		// The error may carry the request's address, never its headers.
		return 0, fmt.Errorf("discord %s %s: %w", req.Method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		se := &StatusError{Method: req.Method, Path: path, Status: resp.StatusCode}
		var said struct {
			Message    string  `json:"message"`
			RetryAfter float64 `json:"retry_after"`
		}
		if json.Unmarshal(raw, &said) == nil {
			se.Message = said.Message
			if resp.StatusCode == http.StatusTooManyRequests && said.RetryAfter > 0 {
				se.RetryAfter = time.Duration(said.RetryAfter * float64(time.Second))
			}
		}
		if se.RetryAfter == 0 && resp.StatusCode == http.StatusTooManyRequests {
			if secs, err := strconv.ParseFloat(resp.Header.Get("Retry-After"), 64); err == nil && secs > 0 {
				se.RetryAfter = time.Duration(secs * float64(time.Second))
			}
		}
		return resp.StatusCode, se
	}
	if out == nil || len(raw) == 0 {
		return resp.StatusCode, nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return resp.StatusCode, fmt.Errorf("discord %s %s: decode: %w", req.Method, path, err)
	}
	return resp.StatusCode, nil
}

// Message is what the bot posts. Mentions are switched off: whatever
// the text holds, nobody is pinged by it.
type Message struct {
	Content string
}

// messageMax is Discord's limit on a message's text, in characters.
const messageMax = 2000

func (m Message) payload() map[string]any {
	content := m.Content
	if runes := []rune(content); len(runes) > messageMax {
		content = string(runes[:messageMax-1]) + "…"
	}
	return map[string]any{
		"content":          content,
		"allowed_mentions": map[string]any{"parse": []string{}},
	}
}

// SendChannel posts a message in a channel.
func (c *Client) SendChannel(ctx context.Context, channelID string, m Message) error {
	if !ValidID(channelID) {
		return fmt.Errorf("discord: %q is not a channel id", channelID)
	}
	return c.bot(ctx, http.MethodPost, "/channels/"+channelID+"/messages", m.payload(), nil)
}

// SendDM sends a message to one user. Discord refuses (403) when the
// user shares no server with the bot or does not take messages from
// server members.
func (c *Client) SendDM(ctx context.Context, userID string, m Message) error {
	if !ValidID(userID) {
		return fmt.Errorf("discord: %q is not a user id", userID)
	}
	var channel struct {
		ID string `json:"id"`
	}
	if err := c.bot(ctx, http.MethodPost, "/users/@me/channels", map[string]string{"recipient_id": userID}, &channel); err != nil {
		return err
	}
	return c.SendChannel(ctx, channel.ID, m)
}

// MemberRoles lists the roles a user holds in a server. A user who is
// not in the server is a 404 (see IsStatus).
func (c *Client) MemberRoles(ctx context.Context, guildID, userID string) ([]string, error) {
	if !ValidID(userID) || !ValidID(guildID) {
		return nil, errors.New("discord: not a user or server id")
	}
	var member struct {
		Roles []string `json:"roles"`
	}
	if err := c.bot(ctx, http.MethodGet, "/guilds/"+guildID+"/members/"+userID, nil, &member); err != nil {
		return nil, err
	}
	return member.Roles, nil
}

// SetRole gives a user a role in a server, or takes it away.
func (c *Client) SetRole(ctx context.Context, guildID, userID, roleID string, held bool) error {
	if !ValidID(userID) || !ValidID(roleID) || !ValidID(guildID) {
		return errors.New("discord: not a user, role or server id")
	}
	method := http.MethodPut
	if !held {
		method = http.MethodDelete
	}
	return c.bot(ctx, method, "/guilds/"+guildID+"/members/"+userID+"/roles/"+roleID, nil, nil)
}

// Role is a role of a server. Managed roles belong to a bot or an
// integration and cannot be given to members.
type Role struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Managed  bool   `json:"managed"`
	Position int    `json:"position"`
}

// GuildRoles lists a server's roles, highest first, without the
// @everyone role (whose id is the server's own).
func (c *Client) GuildRoles(ctx context.Context, guildID string) ([]Role, error) {
	if !ValidID(guildID) {
		return nil, errors.New("discord: not a server id")
	}
	var roles []Role
	if err := c.bot(ctx, http.MethodGet, "/guilds/"+guildID+"/roles", nil, &roles); err != nil {
		return nil, err
	}
	out := roles[:0]
	for _, role := range roles {
		if role.ID != guildID {
			out = append(out, role)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Position > out[j].Position })
	return out, nil
}

// Channel is a channel of a server.
type Channel struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Type     int    `json:"type"`
	Position int    `json:"position"`
}

// GuildTextChannels lists the channels of a server a message can be
// posted in: text (0) and announcement (5) channels.
func (c *Client) GuildTextChannels(ctx context.Context, guildID string) ([]Channel, error) {
	if !ValidID(guildID) {
		return nil, errors.New("discord: not a server id")
	}
	var channels []Channel
	if err := c.bot(ctx, http.MethodGet, "/guilds/"+guildID+"/channels", nil, &channels); err != nil {
		return nil, err
	}
	out := channels[:0]
	for _, ch := range channels {
		if ch.Type == 0 || ch.Type == 5 {
			out = append(out, ch)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Position < out[j].Position })
	return out, nil
}

// LeaveGuild takes the bot out of a server.
func (c *Client) LeaveGuild(ctx context.Context, guildID string) error {
	if !ValidID(guildID) {
		return errors.New("discord: not a server id")
	}
	return c.bot(ctx, http.MethodDelete, "/users/@me/guilds/"+guildID, nil, nil)
}
