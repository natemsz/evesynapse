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
	case "structures":
		return "structures"
	case "structure equipment":
		return "structure-equipment"
	case "structure modifications":
		return "structure-modifications"
	case "manufacture & research", "materials", "raw materials":
		return "materials"
	case "blueprints":
		return "blueprints"
	case "skills":
		return "skills"
	case "trade goods":
		return "tradegoods"
	case "personalization":
		return "personalization"
	case "pilot's services":
		return "services"
	case "ship skins":
		return "skins"
	case "special edition assets":
		return "special"
	}

	// Child-appropriate reuse for subgroups below the mapped
	// top level. Order matters: the specific words win before
	// the broad ones.
	switch {
	case strings.Contains(normalized, "skin"):
		return "skins"
	case strings.Contains(normalized, "blueprint"):
		return "blueprints"
	case strings.Contains(normalized, "skill"):
		return "skills"
	case strings.Contains(normalized, "trade good"):
		return "tradegoods"
	case strings.Contains(normalized, "implant"), strings.Contains(normalized, "booster"):
		return "implants"
	case strings.Contains(normalized, "drone"), strings.Contains(normalized, "fighter"):
		return "drones"
	case strings.Contains(normalized, "apparel"), strings.Contains(normalized, "clothing"):
		return "apparel"
	case strings.Contains(normalized, "structure equipment"):
		return "structure-equipment"
	case strings.Contains(normalized, "structure modification"), strings.Contains(normalized, "structure rig"):
		return "structure-modifications"
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
	// A rolled sheet whose paper carries a grid of filled squares;
	// the gaps between the squares read as the grid lines.
	"blueprints": `<g ` + marketIconPaint + `><rect x="8.4" y="5" width="11.8" height="13.4" rx="1"/><rect x="4.2" y="3.2" width="4.2" height="17" rx="2.1"/><path d="M4.2 6.6h4.2"/></g><g ` + marketIconFill + `><rect x="10.2" y="7.6" width="1.7" height="1.7" rx="0.3"/><rect x="12.5" y="7.6" width="1.7" height="1.7" rx="0.3"/><rect x="14.8" y="7.6" width="1.7" height="1.7" rx="0.3"/><rect x="17.1" y="7.6" width="1.7" height="1.7" rx="0.3"/><rect x="10.2" y="9.9" width="1.7" height="1.7" rx="0.3"/><rect x="12.5" y="9.9" width="1.7" height="1.7" rx="0.3"/><rect x="14.8" y="9.9" width="1.7" height="1.7" rx="0.3"/><rect x="17.1" y="9.9" width="1.7" height="1.7" rx="0.3"/><rect x="10.2" y="12.2" width="1.7" height="1.7" rx="0.3"/><rect x="12.5" y="12.2" width="1.7" height="1.7" rx="0.3"/><rect x="14.8" y="12.2" width="1.7" height="1.7" rx="0.3"/><rect x="17.1" y="12.2" width="1.7" height="1.7" rx="0.3"/><rect x="10.2" y="14.5" width="1.7" height="1.7" rx="0.3"/><rect x="12.5" y="14.5" width="1.7" height="1.7" rx="0.3"/><rect x="14.8" y="14.5" width="1.7" height="1.7" rx="0.3"/><rect x="17.1" y="14.5" width="1.7" height="1.7" rx="0.3"/></g>`,
	// An open book with a marker dot above the spine.
	"skills": `<g ` + marketIconPaint + `><path d="M12 6.2C9.8 4.8 6.6 4.4 4.2 5.1v13c2.4-.7 5.6-.3 7.8 1.1 2.2-1.4 5.4-1.8 7.8-1.1v-13c-2.4-.7-5.6-.3-7.8 1.1Z"/><path d="M12 6.2v13.1"/></g><circle cx="12" cy="3" r="1.1" ` + marketIconFill + `/>`,
	// An isometric parcel: a cube outline with its face edges.
	"tradegoods": `<g ` + marketIconPaint + `><path d="M12 3.6 19 7.7v8.6l-7 4.1-7-4.1V7.7Z"/><path d="M12 12 19 7.7M12 12 5 7.7M12 12v8.4"/></g>`,
	// The station tower fitted with a module chip.
	"structure-equipment": `<g ` + marketIconPaint + `><path d="M8.6 20.4v-8.6L12 8.4l3.4 3.4v8.6"/><path d="M4.4 20.4h15.2M12 8.4V5.2"/></g><circle cx="12" cy="4.4" r="1" ` + marketIconFill + `/><rect x="10" y="13.4" width="4" height="4" rx="0.8" ` + marketIconFill + `/>`,
	// The station tower wearing a tilted calibration ring.
	"structure-modifications": `<ellipse cx="12" cy="14.2" rx="6.8" ry="2.5" transform="rotate(-16 12 14.2)" ` + marketIconPaint + `/><g ` + marketIconPaint + `><path d="M9.2 20.4V11L12 7.6l2.8 3.4v9.4"/><path d="M4.8 20.4h14.4M12 7.6V4.6"/></g><circle cx="12" cy="3.8" r="1" ` + marketIconFill + `/>`,
	// A pilot bust with a sparkle: making it yours.
	"personalization": `<g ` + marketIconPaint + `><circle cx="10.8" cy="8" r="3.4"/><path d="M4.6 19.6c.7-3.3 3.1-5 6.2-5 2 0 3.7.7 4.9 2.1"/></g><path d="M18.4 3.2l.8 2 2 .8-2 .8-.8 2-.8-2-2-.8 2-.8Z" ` + marketIconFill + `/>`,
	// A pilot license card: portrait mark and service lines.
	"services": `<g ` + marketIconPaint + `><rect x="3.4" y="5.8" width="17.2" height="12.4" rx="1.8"/><circle cx="8.4" cy="10.6" r="1.7"/><path d="M5.9 15.4c.4-1.4 1.3-2.1 2.5-2.1s2.1.7 2.5 2.1M14.6 9.8h3.8M14.6 13h3.8"/></g>`,
	// A hull wedge wearing a paint droplet.
	"skins": `<g ` + marketIconPaint + `><path d="M11 3.4 14 7.8v5l4.2 4.5H3.8L8 12.8v-5Z"/></g><path d="M17.2 12.6c1.5 1.9 2.4 3.2 2.4 4.7a2.4 2.4 0 1 1-4.8 0c0-1.5.9-2.8 2.4-4.7Z" ` + marketIconFill + `/>`,
	// A rosette seal with a star: rare and commemorative.
	"special": `<g ` + marketIconPaint + `><circle cx="12" cy="9.8" r="5.4"/><path d="M8.9 14.2 7.2 19.8l3-1.5 1.8 1.9 1.8-1.9 3 1.5-1.7-5.6"/></g><path d="M12 6.6l.9 2.1 2.1.9-2.1.9-.9 2.1-.9-2.1-2.1-.9 2.1-.9Z" ` + marketIconFill + `/>`,
	// Fallback: a lone market node.
	"generic": `<g ` + marketIconPaint + `><path d="M12 4.6 18.4 12 12 19.4 5.6 12Z"/></g><circle cx="12" cy="12" r="1.6" ` + marketIconFill + `/>`,
}
