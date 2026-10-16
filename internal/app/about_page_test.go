package app

import (
	"net/http"
	"strings"
	"testing"
)

// The About page is the public home of the privacy policy: it
// renders for a visitor with no account, carries the three
// anchored sections the site footer links into, and shows the
// operator's mailing address exactly once (in the privacy
// policy, the one place it is legally required).
func TestAboutPagePublic(t *testing.T) {
	app, _, _ := buildCorpTestApp(t, &countingTransport{})
	rec := get(app, nil, "/about/")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /about/ = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	mustContain(t, "/about/", body,
		`id="about"`, `id="contact"`, `id="privacy"`,
		"3518 Fremont Ave N, Seattle, WA 98103 Ste E",
		"evesynapse@pm.me",
		`<a href="/about/#about">About</a>`,
		`<a href="/about/#contact">Contact</a>`,
		`<a href="/about/#privacy">Privacy Policy</a>`,
	)
	if n := strings.Count(body, "3518 Fremont Ave N"); n != 1 {
		t.Errorf("business address appears %d times on /about/, want exactly 1 (privacy policy only)", n)
	}
}
