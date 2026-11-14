package app

import (
	"net/http"
	"os"
	"strings"
	"testing"
)

// The changelog page renders the project changelog in the app
// shell for a visitor with no account, starting at the newest
// release. Its embedded copy must match the repository root's
// CHANGELOG.md byte for byte, so the page can never drift from
// the canonical text.
func TestChangelogPagePublic(t *testing.T) {
	app, _, _ := buildCorpTestApp(t, &countingTransport{})
	rec := get(app, nil, "/changelog/")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /changelog/ = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	root, err := os.ReadFile("../../CHANGELOG.md")
	if err != nil {
		t.Fatalf("read repo root CHANGELOG.md: %v", err)
	}
	if string(root) != changelogText {
		t.Errorf("embedded changelog.md differs from the repository root CHANGELOG.md")
	}

	// The newest release section in the canonical text leads
	// the rendered page; the file's own title block stays off it.
	first := ""
	for _, line := range strings.Split(string(root), "\n") {
		if strings.HasPrefix(line, "## [v") {
			first = strings.TrimPrefix(line, "## ")
			break
		}
	}
	if first == "" {
		t.Fatalf("no version section found in CHANGELOG.md")
	}
	mustContain(t, "/changelog/", body,
		first, "What&#39;s Changed",
		`<a href="/changelog/">Changelog</a>`,
		// An entry naming a setting keeps its underscores:
		// they are part of the name, not italics.
		"TRUST_CF_CONNECTING_IP",
	)
	if strings.Contains(body, "keepachangelog.com") {
		t.Errorf("the changelog file's intro is rendered on the page; want only the version sections")
	}
}
