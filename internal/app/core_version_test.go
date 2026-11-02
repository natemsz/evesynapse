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
	// The project name links to the project's Forgejo
	// organization page, the version number links to that
	// version's release page there, and a known build identity
	// prints the binary hash's first nine characters in the
	// line, with the full hash in its "view hash" pop-up.
	savedBuild := runningBuild
	defer func() { runningBuild = savedBuild }()
	runningBuild = buildIdentity{
		hash: "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210",
	}
	_, body := getPage(t, app, cookie, "/market/")
	mustContain(t, "/market/ footer", body, `<a href="https://natems.dev/EveSynapse" target="_blank" rel="noopener noreferrer">EveSynapse</a> <a class="version-number" href="https://natems.dev/EveSynapse/evesynapse/releases/tag/`+appVersion+`" target="_blank" rel="noopener noreferrer">`+appVersion+`</a> · binary hash <span class="binary-hash">fedcba987</span> <span class="hash-pop-wrap"><button type="button" class="hash-view" aria-expanded="false" aria-controls="build-hash-pop">view hash</button><span class="hash-pop" id="build-hash-pop" role="note" aria-label="Full binary hash" hidden>fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210</span></span> · Made in Seattle, WA by <a href="https://natems.dev" target="_blank" rel="noopener noreferrer">natemsz</a>`)
	if strings.Contains(body, "0.2.0") {
		t.Fatal("footer still shows the old hardcoded version")
	}

	// With no identity known, the hash is left out and the line
	// is the version line alone.
	runningBuild = buildIdentity{}
	_, body = getPage(t, app, cookie, "/market/")
	mustContain(t, "/market/ footer with no build identity", body, `<a href="https://natems.dev/EveSynapse" target="_blank" rel="noopener noreferrer">EveSynapse</a> <a class="version-number" href="https://natems.dev/EveSynapse/evesynapse/releases/tag/`+appVersion+`" target="_blank" rel="noopener noreferrer">`+appVersion+`</a> · Made in Seattle, WA by <a href="https://natems.dev" target="_blank" rel="noopener noreferrer">natemsz</a>`)
}
