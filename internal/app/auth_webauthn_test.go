package app

// Second-factor tests: real registration and assertion
// ceremonies against the WebAuthn library, driven by a small
// in-test software key (ES256, attestation none), plus the
// pending-session gate, the admin arming rules, recovery code
// burn, sign-count clone refusal, and the fresh-factor
// management gate.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"

	db "evesynapse/internal/db/sqlc"
)

// factorJar carries the session cookie across test requests.
type factorJar struct {
	cookies []*http.Cookie
}

func (j *factorJar) set(req *http.Request) {
	for _, c := range j.cookies {
		req.AddCookie(c)
	}
}

func (j *factorJar) save(resp *http.Response) {
	for _, c := range resp.Cookies() {
		found := false
		for i, old := range j.cookies {
			if old.Name == c.Name {
				j.cookies[i] = c
				found = true
			}
		}
		if !found {
			j.cookies = append(j.cookies, c)
		}
	}
}

// serveFactor runs one request through the session middleware
// and the handler, keeping the jar current.
func serveFactor(t *testing.T, app *Application, jar *factorJar, handler http.HandlerFunc, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	jar.set(req)
	rr := httptest.NewRecorder()
	app.sessions.LoadAndSave(handler).ServeHTTP(rr, req)
	jar.save(rr.Result())
	return rr
}

// factorAuthedSession opens a signed-in session for the user,
// applying mutate inside it (pending flags, satisfaction).
func factorAuthedSession(t *testing.T, app *Application, jar *factorJar, userID int64, mutate func(ctx context.Context)) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "https://example.com/", nil)
	req.TLS = &tls.ConnectionState{}
	serveFactor(t, app, jar, func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		app.sessions.Put(ctx, sessionAuthenticated, true)
		app.sessions.Put(ctx, sessionUserID, int(userID))
		putSessionCharID(app.sessions, ctx, 90000001)
		app.sessions.Put(ctx, sessionCharacterName, "Factor Pilot")
		if mutate != nil {
			mutate(ctx)
		}
	}, req)
}

func factorJSONRequest(target string, body any) *http.Request {
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(http.MethodPost, "https://example.com"+target, &buf)
	req.TLS = &tls.ConnectionState{}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	return req
}

func factorFormRequest(target string, form url.Values) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "https://example.com"+target, strings.NewReader(form.Encode()))
	req.TLS = &tls.ConnectionState{}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

// virtualKey is a software security key for ceremony tests.
type virtualKey struct {
	priv      *ecdsa.PrivateKey
	credID    []byte
	signCount uint32
}

