package app

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"rsc.io/qr"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

const (
	webauthnQRLifetime    = 3 * time.Minute
	webauthnQRMaxPending  = 128
	webauthnQRMaxAttempts = 3
)

type webauthnQRHandoff struct {
	userID          int64
	mode            string // factor, enroll, settings
	name            string
	code            string
	codeAttempts    int
	codeOK          bool
	stage           string // assert or register
	needsStepUp     bool
	completedStepUp bool
	first           bool
	codes           []string
	session         *webauthn.SessionData
	approved        bool
	expires         time.Time
	desktopHash     [32]byte
}

type webauthnQRHandoffs struct {
	mu sync.Mutex
	m  map[string]*webauthnQRHandoff
}

type webauthnQRDeviceView struct {
	Token   string
	Mode    string
	Expired bool
}

func qrDigest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func randomQRSecret(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func randomPairingCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

func cleanPairingCode(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else if r != ' ' && r != '-' {
			return ""
		}
	}
	return b.String()
}

func (q *webauthnQRHandoffs) put(id string, h *webauthnQRHandoff, now time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.m == nil {
		q.m = make(map[string]*webauthnQRHandoff)
	}
	for key, old := range q.m {
		if now.After(old.expires) {
			delete(q.m, key)
		}
	}
	if len(q.m) >= webauthnQRMaxPending {
		oldestKey := ""
		var oldest time.Time
		for key, old := range q.m {
			if oldestKey == "" || old.expires.Before(oldest) {
				oldestKey, oldest = key, old.expires
			}
		}
		delete(q.m, oldestKey)
	}
	q.m[id] = h
}

func (q *webauthnQRHandoffs) get(id string, now time.Time) *webauthnQRHandoff {
	q.mu.Lock()
	defer q.mu.Unlock()
	h := q.m[id]
	if h == nil || now.After(h.expires) {
		delete(q.m, id)
		return nil
	}
	cp := *h
	return &cp
}

func (q *webauthnQRHandoffs) verifyCode(id, raw string, now time.Time) (*webauthnQRHandoff, int, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	h := q.m[id]
	if h == nil || now.After(h.expires) || h.approved {
		delete(q.m, id)
		return nil, 0, false
	}
	code := cleanPairingCode(raw)
	if len(code) == 6 && subtle.ConstantTimeCompare([]byte(code), []byte(h.code)) == 1 {
		h.codeOK = true
		cp := *h
		return &cp, 0, true
	}
	h.codeAttempts++
	left := webauthnQRMaxAttempts - h.codeAttempts
	if left <= 0 {
		delete(q.m, id)
		return nil, 0, false
	}
	return nil, left, true
}

func (q *webauthnQRHandoffs) saveChallenge(id string, session *webauthn.SessionData, now time.Time) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	h := q.m[id]
	if h == nil || now.After(h.expires) || h.approved || !h.codeOK {
		return false
	}
	h.session = session
	return true
}

func (q *webauthnQRHandoffs) takeChallenge(id string, now time.Time) (*webauthnQRHandoff, *webauthn.SessionData) {
	q.mu.Lock()
	defer q.mu.Unlock()
	h := q.m[id]
	if h == nil || now.After(h.expires) || h.approved || !h.codeOK || h.session == nil {
		return nil, nil
	}
	session := h.session
	h.session = nil
	return h, session
}

func (q *webauthnQRHandoffs) approve(id string, now time.Time) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	h := q.m[id]
	if h == nil || now.After(h.expires) || h.approved {
		return false
	}
	h.approved = true
	return true
}

func (q *webauthnQRHandoffs) fail(id string, now time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if h := q.m[id]; h != nil {
		h.session = nil
		if now.After(h.expires) {
			delete(q.m, id)
		}
	}
}

