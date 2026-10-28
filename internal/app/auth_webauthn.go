package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// Second-factor authentication (WebAuthn security keys and
// passkeys), run by EveSynapse itself. It is independent of the
// EVE account's own 2FA: the enrollment copy recommends a key
// separate from the one guarding the EVE account.
//
// Admin accounts must enroll and assert at every sign-in;
// everyone else opts in from Settings. Recovery is ten one-time
// codes shown once at enrollment. Losing every key and the codes
// is an operator-side fix: there is deliberately no in-page
// recovery path, matching the admin surface's no-host-control
// boundary.
//
// Ceremony state (challenges) lives in the session, never in the
// database. The ceremonies themselves are the go-webauthn
// library's, never hand-rolled.
const (
	// session2FAPending marks a session signed in through SSO but
	// not yet through the second factor. Only /auth/* is
	// reachable until the ceremony completes (factorGate).
	session2FAPending = "2fa_pending"
	// session2FAMustEnroll marks a pending admin session with no
	// credential yet: it lands on forced enrollment.
	session2FAMustEnroll = "2fa_enroll"
	// session2FASatisfiedAt is the unix time the factor was
	// last satisfied. Sensitive actions need it within
	// factorFreshFor.
	session2FASatisfiedAt = "2fa_ok_at"
	// session2FAPendingAt is the unix time the pending state
	// started; a pending session older than pendingLifetime is
	// dead and its handlers destroy it.
	session2FAPendingAt = "2fa_pending_at"
	// session2FAFailures counts failed factor attempts in this
	// pending session; maxFactorFailures ends it.
	session2FAFailures = "2fa_failures"
	// session2FARegChallenge and session2FALoginChallenge hold
	// the in-flight ceremony state as JSON (one key each so a
	// settings registration and a factor assertion cannot eat
	// each other's challenge).
	session2FARegChallenge   = "2fa_reg_challenge"
	session2FALoginChallenge = "2fa_login_challenge"
	// session2FARegName holds the credential name typed for the
	// in-flight registration.
	session2FARegName = "2fa_reg_name"
)

const (
	// factorFreshFor is how long one factor satisfaction covers
	// sensitive actions (adding or removing a credential,
	// viewing recovery codes, disabling 2FA).
	factorFreshFor = 10 * time.Minute
	// pendingLifetime caps a pending session: SSO without the
	// second factor must not linger.
	pendingLifetime = 30 * time.Minute
	// maxFactorFailures ends the pending session.
	maxFactorFailures = 5
	// recoveryCodeCount is the codes minted at enrollment and
	// on regeneration.
	recoveryCodeCount = 10
)

// Second-factor audit event kinds (webauthn_events.kind).
const (
	factorEventEnroll    = "enroll"
	factorEventAssertOK  = "assert_ok"
	factorEventAssertBad = "assert_fail"
	factorEventRecover   = "recover"
	factorEventRemove    = "remove"
	factorEventRegen     = "codes_regenerated"
)

// factorEventLabel is the settings display line for an event kind.
func factorEventLabel(kind string) string {
	switch kind {
	case factorEventEnroll:
		return "Key enrolled"
	case factorEventAssertOK:
		return "Sign-in verified"
	case factorEventAssertBad:
		return "Failed attempt"
	case factorEventRecover:
		return "Recovery code used"
	case factorEventRemove:
		return "Key removed"
	case factorEventRegen:
		return "Codes regenerated"
	default:
		return kind
	}
}

// webauthnUser adapts an account to the library's User interface.
// The handle is the account id: opaque to the browser, stable
// across ceremonies.
type webauthnUser struct {
	id          int64
	name        string
	displayName string
	credentials []webauthn.Credential
}

func (u webauthnUser) WebAuthnID() []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(u.id))
	return b[:]
}

func (u webauthnUser) WebAuthnName() string                       { return u.name }
func (u webauthnUser) WebAuthnDisplayName() string                { return u.displayName }
func (u webauthnUser) WebAuthnCredentials() []webauthn.Credential { return u.credentials }
func (u webauthnUser) WebAuthnIcon() string                       { return "" }

// webauthnUserFor loads the account's ceremony user: its stored
// credentials become the library's credential set, carrying
// their sign counts for clone detection.
func (app *Application) webauthnUserFor(ctx context.Context, userID int64, name string) (webauthnUser, error) {
	u := webauthnUser{id: userID, name: name, displayName: name}
	rows, err := app.queries.ListWebAuthnCredentialsByUser(ctx, userID)
	if err != nil {
		return u, err
	}
	for _, row := range rows {
		u.credentials = append(u.credentials, webauthn.Credential{
			ID:        row.CredentialID,
			PublicKey: row.PublicKey,
			Authenticator: webauthn.Authenticator{
				SignCount: uint32(row.SignCount),
			},
		})
	}
	return u, nil
}

// webauthnFor builds the relying party for this request's host.
// RP ID and origins derive from the deployment's own hostname,
// never from configuration: a key enrolled on one hostname does
// not assert on another, and the enrollment copy says so.
func webauthnFor(r *http.Request) (*webauthn.WebAuthn, error) {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	scheme := "https"
	if r.TLS == nil {
		scheme = "http"
		if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
			scheme = strings.ToLower(strings.TrimSpace(strings.Split(proto, ",")[0]))
		}
	}
	return webauthn.New(&webauthn.Config{
		RPDisplayName: "EveSynapse",
		RPID:          host,
		RPOrigins:     []string{scheme + "://" + r.Host},
	})
}

