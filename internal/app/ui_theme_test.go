package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

// TestAccountThemeSaveAndRender: the theme is an account setting.
// Saving posts to /settings/theme, the stored values are rendered
// onto <html> by the server on later loads, and one account's
// theme never appears on another account's pages.
func TestAccountThemeSaveAndRender(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	userA, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user A: %v", err)
	}
	userB, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user B: %v", err)
	}
	seedCharacter(t, q, userA.ID, fixtureCharA, "Fixture A")
	seedCharacter(t, q, userB.ID, fixtureCharB, "Fixture B")
	cookieA := sessionCookie(t, app, userA.ID, fixtureCharA, "Fixture A")
	cookieB := sessionCookie(t, app, userB.ID, fixtureCharB, "Fixture B")

	post := func(cookie *http.Cookie, form url.Values) int {
		req := httptest.NewRequest(http.MethodPost, "/settings/theme", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec.Code
	}

	// Nothing saved yet: the load says so, and no theme attributes
	// are rendered.
	code, body := getPage(t, app, cookieA, "/characters/")
	if code != http.StatusOK {
		t.Fatalf("characters before save: status %d", code)
	}
	mustContain(t, "no saved theme", body, `<meta name="es-theme-account" content="none">`)
	if strings.Contains(body, "data-scheme=") || strings.Contains(body, "data-backdrop=") {
		t.Fatal("theme attributes rendered before any save")
	}

	// The save the browser's adoption posts lands on the account.
	adopt := url.Values{"scheme": {"jove"}, "tier": {"minimalistic"}, "backdrop": {""}}
	if code := post(cookieA, adopt); code != http.StatusOK {
		t.Fatalf("adoption save: status %d", code)
	}
	if prefs, stored := app.themePrefsFor(ctx, userA.ID); !stored || prefs.Scheme != "jove" || prefs.Tier != "minimalistic" {
		t.Fatalf("stored prefs = %+v, stored=%v; want jove/minimalistic", prefs, stored)
	}

	// Later loads render the account's theme in the server's own
	// HTML, ahead of any script.
	code, body = getPage(t, app, cookieA, "/characters/")
	if code != http.StatusOK {
		t.Fatalf("characters after save: status %d", code)
	}
	mustContain(t, "stored theme rendered", body,
		`<html lang="en" data-nav="rail" data-scheme="jove" data-tier="minimalistic">`)
	mustContain(t, "stored marker", body, `<meta name="es-theme-account" content="stored">`)

	// Account B is untouched by A's theme.
	code, body = getPage(t, app, cookieB, "/characters/")
	if code != http.StatusOK {
		t.Fatalf("characters for B: status %d", code)
	}
	mustContain(t, "B has no saved theme", body, `<meta name="es-theme-account" content="none">`)
	if strings.Contains(body, "data-scheme=") {
		t.Fatal("account A's theme rendered for account B")
	}

	// A scheme the picker cannot produce is refused, and a return
	// to the default saves as a stored default with its backdrop.
	if code := post(cookieA, url.Values{"scheme": {"neon"}, "tier": {"muted"}}); code != http.StatusBadRequest {
		t.Fatalf("unknown scheme: status %d, want 400", code)
	}
	if code := post(cookieA, url.Values{"scheme": {""}, "tier": {""}, "backdrop": {"muted"}}); code != http.StatusOK {
		t.Fatalf("default save: status %d", code)
	}
	code, body = getPage(t, app, cookieA, "/characters/")
	if code != http.StatusOK {
		t.Fatalf("characters after default save: status %d", code)
	}
	mustContain(t, "default backdrop rendered", body,
		`<html lang="en" data-nav="rail" data-backdrop="muted">`)
	mustContain(t, "default still stored", body, `<meta name="es-theme-account" content="stored">`)

	// The colour-blind mode saves beside the scheme and renders onto
	// <html>; a post that leaves the field out keeps the stored mode,
	// and a mode the picker cannot produce is refused.
	if code := post(cookieA, url.Values{"scheme": {"caldari"}, "tier": {"muted"}, "backdrop": {"muted"}, "vision": {"redgreen"}}); code != http.StatusOK {
		t.Fatalf("vision save: status %d", code)
	}
	if code := post(cookieA, url.Values{"scheme": {"gallente"}, "tier": {"muted"}, "backdrop": {"muted"}}); code != http.StatusOK {
		t.Fatalf("save without vision: status %d", code)
	}
	code, body = getPage(t, app, cookieA, "/characters/")
	if code != http.StatusOK {
		t.Fatalf("characters after vision save: status %d", code)
	}
	mustContain(t, "vision rendered beside the scheme", body,
		`<html lang="en" data-nav="rail" data-scheme="gallente" data-tier="muted" data-vision="redgreen">`)
	if code := post(cookieA, url.Values{"scheme": {""}, "tier": {""}, "backdrop": {"muted"}, "vision": {"sepia"}}); code != http.StatusBadRequest {
		t.Fatalf("unknown vision: status %d, want 400", code)
	}

	// Starfield-only saves beside Full or Muted tiers and renders onto
	// <html>; a post that leaves the field out keeps the stored choice,
	// while switching to Minimalistic turns Starfield-only off.
	if code := post(cookieA, url.Values{"scheme": {"caldari"}, "tier": {"muted"}, "backdrop": {"muted"}, "starsOnly": {"1"}}); code != http.StatusOK {
		t.Fatalf("starsOnly save: status %d", code)
	}
	if code := post(cookieA, url.Values{"scheme": {"jove"}, "tier": {"muted"}, "backdrop": {"muted"}}); code != http.StatusOK {
		t.Fatalf("save without starsOnly: status %d", code)
	}
	code, body = getPage(t, app, cookieA, "/characters/")
	if code != http.StatusOK {
		t.Fatalf("characters after starsOnly save: status %d", code)
	}
	mustContain(t, "stars-only rendered beside scheme", body,
		`<html lang="en" data-nav="rail" data-scheme="jove" data-tier="muted" data-vision="redgreen" data-stars-only="1">`)
	if strings.Contains(body, `rel="preload" as="image"`) {
		t.Fatal("stars-only should skip wallpaper image preloads on interior pages")
	}
	code, homeBody := getPage(t, app, cookieA, "/")
	if code != http.StatusOK {
		t.Fatalf("home after starsOnly save: status %d", code)
	}
	mustContain(t, "home carries data-home-page marker", homeBody, `data-home-page`)
	if strings.Contains(homeBody, `rel="preload" as="image"`) {
		t.Fatal("stars-only should skip wallpaper image preloads on the home page")
	}
	if code := post(cookieA, url.Values{"scheme": {"jove"}, "tier": {"minimalistic"}, "backdrop": {"muted"}, "starsOnly": {"1"}}); code != http.StatusOK {
		t.Fatalf("minimalistic save: status %d", code)
	}
	code, body = getPage(t, app, cookieA, "/characters/")
	if code != http.StatusOK {
		t.Fatalf("characters after minimalistic save: status %d", code)
	}
	mustContain(t, "minimalistic clears stars-only", body,
		`<html lang="en" data-nav="rail" data-scheme="jove" data-tier="minimalistic" data-vision="redgreen">`)
	if got := parseThemePrefs(`{"backdrop":"glow","starsOnly":true}`); got.StarsOnly {
		t.Error("parseThemePrefs should turn off starsOnly on default minimalistic glow")
	}
	if got := parseThemePrefs(`{"scheme":"caldari","tier":"minimalistic","starsOnly":true}`); got.StarsOnly {
		t.Error("parseThemePrefs should turn off starsOnly on scheme minimalistic tier")
	}
	if code := post(cookieA, url.Values{"scheme": {"jove"}, "tier": {"muted"}, "backdrop": {"muted"}, "starsOnly": {"maybe"}}); code != http.StatusBadRequest {
		t.Fatalf("unknown starsOnly: status %d, want 400", code)
	}
}

