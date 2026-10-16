package app

import (
	"context"
	"evesynapse/internal/app/swarm"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
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
	base, err := resolveLink(linkChoice{Features: []string{"contacts"}})
	if err != nil {
		t.Fatal(err)
	}
	with, err := resolveLink(linkChoice{Features: []string{"contacts"}, Enhance: []string{"esi-alliances.read_contacts.v1"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(with.Scopes) != len(base.Scopes)+1 || !contains(with.Scopes, "esi-alliances.read_contacts.v1") {
		t.Errorf("opting in to alliance contacts gave %v from %v", with.Scopes, base.Scopes)
	}
	if _, err := resolveLink(linkChoice{Features: []string{"wallet"}, Enhance: []string{"esi-alliances.read_contacts.v1"}}); err == nil {
		t.Error("a contacts permission was accepted without the contacts feature")
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
		if exempt[m.ID] {
			continue // operator-level: never a switch in the chooser
		}
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

	plus := eveScopeParam(t, get(app, nil, "/auth/eve?feature=contacts&enhance=esi-alliances.read_contacts.v1"))
	if !contains(plus, "esi-alliances.read_contacts.v1") || !contains(plus, "esi-characters.read_contacts.v1") {
		t.Errorf("contacts with the alliance extra asks for %v", plus)
	}
	// An extra of a feature that is not chosen is dropped, not requested.
	dropped := eveScopeParam(t, get(app, nil, "/auth/eve?feature=skills&enhance=esi-alliances.read_contacts.v1"))
	if contains(dropped, "esi-alliances.read_contacts.v1") {
		t.Errorf("an extra of an unchosen feature was requested: %v", dropped)
	}

	full := eveScopeParam(t, get(app, nil, "/auth/eve"))
	sort.Strings(full)
	wantFull := append([]string(nil), eveScopes...)
	sort.Strings(wantFull)
	if strings.Join(full, " ") != strings.Join(wantFull, " ") {
		t.Errorf("a plain /auth/eve asks for %v, want the full list", full)
	}

	for _, bad := range []string{"/auth/eve?feature=nosuch", "/auth/eve?feature=skills&enhance=esi-nosuch.scope.v1"} {
		if rec := get(app, nil, bad); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", bad, rec.Code)
		}
	}
}

// TestEmptySubmittedChoiceNeverWidens: a submitted choice with every
// feature unticked is a choice of nothing. It must loop back to the
// chooser at zero, never read as "no choice" and widen to every scope.
// A plain /auth/eve with no choice at all stays the full link.
func TestEmptySubmittedChoiceNeverWidens(t *testing.T) {
	app := linkApp(t)

	rec := get(app, nil, "/auth/eve?submitted=1")
	loc := rec.Header().Get("Location")
	if rec.Code != http.StatusFound || strings.Contains(loc, "login.eveonline.com") || strings.Contains(loc, "scope=") {
		t.Fatalf("an empty submitted choice went to EVE (status %d, Location %q); it must loop to the chooser", rec.Code, loc)
	}
	if !strings.HasPrefix(loc, "/auth/link") {
		t.Errorf("an empty submitted choice redirects to %q, want the chooser", loc)
	}

	body := get(app, nil, "/auth/link?submitted=1").Body.String()
	if !strings.Contains(body, "you only sign in") {
		t.Errorf("the zero-choice reveal does not show the zero count:\n%s", body)
	}

	full := eveScopeParam(t, get(app, nil, "/auth/eve"))
	if len(full) != len(eveScopes) {
		t.Errorf("a plain /auth/eve asks for %d scopes, want the full %d", len(full), len(eveScopes))
	}
}

// TestFreshChooserOpensFullyTicked: there is no extras step and no
// playstyle question. A fresh sign-up lands on the categorized readout
// with every feature and every permission ticked, because the optional
// scopes are part of how the features are expected to work. Unticking
// is the player's move from there.
func TestFreshChooserOpensFullyTicked(t *testing.T) {
	app := linkApp(t)

	body := get(app, nil, "/auth/link").Body.String()
	if strings.Contains(body, "What do you want to do") || !strings.Contains(body, "This is what will be turned on") {
		t.Errorf("a fresh sign-up does not open on the readout:\n%s", body)
	}
	for _, want := range []string{`name="feature" value="skills"`, `name="feature" value="fitting"`} {
		if !strings.Contains(body, want+" data-scopes=") && !strings.Contains(body, want) {
			t.Errorf("the readout misses the feature checkbox %q", want)
		}
	}
	for _, frag := range []string{`name="enhance" value="esi-alliances.read_contacts.v1" checked`, `name="enhance" value="esi-wallet.read_corporation_wallets.v1" checked`} {
		if !strings.Contains(body, frag) {
			t.Errorf("the readout does not open with %q ticked", frag)
		}
	}
	if strings.Contains(body, ">Extra: ") || strings.Contains(body, "Tick every extra permission") || strings.Contains(body, "Choose features myself") {
		t.Error("the readout still speaks of extras or a step beyond it")
	}
	// A legacy cards link lands on the same readout, not a dead step.
	if legacy := get(app, nil, "/auth/link?from=cards").Body.String(); !strings.Contains(legacy, "This is what will be turned on") {
		t.Error("a legacy cards link does not land on the readout")
	}
}

// TestChooserPages: the readout renders every feature under its kind
// of play, tells the player how many permissions EVE will ask for, and
// its one form goes straight to EVE. Legacy step links land here too.
func TestChooserPages(t *testing.T) {
	app := linkApp(t)
	rec := get(app, nil, "/auth/link")
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("/auth/link: status %d", rec.Code)
	}
	for _, a := range linkActivities {
		if a.ID == activityEverything {
			continue // no feature declares it; it names no group
		}
		if !strings.Contains(body, html.EscapeString(a.Title)) {
			t.Errorf("the readout groups miss %q", a.Title)
		}
	}
	if !strings.Contains(body, "This is what will be turned on") || !strings.Contains(body, "Spread Scanner") || !strings.Contains(body, "Complete sign up") || strings.Contains(body, "Update the list") || strings.Contains(body, "Go back") || !strings.Contains(body, "Jump to bottom") {
		t.Errorf("the readout is missing its parts:\n%s", body)
	}
	// Complete sign up is a plain submit of the one form, whose action
	// is EVE itself: no formaction detour.
	if !strings.Contains(body, `<form method="get" action="/auth/eve" data-lk-form>`) || strings.Contains(body, "formaction") {
		t.Error("the readout form does not post straight to /auth/eve")
	}

	rec = get(app, nil, "/auth/link?advanced=1")
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "Choose the features") || !strings.Contains(rec.Body.String(), "esi-alliances.read_contacts.v1") {
		t.Errorf("a legacy advanced link does not land on the readout (status %d)", rec.Code)
	}

	// Unticking a feature another one needs does not remove it.
	rec = get(app, nil, "/auth/link?submitted=1&feature=doctrines")
	if !strings.Contains(rec.Body.String(), "Needed by Doctrines") {
		t.Error("the readout does not say Fitting is needed by Doctrines")
	}
}

// formControls collects what a browser would submit from the readout
// form: every successful control (checked checkboxes, hidden fields),
// in the form the page renders.
func formControls(t *testing.T, body string) url.Values {
	t.Helper()
	start := strings.Index(body, `data-lk-form`)
	if start < 0 {
		t.Fatal("the readout form is missing")
	}
	form := body[start:]
	if end := strings.Index(form, "</form>"); end >= 0 {
		form = form[:end]
	}
	vals := url.Values{}
	for _, tag := range strings.FieldsFunc(form, func(r rune) bool { return r == '<' }) {
		if !strings.HasPrefix(tag, "input ") {
			continue
		}
		attr := func(name string) string {
			i := strings.Index(tag, name+`="`)
			if i < 0 {
				return ""
			}
			rest := tag[i+len(name)+2:]
			j := strings.Index(rest, `"`)
			if j < 0 {
				return ""
			}
			return rest[:j]
		}
		name := attr("name")
		if name == "" || strings.Contains(tag, "disabled") {
			continue
		}
		if attr("type") == "checkbox" && !strings.Contains(tag, "checked") {
			continue
		}
		vals.Add(name, attr("value"))
	}
	return vals
}

// TestCompleteSignUpSendsTheFullSelection: clicking Complete sign up on
// the fresh readout sends the browser's form to EVE, and EVE is asked
// for the full selection: every required scope and every optional one,
// the scopes that used to hide behind an extras step included.
func TestCompleteSignUpSendsTheFullSelection(t *testing.T) {
	app := linkApp(t)

	body := get(app, nil, "/auth/link").Body.String()
	// Not one checkbox on the readout starts unticked.
	boxRe := regexp.MustCompile(`<input[^>]*name="(feature|enhance)"[^>]*>`)
	boxes := boxRe.FindAllString(body, -1)
	if len(boxes) == 0 {
		t.Fatal("the readout renders no checkboxes")
	}
	for _, b := range boxes {
		if !strings.Contains(b, "checked") {
			t.Errorf("a checkbox starts unticked: %s", b)
		}
	}

	got := eveScopeParam(t, get(app, nil, "/auth/eve?"+formControls(t, body).Encode()))
	want := app.requestableScopes()
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("Complete sign up asks EVE for %v, want the full selection %v", got, want)
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

	if body := get(app, cookie, "/auth/link").Body.String(); !strings.Contains(body, "This is what will be turned on") {
		t.Fatal("a new account does not open on the readout")
	}
	eveScopeParam(t, get(app, cookie, "/auth/eve?feature=skills&feature=wallet"))
	body := get(app, cookie, "/auth/link").Body.String()
	if !strings.Contains(body, "the choices you made last time") || !strings.Contains(body, "Skills") {
		t.Errorf("the next link does not open on the remembered choice:\n%s", body)
	}
}

// TestCorporationScopesAreRoleOptIns: picking the corporation playstyle
// does not ask for the role-gated corporation permissions. They are offered
// as opt-ins framed by the in-game role each needs, and ticking one adds
// exactly that scope.
func TestCorporationScopesAreRoleOptIns(t *testing.T) {
	const wallets = "esi-wallet.read_corporation_wallets.v1"
	res, err := resolveLink(linkChoice{Activities: []string{"corporation"}})
	if err != nil {
		t.Fatal(err)
	}
	if contains(res.Scopes, wallets) || contains(res.Required, wallets) {
		t.Errorf("the corporation playstyle asks for %q without being told the role", wallets)
	}
	var offered *linkRoleOptIn
	for i := range res.RoleOptIns {
		if res.RoleOptIns[i].Scope == wallets {
			offered = &res.RoleOptIns[i]
		}
	}
	if offered == nil || offered.Chosen || offered.Roles != "Accountant or Junior Accountant" {
		t.Fatalf("the wallet opt-in = %+v, want an unticked one framed by the Accountant role", offered)
	}
	with, err := resolveLink(linkChoice{Activities: []string{"corporation"}, Enhance: []string{wallets}})
	if err != nil || !contains(with.Scopes, wallets) || len(with.Scopes) != len(res.Scopes)+1 {
		t.Errorf("opting in to the wallets gave %v from %v (%v)", with.Scopes, res.Scopes, err)
	}
	// Not offered without a corporation feature to go with it.
	if _, err := resolveLink(linkChoice{Features: []string{"skills"}, Enhance: []string{wallets}}); err == nil {
		t.Error("a corporation permission was accepted without a corporation feature")
	}
	// The full link still carries it.
	if !contains(requestableScopes(), wallets) {
		t.Error("the full link lost the corporation wallets permission")
	}
}

// TestSharedCacheIsNotAFeature: the shared cache is never a switch in the
// chooser. Its one extra, the structures permission, is offered only on a
// server that has the cache on (SHARED_CACHE open or allowlist).
func TestSharedCacheIsNotAFeature(t *testing.T) {
	app := linkApp(t)
	const structures = "esi-universe.read_structures.v1"
	for _, f := range linkFeatures() {
		if f.ID == "swarm" {
			t.Fatal("the shared cache is offered as a feature")
		}
	}

	// The readout lists every feature, and the Structures feature's own
	// permission is this same scope; what must not appear with the
	// cache off is the server offer: the "This server" section and its
	// enhance checkbox.
	off := get(app, nil, "/auth/link?submitted=1&feature=skills").Body.String()
	if strings.Contains(off, "This server") || strings.Contains(off, `name="enhance" value="`+structures+`"`) {
		t.Error("the structures extra is offered on a server with the shared cache off")
	}
	if rec := get(app, nil, "/auth/eve?feature=skills&enhance="+structures); rec.Code != http.StatusBadRequest {
		t.Errorf("the structures extra was accepted with the cache off (status %d)", rec.Code)
	}

	swarm.StateFor(app).Cfg = swarm.SwarmConfig{Mode: swarm.SwarmOpen, Secret: swarmTestSecret, GossipHours: swarm.DefaultSwarmGossipHours}
	on := get(app, nil, "/auth/link?submitted=1&feature=skills").Body.String()
	if !strings.Contains(on, "This server") || !strings.Contains(on, structures) {
		t.Error("the structures extra is not offered with the shared cache on")
	}
	if strings.Contains(on, "Shared Cache") || strings.Contains(on, ">swarm<") {
		t.Error("the page offers the shared cache itself")
	}
	if got := eveScopeParam(t, get(app, nil, "/auth/eve?feature=skills&enhance="+structures)); !contains(got, structures) {
		t.Errorf("opting in asked for %v", got)
	}
	if got := eveScopeParam(t, get(app, nil, "/auth/eve?feature=skills")); contains(got, structures) {
		t.Error("the structures extra is ticked by default")
	}
}