func (q *webauthnQRHandoffs) claim(id string, desktopHash [32]byte, now time.Time) *webauthnQRHandoff {
	q.mu.Lock()
	defer q.mu.Unlock()
	h := q.m[id]
	if h == nil || subtle.ConstantTimeCompare(desktopHash[:], h.desktopHash[:]) != 1 || now.After(h.expires) || !h.approved {
		return nil
	}
	delete(q.m, id)
	return h
}

func (q *webauthnQRHandoffs) drop(id string, desktopHash [32]byte) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if h := q.m[id]; h != nil && subtle.ConstantTimeCompare(desktopHash[:], h.desktopHash[:]) == 1 {
		delete(q.m, id)
	}
}

func qrCookieName(secure bool) string {
	if secure {
		return "__Host-es_webauthn_qr"
	}
	return "es_webauthn_qr"
}

func requestIsSecure(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]), "https")
}

func (app *Application) desktopQRSecret(r *http.Request) (string, [32]byte, bool) {
	var zero [32]byte
	c, err := r.Cookie(qrCookieName(requestIsSecure(r)))
	if err != nil {
		return "", zero, false
	}
	token, secret, found := strings.Cut(c.Value, ".")
	if !found || len(token) < 20 || len(secret) < 20 || len(token) > 100 || len(secret) > 100 {
		return "", zero, false
	}
	return qrDigest(token), sha256.Sum256([]byte(secret)), true
}

func (app *Application) clearDesktopQRCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: qrCookieName(requestIsSecure(r)), Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: requestIsSecure(r), SameSite: http.SameSiteStrictMode,
	})
}

func qrSVG(target string) ([]byte, error) {
	code, err := qr.Encode(target, qr.L)
	if err != nil {
		return nil, err
	}
	const pad = 4
	total := code.Size + pad*2
	var b bytes.Buffer
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges"><rect width="%d" height="%d" fill="#fff"/><path fill="#111" d="`, total, total, total, total)
	for y := 0; y < code.Size; y++ {
		for x := 0; x < code.Size; {
			if !code.Black(x, y) {
				x++
				continue
			}
			run := 1
			for x+run < code.Size && code.Black(x+run, y) {
				run++
			}
			fmt.Fprintf(&b, "M%d %dh%dv1h-%dz", x+pad, y+pad, run, run)
			x += run
		}
	}
	b.WriteString(`"/></svg>`)
	return b.Bytes(), nil
}

func qrJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Cache-Control", "no-store")
	writeFactorJSON(w, status, map[string]string{"error": message})
}

