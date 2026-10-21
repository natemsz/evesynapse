package app

// ---------------------------------------------------------------------------
// EveSynapse Axon's side of the site: posting tokens and the small
// API Axon (github.com/natemsz/axon) talks to.
//
// An account makes a token on the channels page, names it ("Home PC"),
// and sees it once, inside a ready-made axon.json. Only the token's
// SHA-256 is stored (schema 079), with its first characters to tell
// tokens apart; revoking one stops it at once. A request bearing a token
// ("Authorization: Bearer esw_...") acts as its account, for this API
// only:
//
//	GET  /api/intel/v1/channels              the channels it may post to
//	POST /api/intel/v1/channels/{id}/lines   {"lines": [...]}: chat lines
//
// Lines go through the same reader as pasted ones (postChatLines), so an
// Axon that restarts, or two characters logging one channel, store a
// sighting once. Each token is held to a steady rate.
// ---------------------------------------------------------------------------

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
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

// Axon's downloads, assets of the latest release of
// github.com/natemsz/axon. Each program is its own installer: run from
// a download, it installs itself for the player and opens its window,
// and a newer download updates it in place. Windows first.
var axonDownloads = []struct{ Label, File string }{
	{"Windows (installs itself)", "axon-windows-amd64.exe"},
	{"macOS, Apple silicon", "axon-darwin-arm64"},
	{"macOS, Intel", "axon-darwin-amd64"},
	{"Linux", "axon-linux-amd64"},
}

const axonReleases = "https://github.com/natemsz/axon/releases/latest/download/"

// AxonOpenURL opens Axon's window on the player's own computer: Axon
// registers the evesynapse-axon link type when it installs, and starts
// itself if it isn't running. The channels page and the Intel menu
// (base.html) link there for accounts with a paired device.
const AxonOpenURL = "evesynapse-axon://open"

type intelTokenRow struct {
	ID       int64
	Name     string
	Prefix   string
	Created  string
	LastUsed string
	Device   string
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

// fillAxon adds EveSynapse Axon's part of the channels page: the
// account's tokens and the downloads.
func (app *Application) fillAxon(ctx context.Context, v *channelListView, userID int64) {
	v.SiteAddr = app.cfg.publicOrigin()
	for _, d := range axonDownloads {
		v.Downloads = append(v.Downloads, intelDownload{Label: d.Label, URL: axonReleases + d.File})
	}
	rows, err := app.queries.ListIntelTokens(ctx, userID)
	if err != nil {
		return
	}
	now := time.Now()
	for _, r := range rows {
		t := intelTokenRow{ID: r.ID, Name: r.Name, Prefix: r.Prefix, Created: r.CreatedAt.UTC().Format("2006-01-02"), LastUsed: "never", Device: r.Device}
		if r.LastUsedAt.Valid {
			t.LastUsed = ageText(now, r.LastUsedAt.Time)
		}
		v.Tokens = append(v.Tokens, t)
	}
}

// pairingCodeTTL is how long a pairing code stays good: long
// enough to walk from the site to Axon, short enough to leak little.
const pairingCodeTTL = 10 * time.Minute

// newPairingCode is a fresh code in two readable groups, e.g.
// "K7QX-4MZD". Only its hash is stored.
func newPairingCode() (string, error) {
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	b := make([]byte, 8)
	n := big.NewInt(int64(len(alphabet)))
	for i := range b {
		v, err := rand.Int(rand.Reader, n)
		if err != nil {
			return "", err
		}
		b[i] = alphabet[v.Int64()]
	}
	return string(b[:4]) + "-" + string(b[4:]), nil
}

func normalizePairingCode(s string) string {
	return strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(s), " ", ""))
}

// handlePairingCodeCreate makes a one-time pairing code and shows
// it once. Axon exchanges it (with its device public key) for a
// posting credential at POST /api/intel/v1/pair; no token is ever
// shown or pasted any more.
func (app *Application) handlePairingCodeCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	back := app.flashBack(w, r, "/intel/channels/#axon")
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		name = "EveSynapse Axon"
	}
	if utf8.RuneCountInString(name) > 40 {
		back("Name the device in up to 40 characters.")
		return
	}
	if n, err := app.queries.CountIntelTokens(ctx, userID); err == nil && n >= intelMaxTokens {
		back(fmt.Sprintf("An account holds up to %d paired devices; revoke one first.", intelMaxTokens))
		return
	}
	code, err := newPairingCode()
	now := time.Now().UTC()
	if err == nil {
		_, err = app.queries.CreateIntelPairingCode(ctx, db.CreateIntelPairingCodeParams{UserID: userID, Name: name, CodeHash: hashIntelToken(normalizePairingCode(code)), CreatedAt: now, ExpiresAt: now.Add(pairingCodeTTL)})
	}
	if err != nil {
		logging.Errorf("channels: create pairing code: %v", err)
		back("That did not go through — please try again.")
		return
	}
	data := app.publicPage(ctx)
	v := &channelListView{SignedIn: true}
	id := app.intelIdentity(ctx)
	for _, c := range app.accessibleChannels(ctx, id) {
		v.Channels = append(v.Channels, channelRow{ID: c.ID, Name: c.Name, Description: c.Description, Owner: c.OwnerUserID == id.userID, CanPost: c.CanPost})
	}
	app.fillAxon(ctx, v, userID)
	v.PairingCode = code
	data.Channels = v
	w.Header().Set("Cache-Control", "no-store")
	app.render(ctx, w, http.StatusOK, "channels.html", data)
}

