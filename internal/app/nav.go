package app

import (
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
	},
	"corporation": {
		{"/corporations/", "Overview", []string{"corporations.html"}},
		{"/corporations/killmails/", "Killmails", []string{"killmails.html"}},
		{"/corporations/settings/", "Settings", []string{"corp_settings.html"}},
	},
	"tools": {
		{"/items/", "Item Browser", []string{"items.html"}},
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
// is by href; the order of pages inside a group is the section's
// nav order, so module registration still decides it.
type navGroup struct {
	Label string
	Hrefs []string
}

// sectionGroups clusters a section's pages for the tab strip
// and the sidebar menu alike: the two render the same groups, so
// a page sits under the same heading in both. Only corporation
// is grouped: at thirteen pages its strip was the widest in the
// app by a third again, and no single row of folder tabs holds
// it under a very wide window. No page's route changes.
var sectionGroups = map[string][]navGroup{
	"corporation": {
		{"People", []string{"/corporations/", "/corporations/members/", "/groups/", "/corporations/killmails/"}},
		{"Assets & Money", []string{"/corporations/wallets/", "/corporations/orders/", "/corporations/assets/", "/corporations/structures/"}},
		{"Programs", []string{"/doctrines/", "/doctrines/fits/", "/srp/"}},
		{"Comms & Admin", []string{"/discord/servers", "/corporations/settings/"}},
	},
}

// sectionTabsGrouped is a page's tab strip data: for an ungrouped
// section, its pages and no sub-row. For a grouped one, the strip
// shows the groups — labelled by cluster, linking at the cluster's
// first page in nav order, active when one of its pages is — and
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
		pages := buckets[gi]
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
// headings, in map order, pages inside a group in nav order —
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
			menu = append(menu, module.NavGroup{Label: g.Label, Links: buckets[gi]})
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
