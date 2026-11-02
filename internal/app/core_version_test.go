package app

// The footer version comes from version.txt alone: the embedded
// file parses, appVersion carries it, and every rendered page
// shows it. The same footer line names the running build
// (core_build.go) whenever its identity is known.

import (
	"context"
	"regexp"
	"strings"
	"testing"
)

// versionShape is what version.txt has to hold: four plain numbers
// with no leading zeros, stable.major.feature.fix, with an optional
// suffix for a development build ("0.4.1.4", "0.4.1.4-dev"). The first
// number is 0 until there is a feature-complete stable release.
var versionShape = regexp.MustCompile(`^(0|[1-9]\d*)(\.(0|[1-9]\d*)){3}(-[A-Za-z0-9.]+)?$`)

// TestVersionShape pins what counts as a version, since the check on
// version.txt is only as good as this pattern.
func TestVersionShape(t *testing.T) {
	for _, ok := range []string{"0.4.1.4", "0.5.0.0", "1.0.0.0", "10.20.30.400", "0.4.1.4-dev", "1.2.3.4-rc.1"} {
		if !versionShape.MatchString(ok) {
			t.Errorf("%q should be accepted as a version", ok)
		}
	}
	for _, bad := range []string{"", "v0.4.1.4", "0.4.1.4 ", "1", "1.4", "1.4.2", "1.2.3.4.5", "0.4.01.003", "0.4.1.04", "00.4.1.4", "1..2.3", "one.two.three.four", "ddd", "0.4.1.4-"} {
		if versionShape.MatchString(bad) {
			t.Errorf("%q should not be accepted as a version", bad)
		}
	}
}

func TestVersionFileAndFooter(t *testing.T) {
	// version.txt is the one place a release number is written, and
	// no test repeats it: a release changes that file and nothing
	// else. What is checked is that the file holds something shaped
	// like a version and that everything else reads it from there.
	file := strings.TrimSpace(versionFile)
	if !versionShape.MatchString(file) {
		t.Fatalf("version.txt = %q, want four numbers with no leading zeros, stable.major.feature.fix, like 0.4.1.4 (optionally with a suffix such as -dev)", file)
	}
	if appVersion != "v"+file {
		t.Fatalf("appVersion = %q, want v%s, the contents of version.txt", appVersion, file)
	}
	if got := Version(); got != appVersion {
		t.Fatalf("Version() = %q, want %q", got, appVersion)
	}

	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")

	// The footer line also names the build serving the page.
	// With an identity known, it carries the short commit linked
	// to the full one and the truncated binary hash with the
	// full hash in its title.
	savedBuild := runningBuild
	defer func() { runningBuild = savedBuild }()
	runningBuild = buildIdentity{
		commit: "0123456789abcdef0123456789abcdef01234567",
		hash:   "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210",
	}
	_, body := getPage(t, app, cookie, "/market/")
	mustContain(t, "/market/ footer", body, `<a href="https://gitlab.com/natemsz/evesynapse" target="_blank" rel="noopener noreferrer">EveSynapse</a> <span class="version-number">`+appVersion+`</span> · commit <a href="https://gitlab.com/natemsz/evesynapse/-/commit/0123456789abcdef0123456789abcdef01234567" target="_blank" rel="noopener noreferrer">01234567</a> · binary hash <span title="SHA-256 of the running binary: fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210">fedcba987654…</span> · Made in Seattle, WA by <a href="https://natems.dev" target="_blank" rel="noopener noreferrer">natemsz</a>`)
	if strings.Contains(body, "0.2.0") {
		t.Fatal("footer still shows the old hardcoded version")
	}

	// With no identity known, both halves are left out and the
	// line is the version line alone.
	runningBuild = buildIdentity{}
	_, body = getPage(t, app, cookie, "/market/")
	mustContain(t, "/market/ footer with no build identity", body, `<a href="https://gitlab.com/natemsz/evesynapse" target="_blank" rel="noopener noreferrer">EveSynapse</a> <span class="version-number">`+appVersion+`</span> · Made in Seattle, WA by <a href="https://natems.dev" target="_blank" rel="noopener noreferrer">natemsz</a>`)
}
