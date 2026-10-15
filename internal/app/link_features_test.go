package app

import (
	"context"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
)

func inEveScopes(scope string) bool { return contains(eveScopes, scope) }

// TestFullLinkIsTodaysLink: with every module built in, the link that asks
// for everything is exactly the scope list the developer portal allows,
// and the plain /auth/eve (no choice) asks for it too.
func TestFullLinkIsTodaysLink(t *testing.T) {
	got := append([]string(nil), requestableScopes()...)
	want := append([]string(nil), eveScopes...)
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("the full link asks for\n%v\nbut the portal list is\n%v", got, want)
	}
}

// TestEveryChoiceStaysInsideThePortalList: whatever is picked, the link
// asks only for a subset of what the developer portal allows (a scope
// outside it would fail the sign-in for everyone).
func TestEveryChoiceStaysInsideThePortalList(t *testing.T) {
	check := func(name string, c linkChoice) {
		res, err := resolveLink(c)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			return
		}
		for _, s := range res.Scopes {
			if !inEveScopes(s) {
				t.Errorf("%s asks for %q, which is not in the portal list", name, s)
			}
		}
	}
	for _, a := range linkActivities {
		check("activity "+a.ID, linkChoice{Activities: []string{a.ID}})
	}
	for _, f := range linkFeatures() {
		var enhance []string
		for _, s := range f.Enhance {
			enhance = append(enhance, s.Scope)
		}
		check("feature "+f.ID, linkChoice{Features: []string{f.ID}, Enhance: enhance})
	}
	check("everything", fullLinkChoice())
}

// TestPlaystylePicksGrantRequiredScopesOnly: an answer to the question
// asks for what its features cannot work without; the extras are offered,
// never ticked.
func TestPlaystylePicksGrantRequiredScopesOnly(t *testing.T) {
	for _, a := range linkActivities {
		res, err := resolveLink(linkChoice{Activities: []string{a.ID}})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(res.Scopes, " ") != strings.Join(res.Required, " ") {
			t.Errorf("%s asks for %v but only %v are required", a.ID, res.Scopes, res.Required)
		}
		for _, e := range res.Enhancements {
			if e.Chosen {
				t.Errorf("%s has the extra %q ticked", a.ID, e.Scope)
			}
		}
	}
	res, _ := resolveLink(linkChoice{Activities: []string{"pilots"}})
	if len(res.Enhancements) == 0 {
		t.Error("tracking pilots offers no extra permissions (mail, contacts, ...)")
	}
}