func newVirtualKey(t *testing.T) *virtualKey {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	credID := make([]byte, 32)
	if _, err := rand.Read(credID); err != nil {
		t.Fatalf("credential id: %v", err)
	}
	return &virtualKey{priv: priv, credID: credID}
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// cosePublicKey is the ES256 COSE encoding of the key,
// hand-built so the tests take no CBOR dependency.
func (k *virtualKey) cosePublicKey() []byte {
	x := k.priv.PublicKey.X.Bytes()
	y := k.priv.PublicKey.Y.Bytes()
	xb := make([]byte, 32)
	yb := make([]byte, 32)
	copy(xb[32-len(x):], x)
	copy(yb[32-len(y):], y)
	out := []byte{0xA5, 0x01, 0x02, 0x03, 0x26, 0x20, 0x01, 0x21, 0x58, 0x20}
	out = append(out, xb...)
	out = append(out, 0x22, 0x58, 0x20)
	return append(out, yb...)
}

// attestationObject is a fmt=none attestation for the
// registration challenge.
func (k *virtualKey) attestationObject(rpID string) []byte {
	rpHash := sha256.Sum256([]byte(rpID))
	authData := append([]byte{}, rpHash[:]...)
	authData = append(authData, 0x45) // UP, UV, AT
	authData = append(authData, 0, 0, 0, 0)
	authData = append(authData, make([]byte, 16)...) // AAGUID
	var l [2]byte
	binary.BigEndian.PutUint16(l[:], uint16(len(k.credID)))
	authData = append(authData, l[:]...)
	authData = append(authData, k.credID...)
	authData = append(authData, k.cosePublicKey()...)
	obj := []byte{0xA3, 0x63, 'f', 'm', 't', 0x64, 'n', 'o', 'n', 'e', 0x67, 'a', 't', 't', 'S', 't', 'm', 't', 0xA0, 0x68, 'a', 'u', 't', 'h', 'D', 'a', 't', 'a', 0x58, byte(len(authData))}
	return append(obj, authData...)
}

func clientDataJSON(kind, challengeB64, origin string) []byte {
	raw, _ := json.Marshal(map[string]string{"type": kind, "challenge": challengeB64, "origin": origin})
	return raw
}

// registrationBody answers creation options with a complete
// attestation response.
func (k *virtualKey) registrationBody(challengeB64, rpID, origin string) map[string]any {
	return map[string]any{
		"id":    b64url(k.credID),
		"rawId": b64url(k.credID),
		"type":  "public-key",
		"response": map[string]string{
			"clientDataJSON":    b64url(clientDataJSON("webauthn.create", challengeB64, origin)),
			"attestationObject": b64url(k.attestationObject(rpID)),
		},
	}
}

// assertionBody answers assertion options with a signed
// assertion, advancing the key's sign count.
func (k *virtualKey) assertionBody(t *testing.T, challengeB64, rpID, origin string, count uint32) map[string]any {
	t.Helper()
	k.signCount = count
	rpHash := sha256.Sum256([]byte(rpID))
	authData := append([]byte{}, rpHash[:]...)
	authData = append(authData, 0x05) // UP, UV
	var c [4]byte
	binary.BigEndian.PutUint32(c[:], count)
	authData = append(authData, c[:]...)
	clientData := clientDataJSON("webauthn.get", challengeB64, origin)
	clientHash := sha256.Sum256(clientData)
	digest := sha256.Sum256(append(append([]byte{}, authData...), clientHash[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, k.priv, digest[:])
	if err != nil {
		t.Fatalf("sign assertion: %v", err)
	}
	return map[string]any{
		"id":    b64url(k.credID),
		"rawId": b64url(k.credID),
		"type":  "public-key",
		"response": map[string]string{
			"clientDataJSON":    b64url(clientData),
			"authenticatorData": b64url(authData),
			"signature":         b64url(sig),
		},
	}
}

func decodeChallenge(t *testing.T, body []byte) string {
	t.Helper()
	var options struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(body, &options); err != nil {
		t.Fatalf("decode options: %v", err)
	}
	if options.PublicKey.Challenge == "" {
		t.Fatal("options carry no challenge")
	}
	return options.PublicKey.Challenge
}

func factorTestUser(t *testing.T, q *db.Queries) int64 {
	t.Helper()
	user, err := q.CreateUser(context.Background())
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return user.ID
}

// A key enrolls and then asserts through the real ceremonies:
// the settings registration stores the credential and mints
// the codes, and the factor assertion verifies and lands home.
func TestFactorRegistrationAndAssertion(t *testing.T) {
	ctx := context.Background()
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	userID := factorTestUser(t, q)
	var jar factorJar
	factorAuthedSession(t, app, &jar, userID, nil)
	const rpID, origin = "example.com", "https://example.com"

	rr := serveFactor(t, app, &jar, app.handleSecurityKeysBegin, factorJSONRequest("/settings/security/keys/begin", map[string]string{"name": "Test key"}))
	if rr.Code != http.StatusOK {
		t.Fatalf("register begin: status %d, body %s", rr.Code, rr.Body.String())
	}
	key := newVirtualKey(t)
	rr = serveFactor(t, app, &jar, app.handleSecurityKeysFinish, factorJSONRequest("/settings/security/keys/finish", key.registrationBody(decodeChallenge(t, rr.Body.Bytes()), rpID, origin)))
	if rr.Code != http.StatusOK {
		t.Fatalf("register finish: status %d, body %s", rr.Code, rr.Body.String())
	}
	var enrolled struct {
		Codes []string `json:"codes"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &enrolled); err != nil || len(enrolled.Codes) != recoveryCodeCount {
		t.Fatalf("first key mints %d codes, got %v (%v)", recoveryCodeCount, enrolled.Codes, err)
	}
	rows, err := q.ListWebAuthnCredentialsByUser(ctx, userID)
	if err != nil || len(rows) != 1 || rows[0].Name != "Test key" {
		t.Fatalf("credential stored: rows %v, err %v", rows, err)
	}

	rr = serveFactor(t, app, &jar, app.handleFactorBegin, factorJSONRequest("/auth/factor/begin", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("assert begin: status %d, body %s", rr.Code, rr.Body.String())
	}
	rr = serveFactor(t, app, &jar, app.handleFactorFinish, factorJSONRequest("/auth/factor/finish", key.assertionBody(t, decodeChallenge(t, rr.Body.Bytes()), rpID, origin, 1)))
	if rr.Code != http.StatusOK {
		t.Fatalf("assert finish: status %d, body %s", rr.Code, rr.Body.String())
	}
	var done struct {
		Redirect string `json:"redirect"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &done); err != nil || done.Redirect == "" {
		t.Fatalf("assertion answers a redirect, got %q (%v)", rr.Body.String(), err)
	}
	stored, err := q.GetWebAuthnCredential(ctx, key.credID)
	if err != nil || stored.SignCount != 1 || !stored.LastUsedAt.Valid {
		t.Fatalf("sign count stored: %+v, err %v", stored, err)
	}
	events, err := q.ListWebAuthnEventsByUser(ctx, userID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	kinds := map[string]bool{}
	for _, ev := range events {
		kinds[ev.Kind] = true
	}
	if !kinds[factorEventEnroll] || !kinds[factorEventAssertOK] {
		t.Fatalf("audit events recorded: %v", events)
	}
}

// A pending session reaches only /auth/*: ordinary pages
// bounce to the factor page (or to enrollment for a
// must-enroll admin), and satisfying the factor unlocks them.
func TestFactorPendingGate(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	userID := factorTestUser(t, q)
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("in")) })
	get := func(target string) *http.Request {
		req := httptest.NewRequest(http.MethodGet, "https://example.com"+target, nil)
		req.TLS = &tls.ConnectionState{}
		req.Header.Set("Accept", "text/html")
		return req
	}

	var jar factorJar
	factorAuthedSession(t, app, &jar, userID, func(ctx context.Context) {
		app.sessions.Put(ctx, session2FAPending, true)
		app.sessions.Put(ctx, session2FAPendingAt, time.Now().Unix())
	})
	if rr := serveFactor(t, app, &jar, app.requireAuth(final).ServeHTTP, get("/settings")); rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/auth/factor" {
		t.Fatalf("pending settings: status %d, location %q", rr.Code, rr.Header().Get("Location"))
	}
	if rr := serveFactor(t, app, &jar, app.requireAuth(final).ServeHTTP, get("/auth/factor")); rr.Code != http.StatusOK {
		t.Fatalf("pending factor page: status %d", rr.Code)
	}
	if rr := serveFactor(t, app, &jar, app.requireAuth(final).ServeHTTP, get("/auth/eve")); rr.Code != http.StatusOK {
		t.Fatalf("pending re-sign-in: status %d", rr.Code)
	}
	if rr := serveFactor(t, app, &jar, app.requireAuth(final).ServeHTTP, get("/auth/link")); rr.Code != http.StatusSeeOther {
		t.Fatalf("pending chooser: status %d, want a bounce to the factor page", rr.Code)
	}

	var enrollJar factorJar
	factorAuthedSession(t, app, &enrollJar, userID, func(ctx context.Context) {
		app.sessions.Put(ctx, session2FAPending, true)
		app.sessions.Put(ctx, session2FAMustEnroll, true)
		app.sessions.Put(ctx, session2FAPendingAt, time.Now().Unix())
	})
	if rr := serveFactor(t, app, &enrollJar, app.requireAuth(final).ServeHTTP, get("/")); rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/auth/enroll" {
		t.Fatalf("must-enroll home: status %d, location %q", rr.Code, rr.Header().Get("Location"))
	}

	serveFactor(t, app, &jar, func(w http.ResponseWriter, r *http.Request) {
		app.satisfyFactor(r.Context())
	}, get("/auth/factor"))
	if rr := serveFactor(t, app, &jar, app.requireAuth(final).ServeHTTP, get("/settings")); rr.Code != http.StatusOK {
		t.Fatalf("satisfied settings: status %d", rr.Code)
	}
}

