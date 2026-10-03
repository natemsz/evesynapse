package app

import (
	"fmt"
	"html"
	"html/template"
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
//
// The helpers are template functions (registered in pages.go's
// render) taking the page's ViewerChars set, so templates phrase
// every link identically and the policy lives in exactly one place.
// All three degrade to plain escaped text when the id is unknown
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

// linkFuncMap is the template function set carrying the link
// policy; pages.go registers it for every render.
func linkFuncMap() template.FuncMap {
	return template.FuncMap{
		"itemLink":      itemLink,
		"charLink":      charLink,
		"killCharLink":  killCharLink,
		"zkillKillLink": zkillKillLink,
	}
}
