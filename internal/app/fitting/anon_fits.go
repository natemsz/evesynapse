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
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

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

func isFitShareTokenShape(s string) bool {
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
	if secret, err := app.queries.GetFittingCookieSecret(ctx); err == nil && secret != "" {
		return []byte(secret), nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	secret := hex.EncodeToString(buf)
	if err := app.queries.InsertFittingCookieSecret(ctx, secret); err != nil {
		return nil, err
	}
	stored, err := app.queries.GetFittingCookieSecret(ctx)
	if err != nil {
		return nil, err
	}
	return []byte(stored), nil
}

func signFitValue(secret []byte, payload string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(payload))
	return payload + "." + hex.EncodeToString(mac.Sum(nil))
}

func verifyFitValue(secret []byte, value string) (string, bool) {
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

// anonFitTokens reads the request's verified fit tokens.
func (app *tool) anonFitTokens(ctx context.Context, r *http.Request) []string {
	c, err := r.Cookie(anonFitCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	secret, err := app.fitHistorySecret(ctx)
	if err != nil {
		logging.Errorf("fittings: anon fit secret: %v", err)
		return nil
	}
	payload, ok := verifyFitValue(secret, c.Value)
	if !ok {
		return nil
	}
	var out []string
	for _, tok := range strings.Split(payload, ".") {
		if isFitShareTokenShape(tok) {
			out = append(out, tok)
		}
	}
	return out
}

func (app *tool) writeAnonFitCookie(ctx context.Context, w http.ResponseWriter, tokens []string) {
	if len(tokens) > anonFitCap {
		tokens = tokens[:anonFitCap]
	}
	secret, err := app.fitHistorySecret(ctx)
	if err != nil {
		logging.Errorf("fittings: anon fit secret: %v", err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     anonFitCookie,
		Value:    signFitValue(secret, strings.Join(tokens, ".")),
		Path:     "/",
		MaxAge:   int(anonFitTTL / time.Second),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func (app *tool) rememberAnonFit(ctx context.Context, w http.ResponseWriter, r *http.Request, tok string) {
	tokens := []string{tok}
	for _, t := range app.anonFitTokens(ctx, r) {
		if t != tok {
			tokens = append(tokens, t)
		}
	}
	app.writeAnonFitCookie(ctx, w, tokens)
}

func (app *tool) forgetAnonFit(ctx context.Context, w http.ResponseWriter, r *http.Request, tok string) {
	var tokens []string
	for _, t := range app.anonFitTokens(ctx, r) {
		if t != tok {
			tokens = append(tokens, t)
		}
	}
	app.writeAnonFitCookie(ctx, w, tokens)
}

// hasAnonFit reports whether the request's verified history
// holds tok: the cookie-holder is the fit's creator.
func (app *tool) hasAnonFit(ctx context.Context, r *http.Request, tok string) bool {
	for _, t := range app.anonFitTokens(ctx, r) {
		if t == tok {
			return true
		}
	}
	return false
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
	tokens := app.anonFitTokens(ctx, r)
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
	app.rememberAnonFit(ctx, w, r, token)
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
	if app.hasAnonFit(ctx, r, token) {
		if _, err := app.queries.DeleteAnonymousFittingByToken(ctx, token); err != nil {
			logging.Errorf("fittings: delete anonymous fit: %v", err)
		}
		app.forgetAnonFit(ctx, w, r, token)
	}
	http.Redirect(w, r, "/fittings/", http.StatusSeeOther)
}

// pruneExpiredAnonymousFits is the worker sweep's job.
func (app *tool) pruneExpiredAnonymousFits(ctx context.Context, now time.Time) (int64, error) {
	return app.queries.DeleteExpiredAnonymousFittings(ctx, sql.NullTime{Time: now.Add(-anonFitTTL), Valid: true})
}
