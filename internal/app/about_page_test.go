package app

import (
	"net/http"
	"os"
	"strings"
	"testing"
)

// The About page is the public home of the privacy policy: it
// renders for a visitor with no account, carries the three
// anchored sections the site footer links into, and shows the
// operator's mailing address exactly once (in the privacy
// policy, the one place it is legally required). The copy is
// factual, not marketing: the landing page's slogan stays off it.
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
		"admin@evesynapse.app",
		`<a href="/about/#about">About</a>`,
		`<a href="/about/#contact">Contact</a>`,
		`<a href="/about/#privacy">Privacy Policy</a>`,
		`<a href="/license/">License</a>`,
	)
	if n := strings.Count(body, "3518 Fremont Ave N"); n != 1 {
		t.Errorf("business address appears %d times on /about/, want exactly 1 (privacy policy only)", n)
	}
	if strings.Contains(body, "greater than the sum of its parts") {
		t.Errorf("/about/ carries the landing page's marketing slogan; the About copy stays factual")
	}
}

// The license page serves the project's legal texts in the app
// shell. Its embedded copies must match the repository root's
// LICENSE and PATENTS files byte for byte, so the page can never
// drift from the canonical texts.
func TestLicensePagePublic(t *testing.T) {
	app, _, _ := buildCorpTestApp(t, &countingTransport{})
	rec := get(app, nil, "/license/")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /license/ = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	mustContain(t, "/license/", body, "BSD-3-Clause-Clear", "Patent Grant")

	rootLicense, err := os.ReadFile("../../LICENSE")
	if err != nil {
		t.Fatalf("read repo root LICENSE: %v", err)
	}
	if string(rootLicense) != licenseText {
		t.Errorf("embedded license.txt differs from the repository root LICENSE")
	}
	rootPatents, err := os.ReadFile("../../PATENTS")
	if err != nil {
		t.Fatalf("read repo root PATENTS: %v", err)
	}
	if string(rootPatents) != patentsText {
		t.Errorf("embedded patents.txt differs from the repository root PATENTS")
	}
}
