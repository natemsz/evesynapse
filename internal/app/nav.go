package app

import (
	"html/template"
	"slices"
	"strconv"
	"strings"

	"evesynapse/internal/app/module"
)

// ---------------------------------------------------------------------------
// The section nav: one list of every category's pages. The sidebar's
// category menus and the tab strip under each page's title both
// render from it, so the two can never disagree.
// ---------------------------------------------------------------------------

// navEntry is one page in a section's nav: its link, its label, and
// the template files that count as being on it (a list page and its
// detail page can share one entry).
type navEntry struct {
	Href  string
	Label string
	Pages []string
}

var (
	registeredModules []module.Module
	registeredPages   = map[string]module.Page{}
)

// registerModules adds modules to the nav and page registries. A
// module already registered (same ID) is skipped, so calling it again
// is harmless.
func registerModules(modules []module.Module) {
	for _, m := range modules {
		if moduleCompiledIn(m.ID) {
			continue
		}
		validateModule(m)
		registeredModules = append(registeredModules, m)
		for _, sec := range m.Nav {
			for _, e := range sec.Entries {
				entry := navEntry{Href: e.Href, Label: e.Label, Pages: e.Pages}
				list := sectionNav[sec.Section]
				at := len(list)
				for i, existing := range list {
					if e.After != "" && existing.Href == e.After {
						at = i + 1
						break
					}
				}
				list = append(list, navEntry{})
				copy(list[at+1:], list[at:])
				list[at] = entry
				sectionNav[sec.Section] = list
			}
		}
		for _, p := range m.Pages {
			registeredPages[p.Template] = p
		}
	}
}

// validateModule stops a module that left a declaration out: ID, Title
// and the three lists are all required, the lists explicit even when
// empty. A module is compiled-in code, so this is a developer error
// caught on the first start or test run.
func validateModule(m module.Module) {
	switch {
	case m.ID == "":
		panic("module registered without an ID")
	case m.Title == "":
		panic("module " + m.ID + " declares no Title")
	case m.Requires == nil, m.Scopes == nil, m.EnhanceScopes == nil:
		panic("module " + m.ID + " must declare Requires, Scopes and EnhanceScopes (empty lists are written out, not omitted)")
	}
}

// moduleCompiledIn reports whether a module with this ID is compiled in
// and registered.
func moduleCompiledIn(id string) bool {
	for _, m := range registeredModules {
		if m.ID == id {
			return true
		}
	}
	return false
}

// sectionNav is every nav category's pages, in menu order. The keys
// are the section keys sectionForPage returns.
var sectionNav = map[string][]navEntry{
	"pilot": {
		{"/character/", "Overview", []string{"character.html"}},
		{"/characters/", "Characters", []string{"characters.html"}},
		{"/assets/", "Assets", []string{"assets.html"}},
		{"/killmails/", "Killmails", []string{"killmails.html"}},
	},
	"fitting": {},
	"industry": {
		{"/industry/", "Blueprints", []string{"industry.html"}},
		{"/industry/jobs/", "My Jobs", []string{"industry_jobs.html"}},
		{"/industry/mining/", "Mining Ledger", []string{"industry_mining.html"}},
		{"/planets/", "My Colonies", []string{"planets.html"}},
	},
	"market": {
		{"/market/", "Market", []string{"market.html"}},
		{"/market/trends/", "Trends", []string{"market_trends.html"}},
		{"/market/lp/", "LP Store", []string{"market_lp.html"}},
		{"/market/baskets/", "Baskets", []string{"market_baskets.html", "market_basket.html"}},
		{"/profit/", "Profit Analysis", []string{"profit.html", "profit_blueprint.html"}},
	},
	"corporation": {
		{"/corporations/", "Overview", []string{"corporations.html"}},
		{"/corporations/killmails/", "Killmails", []string{"killmails.html"}},
		{"/corporations/settings/", "Settings", []string{"corp_settings.html"}},
	},
	"tools": {
		{"/items/", "Item Browser", []string{"items.html"}},
	},
	"map": {
		{"/map/", "Map", []string{"map.html"}},
	},
}

// navLink is one rendered nav link: the sidebar menu item and the
// section tab are the same thing.
type navLink = module.NavLink

