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
// Axon: the link that opens its window (the URL scheme it registers)
// and the axon repo's release assets.
func TestAxonLinks(t *testing.T) {
	if AxonOpenURL != "evesynapse-axon://open" {
		t.Errorf("AxonOpenURL %q, want the link Axon registers, evesynapse-axon://open", AxonOpenURL)
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
	m := regexp.MustCompile(`<p class="ch-code-v">([0-9A-HJKMNP-TV-Z]{4}-[0-9A-HJKMNP-TV-Z]{4})</p>`).FindStringSubmatch(rec.Body.String())
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
	if _, body := getPage(t, app, cookie, "/intel/channels/"); !strings.Contains(body, `href="`+AxonOpenURL+`"`) || !strings.Contains(body, "Home PC") {
		t.Errorf("a paired account is offered the Axon link and its device list")
	}

	apiAt := func(method, path, body, bearer string, key ed25519.PrivateKey, at time.Time) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		if key != nil {
			ts := strconv.FormatInt(at.Unix(), 10)
			sig := ed25519.Sign(key, []byte(axonSignatureText(method, path, ts, []byte(body))))
			req.Header.Set("X-Axon-Timestamp", ts)
			req.Header.Set("X-Axon-Signature", base64.StdEncoding.EncodeToString(sig))
		}
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec
	}
	api := func(method, path, body, bearer string, key ed25519.PrivateKey) *httptest.ResponseRecorder {
		return apiAt(method, path, body, bearer, key, time.Now())
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
	// The same lines again, signed a second later the way Axon
	// re-signs a retry: still a duplicate — while a byte-identical
	// replay of the first request is refused (TestAxonReplay).
	if r := apiAt(http.MethodPost, path, lines, token, priv, time.Now().Add(time.Second)); !strings.Contains(r.Body.String(), `"posted":0`) || !strings.Contains(r.Body.String(), `"duplicates":1`) {
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

// pairDevice runs the pairing flow for one device and returns
// its bearer token and signing key.
func pairDevice(t *testing.T, app *Application, cookie *http.Cookie, device string) (string, ed25519.PrivateKey) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/intel/channels/pairing", strings.NewReader(url.Values{"name": {device}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	m := regexp.MustCompile(`<p class="ch-code-v">([0-9A-HJKMNP-TV-Z]{4}-[0-9A-HJKMNP-TV-Z]{4})</p>`).FindStringSubmatch(rec.Body.String())
	if rec.Code != http.StatusOK || m == nil {
		t.Fatalf("the pairing code shown once: %d", rec.Code)
	}
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]string{"code": m[1], "public_key": base64.StdEncoding.EncodeToString(pub), "device": device})
	req = httptest.NewRequest(http.MethodPost, "/api/intel/v1/pair", strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	var paired struct {
		Token string `json:"token"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &paired) != nil || paired.Token == "" {
		t.Fatalf("pairing exchange: %d %s", rec.Code, rec.Body.String())
	}
	return paired.Token, priv
}

// TestAxonReplay pins the replay guard: an accepted signed
// request sent again byte-for-byte is refused, while the same
// request signed afresh a moment later goes through.
func TestAxonReplay(t *testing.T) {
	app, conn, q := buildCorpTestApp(t, nil)
	ctx := context.Background()
	if _, err := conn.ExecContext(ctx, `INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (30000142, 'Jita', 10000002, 0.946) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	u, _ := q.CreateUser(ctx)
	seedCharacter(t, q, u.ID, 90000201, "Watcher Pilot")
	cookie := sessionCookie(t, app, u.ID, 90000201, "Watcher Pilot")
	token, priv := pairDevice(t, app, cookie, "Replay PC")

	send := func(at time.Time) *httptest.ResponseRecorder {
		ts := strconv.FormatInt(at.Unix(), 10)
		sig := ed25519.Sign(priv, []byte(axonSignatureText(http.MethodGet, "/api/intel/v1/channels", ts, nil)))
		req := httptest.NewRequest(http.MethodGet, "/api/intel/v1/channels", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Axon-Timestamp", ts)
		req.Header.Set("X-Axon-Signature", base64.StdEncoding.EncodeToString(sig))
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec
	}
	at := time.Now()
	if rec := send(at); rec.Code != http.StatusOK {
		t.Fatalf("the first signed request: %d %s", rec.Code, rec.Body.String())
	}
	if rec := send(at); rec.Code != http.StatusUnauthorized {
		t.Errorf("the identical request sent again: %d, want 401 — a signed request is single-use", rec.Code)
	}
	if rec := send(at.Add(time.Second)); rec.Code != http.StatusOK {
		t.Errorf("the request signed afresh: %d %s", rec.Code, rec.Body.String())
	}
}

// TestIntelPairCapAtRedemption pins the device cap at the
// redemption end of pairing: a code minted while the account
// sat under the cap cannot redeem once the account is full, and
// the refusal leaves the code good for after a revocation.
func TestIntelPairCapAtRedemption(t *testing.T) {
	app, _, q := buildCorpTestApp(t, nil)
	ctx := context.Background()
	u, _ := q.CreateUser(ctx)

	for i := 0; i < intelMaxTokens; i++ {
		full, err := newIntelToken()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := q.CreateIntelToken(ctx, db.CreateIntelTokenParams{UserID: u.ID, Name: "Device " + strconv.Itoa(i), Prefix: full[:len(intelTokenPrefix)+4], TokenHash: hashIntelToken(full), CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}

	code, err := newPairingCode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.CreateIntelPairingCode(ctx, db.CreateIntelPairingCodeParams{UserID: u.ID, Name: "Cap PC", CodeHash: hashIntelToken(normalizePairingCode(code)), CreatedAt: time.Now(), ExpiresAt: time.Now().Add(pairingCodeTTL)}); err != nil {
		t.Fatal(err)
	}
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	pairIt := func() *httptest.ResponseRecorder {
		b, _ := json.Marshal(map[string]string{"code": code, "public_key": base64.StdEncoding.EncodeToString(pub), "device": "Cap PC"})
		req := httptest.NewRequest(http.MethodPost, "/api/intel/v1/pair", strings.NewReader(string(b)))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec
	}
	if rec := pairIt(); rec.Code != http.StatusConflict {
		t.Fatalf("redeeming at the cap: %d %s, want 409", rec.Code, rec.Body.String())
	}
	row, err := q.GetIntelPairingCodeByHash(ctx, hashIntelToken(normalizePairingCode(code)))
	if err != nil {
		t.Fatal(err)
	}
	if row.UsedAt.Valid {
		t.Error("the refused redemption consumed the code")
	}

	tokens, err := q.ListIntelTokens(ctx, u.ID)
	if err != nil || len(tokens) != intelMaxTokens {
		t.Fatalf("the account's devices: %d, %v", len(tokens), err)
	}
	if _, err := q.DeleteIntelToken(ctx, db.DeleteIntelTokenParams{ID: tokens[0].ID, UserID: u.ID}); err != nil {
		t.Fatal(err)
	}
	if rec := pairIt(); rec.Code != http.StatusOK {
		t.Errorf("the same code after a revocation: %d %s, want 200", rec.Code, rec.Body.String())
	}
}

// TestPairingCodePurge pins the housekeeping on code creation:
// used and expired codes are swept away, live ones stay.
func TestPairingCodePurge(t *testing.T) {
	app, conn, q := buildCorpTestApp(t, nil)
	ctx := context.Background()
	u, _ := q.CreateUser(ctx)
	seedCharacter(t, q, u.ID, 90000201, "Watcher Pilot")
	cookie := sessionCookie(t, app, u.ID, 90000201, "Watcher Pilot")
	now := time.Now()

	mint := func(hash string, expires time.Time) int64 {
		id, err := q.CreateIntelPairingCode(ctx, db.CreateIntelPairingCodeParams{UserID: u.ID, Name: "Old PC", CodeHash: hash, CreatedAt: now.Add(-time.Hour), ExpiresAt: expires})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	mint("expired-code", now.Add(-time.Minute))
	used := mint("used-code", now.Add(pairingCodeTTL))
	if _, err := q.UseIntelPairingCode(ctx, db.UseIntelPairingCodeParams{ID: used, At: timeSet(now)}); err != nil {
		t.Fatal(err)
	}
	mint("live-code", now.Add(pairingCodeTTL))

	// Making a fresh code runs the sweep.
	if code, _ := postForm(t, app, cookie, "/intel/channels/pairing", url.Values{"name": {"New PC"}}); code != http.StatusOK {
		t.Fatalf("pairing form: %d", code)
	}
	count := func(hash string) int {
		var n int
		if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM intel_pairing_codes WHERE code_hash = $1`, hash).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count("expired-code") != 0 || count("used-code") != 0 {
		t.Error("stale pairing codes survived a fresh code being made")
	}
	if count("live-code") != 1 {
		t.Error("the sweep took a live code")
	}
}

// TestPairEndpointThrottled pins the anonymous per-address
// throttle on the pairing exchange: repeated attempts from one
// address are cut off, where before they were not counted at all.
func TestPairEndpointThrottled(t *testing.T) {
	app, _, _ := buildCorpTestApp(t, nil)
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"code": "XXXX-XXXX", "public_key": base64.StdEncoding.EncodeToString(pub), "device": "Test PC"})
	attempt := func() int {
		req := httptest.NewRequest(http.MethodPost, "/api/intel/v1/pair", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec.Code
	}
	if code := attempt(); code != http.StatusUnauthorized {
		t.Fatalf("the first attempt: %d, want 401 for the invented code", code)
	}
	throttled := false
	for i := 0; i < 90 && !throttled; i++ {
		throttled = attempt() == http.StatusTooManyRequests
	}
	if !throttled {
		t.Error("90 pairing attempts from one address were never throttled")
	}
}

// TestIntelLimiterForgetsIdleTokens pins the limiter's memory:
// a token whose window has fully lapsed leaves the map, by the
// sweep or by its own next call.
func TestIntelLimiterForgetsIdleTokens(t *testing.T) {
	l := &intelLimiter{seen: map[int64][]time.Time{}}
	t0 := time.Now()
	l.allow(1, t0)
	l.allow(2, t0)
	if len(l.seen) != 2 {
		t.Fatalf("two active tokens tracked, got %v", l.seen)
	}
	// A minute on, both windows have lapsed; another token's
	// call sweeps the idle pair out of the map entirely.
	l.allow(3, t0.Add(61*time.Second))
	if len(l.seen) != 1 {
		t.Fatalf("idle tokens kept in the map: %v", l.seen)
	}
	if _, ok := l.seen[3]; !ok {
		t.Fatalf("the calling token is not tracked: %v", l.seen)
	}
	// And a token returning after its own window lapsed starts
	// with just the new request counted.
	l.allow(3, t0.Add(2*time.Minute+2*time.Second))
	if got := len(l.seen[3]); got != 1 {
		t.Errorf("a returned token carries %d stamps, want 1", got)
	}
}
