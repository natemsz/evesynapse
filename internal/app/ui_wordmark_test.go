package app

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// TestWordmarkIsOneFontAndOneKindOfPaint: EVE and SYNAPSE are the same
// size because both are solid text in the same self-hosted face. SYNAPSE
// used to be a gradient clipped to its letters, which is painted through a
// mask and came out a pixel shorter than the solid white EVE at the header's
// small size (and vanished in WebKit under a transformed ancestor). The
// gradient is now one solid colour per letter, and nothing is fetched from
// Google any more.
func TestWordmarkIsOneFontAndOneKindOfPaint(t *testing.T) {
	app, _, _ := buildCorpTestApp(t, &countingTransport{})

	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}

	page := get("/")
	if page.Code != http.StatusOK {
		t.Fatalf("GET / = %d", page.Code)
	}
	body := page.Body.String()

	// SYNAPSE is seven letter spans, in order, inside .wm-syn.
	m := regexp.MustCompile(`<span class="wm-eve">EVE</span><span class="wm-syn">((?:<span>[A-Z]</span>){7})</span>`).FindStringSubmatch(body)
	if m == nil {
		t.Fatal(`the wordmark is not <span class="wm-eve">EVE</span> then <span class="wm-syn"> with seven <span>letter</span>s`)
	}
	if letters := regexp.MustCompile(`<span>([A-Z])</span>`).ReplaceAllString(m[1], "$1"); letters != "SYNAPSE" {
		t.Errorf("the letter spans spell %q, want SYNAPSE", letters)
	}
	for _, banned := range []string{"fonts.googleapis.com", "fonts.gstatic.com"} {
		if strings.Contains(body, banned) {
			t.Errorf("the page still refers to %s", banned)
		}
	}

	raw, err := fs.ReadFile(staticFS, "static/style.css")
	if err != nil {
		t.Fatalf("read style.css: %v", err)
	}
	css := string(raw)
	for _, want := range []string{
		"@font-face {\n  font-family: 'Pixelify Sans';",
		"url('/static/fonts/pixelify-sans-semibold.ttf') format('truetype')",
		".wm-syn > span:nth-child(1) { color: #ffc36c; }",
		".wm-syn > span:nth-child(4) { color: #ff6a1a; }",
		".wm-syn > span:nth-child(7) { color: #dc4315; }",
		".wordmark:hover .wm-syn > span { color: var(--text-bright); }",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("style.css is missing %q", want)
		}
	}
	// Seven letters, seven colours, none left to a clipped gradient.
	if n := strings.Count(css, ".wm-syn > span:nth-child("); n != 7 {
		t.Errorf("%d per-letter colour rules, want 7", n)
	}
	if strings.Contains(css, ".wm-syn {") {
		t.Error("style.css still styles .wm-syn as one block (the clipped gradient)")
	}
	if strings.Contains(css, "VT323") {
		t.Error("style.css still names VT323")
	}

	boot := get("/static/boot.js")
	if strings.Contains(boot.Body.String(), "googleapis") || strings.Contains(boot.Body.String(), "VT323") {
		t.Error("boot.js still loads the wordmark font from Google")
	}

	// The font itself: served by the app, a TrueType file, cached for a year.
	font := get("/static/fonts/pixelify-sans-semibold.ttf")
	if font.Code != http.StatusOK {
		t.Fatalf("font GET = %d (run `make assets` to decode ci-assets/ first)", font.Code)
	}
	if b := font.Body.Bytes(); len(b) < 4 || string(b[:4]) != "\x00\x01\x00\x00" {
		t.Errorf("the font file does not start with the TrueType header: % x", b[:min(4, len(b))])
	}
	if got := font.Header().Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Errorf("font Cache-Control = %q, want immutable", got)
	}
}
