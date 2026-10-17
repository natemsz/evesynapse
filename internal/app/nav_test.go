package app

import (
	"context"
	"net/http"
	"strconv"
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
	// The strip renders above the glass, not inside it: the nav
	// precedes .page-body in the document.
	if tabsAt, bodyAt := strings.Index(body, `<nav class="section-tabs"`), strings.Index(body, `<div class="page-body">`); tabsAt < 0 || bodyAt < 0 || tabsAt > bodyAt {
		t.Error("section tabs render inside .page-body; they belong above the glass, under the title")
	}
	for _, e := range sectionNav["market"] {
		if !strings.Contains(tabs, `href="`+e.Href+`"`) {
			t.Errorf("market tabs miss %s", e.Href)
		}
	}
	mustContain(t, "market tabs", tabs, `<a href="/market/ideas/" class="active" aria-current="page">Trade Ideas</a>`)
	if strings.Count(tabs, `class="active"`) != 1 {
		t.Errorf("market tabs mark %d pages, want 1", strings.Count(tabs, `class="active"`))
	}
	// The sidebar menu renders from the same list.
	mustContain(t, "sidebar", body, `<a href="/market/restock/">Restock Planner</a>`)

	// The corporation strip is grouped: the strip shows the four
	// clusters, and the page itself is marked in the sub-row under
	// its cluster. The corporation killmails page lights Comms &
	// Admin (whose first page Killmails is) and Corporation >
	// Killmails in the sub-row, not Pilot > Killmails.
	_, body = getPage(t, app, cookie, "/corporations/killmails/")
	tabs = sectionTabsBlock(t, body)
	for _, want := range []string{
		`<a href="/corporations/">People</a>`,
		`<a href="/corporations/wallets/">Assets &amp; Money</a>`,
		`<a href="/doctrines/">Programs</a>`,
		`<a href="/corporations/killmails/" class="active" aria-current="page">Comms &amp; Admin</a>`,
	} {
		mustContain(t, "corp killmails group tabs", tabs, want)
	}
	if strings.Count(tabs, `class="active"`) != 1 {
		t.Errorf("corp group tabs mark %d clusters, want 1", strings.Count(tabs, `class="active"`))
	}
	sub := sectionSubTabsBlock(t, body)
	mustContain(t, "corp killmails subtabs", sub, `<a href="/corporations/killmails/" class="active" aria-current="page">Killmails</a>`)
	mustContain(t, "corp killmails subtabs", sub, `<a href="/discord/servers">Discord Server</a>`)
	mustContain(t, "corp killmails subtabs", sub, `<a href="/corporations/settings/">Settings</a>`)
	if strings.Count(sub, `class="active"`) != 1 {
		t.Errorf("corp killmails subtabs mark %d pages, want 1", strings.Count(sub, `class="active"`))
	}
	if strings.Contains(body, `<a href="/killmails/" class="active">`) {
		t.Error("Pilot > Killmails marked active on the corporation killmails page")
	}

	// The corporation overview lights People, with its own pages
	// in the sub-row.
	_, body = getPage(t, app, cookie, "/corporations/")
	tabs = sectionTabsBlock(t, body)
	mustContain(t, "corp overview group tabs", tabs, `<a href="/corporations/" class="active" aria-current="page">People</a>`)
	sub = sectionSubTabsBlock(t, body)
	mustContain(t, "corp overview subtabs", sub, `<a href="/corporations/" class="active" aria-current="page">Overview</a>`)
	mustContain(t, "corp overview subtabs", sub, `<a href="/corporations/members/">Members</a>`)
	mustContain(t, "corp overview subtabs", sub, `<a href="/groups/">Groups</a>`)

	// A page asked for one character passes it to its section's
	// tabs, so switching tabs keeps the pilot; other sections' links
	// stay bare.
	_, body = getPage(t, app, cookie, "/character/?character="+strconv.FormatInt(fixtureCharA, 10))
	tabs = sectionTabsBlock(t, body)
	mustContain(t, "character tabs", tabs, `href="/assets/?character=`+strconv.FormatInt(fixtureCharA, 10)+`"`)
	mustContain(t, "sidebar", body, `<a href="/market/scanner/">Spread Scanner</a>`)

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

// sectionSubTabsBlock returns a page's group sub-row.
func sectionSubTabsBlock(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, `<nav class="section-subtabs"`)
	if start < 0 {
		t.Fatal("no section subtabs on the page")
	}
	end := strings.Index(body[start:], "</nav>")
	return body[start : start+end]
}

// TestSectionGroups pins the corporation group map against the
// registered nav: every corporation page belongs to exactly one
// group, so no page can fall out of the grouped strip (the
// runtime falls back to the flat strip for a page no group
// claims; this test is what keeps that a safety net). It also
// pins the grouped strip's shape: group tabs in map order,
// linking at each group's first page in nav order, with the
// active page's group marked and its pages in the sub-row.
func TestSectionGroups(t *testing.T) {
	claimed := map[string]string{}
	for _, g := range sectionGroups["corporation"] {
		for _, h := range g.Hrefs {
			if other, dup := claimed[h]; dup {
				t.Errorf("%s claimed by both %s and %s", h, other, g.Label)
			}
			claimed[h] = g.Label
		}
	}
	for _, e := range sectionNav["corporation"] {
		if _, ok := claimed[e.Href]; !ok {
			t.Errorf("corporation page %s belongs to no group", e.Href)
		}
	}

	links := navLinksFor("corporation", "corp_wallets.html", 0)
	tabs, sub := sectionTabsGrouped(links, "corporation")
	wantTabs := []navLink{
		{Href: "/corporations/", Label: "People"},
		{Href: "/corporations/wallets/", Label: "Assets & Money", Active: true},
		{Href: "/doctrines/", Label: "Programs"},
		{Href: "/corporations/killmails/", Label: "Comms & Admin"},
	}
	if len(tabs) != len(wantTabs) {
		t.Fatalf("grouped strip has %d tabs, want %d", len(tabs), len(wantTabs))
	}
	for i, want := range wantTabs {
		if tabs[i] != want {
			t.Errorf("group tab %d = %+v, want %+v", i, tabs[i], want)
		}
	}
	wantSub := []string{"/corporations/wallets/", "/corporations/orders/", "/corporations/assets/", "/corporations/structures/"}
	if len(sub) != len(wantSub) {
		t.Fatalf("sub-row has %d pages, want %d", len(sub), len(wantSub))
	}
	for i, href := range wantSub {
		if sub[i].Href != href {
			t.Errorf("sub-row page %d = %s, want %s", i, sub[i].Href, href)
		}
	}
	if !sub[0].Active {
		t.Error("the wallets page is not marked in its sub-row")
	}

	// An ungrouped section passes through flat, with no sub-row.
	flat, flatSub := sectionTabsGrouped(navLinksFor("market", "market.html", 0), "market")
	if flatSub != nil || len(flat) != len(sectionNav["market"]) {
		t.Error("the market strip changed shape under grouping")
	}
}
