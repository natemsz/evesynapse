package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// warmIDs drains the worker's priority set (sorted).
func warmIDs(app *Application) []int64 {
	return app.takePriorityCharacters()
}

// TestSyncWarmInvalidCharacterWarmsNothing: garbage in
// ?character= used to warm every linked character (the parse
// error was ignored and want stayed 0); now it warms nothing and
// still bounces back to the Sync page.
func TestSyncWarmInvalidCharacterWarmsNothing(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	grantTestAdmin(app, fixtureCharA)
	user, err := q.CreateUser(context.Background())
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	seedCharacter(t, q, user.ID, fixtureCharB, "Fixture Beta")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")

	for _, raw := range []string{"abc", "12x", "-1", "0"} {
		code, _, _ := doReq(t, app, "POST", "/sync/warm?character="+raw, nil, cookie)
		if code != http.StatusSeeOther {
			t.Fatalf("POST /sync/warm?character=%s = %d, want 303", raw, code)
		}
		if got := warmIDs(app); len(got) != 0 {
			t.Fatalf("?character=%s warmed %v, want nothing", raw, got)
		}
	}

	// The documented shapes still work: one ID warms one
	// character, no parameter warms all of them.
	code, _, _ := doReq(t, app, "POST", "/sync/warm?character=90000002", nil, cookie)
	if code != http.StatusSeeOther {
		t.Fatalf("POST one character = %d, want 303", code)
	}
	if got, want := warmIDs(app), []int64{fixtureCharB}; !reflect.DeepEqual(got, want) {
		t.Fatalf("one character warmed %v, want %v", got, want)
	}
	code, _, _ = doReq(t, app, "POST", "/sync/warm", nil, cookie)
	if code != http.StatusSeeOther {
		t.Fatalf("POST no parameter = %d, want 303", code)
	}
	if got, want := warmIDs(app), []int64{fixtureCharA, fixtureCharB}; !reflect.DeepEqual(got, want) {
		t.Fatalf("no parameter warmed %v, want %v", got, want)
	}
}

// TestSkillsRedirectEscapesCharacter: the /skills/ bounce to
// /character/ escapes the parameter instead of pasting it raw
// into the Location, where "&admin=1" would have become a second
// parameter.
func TestSkillsRedirectEscapesCharacter(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	user, err := q.CreateUser(context.Background())
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")

	// The raw query carries a smuggled second parameter; the
	// handler must forward the character value escaped, not pasted.
	req := httptest.NewRequest("GET", "/skills/?character="+`90000001%26admin%3D1`, nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("GET /skills/ = %d, want 303", rec.Code)
	}
	loc := rec.Header().Get("Location")
	// %26 must survive escaped: a raw "&admin=1" in the Location
	// would be a parameter of /character/, not of the value.
	if loc != "/character/?character=90000001%26admin%3D1" {
		t.Fatalf("Location = %q", loc)
	}
}
