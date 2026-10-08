package app

// Tests for the browser-facing protections (httpsec.go): the
// security headers on every response, the session cookie's
// attributes, the cross-site request guard, and sign-out as a POST.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestPublicOriginFromCallbackURL(t *testing.T) {
	cases := []struct {
		callback string
		origin   string
		tls      bool
		local    bool
	}{
		{"https://eve.example.org/auth/callback", "https://eve.example.org", true, false},
		{"https://eve.example.org:8443/auth/callback", "https://eve.example.org:8443", true, false},
		{"http://eve.example.org/auth/callback", "http://eve.example.org", false, false},
		{"http://localhost:8080/auth/callback", "http://localhost:8080", false, true},
		{"http://127.0.0.1:8080/auth/callback", "http://127.0.0.1:8080", false, true},
		{"http://[::1]:8080/auth/callback", "http://[::1]:8080", false, true},
		{"http://app.localhost:8080/auth/callback", "http://app.localhost:8080", false, true},
		{"", "", false, false},
		{"not a url", "", false, false},
		{"ftp://eve.example.org/x", "", false, false},
	}
	for _, tc := range cases {
		cfg := Config{eveCallbackURL: tc.callback}
		if got := cfg.publicOrigin(); got != tc.origin {
			t.Errorf("publicOrigin(%q) = %q, want %q", tc.callback, got, tc.origin)
		}
		if got := cfg.servedOverTLS(); got != tc.tls {
			t.Errorf("servedOverTLS(%q) = %v, want %v", tc.callback, got, tc.tls)
		}
		if got := cfg.publicHostIsLocal(); got != tc.local {
			t.Errorf("publicHostIsLocal(%q) = %v, want %v", tc.callback, got, tc.local)
		}
	}
}

func TestSessionCookieAttributes(t *testing.T) {
	overTLS := newSessionManager(nil, Config{eveCallbackURL: "https://eve.example.org/auth/callback"})
	if !overTLS.Cookie.Secure {
		t.Error("the session cookie is not Secure on an https site")
	}
	if !overTLS.Cookie.HttpOnly || overTLS.Cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("session cookie: HttpOnly=%v SameSite=%v, want HttpOnly and Lax", overTLS.Cookie.HttpOnly, overTLS.Cookie.SameSite)
	}
	// Local development over plain http must still be able to
	// sign in: a Secure cookie would never be sent back.
	local := newSessionManager(nil, Config{eveCallbackURL: "http://localhost:8080/auth/callback"})
	if local.Cookie.Secure {
		t.Error("the session cookie is Secure on a plain-http site, so it would never be sent")
	}
}

func TestSecurityHeaders(t *testing.T) {
	app, _, _ := buildCorpTestApp(t, &countingTransport{})

	get := func() http.Header {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec.Header()
	}

	h := get()
	for name, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "same-origin",
	} {
		if got := h.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	csp := h.Get("Content-Security-Policy")
	for _, want := range []string{
		"default-src 'self'",
		"script-src 'self'",
		"style-src 'self'",
		"img-src 'self' data: https://images.evetech.net",
		"frame-ancestors 'none'",
		"form-action 'self'",
		"base-uri 'self'",
		"object-src 'none'",
	} {
		if !strings.Contains(csp, want) {
			t.Errorf("Content-Security-Policy %q is missing %q", csp, want)
		}
	}
	// Every script is a versioned file under /static/ and no
	// template carries inline scripts, style attributes, or event
	// handlers, so the policy needs no 'unsafe-inline' anywhere.
	// An inline block added back to a template must trip this
	// test, not slide by.
	if strings.Contains(csp, "unsafe-inline") {
		t.Errorf("Content-Security-Policy %q contains 'unsafe-inline'", csp)
	}
	// Every font is served by the app (the wordmark's too), so no
	// third party is allowed to supply styles or fonts.
	for _, banned := range []string{"googleapis", "gstatic"} {
		if strings.Contains(csp, banned) {
			t.Errorf("Content-Security-Policy %q still allows %s", csp, banned)
		}
	}
	// HSTS only makes sense, and is only sent, on an https site.
	if got := h.Get("Strict-Transport-Security"); got != "" {
		t.Errorf("Strict-Transport-Security = %q on a site with no https address", got)
	}
	app.cfg.eveCallbackURL = "https://eve.example.org/auth/callback"
	if got, want := get().Get("Strict-Transport-Security"), "max-age=31536000; includeSubDomains"; got != want {
		t.Errorf("Strict-Transport-Security = %q on an https site, want %q", got, want)
	}
}

