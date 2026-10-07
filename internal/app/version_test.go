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

// versionShape is what version.txt has to hold: dotted numbers, with
// an optional suffix for a development build ("1.2.3.004",
// "1.2.3.004-dev").
var versionShape = regexp.MustCompile(`^\d+(\.\d+){1,3}(-[A-Za-z0-9.]+)?$`)

// TestVersionShape pins what counts as a version, since the check on
// version.txt is only as good as this pattern.
func TestVersionShape(t *testing.T) {
	for _, ok := range []string{"0.3.39.001", "0.3.38.002-dev", "1.2", "10.20.30.400", "1.2.3-rc.1"} {
		if !versionShape.MatchString(ok) {
			t.Errorf("%q should be accepted as a version", ok)
		}
	}
	for _, bad := range []string{"", "v0.3.39.001", "0.3.39.001 ", "1", "1.2.3.4.5", "1..2", "one.two", "ddd", "1.2.3-"} {
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
		t.Fatalf("version.txt = %q, want a version like 1.2.3.004 (optionally with a suffix such as -dev)", file)
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