// navLinksFor renders every section's links for one page. A link is
// active when the page is one of its pages and the page belongs to
// the link's section: the killmails page under Corporation lights
// Corporation > Killmails, not Pilot > Killmails. A page asked for one
// character (?character=) passes it on to its own section's links, so
// moving between a pilot's pages, or a corporation's, keeps it.
func navLinksFor(section, page string, character int64) map[string][]navLink {
	out := make(map[string][]navLink, len(sectionNav))
	for sec, entries := range sectionNav {
		links := make([]navLink, len(entries))
		for i, e := range entries {
			href := e.Href
			if sec == section && character > 0 && !strings.Contains(href, "?") {
				href += "?character=" + strconv.FormatInt(character, 10)
			}
			links[i] = navLink{Href: href, Label: e.Label, Active: sec == section && slices.Contains(e.Pages, page)}
		}
		out[sec] = links
	}
	return out
}

// sectionTabsFor is the tab strip under a page's title: its
// section's links, shown on the section's own pages only (a detail
// page reached from elsewhere, like a public pilot or a station,
// keeps its plain header) and only where there is a choice to make.
func sectionTabsFor(links map[string][]navLink, section string) []navLink {
	tabs := links[section]
	if len(tabs) < 2 {
		return nil
	}
	for _, t := range tabs {
		if t.Active {
			return tabs
		}
	}
	return nil
}

// navGroup is one cluster of a grouped section's pages. Membership
// and order are both by href: the pages inside a group render in
// the order the group lists them, so the map is the one place a
// cluster's shape is decided, not module registration order.
type navGroup struct {
	Label string
	Hrefs []string
}

// orderGroup orders one group's links by the group's declared
// href order. A link whose href the group does not declare (the
// coverage pins say there are none) keeps its place after the
// declared ones, in nav order.
func orderGroup(links []navLink, hrefs []string) []navLink {
	rank := make(map[string]int, len(hrefs))
	for i, h := range hrefs {
		rank[h] = i
	}
	out := slices.Clone(links)
	slices.SortStableFunc(out, func(a, b navLink) int {
		ra, ok := rank[baseHref(a.Href)]
		if !ok {
			ra = len(hrefs)
		}
		rb, ok := rank[baseHref(b.Href)]
		if !ok {
			rb = len(hrefs)
		}
		return ra - rb
	})
	return out
}

// sectionGroups clusters a section's pages for the tab strip
// and the sidebar menu alike: the two render the same groups, so
// a page sits under the same heading in both. Corporation set
// the pattern: at thirteen pages its strip was the widest in the
// app by a third again, and no single row of folder tabs holds
// it under a very wide window. The other wide sections follow
// it: pilot, industry and market group the same way, each map
// claiming every one of the section's pages exactly once
// (TestSectionGroups pins that, so the runtime's flat fallback
// stays a safety net). The short sections stay flat: fitting
// and intel carry three pages apiece and tools two, their
// strips are a single short row and their pages form no
// clusters worth a heading. No page's route changes.
var sectionGroups = map[string][]navGroup{
	"pilot": {
		{"Character", []string{"/character/", "/characters/", "/skills/plans", "/killmails/"}},
		{"Assets & Money", []string{"/assets/", "/wallet/"}},
		{"Comms", []string{"/mail/", "/calendar/", "/contacts/"}},
	},
	"industry": {
		{"Production", []string{"/industry/", "/industry/jobs/", "/industry/mining/", "/planner/"}},
		{"Planetary", []string{"/planets/", "/industry/pi/"}},
	},
	"market": {
		{"Browse", []string{"/market/", "/contracts/search/", "/market/lp/"}},
		{"Tools & Analysis", []string{"/market/trends/", "/market/baskets/", "/market/ideas/", "/market/scanner/", "/market/tradefinder/", "/market/restock/", "/market/leaderboard/"}},
		{"My Finances", []string{"/orders/", "/contracts/", "/profit/"}},
	},
	"intel": {
		{"Killboard", []string{"/intel/kills/", "/intel/battles/"}},
		{"Live intel", []string{"/intel/feed/", "/intel/channels/", "/intel/workbench/", "/intel/scan/"}},
		{"Territory", []string{"/intel/sov/", "/intel/fw/", "/intel/wars/", "/intel/incursions/"}},
	},
	"corporation": {
		{"People", []string{"/corporations/", "/corporations/members/", "/corporations/killmails/", "/groups/"}},
		{"Assets & Money", []string{"/corporations/wallets/", "/corporations/orders/", "/corporations/contracts/", "/corporations/assets/", "/corporations/structures/"}},
		{"Programs", []string{"/doctrines/", "/doctrines/fits/", "/srp/"}},
		{"Comms & Admin", []string{"/discord/servers", "/corporations/settings/"}},
	},
}