// putCeremony stores one ceremony's session data in the session
// as JSON under key.
func (app *Application) putCeremony(ctx context.Context, key string, session *webauthn.SessionData) error {
	raw, err := json.Marshal(session)
	if err != nil {
		return err
	}
	app.sessions.Put(ctx, key, string(raw))
	return nil
}

// popCeremony takes one ceremony's session data back out. It is
// single-use: read it, clear it, then verify against it.
func (app *Application) popCeremony(ctx context.Context, key string) (webauthn.SessionData, bool) {
	var session webauthn.SessionData
	raw := app.sessions.PopString(ctx, key)
	if raw == "" {
		return session, false
	}
	if err := json.Unmarshal([]byte(raw), &session); err != nil {
		return session, false
	}
	return session, true
}

// factorGate is where a pending session may go: the factor
// and enrollment pages and their ceremonies, sign-out, and
// the SSO round trip itself (signing in again re-arms). The
// link chooser and demo sign-in wait for a full session.
// Blocked requests are sent to the factor page, or to forced
// enrollment for an admin with no credential yet. A session
// that is not pending is never gated.
func (app *Application) factorGate(r *http.Request) (target string, blocked bool) {
	ctx := r.Context()
	if !app.sessions.GetBool(ctx, session2FAPending) {
		return "", false
	}
	switch {
	case strings.HasPrefix(r.URL.Path, "/auth/factor"),
		strings.HasPrefix(r.URL.Path, "/auth/enroll"),
		r.URL.Path == "/auth/logout",
		r.URL.Path == "/auth/eve",
		r.URL.Path == "/auth/login",
		r.URL.Path == "/auth/callback":
		return "", false
	}
	if app.sessions.GetBool(ctx, session2FAMustEnroll) {
		return "/auth/enroll", true
	}
	return "/auth/factor", true
}

// armSecondFactor runs at the end of every SSO sign-in. An
// account with 2FA enabled (any credential stored) gets a
// pending session until it asserts; an admin account with no
// credential yet gets forced enrollment. Anything else signs
// straight in. On a read failure it logs and signs in: the
// database being unreachable already breaks every page.
func (app *Application) armSecondFactor(ctx context.Context, userID int64) {
	app.sessions.Put(ctx, session2FAPending, false)
	app.sessions.Put(ctx, session2FAMustEnroll, false)
	if userID == 0 {
		return
	}
	n, err := app.queries.CountWebAuthnCredentialsByUser(ctx, userID)
	if err != nil {
		logging.Errorf("2fa: count credentials for user %d: %v", userID, err)
		return
	}
	admin := false
	if chars, err := app.queries.ListCharactersByUser(ctx, userID); err != nil {
		logging.Errorf("2fa: list characters for user %d: %v", userID, err)
	} else {
		admin = app.adminAmong(chars)
	}
	if n == 0 && !admin {
		return
	}
	app.sessions.Put(ctx, session2FAPending, true)
	app.sessions.Put(ctx, session2FAMustEnroll, n == 0 && admin)
	app.sessions.Put(ctx, session2FAPendingAt, time.Now().Unix())
	app.sessions.Put(ctx, session2FAFailures, 0)
}

// satisfyFactor completes the second factor: the pending state
// clears and the satisfaction timestamp covers sensitive
// actions for the next factorFreshFor.
func (app *Application) satisfyFactor(ctx context.Context) {
	app.sessions.Put(ctx, session2FAPending, false)
	app.sessions.Put(ctx, session2FAMustEnroll, false)
	app.sessions.Put(ctx, session2FASatisfiedAt, time.Now().Unix())
	app.sessions.Put(ctx, session2FAFailures, 0)
	app.sessions.Remove(ctx, session2FAPendingAt)
}

// factorFresh reports whether the factor was satisfied within
// the last factorFreshFor. Sensitive management actions need
// it; anything older re-asserts first.
func (app *Application) factorFresh(ctx context.Context) bool {
	at := app.sessions.GetInt64(ctx, session2FASatisfiedAt)
	if at == 0 {
		return false
	}
	return time.Since(time.Unix(at, 0)) < factorFreshFor
}

// pendingAlive reports whether the pending state is still
// within its lifetime.
func (app *Application) pendingAlive(ctx context.Context) bool {
	at := app.sessions.GetInt64(ctx, session2FAPendingAt)
	if at == 0 {
		return false
	}
	return time.Since(time.Unix(at, 0)) < pendingLifetime
}

// noteFactorFailure counts one failed attempt. It returns false
// once the failures end the pending session, in which case the
// caller destroys it. Every failure is logged; the caller files
// the audit event.
func (app *Application) noteFactorFailure(ctx context.Context, userID int64, what string) bool {
	n := app.sessions.GetInt(ctx, session2FAFailures) + 1
	app.sessions.Put(ctx, session2FAFailures, n)
	logging.Warnf("2fa: %s for user %d (failure %d of %d)", what, userID, n, maxFactorFailures)
	return n < maxFactorFailures
}

