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
	"time"

	"evesynapse/internal/app/module"
)

// anonHistoryCookie names the anonymous-history cookie.
const anonHistoryCookie = "evesynapse-appraisals"

// anonHistoryCap bounds the remembered tokens, newest first.
const anonHistoryCap = 20

// anonAppraisalTTL is how long an anonymous appraisal lives
// after its last view before the worker sweep deletes it.
const anonAppraisalTTL = 180 * 24 * time.Hour

// anonHistorySecret returns the per-install signing secret,
// creating and storing one on first use.
func (app *page) anonHistorySecret(ctx context.Context) ([]byte, error) {
	return module.CookieSecret(ctx, app.queries.GetAppraisalCookieSecret, app.queries.InsertAppraisalCookieSecret)
}

// anonHistory is the anonymous appraisal history cookie.
func (app *page) anonHistory() module.TokenHistory {
	return module.TokenHistory{
		Cookie: anonHistoryCookie,
		Cap:    anonHistoryCap,
		TTL:    anonAppraisalTTL,
		What:   "appraisal: anon history",
		Secret: app.anonHistorySecret,
	}
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
