package app

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// TestMenuTextIsAllCapsRajdhani pins the menu face: every menu
// surface (sidenav rows, branch summaries, header links, pop menus)
// reads in Rajdhani full capitals, never the body face and never
// small caps. Dropdown items are the one exception: Rajdhani
// SemiBold in their labels' own case, under capital group labels.
func TestMenuTextIsAllCapsRajdhani(t *testing.T) {
	style := readStatic(t, "style.css")
	for _, want := range []string{
		// The display-face rows state their capitals in the menu rule.
		".sidenav a.navlink,\n.sidebar-account-footer .navlink,\n.branch > summary {\n  text-transform: uppercase;\n}",
		// Header links keep the SemiBold face.
		"header a.navlink {\n  font-family: \"Rajdhani\", \"Segoe UI\", sans-serif;\n  font-weight: 600;\n  font-synthesis-weight: none;\n  text-transform: uppercase;",
		// Dropdown items: SemiBold, in their labels' own case,
		// tighter letter spacing.
		".branch .menu a, .switcher .menu a, .switcher-menu a {\n  font-family: \"Rajdhani\", \"Segoe UI\", sans-serif;\n  font-weight: 600;\n  font-synthesis-weight: none;\n  text-transform: none;\n  letter-spacing: 0.03em;",
		// Pop-menu items too.
		".menu-pop-body a, .menu-pop-body button {\n  display: block;\n  width: 100%;\n  text-align: left;\n  padding: 0.45rem 0.6rem;\n  font-family: \"Rajdhani\", \"Segoe UI\", sans-serif;\n  font-weight: 600;\n  text-transform: uppercase;",
		// The sidenav layout rule carries no casing of its own: the
		// menu rule above is the only casing menu rows get.
		".sidenav a.navlink,\n.sidebar .branch > summary {\n  display: flex;\n  align-items: center;\n  gap: 0.6rem;\n  min-height: 2.35rem;\n  padding: 0.42rem 0.55rem;\n  border-radius: var(--r-sm);\n  color: var(--text);\n  font-size: var(--fs-md);\n  letter-spacing: 0.04em;\n  cursor: pointer;\n}",
	} {
		if !strings.Contains(style, want) {
			t.Errorf("style.css missing menu rule %q", want)
		}
	}
	// Menu text never renders in small caps.
	if strings.Contains(style, "font-variant-caps: small-caps") {
		t.Errorf("style.css carries a small-caps rule; menu text is full capitals")
	}
}

// TestTabNavTypeSpec pins the tab-navigation face: every tab bar
// on the Rajdhani capitals treatment (section tabs, the section
// sub-tabs, the strategy tabs, the contract spec tabs, the
// fitting view tabs) reads at 12px, weight 500, regular letter
// spacing. Filter chips keep their own heavier control
// treatment.
func TestTabNavTypeSpec(t *testing.T) {
	style := readStatic(t, "style.css")
	for _, want := range []string{
		// Section tabs, sub-tabs and strategy tabs share the tab
		// rule; the chip is not a tab and keeps its own rule.
		".section-tabs a, .section-tabs .tabs-more-btn, .section-subtabs a, .section-subtabs .tabs-more-btn, .ti-tabs button {\n  font-family: \"Rajdhani\", \"Segoe UI\", sans-serif;\n  font-weight: 500;\n  text-transform: uppercase;\n  letter-spacing: normal;\n  font-size: var(--fs-xs);\n}",
		".chip {\n  font-family: \"Rajdhani\", \"Segoe UI\", sans-serif;\n  font-weight: 600;\n  text-transform: uppercase;\n  letter-spacing: 0.06em;\n}",
		// The strategy tabs' pill paint block states the same spec.
		".ti-tabs button {\n  font-family: Rajdhani, sans-serif;\n  font-weight: 500;\n  text-transform: uppercase;\n  letter-spacing: normal;\n  font-size: var(--fs-xs);",
	} {
		if !strings.Contains(style, want) {
			t.Errorf("style.css missing tab rule %q", want)
		}
	}
	contracts := readStatic(t, "contracts.css")
	if want := "font-weight: 600; text-transform: uppercase; letter-spacing: normal; font-size: var(--fs-xs);"; !strings.Contains(contracts, want) {
		t.Errorf("contracts.css .cs-tab missing tab spec %q", want)
	}
	fit := readStatic(t, "fit.css")
	if want := "font-weight: 500; text-transform: uppercase; letter-spacing: normal;\n    font-size: var(--fs-xs);"; !strings.Contains(fit, want) {
		t.Errorf("fit.css .fit-mtabs button missing tab spec %q", want)
	}
}

