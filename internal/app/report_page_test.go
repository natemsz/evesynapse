package app

import (
	"net/http"
	"testing"
)

// The Report a bug page is public: it renders for a visitor with
// no account, carries both doors (the Discord invite and the
// bugs@ address), and the footer on every page links to it, so a
// tester who hits a bug can always find the way back here.
func TestReportPagePublic(t *testing.T) {
	app, _, _ := buildCorpTestApp(t, &countingTransport{})
	rec := get(app, nil, "/report/")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /report/ = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	mustContain(t, "/report/", body,
		`id="report"`, `id="feature"`,
		"https://discord.gg/76jpPP6Whb",
		`<a href="mailto:bugs@evesynapse.app">bugs@evesynapse.app</a>`,
		"what you expected to happen",
		`<a href="/report/">Report a bug</a>`,
	)
}