// TestSignupThemePrefs: the sign-up theme step's two answers map
// onto the stored account theme: the default look's types are its
// backdrop strengths and a scheme's types are its tiers.
func TestSignupThemePrefs(t *testing.T) {
	setAssetsBase("https://cdn.example.com")
	t.Cleanup(func() { setAssetsBase("") })
	cases := []struct {
		scheme, tierType string
		want             themePrefs
		ok               bool
	}{
		{"", "full", themePrefs{Backdrop: "full"}, true},
		{"", "muted", themePrefs{Backdrop: "muted"}, true},
		{"", "minimalistic", themePrefs{Backdrop: "glow"}, true},
		{"soe", "muted", themePrefs{Scheme: "soe", Tier: "muted"}, true},
		{"soe", "minimalistic", themePrefs{Scheme: "soe", Tier: "minimalistic"}, true},
		{"soe", "full", themePrefs{Scheme: "soe", Tier: "full"}, true},
		{"minmatar", "full", themePrefs{Scheme: "minmatar", Tier: "full"}, true},
		{"minmatar", "muted", themePrefs{Scheme: "minmatar", Tier: "muted"}, true},
		{"minmatar-v3", "full", themePrefs{Scheme: "minmatar-v3", Tier: "full"}, true},
		{"minmatar-v3", "muted", themePrefs{Scheme: "minmatar-v3", Tier: "muted"}, true},
		{"nope", "muted", themePrefs{}, false},
		{"soe", "loud", themePrefs{}, false},
		{"", "", themePrefs{}, false},
	}
	for _, c := range cases {
		got, ok := signupThemePrefs(c.scheme, c.tierType)
		if ok != c.ok || !reflect.DeepEqual(got, c.want) {
			t.Errorf("signupThemePrefs(%q, %q) = %v, %v; want %v, %v", c.scheme, c.tierType, got, ok, c.want, c.ok)
		}
	}
}

