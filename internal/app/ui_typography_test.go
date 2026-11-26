package app

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// TestFontSizesNameTheScale: every font-size in every sheet names a
// step of the type scale (or a relative step) or inherits. A literal
// size drifts off the scale the next time the scale moves, leaving
// text too small or bigger than its parent. The phone band's root
// size is the one
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
