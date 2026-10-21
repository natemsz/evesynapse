package app

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
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
// Axon: the GUI's fixed local address (gui.ListenAddr in natemsz/axon)
// and the axon repo's release assets.
func TestAxonLinks(t *testing.T) {
	if AxonGUIAddr != "127.0.0.1:47611" {
		t.Errorf("AxonGUIAddr %q, want Axon's fixed 127.0.0.1:47611", AxonGUIAddr)
	}
	if len(axonDownloads) == 0 || axonDownloads[0].File != "axon-windows-amd64.exe" {
		t.Errorf("first download should be the Windows program (its own installer): %v", axonDownloads)
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

// TestIntelTokens: a pairing code made on the channels page is shown
// once, exchanged by Axon for a credential bound to its device key,
// and only requests signed by that key act as the account -- listing
// channels, posting lines that become reports, the same lines sent
// again stored once -- until the device is revoked.
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

	req := httptest.NewRequest(http.MethodPost, "/intel/channels/pairing", strings.NewReader(url.Values{"name": {"Home PC"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	m := regexp.MustCompile(`<pre class="ch-config">([0-9A-HJKMNP-TV-Z]{4}-[0-9A-HJKMNP-TV-Z]{4})</pre>`).FindStringSubmatch(rec.Body.String())
	if rec.Code != http.StatusOK || m == nil {
		t.Fatalf("the pairing code shown once: %d", rec.Code)
	}
	code := m[1]
	if _, body := getPage(t, app, cookie, "/intel/channels/"); strings.Contains(body, code) {
		t.Errorf("the pairing code is shown once, not on later visits")
	}

	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	_, wrongPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	pair := func(code, device string, pub ed25519.PublicKey) *httptest.ResponseRecorder {
		b, _ := json.Marshal(map[string]string{"code": code, "public_key": base64.StdEncoding.EncodeToString(pub), "device": device})
		req := httptest.NewRequest(http.MethodPost, "/api/intel/v1/pair", strings.NewReader(string(b)))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec
	}
	rec = pair(code, "Test PC", pub)
	var paired struct {
		Token   string `json:"token"`
		Account string `json:"account"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &paired) != nil || paired.Token == "" || paired.Account != "Watcher Pilot" {
		t.Fatalf("pairing exchange: %d %s", rec.Code, rec.Body.String())
	}
	token := paired.Token
	if rec := pair(code, "Test PC", pub); rec.Code != http.StatusUnauthorized {
		t.Errorf("a pairing code works twice: %d", rec.Code)
	}
	if rec := pair("XXXX-XXXX", "Test PC", pub); rec.Code != http.StatusUnauthorized {
		t.Errorf("an invented pairing code works: %d", rec.Code)
	}
	if _, body := getPage(t, app, cookie, "/intel/channels/"); !strings.Contains(body, "Open EveSynapse Axon @ "+AxonGUIAddr) || !strings.Contains(body, "Home PC") {
		t.Errorf("a paired account is offered the Axon link and its device list")
	}

	api := func(method, path, body, bearer string, key ed25519.PrivateKey) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		if key != nil {
			ts := strconv.FormatInt(time.Now().Unix(), 10)
			sig := ed25519.Sign(key, []byte(axonSignatureText(method, path, ts, []byte(body))))
			req.Header.Set("X-Axon-Timestamp", ts)
			req.Header.Set("X-Axon-Signature", base64.StdEncoding.EncodeToString(sig))
		}
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec
	}
	if r := api(http.MethodGet, "/api/intel/v1/channels", "", "", nil); r.Code != http.StatusUnauthorized {
		t.Errorf("no token: %d", r.Code)
	}
	if r := api(http.MethodGet, "/api/intel/v1/channels", "", "esw_nope", nil); r.Code != http.StatusUnauthorized {
		t.Errorf("a wrong token: %d", r.Code)
	}
	if r := api(http.MethodGet, "/api/intel/v1/channels", "", token, nil); r.Code != http.StatusUnauthorized {
		t.Errorf("the token without its device key: %d", r.Code)
	}
	if r := api(http.MethodGet, "/api/intel/v1/channels", "", token, wrongPriv); r.Code != http.StatusUnauthorized {
		t.Errorf("the token signed by another key: %d", r.Code)
	}
	if r := api(http.MethodGet, "/api/intel/v1/channels", "", token, priv); r.Code != http.StatusOK || !strings.Contains(r.Body.String(), `"name":"Home intel"`) || !strings.Contains(r.Body.String(), `"account":"Watcher Pilot"`) {
		t.Fatalf("channels: %d %s", r.Code, r.Body.String())
	}
	lines := `{"lines":["[ 2026.10.21 11:20:05 ] Scout > Jita +3 Bad Guy","[ 2026.10.21 11:20:09 ] Scout > anyone?"]}`
	path := "/api/intel/v1/channels/" + strconv.FormatInt(cid, 10) + "/lines"
	r := api(http.MethodPost, path, lines, token, priv)
	if r.Code != http.StatusOK || !strings.Contains(r.Body.String(), `"posted":1`) || !strings.Contains(r.Body.String(), `"skipped":1`) {
		t.Fatalf("lines: %d %s", r.Code, r.Body.String())
	}
	if r := api(http.MethodPost, path, lines, token, priv); !strings.Contains(r.Body.String(), `"posted":0`) || !strings.Contains(r.Body.String(), `"duplicates":1`) {
		t.Errorf("the same lines again are stored once: %s", r.Body.String())
	}
	if r := api(http.MethodPost, "/api/intel/v1/channels/999999/lines", lines, token, priv); r.Code != http.StatusForbidden {
		t.Errorf("another channel: %d", r.Code)
	}

	toks, _ := q.ListIntelTokens(ctx, u.ID)
	if len(toks) != 1 || !toks[0].LastUsedAt.Valid || toks[0].Device != "Test PC" {
		t.Fatalf("the device is listed and marked used: %+v", toks)
	}
	req = httptest.NewRequest(http.MethodPost, "/intel/channels/tokens/"+strconv.FormatInt(toks[0].ID, 10)+"/revoke", nil)
	req.AddCookie(cookie)
	app.Handler().ServeHTTP(httptest.NewRecorder(), req)
	if r := api(http.MethodGet, "/api/intel/v1/channels", "", token, priv); r.Code != http.StatusUnauthorized {
		t.Errorf("a revoked device still works: %d", r.Code)
	}
}
