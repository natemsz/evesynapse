package appraise

// Anonymous appraisal history: a signed, HttpOnly cookie that
// remembers the share tokens an anonymous visitor created, so
// the appraise page can show their recent appraisals and the
// appraisal view can offer them its delete control. The cookie
// is HMAC-signed with a per-install secret kept in the database
// (schema 064): a token the holder was never given cannot be
// added by editing the cookie, and a tampered cookie verifies
// as no history at all rather than erroring.

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"evesynapse/internal/logging"
)

// anonHistoryCookie names the anonymous-history cookie.
const anonHistoryCookie = "evesynapse-appraisals"

// anonHistoryCap bounds the remembered tokens, newest first.
const anonHistoryCap = 20

// anonAppraisalTTL is how long an anonymous appraisal lives
// after its last view before the worker sweep deletes it.
const anonAppraisalTTL = 180 * 24 * time.Hour

// isShareTokenShape reports whether s looks like a share token
// (48 lowercase hex characters from newShareToken).
func isShareTokenShape(s string) bool {
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

// anonHistorySecret returns the per-install signing secret,
// creating and storing one on first use.
func (app *page) anonHistorySecret(ctx context.Context) ([]byte, error) {
	if secret, err := app.queries.GetAppraisalCookieSecret(ctx); err == nil && secret != "" {
		return []byte(secret), nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	secret := hex.EncodeToString(buf)
	if err := app.queries.InsertAppraisalCookieSecret(ctx, secret); err != nil {
		return nil, err
	}
	stored, err := app.queries.GetAppraisalCookieSecret(ctx)
	if err != nil {
		return nil, err
	}
	return []byte(stored), nil
}

// signValue signs one payload: payload + "." + HMAC hex.
func signValue(secret []byte, payload string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(payload))
	return payload + "." + hex.EncodeToString(mac.Sum(nil))
}

// verifySignedValue returns a signed value's payload when
// its signature checks out.
func verifySignedValue(secret []byte, value string) (string, bool) {
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

func signAnonTokens(secret []byte, tokens []string) string {
	return signValue(secret, strings.Join(tokens, "."))
}

func verifyAnonTokens(secret []byte, value string) []string {
	payload, ok := verifySignedValue(secret, value)
	if !ok {
		return nil
	}
	var out []string
	for _, tok := range strings.Split(payload, ".") {
		if isShareTokenShape(tok) {
			out = append(out, tok)
		}
	}
	return out
}

// anonTokens reads the request's verified history tokens,
// newest first. No cookie, a bad signature, or an unavailable
// secret all read as no history.
func (app *page) anonTokens(ctx context.Context, r *http.Request) []string {
	c, err := r.Cookie(anonHistoryCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	secret, err := app.anonHistorySecret(ctx)
	if err != nil {
		logging.Errorf("appraisal: anon history secret: %v", err)
		return nil
	}
	return verifyAnonTokens(secret, c.Value)
}

// writeAnonCookie stores the tokens as the history cookie.
func (app *page) writeAnonCookie(ctx context.Context, w http.ResponseWriter, tokens []string) {
	if len(tokens) > anonHistoryCap {
		tokens = tokens[:anonHistoryCap]
	}
	secret, err := app.anonHistorySecret(ctx)
	if err != nil {
		logging.Errorf("appraisal: anon history secret: %v", err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     anonHistoryCookie,
		Value:    signAnonTokens(secret, tokens),
		Path:     "/",
		MaxAge:   int(anonAppraisalTTL / time.Second),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// rememberAnonToken returns the request's tokens with tok
// moved to the front, capped, and writes the updated cookie.
func (app *page) rememberAnonToken(ctx context.Context, w http.ResponseWriter, r *http.Request, tok string) {
	tokens := []string{tok}
	for _, t := range app.anonTokens(ctx, r) {
		if t != tok {
			tokens = append(tokens, t)
		}
	}
	app.writeAnonCookie(ctx, w, tokens)
}

// forgetAnonToken drops tok from the history cookie.
func (app *page) forgetAnonToken(ctx context.Context, w http.ResponseWriter, r *http.Request, tok string) {
	var tokens []string
	for _, t := range app.anonTokens(ctx, r) {
		if t != tok {
			tokens = append(tokens, t)
		}
	}
	app.writeAnonCookie(ctx, w, tokens)
}

// hasAnonToken reports whether the request's verified history
// holds tok: the cookie-holder is the appraisal's creator.
func (app *page) hasAnonToken(ctx context.Context, r *http.Request, tok string) bool {
	for _, t := range app.anonTokens(ctx, r) {
		if t == tok {
			return true
		}
	}
	return false
}

// anonymousOwnerID returns the shared anonymous owner's user
// id, creating the owner account on first use.
func (app *page) anonymousOwnerID(ctx context.Context) (int64, error) {
	if id, err := app.queries.GetAppraisalAnonymousOwner(ctx); err == nil && id > 0 {
		return id, nil
	}
	user, err := app.queries.CreateUser(ctx)
	if err != nil {
		return 0, err
	}
	if err := app.queries.InsertAppraisalAnonymousOwner(ctx, user.ID); err != nil {
		return 0, err
	}
	return app.queries.GetAppraisalAnonymousOwner(ctx)
}

// pruneExpiredAnonymousAppraisals deletes anonymous appraisals
// unviewed for longer than the TTL. Signed-in appraisals are
// never touched. It is the worker sweep's whole job, exported
// within the package for the module's worker registration and
// for tests.
func (app *page) pruneExpiredAnonymousAppraisals(ctx context.Context, now time.Time) (int64, error) {
	return app.queries.DeleteExpiredAnonymousAppraisals(ctx, now.Add(-anonAppraisalTTL))
}