// TestOpenProseTreatment pins the prose role: a block whose job
// is to explain reads in Rajdhani Medium at --fs-sm with a 1.55
// line height, in the prose ink each theme mixes from its own
// text and muted values; emphasis inside prose reads at 600.
// Pages across the site mark their explanatory blocks with it.
// The admin pages mark none: their prose is marked with the
// admin rework. The landing and home pages mark none either:
// the landing carries its own type system and home is widgets.
func TestOpenProseTreatment(t *testing.T) {
	style := readStatic(t, "style.css")
	for _, want := range []string{
		"--prose: color-mix(in srgb, var(--text) 55%, var(--muted));",
		".prose {\n  font-family: \"Rajdhani\", \"Eve Sans Neue\", \"Inter\", \"Segoe UI\", \"Helvetica Neue\", Helvetica, Arial, sans-serif;\n  font-weight: 500;\n  font-size: var(--fs-sm);\n  line-height: 1.55;\n  color: var(--prose);\n}",
		// Emphasis inside prose reads in Rajdhani SemiBold.
		".prose strong { font-weight: 600; }",
		// Definition answers inside a prose block stay prose.
		".prose dd { font-family: inherit; }",
	} {
		if !strings.Contains(style, want) {
			t.Errorf("style.css missing prose rule %q", want)
		}
	}
	readTpl := func(name string) string {
		raw, err := fs.ReadFile(templatesFS, "templates/"+name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return string(raw)
	}
	marksProse := regexp.MustCompile(`class="(?:[^"]* )?prose(?: [^"]*)?"`)
	for _, name := range []string{
		"about.html", "assets.html", "battle.html", "battles.html",
		"calendar.html", "channel.html", "channels.html", "character.html",
		"characters.html", "contacts.html", "contract_detail.html",
		"corp_assets.html", "corp_contracts.html", "corp_members.html",
		"corp_orders.html", "corp_settings.html", "corp_structures.html",
		"corp_wallets.html", "discord_servers.html", "doctrine.html",
		"doctrines.html", "enroll.html", "factor.html", "feed.html",
		"fittings.html", "fittings_saved.html", "groups.html",
		"industry_jobs.html", "intel_fw.html", "intel_workbench.html",
		"items.html", "kill.html", "killboard.html", "killmails.html",
		"link.html", "market.html", "market_appraise.html",
		"market_basket.html", "market_baskets.html",
		"market_leaderboard.html", "market_restock.html",
		"market_scanner.html", "market_tradefinder.html",
		"market_tradeideas.html", "market_trends.html",
		"notifications.html", "op_paps.html", "orders.html",
		"piplanner.html", "planets.html", "planner.html", "profit.html",
		"profit_blueprint.html", "report.html", "scans.html",
		"search.html", "security_codes.html", "settings.html",
		"srp.html", "swarm.html",
	} {
		if !marksProse.MatchString(readTpl(name)) {
			t.Errorf("%s marks no prose; its explanatory blocks carry the treatment", name)
		}
	}
	for _, name := range []string{"admin.html", "admin_esi.html", "sync.html", "admin_notices.html", "landing.html", "home.html"} {
		if marksProse.MatchString(readTpl(name)) {
			t.Errorf("%s marks prose; it stays outside the sweep", name)
		}
	}
}

// TestPlanNodeSelectionWeight pins the PI tree's selection state:
// a selected node is marked by its border and background alone, at
// the node's normal text weight, like its sibling states.
func TestPlanNodeSelectionWeight(t *testing.T) {
	pi := readStatic(t, "pi.css")
	want := ".pi-node.selected { border-color: var(--gold); background: var(--sel-bg); }"
	if !strings.Contains(pi, want) {
		t.Errorf("pi.css missing selection rule %q", want)
	}
}

// TestFontSizesNameTheScale: every font-size in every sheet names a
// step of the type scale (or a relative step) or inherits. A literal
// size drifts off the scale the next time the scale moves; the type
// sweep and its restamp left sizes too small or bigger than their
// parent exactly that way. The phone band's root size is the one
// literal: it sets the rem the whole scale hangs from.
func TestFontSizesNameTheScale(t *testing.T) {
	decl := regexp.MustCompile(`font-size:\s*([^;}]+)`)
	// A fluid size (clamp, min, max over container or viewport units)
	// passes when its bounds are scale steps: no literal length but 1em.
	oneEm := regexp.MustCompile(`(^|[^0-9.])1em\b`)
	literal := regexp.MustCompile(`(^|[^-a-z0-9.])[0-9.]+(em|rem|px)\b`)
	sheets, _ := fs.Glob(staticFS, "static/*.css")
	for _, name := range sheets {
		raw, err := fs.ReadFile(staticFS, name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		css := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(string(raw), "")
		for _, m := range decl.FindAllStringSubmatch(css, -1) {
			v := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(m[1]), "!important"))
			if v == "inherit" || v == "15px" {
				continue
			}
			if strings.Contains(v, "var(--fs-") && !literal.MatchString(oneEm.ReplaceAllString(v, "$1")) {
				continue
			}
			t.Errorf("%s: font-size %q names no step of the type scale", name, v)
		}
	}
}