// Sign-in arms the factor from the account's state: a plain
// account signs straight in, an enrolled one asserts, and an
// admin with no key is sent to forced enrollment.
func TestArmSecondFactor(t *testing.T) {
	ctx := context.Background()
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	plain := factorTestUser(t, q)
	enrolled := factorTestUser(t, q)
	if _, err := q.CreateWebAuthnCredential(ctx, db.CreateWebAuthnCredentialParams{
		UserID: enrolled, CredentialID: []byte("cred-1"), PublicKey: []byte("pub-1"), Name: "Key",
	}); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	const adminChar = int64(90000055)
	admin := factorTestUser(t, q)
	if _, err := app.linkVerifiedCharacter(ctx, linkCharacterInput{
		UserID: admin, CharacterID: adminChar, Name: "Admin Pilot", OwnerHash: "owner-admin",
		AccessToken: "fixture", RefreshToken: "fixture",
	}); err != nil {
		t.Fatalf("link admin character: %v", err)
	}
	app.cfg.adminCharIDs = map[int64]bool{adminChar: true}

	arm := func(userID int64) (pending, enroll bool) {
		var jar factorJar
		factorAuthedSession(t, app, &jar, userID, nil)
		req := httptest.NewRequest(http.MethodGet, "https://example.com/", nil)
		req.TLS = &tls.ConnectionState{}
		serveFactor(t, app, &jar, func(w http.ResponseWriter, r *http.Request) {
			app.armSecondFactor(r.Context(), userID)
			pending = app.sessions.GetBool(r.Context(), session2FAPending)
			enroll = app.sessions.GetBool(r.Context(), session2FAMustEnroll)
		}, req)
		return pending, enroll
	}
	if pending, enroll := arm(plain); pending || enroll {
		t.Fatalf("plain account arms: pending %v enroll %v", pending, enroll)
	}
	if pending, enroll := arm(enrolled); !pending || enroll {
		t.Fatalf("enrolled account arms: pending %v enroll %v", pending, enroll)
	}
	if pending, enroll := arm(admin); !pending || !enroll {
		t.Fatalf("unenrolled admin arms: pending %v enroll %v", pending, enroll)
	}
	if _, err := q.CreateWebAuthnCredential(ctx, db.CreateWebAuthnCredentialParams{
		UserID: admin, CredentialID: []byte("cred-admin"), PublicKey: []byte("pub-admin"), Name: "Key",
	}); err != nil {
		t.Fatalf("seed admin credential: %v", err)
	}
	if pending, enroll := arm(admin); !pending || enroll {
		t.Fatalf("enrolled admin arms: pending %v enroll %v", pending, enroll)
	}
}

