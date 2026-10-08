package app

// The footer version comes from version.txt alone: the embedded
// file parses, appVersion carries it, and every rendered page
// shows it.

import (
	"context"
	"regexp"
	"strings"
	"testing"
)

// versionShape is what version.txt has to hold: major.feature.fix,
// each a plain number with no leading zeros, with an optional suffix
// for a development build ("1.4.2", "1.4.2-dev").
var versionShape = regexp.MustCompile(`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(-[A-Za-z0-9.]+)?$`)

// lastPaddedVersion is the last release numbered the old way, with
// four zero-padded parts. version.txt may still hold it; the next
// change to that file has to be a major.feature.fix number.
const lastPaddedVersion = "0.4.01.003"

// TestVersionShape pins what counts as a version, since the check on
// version.txt is only as good as this pattern.
func TestVersionShape(t *testing.T) {
	for _, ok := range []string{"0.5.0", "1.4.2", "10.20.300", "1.4.2-dev", "1.4.2-rc.1"} {
		if !versionShape.MatchString(ok) {
			t.Errorf("%q should be accepted as a version", ok)
		}
	}
	for _, bad := range []string{"", "v1.4.2", "1.4.2 ", "1", "1.4", "1.4.2.1", "0.4.01.003", "1.04.2", "01.4.2", "1.4.02", "1..2", "one.two.three", "ddd", "1.4.2-"} {
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
	if file != lastPaddedVersion && !versionShape.MatchString(file) {
		t.Fatalf("version.txt = %q, want major.feature.fix with no leading zeros, like 1.4.2 (optionally with a suffix such as -dev)", file)
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

	_, body := getPage(t, app, cookie, "/market/")
	mustContain(t, "/market/ footer", body, `Powered by EveSynapse `+appVersion+` 🏓 by <a href="https://natems.dev" target="_blank" rel="noopener noreferrer">natemsz</a>`)
	if strings.Contains(body, "0.2.0") {
		t.Fatal("footer still shows the old hardcoded version")
	}
}