// handleWebAuthnQRCreate starts a short-lived desktop-bound handoff.
// The browser session, account, intent and any required step-up are
// fixed here; the phone only receives a WebAuthn challenge for that
// already-authorized operation.
func (app *Application) handleWebAuthnQRCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		For  string `json:"for"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
		qrJSONError(w, http.StatusBadRequest, "bad request")
		return
	}
	ctx := r.Context()
	userID := int64(0)
	h := &webauthnQRHandoff{mode: in.For, name: strings.TrimSpace(in.Name), expires: time.Now().Add(webauthnQRLifetime)}
	switch in.For {
	case "factor":
		var ok bool
		ctx, userID, ok = app.factorAuthed(w, r, true)
		if !ok {
			return
		}
		if app.sessions.GetBool(ctx, session2FAPending) && !app.pendingAlive(ctx) {
			app.endPendingSession(ctx)
			qrJSONError(w, http.StatusForbidden, "expired")
			return
		}
		if app.sessions.GetBool(ctx, session2FAMustEnroll) {
			qrJSONError(w, http.StatusForbidden, "state")
			return
		}
		h.stage = "assert"
	case "enroll":
		var ok bool
		ctx, userID, ok = app.enrollAuthed(w, r, true)
		if !ok {
			return
		}
		h.stage = "register"
	case "settings":
		var ok bool
		ctx, userID, ok = app.securityAuthed(w, r, true)
		if !ok {
			return
		}
		count, err := app.queries.CountWebAuthnCredentialsByUser(ctx, userID)
		if err != nil {
			qrJSONError(w, http.StatusInternalServerError, "store")
			return
		}
		h.needsStepUp = count > 0 && !app.factorFresh(ctx)
		if h.needsStepUp {
			h.stage = "assert"
		} else {
			h.stage = "register"
		}
	default:
		qrJSONError(w, http.StatusBadRequest, "unknown request")
		return
	}
	if h.name == "" {
		h.name = "Security key"
	}
	if len(h.name) > 64 {
		h.name = h.name[:64]
	}
	h.userID = userID
	if h.mode == "factor" {
		count, err := app.queries.CountWebAuthnCredentialsByUser(ctx, userID)
		if err != nil || count == 0 {
			qrJSONError(w, http.StatusBadRequest, "none")
			return
		}
	}
	token, err := randomQRSecret(32)
	if err != nil {
		qrJSONError(w, http.StatusInternalServerError, "begin")
		return
	}
	secret, err := randomQRSecret(32)
	if err != nil {
		qrJSONError(w, http.StatusInternalServerError, "begin")
		return
	}
	h.code, err = randomPairingCode()
	if err != nil {
		qrJSONError(w, http.StatusInternalServerError, "begin")
		return
	}
	h.desktopHash = sha256.Sum256([]byte(secret))
	deviceURL := requestScheme(r) + "://" + r.Host + "/auth/factor/qr/device?t=" + url.QueryEscape(token)
	svg, err := qrSVG(deviceURL)
	if err != nil {
		logging.Errorf("2fa: make QR: %v", err)
		qrJSONError(w, http.StatusInternalServerError, "begin")
		return
	}
	app.webauthnQR.put(qrDigest(token), h, time.Now())
	http.SetCookie(w, &http.Cookie{
		Name: qrCookieName(requestIsSecure(r)), Value: token + "." + secret,
		Path: "/", Expires: h.expires, HttpOnly: true, Secure: requestIsSecure(r), SameSite: http.SameSiteStrictMode,
	})
	w.Header().Set("Cache-Control", "no-store")
	writeFactorJSON(w, http.StatusOK, map[string]any{
		"qr":   "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString(svg),
		"code": h.code[:3] + " " + h.code[3:],
	})
}

func requestScheme(r *http.Request) string {
	if requestIsSecure(r) {
		return "https"
	}
	return "http"
}

func (app *Application) handleWebAuthnQRStatus(w http.ResponseWriter, r *http.Request) {
	ctx, userID, ok := app.factorAuthed(w, r, true)
	if !ok {
		return
	}
	id, desktopHash, hasCookie := app.desktopQRSecret(r)
	if !hasCookie {
		writeFactorJSON(w, http.StatusOK, map[string]string{"status": "expired"})
		return
	}
	h := app.webauthnQR.get(id, time.Now())
	if h == nil || h.userID != userID {
		app.clearDesktopQRCookie(w, r)
		writeFactorJSON(w, http.StatusOK, map[string]string{"status": "expired"})
		return
	}
	if !h.approved {
		w.Header().Set("Cache-Control", "no-store")
		writeFactorJSON(w, http.StatusOK, map[string]string{"status": "pending"})
		return
	}
	claimed := app.webauthnQR.claim(id, desktopHash, time.Now())
	app.clearDesktopQRCookie(w, r)
	if claimed == nil || claimed.userID != userID {
		writeFactorJSON(w, http.StatusOK, map[string]string{"status": "expired"})
		return
	}
	result := map[string]any{"status": "done"}
	switch claimed.mode {
	case "factor":
		pending := app.sessions.GetBool(ctx, session2FAPending)
		if pending && !app.pendingAlive(ctx) {
			app.endPendingSession(ctx)
			writeFactorJSON(w, http.StatusOK, map[string]string{"status": "expired"})
			return
		}
		app.satisfyFactor(ctx)
		redirect := "/settings#security"
		if pending {
			redirect = app.takeReturnTo(ctx)
		}
		result["redirect"] = redirect
	case "enroll":
		result["codes"] = claimed.codes
	case "settings":
		if claimed.completedStepUp || claimed.first {
			app.satisfyFactor(ctx)
		}
		if claimed.first {
			result["codes"] = claimed.codes
		} else {
			result["redirect"] = "/settings#security"
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeFactorJSON(w, http.StatusOK, result)
}

func (app *Application) handleWebAuthnQRCancel(w http.ResponseWriter, r *http.Request) {
	id, desktopHash, ok := app.desktopQRSecret(r)
	if ok {
		app.webauthnQR.drop(id, desktopHash)
	}
	app.clearDesktopQRCookie(w, r)
	writeFactorJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}

func (app *Application) handleWebAuthnQRDevicePage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	token := r.URL.Query().Get("t")
	h := app.webauthnQR.get(qrDigest(token), time.Now())
	if len(token) < 20 || len(token) > 100 || h == nil {
		app.render(r.Context(), w, http.StatusGone, "webauthn_device.html", pageData{WebAuthnQR: &webauthnQRDeviceView{Expired: true}})
		return
	}
	data := pageData{WebAuthnQR: &webauthnQRDeviceView{Token: token, Mode: h.mode}}
	data.Scripts = []string{"/static/webauthn.js"}
	app.render(r.Context(), w, http.StatusOK, "webauthn_device.html", data)
}

func deviceQRInputs(r *http.Request) (string, string) {
	return r.Header.Get("X-Evesynapse-QR-Token"), r.Header.Get("X-Evesynapse-QR-Code")
}

func (app *Application) handleWebAuthnQRDeviceOptions(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
		qrJSONError(w, http.StatusBadRequest, "Enter the pairing code shown on your computer.")
		return
	}
	token := r.Header.Get("X-Evesynapse-QR-Token")
	id := qrDigest(token)
	h, left, active := app.webauthnQR.verifyCode(id, in.Code, time.Now())
	if !active {
		qrJSONError(w, http.StatusGone, "This QR code expired or too many codes were entered. Get a new one on your computer.")
		return
	}
	if h == nil {
		qrJSONError(w, http.StatusForbidden, "That code does not match. "+strconv.Itoa(left)+" tries left.")
		return
	}
	ctx := r.Context()
	var (
		options any
		session *webauthn.SessionData
		err     error
	)
	if h.stage == "assert" {
		user, err2 := app.webauthnUserFor(ctx, h.userID, app.factorDisplayName(ctx, h.userID))
		if err2 != nil || len(user.credentials) == 0 {
			qrJSONError(w, http.StatusGone, "This passkey request expired. Start again on your computer.")
			return
		}
		rp, err2 := webauthnFor(r)
		if err2 != nil {
			qrJSONError(w, http.StatusInternalServerError, "begin")
			return
		}
		options, session, err = rp.BeginLogin(user, webauthn.WithUserVerification(protocol.VerificationRequired))
	} else if h.stage == "register" {
		options, session, err = app.beginRegistrationFor(ctx, r, h.userID)
	} else {
		qrJSONError(w, http.StatusGone, "This passkey request expired. Start again on your computer.")
		return
	}
	if err != nil {
		logging.Warnf("2fa: QR begin for user %d: %v", h.userID, err)
		qrJSONError(w, http.StatusInternalServerError, "begin")
		return
	}
	if !app.webauthnQR.saveChallenge(id, session, time.Now()) {
		qrJSONError(w, http.StatusGone, "This QR code expired. Get a new one on your computer.")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeFactorJSON(w, http.StatusOK, map[string]any{"operation": h.stage, "publicKey": options})
}

func (app *Application) handleWebAuthnQRDeviceVerify(w http.ResponseWriter, r *http.Request) {
	token, code := deviceQRInputs(r)
	id := qrDigest(token)
	h := app.webauthnQR.get(id, time.Now())
	cleanCode := cleanPairingCode(code)
	if h == nil || len(token) < 20 || len(token) > 100 || !h.codeOK || len(cleanCode) != len(h.code) || subtle.ConstantTimeCompare([]byte(cleanCode), []byte(h.code)) != 1 {
		qrJSONError(w, http.StatusGone, "This QR code expired. Get a new one on your computer.")
		return
	}
	h, session := app.webauthnQR.takeChallenge(id, time.Now())
	if h == nil || session == nil {
		qrJSONError(w, http.StatusGone, "This passkey request expired. Get a new one on your computer.")
		return
	}
	ctx := r.Context()
	if h.stage == "assert" {
		user, err := app.webauthnUserFor(ctx, h.userID, app.factorDisplayName(ctx, h.userID))
		if err != nil {
			qrJSONError(w, http.StatusInternalServerError, "store")
			return
		}
		rp, err := webauthnFor(r)
		if err != nil {
			qrJSONError(w, http.StatusInternalServerError, "rp")
			return
		}
		cred, err := rp.FinishLogin(user, *session, r)
		if err != nil {
			app.logFactorEvent(ctx, h.userID, factorEventAssertBad)
			app.webauthnQR.fail(id, time.Now())
			qrJSONError(w, http.StatusBadRequest, "verify")
			return
		}
		stored, err := app.queries.GetWebAuthnCredential(ctx, cred.ID)
		if err != nil || !signCountOK(stored.SignCount, cred) {
			app.logFactorEvent(ctx, h.userID, factorEventAssertBad)
			app.webauthnQR.fail(id, time.Now())
			qrJSONError(w, http.StatusBadRequest, "verify")
			return
		}
		if err := app.queries.UpdateWebAuthnCredentialSignCount(ctx, db.UpdateWebAuthnCredentialSignCountParams{CredentialID: cred.ID, SignCount: int64(cred.Authenticator.SignCount)}); err != nil {
			logging.Errorf("2fa: QR update sign count for user %d: %v", h.userID, err)
		}
		app.logFactorEvent(ctx, h.userID, factorEventAssertOK)
		if h.mode == "settings" && h.needsStepUp {
			app.webauthnQR.mu.Lock()
			if current := app.webauthnQR.m[id]; current != nil {
				current.completedStepUp = true
				current.stage = "register"
			}
			app.webauthnQR.mu.Unlock()
			writeFactorJSON(w, http.StatusOK, map[string]string{"next": "register"})
			return
		}
		if !app.webauthnQR.approve(id, time.Now()) {
			qrJSONError(w, http.StatusGone, "This QR code expired. Get a new one on your computer.")
			return
		}
		writeFactorJSON(w, http.StatusOK, map[string]string{"done": "true"})
		return
	}
	if h.stage != "register" {
		qrJSONError(w, http.StatusGone, "This passkey request expired. Start again on your computer.")
		return
	}
	first, err := app.finishRegistrationFor(ctx, r, h.userID, h.name, session)
	if err != nil {
		if err == errFactorExists {
			qrJSONError(w, http.StatusBadRequest, "exists")
		} else {
			logging.Warnf("2fa: QR finish registration for user %d: %v", h.userID, err)
			qrJSONError(w, http.StatusBadRequest, "verify")
		}
		return
	}
	var codes []string
	if first {
		codes, err = app.mintRecoveryCodes(ctx, h.userID)
		if err != nil {
			logging.Errorf("2fa: mint QR recovery codes for user %d: %v", h.userID, err)
			qrJSONError(w, http.StatusInternalServerError, "store")
			return
		}
	}
	app.webauthnQR.mu.Lock()
	if current := app.webauthnQR.m[id]; current != nil {
		current.first = first
		current.codes = codes
	}
	app.webauthnQR.mu.Unlock()
	if !app.webauthnQR.approve(id, time.Now()) {
		qrJSONError(w, http.StatusGone, "This QR code expired. Get a new one on your computer.")
		return
	}
	writeFactorJSON(w, http.StatusOK, map[string]string{"done": "true"})
}
