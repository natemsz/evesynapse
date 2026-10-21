package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
)

// TestAxonLinks pins the address and downloads the site offers for
// Axon: the GUI's fixed local address (gui.ListenAddr in natemsz/axon,
// probed by static/app.js) and the axon repo's release assets.
func TestAxonLinks(t *testing.T) {
	if AxonGUIAddr != "127.0.0.1:47611" {
		t.Errorf("AxonGUIAddr %q, want Axon's fixed 127.0.0.1:47611", AxonGUIAddr)
	}
	if len(axonDownloads) == 0 || axonDownloads[0].File != "install.ps1" {
		t.Errorf("first download should be the Windows installer: %v", axonDownloads)
	}
	for _, d := range axonDownloads {
		if strings.Contains(d.File, "intelwatch") || strings.Contains(d.Label, "intelwatch") {
			t.Errorf("old intelwatch download remains: %+v", d)
		}
	}
	if !strings.HasPrefix(axonReleases, "https://github.com/natemsz/axon/") {
		t.Errorf("downloads should come from the axon repo: %q", axonReleases)
	}
}

// TestIntelTokens: a token made on the channels page is shown once in
// a ready config, acts as its account on the Axon API -- listing
// channels, posting lines that become reports, the same lines sent
// again stored once -- and stops the moment it is revoked.
func TestIntelTokens(t *testing.T) {
	app, conn, q := buildCorpTestApp(t, nil)
	ctx := context.Background()
	if _, err := conn.ExecContext(ctx, `INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30000142, 'Jita', 10000002, 0.946) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	u, _ := q.CreateUser(ctx)
	seedCharacter(t, q, u.ID, 90000201, "Watcher Pilot")
	_ = q.UpsertCharacterCorporation(ctx, db.UpsertCharacterCorporationParams{CharacterID: 90000201, CorporationID: 98000201, UpdatedAt: time.Now()})
	cookie := sessionCookie(t, app, u.ID, 90000201, "Watcher Pilot")
	cid, err := q.CreateIntelChannel(ctx, db.CreateIntelChannelParams{OwnerUserID: u.ID, Name: "Home intel", CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/intel/channels/tokens", strings.NewReader(url.Values{"name": {"Home PC"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	token := regexp.MustCompile(`esw_[0-9A-Za-z]{32}`).FindString(rec.Body.String())
	if rec.Code != http.StatusOK || token == "" || !strings.Contains(rec.Body.String(), `&#34;token&#34;`) {
		t.Fatalf("the new token in its config: %d", rec.Code)
	}
	if _, body := getPage(t, app, cookie, "/intel/channels/"); strings.Contains(body, token) || !strings.Contains(body, "Home PC") {
		t.Errorf("the token is shown once; afterwards only its name and prefix")
	}

	api := func(method, path, body, bearer string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec
	}
	if r := api(http.MethodGet, "/api/intel/v1/channels", "", ""); r.Code != http.StatusUnauthorized {
		t.Errorf("no token: %d", r.Code)
	}
	if r := api(http.MethodGet, "/api/intel/v1/channels", "", "esw_nope"); r.Code != http.StatusUnauthorized {
		t.Errorf("a wrong token: %d", r.Code)
	}
	if r := api(http.MethodGet, "/api/intel/v1/channels", "", token); r.Code != http.StatusOK || !strings.Contains(r.Body.String(), `"name":"Home intel"`) || !strings.Contains(r.Body.String(), `"account":"Watcher Pilot"`) {
		t.Fatalf("channels: %d %s", r.Code, r.Body.String())
	}
	lines := `{"lines":["[ 2026.10.21 11:20:05 ] Scout > Jita +3 Bad Guy","[ 2026.10.21 11:20:09 ] Scout > anyone?"]}`
	path := "/api/intel/v1/channels/" + strconv.FormatInt(cid, 10) + "/lines"
	r := api(http.MethodPost, path, lines, token)
	if r.Code != http.StatusOK || !strings.Contains(r.Body.String(), `"posted":1`) || !strings.Contains(r.Body.String(), `"skipped":1`) {
		t.Fatalf("lines: %d %s", r.Code, r.Body.String())
	}
	if r := api(http.MethodPost, path, lines, token); !strings.Contains(r.Body.String(), `"posted":0`) || !strings.Contains(r.Body.String(), `"duplicates":1`) {
		t.Errorf("the same lines again are stored once: %s", r.Body.String())
	}
	if r := api(http.MethodPost, "/api/intel/v1/channels/999999/lines", lines, token); r.Code != http.StatusForbidden {
		t.Errorf("another channel: %d", r.Code)
	}

	toks, _ := q.ListIntelTokens(ctx, u.ID)
	if len(toks) != 1 || !toks[0].LastUsedAt.Valid {
		t.Fatalf("the token is listed and marked used: %+v", toks)
	}
	req = httptest.NewRequest(http.MethodPost, "/intel/channels/tokens/"+strconv.FormatInt(toks[0].ID, 10)+"/revoke", nil)
	req.AddCookie(cookie)
	app.Handler().ServeHTTP(httptest.NewRecorder(), req)
	if r := api(http.MethodGet, "/api/intel/v1/channels", "", token); r.Code != http.StatusUnauthorized {
		t.Errorf("a revoked token still works: %d", r.Code)
	}
}
