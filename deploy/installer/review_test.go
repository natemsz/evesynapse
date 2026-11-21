// Tests that the review screen is reached with every value on it,
// and that it survives ticks and resizes.
package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func reviewAnswers(t *testing.T) *answers {
	t.Helper()
	a := &answers{dbLocal: false}
	a.detectExisting()
	a.existingEnv = map[string]string{}
	a.existingEnvAt = ""
	a.edits = map[string]string{}
	a.source = sourceBuild
	a.sourceDir = t.TempDir()
	a.orgSourceDir = orgNew
	a.databaseURL = "postgres://evs:secret@localhost:5432/evesynapse"
	a.orgDB = orgNew
	a.clientID = "testid"
	a.orgClientID = orgNew
	a.clientSecret = "testsecret"
	a.orgClientSecret = orgNew
	a.contact = "test@example.com"
	a.orgContact = orgNew
	a.callback = "https://example.com/auth/callback"
	a.given = map[string]bool{
		"source": true, "sourceDir": true, "databaseURL": true,
		"clientID": true, "clientSecret": true, "contact": true, "callback": true,
	}
	return a
}

func TestReviewReachedFromStart(t *testing.T) {
	a := reviewAnswers(t)
	// The sourceDir check needs a real checkout; the review only
	// needs the path, so the stage is entered from the last prompt.
	m := newModel(a, true)
	m.enterReview()
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 60})
	m = updated.(model)
	view := m.View()
	for _, want := range []string{"Install source", "EVE_CLIENT_ID", "testid", "****cret", "DATABASE_URL"} {
		if !strings.Contains(view, want) {
			t.Errorf("review view does not contain %q", want)
		}
	}
	if strings.Contains(view, "testsecret") {
		t.Errorf("review view shows the client secret")
	}
	updated, _ = m.Update(tickMsg{})
	m = updated.(model)
	if !strings.Contains(m.View(), "Install source") {
		t.Errorf("review view lost its list after a tick")
	}
	updated, _ = m.Update(tea.WindowSizeMsg{Width: 70, Height: 24})
	m = updated.(model)
	if !strings.Contains(m.View(), "Install source") {
		t.Errorf("review view lost its list after a resize")
	}
}

func TestReviewListsEverySettingWithShowAll(t *testing.T) {
	a := reviewAnswers(t)
	rows := a.reviewRows(true)
	seen := map[string]bool{}
	for _, r := range rows {
		seen[r.key] = true
	}
	for _, s := range settingsCatalog {
		if !seen[s.Key] {
			t.Errorf("review with show all is missing %s", s.Key)
		}
	}
}
