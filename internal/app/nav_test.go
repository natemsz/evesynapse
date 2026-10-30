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
	// The market strip is grouped: the strip shows the
	// clusters, Trade Ideas' own cluster marked, and the sub-row
	// carries the cluster's pages with Trade Ideas itself marked.
	mustContain(t, "market group tabs", tabs, `<a href="/market/scanner/" class="active" aria-current="page">Find Trades</a>`)
	mustContain(t, "market group tabs", tabs, `<a href="/orders/">My Finances</a>`)
	if strings.Count(tabs, `class="active"`) != 1 {
		t.Errorf("market group tabs mark %d clusters, want 1", strings.Count(tabs, `class="active"`))
	}
	sub := sectionSubTabsBlock(t, body)
	for _, href := range []string{"/market/scanner/", "/market/ideas/", "/market/tradefinder/"} {
		if !strings.Contains(sub, `href="`+href+`"`) {
			t.Errorf("market subtabs miss %s", href)
		}
	}
	mustContain(t, "market subtabs", sub, `<a href="/market/ideas/" class="active" aria-current="page">Trade Ideas</a>`)
	if strings.Count(sub, `class="active"`) != 1 {
		t.Errorf("market subtabs mark %d pages, want 1", strings.Count(sub, `class="active"`))
	}
	// The sidebar menu renders the same groups: the headings
	// in order, the restock page under its group, My Orders under
	// the last.
	mustContain(t, "sidebar", body, `<a href="/market/restock/">Restock Planner</a>`)
	mat := strings.Index(body, `data-nav-category="market"`)
	if mat < 0 {
		t.Fatal("no market branch in the sidebar")
	}
	mmenu := body[mat:]
	if end := strings.Index(mmenu, "</details>"); end >= 0 {
		mmenu = mmenu[:end]
	}
	toolsAt := strings.Index(mmenu, `<p class="nav-group-label">Browse</p>`)
	finAt := strings.Index(mmenu, `<p class="nav-group-label">My Finances</p>`)
	restockAt := strings.Index(mmenu, `href="/market/restock/"`)
	ordersAt := strings.Index(mmenu, `href="/orders/"`)
	if toolsAt < 0 || finAt < 0 || restockAt < 0 || ordersAt < 0 {
		t.Fatalf("sidebar market menu landmarks missing: tools %d, finances %d, restock %d, orders %d", toolsAt, finAt, restockAt, ordersAt)
	}
	if !(toolsAt < restockAt && restockAt < finAt && finAt < ordersAt) {
		t.Error("the sidebar market menu does not render its groups in map order with their pages under them")
	}

	// The corporation strip is grouped: the strip shows the four
	// clusters, and the page itself is marked in the sub-row under
	// its cluster. The corporation killmails page lights People
	// (Killmails is a People page) and Corporation > Killmails
	// in the sub-row, not Pilot > Killmails.
	_, body = getPage(t, app, cookie, "/corporations/killmails/")
	tabs = sectionTabsBlock(t, body)
	for _, want := range []string{
		`<a href="/corporations/" class="active" aria-current="page">People</a>`,
		`<a href="/corporations/wallets/">Assets &amp; Money</a>`,
		`<a href="/doctrines/">Programs</a>`,
		`<a href="/discord/servers">Comms &amp; Admin</a>`,
	} {
		mustContain(t, "corp killmails group tabs", tabs, want)
	}
	if strings.Count(tabs, `class="active"`) != 1 {
		t.Errorf("corp group tabs mark %d clusters, want 1", strings.Count(tabs, `class="active"`))
	}
	sub = sectionSubTabsBlock(t, body)
	mustContain(t, "corp killmails subtabs", sub, `<a href="/corporations/killmails/" class="active" aria-current="page">Killmails</a>`)
	mustContain(t, "corp killmails subtabs", sub, `<a href="/corporations/">Overview</a>`)
	mustContain(t, "corp killmails subtabs", sub, `<a href="/corporations/members/">Members</a>`)
	mustContain(t, "corp killmails subtabs", sub, `<a href="/groups/">Groups</a>`)
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
	mustContain(t, "corp overview subtabs", sub, `<a href="/corporations/killmails/">Killmails</a>`)

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

