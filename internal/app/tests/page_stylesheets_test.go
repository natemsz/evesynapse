package tests

// Page stylesheets: the feature sheets (plan, trade ideas, link
// chooser) load only on the pages whose markup uses their classes,
// never site-wide. The map below is the contract: a page gains a
// sheet by rendering markup that needs it, and this test fails if
// base.html's conditionals and the pages drift apart.

import (
	"context"
	"strings"
	"testing"

	"evesynapse/internal/apptest"
)

func TestPageStylesheetsLoadOnlyWhereUsed(t *testing.T) {
	rig := apptest.Build(t, nil)
	q := rig.Queries()
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	apptest.SeedCharacter(t, q, user.ID, apptest.FixtureCharA, "Alpha Pilot")
	cookie := apptest.SessionCookie(t, rig, user.ID, apptest.FixtureCharA, "Alpha Pilot")

	const (
		plan  = `/static/plan.css?v=`
		ideas = `/static/tradeideas.css?v=`
		link  = `/static/link.css?v=`
	)
	// want: sheets of the three a page must carry; every other of
	// the three must be absent. style.css and bg.css always load.
	cases := []struct {
		path string
		want []string
	}{
		{"/", []string{ideas}},                // home trade-ideas widget
		{"/planner/", []string{plan}},         // build planner
		{"/industry/", []string{plan}},        // planner glyph on Blueprints
		{"/industry/pi/", []string{plan}},     // PI planner
		{"/market/ideas/", []string{ideas}},   // trade ideas page
		{"/auth/link", []string{ideas, link}}, // chooser cards take the accents
		{"/characters/", nil},                 // a page using none of them
	}
	all := []string{plan, ideas, link}
	for _, tc := range cases {
		code, body := apptest.GetPage(t, rig, cookie, tc.path)
		if code != 200 {
			t.Errorf("GET %s status = %d, want 200", tc.path, code)
			continue
		}
		for _, sheet := range []string{`/static/style.css?v=`, `/static/bg.css?v=`} {
			if !strings.Contains(body, sheet) {
				t.Errorf("GET %s lost the always-on sheet %s", tc.path, sheet)
			}
		}
		for _, sheet := range all {
			want := false
			for _, w := range tc.want {
				if w == sheet {
					want = true
				}
			}
			if got := strings.Contains(body, sheet); got != want {
				t.Errorf("GET %s stylesheet %s present = %v, want %v", tc.path, sheet, got, want)
			}
		}
	}
}
