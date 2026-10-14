package app

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// TestSectionTabs: a section's own pages carry its tabs under the
// title, the current page marked, and the sidebar menu renders the
// same links; a page shared by two sections (killmails) is marked in
// the section it is shown under only.
func TestSectionTabs(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	user, err := q.CreateUser(context.Background())
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")

	code, body := getPage(t, app, cookie, "/market/ideas/")
	if code != http.StatusOK {
		t.Fatalf("GET /market/ideas/: status %d", code)
	}
	tabs := sectionTabsBlock(t, body)
	for _, e := range sectionNav["market"] {
		if !strings.Contains(tabs, `href="`+e.Href+`"`) {
			t.Errorf("market tabs miss %s", e.Href)
		}
	}
	mustContain(t, "market tabs", tabs, `<a href="/market/ideas/" class="active" aria-current="page">Trade ideas</a>`)
	if strings.Count(tabs, `class="active"`) != 1 {
		t.Errorf("market tabs mark %d pages, want 1", strings.Count(tabs, `class="active"`))
	}
	// The sidebar menu renders from the same list.
	mustContain(t, "sidebar", body, `<a href="/market/restock/">Restock planner</a>`)

	// The corporation killmails page lights Corporation > Killmails,
	// not Pilot > Killmails.
	_, body = getPage(t, app, cookie, "/corporations/killmails/")
	tabs = sectionTabsBlock(t, body)
	mustContain(t, "corp killmails tabs", tabs, `<a href="/corporations/killmails/" class="active" aria-current="page">Killmails</a>`)
	if strings.Contains(body, `<a href="/killmails/" class="active">`) {
		t.Error("Pilot > Killmails marked active on the corporation killmails page")
	}

	// A one-page section has no choice to offer: no tabs.
	_, body = getPage(t, app, cookie, "/items/")
	if strings.Contains(body, `class="section-tabs"`) {
		t.Error("the Item Browser shows section tabs")
	}
}

// sectionTabsBlock returns a page's section tab strip.
func sectionTabsBlock(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, `<nav class="section-tabs"`)
	if start < 0 {
		t.Fatal("no section tabs on the page")
	}
	end := strings.Index(body[start:], "</nav>")
	return body[start : start+end]
}
