package app

// PKCE (RFC 7636) binds an authorization code to the browser
// session that asked for it: every authorize redirect carries the
// S256 challenge of a verifier held server-side in the session,
// and the callback presents that verifier at the code exchange.
// These tests pin the derivation, the redirect/session pairing,
// the exchange parameter, and the refusal to exchange without a
// verifier.

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestPKCEChallengeMatchesRFC7636Vector(t *testing.T) {
	// RFC 7636 Appendix B.
	const verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	const want = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	if got := pkceChallenge(verifier); got != want {
		t.Fatalf("pkceChallenge(RFC vector) = %q, want %q", got, want)
	}
}

func TestPKCEVerifierShape(t *testing.T) {
	v1, err := newPKCEVerifier()
	if err != nil {
		t.Fatalf("newPKCEVerifier: %v", err)
	}
	// 32 random bytes base64url-encoded without padding: 43
	// characters from the RFC 7636 unreserved set.
	if len(v1) != 43 {
		t.Fatalf("verifier length = %d, want 43 (%q)", len(v1), v1)
	}
	raw, err := base64.RawURLEncoding.DecodeString(v1)
	if err != nil || len(raw) != 32 {
		t.Fatalf("verifier decodes to %d bytes, err %v; want 32 bytes", len(raw), err)
	}
	v2, err := newPKCEVerifier()
	if err != nil {
		t.Fatalf("newPKCEVerifier: %v", err)
	}
	if v1 == v2 {
		t.Fatalf("two verifiers are identical: %q", v1)
	}
}

// pkceSessionCookie extracts the session cookie a response set.
func pkceSessionCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == "evesynapse_session" && c.Value != "" {
			return c
		}
	}
	t.Fatalf("response set no session cookie")
	return nil
}

// TestAuthorizeRedirectsCarryPKCE drives both SSO entry points
// (the identity login and the full link): each redirect must
// carry a state and an S256 challenge, and the session must hold
// the verifier that challenge was derived from.
func TestAuthorizeRedirectsCarryPKCE(t *testing.T) {
	for _, path := range []string{"/auth/login", "/auth/eve"} {
		app := linkApp(t)
		rec := get(app, nil, path)
		if rec.Code != http.StatusFound {
			t.Fatalf("GET %s: status %d, want 302", path, rec.Code)
		}
		loc, err := url.Parse(rec.Header().Get("Location"))
		if err != nil || loc.Host != "login.eveonline.com" {
			t.Fatalf("GET %s: redirect = %q", path, rec.Header().Get("Location"))
		}
		q := loc.Query()
		state, challenge := q.Get("state"), q.Get("code_challenge")
		if state == "" || challenge == "" {
			t.Fatalf("GET %s: redirect lacks state or code_challenge: %q", path, loc)
		}
		if m := q.Get("code_challenge_method"); m != "S256" {
			t.Fatalf("GET %s: code_challenge_method = %q, want S256", path, m)
		}

		cookie := pkceSessionCookie(t, rec)
		lctx, err := app.sessions.Load(context.Background(), cookie.Value)
		if err != nil {
			t.Fatalf("GET %s: reload session: %v", path, err)
		}
		if got := app.sessions.GetString(lctx, sessionOAuthState); got != state {
			t.Fatalf("GET %s: session state %q does not match redirect state %q", path, got, state)
		}
		verifier := app.sessions.GetString(lctx, sessionOAuthVerifier)
		if verifier == "" {
			t.Fatalf("GET %s: session holds no PKCE verifier", path)
		}
		if got := pkceChallenge(verifier); got != challenge {
			t.Fatalf("GET %s: challenge %q is not S256 of the session verifier (derived %q)", path, challenge, got)
		}
	}
}

// pkceTokenStub stands in for CCP's endpoints during a callback:
// the token endpoint records its form and answers a token set;
// anything else (discovery, JWKS) fails, which is fine — the
// exchange has already happened by then.
type pkceTokenStub struct {
	calls int
	form  url.Values
}

