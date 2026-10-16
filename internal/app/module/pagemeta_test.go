package module

import (
	"strings"
	"testing"
)

// TestRenderPageMetaEscapesHostileValues: quotes, angle brackets
// and ampersands in a name are escaped in every context the helper
// emits — no value can break out of its attribute or element.
func TestRenderPageMetaEscapesHostileValues(t *testing.T) {
	hostile := `Bad " <script>alert(1)</script> & Co`
	out := string(RenderPageMeta(PageMeta{
		Title:         hostile,
		Description:   "About " + hostile,
		CanonicalPath: "/items/type/34/",
		Breadcrumbs:   []Breadcrumb{{Name: hostile, Path: "/items/"}, {Name: "Fine", Path: "/items/type/34/"}},
	}))
	if strings.Contains(out, "<script>alert") || strings.Contains(out, `content="Bad "`) {
		t.Errorf("a hostile value escaped raw into the head:\n%s", out)
	}
	for _, want := range []string{`Bad &#34; &lt;script&gt;`, "&amp; Co", `\u003cscript\u003e`} {
		if !strings.Contains(out, want) {
			t.Errorf("the escaped form %q is missing from:\n%s", want, out)
		}
	}
}

// TestCanonicalURL pins the canonical rules: the evesynapse.app
// host always, the clean path only — query strings and fragments
// never survive.
func TestCanonicalURL(t *testing.T) {
	for in, want := range map[string]string{
		"/items/":                    "https://evesynapse.app/items/",
		"/items/type/34/?q=x&page=2": "https://evesynapse.app/items/type/34/",
		"items/type/34/#top":         "https://evesynapse.app/items/type/34/",
		"/planner/?product=34":       "https://evesynapse.app/planner/",
		"":                           "",
		"?only=query":                "",
	} {
		if got := CanonicalURL(in); got != want {
			t.Errorf("CanonicalURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestRenderPageMetaKeepsTheFitPageTags: the tags the public fit
// page shipped before the shared mechanism (description, og:title,
// og:description, og:type, og:url) render byte-for-byte, in order;
// the mechanism adds the canonical and the twitter card around them.
func TestRenderPageMetaKeepsTheFitPageTags(t *testing.T) {
	out := string(RenderPageMeta(PageMeta{
		Title:         "My Fit (Rifter)",
		Description:   "A Rifter fit by Fixture Alpha for brawling.",
		CanonicalPath: "/fittings/public/fit/7/",
	}))
	legacy := []string{
		`<meta name="description" content="A Rifter fit by Fixture Alpha for brawling.">`,
		`<meta property="og:title" content="My Fit (Rifter)">`,
		`<meta property="og:description" content="A Rifter fit by Fixture Alpha for brawling.">`,
		`<meta property="og:type" content="website">`,
		`<meta property="og:url" content="https://evesynapse.app/fittings/public/fit/7/">`,
	}
	at := -1
	for _, line := range legacy {
		i := strings.Index(out, line)
		if i < 0 {
			t.Fatalf("a legacy fit-page tag changed:\nwant %q\nin:\n%s", line, out)
		}
		if i < at {
			t.Fatalf("legacy fit-page tags fell out of order at %q", line)
		}
		at = i
	}
	for _, added := range []string{
		`<link rel="canonical" href="https://evesynapse.app/fittings/public/fit/7/">`,
		`<meta name="twitter:card" content="summary">`,
	} {
		if !strings.Contains(out, added) {
			t.Errorf("the shared mechanism did not add %q", added)
		}
	}
}

// TestRenderPageMetaStructuredData: the front page's WebApplication
// block and a BreadcrumbList carry only what they were given.
func TestRenderPageMetaStructuredData(t *testing.T) {
	front := string(RenderPageMeta(PageMeta{WebApplication: true}))
	for _, want := range []string{`"@type":"WebApplication"`, `"name":"EveSynapse"`, `"operatingSystem":"Web"`, `"price":0`} {
		if !strings.Contains(front, want) {
			t.Errorf("the WebApplication block is missing %q", want)
		}
	}
	detail := string(RenderPageMeta(PageMeta{Breadcrumbs: []Breadcrumb{
		{Name: "Item Database", Path: "/items/"},
		{Name: "Minerals", Path: "/items/group/18/"},
		{Name: "Tritanium", Path: "/items/type/34/"},
	}}))
	for _, want := range []string{`"@type":"BreadcrumbList"`, `"position":2`, `"name":"Tritanium"`,
		`"item":"https://evesynapse.app/items/type/34/"`} {
		if !strings.Contains(detail, want) {
			t.Errorf("the BreadcrumbList is missing %q", want)
		}
	}
	if strings.Contains(string(RenderPageMeta(PageMeta{})), "ld+json") {
		t.Error("an empty PageMeta emitted structured data")
	}
}