// navGroupIcons is the small glyph each group heading carries in
// the sidebar menu, keyed by group label: a 24-unit stroke glyph
// drawn in the heading's own color at heading size, so a cluster
// reads at a glance the way a section does by its summary icon.
// A label with no entry renders its heading plain.
var navGroupIcons = map[string]template.HTML{
	"Character":        `<svg width="12" height="12" viewBox="0 0 24 24" aria-hidden="true"><g fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round"><circle cx="12" cy="8" r="4"/><path d="M4.8 20.2c1.1-3.2 3.9-4.9 7.2-4.9s6.1 1.7 7.2 4.9"/></g></svg>`,
	"Assets & Money":   `<svg width="12" height="12" viewBox="0 0 24 24" aria-hidden="true"><g fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round"><ellipse cx="12" cy="6.5" rx="7" ry="3"/><path d="M5 6.5v11c0 1.7 3.1 3 7 3s7-1.3 7-3v-11"/><path d="M5 12c0 1.7 3.1 3 7 3s7-1.3 7-3"/></g></svg>`,
	"Comms":            `<svg width="12" height="12" viewBox="0 0 24 24" aria-hidden="true"><g fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="5.5" width="18" height="13" rx="2"/><path d="m4 7.5 8 6 8-6"/></g></svg>`,
	"Production":       `<svg width="12" height="12" viewBox="0 0 24 24" aria-hidden="true"><g fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"><path d="M3.5 21h17"/><path d="M5.5 21v-9.5l5.5 3.7v-3.7l5.5 3.7V6h2v15"/></g></svg>`,
	"Planetary":        `<svg width="12" height="12" viewBox="0 0 24 24" aria-hidden="true"><g fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round"><circle cx="12" cy="12" r="4.5"/><path d="M3 16.8c5.8 2.8 12.2 2.8 18 0"/><path d="M3.8 8.6c1.9-.9 4-1.5 6.2-1.7"/></g></svg>`,
	"Browse":           `<svg width="12" height="12" viewBox="0 0 24 24" aria-hidden="true"><g fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round"><circle cx="10.5" cy="10.5" r="6.5"/><path d="M15.5 15.5 21 21"/></g></svg>`,
	"Tools & Analysis": `<svg width="12" height="12" viewBox="0 0 24 24" aria-hidden="true"><g fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"><path d="M4 4v15.5h16"/><path d="M7.5 15.5 11 10l3 2.5 4.5-5"/></g></svg>`,
	"My Finances":      `<svg width="12" height="12" viewBox="0 0 24 24" aria-hidden="true"><g fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"><rect x="3.5" y="6.5" width="17" height="12" rx="2"/><path d="M3.5 10.5h17"/></g><circle cx="16.8" cy="14.5" r="1.4" fill="currentColor"/></svg>`,
	"People":           `<svg width="12" height="12" viewBox="0 0 24 24" aria-hidden="true"><g fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round"><circle cx="9" cy="9" r="3.4"/><path d="M3.2 19.6c.7-2.9 2.9-4.6 5.8-4.6 1.5 0 2.8.5 3.8 1.3"/><path d="M15.5 6.4a3.4 3.4 0 1 1 .9 4.5"/><path d="M16.8 15.3c2 .4 3.4 1.9 4 4.3"/></g></svg>`,
	"Programs":         `<svg width="12" height="12" viewBox="0 0 24 24" aria-hidden="true"><g fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 3.5 19 6v5.2c0 4.4-2.9 7.5-7 9.3-4.1-1.8-7-4.9-7-9.3V6z"/></g></svg>`,
	"Comms & Admin":    `<svg width="12" height="12" viewBox="0 0 24 24" aria-hidden="true"><g fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round"><path d="M4 7h9"/><path d="M17 7h3"/><path d="M4 12h3"/><path d="M11 12h9"/><path d="M4 17h11"/><path d="M19 17h1"/></g><g fill="none" stroke="currentColor" stroke-width="2.2"><circle cx="15" cy="7" r="2"/><circle cx="9" cy="12" r="2"/><circle cx="17" cy="17" r="2"/></g></svg>`,
}