// handleAPIPair exchanges a pairing code for a device credential.
// It is the one Axon API call without a credential: the code is
// single-use and short-lived, and the returned token only ever
// works signed by the private half of the public key sent here.
func (app *Application) handleAPIPair(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var in struct {
		Code      string `json:"code"`
		PublicKey string `json:"public_key"`
		Device    string `json:"device"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil {
		intelAPIError(w, http.StatusBadRequest, "send {\"code\", \"public_key\", \"device\"}")
		return
	}
	pub, err := base64.StdEncoding.DecodeString(in.PublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		intelAPIError(w, http.StatusBadRequest, "public_key must be a base64 Ed25519 public key")
		return
	}
	device := strings.TrimSpace(in.Device)
	if device == "" || utf8.RuneCountInString(device) > 80 {
		intelAPIError(w, http.StatusBadRequest, "name this device (up to 80 characters)")
		return
	}
	row, err := app.queries.GetIntelPairingCodeByHash(ctx, hashIntelToken(normalizePairingCode(in.Code)))
	if err != nil || row.UsedAt.Valid {
		intelAPIError(w, http.StatusUnauthorized, "unknown, used or expired pairing code")
		return
	}
	now := time.Now().UTC()
	if n, err := app.queries.UseIntelPairingCode(ctx, db.UseIntelPairingCodeParams{ID: row.ID, At: timeSet(now)}); err != nil || n == 0 {
		intelAPIError(w, http.StatusUnauthorized, "unknown, used or expired pairing code")
		return
	}
	token, err := newIntelToken()
	if err != nil {
		intelAPIError(w, http.StatusInternalServerError, "try again")
		return
	}
	if _, err := app.queries.CreateIntelToken(ctx, db.CreateIntelTokenParams{UserID: row.UserID, Name: row.Name, Prefix: token[:len(intelTokenPrefix)+4], TokenHash: hashIntelToken(token), CreatedAt: now, PublicKey: in.PublicKey, Device: device}); err != nil {
		logging.Errorf("pair: create token: %v", err)
		intelAPIError(w, http.StatusInternalServerError, "try again")
		return
	}
	id := app.intelIdentityFor(ctx, row.UserID, 0)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "token": token, "account": id.posterName})
}

func (app *Application) handleIntelTokenRevoke(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	back := app.flashBack(w, r, "/intel/channels/#axon")
	id, _ := strconv.ParseInt(chi.URLParam(r, "tokenID"), 10, 64)
	if n, err := app.queries.DeleteIntelToken(ctx, db.DeleteIntelTokenParams{ID: id, UserID: app.userID(ctx)}); err != nil || n == 0 {
		back("That token is not yours, or already revoked.")
		return
	}
	back("Token revoked: an Axon using it can no longer post.")
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

// axonSignatureText is what Axon signs and the site checks, in
// both repos (axon watch.SignText): the method, the path, the
// timestamp and the body's hash, so a captured request cannot be
// replayed later or pointed elsewhere.
func axonSignatureText(method, path, timestamp string, body []byte) string {
	sum := sha256.Sum256(body)
	return method + "\n" + path + "\n" + timestamp + "\n" + hex.EncodeToString(sum[:])
}

// intelTokenAuth admits a request bearing a live device credential
// signed by its paired private key, as its account. A token alone
// (copied, or issued before pairing) is not enough.
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
		pub, err := base64.StdEncoding.DecodeString(tok.PublicKey)
		if err != nil || len(pub) != ed25519.PublicKeySize {
			intelAPIError(w, http.StatusUnauthorized, "this credential is not paired: pair Axon again from the Channels page")
			return
		}
		ts := r.Header.Get("X-Axon-Timestamp")
		at, err := strconv.ParseInt(ts, 10, 64)
		if err != nil {
			intelAPIError(w, http.StatusUnauthorized, "missing X-Axon-Timestamp")
			return
		}
		var body []byte
		if r.Body != nil {
			body, err = io.ReadAll(http.MaxBytesReader(w, r.Body, intelMaxPaste))
			if err != nil {
				intelAPIError(w, http.StatusBadRequest, "unreadable body")
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		sig, err := base64.StdEncoding.DecodeString(r.Header.Get("X-Axon-Signature"))
		if err != nil || !ed25519.Verify(ed25519.PublicKey(pub), []byte(axonSignatureText(r.Method, r.URL.RequestURI(), ts, body)), sig) {
			intelAPIError(w, http.StatusUnauthorized, "bad signature")
			return
		}
		now := time.Now()
		if d := now.Unix() - at; d > 300 || d < -300 {
			intelAPIError(w, http.StatusUnauthorized, "signed too long ago: check this computer's clock")
			return
		}
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
	res := app.postChatLines(ctx, base, strings.Join(body.Lines, "\n"), "axon")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(struct {
		OK bool `json:"ok"`
		chatPostResult
	}{true, res})
}
