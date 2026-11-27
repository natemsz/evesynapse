package app

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
)

// The changelog page renders the project changelog in the app
// shell for a visitor with no account, defaulting to the newest
// minor series consolidated across its revisions. Its embedded
// copy must match the repository root's CHANGELOG.md byte for
// byte, so the page can never drift from the canonical text.
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

	if len(changelogStore.Releases) == 0 {
		t.Fatalf("changelogStore has no parsed releases")
	}
	newestRel := changelogStore.Releases[0]
	newestMaj := newestRel.Majors[0]
	newestMin := newestMaj.Minors[0]
	newestRev := newestMin.Revisions[0]

	mustContain(t, "/changelog/", body,
		"<search>",
		`id="cl-explorer"`,
		`id="cl-release"`,
		`id="cl-major"`,
		`id="cl-minor"`,
		`id="cl-rev"`,
		`id="cl-index"`,
		newestMin.Label,
		"("+newestRev.Tag+")",
		`<a href="/changelog/">Changelog</a>`,
	)
	if strings.Contains(body, "keepachangelog.com") {
		t.Errorf("the changelog file's intro is rendered on the page; want only version notes")
	}
	// Default view shows only the current minor, so entries from 0.8.3.x stay off it.
	if newestMin.Label != "0.8.3.x" && strings.Contains(body, "TRUST_CF_CONNECTING_IP") {
		t.Errorf("default view rendered older minor entry TRUST_CF_CONNECTING_IP; want only %s", newestMin.Label)
	}

	// Filtering to 0.8.3.x renders that consolidated minor and keeps
	// snake_case setting names literal (underscores are text, not italics).
	recOld := get(app, nil, "/changelog/?release=0&major=8&minor=3")
	if recOld.Code != http.StatusOK {
		t.Fatalf("GET /changelog/?release=0&major=8&minor=3 = %d, want 200", recOld.Code)
	}
	oldBody := recOld.Body.String()
	mustContain(t, "/changelog/?release=0&major=8&minor=3", oldBody,
		"0.8.3.x",
		"TRUST_CF_CONNECTING_IP",
		"(@natemsz) (v0.8.3.1)",
		`href="/changelog/">Latest</a>`,
	)

	// Filtering to a single revision renders only that build.
	revURL := fmt.Sprintf("/changelog/?release=%d&major=%d&minor=%d&rev=%d",
		newestRel.Release, newestMaj.Major, newestMin.Minor, newestRev.Rev)
	recRev := get(app, nil, revURL)
	if recRev.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", revURL, recRev.Code)
	}
	revBody := recRev.Body.String()
	mustContain(t, revURL, revBody,
		"<h2>"+newestRev.Tag+"</h2>",
		"("+newestRev.Tag+")",
	)
	if len(newestMin.Revisions) > 1 {
		otherRev := newestMin.Revisions[len(newestMin.Revisions)-1]
		if strings.Contains(revBody, "("+otherRev.Tag+")") {
			t.Errorf("single-revision view for %s included entry from %s", newestRev.Tag, otherRev.Tag)
		}
	}
}

// TestChangelogParseScopingAndConsolidation verifies that minor groups are
// strictly scoped by (release, major, minor), consolidate categories across
// revisions with author and build attribution, skip boilerplate, and clamp
// invalid child parameters.
func TestChangelogParseScopingAndConsolidation(t *testing.T) {
	sample := `# Changelog

## [Unreleased]

## [v1.1.2.2] - 2026-11-26

Automated build of EveSynapse v1.1.2.2.
If this is your first install, use the installer.

## What's Changed

### 🚀 Features
* Add fleet export by @alice in #200

### 🐛 Bug Fixes
* Fix LOW_RAM_MODE startup flag by @bob in #201

### Other Changes
* Update changelog for v1.1.2.1 by @alice in #202
* Updated Changelog to version 1.1.2.0 by @alice in #203
* docs(changelog): review changelog for v1.1.2.0 by @github-actions[bot] in #204

## [v1.1.2.1]

## What's Changed

### 🚀 Features
* Add market alerts by @carol in #199
* Handcrafted feature without author

### 🐛 Bug Fixes
* No changes recorded for this release window.

## [v0.1.2.1]

## What's Changed

### 🚀 Features
* Legacy feature from release zero by @dave in #10
`

	idx := parseChangelog(sample)

	// Default view is 1.1.2.x consolidated across v1.1.2.2 and v1.1.2.1,
	// with zero leakage from 0.1.2.x.
	v := idx.view("", "", "", "")
	if v.Heading != "1.1.2.x" {
		t.Fatalf("default heading = %q, want 1.1.2.x", v.Heading)
	}
	if !v.IsLatestMinor || v.IsSingleRev {
		t.Errorf("default view flags = (latest=%v, single=%v), want (true, false)", v.IsLatestMinor, v.IsSingleRev)
	}
	body := string(v.Body)
	for _, want := range []string{
		"Add fleet export (@alice) (v1.1.2.2)",
		"Add market alerts (@carol) (v1.1.2.1)",
		"Handcrafted feature without author (v1.1.2.1)",
		"Fix LOW_RAM_MODE startup flag (@bob) (v1.1.2.2)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("1.1.2.x consolidated body missing %q\nbody:\n%s", want, body)
		}
	}
	for _, forbid := range []string{
		"Legacy feature from release zero",
		"v0.1.2.1",
		"Automated build of EveSynapse",
		"No changes recorded for this release window.",
		"Update changelog for v1.1.2.1",
		"Updated Changelog to version 1.1.2.0",
		"review changelog for v1.1.2.0",
	} {
		if strings.Contains(body, forbid) {
			t.Errorf("1.1.2.x consolidated body unexpectedly contains %q", forbid)
		}
	}

	// Selecting release=0, major=1, minor=2 shows 0.1.2.x and never 1.1.2.x.
	v0 := idx.view("0", "1", "2", "")
	if v0.Heading != "0.1.2.x" {
		t.Fatalf("0.1.2.x heading = %q, want 0.1.2.x", v0.Heading)
	}
	body0 := string(v0.Body)
	if !strings.Contains(body0, "Legacy feature from release zero (@dave) (v0.1.2.1)") {
		t.Errorf("0.1.2.x body missing v0.1.2.1 item: %s", body0)
	}
	if strings.Contains(body0, "Add fleet export") {
		t.Errorf("0.1.2.x body leaked v1.1.2.2 item")
	}

	// Single revision filter shows only that build.
	vRev := idx.view("1", "1", "2", "1")
	if vRev.Heading != "v1.1.2.1" || !vRev.IsSingleRev {
		t.Fatalf("single revision view = (%q, single=%v), want (v1.1.2.1, true)", vRev.Heading, vRev.IsSingleRev)
	}
	if !strings.Contains(string(vRev.Body), "Add market alerts (@carol) (v1.1.2.1)") {
		t.Errorf("v1.1.2.1 body missing its feature")
	}
	if strings.Contains(string(vRev.Body), "Add fleet export") {
		t.Errorf("v1.1.2.1 body included v1.1.2.2 feature")
	}

	// Invalid child params clamp cleanly to the newest valid minor under release 0.
	vClamp := idx.view("0", "99", "99", "99")
	if vClamp.Heading != "0.1.2.x" || vClamp.IsSingleRev {
		t.Errorf("clamped view = (%q, single=%v), want (0.1.2.x, false)", vClamp.Heading, vClamp.IsSingleRev)
	}
}
