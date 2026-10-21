package app

// ---------------------------------------------------------------------------
// The desktop log watcher's side of the site: posting tokens and the small
// API the watcher (intelwatch, github.com/natemsz/intelwatch) talks to.
//
// An account makes a token on the channels page, names it ("Home PC"),
// and sees it once, inside a ready-made intelwatch.json. Only the token's
// SHA-256 is stored (schema 079), with its first characters to tell
// tokens apart; revoking one stops it at once. A request bearing a token
// ("Authorization: Bearer esw_...") acts as its account, for this API
// only:
//
//	GET  /api/intel/v1/channels              the channels it may post to
//	POST /api/intel/v1/channels/{id}/lines   {"lines": [...]}: chat lines
//
// Lines go through the same reader as pasted ones (postChatLines), so a
// watcher that restarts, or two characters logging one channel, store a
// sighting once. Each token is held to a steady rate.
// ---------------------------------------------------------------------------

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

const (
	intelTokenPrefix  = "esw_"
	intelMaxTokens    = 10
	intelAPIMaxLines  = 500
	intelAPIPerMinute = 120
)

// The watcher's download, per platform: assets of the latest release
// of its own repository, github.com/natemsz/intelwatch.
var intelWatchDownloads = []struct{ Label, File string }{
	{"Windows", "intelwatch-windows-amd64.exe"},
	{"macOS (Apple silicon)", "intelwatch-darwin-arm64"},
	{"macOS (Intel)", "intelwatch-darwin-amd64"},
	{"Linux", "intelwatch-linux-amd64"},
}

const intelWatchReleases = "https://github.com/natemsz/intelwatch/releases/latest/download/"

type intelTokenRow struct {
	ID       int64
	Name     string
	Prefix   string
	Created  string
	LastUsed string
}

type intelDownload struct{ Label, URL string }

// newIntelToken is a fresh token: the prefix and 32 base-62
// characters.
func newIntelToken() (string, error) {
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	b := make([]byte, 32)
	n := big.NewInt(int64(len(alphabet)))
	for i := range b {
		v, err := rand.Int(rand.Reader, n)
		if err != nil {
			return "", err
		}
		b[i] = alphabet[v.Int64()]
	}
	return intelTokenPrefix + string(b), nil
}

func hashIntelToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

// fillWatcher adds the log watcher's part of the channels page: the
// account's tokens and the downloads.
func (app *Application) fillWatcher(ctx context.Context, v *channelListView, userID int64) {
	for _, d := range intelWatchDownloads {
		v.Downloads = append(v.Downloads, intelDownload{Label: d.Label, URL: intelWatchReleases + d.File})
	}
	rows, err := app.queries.ListIntelTokens(ctx, userID)
	if err != nil {
		return
	}
	now := time.Now()
	for _, r := range rows {
		t := intelTokenRow{ID: r.ID, Name: r.Name, Prefix: r.Prefix, Created: r.CreatedAt.UTC().Format("2006-01-02"), LastUsed: "never"}
		if r.LastUsedAt.Valid {
			t.LastUsed = ageText(now, r.LastUsedAt.Time)
		}
		v.Tokens = append(v.Tokens, t)
	}
}

// handleIntelTokenCreate makes a token and shows it once, in the
// watcher's config.
func (app *Application) handleIntelTokenCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	back := app.flashBack(w, r, "/intel/channels/#watcher")
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		name = "Log watcher"
	}
	if utf8.RuneCountInString(name) > 40 {
		back("Name the token in up to 40 characters.")
		return
	}
	if n, err := app.queries.CountIntelTokens(ctx, userID); err == nil && n >= intelMaxTokens {
		back(fmt.Sprintf("An account holds up to %d tokens; revoke one first.", intelMaxTokens))
		return
	}
	token, err := newIntelToken()
	if err == nil {
		_, err = app.queries.CreateIntelToken(ctx, db.CreateIntelTokenParams{UserID: userID, Name: name, Prefix: token[:len(intelTokenPrefix)+4], TokenHash: hashIntelToken(token), CreatedAt: time.Now().UTC()})
	}
	if err != nil {
		logging.Errorf("channels: create token: %v", err)
		back("That did not go through — please try again.")
		return
	}
	data := app.publicPage(ctx)
	v := &channelListView{SignedIn: true}
	id := app.intelIdentity(ctx)
	for _, c := range app.accessibleChannels(ctx, id) {
		v.Channels = append(v.Channels, channelRow{ID: c.ID, Name: c.Name, Description: c.Description, Owner: c.OwnerUserID == id.userID, CanPost: c.CanPost})
	}
	app.fillWatcher(ctx, v, userID)
	v.NewToken = token
	cfg, _ := json.MarshalIndent(map[string]any{"server": app.cfg.publicOrigin(), "token": token, "channels": map[string]int64{}}, "", "  ")
	v.NewConfig = string(cfg)
	data.Channels = v
	w.Header().Set("Cache-Control", "no-store")
	app.render(ctx, w, http.StatusOK, "channels.html", data)
}

