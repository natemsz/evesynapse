package app

import (
	"os"
	"strings"
	"testing"
)

// TestCoreSheetDefinesFontRoles: the landing page loads style.css
// without layout.css, so the core sheet holds the font roles.
func TestCoreSheetDefinesFontRoles(t *testing.T) {
	css, err := os.ReadFile("static/style.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"--font-body:", "--font-display:", "--font-prose:", "--font-mono:", "--font-num:"} {
		if !strings.Contains(string(css), role) {
			t.Errorf("style.css does not define %s", strings.TrimSuffix(role, ":"))
		}
	}
}
