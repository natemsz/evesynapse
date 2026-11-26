package module

// Anonymous history cookies: a signed, HttpOnly cookie that remembers
// the share tokens an anonymous visitor created (appraisals, fits), so
// a page can list them and offer their delete control. The cookie is
// HMAC-signed with a per-install secret kept in the database: a token
// the holder was never given cannot be added by editing the cookie,
// and a tampered cookie verifies as no history at all rather than
// erroring.

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"slices"
	"strings"
	"time"

	"evesynapse/internal/logging"
)

// IsShareTokenShape reports whether s looks like a share token: 48
// lowercase hex characters.
func IsShareTokenShape(s string) bool {
	if len(s) != 48 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// CookieSecret returns a per-install signing secret, creating and
// storing a random one on first use. get reads the stored secret
// ("" or an error when there is none); insert stores a new one
// without replacing an existing one, and the stored value is read
// back so two first uses agree.
func CookieSecret(ctx context.Context, get func(context.Context) (string, error), insert func(context.Context, string) error) ([]byte, error) {
	if secret, err := get(ctx); err == nil && secret != "" {
		return []byte(secret), nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	if err := insert(ctx, hex.EncodeToString(buf)); err != nil {
		return nil, err
	}
	stored, err := get(ctx)
	if err != nil {
		return nil, err
	}
	return []byte(stored), nil
}

// SignValue signs one payload: payload + "." + HMAC-SHA256 hex.
func SignValue(secret []byte, payload string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(payload))
	return payload + "." + hex.EncodeToString(mac.Sum(nil))
}

// VerifySignedValue returns a signed value's payload when its
// signature checks out.
func VerifySignedValue(secret []byte, value string) (string, bool) {
	i := strings.LastIndex(value, ".")
	if i < 0 {
		return "", false
	}
	payload, sig := value[:i], value[i+1:]
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(payload))
	if !hmac.Equal([]byte(hex.EncodeToString(mac.Sum(nil))), []byte(sig)) {
		return "", false
	}
	return payload, true
}

// SignTokens signs a token list as one cookie value.
func SignTokens(secret []byte, tokens []string) string {
	return SignValue(secret, strings.Join(tokens, "."))
}

// VerifyTokens returns the share tokens a signed cookie value holds;
// a bad signature reads as none, and anything not shaped like a share
// token is dropped.
func VerifyTokens(secret []byte, value string) []string {
	payload, ok := VerifySignedValue(secret, value)
	if !ok {
		return nil
	}
	var out []string
	for tok := range strings.SplitSeq(payload, ".") {
		if IsShareTokenShape(tok) {
			out = append(out, tok)
		}
	}
	return out
}

// TokenHistory is one anonymous history cookie.
type TokenHistory struct {
	Cookie string        // the cookie's name
	Cap    int           // the most tokens remembered, newest first
	TTL    time.Duration // the cookie's lifetime
	What   string        // log prefix for a secret that cannot be read
	Secret func(context.Context) ([]byte, error)
}

// Tokens reads the request's verified tokens, newest first. No
// cookie, a bad signature, or an unavailable secret all read as no
// history.
func (h TokenHistory) Tokens(ctx context.Context, r *http.Request) []string {
	c, err := r.Cookie(h.Cookie)
	if err != nil || c.Value == "" {
		return nil
	}
	secret, err := h.Secret(ctx)
	if err != nil {
		logging.Errorf("%s secret: %v", h.What, err)
		return nil
	}
	return VerifyTokens(secret, c.Value)
}

// write stores tokens, capped, as the signed cookie.
func (h TokenHistory) write(ctx context.Context, w http.ResponseWriter, tokens []string) {
	if len(tokens) > h.Cap {
		tokens = tokens[:h.Cap]
	}
	secret, err := h.Secret(ctx)
	if err != nil {
		logging.Errorf("%s secret: %v", h.What, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     h.Cookie,
		Value:    SignTokens(secret, tokens),
		Path:     "/",
		MaxAge:   int(h.TTL / time.Second),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// Remember moves tok to the front of the request's history and
// writes the updated cookie.
func (h TokenHistory) Remember(ctx context.Context, w http.ResponseWriter, r *http.Request, tok string) {
	tokens := []string{tok}
	for _, t := range h.Tokens(ctx, r) {
		if t != tok {
			tokens = append(tokens, t)
		}
	}
	h.write(ctx, w, tokens)
}

// Forget drops tok from the history cookie.
func (h TokenHistory) Forget(ctx context.Context, w http.ResponseWriter, r *http.Request, tok string) {
	var tokens []string
	for _, t := range h.Tokens(ctx, r) {
		if t != tok {
			tokens = append(tokens, t)
		}
	}
	h.write(ctx, w, tokens)
}

// Has reports whether the request's verified history holds tok: the
// cookie holder created it.
func (h TokenHistory) Has(ctx context.Context, r *http.Request, tok string) bool {
	return slices.Contains(h.Tokens(ctx, r), tok)
}