// TestExtrasBelongToChosenFeatures: an opt-in adds exactly its scope, and
// one that belongs to no chosen feature is refused.
func TestExtrasBelongToChosenFeatures(t *testing.T) {
	base, err := resolveLink(linkChoice{Features: []string{"mail"}})
	if err != nil {
		t.Fatal(err)
	}
	with, err := resolveLink(linkChoice{Features: []string{"mail"}, Enhance: []string{"esi-mail.send_mail.v1"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(with.Scopes) != len(base.Scopes)+1 || !contains(with.Scopes, "esi-mail.send_mail.v1") {
		t.Errorf("opting in to send mail gave %v from %v", with.Scopes, base.Scopes)
	}
	if _, err := resolveLink(linkChoice{Features: []string{"wallet"}, Enhance: []string{"esi-mail.send_mail.v1"}}); err == nil {
		t.Error("a mail permission was accepted without the mail feature")
	}
	if _, err := resolveLink(linkChoice{Features: []string{"nosuchfeature"}}); err == nil {
		t.Error("an unknown feature was accepted")
	}
	if _, err := resolveLink(linkChoice{Activities: []string{"nosuchactivity"}}); err == nil {
		t.Error("an unknown activity was accepted")
	}
}

// TestRequiresClosureIsShown: Doctrines needs Fitting, and the row says so.
func TestRequiresClosureIsShown(t *testing.T) {
	res, err := resolveLink(linkChoice{Features: []string{"doctrines"}})
	if err != nil {
		t.Fatal(err)
	}
	var fitting *linkFeatureRow
	for i := range res.Features {
		if res.Features[i].ID == "fitting" {
			fitting = &res.Features[i]
		}
	}
	if fitting == nil || fitting.Pinned || len(fitting.NeededBy) != 1 || fitting.NeededBy[0] != "Doctrines" {
		t.Fatalf("fitting in the closure of doctrines = %+v", fitting)
	}
	view := &linkView{}
	fillLinkView(view, res)
	locked := false
	for _, f := range view.Features {
		if f.ID == "fitting" && f.Locked && strings.Contains(f.Why, "Doctrines") {
			locked = true
		}
	}
	if !locked {
		t.Errorf("the reveal does not show fitting as needed by doctrines: %+v", view.Features)
	}
}

// TestEveryModuleIsReachable: the answers reach every module, so the
// sign-up can switch each of them on: each module declares the kind of
// play it serves (the shared cache is the one operator-level exception,
// reached by "a bit of everything" and the advanced list), and each kind of
// play reaches at least one feature.
func TestEveryModuleIsReachable(t *testing.T) {
	exempt := map[string]bool{"swarm": true}
	for _, m := range registeredModules {
		if len(m.Activities) == 0 && !exempt[m.ID] {
			t.Errorf("module %q declares no activity, so no answer reaches it", m.ID)
		}
		for _, a := range m.Activities {
			if _, ok := linkActivityByID(a); !ok || a == activityEverything {
				t.Errorf("module %q names the unknown activity %q", m.ID, a)
			}
		}
	}
	for _, a := range linkActivities {
		res, err := resolveLink(linkChoice{Activities: []string{a.ID}})
		if err != nil || len(res.Features) == 0 {
			t.Errorf("answering %q reaches no feature (%v)", a.ID, err)
		}
	}
	all, _ := resolveLink(linkChoice{Activities: []string{activityEverything}})
	for _, m := range registeredModules {
		found := false
		for _, f := range all.Features {
			found = found || f.ID == m.ID
		}
		if !found {
			t.Errorf("a bit of everything does not reach module %q", m.ID)
		}
	}
}

func linkApp(t *testing.T) *Application {
	t.Helper()
	app, _, _ := buildCorpTestApp(t, &countingTransport{})
	app.cfg.eveClientID, app.cfg.eveClientSecret = "client", "secret"
	app.cfg.eveCallbackURL = "http://node.example/auth/callback"
	return app
}

func get(app *Application, cookie *http.Cookie, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	return rec
}

func eveScopeParam(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	if rec.Code != http.StatusFound {
		t.Fatalf("status %d, want a redirect to EVE: %s", rec.Code, rec.Body.String())
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil || loc.Host != "login.eveonline.com" {
		t.Fatalf("redirect = %q", rec.Header().Get("Location"))
	}
	return strings.Fields(loc.Query().Get("scope"))
}

// TestLinkToEVEIsBuiltFromTheChoice: the link carries exactly the union the
// server works out, and a plain /auth/eve is still the full link.
func TestLinkToEVEIsBuiltFromTheChoice(t *testing.T) {
	app := linkApp(t)

	got := eveScopeParam(t, get(app, nil, "/auth/eve?feature=skills"))
	want, _ := resolveLink(linkChoice{Features: []string{"skills"}})
	if strings.Join(got, " ") != strings.Join(want.Scopes, " ") || len(got) == 0 {
		t.Errorf("feature=skills asks for %v, want %v", got, want.Scopes)
	}
	for _, s := range got {
		if !inEveScopes(s) {
			t.Errorf("the link asks for %q, outside the portal list", s)
		}
	}

	plus := eveScopeParam(t, get(app, nil, "/auth/eve?feature=mail&enhance=esi-mail.send_mail.v1"))
	if !contains(plus, "esi-mail.send_mail.v1") || contains(plus, "esi-mail.organize_mail.v1") {
		t.Errorf("mail with the send extra asks for %v", plus)
	}

	full := eveScopeParam(t, get(app, nil, "/auth/eve"))
	sort.Strings(full)
	wantFull := append([]string(nil), eveScopes...)
	sort.Strings(wantFull)
	if strings.Join(full, " ") != strings.Join(wantFull, " ") {
		t.Errorf("a plain /auth/eve asks for %v, want the full list", full)
	}

	for _, bad := range []string{"/auth/eve?feature=nosuch", "/auth/eve?enhance=esi-mail.send_mail.v1", "/auth/eve?feature=skills&enhance=esi-wallet.read_character_wallet.v1"} {
		if rec := get(app, nil, bad); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", bad, rec.Code)
		}
	}
}

// TestChooserPages: the cards, the reveal and the advanced list render, and
// the reveal tells the player how many permissions EVE will ask for.
func TestChooserPages(t *testing.T) {
	app := linkApp(t)
	rec := get(app, nil, "/auth/link")
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("/auth/link: status %d", rec.Code)
	}
	for _, a := range linkActivities {
		if !strings.Contains(body, html.EscapeString(a.Title)) {
			t.Errorf("the cards miss %q", a.Title)
		}
	}
	if !strings.Contains(body, `class="lk-card ti-acc ti-a1"`) {
		t.Error("the cards do not carry the accent classes the Trade Ideas page uses")
	}

	rec = get(app, nil, "/auth/link?activity=market")
	body = rec.Body.String()
	if !strings.Contains(body, "This is what will be turned on") || !strings.Contains(body, "Spread Scanner") || !strings.Contains(body, "Continue to EVE") {
		t.Errorf("the reveal for the market is missing its parts:\n%s", body)
	}

	rec = get(app, nil, "/auth/link?advanced=1")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Choose the features") || !strings.Contains(rec.Body.String(), "esi-mail.send_mail.v1") {
		t.Errorf("the advanced list is missing its parts (status %d)", rec.Code)
	}

	// Unticking a feature another one needs does not remove it.
	rec = get(app, nil, "/auth/link?submitted=1&feature=doctrines")
	if !strings.Contains(rec.Body.String(), "Needed by Doctrines") {
		t.Error("the reveal does not say Fitting is needed by Doctrines")
	}
}

// TestChoiceIsRemembered: the account's last choice opens the next link
// preselected, as a confirmation.
func TestChoiceIsRemembered(t *testing.T) {
	app := linkApp(t)
	ctx := context.Background()
	user, err := app.queries.CreateUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seedCharacter(t, app.queries, user.ID, fixtureCharA, "Fixture Ceo")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")

	if body := get(app, cookie, "/auth/link").Body.String(); !strings.Contains(body, "What do you want to do") {
		t.Fatal("a new account is not asked what it wants to do")
	}
	eveScopeParam(t, get(app, cookie, "/auth/eve?feature=skills&feature=wallet"))
	body := get(app, cookie, "/auth/link").Body.String()
	if !strings.Contains(body, "the choices you made last time") || !strings.Contains(body, "Skills") {
		t.Errorf("the next link does not open on the remembered choice:\n%s", body)
	}
}
