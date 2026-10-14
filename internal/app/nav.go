package app

import (
	"slices"
	"strconv"
	"strings"
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

// sectionNav is every nav category's pages, in menu order. The keys
// are the section keys sectionForPage returns.
var sectionNav = map[string][]navEntry{
	"pilot": {
		{"/character/", "Overview", []string{"character.html"}},
		{"/characters/", "Characters", []string{"characters.html"}},
		{"/skills/plans", "Skill plans", []string{"skillplans.html"}},
		{"/assets/", "Assets", []string{"assets.html"}},
		{"/wallet/", "Wallet", []string{"wallet.html"}},
		{"/mail/", "Mail", []string{"mail.html"}},
		{"/calendar/", "Calendar", []string{"calendar.html"}},
		{"/contacts/", "Contacts", []string{"contacts.html"}},
		{"/killmails/", "Killmails", []string{"killmails.html"}},
	},
	"fitting": {
		{"/fittings/", "Ship Workbench", []string{"fittings.html"}},
		{"/fittings/saved/", "My Loadouts", []string{"fittings_saved.html"}},
		{"/fittings/public/", "Public Loadouts Browser", []string{"fittings_public.html"}},
	},
	"industry": {
		{"/industry/", "Industry", []string{"industry.html"}},
		{"/planner/", "Build Planner", []string{"planner.html"}},
		{"/planets/", "Planetary Industry", []string{"planets.html"}},
		{"/industry/pi/", "PI Planner", []string{"piplanner.html"}},
	},
	"market": {
		{"/market/", "Market", []string{"market.html"}},
		{"/market/scanner/", "Spread scanner", []string{"market_scanner.html"}},
		{"/market/ideas/", "Trade ideas", []string{"market_tradeideas.html"}},
		{"/market/tradefinder/", "Tradefinder", []string{"market_tradefinder.html"}},
		{"/market/leaderboard/", "Top stations", []string{"market_leaderboard.html"}},
		{"/market/restock/", "Restock planner", []string{"market_restock.html"}},
		{"/orders/", "Orders", []string{"orders.html"}},
		{"/contracts/", "Contracts", []string{"contracts.html"}},
	},
	"corporation": {
		{"/corporations/", "Overview", []string{"corporations.html"}},
		{"/corporations/members/", "Members", []string{"corp_members.html"}},
		{"/corporations/wallets/", "Wallets", []string{"corp_wallets.html"}},
		{"/corporations/orders/", "Orders", []string{"corp_orders.html"}},
		{"/corporations/assets/", "Assets", []string{"corp_assets.html"}},
		{"/corporations/structures/", "Structures", []string{"corp_structures.html"}},
		{"/corporations/killmails/", "Killmails", []string{"killmails.html"}},
		{"/doctrines/", "Doctrines", []string{"doctrines.html", "doctrine.html"}},
		{"/doctrines/fits/", "Corporate Loadouts", []string{"corp_fits.html"}},
		{"/srp/", "Ship replacement", []string{"srp.html"}},
		{"/discord/servers", "Discord server", []string{"discord_servers.html"}},
		{"/groups/", "Groups", []string{"groups.html"}},
		{"/corporations/settings/", "Settings", []string{"corp_settings.html"}},
	},
	"intel": {
		{"/intel/wars/", "Wars", []string{"intel_wars.html"}},
		{"/intel/incursions/", "Incursions", []string{"intel_incursions.html"}},
		{"/intel/fw/", "Faction Warfare", []string{"intel_fw.html"}},
	},
	"tools": {
		{"/items/", "Item Browser", []string{"items.html"}},
	},
}

// navLink is one rendered nav link: the sidebar menu item and the
// section tab are the same thing.
type navLink struct {
	Href   string
	Label  string
	Active bool
}

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
