package app

import (
	"fmt"
	"html/template"
	"strings"
)

// ---------------------------------------------------------------------------
// Market category glyphs: our own original inline-SVG marks for the
// Market page's category browser, drawn in the same ember family
// as the navigation icons (viewBox 0 0 24 24, painted from the one
// shared nav-glyph-gradient, with the solid flare-gold CSS fill on
// .market-group-icon as the fallback when gradient paint fails).
// They are simple geometric compositions inspired by the market's
// subject matter — never CCP artwork.
//
// Mapping is by the real group names in the imported
// invMarketGroups data (plus the one verified ID anchor below).
// Anything unmapped — most subgroups included — takes the generic
// mark; a handful of child-name patterns (drones, charges, rigs…)
// reuse the child-appropriate glyph so deep tree levels still read
// consistently.
// ---------------------------------------------------------------------------

// marketGroupIconKey names the glyph for one market group. id is
// the market group ID; name is its imported display name.
func marketGroupIconKey(name string, id int64) string {
	normalized := strings.ToLower(strings.TrimSpace(name))

	// Verified anchor: Implants & Boosters is market group 24
	// (its ESI record names children 23 and 977).
	if id == 24 {
		return "implants"
	}
	switch normalized {
	case "ships":
		return "ships"
	case "ship equipment":
		return "modules"
	case "ship modifications", "ship and module modifications":
		return "rigs"
	case "ammunition & charges":
		return "charges"
	case "drones":
		return "drones"
	case "implants & boosters":
		return "implants"
	case "apparel":
		return "apparel"
	case "structures", "structure equipment", "structure modifications":
		return "structures"
	case "manufacture & research", "materials", "raw materials":
		return "materials"
	}

	// Child-appropriate reuse for subgroups below the mapped
	// top level. Order matters: the specific words win before
	// the broad ones.
	switch {
	case strings.Contains(normalized, "implant"), strings.Contains(normalized, "booster"):
		return "implants"
	case strings.Contains(normalized, "drone"), strings.Contains(normalized, "fighter"):
		return "drones"
	case strings.Contains(normalized, "apparel"), strings.Contains(normalized, "clothing"):
		return "apparel"
	case strings.Contains(normalized, "structure"):
		return "structures"
	case strings.Contains(normalized, "material"), strings.Contains(normalized, "mineral"), strings.Contains(normalized, " ore"):
		return "materials"
	case strings.Contains(normalized, "rig"):
		return "rigs"
	case strings.Contains(normalized, "charge"), strings.Contains(normalized, "ammunition"), strings.Contains(normalized, "ammo"):
		return "charges"
	case strings.Contains(normalized, "module"), strings.Contains(normalized, "equipment"):
		return "modules"
	}
	return "generic"
}

// marketGroupIconSVG renders the group's glyph as inline SVG.
// The markup carries no gradient definition of its own: the page's
// single shared nav-glyph-gradient (defined once by the base
// layout) paints every glyph, exactly like the nav icons.
func marketGroupIconSVG(name string, id int64) template.HTML {
	key := marketGroupIconKey(name, id)
	return template.HTML(fmt.Sprintf(
		`<svg class="market-group-icon-svg" viewBox="0 0 24 24" aria-hidden="true" focusable="false" data-market-icon="%s">%s</svg>`,
		key, marketGroupIconArt[key]))
}

const marketIconPaint = `fill="none" stroke="url(#nav-glyph-gradient)" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"`
const marketIconFill = `fill="url(#nav-glyph-gradient)"`

// marketGroupIconArt is the original path art per glyph key.
var marketGroupIconArt = map[string]string{
	// A hull wedge with a canopy dot.
	"ships": `<g ` + marketIconPaint + `><path d="M12 3.2 15.4 8.2v5.6l4.6 4.9H4l4.6-4.9V8.2Z"/><path d="M8.6 13.8H4.6M19.4 13.8h-4"/></g><circle cx="12" cy="10.4" r="1.5" ` + marketIconFill + `/>`,
	// A socketed chip with pins: a fitted module.
	"modules": `<g ` + marketIconPaint + `><rect x="7" y="7" width="10" height="10" rx="1.6"/><path d="M9.5 4.5V7M14.5 4.5V7M9.5 17v2.5M14.5 17v2.5M4.5 9.5H7M4.5 14.5H7M17 9.5h2.5M17 14.5h2.5"/></g><circle cx="12" cy="12" r="1.6" ` + marketIconFill + `/>`,
	// A ring braced by three nodes: a rig's calibration loop.
	"rigs": `<g ` + marketIconPaint + `><circle cx="12" cy="12" r="6.4"/><path d="M12 5.6v3M17.6 15.2l-2.6-1.5M6.4 15.2l2.6-1.5"/></g><g ` + marketIconFill + `><circle cx="12" cy="5.4" r="1.4"/><circle cx="17.8" cy="15.4" r="1.4"/><circle cx="6.2" cy="15.4" r="1.4"/></g>`,
	// A cased charge standing upright.
	"charges": `<g ` + marketIconPaint + `><path d="M9.2 21v-8.2c0-3 1.2-5.6 2.8-7.8 1.6 2.2 2.8 4.8 2.8 7.8V21Z"/><path d="M9.2 16.4h5.6M9.6 21h4.8"/></g>`,
	// A central eye with four orbit arms.
	"drones": `<g ` + marketIconPaint + `><path d="M5 5l4.2 4.2M19 5l-4.2 4.2M5 19l4.2-4.2M19 19l-4.2-4.2"/></g><g ` + marketIconFill + `><circle cx="4.6" cy="4.6" r="1.5"/><circle cx="19.4" cy="4.6" r="1.5"/><circle cx="4.6" cy="19.4" r="1.5"/><circle cx="19.4" cy="19.4" r="1.5"/></g><path d="M12 8.6 15.4 12 12 15.4 8.6 12Z" ` + marketIconFill + `/>`,
	// A cranial node with circuit branches.
	"implants": `<g ` + marketIconPaint + `><circle cx="10.6" cy="10.6" r="5.6"/><path d="M14.8 14.8 17 17h3.4M15.8 11h4.6M11 15.8v4.6"/></g><g ` + marketIconFill + `><circle cx="10.6" cy="10.6" r="1.7"/><circle cx="20.6" cy="17" r="1.3"/><circle cx="20.6" cy="11" r="1.3"/><circle cx="11" cy="20.6" r="1.3"/></g>`,
	// A plain shirt outline.
	"apparel": `<g ` + marketIconPaint + `><path d="M8.4 4.8 4 7.6l1.8 3.4 2-1.1v9.3h8.4v-9.3l2 1.1L20 7.6l-4.4-2.8a3.6 3.6 0 0 1-7.2 0Z"/></g>`,
	// A tower block on a base plinth with a beacon.
	"structures": `<g ` + marketIconPaint + `><path d="M9 20.4v-9.8L12 7l3 3.6v9.8"/><path d="M4.8 20.4h14.4M12 7V4"/></g><circle cx="12" cy="3.2" r="1.1" ` + marketIconFill + `/>`,
	// Three stacked raw ingots.
	"materials": `<g ` + marketIconPaint + `><path d="M12 4.2 18 7.4l-6 3.2-6-3.2Z"/><path d="M6.2 12.2 12 15.4l5.8-3.2M6.2 16.6 12 19.8l5.8-3.2"/></g>`,
	// Fallback: a lone market node.
	"generic": `<g ` + marketIconPaint + `><path d="M12 4.6 18.4 12 12 19.4 5.6 12Z"/></g><circle cx="12" cy="12" r="1.6" ` + marketIconFill + `/>`,
}
