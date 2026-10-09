package app

// Tests for how static assets are cached and compressed (core_static.go),
// for which pages load the fitting script, and for the parsed
// template cache behind every render.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestStaticAssetCaching(t *testing.T) {
	app, _, _ := buildCorpTestApp(t, &countingTransport{})
	get := func(path string, headers map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec
	}

	version := staticAssetVersion()
	if len(version) != 12 {
		t.Fatalf("asset version %q, want 12 hex characters", version)
	}

	// The versioned address names exactly these bytes for good.
	rec := get("/static/style.css?v="+version, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("versioned stylesheet: status %d", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != cacheForever {
		t.Errorf("versioned stylesheet Cache-Control = %q, want %q", got, cacheForever)
	}
	plainSize := rec.Body.Len()

	// The head bootstrap serves versioned and immutable, like the
	// other scripts, and carries the nav/theme first-paint code
	// that used to be inline in every page.
	rec = get("/static/boot.js?v="+version, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("versioned boot script: status %d", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != cacheForever {
		t.Errorf("versioned boot script Cache-Control = %q, want %q", got, cacheForever)
	}
	for _, want := range []string{"evesynapse-nav", "evesynapse-theme"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("boot.js is missing %q", want)
		}
	}

	// Without the current version the file is revalidated, and has
	// an ETag to revalidate against.
	for _, path := range []string{"/static/style.css", "/static/style.css?v=an-older-build", "/static/app.js", "/static/favicon.svg"} {
		rec := get(path, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: status %d", path, rec.Code)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("GET %s: Cache-Control = %q, want no-cache", path, got)
		}
		if rec.Header().Get("ETag") == "" {
			t.Errorf("GET %s: no ETag to revalidate against", path)
		}
	}

	// Revalidating with the ETag costs a 304 and no body.
	etag := get("/static/style.css", nil).Header().Get("ETag")
	rec = get("/static/style.css", map[string]string{"If-None-Match": etag})
	if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
		t.Errorf("revalidation with the current ETag: status %d with %d body bytes, want 304 and none", rec.Code, rec.Body.Len())
	}
	if rec := get("/static/style.css", map[string]string{"If-None-Match": `"0000000000000000"`}); rec.Code != http.StatusOK {
		t.Errorf("revalidation with a stale ETag: status %d, want 200", rec.Code)
	}

	// Text assets are compressed for browsers that accept it.
	rec = get("/static/style.css?v="+version, map[string]string{"Accept-Encoding": "gzip"})
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("stylesheet Content-Encoding = %q with Accept-Encoding: gzip", got)
	}
	if rec.Body.Len() >= plainSize/2 {
		t.Errorf("compressed stylesheet is %d bytes of %d: not much of a saving", rec.Body.Len(), plainSize)
	}
}

func TestPagesLinkVersionedAssets(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")
	version := staticAssetVersion()

	// An ordinary page: stylesheet and the shared script, both by
	// version, and no fitting script — it only drives the fitting
	// pages and is as large as the shared one.
	_, body := getPage(t, app, cookie, "/characters/")
	mustContain(t, "/characters/", body,
		`href="/static/style.css?v=`+version+`"`,
		`src="/static/app.js?v=`+version+`"`,
		// The head bootstrap (sync, before first paint), the
		// versioned icons, and the skip link travel on every page.
		`<script src="/static/boot.js?v=`+version+`" data-cfasync="false"></script>`,
		`href="/static/favicon.svg?v=`+version+`"`,
		`href="/static/manifest.webmanifest?v=`+version+`"`,
		`href="/static/apple-touch-icon.png?v=`+version+`"`,
		`href="/static/favicon.ico?v=`+version+`" sizes="48x48"`,
		`<a class="skip-link" href="#main-content">`,
		`<main id="main-content">`)
	if strings.Contains(body, "/static/fit.js") {
		t.Error("/characters/ loads the fitting script")
	}
	// No inline scripts or style attributes anywhere: the CSP
	// allows none, so a regression here would silently not run.
	// Every script tag must load an external /static/ file.
	for _, forbidden := range []string{"<script>", "style=\"", "onclick="} {
		if strings.Contains(body, forbidden) {
			t.Errorf("/characters/ contains forbidden inline code %q", forbidden)
		}
	}
	rest := body
	for {
		i := strings.Index(rest, "<script ")
		if i < 0 {
			break
		}
		tag := rest[i:]
		end := strings.Index(tag, ">")
		if end < 0 || !strings.Contains(tag[:end], `src="/static/`) {
			t.Errorf("/characters/ has a script tag that is not an external /static/ file: %.80q", tag)
			break
		}
		rest = rest[i+len("<script "):]
	}

	// The fitting pages get it.
	for _, path := range []string{"/fittings/", "/fittings/saved/"} {
		code, body := getPage(t, app, cookie, path)
		if code != http.StatusOK {
			t.Fatalf("GET %s: status %d", path, code)
		}
		mustContain(t, path, body, `src="/static/fit.js?v=`+version+`"`)
	}
}

func TestTemplatesAreParsedOnce(t *testing.T) {
	shared := []string{"templates/base.html", "templates/balancechart.html", "templates/charselector.html", "templates/locked.html"}
	first, err := parsedTemplate(&pageTemplates, "base", "home.html", shared...)
	if err != nil {
		t.Fatalf("parse home.html: %v", err)
	}
	second, err := parsedTemplate(&pageTemplates, "base", "home.html", shared...)
	if err != nil {
		t.Fatalf("parse home.html again: %v", err)
	}
	if first != second {
		t.Fatal("home.html was parsed again instead of being reused")
	}
	if _, err := parsedTemplate(&pageTemplates, "base", "no-such-page.html", shared...); err == nil {
		t.Fatal("a page that does not exist parsed without error")
	}
}

// TestConcurrentRendersShareTemplates renders one page from many
// requests at once. Under the race detector this is what checks
// that sharing a parsed template set between requests is safe.
func TestConcurrentRendersShareTemplates(t *testing.T) {
	app, _, _ := buildCorpTestApp(t, &countingTransport{})
	handler := app.Handler()

	const requests = 24
	codes := make([]int, requests)
	bodies := make([]string, requests)
	var wg sync.WaitGroup
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			codes[i], bodies[i] = rec.Code, rec.Body.String()
		}()
	}
	wg.Wait()
	for i := range codes {
		if codes[i] != http.StatusOK {
			t.Fatalf("request %d: status %d", i, codes[i])
		}
		if !strings.Contains(bodies[i], "</html>") || bodies[i] != bodies[0] {
			t.Fatalf("request %d rendered a different or incomplete page", i)
		}
	}
}
