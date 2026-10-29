package app

// Market API keys on the Settings page (the API tab). A player makes a
// named key and sees it once; only its SHA-256 is kept. Making a key
// is held to the same bar as other security actions (a completed
// two-factor sign-in, and a fresh one when the account has keys), at
// most apiMaxKeysPerUser live and apiMaxKeysPerDay made in a day.
// Revoking a key stops it at once.

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

type apiKeyRow struct {
	ID       int64
	Name     string
	Prefix   string
	Created  string
	LastUsed string
	Today    string
	Quota    string
}

type apiKeysView struct {
	Keys      []apiKeyRow
	NewKey    string // shown once, right after it is made
	Error     string
	AtLimit   bool
	BaseURL   string
	PerMinute int
	MaxBatch  int
}

type newAPIKeyCtx struct{}

// attachAPIKeys fills the Settings page's API tab.
func (app *Application) attachAPIKeys(ctx context.Context, data *pageData, userID int64) {
	view := &apiKeysView{PerMinute: marketAPIPerMinute, MaxBatch: marketAPIMaxBatch, BaseURL: app.marketAPIBaseURL()}
	if v, ok := ctx.Value(newAPIKeyCtx{}).([2]string); ok {
		view.NewKey, view.Error = v[0], v[1]
	}
	rows, err := app.queries.ListAPIKeys(ctx, db.ListAPIKeysParams{UserID: userID, Day: time.Now().UTC().Format("2006-01-02")})
	if err != nil {
		logging.Errorf("api keys: list for user %d: %v", userID, err)
	}
	now := time.Now()
	for _, k := range rows {
		row := apiKeyRow{ID: k.ID, Name: k.Name, Prefix: k.Prefix + "…", Created: ageText(now, k.CreatedAt),
			LastUsed: "Never", Today: esi.FormatInt(max(k.TodayRequests, marketAPIUsage.used(k.ID))), Quota: esi.FormatInt(k.DailyQuota)}
		if k.LastUsedAt.Valid {
			row.LastUsed = ageText(now, k.LastUsedAt.Time)
		}
		view.Keys = append(view.Keys, row)
	}
	view.AtLimit = len(rows) >= apiMaxKeysPerUser
	data.APIKeys = view
}

// marketAPIBaseURL is where the API answers: its own host when one is
// set, else the site's.
func (app *Application) marketAPIBaseURL() string {
	if h := app.cfg.marketAPIHost; h != "" {
		return "https://" + h + "/api/market/v1"
	}
	return app.cfg.publicOrigin() + "/api/market/v1"
}

func (app *Application) handleAPIKeyCreate(w http.ResponseWriter, r *http.Request) {
	ctx, userID, ok := app.securityAuthed(w, r, false)
	if !ok {
		return
	}
	if app.securityNeedsFresh(w, r, ctx, userID, false) {
		return
	}
	show := func(newKey, msg string) {
		app.handleSettings(w, r.WithContext(context.WithValue(ctx, newAPIKeyCtx{}, [2]string{newKey, msg})))
	}
	if err := r.ParseForm(); err != nil {
		show("", "That form didn't come through. Try again.")
		return
	}
	name := strings.TrimSpace(r.Form.Get("name"))
	if name == "" || len([]rune(name)) > 40 {
		show("", "Give the key a name of up to 40 characters.")
		return
	}
	if n, err := app.queries.CountLiveAPIKeys(ctx, userID); err != nil || n >= apiMaxKeysPerUser {
		show("", "You have the most keys one account can hold. Revoke one first.")
		return
	}
	if n, err := app.queries.CountAPIKeysCreatedSince(ctx, db.CountAPIKeysCreatedSinceParams{UserID: userID, Since: time.Now().UTC().Add(-24 * time.Hour)}); err != nil || n >= apiMaxKeysPerDay {
		show("", "You've made as many keys as you can today. Try again tomorrow.")
		return
	}
	key, err := newAPIKey()
	if err != nil {
		show("", "Couldn't make a key right now.")
		return
	}
	if _, err := app.queries.CreateAPIKey(ctx, db.CreateAPIKeyParams{
		UserID: userID, Name: name, Prefix: key[:len(apiKeyPrefix)+6], KeyHash: hashAPIKey(key),
	}); err != nil {
		logging.Errorf("api keys: create for user %d: %v", userID, err)
		show("", "Couldn't make a key right now.")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	show(key, "")
}

func (app *Application) handleAPIKeyRevoke(w http.ResponseWriter, r *http.Request) {
	ctx, userID, ok := app.securityAuthed(w, r, false)
	if !ok {
		return
	}
	if err := r.ParseForm(); err == nil {
		if id, err := strconv.ParseInt(r.Form.Get("id"), 10, 64); err == nil && id > 0 {
			if _, err := app.queries.RevokeAPIKey(ctx, db.RevokeAPIKeyParams{ID: id, UserID: userID}); err != nil {
				logging.Errorf("api keys: revoke %d: %v", id, err)
			}
		}
	}
	http.Redirect(w, r, "/settings#api", http.StatusSeeOther)
}
