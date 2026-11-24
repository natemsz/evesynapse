package app

import (
	"strings"
	"testing"
)

func TestAssetsCDNControlsWallpapers(t *testing.T) {
	t.Cleanup(func() { setAssetsBase("") })
	for in, want := range map[string]string{
		"cdn.example.com":                 "https://cdn.example.com",
		"https://cdn.example.com/static/": "https://cdn.example.com",
		"http://cdn.example.com":          "",
		`cdn.example.com/"x`:              "",
	} {
		if got := parseAssetsPath(in); got != want {
			t.Errorf("parseAssetsPath(%q) = %q, want %q", in, got, want)
		}
	}

	setAssetsBase("")
	if got, _ := signupThemePrefs("jove", "full"); got.Tier != "muted" {
		t.Errorf("without a CDN, a scheme at full = %q, want muted", got.Tier)
	}
	if got := parseThemePrefs(`{"backdrop":"full"}`); got.Backdrop != "muted" {
		t.Errorf("without a CDN, a stored full backdrop = %q, want muted", got.Backdrop)
	}
	if types := signupThemeViewFor(nil).Types; !types[0].Disabled {
		t.Error("without a CDN, sign-up should disable the Full type")
	}
	if _, ok := cdnStylesheet("/wallpapers-cdn.css"); ok {
		t.Error("the CDN stylesheet should not exist without a CDN")
	}

	setAssetsBase("https://cdn.example.com")
	sheet, ok := cdnStylesheet("/wallpapers-cdn.css")
	if !ok {
		t.Fatal("no CDN stylesheet")
	}
	css := string(sheet)
	for _, want := range []string{
		"https://cdn.example.com/static/bg-calm-uhd.jpg",
		"https://cdn.example.com/static/bg-jove-uhd.jpg",
		"https://cdn.example.com/static/bg-nebula-uhd.avif",
		"https://cdn.example.com/static/bg-nebula-jove-uhd.avif",
		"https://cdn.example.com/static/bg-nebula-caldari-mobile.avif",
		"min-width:3201px",
		assetsOnScope,
		`:not([data-stars-only])`,
		`:not([data-backdrop=glow])`,
		`:not([data-tier=minimalistic])`,
	} {
		if !strings.Contains(css, want) {
			t.Errorf("the CDN stylesheet lacks %q", want)
		}
	}
	if strings.Contains(strings.ReplaceAll(css, "https://cdn.example.com/static/", ""), "/static/bg") {
		t.Error("a CDN stylesheet rule still points at the built-in files")
	}
	if csp := contentSecurityPolicy(); !strings.Contains(csp, "media-src 'self' https://cdn.example.com") {
		t.Errorf("the policy does not allow the CDN: %s", csp)
	}
}
