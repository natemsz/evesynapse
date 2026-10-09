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
//   - The bot token acts as the bot, in the one server it has been
//     invited to: it sends messages and gives or takes roles. It can do
//     only what that server's settings let the bot's own role do.
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
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is Discord's API root.
const DefaultBaseURL = "https://discord.com/api/v10"

// authorizeURL is where a user is sent to approve the sign-in.
const authorizeURL = "https://discord.com/oauth2/authorize"

// Config is what an install is given. Any part may be empty: the
// sign-in needs the client id and secret, the bot calls the token and
// the server.
type Config struct {
	ClientID     string
	ClientSecret string
	BotToken     string
	GuildID      string // the one server the bot acts in
	RedirectURL  string // this site's /discord/callback
}

// CanLink reports whether the "Connect Discord" sign-in can run.
func (c Config) CanLink() bool {
	return c.ClientID != "" && c.ClientSecret != "" && c.RedirectURL != ""
}

// HasBot reports whether the bot can act in a server.
func (c Config) HasBot() bool { return c.BotToken != "" && c.GuildID != "" }

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
// Only "identify" is asked for: the account's id and name.
func (c *Client) AuthURL(state string) string {
	q := url.Values{}
	q.Set("client_id", c.cfg.ClientID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", c.cfg.RedirectURL)
	q.Set("scope", "identify")
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

// Identify finishes a sign-in: it trades the code Discord sent back
// for a token, reads whose account it is, and returns that. The token
// is not kept.
func (c *Client) Identify(ctx context.Context, code string) (User, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", c.cfg.RedirectURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return User{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(c.cfg.ClientID, c.cfg.ClientSecret)
	var token struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
	}
	if err := c.do(req, "/oauth2/token", &token); err != nil {
		return User{}, err
	}
	if token.AccessToken == "" {
		return User{}, errors.New("discord: the sign-in returned no token")
	}

	req, err = http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/users/@me", nil)
	if err != nil {
		return User{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token.AccessToken)
	var user User
	if err := c.do(req, "/users/@me", &user); err != nil {
		return User{}, err
	}
	if !ValidID(user.ID) {
		return User{}, errors.New("discord: the sign-in returned no account id")
	}
	return user, nil
}

// bot makes a call as the bot. payload and out may be nil.
func (c *Client) bot(ctx context.Context, method, path string, payload, out any) error {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bot "+c.cfg.BotToken)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.do(req, path, out)
}

func (c *Client) do(req *http.Request, path string, out any) error {
	req.Header.Set("User-Agent", "EveSynapse (https://github.com/natemsz/evesynapse)")
	resp, err := c.http.Do(req)
	if err != nil {
		// The error may carry the request's address, never its headers.
		return fmt.Errorf("discord %s %s: %w", req.Method, path, err)
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
		return se
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("discord %s %s: decode: %w", req.Method, path, err)
	}
	return nil
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

// MemberRoles lists the roles a user holds in the bot's server. A
// user who is not in the server is a 404 (see IsStatus).
func (c *Client) MemberRoles(ctx context.Context, userID string) ([]string, error) {
	if !ValidID(userID) || !ValidID(c.cfg.GuildID) {
		return nil, errors.New("discord: not a user or server id")
	}
	var member struct {
		Roles []string `json:"roles"`
	}
	if err := c.bot(ctx, http.MethodGet, "/guilds/"+c.cfg.GuildID+"/members/"+userID, nil, &member); err != nil {
		return nil, err
	}
	return member.Roles, nil
}

// SetRole gives a user a role in the bot's server, or takes it away.
func (c *Client) SetRole(ctx context.Context, userID, roleID string, held bool) error {
	if !ValidID(userID) || !ValidID(roleID) || !ValidID(c.cfg.GuildID) {
		return errors.New("discord: not a user, role or server id")
	}
	method := http.MethodPut
	if !held {
		method = http.MethodDelete
	}
	return c.bot(ctx, method, "/guilds/"+c.cfg.GuildID+"/members/"+userID+"/roles/"+roleID, nil, nil)
}