// logFactorEvent files one audit row. A write failure is logged,
// never fatal to the ceremony it records.
func (app *Application) logFactorEvent(ctx context.Context, userID int64, kind string) {
	if err := app.queries.InsertWebAuthnEvent(ctx, db.InsertWebAuthnEventParams{UserID: userID, Kind: kind}); err != nil {
		logging.Errorf("2fa: record %s event for user %d: %v", kind, userID, err)
	}
}

// factorDisplayName is the account's name inside ceremonies: the
// acting character's, falling back to the account itself.
func (app *Application) factorDisplayName(ctx context.Context, userID int64) string {
	if name := app.sessions.GetString(ctx, sessionCharacterName); name != "" {
		return name
	}
	return fmt.Sprintf("account %d", userID)
}

// credentialKind is the list's one-word kind for stored
// transports: a platform authenticator is a passkey, anything
// else a security key. Display only.
func credentialKind(transports string) string {
	for _, t := range strings.Split(transports, ",") {
		if strings.TrimSpace(t) == "internal" {
			return "passkey"
		}
	}
	return "security key"
}

// joinTransports stores the library's transport list as one
// comma-joined row value.
func joinTransports(transports []protocol.AuthenticatorTransport) string {
	parts := make([]string, 0, len(transports))
	for _, t := range transports {
		parts = append(parts, string(t))
	}
	return strings.Join(parts, ",")
}

// signCountOK rejects a cloned-key signal: an assertion whose
// sign count did not advance past the stored one. A count of
// zero means the authenticator does not track it, which is not
// a signal. The library's own clone warning rejects the same
// way.
func signCountOK(stored int64, cred *webauthn.Credential) bool {
	if cred.Authenticator.CloneWarning {
		return false
	}
	fresh := int64(cred.Authenticator.SignCount)
	return fresh == 0 || fresh > stored
}

// recoveryCodeAlphabet skips the ambiguous characters (0/O,
// 1/I/L): a code read off a screen must type back in.
const recoveryCodeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

// newRecoveryCode is one code in XXXX-XXXX form.
func newRecoveryCode() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	var sb strings.Builder
	for i, v := range b {
		if i == 4 {
			sb.WriteByte('-')
		}
		sb.WriteByte(recoveryCodeAlphabet[int(v)%len(recoveryCodeAlphabet)])
	}
	return sb.String(), nil
}

// hashRecoveryCode normalizes a code (case, dashes, spaces) and
// hashes it for storage. Only hashes are stored; the codes
// themselves are shown once and never again.
func hashRecoveryCode(code string) string {
	upper := strings.ToUpper(code)
	upper = strings.ReplaceAll(upper, "-", "")
	upper = strings.ReplaceAll(upper, " ", "")
	sum := sha256.Sum256([]byte(upper))
	return hex.EncodeToString(sum[:])
}

// mintRecoveryCodes replaces the account's code set with
// recoveryCodeCount fresh codes and returns them for their one
// showing. The old set dies with the replacement.
func (app *Application) mintRecoveryCodes(ctx context.Context, userID int64) ([]string, error) {
	codes := make([]string, 0, recoveryCodeCount)
	for range recoveryCodeCount {
		code, err := newRecoveryCode()
		if err != nil {
			return nil, err
		}
		codes = append(codes, code)
	}
	if err := app.queries.DeleteWebAuthnRecoveryCodesByUser(ctx, userID); err != nil {
		return nil, err
	}
	for _, code := range codes {
		if _, err := app.queries.CreateWebAuthnRecoveryCode(ctx, db.CreateWebAuthnRecoveryCodeParams{
			UserID: userID, CodeHash: hashRecoveryCode(code),
		}); err != nil {
			return nil, err
		}
	}
	return codes, nil
}

// burnRecoveryCode spends one code: it must match an unused
// stored hash, and it burns on use. The returned bool is
// whether a code was spent.
func (app *Application) burnRecoveryCode(ctx context.Context, userID int64, code string) (bool, error) {
	rows, err := app.queries.ListUnusedWebAuthnRecoveryCodesByUser(ctx, userID)
	if err != nil {
		return false, err
	}
	want := hashRecoveryCode(code)
	for _, row := range rows {
		if subtle.ConstantTimeCompare([]byte(row.CodeHash), []byte(want)) == 1 {
			if err := app.queries.BurnWebAuthnRecoveryCode(ctx, row.ID); err != nil {
				return false, err
			}
			return true, nil
		}
	}
	return false, nil
}

// pruneWebAuthnEvents drops second-factor audit rows past a
// year. The worker runs it every cycle (worker.go).
func (app *Application) pruneWebAuthnEvents(ctx context.Context) {
	n, err := app.queries.PruneWebAuthnEvents(ctx, time.Now().Add(-365*24*time.Hour))
	if err != nil {
		logging.Warnf("2fa: prune events: %v", err)
		return
	}
	if n > 0 {
		logging.Infof("2fa: pruned %d events past a year", n)
	}
}

// factorView is the second-factor page (templates/factor.html):
// the key assertion plus the recovery code form.
type factorView struct {
	Error      string
	HasCodes   bool
	Deadlocked bool // expired or exhausted: sign in again
}

// enrollView is the forced-enrollment page for admins with no
// credential yet (templates/enroll.html).
type enrollView struct {
	Error string
	Host  string
}

