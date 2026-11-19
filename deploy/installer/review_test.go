// Tests that the review screen is reached with its table
// built: the prompts end at the review, and the table holds
// every settings row plus the Install row.
package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func reviewAnswers() *answers {
	a := &answers{dbLocal: false}
	a.detectExisting()
	a.source = sourceBuild
	a.sourceDir = "/home/hatch/workspace/theme-video/wt-installer"
	a.orgSourceDir = orgNew
	a.databaseURL = "postgres://evs:secret@localhost:5432/evesynapse"
	a.orgDB = orgNew
	a.clientID = "testid"
	a.orgClientID = orgNew
	a.clientSecret = "testsecret"
	a.orgClientSecret = orgNew
	a.contact = "test@example.com"
	a.orgContact = orgNew
	a.given = map[string]bool{
		"source": true, "sourceDir": true, "databaseURL": true,
		"clientID": true, "clientSecret": true, "contact": true,
	}
	return a
}

func TestReviewReachedFromStart(t *testing.T) {
	m := newModel(reviewAnswers(), true)
	updated, _ := m.Update(startMsg{})
	m = updated.(model)
	if m.stage != stageReview {
		t.Fatalf("stage after start = %v, want review", m.stage)
	}
	if got := len(m.review.Rows()); got != len(m.a.reviewRows())+1 {
		t.Fatalf("review table has %d rows, want %d", got, len(m.a.reviewRows())+1)
	}
	view := m.View()
	for _, want := range []string{"Install source", "Install EveSynapse", "****cret", "testid"} {
		if !strings.Contains(view, want) {
			t.Errorf("review view does not contain %q", want)
		}
	}
	// A tick must not empty the table.
	updated, _ = m.Update(tickMsg{})
	m = updated.(model)
	if !strings.Contains(m.View(), "Install source") {
		t.Errorf("review view lost the table after a tick")
	}
	// A window size message must not empty the table.
	updated, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = updated.(model)
	if !strings.Contains(m.View(), "Install source") {
		t.Errorf("review view lost the table after a window size message")
	}
}

func TestReviewTableRenders(t *testing.T) {
	m := newModel(reviewAnswers(), true)
	m.enterReview()
	view := m.review.View()
	for _, want := range []string{"Install source", "Database", "EVE_CLIENT_ID", "Install EveSynapse"} {
		if !strings.Contains(view, want) {
			t.Errorf("review table does not contain %q", want)
		}
	}
}
