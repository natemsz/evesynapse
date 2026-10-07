package app

import (
	"fmt"
	"html"
	"html/template"
	"strings"
)

// =====================================================================
// Name links — one policy, every page
//
// EveSynapse's names exist to be followed. Anywhere a resolved name
// appears it renders as a link to the page that describes it:
//
//   - Items link to the item details page (/items/type/<typeID>/).
//   - The signed-in user's own linked characters link to their
//     character sheet (/character/?character=<id>) — everywhere,
//     including kill contexts.
//   - Everyone else links to the public pilot page
//     (/pilot/?character=<id>) — except in kill contexts, where
//     strangers link to their zKillboard pilot page instead, since
//     kill history is what zKillboard does best.
//   - Corporations link to the public corporation page
//     (/corporation/?corporation=<id>) and alliances to the public
//     alliance page (/alliance/?alliance=<id>) — in every context,
//     kill pages included: only characters change destination in
//     kill contexts, organizations stay in-app.
//   - Solar systems link to the system page (/system/?system=<id>)
//     and NPC stations to the station page
//     (/station/?station=<id>), again in every context. Player
//     structures link to the structure page
//     (/structure/?structure=<id>) once the worker has resolved
//     their name or the app knows their context; while a structure
//     is still just "Structure #<id>", its name stays text.
//
// The helpers are template functions (registered in pages.go's
// render) taking the page's ViewerChars set, so templates phrase
// every link identically and the policy lives in exactly one place.
// All of them degrade to plain escaped text when the id is unknown
// (<= 0) or the name unresolved — an unresolvable name stays text
// instead of becoming a dead link.
// =====================================================================

// itemLink renders an item name as a link to its item details
// page. Plain text when either side of the resolution is missing.
func itemLink(typeID int64, name string) template.HTML {
	if typeID <= 0 || name == "" {
		return template.HTML(html.EscapeString(name))
	}
	return template.HTML(fmt.Sprintf(`<a href="/items/type/%d/">%s</a>`, typeID, html.EscapeString(name)))
}

// charLink renders a character name by the link policy above: own
// characters to their sheet, strangers to the public pilot page.
// viewer is the signed-in user's linked-character set (pageData's
// ViewerChars). Plain text when the id is unknown.
func charLink(viewer map[int64]bool, charID int64, name string) template.HTML {
	if charID <= 0 || name == "" {
		return template.HTML(html.EscapeString(name))
	}
	if viewer[charID] {
		return template.HTML(fmt.Sprintf(`<a href="/character/?character=%d">%s</a>`, charID, html.EscapeString(name)))
	}
	return template.HTML(fmt.Sprintf(`<a href="/pilot/?character=%d">%s</a>`, charID, html.EscapeString(name)))
}

// killCharLink is charLink for kill contexts (killmail listings,
// kill pages): own characters still go to their sheet, but
// strangers go to zKillboard's character page, where their kill
// history lives.
func killCharLink(viewer map[int64]bool, charID int64, name string) template.HTML {
	if charID <= 0 || name == "" {
		return template.HTML(html.EscapeString(name))
	}
	if viewer[charID] {
		return template.HTML(fmt.Sprintf(`<a href="/character/?character=%d">%s</a>`, charID, html.EscapeString(name)))
	}
	return template.HTML(fmt.Sprintf(`<a href="https://zkillboard.com/character/%d/" target="_blank" rel="noopener noreferrer">%s</a>`, charID, html.EscapeString(name)))
}

// zkillKillLink renders the "View on zKillboard" link for one
// killmail. Plain text (empty name) degrades to empty.
func zkillKillLink(killmailID int64) template.HTML {
	if killmailID <= 0 {
		return ""
	}
	return template.HTML(fmt.Sprintf(`<a href="https://zkillboard.com/kill/%d/" target="_blank" rel="noopener noreferrer">View on zKillboard</a>`, killmailID))
}

// corpLink renders a corporation name as a link to the public
// corporation page. Plain text when either side is missing.
func corpLink(corpID int64, name string) template.HTML {
	if corpID <= 0 || name == "" {
		return template.HTML(html.EscapeString(name))
	}
	return template.HTML(fmt.Sprintf(`<a href="/corporation/?corporation=%d">%s</a>`, corpID, html.EscapeString(name)))
}

// allianceLink renders an alliance name as a link to the public
// alliance page. Plain text when either side is missing.
func allianceLink(allianceID int64, name string) template.HTML {
	if allianceID <= 0 || name == "" {
		return template.HTML(html.EscapeString(name))
	}
	return template.HTML(fmt.Sprintf(`<a href="/alliance/?alliance=%d">%s</a>`, allianceID, html.EscapeString(name)))
}

