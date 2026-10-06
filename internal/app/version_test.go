package app

// The footer version comes from version.txt alone: the embedded
// file parses, appVersion carries it, and every rendered page
// shows it.

import (
	"context"
	"strings"
	"testing"
)

func TestVersionFileAndFooter(t *testing.T) {
	if got := strings.TrimSpace(versionFile); got != "0.3.36.003" {
		t.Fatalf("version.txt = %q, want 0.3.36.003", got)
	}
	if appVersion != "v0.3.36.003" {
		t.Fatalf("appVersion = %q, want v0.3.36.003", appVersion)
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
	mustContain(t, "/market/ footer", body, `Powered by EveSynapse v0.3.36.003 🏓 by <a href="https://natems.dev" target="_blank" rel="noopener noreferrer">natemsz</a>`)
	if strings.Contains(body, "0.2.0") {
		t.Fatal("footer still shows the old hardcoded version")
	}
}
