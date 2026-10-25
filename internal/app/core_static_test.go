package app

// Tests for how static assets are cached and compressed (core_static.go),
// for which pages load the fitting script, and for the parsed
// template cache behind every render.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
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
	for _, want := range []string{"evesynapse-nav", "evesynapse-theme", "evesynapse-backdrop"} {
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

// TestStaticServesMinifiedCSS: stylesheets are minified once at
// index build and served from those bytes. The served form is
// smaller and comment-free, the ETag still names exactly the
// bytes served, pinned values survive the minifier's respelling,
// and the wallpaper references in the served sheets still carry
// the files' own hash tokens (the handler versions a wallpaper
// URL by that token).
func TestStaticServesMinifiedCSS(t *testing.T) {
	app, _, _ := buildCorpTestApp(t, &countingTransport{})
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec
	}
	idx := loadStaticIndex()

	for _, name := range []string{"style.css", "bg.css", "landing.css"} {
		raw, err := fs.ReadFile(staticFS, "static/"+name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		rec := get("/static/" + name)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /static/%s: status %d", name, rec.Code)
		}
		served := rec.Body.Bytes()
		if len(served) >= len(raw) {
			t.Errorf("%s: served %d bytes, want fewer than the %d source bytes", name, len(served), len(raw))
		}
		if strings.Contains(string(served), "/*") {
			t.Errorf("%s: served bytes still carry a comment", name)
		}
		if string(served) != string(idx.served["/"+name]) {
			t.Errorf("%s: served bytes differ from the index bytes the version and ETag hash", name)
		}
		sum := sha256.Sum256(served)
		wantETag := `"` + hex.EncodeToString(sum[:8]) + `"`
		if got := rec.Header().Get("ETag"); got != wantETag {
			t.Errorf("%s: ETag = %q, want %q (the hash of the served bytes)", name, got, wantETag)
		}
	}

	// Values other tests and the design pin, in the minifier's
	// spelling: the column cap and font-display on every face.
	style := get("/static/style.css").Body.String()
	for _, want := range []string{"max-width:1720px", "font-display:swap"} {
		if !strings.Contains(style, want) {
			t.Errorf("served style.css is missing %q", want)
		}
	}

	// The wallpaper tokens survive minification in the served
	// sheets: each reference's ?v= is still the file's hash.
	for _, name := range []string{"bg.css", "landing.css"} {
		served := get("/static/" + name).Body.String()
		refs := wallpaperRef.FindAllStringSubmatch(served, -1)
		if len(refs) == 0 {
			t.Errorf("served %s: no wallpaper references found", name)
			continue
		}
		for _, m := range refs {
			etag, ok := idx.etags["/"+m[1]]
			if !ok {
				t.Errorf("served %s: wallpaper reference %q is not a static file", name, m[1])
				continue
			}
			if want := strings.Trim(etag, `"`); m[2] != want {
				t.Errorf("served %s: /static/%s token %q, want %q", name, m[1], m[2], want)
			}
		}
	}
}

// cssDeclarations returns the declarations of the first top-level
// rule whose selector is exactly sel, as property -> value. It
// scans rules at brace depth zero, so it reads the served
// (minified) stylesheet as well as a formatted one; an at-rule
// block such as @media is skipped whole, so a rule nested inside
// one never matches. The sheets' strings carry no braces, which
// is all the brace matching assumes.
func cssDeclarations(css, sel string) (map[string]string, bool) {
	i := 0
	for i < len(css) {
		for i < len(css) && strings.IndexByte(" \t\r\n;", css[i]) >= 0 {
			i++
		}
		if i >= len(css) {
			break
		}
		start := i
		for i < len(css) && css[i] != '{' && css[i] != ';' {
			i++
		}
		if i >= len(css) {
			break
		}
		if css[i] == ';' { // a statement at-rule, e.g. @import
			i++
			continue
		}
		prelude := strings.TrimSpace(css[start:i])
		i++ // consume '{'
		depth := 1
		bodyStart := i
		for i < len(css) && depth > 0 {
			switch css[i] {
			case '{':
				depth++
				i++
			case '}':
				depth--
				if depth > 0 {
					i++
				}
			default:
				i++
			}
		}
		body := css[bodyStart:i]
		i++ // consume '}'
		if prelude != sel {
			continue
		}
		out := map[string]string{}
		for _, decl := range strings.Split(body, ";") {
			prop, value, ok := strings.Cut(decl, ":")
			if !ok {
				continue
			}
			out[strings.TrimSpace(prop)] = strings.TrimSpace(value)
		}
		return out, true
	}
	return nil, false
}

// TestStylesheetMainColumnWidth: the page column and the top bar
// that sits over it share one width cap. The test reads those
// two rules and nothing else, so a number elsewhere in the
// stylesheet cannot pass or fail it.
func TestStylesheetMainColumnWidth(t *testing.T) {
	app, _, _ := buildCorpTestApp(t, &countingTransport{})
	req := httptest.NewRequest(http.MethodGet, "/static/style.css", nil)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("stylesheet: status %d", rec.Code)
	}
	css := strings.ReplaceAll(rec.Body.String(), "\r\n", "\n")
	const want = "1720px"
	for _, sel := range []string{"main", "header nav"} {
		rule, ok := cssDeclarations(css, sel)
		if !ok {
			t.Errorf("stylesheet has no top-level %q rule", sel)
			continue
		}
		if got := rule["max-width"]; got != want {
			t.Errorf("%s { max-width: %s }, want %s", sel, got, want)
		}
	}
}

