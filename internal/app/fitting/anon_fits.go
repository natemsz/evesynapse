package fitting

// Anonymous fit sharing: an anonymous user can save a fit
// from the workbench and receives an unguessable share link.
// The fit is stored under a shared anonymous owner, is never
// listed in the public browser (is_public stays false), and
// renders read-only at /fittings/share/{token}/. A signed,
// HttpOnly cookie in the same style as the appraisal history
// cookie remembers the creator's fits, offers their delete
// control, and anonymous fits fall off 180 days after their
// last view (the fitting module's worker sweep).

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	json "github.com/goccy/go-json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/fitview"
	"evesynapse/internal/logging"
)

// anonFitCookie names the anonymous-fit history cookie.
const anonFitCookie = "evesynapse-fits"

// anonFitCap bounds the remembered fit tokens, newest first.
const anonFitCap = 20

// anonFitTTL is how long an anonymous fit lives after its
// last view before the sweep deletes it.
const anonFitTTL = 180 * 24 * time.Hour

func newFitShareToken() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// fitHistorySecret returns the per-install signing secret,
// creating and storing one on first use.
func (app *tool) fitHistorySecret(ctx context.Context) ([]byte, error) {
	return module.CookieSecret(ctx, app.queries.GetFittingCookieSecret, app.queries.InsertFittingCookieSecret)
}

// anonFits is the anonymous fit history cookie.
func (app *tool) anonFits() module.TokenHistory {
	return module.TokenHistory{
		Cookie: anonFitCookie,
		Cap:    anonFitCap,
		TTL:    anonFitTTL,
		What:   "fittings: anon fit",
		Secret: app.fitHistorySecret,
	}
}

// anonFitOwnerID returns the shared anonymous owner's user id.
func (app *tool) anonFitOwnerID(ctx context.Context) (int64, error) {
	if id, err := app.queries.GetFittingAnonymousOwner(ctx); err == nil && id > 0 {
		return id, nil
	}
	user, err := app.queries.CreateUser(ctx)
	if err != nil {
		return 0, err
	}
	if err := app.queries.InsertFittingAnonymousOwner(ctx, user.ID); err != nil {
		return 0, err
	}
	return app.queries.GetFittingAnonymousOwner(ctx)
}

// anonFitEntry is one of the creator's anonymous fits, for
// the workbench history list.
type anonFitEntry struct {
	Name      string
	ShipName  string
	ShareURL  string
	DeleteURL string
	Updated   string
}

// anonFitHistory shapes the cookie's anonymous fits in cookie
// order, skipping tokens whose rows are gone.
func (app *tool) anonFitHistory(ctx context.Context, r *http.Request) []anonFitEntry {
	tokens := app.anonFits().Tokens(ctx, r)
	if len(tokens) == 0 {
		return nil
	}
	rows, err := app.queries.ListFittingsByShareTokens(ctx, tokens)
	if err != nil {
		logging.Errorf("fittings: anon fit history: %v", err)
		return nil
	}
	byToken := make(map[string]db.ListFittingsByShareTokensRow, len(rows))
	for _, row := range rows {
		byToken[row.ShareToken] = row
	}
	out := make([]anonFitEntry, 0, len(tokens))
	for _, tok := range tokens {
		row, ok := byToken[tok]
		if !ok {
			continue
		}
		out = append(out, anonFitEntry{
			Name:      row.Name,
			ShipName:  app.deps.TypeName(ctx, row.ShipTypeID),
			ShareURL:  "/fittings/share/" + tok + "/",
			DeleteURL: "/fittings/share/" + tok + "/delete",
			Updated:   row.UpdatedAt.Format("Jan 2, 2006"),
		})
	}
	return out
}

// handleFitSaveAnonymous saves the posted fit for an
// anonymous user and answers with its share link.
func (app *tool) handleFitSaveAnonymous(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Tags        string         `json:"tags"`
		Fit         fitview.FitDoc `json:"fit"`
	}
	body := http.MaxBytesReader(w, r.Body, 256<<10)
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		http.Error(w, "That fitting couldn't be read.", http.StatusBadRequest)
		return
	}
	fitview.SanitizeFitDoc(&req.Fit)
	if req.Fit.ShipTypeID <= 0 {
		http.Error(w, "Pick a ship before sharing the fit.", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = strings.TrimSpace(req.Fit.Name)
	}
	if name == "" {
		name = "Unnamed fit"
	}
	if len(name) > 120 {
		name = name[:120]
	}
	description := strings.TrimSpace(req.Description)
	if len(description) > 500 {
		description = description[:500]
	}
	req.Fit.Name = name
	req.Fit.Description = description
	req.Fit.Tags = fitview.ParseFitTags(req.Tags)
	raw, err := json.Marshal(req.Fit)
	if err != nil {
		http.Error(w, "That fitting couldn't be saved.", http.StatusBadRequest)
		return
	}
	ownerID, err := app.anonFitOwnerID(ctx)
	if err != nil {
		logging.Errorf("fittings: anon owner: %v", err)
		http.Error(w, "That fitting couldn't be saved.", http.StatusInternalServerError)
		return
	}
	now := time.Now().UTC()
	row, err := app.queries.CreateLocalFitting(ctx, db.CreateLocalFittingParams{
		UserID: ownerID, Name: name, ShipTypeID: req.Fit.ShipTypeID,
		ItemsJson: string(raw), IsPublic: false, IsDraft: false,
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		logging.Errorf("fittings: anon create: %v", err)
		http.Error(w, "That fitting couldn't be saved.", http.StatusInternalServerError)
		return
	}
	token, err := newFitShareToken()
	if err != nil {
		http.Error(w, "That fitting couldn't be saved.", http.StatusInternalServerError)
		return
	}
	if err := app.queries.SetAnonymousFittingShare(ctx, db.SetAnonymousFittingShareParams{ID: row.ID, ShareToken: token}); err != nil {
		logging.Errorf("fittings: anon share %d: %v", row.ID, err)
		http.Error(w, "That fitting couldn't be saved.", http.StatusInternalServerError)
		return
	}
	if err := app.queries.SetLocalFittingPrice(ctx, db.SetLocalFittingPriceParams{
		TotalPrice: app.deps.FitBuilder().FitDocPrice(ctx, &req.Fit), ID: row.ID, UserID: ownerID,
	}); err != nil {
		logging.Errorf("fittings: anon price %d: %v", row.ID, err)
	}
	app.anonFits().Remember(ctx, w, r, token)
	writeFitJSON(w, map[string]any{
		"id": row.ID, "shareToken": token,
		"shareUrl": "/fittings/share/" + token + "/",
	})
}

// handleFitDeleteAnonymous deletes an anonymous fit for the
// history-cookie holder who created it.
func (app *tool) handleFitDeleteAnonymous(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	token := chi.URLParam(r, "token")
	if app.anonFits().Has(ctx, r, token) {
		if _, err := app.queries.DeleteAnonymousFittingByToken(ctx, token); err != nil {
			logging.Errorf("fittings: delete anonymous fit: %v", err)
		}
		app.anonFits().Forget(ctx, w, r, token)
	}
	http.Redirect(w, r, "/fittings/", http.StatusSeeOther)
}

// pruneExpiredAnonymousFits is the worker sweep's job.
func (app *tool) pruneExpiredAnonymousFits(ctx context.Context, now time.Time) (int64, error) {
	return app.queries.DeleteExpiredAnonymousFittings(ctx, sql.NullTime{Time: now.Add(-anonFitTTL), Valid: true})
}