// sectionTabsGrouped is a page's tab strip data: for an ungrouped
// section, its pages and no sub-row. For a grouped one, the strip
// shows the groups, labelled by cluster, linking at the cluster's
// first page in the group's declared order, active when one of its pages is, and
// the sub-row shows the active group's pages. A group with no
// compiled-in pages is skipped. A strip the group map does not
// cover exactly falls back to the flat strip whole, so a page no
// group claims can never vanish from the nav; TestSectionGroups
// pins the map against the registry so the fallback stays a
// safety net, not a state anyone ships in.
func sectionTabsGrouped(links map[string][]navLink, section string) ([]navLink, []navLink) {
	tabs := sectionTabsFor(links, section)
	groups, grouped := sectionGroups[section]
	if !grouped || tabs == nil {
		return tabs, nil
	}
	member := make(map[string]int, len(tabs))
	for gi, g := range groups {
		for _, h := range g.Hrefs {
			member[h] = gi
		}
	}
	buckets := make([][]navLink, len(groups))
	for _, t := range tabs {
		gi, ok := member[baseHref(t.Href)]
		if !ok {
			return tabs, nil
		}
		buckets[gi] = append(buckets[gi], t)
	}
	var groupTabs, sub []navLink
	for gi, g := range groups {
		pages := orderGroup(buckets[gi], g.Hrefs)
		if len(pages) == 0 {
			continue
		}
		gt := navLink{Href: pages[0].Href, Label: g.Label}
		for _, p := range pages {
			if p.Active {
				gt.Active = true
				sub = pages
			}
		}
		groupTabs = append(groupTabs, gt)
	}
	if len(groupTabs) < 2 || sub == nil {
		return tabs, nil
	}
	return groupTabs, sub
}

// navGroupsFor is the sidebar's view of sectionGroups: each
// grouped section's flat links bucketed under their group
// headings, in map order, pages inside a group in the group's
// declared order,
// the tab strip's grouping mirrored, not a second opinion of
// it. A section the map does not cover exactly (a page no
// group claims, a group with no compiled-in pages) is left
// out, and its menu renders the flat list whole, the same
// safety net sectionTabsGrouped keeps for the strip.
func navGroupsFor(links map[string][]navLink) map[string][]module.NavGroup {
	out := make(map[string][]module.NavGroup, len(sectionGroups))
	for section, groups := range sectionGroups {
		flat := links[section]
		if len(flat) == 0 {
			continue
		}
		member := make(map[string]int, len(flat))
		for gi, g := range groups {
			for _, h := range g.Hrefs {
				member[h] = gi
			}
		}
		buckets := make([][]navLink, len(groups))
		claimed := 0
		for _, l := range flat {
			gi, ok := member[baseHref(l.Href)]
			if !ok {
				claimed = -1
				break
			}
			buckets[gi] = append(buckets[gi], l)
			claimed++
		}
		if claimed != len(flat) {
			continue
		}
		var menu []module.NavGroup
		for gi, g := range groups {
			if len(buckets[gi]) == 0 {
				continue
			}
			menu = append(menu, module.NavGroup{Label: g.Label, Icon: navGroupIcons[g.Label], Links: orderGroup(buckets[gi], g.Hrefs)})
		}
		if len(menu) < 2 {
			continue
		}
		out[section] = menu
	}
	return out
}

// baseHref is a rendered link's path, without the ?character= a
// section's own links carry.
func baseHref(href string) string {
	if at := strings.Index(href, "?"); at >= 0 {
		return href[:at]
	}
	return href
}

// ---------------------------------------------------------------------------
// The phone bottom bar and its menu: the primary nav restated
// for the phone band. The bar's fixed slots and the menu's
// section order are the two lists below, one line each
// to change, and every destination derives from the same
// sectionNav the sidebar renders, so the three can never
// disagree. A section's landing is its first nav page.
// ---------------------------------------------------------------------------