// TestVisionSchemeAndSemanticTokens pins that colour-blind modes
// retune colliding scheme accents, wire security/standing/fitting/ESI
// status elements to --v-* tokens, and prevent light-mode rules from
// overwriting data-vision map and pilot tokens.
func TestVisionSchemeAndSemanticTokens(t *testing.T) {
	bg := readStatic(t, "bg.css")
	for _, want := range []string{
		`html[data-vision="redgreen"][data-scheme="caldari"] { --v-good: #38bdf8; --v-bad: #ff9a40; --sch-hi: #b8dcff; --sch-core: #5b92f6; --sch-accent: #5b92f6; }`,
		`html[data-vision="redgreen"][data-scheme="triglavian"] { --sch-hi: #ff6b7a; --sch-core: #d9483b; --sch-accent: #d9483b; --sch-nav-head: #facc15; }`,
		`html[data-vision="redgreen"][data-scheme="minmatar-v3"] { --sch-hi: #ffd27a; }`,
		`html[data-vision="blueyellow"][data-scheme="gallente"] { --v-good: #33d6b4; --v-bad: #ff6a7e; --sch-hi: #88f5b2; --sch-core: #1ba89e; --sch-accent: #1ba89e; }`,
		`html[data-vision="blueyellow"][data-scheme="soe"] { --v-good: #3dd6c8; --v-bad: #ff5ca8; --sch-hi: #a8e6ec; --sch-core: #d63838; --sch-accent: #d63838; --sch-nav-head: #7ce4f0; }`,
		`--v-res-em: #5aaeff;`,
		`--v-res-em: #4ad9c8;`,
	} {
		if !strings.Contains(bg, want) {
			t.Errorf("bg.css missing vision token rule %q", want)
		}
	}
	for _, tc := range []struct {
		file, want string
	}{
		{"contracts.css", "var(--v-sec-0, #c0508a)"},
		{"contacts.css", `html[data-vision="blueyellow"] .ct-list`},
		{"workbench.css", `.wb-role-dictor { color: var(--v-bad, #e0301c); }`},
		{"scans.css", `var(--v-good, #4fbf6a)`},
		{"style.css", `.res-em { color: var(--v-res-em, #6fb6ff); }`},
		{"style.css", `html[data-vision="redgreen"] .pip-ds`},
		{"style.css", `.fit-vslot.st-overheated { border-color: var(--v-bad, #ff6a1a); }`},
		{"market.css", `var(--v-ma7, #f472b6)`},
		{"tradeideas.css", `html[data-vision="redgreen"] .ti-a1`},
		{"assets.css", `var(--v-good-ink, #7fe0a3)`},
		{"pi.css", `html[data-vision="blueyellow"] .pi`},
		{"colonies.css", `.pc-temperate { color: var(--v-good, #79d18f); }`},
	} {
		if !strings.Contains(readStatic(t, tc.file), tc.want) {
			t.Errorf("%s missing vision rule %q", tc.file, tc.want)
		}
	}
}