func (app *Application) handleIntelTokenRevoke(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	back := app.flashBack(w, r, "/intel/channels/#watcher")
	id, _ := strconv.ParseInt(chi.URLParam(r, "tokenID"), 10, 64)
	if n, err := app.queries.DeleteIntelToken(ctx, db.DeleteIntelTokenParams{ID: id, UserID: app.userID(ctx)}); err != nil || n == 0 {
		back("That token is not yours, or already revoked.")
		return
	}
	back("Token revoked: a watcher using it can no longer post.")
}

// --- The API ---

type intelTokenCtxKey struct{}

// intelLimiter holds each token to intelAPIPerMinute requests a minute.
type intelLimiter struct {
	mu   sync.Mutex
	seen map[int64][]time.Time
}

var intelAPILimit = &intelLimiter{seen: map[int64][]time.Time{}}

func (l *intelLimiter) allow(id int64, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cut := now.Add(-time.Minute)
	keep := l.seen[id][:0]
	for _, t := range l.seen[id] {
		if t.After(cut) {
			keep = append(keep, t)
		}
	}
	if len(keep) >= intelAPIPerMinute {
		l.seen[id] = keep
		return false
	}
	l.seen[id] = append(keep, now)
	return true
}

func intelAPIError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": msg})
}

// intelTokenAuth admits a request bearing a live token, as its
// account.
func (app *Application) intelTokenAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		bearer = strings.TrimSpace(bearer)
		if !ok || !strings.HasPrefix(bearer, intelTokenPrefix) || len(bearer) > 64 {
			intelAPIError(w, http.StatusUnauthorized, "missing or malformed token")
			return
		}
		tok, err := app.queries.GetIntelTokenByHash(r.Context(), hashIntelToken(bearer))
		if err != nil {
			intelAPIError(w, http.StatusUnauthorized, "unknown or revoked token")
			return
		}
		now := time.Now()
		if !intelAPILimit.allow(tok.ID, now) {
			w.Header().Set("Retry-After", "30")
			intelAPIError(w, http.StatusTooManyRequests, "too many requests: slow down")
			return
		}
		_ = app.queries.TouchIntelToken(r.Context(), db.TouchIntelTokenParams{ID: tok.ID, At: timeSet(now)})
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), intelTokenCtxKey{}, tok.UserID)))
	})
}

func tokenUser(ctx context.Context) int64 {
	id, _ := ctx.Value(intelTokenCtxKey{}).(int64)
	return id
}

func (app *Application) handleAPIChannels(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := app.intelIdentityFor(ctx, tokenUser(ctx), 0)
	type channel struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
		Post bool   `json:"post"`
	}
	out := struct {
		OK       bool      `json:"ok"`
		Account  string    `json:"account"`
		Channels []channel `json:"channels"`
	}{OK: true, Account: id.posterName, Channels: []channel{}}
	for _, c := range app.accessibleChannels(ctx, id) {
		out.Channels = append(out.Channels, channel{ID: c.ID, Name: c.Name, Post: c.CanPost})
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(out)
}

func (app *Application) handleAPILines(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := app.intelIdentityFor(ctx, tokenUser(ctx), 0)
	c, ok := app.channelAccess(ctx, id, channelIDParam(r))
	if !ok || !c.CanPost {
		intelAPIError(w, http.StatusForbidden, "this account cannot post to that channel")
		return
	}
	var body struct {
		Lines []string `json:"lines"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, intelMaxPaste)).Decode(&body); err != nil {
		intelAPIError(w, http.StatusBadRequest, "send {\"lines\": [...]}, up to 64 KB")
		return
	}
	if len(body.Lines) > intelAPIMaxLines {
		intelAPIError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("send up to %d lines at a time", intelAPIMaxLines))
		return
	}
	now := time.Now().UTC()
	base := db.InsertIntelReportParams{ChannelID: c.ID, AuthorUserID: id.userID, AuthorCharacterID: id.posterID, Reporter: id.posterName, CreatedAt: now, SeenAt: now}
	for i, l := range body.Lines {
		body.Lines[i] = strings.ReplaceAll(l, "\n", " ")
	}
	res := app.postChatLines(ctx, base, strings.Join(body.Lines, "\n"), "watcher")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(struct {
		OK bool `json:"ok"`
		chatPostResult
	}{true, res})
}