// bottomBarSlots is the bottom bar's fixed section slots, in bar
// order, between Home and the Sections button.
var bottomBarSlots = []struct{ section, label string }{
	{"pilot", "Pilots"},
	{"industry", "Industry"},
	{"market", "Market"},
}

// bottomSheetSections is the sections sheet's rows, in sheet
// order: every primary section.
var bottomSheetSections = []struct{ section, label string }{
	{"map", "Map"},
	{"pilot", "Pilot"},
	{"fitting", "Fitting"},
	{"industry", "Industry"},
	{"market", "Market"},
	{"corporation", "Corporation"},
	{"intel", "Intel"},
	{"tools", "Tools"},
}

// sectionLanding is a section's landing href: its first nav
// page's path. Empty when the section has no pages (a module
// that is not compiled in).
func sectionLanding(links map[string][]navLink, section string) string {
	ls := links[section]
	if len(ls) == 0 {
		return ""
	}
	return baseHref(ls[0].Href)
}

// sectionSummary is a section's one-line contents for the phone
// menu's section row: a grouped section's group labels, else its
// first pages' labels, so a reader sees what a row opens before
// opening it. A one-page section has nothing to summarize: its
// row goes straight to the page.
func sectionSummary(groups map[string][]module.NavGroup, links map[string][]navLink, section string) string {
	var parts []string
	if gs := groups[section]; len(gs) > 0 {
		for _, g := range gs {
			parts = append(parts, g.Label)
		}
	} else if len(links[section]) > 1 {
		for i, l := range links[section] {
			if i == 3 {
				break
			}
			parts = append(parts, l.Label)
		}
	}
	return strings.Join(parts, ", ")
}

// bottomNavFor builds the phone bar's slots and the menu's
// section rows for one page. The bar marks the current section
// when it has a slot; when the current section lives only in the
// menu, the Menu button carries the mark instead, the same
// reflection the tab strip's More button gives a folded active
// tab. The menu marks its section; each section's own screen in
// the menu renders from the NavGroups and NavLinks the sidebar
// menus use.
func bottomNavFor(links map[string][]navLink, groups map[string][]module.NavGroup, section string) ([]module.BottomNavItem, []module.BottomSection) {
	bar := []module.BottomNavItem{
		{Label: "Home", Href: "/", Section: "home", Active: section == "home"},
	}
	barSections := map[string]bool{"home": true}
	for _, slot := range bottomBarSlots {
		landing := sectionLanding(links, slot.section)
		if landing == "" {
			continue
		}
		bar = append(bar, module.BottomNavItem{
			Label: slot.label, Href: landing, Section: slot.section,
			Active: section == slot.section,
		})
		barSections[slot.section] = true
	}
	var sheet []module.BottomSection
	sheetSections := map[string]bool{}
	for _, row := range bottomSheetSections {
		landing := sectionLanding(links, row.section)
		if landing == "" {
			continue
		}
		sheetSections[row.section] = true
		sheet = append(sheet, module.BottomSection{
			Label: row.label, Href: landing, Section: row.section,
			Active:  section == row.section,
			Summary: sectionSummary(groups, links, row.section),
			Direct:  len(links[row.section]) == 1,
		})
	}
	sectionsActive := section != "" && !barSections[section] && sheetSections[section]
	bar = append(bar, module.BottomNavItem{Label: "Menu", Section: "sections", Sheet: true, Active: sectionsActive})
	return bar, sheet
}

// sheetSwitcherCells is the phone sheet's character grid size:
// two columns of three. The grid is the sheet's one unbounded
// input, an account can link any number of pilots, so past
// the cap the grid's last cell is a "+N more" tile to the
// character management page, and the sheet's account block is
// one bounded height however many pilots the account carries.
const sheetSwitcherCells = 6

// sheetSwitcherCap caps the sheet's switcher entries to the
// grid: every entry when they fit, else the entries that fill
// all but the last cell, with the count left for the tile.
func sheetSwitcherCap(entries []switcherEntry) ([]switcherEntry, int) {
	if len(entries) <= sheetSwitcherCells {
		return entries, 0
	}
	shown := sheetSwitcherCells - 1
	return entries[:shown], len(entries) - shown
}