// A recovery code spends once: it burns on use, and
// regenerating the set kills the old codes outright.
func TestRecoveryCodesBurnAndRegenerate(t *testing.T) {
	ctx := context.Background()
	_, _, q := buildCorpTestApp(t, &countingTransport{})
	app := &Application{queries: q}
	userID := factorTestUser(t, q)
	codes, err := app.mintRecoveryCodes(ctx, userID)
	if err != nil || len(codes) != recoveryCodeCount {
		t.Fatalf("mint codes: %v, %v", codes, err)
	}
	if spent, err := app.burnRecoveryCode(ctx, userID, codes[0]); err != nil || !spent {
		t.Fatalf("burn fresh code: spent %v, err %v", spent, err)
	}
	if spent, err := app.burnRecoveryCode(ctx, userID, codes[0]); err != nil || spent {
		t.Fatalf("burn used code: spent %v, err %v", spent, err)
	}
	if spent, err := app.burnRecoveryCode(ctx, userID, "NOPE-NOPE"); err != nil || spent {
		t.Fatalf("burn wrong code: spent %v, err %v", spent, err)
	}
	fresh, err := app.mintRecoveryCodes(ctx, userID)
	if err != nil {
		t.Fatalf("regenerate: %v", err)
	}
	if spent, err := app.burnRecoveryCode(ctx, userID, codes[1]); err != nil || spent {
		t.Fatalf("burn pre-regeneration code: spent %v, err %v", spent, err)
	}
	if spent, err := app.burnRecoveryCode(ctx, userID, fresh[0]); err != nil || !spent {
		t.Fatalf("burn regenerated code: spent %v, err %v", spent, err)
	}
}

