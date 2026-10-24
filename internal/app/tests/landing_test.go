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
// not the sign-up chooser), the tools inventory, then the trust
// points, then the playstyles, and the closing
// trust paragraph at the bottom, and no em dashes anywhere on
// the page.
func TestLandingAnonymous(t *testing.T) {
	rig := apptest.Build(t, nil)
	code, body := apptest.GetPage(t, rig, nil, "/")
	if code != 200 {
		t.Fatalf("anonymous / status = %d", code)
	}
	for _, want := range []string{
		"Everything your fleet needs, in <span class=\"ld-amp\">one connected system.</span>",
		"Track markets, fittings, industry, intel, and corp operations in one place, all using the same shared data.",
		"Sign up free",
		"Try the demo",
		"data-open-tools",
		"The demo uses a sample fleet and needs no sign-in.",
		"Linking asks only for the scopes behind the features you pick",
		"EveSynapse is free to use forever.",
		"No donation pop-ups.",
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
		"Every tool you need, in one place.",
		"Twenty-three modules and the core pages all work from the same shared data.",
		"Developed in Seattle, WA by",
		"BSD-3-Clause-Clear",
		"© 2026 Synap6.io LLC, dba Synap6",
		"intellectual property of CCP Games hf",
		"Ship Replacement",
		"Prices come from EveSynapse's own swept market data, and every price shows its source.",
		"accurately prices items that aren't in market data, like abyssals, capital ships, and blueprints",
		"Public data, no login",
		"Tools using public data require no login at all to use",
		"Sign in only when you want your own characters in the picture.",
		"One stack for the whole corporation.",
		"Directors publish the doctrines for ships and entire fleets, once",
		"Three alts, or a hundred multiboxed characters.",
		"Every character syncs in the background and the briefing leans toward what you actually do",
		"whether that is a market alt army or a single main",
		"Know what your ISK is doing.",
		"Build with the margin already proven.",
		"A hundred pilots, one morning briefing.",
		"If you find it useful, my only ask is that you share it with your friends.",
		"Your keys, your call.",
		"You pick the scopes",
		"the sign-in asks for the scopes behind those features, and no more",
		"Encrypted at rest",
		"Refresh tokens are sealed with AES-256-GCM",
		"Or run it yourself",
		"Open source, one setup script, your own ESI application and your own data.",
		"PKCE on every sign-in",
		"an intercepted code is worthless to anyone else",
		"Revoke on unlink",
		"killing the token at the source is your call",
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
	// The section order: the tools inventory sits above the
	// trust points, then the playstyles, and the closing
	// trust paragraph at the bottom of the page.
	toolsAt := strings.Index(body, "Every tool you need, in one place.")
	trustAt := strings.Index(body, "Your keys, your call.")
	playstylesAt := strings.Index(body, "Three alts, or a hundred multiboxed characters.")
	closingAt := strings.Index(body, "If you find it useful, my only ask is that you share it with your friends.")
	if !(toolsAt >= 0 && toolsAt < trustAt && trustAt < playstylesAt && playstylesAt < closingAt) {
		t.Error("landing sections out of order: want tools, trust, playstyles, then the closing trust paragraph")
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
	// The closing copy is the owner's own wording;
	// the stand-in sentences it replaced never appear.
	for _, replaced := range []string{"Fitting in the browser", "A full fitting simulator in a browser tab", "full Dogma simulation", "Prices we compute ourselves", "Every price comes from our own swept market data", "Search every public contract in New Eden in one place", "Gate camps are counted from ship kills only.", "Directors can author doctrine fits in the fitting tool itself"} {
		if strings.Contains(body, replaced) {
			t.Errorf("landing still carries the replaced copy %q", replaced)
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
	if strings.Contains(body, "Everything your fleet needs") {
		t.Error("signed-in member got the landing hero")
	}
}
