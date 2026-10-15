//go:build core_only

package app

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// TestCoreOnlyOmitsModules: in the core-only build no optional module
// is compiled in -- the PI planner's nav entry, route and sitemap line
// are all absent.
func TestCoreOnlyOmitsModules(t *testing.T) {
	if len(registeredModules) != 0 {
		t.Fatalf("core-only build registered %d modules, want none", len(registeredModules))
	}
	for section, entries := range sectionNav {
		for _, e := range entries {
			if e.Href == "/industry/pi/" {
				t.Errorf("nav section %q lists /industry/pi/ in the core-only build", section)
			}
		}
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
	if code, _ := getPage(t, app, cookie, "/industry/pi/"); code != http.StatusNotFound {
		t.Errorf("GET /industry/pi/ in the core-only build: status %d, want 404", code)
	}

	sitemap, err := app.buildSitemap(ctx, "https://node.example.org")
	if err != nil {
		t.Fatalf("sitemap: %v", err)
	}
	if strings.Contains(string(sitemap), "/industry/pi/") {
		t.Error("sitemap lists /industry/pi/ in the core-only build")
	}
}