func (s *pkceTokenStub) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasSuffix(req.URL.Path, "/oauth/token") {
		body, _ := io.ReadAll(req.Body)
		s.form, _ = url.ParseQuery(string(body))
		s.calls++
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"access_token":"not-a-jwt","token_type":"Bearer","expires_in":1200,"refresh_token":"fixture-refresh"}`)),
			Request:    req,
		}, nil
	}
	return &http.Response{
		StatusCode: http.StatusInternalServerError,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"error":"stub"}`)),
		Request:    req,
	}, nil
}

func stubLoginClient(t *testing.T, stub *pkceTokenStub) {
	t.Helper()
	old := loginHTTPClient
	loginHTTPClient = &http.Client{Transport: stub}
	t.Cleanup(func() { loginHTTPClient = old })
}

// TestCallbackExchangeSendsPKCEVerifier runs a full round trip:
// start the login, then present the returned code and state to
// the callback. The token endpoint must be presented the session's
// verifier — the one whose challenge the authorize redirect
// carried — alongside the code.
func TestCallbackExchangeSendsPKCEVerifier(t *testing.T) {
	app := linkApp(t)
	stub := &pkceTokenStub{}
	stubLoginClient(t, stub)

	rec := get(app, nil, "/auth/login")
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse authorize redirect: %v", err)
	}
	state, challenge := loc.Query().Get("state"), loc.Query().Get("code_challenge")
	cookie := pkceSessionCookie(t, rec)

	rec = get(app, cookie, "/auth/callback?code=fixture-code&state="+state)
	// The stub's access token is not a real JWT, so the flow ends
	// at verification — proof it got past the exchange.
	if got := rec.Header().Get("Location"); got != "/?error=verify" {
		t.Fatalf("callback redirect = %q, want /?error=verify (past the exchange)", got)
	}
	if stub.calls != 1 {
		t.Fatalf("token endpoint calls = %d, want 1", stub.calls)
	}
	if got := stub.form.Get("code"); got != "fixture-code" {
		t.Fatalf("exchange presented code %q, want fixture-code", got)
	}
	verifier := stub.form.Get("code_verifier")
	if verifier == "" {
		t.Fatalf("exchange presented no code_verifier; form: %v", stub.form)
	}
	if got := pkceChallenge(verifier); got != challenge {
		t.Fatalf("exchange verifier derives challenge %q, want the authorize challenge %q", got, challenge)
	}

	// Both secrets are single-use: the session no longer holds
	// either after the callback.
	lctx, err := app.sessions.Load(context.Background(), cookie.Value)
	if err != nil {
		t.Fatalf("reload session: %v", err)
	}
	if got := app.sessions.GetString(lctx, sessionOAuthVerifier); got != "" {
		t.Fatalf("verifier survived the callback: %q", got)
	}
	if got := app.sessions.GetString(lctx, sessionOAuthState); got != "" {
		t.Fatalf("state survived the callback: %q", got)
	}
}

// TestCallbackWithoutVerifierDoesNotExchange presents a callback
// whose session holds a valid state but no PKCE verifier (a flow
// that did not start at the authorize redirect): the callback
// must refuse before any token-endpoint call.
func TestCallbackWithoutVerifierDoesNotExchange(t *testing.T) {
	app := linkApp(t)
	stub := &pkceTokenStub{}
	stubLoginClient(t, stub)

	lctx, err := app.sessions.Load(context.Background(), "")
	if err != nil {
		t.Fatalf("load session: %v", err)
	}
	app.sessions.Put(lctx, sessionOAuthState, "fixture-state")
	token, _, err := app.sessions.Commit(lctx)
	if err != nil {
		t.Fatalf("commit session: %v", err)
	}
	cookie := &http.Cookie{Name: "evesynapse_session", Value: token, Path: "/"}

	rec := get(app, cookie, "/auth/callback?code=fixture-code&state=fixture-state")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("callback status = %d, want 303", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/?error=exchange" {
		t.Fatalf("callback redirect = %q, want /?error=exchange", got)
	}
	if stub.calls != 0 {
		t.Fatalf("token endpoint calls = %d, want 0 without a verifier", stub.calls)
	}
}