// securityCredentialView is one row of the settings key list.
type securityCredentialView struct {
	ID       int64
	Name     string
	Kind     string
	Created  string
	LastUsed string
}

// securityEventView is one row of the settings audit list.
type securityEventView struct {
	Label string
	At    string
}

// securityView is the settings Security section.
type securityView struct {
	Enabled     bool
	IsAdmin     bool
	Fresh       bool
	Host        string
	Credentials []securityCredentialView
	CodesLeft   int64
	Events      []securityEventView
}

// writeFactorJSON answers the ceremony endpoints. Failures
// carry a short code the script turns into a message; nothing
// sensitive crosses in either direction.
func writeFactorJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// factorAuthed is the guard for the factor and enrollment
// endpoints: a signed-in session, pending or fully in. JSON
// endpoints get a 401 body; pages bounce home.
func (app *Application) factorAuthed(w http.ResponseWriter, r *http.Request, json bool) (ctx context.Context, userID int64, ok bool) {
	ctx = r.Context()
	if !app.sessions.GetBool(ctx, sessionAuthenticated) {
		if json {
			writeFactorJSON(w, http.StatusUnauthorized, map[string]string{"error": "signin"})
		} else {
			http.Redirect(w, r, "/", http.StatusSeeOther)
		}
		return ctx, 0, false
	}
	if userID = app.userID(ctx); userID == 0 {
		if json {
			writeFactorJSON(w, http.StatusUnauthorized, map[string]string{"error": "signin"})
		} else {
			http.Redirect(w, r, "/", http.StatusSeeOther)
		}
		return ctx, 0, false
	}
	return ctx, userID, true
}