// TestSectionGroups pins every group map against the registered
// nav: every page of a grouped section belongs to exactly one
// group, so no page can fall out of the grouped strip (the
// runtime falls back to the flat strip for a page no group
// claims; this test is what keeps that a safety net). It also
// pins the grouped strip's shape: group tabs in map order,
// linking at each group's first page in nav order, with the
// active page's group marked and its pages in the sub-row.
func TestSectionGroups(t *testing.T) {
	for section, groups := range sectionGroups {
		claimed := map[string]string{}
		for _, g := range groups {
			for _, h := range g.Hrefs {
				if other, dup := claimed[h]; dup {
					t.Errorf("%s page %s claimed by both %s and %s", section, h, other, g.Label)
				}
				claimed[h] = g.Label
			}
		}
		for _, e := range sectionNav[section] {
			if _, ok := claimed[e.Href]; !ok {
				t.Errorf("%s page %s belongs to no group", section, e.Href)
			}
		}
	}

	links := navLinksFor("corporation", "corp_wallets.html", 0)
	tabs, sub := sectionTabsGrouped(links, "corporation")
	wantTabs := []navLink{
		{Href: "/corporations/", Label: "People"},
		{Href: "/corporations/wallets/", Label: "Assets & Money", Active: true},
		{Href: "/doctrines/", Label: "Programs"},
		{Href: "/discord/servers", Label: "Comms & Admin"},
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

	// The market strip groups by job: Browse first, linking at
	// the market page itself, then Find Trades, Baskets &
	// Restock, LP & Contracts, and My Finances at My Orders;
	// the sub-row follows the viewed page's cluster.
	mtabs, msub := sectionTabsGrouped(navLinksFor("market", "orders.html", 0), "market")
	wantMTabs := []navLink{
		{Href: "/market/", Label: "Browse"},
		{Href: "/market/scanner/", Label: "Find Trades"},
		{Href: "/market/restock/", Label: "Baskets & Restock"},
		{Href: "/contracts/search/", Label: "LP & Contracts"},
		{Href: "/orders/", Label: "My Finances", Active: true},
	}
	if len(mtabs) != len(wantMTabs) {
		t.Fatalf("market strip has %d group tabs, want %d", len(mtabs), len(wantMTabs))
	}
	for i, want := range wantMTabs {
		if mtabs[i] != want {
			t.Errorf("market group tab %d = %+v, want %+v", i, mtabs[i], want)
		}
	}
	if len(msub) != 2 || msub[0].Href != "/orders/" || msub[1].Href != "/contracts/" || !msub[0].Active {
		t.Errorf("market sub-row = %+v, want My Orders (active) and My Contracts", msub)
	}
	_, msub = sectionTabsGrouped(navLinksFor("market", "market.html", 0), "market")
	wantTools := []string{"/market/", "/market/leaderboard/", "/market/trends/"}
	if len(msub) != len(wantTools) {
		t.Fatalf("market browse sub-row has %d pages, want %d", len(msub), len(wantTools))
	}
	for i, href := range wantTools {
		if msub[i].Href != href {
			t.Errorf("market browse sub-row page %d = %s, want %s", i, msub[i].Href, href)
		}
	}

	// The pilot strip: three clusters, the wallet in Assets &
	// Money's sub-row.
	ptabs, psub := sectionTabsGrouped(navLinksFor("pilot", "wallet.html", 0), "pilot")
	wantPTabs := []navLink{
		{Href: "/character/", Label: "Character"},
		{Href: "/assets/", Label: "Assets & Money", Active: true},
		{Href: "/mail/", Label: "Comms"},
	}
	if len(ptabs) != len(wantPTabs) {
		t.Fatalf("pilot strip has %d group tabs, want %d", len(ptabs), len(wantPTabs))
	}
	for i, want := range wantPTabs {
		if ptabs[i] != want {
			t.Errorf("pilot group tab %d = %+v, want %+v", i, ptabs[i], want)
		}
	}
	if len(psub) != 3 || psub[0].Href != "/assets/" || psub[1].Href != "/wallet/" || psub[2].Href != "/profit/" || !psub[1].Active {
		t.Errorf("pilot sub-row = %+v, want Assets, the active Wallet and Profit", psub)
	}

	// The industry strip: Production and Planetary, the
	// colonies page in Planetary's sub-row.
	itabs, isub := sectionTabsGrouped(navLinksFor("industry", "planets.html", 0), "industry")
	wantITabs := []navLink{
		{Href: "/industry/", Label: "Production"},
		{Href: "/planets/", Label: "Planetary", Active: true},
	}
	if len(itabs) != len(wantITabs) {
		t.Fatalf("industry strip has %d group tabs, want %d", len(itabs), len(wantITabs))
	}
	for i, want := range wantITabs {
		if itabs[i] != want {
			t.Errorf("industry group tab %d = %+v, want %+v", i, itabs[i], want)
		}
	}
	if len(isub) != 2 || isub[0].Href != "/planets/" || isub[1].Href != "/industry/pi/" || !isub[0].Active {
		t.Errorf("industry sub-row = %+v, want My Colonies (active) and the PI Planner", isub)
	}

	// An ungrouped section passes through flat, with no sub-row.
	flat, flatSub := sectionTabsGrouped(navLinksFor("tools", "items.html", 0), "tools")
	if flatSub != nil || len(flat) != len(sectionNav["tools"]) {
		t.Error("the tools strip changed shape under grouping")
	}
}

// TestNavGroupsMirror pins the sidebar's grouped menus against
// the same map the tab strip renders from: each grouped
// section's menu carries its group headings in map order, each
// group's pages in nav order, and every page appears under
// exactly one heading — a grouping that exists in the strip but
// not the menu (or the other way round) fails here. Ungrouped
// sections get no menu groups at all.
func TestNavGroupsMirror(t *testing.T) {
	links := navLinksFor("corporation", "corp_fits.html", 0)
	groups := navGroupsFor(links)["corporation"]
	want := []struct {
		label string
		hrefs []string
	}{
		{"People", []string{"/corporations/", "/corporations/members/", "/corporations/killmails/", "/groups/"}},
		{"Assets & Money", []string{"/corporations/wallets/", "/corporations/orders/", "/corporations/assets/", "/corporations/structures/"}},
		{"Programs", []string{"/doctrines/", "/doctrines/fits/", "/srp/"}},
		{"Comms & Admin", []string{"/discord/servers", "/corporations/settings/"}},
	}
	if len(groups) != len(want) {
		t.Fatalf("sidebar has %d corporation groups, want %d", len(groups), len(want))
	}
	seen := map[string]int{}
	for gi, w := range want {
		g := groups[gi]
		if g.Label != w.label {
			t.Errorf("sidebar group %d = %q, want %q", gi, g.Label, w.label)
		}
		if len(g.Links) != len(w.hrefs) {
			t.Fatalf("sidebar group %q has %d pages, want %d", w.label, len(g.Links), len(w.hrefs))
		}
		for li, href := range w.hrefs {
			if g.Links[li].Href != href {
				t.Errorf("sidebar group %q page %d = %s, want %s", w.label, li, g.Links[li].Href, href)
			}
			seen[href]++
		}
	}
	for _, e := range sectionNav["corporation"] {
		if seen[e.Href] != 1 {
			t.Errorf("corporation page %s appears %d times in the sidebar groups, want 1", e.Href, seen[e.Href])
		}
	}
	// The viewed page is marked inside its group.
	programs := groups[2]
	if !programs.Links[1].Active {
		t.Error("the loadouts page is not marked in the sidebar's Programs group")
	}

	// The sections grouped after corporation carry the same
	// pinning: headings in map order, each group's pages in nav
	// order, every page under exactly one heading, and the
	// viewed page marked inside its group.
	for _, tc := range []struct {
		section string
		page    string
		want    []struct {
			label string
			hrefs []string
		}
		activeGroup, activeLink int
	}{
		{"pilot", "wallet.html", []struct {
			label string
			hrefs []string
		}{
			{"Character", []string{"/character/", "/characters/", "/skills/plans", "/killmails/"}},
			{"Assets & Money", []string{"/assets/", "/wallet/", "/profit/"}},
			{"Comms", []string{"/mail/", "/calendar/", "/contacts/"}},
		}, 1, 1},
		{"industry", "planets.html", []struct {
			label string
			hrefs []string
		}{
			{"Production", []string{"/industry/", "/industry/jobs/", "/industry/mining/", "/planner/"}},
			{"Planetary", []string{"/planets/", "/industry/pi/"}},
		}, 1, 0},
		{"market", "orders.html", []struct {
			label string
			hrefs []string
		}{
			{"Browse", []string{"/market/", "/market/leaderboard/", "/market/trends/"}},
			{"Find Trades", []string{"/market/scanner/", "/market/ideas/", "/market/tradefinder/"}},
			{"Baskets & Restock", []string{"/market/restock/", "/market/baskets/"}},
			{"LP & Contracts", []string{"/contracts/search/", "/market/lp/"}},
			{"My Finances", []string{"/orders/", "/contracts/"}},
		}, 4, 0},
	} {
		sgs := navGroupsFor(navLinksFor(tc.section, tc.page, 0))[tc.section]
		if len(sgs) != len(tc.want) {
			t.Fatalf("sidebar has %d %s groups, want %d", len(sgs), tc.section, len(tc.want))
		}
		seen := map[string]int{}
		for gi, w := range tc.want {
			g := sgs[gi]
			if g.Label != w.label {
				t.Errorf("sidebar %s group %d = %q, want %q", tc.section, gi, g.Label, w.label)
			}
			if len(g.Links) != len(w.hrefs) {
				t.Fatalf("sidebar %s group %q has %d pages, want %d", tc.section, w.label, len(g.Links), len(w.hrefs))
			}
			for li, href := range w.hrefs {
				if g.Links[li].Href != href {
					t.Errorf("sidebar %s group %q page %d = %s, want %s", tc.section, w.label, li, g.Links[li].Href, href)
				}
				seen[href]++
			}
		}
		for _, e := range sectionNav[tc.section] {
			if seen[e.Href] != 1 {
				t.Errorf("%s page %s appears %d times in the sidebar groups, want 1", tc.section, e.Href, seen[e.Href])
			}
		}
		if !sgs[tc.activeGroup].Links[tc.activeLink].Active {
			t.Errorf("the %s page is not marked in its sidebar group", tc.section)
		}
	}

	// No group map, no grouped menu: the short sections stay
	// flat.
	for _, section := range []string{"fitting", "intel", "tools"} {
		if _, ok := navGroupsFor(navLinksFor(section, "", 0))[section]; ok {
			t.Errorf("the %s menu gained groups; only mapped sections group", section)
		}
	}

	// The rendered sidebar carries the headings: the corporation
	// menu on a corporation page shows the four labels, in
	// order, with Groups up in People (not down in the flat
	// list's tail where the registry order puts it).
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	user, err := q.CreateUser(context.Background())
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")
	_, body := getPage(t, app, cookie, "/corporations/killmails/")
	at := strings.Index(body, `data-nav-category="corporation"`)
	if at < 0 {
		t.Fatal("no corporation branch in the sidebar")
	}
	menu := body[at:]
	if end := strings.Index(menu, "</details>"); end >= 0 {
		menu = menu[:end]
	}
	last := -1
	for _, label := range []string{"People", "Assets &amp; Money", "Programs", "Comms &amp; Admin"} {
		i := strings.Index(menu, `<p class="nav-group-label">`+label+`</p>`)
		if i < 0 {
			t.Errorf("sidebar corporation menu misses the %s heading", label)
			continue
		}
		if i < last {
			t.Errorf("sidebar group heading %s is out of order", label)
		}
		last = i
	}
	peopleAt := strings.Index(menu, `<p class="nav-group-label">People</p>`)
	assetsAt := strings.Index(menu, `<p class="nav-group-label">Assets &amp; Money</p>`)
	groupsAt := strings.Index(menu, `href="/groups/"`)
	if peopleAt < 0 || assetsAt < 0 || groupsAt < 0 {
		t.Fatalf("sidebar menu landmarks missing: people %d, assets %d, groups link %d", peopleAt, assetsAt, groupsAt)
	}
	if !(peopleAt < groupsAt && groupsAt < assetsAt) {
		t.Error("the Groups page does not render inside the sidebar's People group")
	}
}