// TestCrossSiteRequestsAreRefused drives a state-changing route the
// way a browser would from this site, from another site, and the way
// a script would, and checks only the cross-site ones are stopped.
func TestCrossSiteRequestsAreRefused(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")

	post := func(host string, headers map[string]string) int {
		form := url.Values{"character_id": {"90000001"}, "tags": {"scout"}}
		req := httptest.NewRequest(http.MethodPost, "/characters/tags", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if host != "" {
			req.Host = host
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec.Code
	}

	cases := []struct {
		name    string
		host    string
		headers map[string]string
		refused bool
	}{
		{"this site (modern browser)", "", map[string]string{"Sec-Fetch-Site": "same-origin"}, false},
		{"this site (Origin matches Host)", "eve.example.org", map[string]string{"Origin": "https://eve.example.org"}, false},
		{"typed address or bookmark", "", map[string]string{"Sec-Fetch-Site": "none"}, false},
		{"a script, no browser headers", "", nil, false},
		{"another site (modern browser)", "", map[string]string{"Sec-Fetch-Site": "cross-site"}, true},
		{"a sibling subdomain", "", map[string]string{"Sec-Fetch-Site": "same-site"}, true},
		{"another site (Origin differs from Host)", "eve.example.org", map[string]string{"Origin": "https://evil.example"}, true},
	}
	for _, tc := range cases {
		code := post(tc.host, tc.headers)
		if refused := code == http.StatusForbidden; refused != tc.refused {
			t.Errorf("%s: status %d, refused=%v, want refused=%v", tc.name, code, refused, tc.refused)
		}
	}

	// Behind a proxy that rewrites Host, the browser's Origin is the
	// site's public address and no longer matches Host. That address
	// is trusted as the site's own.
	if code := post("127.0.0.1:8080", map[string]string{"Origin": "https://eve.example.org"}); code != http.StatusForbidden {
		t.Fatalf("Origin differing from Host with no public address configured: status %d, want 403", code)
	}
	app.cfg.eveCallbackURL = "https://eve.example.org/auth/callback"
	if code := post("127.0.0.1:8080", map[string]string{"Origin": "https://eve.example.org"}); code == http.StatusForbidden {
		t.Fatal("a request from the site's own public address was refused behind a Host-rewriting proxy")
	}
	if code := post("127.0.0.1:8080", map[string]string{"Origin": "https://evil.example"}); code != http.StatusForbidden {
		t.Fatalf("another site behind the proxy: status %d, want 403", code)
	}

	// Reads are never refused, wherever they come from.
	req := httptest.NewRequest(http.MethodGet, "/characters/", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("cross-site GET: status %d, want 200", rec.Code)
	}
}

// TestSignOutIsAPost: a link (or an image tag) on another page must
// not be able to sign the visitor out, so GET no longer does it.
func TestSignOutIsAPost(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")

	if code, _, _ := doReq(t, app, http.MethodGet, "/auth/logout", nil, cookie); code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /auth/logout: status %d, want 405", code)
	}
	if code, _ := getPage(t, app, cookie, "/characters/"); code != http.StatusOK {
		t.Fatalf("still signed in after the refused GET: /characters/ status %d, want 200", code)
	}

	// The page offers sign-out as a form posting to it.
	_, body := getPage(t, app, cookie, "/characters/")
	mustContain(t, "/characters/", body, `<form class="sidebar-signout-form" method="post" action="/auth/logout">`)

	if code, _, _ := doReq(t, app, http.MethodPost, "/auth/logout", url.Values{}, cookie); code != http.StatusSeeOther {
		t.Fatalf("POST /auth/logout: status %d, want 303", code)
	}
	// The session is gone server-side: the old cookie no longer
	// opens a signed-in page.
	if code, _ := getPage(t, app, cookie, "/characters/"); code != http.StatusSeeOther {
		t.Fatalf("after sign-out: /characters/ status %d, want the 303 home", code)
	}
}