// handleFactor renders the second-factor page: the key
// assertion and the recovery code form. Pending sessions land
// here after SSO; signed-in ones re-assert here when a
// sensitive action needs a fresh factor.
func (app *Application) handleFactor(w http.ResponseWriter, r *http.Request) {
	ctx, userID, ok := app.factorAuthed(w, r, false)
	if !ok {
		return
	}
	if app.sessions.GetBool(ctx, session2FAPending) {
		if !app.pendingAlive(ctx) {
			app.endPendingSession(ctx)
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		if app.sessions.GetBool(ctx, session2FAMustEnroll) {
			http.Redirect(w, r, "/auth/enroll", http.StatusSeeOther)
			return
		}
	}
	view := &factorView{}
	if n, err := app.queries.CountUnusedWebAuthnRecoveryCodesByUser(ctx, userID); err == nil {
		view.HasCodes = n > 0
	}
	switch r.URL.Query().Get("error") {
	case "code":
		view.Error = "That code did not match. Check it and try again."
	case "fail":
		view.Error = "The key did not verify. Try again."
	case "stale":
		view.Error = "That step expired. Start again."
	}
	data := app.page(ctx)
	data.Factor = view
	data.Scripts = []string{"/static/webauthn.js"}
	app.render(ctx, w, http.StatusOK, "factor.html", data)
}

// endPendingSession destroys a pending session that expired or
// exhausted its attempts.
func (app *Application) endPendingSession(ctx context.Context) {
	if err := app.sessions.Destroy(ctx); err != nil {
		logging.Errorf("2fa: destroy pending session: %v", err)
	}
}

// handleFactorBegin opens an assertion ceremony: the options
// for the account's keys, with user verification required.
func (app *Application) handleFactorBegin(w http.ResponseWriter, r *http.Request) {
	ctx, userID, ok := app.factorAuthed(w, r, true)
	if !ok {
		return
	}
	if app.sessions.GetBool(ctx, session2FAPending) && !app.pendingAlive(ctx) {
		app.endPendingSession(ctx)
		writeFactorJSON(w, http.StatusForbidden, map[string]string{"error": "expired"})
		return
	}
	if app.sessions.GetBool(ctx, session2FAMustEnroll) {
		writeFactorJSON(w, http.StatusForbidden, map[string]string{"error": "state"})
		return
	}
	user, err := app.webauthnUserFor(ctx, userID, app.factorDisplayName(ctx, userID))
	if err != nil {
		logging.Errorf("2fa: load user %d: %v", userID, err)
		writeFactorJSON(w, http.StatusInternalServerError, map[string]string{"error": "store"})
		return
	}
	if len(user.credentials) == 0 {
		writeFactorJSON(w, http.StatusBadRequest, map[string]string{"error": "none"})
		return
	}
	rp, err := webauthnFor(r)
	if err != nil {
		logging.Errorf("2fa: relying party for %q: %v", r.Host, err)
		writeFactorJSON(w, http.StatusInternalServerError, map[string]string{"error": "rp"})
		return
	}
	assertion, session, err := rp.BeginLogin(user, webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		logging.Errorf("2fa: begin assertion for user %d: %v", userID, err)
		writeFactorJSON(w, http.StatusInternalServerError, map[string]string{"error": "begin"})
		return
	}
	if err := app.putCeremony(ctx, session2FALoginChallenge, session); err != nil {
		logging.Errorf("2fa: store challenge for user %d: %v", userID, err)
		writeFactorJSON(w, http.StatusInternalServerError, map[string]string{"error": "store"})
		return
	}
	writeFactorJSON(w, http.StatusOK, assertion)
}

// handleFactorFinish closes an assertion ceremony: a verified
// key satisfies the factor. A failure counts toward ending the
// pending session.
func (app *Application) handleFactorFinish(w http.ResponseWriter, r *http.Request) {
	ctx, userID, ok := app.factorAuthed(w, r, true)
	if !ok {
		return
	}
	pending := app.sessions.GetBool(ctx, session2FAPending)
	if pending && !app.pendingAlive(ctx) {
		app.endPendingSession(ctx)
		writeFactorJSON(w, http.StatusForbidden, map[string]string{"error": "expired"})
		return
	}
	fail := func() {
		app.logFactorEvent(ctx, userID, factorEventAssertBad)
		if !app.noteFactorFailure(ctx, userID, "assertion failed") {
			app.endPendingSession(ctx)
			writeFactorJSON(w, http.StatusForbidden, map[string]string{"error": "locked"})
			return
		}
		writeFactorJSON(w, http.StatusBadRequest, map[string]string{"error": "verify"})
	}
	if app.sessions.GetBool(ctx, session2FAMustEnroll) {
		writeFactorJSON(w, http.StatusForbidden, map[string]string{"error": "state"})
		return
	}
	session, ok := app.popCeremony(ctx, session2FALoginChallenge)
	if !ok {
		fail()
		return
	}
	user, err := app.webauthnUserFor(ctx, userID, app.factorDisplayName(ctx, userID))
	if err != nil {
		logging.Errorf("2fa: load user %d: %v", userID, err)
		writeFactorJSON(w, http.StatusInternalServerError, map[string]string{"error": "store"})
		return
	}
	rp, err := webauthnFor(r)
	if err != nil {
		logging.Errorf("2fa: relying party for %q: %v", r.Host, err)
		writeFactorJSON(w, http.StatusInternalServerError, map[string]string{"error": "rp"})
		return
	}
	cred, err := rp.FinishLogin(user, session, r)
	if err != nil {
		fail()
		return
	}
	stored, err := app.queries.GetWebAuthnCredential(ctx, cred.ID)
	if err != nil {
		logging.Errorf("2fa: unknown credential asserted for user %d", userID)
		fail()
		return
	}
	if !signCountOK(stored.SignCount, cred) {
		logging.Warnf("2fa: sign count regression on credential %d for user %d (stored %d, got %d): possible clone",
			stored.ID, userID, stored.SignCount, cred.Authenticator.SignCount)
		fail()
		return
	}
	if err := app.queries.UpdateWebAuthnCredentialSignCount(ctx, db.UpdateWebAuthnCredentialSignCountParams{
		CredentialID: cred.ID, SignCount: int64(cred.Authenticator.SignCount),
	}); err != nil {
		logging.Errorf("2fa: store sign count for user %d: %v", userID, err)
	}
	app.logFactorEvent(ctx, userID, factorEventAssertOK)
	app.satisfyFactor(ctx)
	redirect := "/settings#security"
	if pending {
		redirect = app.takeReturnTo(ctx)
	}
	writeFactorJSON(w, http.StatusOK, map[string]string{"redirect": redirect})
}

// handleFactorRecover spends one recovery code: it satisfies
// the factor once and burns. A wrong code counts like any
// other failed attempt.
func (app *Application) handleFactorRecover(w http.ResponseWriter, r *http.Request) {
	ctx, userID, ok := app.factorAuthed(w, r, false)
	if !ok {
		return
	}
	pending := app.sessions.GetBool(ctx, session2FAPending)
	if pending && !app.pendingAlive(ctx) {
		app.endPendingSession(ctx)
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if app.sessions.GetBool(ctx, session2FAMustEnroll) {
		http.Redirect(w, r, "/auth/enroll", http.StatusSeeOther)
		return
	}
	fail := func() {
		app.logFactorEvent(ctx, userID, factorEventAssertBad)
		if !app.noteFactorFailure(ctx, userID, "recovery code failed") {
			app.endPendingSession(ctx)
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, "/auth/factor?error=code", http.StatusSeeOther)
	}
	if err := r.ParseForm(); err != nil {
		fail()
		return
	}
	spent, err := app.burnRecoveryCode(ctx, userID, r.Form.Get("code"))
	if err != nil {
		logging.Errorf("2fa: burn recovery code for user %d: %v", userID, err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if !spent {
		time.Sleep(500 * time.Millisecond)
		fail()
		return
	}
	app.logFactorEvent(ctx, userID, factorEventRecover)
	app.satisfyFactor(ctx)
	redirect := "/settings#security"
	if pending {
		redirect = app.takeReturnTo(ctx)
	}
	http.Redirect(w, r, redirect, http.StatusSeeOther)
}

// beginRegistration opens a registration ceremony for one new
// key called name: the creation options, with user verification
// required and no attestation. The account's own keys are
// excluded, so a key cannot be enrolled twice.
func (app *Application) beginRegistration(ctx context.Context, r *http.Request, userID int64, name string) (*protocol.CredentialCreation, error) {
	user, err := app.webauthnUserFor(ctx, userID, app.factorDisplayName(ctx, userID))
	if err != nil {
		return nil, err
	}
	rp, err := webauthnFor(r)
	if err != nil {
		return nil, err
	}
	creation, session, err := rp.BeginRegistration(user,
		webauthn.WithConveyancePreference(protocol.PreferNoAttestation),
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			UserVerification: protocol.VerificationRequired,
		}),
		webauthn.WithExclusions(webauthn.Credentials(user.credentials).CredentialDescriptors()),
	)
	if err != nil {
		return nil, err
	}
	if err := app.putCeremony(ctx, session2FARegChallenge, session); err != nil {
		return nil, err
	}
	app.sessions.Put(ctx, session2FARegName, name)
	return creation, nil
}

// finishRegistration closes a registration ceremony and stores
// the key. First reports whether this is the account's first
// credential, which is what mints the recovery codes.
func (app *Application) finishRegistration(ctx context.Context, r *http.Request, userID int64) (first bool, err error) {
	session, ok := app.popCeremony(ctx, session2FARegChallenge)
	if !ok {
		return false, errFactorStale
	}
	name := app.sessions.PopString(ctx, session2FARegName)
	user, err := app.webauthnUserFor(ctx, userID, app.factorDisplayName(ctx, userID))
	if err != nil {
		return false, err
	}
	rp, err := webauthnFor(r)
	if err != nil {
		return false, err
	}
	cred, err := rp.FinishRegistration(user, session, r)
	if err != nil {
		return false, err
	}
	if _, err := app.queries.GetWebAuthnCredential(ctx, cred.ID); err == nil {
		return false, errFactorExists
	}
	if name == "" {
		name = "Security key"
	}
	if _, err := app.queries.CreateWebAuthnCredential(ctx, db.CreateWebAuthnCredentialParams{
		UserID:            userID,
		CredentialID:      cred.ID,
		PublicKey:         cred.PublicKey,
		SignCount:         int64(cred.Authenticator.SignCount),
		Transports:        joinTransports(cred.Transport),
		AttestationFormat: cred.AttestationFormat,
		Name:              name,
	}); err != nil {
		return false, err
	}
	app.logFactorEvent(ctx, userID, factorEventEnroll)
	n, err := app.queries.CountWebAuthnCredentialsByUser(ctx, userID)
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// errFactorStale is an expired or missing ceremony; the script
// starts over.
var errFactorStale = fmt.Errorf("2fa: ceremony expired")

// errFactorExists is a key that is already enrolled.
var errFactorExists = fmt.Errorf("2fa: credential already enrolled")

// factorRegName reads the credential name off a begin body:
// trimmed, capped, defaulted.
func factorRegName(r *http.Request) string {
	var body struct {
		Name string `json:"name"`
	}
	json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body)
	name := strings.TrimSpace(body.Name)
	if len(name) > 64 {
		name = name[:64]
	}
	if name == "" {
		name = "Security key"
	}
	return name
}

// enrollAuthed is the guard for forced enrollment: a pending
// admin session with no credential yet.
func (app *Application) enrollAuthed(w http.ResponseWriter, r *http.Request, json bool) (ctx context.Context, userID int64, ok bool) {
	ctx, userID, ok = app.factorAuthed(w, r, json)
	if !ok {
		return ctx, 0, false
	}
	if !app.sessions.GetBool(ctx, session2FAPending) {
		if json {
			writeFactorJSON(w, http.StatusForbidden, map[string]string{"error": "state"})
		} else {
			http.Redirect(w, r, "/", http.StatusSeeOther)
		}
		return ctx, 0, false
	}
	if !app.sessions.GetBool(ctx, session2FAMustEnroll) {
		if json {
			writeFactorJSON(w, http.StatusForbidden, map[string]string{"error": "state"})
		} else {
			http.Redirect(w, r, "/auth/factor", http.StatusSeeOther)
		}
		return ctx, 0, false
	}
	if !app.pendingAlive(ctx) {
		app.endPendingSession(ctx)
		if json {
			writeFactorJSON(w, http.StatusForbidden, map[string]string{"error": "expired"})
		} else {
			http.Redirect(w, r, "/", http.StatusSeeOther)
		}
		return ctx, 0, false
	}
	return ctx, userID, true
}

// handleEnroll renders forced enrollment: the explainer, the
// key ceremony, and the recovery codes with their confirm step.
func (app *Application) handleEnroll(w http.ResponseWriter, r *http.Request) {
	ctx, _, ok := app.enrollAuthed(w, r, false)
	if !ok {
		return
	}
	data := app.page(ctx)
	data.Enroll = &enrollView{Host: r.Host}
	data.Scripts = []string{"/static/webauthn.js"}
	app.render(ctx, w, http.StatusOK, "enroll.html", data)
}

// handleEnrollBegin opens the forced-enrollment ceremony.
func (app *Application) handleEnrollBegin(w http.ResponseWriter, r *http.Request) {
	ctx, userID, ok := app.enrollAuthed(w, r, true)
	if !ok {
		return
	}
	creation, err := app.beginRegistration(ctx, r, userID, factorRegName(r))
	if err != nil {
		logging.Errorf("2fa: begin enrollment for user %d: %v", userID, err)
		writeFactorJSON(w, http.StatusInternalServerError, map[string]string{"error": "begin"})
		return
	}
	writeFactorJSON(w, http.StatusOK, creation)
}

// handleEnrollFinish closes the forced-enrollment ceremony.
// The first key mints the recovery codes for their one
// showing; the session stays pending until they are confirmed
// saved.
func (app *Application) handleEnrollFinish(w http.ResponseWriter, r *http.Request) {
	ctx, userID, ok := app.enrollAuthed(w, r, true)
	if !ok {
		return
	}
	first, err := app.finishRegistration(ctx, r, userID)
	switch {
	case err == errFactorStale:
		writeFactorJSON(w, http.StatusBadRequest, map[string]string{"error": "stale"})
		return
	case err == errFactorExists:
		writeFactorJSON(w, http.StatusBadRequest, map[string]string{"error": "exists"})
		return
	case err != nil:
		logging.Warnf("2fa: finish enrollment for user %d: %v", userID, err)
		writeFactorJSON(w, http.StatusBadRequest, map[string]string{"error": "verify"})
		return
	}
	if !first {
		app.satisfyFactor(ctx)
		writeFactorJSON(w, http.StatusOK, map[string]string{"redirect": app.takeReturnTo(ctx)})
		return
	}
	codes, err := app.mintRecoveryCodes(ctx, userID)
	if err != nil {
		logging.Errorf("2fa: mint recovery codes for user %d: %v", userID, err)
		writeFactorJSON(w, http.StatusInternalServerError, map[string]string{"error": "store"})
		return
	}
	writeFactorJSON(w, http.StatusOK, map[string]any{"codes": codes})
}

// handleEnrollConfirm records that the codes were saved: the
// forced enrollment is done and the sign-in completes.
func (app *Application) handleEnrollConfirm(w http.ResponseWriter, r *http.Request) {
	ctx, userID, ok := app.enrollAuthed(w, r, true)
	if !ok {
		return
	}
	n, err := app.queries.CountWebAuthnCredentialsByUser(ctx, userID)
	if err != nil || n == 0 {
		writeFactorJSON(w, http.StatusBadRequest, map[string]string{"error": "state"})
		return
	}
	app.satisfyFactor(ctx)
	writeFactorJSON(w, http.StatusOK, map[string]string{"redirect": app.takeReturnTo(ctx)})
}

// securityCodesView is the shown-once recovery code page
// (templates/security_codes.html). It renders only as a POST
// response, never from a GET.
type securityCodesView struct {
	Codes []string
	Title string
}

// securityViewFor builds the settings Security section: the
// key list, the codes remaining, and the recent audit trail.
func (app *Application) securityViewFor(ctx context.Context, r *http.Request, userID int64) *securityView {
	view := &securityView{Host: r.Host, Fresh: app.factorFresh(ctx), IsAdmin: app.isAdmin(ctx)}
	rows, err := app.queries.ListWebAuthnCredentialsByUser(ctx, userID)
	if err != nil {
		logging.Errorf("2fa: list credentials for user %d: %v", userID, err)
		return view
	}
	view.Enabled = len(rows) > 0
	for _, row := range rows {
		last := "never used"
		if row.LastUsedAt.Valid {
			last = row.LastUsedAt.Time.Format("2 Jan 2006, 15:04")
		}
		view.Credentials = append(view.Credentials, securityCredentialView{
			ID: row.ID, Name: row.Name, Kind: credentialKind(row.Transports),
			Created: row.CreatedAt.Format("2 Jan 2006, 15:04"), LastUsed: last,
		})
	}
	if n, err := app.queries.CountUnusedWebAuthnRecoveryCodesByUser(ctx, userID); err == nil {
		view.CodesLeft = n
	}
	if events, err := app.queries.ListWebAuthnEventsByUser(ctx, userID); err == nil {
		for _, ev := range events {
			view.Events = append(view.Events, securityEventView{
				Label: factorEventLabel(ev.Kind), At: ev.CreatedAt.Format("2 Jan 2006, 15:04"),
			})
		}
	}
	return view
}

// securityAuthed is the guard for the settings security
// endpoints: signed in and fully through the factor (the
// requireAuth gate keeps pending sessions off /settings, and
// this refuses them too for the endpoints mounted elsewhere).
func (app *Application) securityAuthed(w http.ResponseWriter, r *http.Request, json bool) (ctx context.Context, userID int64, ok bool) {
	ctx, userID, ok = app.factorAuthed(w, r, json)
	if !ok {
		return ctx, 0, false
	}
	if app.sessions.GetBool(ctx, session2FAPending) {
		if json {
			writeFactorJSON(w, http.StatusForbidden, map[string]string{"error": "pending"})
		} else {
			http.Redirect(w, r, "/auth/factor", http.StatusSeeOther)
		}
		return ctx, 0, false
	}
	return ctx, userID, true
}

// securityNeedsFresh refuses a sensitive action whose factor
// is older than factorFreshFor. Adding the very first key
// cannot need it: there is nothing to assert with yet.
func (app *Application) securityNeedsFresh(w http.ResponseWriter, r *http.Request, ctx context.Context, userID int64, json bool) bool {
	n, err := app.queries.CountWebAuthnCredentialsByUser(ctx, userID)
	if err != nil {
		if json {
			writeFactorJSON(w, http.StatusInternalServerError, map[string]string{"error": "store"})
		} else {
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
		return true
	}
	if n == 0 || app.factorFresh(ctx) {
		return false
	}
	if json {
		writeFactorJSON(w, http.StatusForbidden, map[string]string{"error": "stale"})
	} else {
		http.Redirect(w, r, "/auth/factor", http.StatusSeeOther)
	}
	return true
}

// handleSecurityKeysBegin opens a settings registration
// ceremony for one more key.
func (app *Application) handleSecurityKeysBegin(w http.ResponseWriter, r *http.Request) {
	ctx, userID, ok := app.securityAuthed(w, r, true)
	if !ok {
		return
	}
	if app.securityNeedsFresh(w, r, ctx, userID, true) {
		return
	}
	creation, err := app.beginRegistration(ctx, r, userID, factorRegName(r))
	if err != nil {
		logging.Errorf("2fa: begin registration for user %d: %v", userID, err)
		writeFactorJSON(w, http.StatusInternalServerError, map[string]string{"error": "begin"})
		return
	}
	writeFactorJSON(w, http.StatusOK, creation)
}

// handleSecurityKeysFinish closes a settings registration
// ceremony. A first key enables 2FA and mints the recovery
// codes for their one showing.
func (app *Application) handleSecurityKeysFinish(w http.ResponseWriter, r *http.Request) {
	ctx, userID, ok := app.securityAuthed(w, r, true)
	if !ok {
		return
	}
	first, err := app.finishRegistration(ctx, r, userID)
	switch {
	case err == errFactorStale:
		writeFactorJSON(w, http.StatusBadRequest, map[string]string{"error": "stale"})
		return
	case err == errFactorExists:
		writeFactorJSON(w, http.StatusBadRequest, map[string]string{"error": "exists"})
		return
	case err != nil:
		logging.Warnf("2fa: finish registration for user %d: %v", userID, err)
		writeFactorJSON(w, http.StatusBadRequest, map[string]string{"error": "verify"})
		return
	}
	if !first {
		writeFactorJSON(w, http.StatusOK, map[string]string{"redirect": "/settings#security"})
		return
	}
	codes, err := app.mintRecoveryCodes(ctx, userID)
	if err != nil {
		logging.Errorf("2fa: mint recovery codes for user %d: %v", userID, err)
		writeFactorJSON(w, http.StatusInternalServerError, map[string]string{"error": "store"})
		return
	}
	app.satisfyFactor(ctx)
	writeFactorJSON(w, http.StatusOK, map[string]any{"codes": codes})
}

// handleSecurityKeysRemove drops one key. Dropping the last
// one disables 2FA, which only non-admin accounts may do;
// admin accounts replace but never drop below one.
func (app *Application) handleSecurityKeysRemove(w http.ResponseWriter, r *http.Request) {
	ctx, userID, ok := app.securityAuthed(w, r, false)
	if !ok {
		return
	}
	if app.securityNeedsFresh(w, r, ctx, userID, false) {
		return
	}
	back := func() {
		http.Redirect(w, r, "/settings#security", http.StatusSeeOther)
	}
	if err := r.ParseForm(); err != nil {
		back()
		return
	}
	id, err := strconv.ParseInt(r.Form.Get("id"), 10, 64)
	if err != nil || id <= 0 {
		back()
		return
	}
	n, err := app.queries.CountWebAuthnCredentialsByUser(ctx, userID)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if n <= 1 && app.isAdmin(ctx) {
		http.Error(w, "Admin accounts keep at least one key.", http.StatusForbidden)
		return
	}
	if err := app.queries.DeleteWebAuthnCredential(ctx, db.DeleteWebAuthnCredentialParams{ID: id, UserID: userID}); err != nil {
		logging.Errorf("2fa: remove credential %d for user %d: %v", id, userID, err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	app.logFactorEvent(ctx, userID, factorEventRemove)
	if n <= 1 {
		if err := app.queries.DeleteWebAuthnRecoveryCodesByUser(ctx, userID); err != nil {
			logging.Errorf("2fa: clear recovery codes for user %d: %v", userID, err)
		}
	}
	back()
}

// handleSecurityCodesRegenerate replaces the code set and
// shows the new codes once.
func (app *Application) handleSecurityCodesRegenerate(w http.ResponseWriter, r *http.Request) {
	ctx, userID, ok := app.securityAuthed(w, r, false)
	if !ok {
		return
	}
	if app.securityNeedsFresh(w, r, ctx, userID, false) {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/settings#security", http.StatusSeeOther)
		return
	}
	codes, err := app.mintRecoveryCodes(ctx, userID)
	if err != nil {
		logging.Errorf("2fa: regenerate recovery codes for user %d: %v", userID, err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	app.logFactorEvent(ctx, userID, factorEventRegen)
	data := app.page(ctx)
	data.SecurityCodes = &securityCodesView{Codes: codes, Title: "New recovery codes"}
	app.render(ctx, w, http.StatusOK, "security_codes.html", data)
}

// signInTarget is where a finished SSO sign-in goes: the
// factor or enrollment page for a pending session (the kept
// return page waits for the assertion), else the kept page.
func (app *Application) signInTarget(ctx context.Context) string {
	if app.sessions.GetBool(ctx, session2FAPending) {
		if app.sessions.GetBool(ctx, session2FAMustEnroll) {
			return "/auth/enroll"
		}
		return "/auth/factor"
	}
	return app.takeReturnTo(ctx)
}
