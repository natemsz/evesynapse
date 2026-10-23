package tests

import (
	"context"
	"strings"
	"testing"

	"evesynapse/internal/apptest"
)

// The anonymous front door is the landing page: hero and heritage
// copy verbatim, the hero's three doors (sign up, the demo, the
// public tools), a sign-in link for existing members (plain login,
// not the sign-up chooser), the tools inventory, then what sets
// EveSynapse apart with the load-test stats and their fine print,
// then the trust points, and no em dashes anywhere on the page.
func TestLandingAnonymous(t *testing.T) {
	rig := apptest.Build(t, nil)
	code, body := apptest.GetPage(t, rig, nil, "/")
	if code != 200 {
		t.Fatalf("anonymous / status = %d", code)
	}
	for _, want := range []string{
		"One connected system for <span class=\"ld-amp\">your whole fleet.</span>",
		"Market, fitting, industry, intel and corporation tools in one place, all reading the same stored data.",
		"Sign up free",
		"Try the demo",
		"data-open-tools",
		"Try the demo runs on a sample fleet. No sign-in needed.",
		"Linking asks only for the scopes behind the features you pick",
		"EveSynapse is free to use forever.",
		"No donation pop-ups.",
		"concurrent pilots on one small server, load-tested",
		"pilots kept fully current, sync load-tested",
		"rosters of 12 to 14,800 pilots driven through the real sync cycle",
		"Run on a 8 core ARM server with 24 GB of RAM",
		"Rebuilt over <em>4 years</em>. Originating in <em>2013</em>. Built for <em>2026</em>.",
		"href=\"/auth/login\"",        // existing members sign in plainly
		"href=\"/auth/link\"",         // new members link characters
		"href=\"/auth/demo\"",         // the demo door
		"href=\"/planner/\"",          // the public industry face is the Build Planner
		"href=\"/contracts/search/\"", // the public contract search
		"href=\"/market/appraise/\"",  // the public appraisal tool
		"href=\"/map/\"",              // the public universe map
		"href=\"/intel/kills/\"",      // the public killboard
		"Appraisal",
		"Contract Search",
		"Fleet Overview",
		"Every tool in the box.",
		"Twenty-three modules plus the core pages, all reading the same stored data.",
		"Developed in Seattle, WA by",
		"BSD-3-Clause-Clear",
		"© 2026 Synap6.io LLC, dba Synap6",
		"intellectual property of CCP Games hf",
		"Ship Replacement",
		"What sets us apart",
		"One connected system.",
		"every character syncs in the background",
		"a medium-sized group gets doctrines, ship replacement and intel channels",
		"full dogma simulation",
		"Save to EVE built in",
		"priced from confirmed contract sales",
		"appraised line by line, and deals are flagged on the row",
		"Gate camps are counted from ship kills only.",
		"author doctrines in the fitting tool itself",
		"Free. No ads. No tracking.",
		"Free to use forever, visitor and member alike.",
		"No donation pop-ups, no tip jar, no nag screens.",
		"solo small-gang low-sec pilot",
		"No coalition funding",
		"Intel and reference",
		"/static/landing.js",
		"Universe Map",
		"Killboard",
		"Route Planner",
		"D-Scan Parser",
		"Intel Channels",
		"Axon",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("landing is missing %q", want)
		}
	}
	// The section order: the tools inventory sits above what
	// sets EveSynapse apart, then the trust points.
	toolsAt := strings.Index(body, "Every tool in the box.")
	apartAt := strings.Index(body, "One connected system.")
	trustAt := strings.Index(body, "Free. No ads. No tracking.")
	if !(toolsAt >= 0 && toolsAt < apartAt && apartAt < trustAt) {
		t.Error("landing sections out of order: want tools, what sets us apart, then trust")
	}
	// The tools inventory is audited against the tree: features
	// that do not exist yet are never claimed on the front door.
	for _, unbuilt := range []string{"Profit Tracking"} {
		if strings.Contains(body, unbuilt) {
			t.Errorf("landing claims the unbuilt %q", unbuilt)
		}
	}
	// Scope claims belong to the entry point they describe: the
	// picked-scopes wording is the link chooser's, and the page
	// never states it as a property of every EVE login.
	for _, misplaced := range []string{"the EVE login asks for exactly those scopes", "The login asks only for the scopes"} {
		if strings.Contains(body, misplaced) {
			t.Errorf("landing still states the chooser claim as a login property %q", misplaced)
		}
	}
	// The antithesis slogans the copy pass removed stay removed:
	// the landing speaks in the playstyle cards' direct voice.
	for _, tell := range []string{"not a dashboard dump", "Critical first, noise never", "not price lists", "calculated, not drawn", "not a hack", "Data in. Decisions out.", "knows itself", "Each point integrates with the other.", "JavaScript frameworks", "design target", "It turns it into insight"} {
		if strings.Contains(body, tell) {
			t.Errorf("landing still carries the removed slogan %q", tell)
		}
	}
	if strings.Contains(body, "—") {
		t.Error("landing contains an em dash")
	}
	// The industry page itself is members-only (it bounces anonymous
	// visitors home); the nav must never link a visitor into a wall.
	if strings.Contains(body, "href=\"/industry/\"") {
		t.Error("landing nav links to the members-only industry page")
	}
	// The landing renders no alert cards: the old canned
	// example alerts never come back.
	if strings.Contains(body, "ld-brief") {
		t.Error("alert cards rendered on the landing")
	}
	for _, canned := range []string{"Skill queues empty", "Planetary extractors halted"} {
		if strings.Contains(body, canned) {
			t.Errorf("landing still shows the canned example alert %q", canned)
		}
	}
	// The signed-in chrome never leaks onto the landing.
	if strings.Contains(body, "sidebar") {
		t.Error("landing rendered the signed-in sidebar chrome")
	}
}

// Signed-in members skip the landing entirely: they get the
// overview, never the hero.
func TestLandingSkippedWhenSignedIn(t *testing.T) {
	rig := apptest.Build(t, nil)
	q := rig.Queries()
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	apptest.SeedCharacter(t, q, user.ID, apptest.FixtureCharA, "Alpha Pilot")
	cookie := apptest.SessionCookie(t, rig, user.ID, apptest.FixtureCharA, "Alpha Pilot")
	code, body := apptest.GetPage(t, rig, cookie, "/")
	if code != 200 {
		t.Fatalf("signed-in / status = %d", code)
	}
	if strings.Contains(body, "One connected system for") {
		t.Error("signed-in member got the landing hero")
	}
}