// systemLink renders a solar-system name as a link to the system
// page. Plain text when either side is missing.
func systemLink(systemID int64, name string) template.HTML {
	if systemID <= 0 || name == "" {
		return template.HTML(html.EscapeString(name))
	}
	return template.HTML(fmt.Sprintf(`<a href="/system/?system=%d">%s</a>`, systemID, html.EscapeString(name)))
}

// stationLink renders an NPC station name as a link to the
// station page. Plain text when either side is missing.
func stationLink(stationID int64, name string) template.HTML {
	if stationID <= 0 || name == "" {
		return template.HTML(html.EscapeString(name))
	}
	return template.HTML(fmt.Sprintf(`<a href="/station/?station=%d">%s</a>`, stationID, html.EscapeString(name)))
}

// structureLink renders a player structure's resolved name as a
// link to the structure page. Plain text when either side is
// missing — callers pass the id only once resolution has landed,
// so an unresolved "Structure #<id>" stays text.
func structureLink(structureID int64, name string) template.HTML {
	if structureID <= 0 || name == "" {
		return template.HTML(html.EscapeString(name))
	}
	return template.HTML(fmt.Sprintf(`<a href="/structure/?structure=%d">%s</a>`, structureID, html.EscapeString(name)))
}

// placeRef is one rendered location: its display title (resolved
// with the usual fallbacks) plus the page it links to when the
// location is a kind the link policy covers — StationID for an
// NPC station, SystemID for a solar system, StructureID for a
// player structure whose name or context the app holds — all 0
// for containers and unresolved ids, which stay text.
type placeRef struct {
	Name        string
	StationID   int64
	StructureID int64
	SystemID    int64
}

// placeLink renders a placeRef by the policy: station wins, then
// structure, then system, otherwise the plain title.
func placeLink(ref placeRef) template.HTML {
	if ref.StationID > 0 {
		return stationLink(ref.StationID, ref.Name)
	}
	if ref.StructureID > 0 {
		return structureLink(ref.StructureID, ref.Name)
	}
	if ref.SystemID > 0 {
		return systemLink(ref.SystemID, ref.Name)
	}
	return template.HTML(html.EscapeString(ref.Name))
}

// skillLevel renders the in-game 5-box skill level indicator:
// five small squares, filled for trained levels (gold when the
// skill is fully trained to V), blue-pulsing for the level
// currently training, darker blue for queued levels, dark for
// untrained. trained is 0-5, next is the training/queued level
// (0 when none), state is "training", "queued", or "".
func skillLevel(trained, next int, state string) template.HTML {
	if trained < 0 {
		trained = 0
	}
	if trained > 5 {
		trained = 5
	}
	label := fmt.Sprintf("Level %d of 5", trained)
	switch state {
	case "training":
		if next >= 1 && next <= 5 {
			label += fmt.Sprintf(", training level %d", next)
		}
	case "queued":
		if next >= 1 && next <= 5 {
			label += fmt.Sprintf(", level %d queued", next)
		}
	}
	var b strings.Builder
	b.WriteString(`<span class="lvl" role="img" aria-label="`)
	b.WriteString(html.EscapeString(label))
	b.WriteString(`">`)
	for i := 1; i <= 5; i++ {
		cls := ""
		switch {
		case state == "training" && i == next:
			cls = ` class="training"`
		case state == "queued" && i > trained && i <= next:
			cls = ` class="queued"`
		case i <= trained:
			if trained >= 5 {
				cls = ` class="on max"`
			} else {
				cls = ` class="on"`
			}
		}
		b.WriteString("<i" + cls + "></i>")
	}
	b.WriteString(`</span>`)
	return template.HTML(b.String())
}

// linkFuncMap is the template function set carrying the link
// policy; pages.go registers it for every render.
func linkFuncMap() template.FuncMap {
	return template.FuncMap{
		"itemLink":      itemLink,
		"charLink":      charLink,
		"killCharLink":  killCharLink,
		"zkillKillLink": zkillKillLink,
		"corpLink":      corpLink,
		"allianceLink":  allianceLink,
		"systemLink":    systemLink,
		"stationLink":   stationLink,
		"structureLink": structureLink,
		"placeLink":     placeLink,
		"skillLevel":    skillLevel,
		// Not a link: how a stored time is written out (timestamps.go).
		"rfc3339": rfc3339,
	}
}
