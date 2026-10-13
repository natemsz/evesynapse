package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestNavStateRenderedFromCookie: a saved rail or hidden layout is on
// the <html> tag in the server's own response, so the page paints in
// it without any script having run. A saved expanded choice leaves
// the tag plain (expanded is the attribute's absence). With no cookie
// or an unknown value the desktop default is the icon rail.
func TestNavStateRenderedFromCookie(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	user, err := q.CreateUser(context.Background())
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	session := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")

	page := func(navValue string) string {
		req := httptest.NewRequest(http.MethodGet, "/characters/", nil)
		req.AddCookie(session)
		if navValue != "" {
			req.AddCookie(&http.Cookie{Name: navCookie, Value: navValue})
		}
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("nav=%q: status %d", navValue, rec.Code)
		}
		return rec.Body.String()
	}

	mustContain(t, "rail cookie", page("rail"), `<html lang="en" data-nav="rail">`)
	mustContain(t, "hidden cookie", page("hidden"), `<html lang="en" data-nav="hidden">`)
	mustContain(t, "expanded cookie", page("expanded"), `<html lang="en" data-nav="expanded">`)
	for _, rail := range []string{"", "bogus", `"><script>`} {
		mustContain(t, "nav="+rail, page(rail), `<html lang="en" data-nav="rail">`)
	}

	// The category of the page being shown is open while the menu shows
	// its labels, and left folded in the icon rail.
	const open = `<details class="branch active" open data-nav-category="pilot">`
	mustContain(t, "expanded menu", page("expanded"), open)
	mustContain(t, "hidden menu", page("hidden"), open)
	for _, folded := range []string{"", "rail"} {
		if body := page(folded); strings.Contains(body, open) || !strings.Contains(body, `<details class="branch active" data-nav-category="pilot">`) {
			t.Fatalf("nav=%q: the icon rail rendered the current category open", folded)
		}
	}
	if n := strings.Count(page("expanded"), `" open data-nav-category=`); n != 1 {
		t.Fatalf("%d categories rendered open, want only the current one", n)
	}
}
