package app

// Anonymous fit sharing: an anonymous user saves a fit from
// the workbench, gets a link-only share page (never listed in
// the public browser), remembers it through a signed cookie,
// and can delete it; unviewed anonymous fits expire.

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func postAnonFit(t *testing.T, app *Application, cookies []*http.Cookie, path, body string) (int, string, http.Header) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	out, _ := io.ReadAll(rec.Result().Body)
	return rec.Code, string(out), rec.Header()
}

func TestAnonymousFitSharing(t *testing.T) {
	tr := &countingTransport{}
	app, conn, _ := buildCorpTestApp(t, tr)
	ctx := context.Background()
	seedFitUniverse(t, func(stmt string) error {
		_, err := conn.ExecContext(ctx, stmt)
		return err
	})
	app.cfg.eveCallbackURL = "https://node.example.org/auth/callback"

	code, body, header := postAnonFit(t, app, nil, "/fittings/save-anonymous/",
		`{"name":"Anon Shared Fit","fit":{"name":"Anon Shared Fit","shipTypeId":1001,"items":[{"typeId":3001,"qty":1},{"typeId":3002,"qty":1}]}}`)
	if code != http.StatusOK {
		t.Fatalf("anonymous fit save: status %d, want 200 (%s)", code, body)
	}
	var saved struct {
		ShareURL   string `json:"shareUrl"`
		ShareToken string `json:"shareToken"`
	}
	if err := json.Unmarshal([]byte(body), &saved); err != nil || !strings.HasPrefix(saved.ShareURL, "/fittings/share/") {
		t.Fatalf("anonymous fit save body = %q, want a share URL", body)
	}
	cookies := headerCookies(header)
	var fitCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == "evesynapse-fits" {
			fitCookie = c
		}
	}
	if fitCookie == nil || !fitCookie.HttpOnly {
		t.Fatalf("fit history cookie = %v, want an HttpOnly evesynapse-fits cookie", fitCookie)
	}

	// The row is link-only: not public, not a draft.
	var isPublic, isDraft, isAnon bool
	if err := conn.QueryRowContext(ctx, `SELECT is_public, is_draft, is_anonymous FROM local_fittings`).Scan(&isPublic, &isDraft, &isAnon); err != nil {
		t.Fatalf("read fit row: %v", err)
	}
	if isPublic || isDraft || !isAnon {
		t.Fatalf("fit flags public=%v draft=%v anon=%v, want link-only anonymous", isPublic, isDraft, isAnon)
	}

	app.refreshPublicFitPages(ctx)

	// Anyone with the link gets the read-only public view.
	code, body = getAnon(t, app, saved.ShareURL)
	if code != http.StatusOK {
		t.Fatalf("shared fit page: status %d, want 200", code)
	}
	mustContain(t, "shared fit page", body, "Anon Shared Fit", "Shared by link", "not listed in the public browser")
	if strings.Contains(body, "Delete this fit") {
		t.Error("a stranger's shared fit view offered the delete control")
	}
	// The creator sees the delete control.
	_, body = getWithCookies(t, app, cookies, saved.ShareURL)
	mustContain(t, "creator fit page", body, "Delete this fit")

	// It is not in the public browser.
	_, body = getAnon(t, app, "/fittings/public/")
	if strings.Contains(body, "Anon Shared Fit") {
		t.Error("an anonymous link-only fit was listed in the public browser")
	}

	// The workbench remembers it for the creator.
	_, body = getWithCookies(t, app, cookies, "/fittings/")
	mustContain(t, "workbench history", body, "Your shared fits", "Anon Shared Fit",
		"Fits you share while signed out are kept for 180 days after they were last viewed, then deleted.")

	// A forged cookie cannot delete it.
	forged := []*http.Cookie{{Name: "evesynapse-fits", Value: saved.ShareToken}}
	postAnonFit(t, app, forged, saved.ShareURL+"delete", "")
	if code, _ := getAnon(t, app, saved.ShareURL); code != http.StatusOK {
		t.Fatalf("share after forged delete: status %d, want 200", code)
	}

	// The creator deletes it; the link 404s.
	postAnonFit(t, app, cookies, saved.ShareURL+"delete", "")
	if code, _ := getAnon(t, app, saved.ShareURL); code != http.StatusNotFound {
		t.Fatalf("deleted shared fit: status %d, want 404", code)
	}
}

func TestAnonymousFitExpirySweep(t *testing.T) {
	tr := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, tr)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	now := time.Now().UTC()
	old := now.Add(-200 * 24 * time.Hour)
	insert := func(anon bool, viewed any, token string) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, `INSERT INTO local_fittings
			(user_id, name, ship_type_id, items_json, is_public, is_draft, created_at, updated_at, share_token, is_anonymous, last_viewed_at)
			VALUES ($1, 'x', 1001, '{}', false, false, $2, $2, $3, $4, $5)`,
			user.ID, now, token, anon, viewed); err != nil {
			t.Fatalf("insert fit: %v", err)
		}
	}
	insert(true, old, strings.Repeat("a", 48))
	insert(true, now.Add(-10*24*time.Hour), strings.Repeat("b", 48))
	insert(false, old, "")
	n, err := q.DeleteExpiredAnonymousFittings(ctx, sql.NullTime{Time: now.Add(-180 * 24 * time.Hour), Valid: true})
	if err != nil || n != 1 {
		t.Fatalf("sweep deleted %d (%v), want exactly the stale anonymous fit", n, err)
	}
	var count int64
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM local_fittings`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("fits after sweep = %d (%v), want the recent anonymous and the signed one", count, err)
	}
	_ = app
}
