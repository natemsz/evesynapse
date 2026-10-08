package app

import (
	"io/fs"
	"strings"
	"testing"
)

// TestBareButtonsShareTheEmberStyle: every plain <button> and submit
// or button <input> with no class falls back to the ember look of
// .btn (the ship-fit tool's style) instead of the browser default, in
// the base, hover, focus and disabled states, and in the light theme.
// The stylesheet is the only place that does it, so this pins it.
func TestBareButtonsShareTheEmberStyle(t *testing.T) {
	raw, err := fs.ReadFile(staticFS, "static/style.css")
	if err != nil {
		t.Fatalf("read style.css: %v", err)
	}
	css := string(raw)
	const bare = `:where(button:not([class]), input[type="submit"]:not([class]), input[type="button"]:not([class]))`
	for _, want := range []string{
		".btn,\n" + bare + " {\n  display: inline-block;",
		bare + ":hover:not(:disabled) {",
		bare + ":focus-visible {",
		bare + ":disabled {\n  opacity: 0.4;",
		`html[data-theme="light"] ` + bare + ":hover:not(:disabled)",
		// The danger variant exists for the buttons that name it.
		".btn.btn-danger {",
		// The typography rule (Univers Condensed) covers them too.
		"\n" + bare + ",\n.branch > summary,",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("style.css missing %q", want)
		}
	}
}

// TestNoButtonStylesInTemplates: a button gets its look from the
// stylesheet. A template that styles one inline (the CSP forbids it
// anyway) or reaches for a class the stylesheet does not know would
// quietly drop out of the shared style.
func TestNoUnknownButtonClassesInTemplates(t *testing.T) {
	raw, err := fs.ReadFile(staticFS, "static/style.css")
	if err != nil {
		t.Fatalf("read style.css: %v", err)
	}
	css := string(raw)
	templates, _ := fs.Glob(templatesFS, "templates/*.html")
	for _, name := range templates {
		src, err := fs.ReadFile(templatesFS, name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, line := range strings.Split(string(src), "\n") {
			at := strings.Index(line, `<button class="`)
			if at < 0 {
				continue
			}
			class := line[at+len(`<button class="`):]
			class = class[:strings.Index(class, `"`)]
			for _, c := range strings.Fields(class) {
				if strings.Contains(c, "{{") {
					continue
				}
				if !strings.Contains(css, "."+c) && !strings.Contains(string(src), `"`+c+`"`) {
					t.Errorf("%s: button class %q is not in style.css", name, c)
				}
			}
		}
	}
}