// TestCSSDeclarations: the reader finds the named rule only.
func TestCSSDeclarations(t *testing.T) {
	css := "nav main { max-width: 1px; }\n@media (min-width: 10px) {\n  main { max-width: 2px; }\n}\nmain {\n  max-width: 3px;\n  margin: 0 auto;\n}\nmain.wide { max-width: 4px; }\n"
	rule, ok := cssDeclarations(css, "main")
	if !ok || rule["max-width"] != "3px" || rule["margin"] != "0 auto" {
		t.Fatalf("declarations of main = %v (found %v), want the top-level rule", rule, ok)
	}
	if _, ok := cssDeclarations(css, "aside"); ok {
		t.Fatal("found a rule that is not there")
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
	// The same shared files render uses: this test fills the cache
	// render reads, and a list of its own that fell behind left every
	// later page of the test run without the notifications partial.
	shared := pageSharedTemplates
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

// TestStaticFileHashVersionTokens: a wallpaper URL whose ?v= is
// the file's own content hash (the form the stylesheets emit)
// caches forever, like a build-versioned URL; a wrong hash and a
// missing token both stay on the revalidation path.
func TestStaticFileHashVersionTokens(t *testing.T) {
	app, _, _ := buildCorpTestApp(t, &countingTransport{})
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec
	}

	etag := get("/static/bg.jpg").Header().Get("ETag")
	if etag == "" {
		t.Fatal("bg.jpg served without an ETag to derive its token from")
	}
	hash := strings.Trim(etag, `"`)

	rec := get("/static/bg.jpg?v=" + hash)
	if rec.Code != http.StatusOK {
		t.Fatalf("hash-versioned wallpaper: status %d", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != cacheForever {
		t.Errorf("hash-versioned wallpaper Cache-Control = %q, want %q", got, cacheForever)
	}
	for _, path := range []string{"/static/bg.jpg?v=0000000000000000", "/static/bg.jpg"} {
		rec := get(path)
		if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("GET %s: Cache-Control = %q, want no-cache", path, got)
		}
	}
	// The token names one file's bytes only: bg.jpg's hash does
	// not version bg-uhd.jpg.
	rec = get("/static/bg-uhd.jpg?v=" + hash)
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("cross-file token: Cache-Control = %q, want no-cache", got)
	}
}

// wallpaperRef matches a wallpaper reference in a stylesheet or
// template, with its ?v= token when one is present. The landing's
// star tile rides the same token discipline.
var wallpaperRef = regexp.MustCompile(`/static/(bg-[^"')?\s]+|bg\.(?:jpg|jxl)|stars-tile\.webp)(?:\?v=([0-9a-f]+))?`)

// TestStaticWallpaperVersionTokens pins the tokens the
// stylesheets and the base template put on the wallpaper files
// to those files' current content hashes. Replacing the art
// without refreshing a token fails here, with the hash to paste
// in the message — a stale token would drop the file back to
// per-view revalidation, and a wrong immutable would be worse.
func TestStaticWallpaperVersionTokens(t *testing.T) {
	idx := loadStaticIndex()
	sources := map[string]string{}
	for _, name := range []string{"static/bg.css", "static/landing.css", "static/style.css"} {
		b, err := fs.ReadFile(staticFS, name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		sources[name] = string(b)
	}
	b, err := fs.ReadFile(templatesFS, "templates/base.html")
	if err != nil {
		t.Fatalf("read base.html: %v", err)
	}
	sources["templates/base.html"] = string(b)

	for src, text := range sources {
		refs := wallpaperRef.FindAllStringSubmatch(text, -1)
		if len(refs) == 0 {
			t.Errorf("%s: no wallpaper references found — the pin has nothing to check", src)
			continue
		}
		for _, m := range refs {
			name, token := m[1], m[2]
			etag, ok := idx.etags["/"+name]
			if !ok {
				t.Errorf("%s: wallpaper reference %q is not a static file", src, name)
				continue
			}
			if want := strings.Trim(etag, `"`); token != want {
				t.Errorf("%s: /static/%s token %q, want %q (the file's current content hash)", src, name, token, want)
			}
		}
	}
}

// The wallpaper's JPEG XL tiers only paint if the file server
// labels them image/jxl: the content sniffer does not recognise
// the format, so without the explicit registration in
// staticHandler the response degrades to octet-stream.
func TestStaticServesJXLContentType(t *testing.T) {
	app, _, _ := buildCorpTestApp(t, &countingTransport{})
	req := httptest.NewRequest(http.MethodGet, "/static/bg-uhd.jxl", nil)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /static/bg-uhd.jxl: status %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "image/jxl" {
		t.Errorf("bg-uhd.jxl Content-Type = %q, want image/jxl", got)
	}
}
