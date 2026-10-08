package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestNavStateRenderedFromCookie: a saved rail or hidden layout is on
// the <html> tag in the server's own response, so the page paints in
// it without any script having run. Expanded, absent and unknown
// values leave the tag plain.
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
	for _, plain := range []string{"", "expanded", "bogus", `"><script>`} {
		mustContain(t, "nav="+plain, page(plain), `<html lang="en">`)
	}
}
