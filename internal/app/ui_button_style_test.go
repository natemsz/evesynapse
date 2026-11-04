package app

import (
	"io/fs"
	"strings"
	"testing"
)

// TestNoButtonStylesInTemplates: a button gets its look from the
// stylesheet. A template that styles one inline (the CSP forbids it
// anyway) or reaches for a class no stylesheet knows would
// quietly drop out of the shared style.
func TestNoUnknownButtonClassesInTemplates(t *testing.T) {
	// Feature sheets own their pages' button classes (appraisal.css
	// holds the ap- rules), so the check reads every stylesheet.
	sheets, _ := fs.Glob(staticFS, "static/*.css")
	var all strings.Builder
	for _, name := range sheets {
		raw, err := fs.ReadFile(staticFS, name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		all.Write(raw)
	}
	css := all.String()
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
					t.Errorf("%s: button class %q is in no stylesheet", name, c)
				}
			}
		}
	}
}