// An assertion whose sign count did not advance is refused:
// the possible clone never satisfies the factor.
func TestSignCountRegressionRefused(t *testing.T) {
	ctx := context.Background()
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	userID := factorTestUser(t, q)
	key := newVirtualKey(t)
	if _, err := q.CreateWebAuthnCredential(ctx, db.CreateWebAuthnCredentialParams{
		UserID: userID, CredentialID: key.credID, PublicKey: key.cosePublicKey(), SignCount: 5, Name: "Key",
	}); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	var jar factorJar
	factorAuthedSession(t, app, &jar, userID, nil)
	const rpID, origin = "example.com", "https://example.com"
	rr := serveFactor(t, app, &jar, app.handleFactorBegin, factorJSONRequest("/auth/factor/begin", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("assert begin: status %d", rr.Code)
	}
	rr = serveFactor(t, app, &jar, app.handleFactorFinish, factorJSONRequest("/auth/factor/finish", key.assertionBody(t, decodeChallenge(t, rr.Body.Bytes()), rpID, origin, 5)))
	if rr.Code == http.StatusOK {
		t.Fatal("regressed sign count accepted")
	}
	events, err := q.ListWebAuthnEventsByUser(ctx, userID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	failed := false
	for _, ev := range events {
		failed = failed || ev.Kind == factorEventAssertBad
	}
	if !failed {
		t.Fatalf("refused assertion files no failure event: %v", events)
	}
	for _, tc := range []struct {
		stored int64
		fresh  uint32
		clone  bool
		want   bool
	}{
		{5, 6, false, true},
		{5, 5, false, false},
		{5, 3, false, false},
		{5, 0, false, true},
		{0, 1, false, true},
		{5, 6, true, false},
	} {
		cred := &webauthn.Credential{Authenticator: webauthn.Authenticator{SignCount: tc.fresh, CloneWarning: tc.clone}}
		if got := signCountOK(tc.stored, cred); got != tc.want {
			t.Errorf("signCountOK(%d, count %d clone %v) = %v, want %v", tc.stored, tc.fresh, tc.clone, got, tc.want)
		}
	}
}

// Key management needs a fresh factor: a stale session is
// sent to assert first, and only a non-admin may drop the
// last key (which disables 2FA and clears the codes).
func TestSecurityManagementNeedsFreshFactor(t *testing.T) {
	ctx := context.Background()
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	userID := factorTestUser(t, q)
	seed := func(credID string) int64 {
		row, err := q.CreateWebAuthnCredential(ctx, db.CreateWebAuthnCredentialParams{
			UserID: userID, CredentialID: []byte(credID), PublicKey: []byte("pub"), Name: credID,
		})
		if err != nil {
			t.Fatalf("seed credential: %v", err)
		}
		return row.ID
	}
	first, second := seed("cred-a"), seed("cred-b")

	var jar factorJar
	factorAuthedSession(t, app, &jar, userID, nil)
	form := url.Values{"id": {factorItoa(first)}}
	if rr := serveFactor(t, app, &jar, app.handleSecurityKeysRemove, factorFormRequest("/settings/security/keys/remove", form)); rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/auth/factor" {
		t.Fatalf("stale remove: status %d, location %q", rr.Code, rr.Header().Get("Location"))
	}
	if rr := serveFactor(t, app, &jar, app.handleSecurityCodesRegenerate, factorFormRequest("/settings/security/codes/regenerate", url.Values{})); rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/auth/factor" {
		t.Fatalf("stale regenerate: status %d, location %q", rr.Code, rr.Header().Get("Location"))
	}

	req := httptest.NewRequest(http.MethodGet, "https://example.com/", nil)
	req.TLS = &tls.ConnectionState{}
	serveFactor(t, app, &jar, func(w http.ResponseWriter, r *http.Request) {
		app.satisfyFactor(r.Context())
	}, req)
	if rr := serveFactor(t, app, &jar, app.handleSecurityKeysRemove, factorFormRequest("/settings/security/keys/remove", form)); rr.Code != http.StatusSeeOther {
		t.Fatalf("fresh remove: status %d", rr.Code)
	}
	if n, _ := q.CountWebAuthnCredentialsByUser(ctx, userID); n != 1 {
		t.Fatalf("credentials after remove: %d, want 1", n)
	}

	if _, err := app.mintRecoveryCodes(ctx, userID); err != nil {
		t.Fatalf("mint codes: %v", err)
	}
	form = url.Values{"id": {factorItoa(second)}}
	if rr := serveFactor(t, app, &jar, app.handleSecurityKeysRemove, factorFormRequest("/settings/security/keys/remove", form)); rr.Code != http.StatusSeeOther {
		t.Fatalf("fresh last remove: status %d", rr.Code)
	}
	if n, _ := q.CountWebAuthnCredentialsByUser(ctx, userID); n != 0 {
		t.Fatalf("credentials after last remove: %d, want 0", n)
	}
	if n, _ := q.CountUnusedWebAuthnRecoveryCodesByUser(ctx, userID); n != 0 {
		t.Fatalf("codes after disable: %d, want 0", n)
	}

	const adminChar = int64(90000056)
	adminID := factorTestUser(t, q)
	if _, err := app.linkVerifiedCharacter(ctx, linkCharacterInput{
		UserID: adminID, CharacterID: adminChar, Name: "Admin Two", OwnerHash: "owner-admin",
		AccessToken: "fixture", RefreshToken: "fixture",
	}); err != nil {
		t.Fatalf("link admin character: %v", err)
	}
	app.cfg.adminCharIDs = map[int64]bool{adminChar: true}
	adminCred, err := q.CreateWebAuthnCredential(ctx, db.CreateWebAuthnCredentialParams{
		UserID: adminID, CredentialID: []byte("cred-admin-2"), PublicKey: []byte("pub"), Name: "Key",
	})
	if err != nil {
		t.Fatalf("seed admin credential: %v", err)
	}
	var adminJar factorJar
	factorAuthedSession(t, app, &adminJar, adminID, func(ctx context.Context) {
		app.satisfyFactor(ctx)
	})
	form = url.Values{"id": {factorItoa(adminCred.ID)}}
	if rr := serveFactor(t, app, &adminJar, app.handleSecurityKeysRemove, factorFormRequest("/settings/security/keys/remove", form)); rr.Code != http.StatusForbidden {
		t.Fatalf("admin last remove: status %d, want 403", rr.Code)
	}
}

// Five failed attempts end the pending session.
func TestFactorFailuresEndSession(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	userID := factorTestUser(t, q)
	var jar factorJar
	factorAuthedSession(t, app, &jar, userID, func(ctx context.Context) {
		app.sessions.Put(ctx, session2FAPending, true)
		app.sessions.Put(ctx, session2FAPendingAt, time.Now().Unix())
	})
	req := httptest.NewRequest(http.MethodGet, "https://example.com/", nil)
	req.TLS = &tls.ConnectionState{}
	var alive []bool
	serveFactor(t, app, &jar, func(w http.ResponseWriter, r *http.Request) {
		for range maxFactorFailures {
			alive = append(alive, app.noteFactorFailure(r.Context(), userID, "test attempt"))
		}
	}, req)
	for i, want := range []bool{true, true, true, true, false} {
		if alive[i] != want {
			t.Fatalf("failure %d: alive %v, want %v", i+1, alive[i], want)
		}
	}
}

func factorItoa(n int64) string {
	return strconv.FormatInt(n, 10)
}
