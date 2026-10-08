package app

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// TestNoInlineCodeInTemplatesOrScripts: the Content-Security-Policy
// allows no inline scripts, style attributes, or event handlers, so
// any of them in a template (or built by a static script into the
// markup it injects) is silently dead in the browser — a missing
// meter, a button that does nothing. This reads every embedded
// template and script, so a page the other tests never render is
// still covered. Dynamic geometry goes through data-* attributes
// that app.js applies (applyDataGeometry), not through style="…".
func TestNoInlineCodeInTemplatesOrScripts(t *testing.T) {
	forbidden := []struct {
		re   *regexp.Regexp
		what string
	}{
		{regexp.MustCompile(`(?i)\sstyle\s*=`), "a style attribute"},
		{regexp.MustCompile(`(?i)<style[\s>]`), "a <style> block"},
		{regexp.MustCompile(`(?i)\son[a-z]+\s*=`), "an inline event handler"},
		{regexp.MustCompile(`(?i)javascript:`), "a javascript: URL"},
	}
	inlineScript := regexp.MustCompile(`(?is)<script\b([^>]*)>`)

	templates, err := fs.Glob(templatesFS, "templates/*.html")
	if err != nil || len(templates) == 0 {
		t.Fatalf("no embedded templates found: %v", err)
	}
	for _, name := range templates {
		raw, err := fs.ReadFile(templatesFS, name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		src := string(raw)
		for _, f := range forbidden {
			if loc := f.re.FindStringIndex(src); loc != nil {
				t.Errorf("%s has %s near %q", name, f.what, snippet(src, loc[0]))
			}
		}
		for _, m := range inlineScript.FindAllStringSubmatchIndex(src, -1) {
			attrs := src[m[2]:m[3]]
			if !strings.Contains(attrs, "src=") {
				t.Errorf("%s has an inline <script> near %q", name, snippet(src, m[0]))
			}
		}
	}

	scripts, err := fs.Glob(staticFS, "static/*.js")
	if err != nil || len(scripts) == 0 {
		t.Fatalf("no embedded scripts found: %v", err)
	}
	jsForbidden := regexp.MustCompile(`(?i)\sstyle\s*=\s*\\?["']|\son[a-z]+\s*=\s*\\?["']|["']\s*<script\b`)
	for _, name := range scripts {
		raw, err := fs.ReadFile(staticFS, name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		src := string(raw)
		if loc := jsForbidden.FindStringIndex(src); loc != nil {
			t.Errorf("%s builds inline code near %q (the CSP would block it)", name, snippet(src, loc[0]))
		}
	}
}

// snippet returns a short, single-line excerpt of src around i for
// failure messages.
func snippet(src string, i int) string {
	start, end := max(i-20, 0), min(i+80, len(src))
	return strings.Join(strings.Fields(src[start:end]), " ")
}
