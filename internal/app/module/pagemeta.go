package module

import (
	"encoding/json"
	"html"
	"html/template"
	"strings"
)

// ---------------------------------------------------------------------------
// PageMeta: the ONE head-metadata mechanism for public pages.
//
// A handler fills a PageMeta with the values it already loaded and
// RenderPageMeta turns it into the .HeadMeta block base.html prints:
// meta description, canonical, Open Graph, twitter:card, and any
// structured data. Every value is attribute-escaped with
// html.EscapeString before assembly (the public fit page's existing
// discipline); only then is the result template.HTML.
// ---------------------------------------------------------------------------

// CanonicalHost is the host every canonical and og:url is built on.
const CanonicalHost = "https://evesynapse.app"

// PageMeta is one public page's head metadata.
type PageMeta struct {
	// Title is the og:title (usually the page's displayed title);
	// the document title itself stays the templates' business.
	Title string
	// Description is the page's unique plain description (<=160
	// chars by convention; the patterns live with the handlers).
	Description string
	// CanonicalPath is the page's clean path ("/items/type/34/").
	// Any query or fragment on it is stripped; "" emits no
	// canonical and no og:url.
	CanonicalPath string
	// CanonicalURL, when set, is the canonical address verbatim
	// and wins over CanonicalPath. It exists for pages that have
	// always carried their own absolute address — the public fit
	// page, whose og:url is the install's configured origin and
	// is omitted entirely where the install has none. Moving it
	// onto the helper must not change that output.
	CanonicalURL string
	// ImagePath, when set, is the page's social preview image
	// ("/intel/scan/{id}/og.png"): emitted as an absolute
	// og:image on the canonical host, and the twitter card
	// upgrades to summary_large_image. Pages without one keep
	// the plain summary card.
	ImagePath string
	// WebApplication asks for the front page's static
	// WebApplication JSON-LD block.
	WebApplication bool
	// Breadcrumbs, when the page itself displays a real hierarchy
	// (never invented): root first, the page itself last.
	Breadcrumbs []Breadcrumb
}

// Breadcrumb is one level of a displayed hierarchy.
type Breadcrumb struct {
	Name string
	Path string
}

// CanonicalURL builds a canonical address: the canonical host plus
// the clean path, with any query string or fragment stripped.
func CanonicalURL(path string) string {
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	if path == "" {
		return ""
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return CanonicalHost + path
}

// RenderPageMeta composes a PageMeta into the head block. The tag
// order and formats are the public fit page's original ones, so a
// migrated page keeps its existing tags byte-for-byte, with the
// canonical and twitter card added by the shared mechanism.
func RenderPageMeta(m PageMeta) template.HTML {
	esc := html.EscapeString
	canonical := CanonicalURL(m.CanonicalPath)
	if m.CanonicalURL != "" {
		canonical = m.CanonicalURL
	}
	var b strings.Builder
	if m.Description != "" {
		b.WriteString(`<meta name="description" content="` + esc(m.Description) + `">` + "\n")
	}
	if canonical != "" {
		b.WriteString(`<link rel="canonical" href="` + esc(canonical) + `">` + "\n")
	}
	if m.Title != "" {
		b.WriteString(`<meta property="og:title" content="` + esc(m.Title) + `">` + "\n")
	}
	if m.Description != "" {
		b.WriteString(`<meta property="og:description" content="` + esc(m.Description) + `">` + "\n")
	}
	b.WriteString(`<meta property="og:type" content="website">` + "\n")
	if canonical != "" {
		b.WriteString(`<meta property="og:url" content="` + esc(canonical) + `">` + "\n")
	}
	if m.ImagePath != "" {
		img := CanonicalHost + m.ImagePath
		b.WriteString(`<meta property="og:image" content="` + esc(img) + `">` + "\n")
		b.WriteString(`<meta name="twitter:image" content="` + esc(img) + `">` + "\n")
		b.WriteString(`<meta name="twitter:card" content="summary_large_image">` + "\n")
	} else {
		b.WriteString(`<meta name="twitter:card" content="summary">` + "\n")
	}
	if m.WebApplication {
		b.WriteString(`<script type="application/ld+json">` +
			jsonLD(map[string]any{
				"@context":            "https://schema.org",
				"@type":               "WebApplication",
				"name":                "EveSynapse",
				"url":                 CanonicalHost + "/",
				"applicationCategory": "GameApplication",
				"operatingSystem":     "Web",
				"offers":              map[string]any{"@type": "Offer", "price": 0},
			}) + `</script>` + "\n")
	}
	if len(m.Breadcrumbs) > 0 {
		items := make([]any, 0, len(m.Breadcrumbs))
		for i, c := range m.Breadcrumbs {
			item := map[string]any{"@type": "ListItem", "position": i + 1, "name": c.Name}
			if u := CanonicalURL(c.Path); u != "" {
				item["item"] = u
			}
			items = append(items, item)
		}
		b.WriteString(`<script type="application/ld+json">` +
			jsonLD(map[string]any{
				"@context":        "https://schema.org",
				"@type":           "BreadcrumbList",
				"itemListElement": items,
			}) + `</script>` + "\n")
	}
	return template.HTML(b.String())
}

// jsonLD marshals one structured-data value. encoding/json escapes
// <, > and & in strings by default, so a name can never close the
// script element it lands in.
func jsonLD(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(raw)
}
