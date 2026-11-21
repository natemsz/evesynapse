// Tests that keep the installer in step with the files it mirrors,
// and that what it writes to .env reads back exactly.
package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestCatalogCoversEveryTemplateKey(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", ".env.example"))
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?m)^(?:# ?)?([A-Z][A-Z0-9_]+)=`)
	for _, m := range re.FindAllStringSubmatch(string(data), -1) {
		if _, ok := settingFor(m[1]); !ok {
			t.Errorf(".env.example has %s, which the installer's catalog does not", m[1])
		}
	}
}

func TestWordmarkMatchesTemplate(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "internal", "app", "templates", "wordmark.html"))
	if err != nil {
		t.Fatal(err)
	}
	for i, d := range wordmarkPaths {
		if !strings.Contains(string(data), `d="`+d+`"`) {
			t.Errorf("wordmark letter %d differs from wordmark.html", i)
		}
	}
}

func TestEnvEditsRoundTrip(t *testing.T) {
	template := "# Comment kept\nEVE_CLIENT_ID=\nDATABASE_URL=postgres://evesynapse@localhost/x\n# The address\n# ESI_CONTACT=\nADDR=:8080\nADDR=:9090\n"
	values := map[string]string{
		"EVE_CLIENT_ID": "abc123",
		"DATABASE_URL":  "postgres://u:p#a$s@db:5432/e?sslmode=require&x=1",
		"ESI_CONTACT":   "Pilot Name <me@example.com>",
		"ADDR":          "127.0.0.1:8080",
		"RANVIA_KEY":    "rnv_new",
	}
	out := applyEnvEdits(template, values)
	got := parseEnv(out)
	for k, want := range values {
		if got[k] != want {
			t.Errorf("%s reads back as %q, want %q", k, got[k], want)
		}
	}
	if !strings.Contains(out, "# Comment kept") || !strings.Contains(out, "# The address\nESI_CONTACT=") {
		t.Errorf("comments or line placement were not kept:\n%s", out)
	}
	if strings.Count(out, "\nADDR=") != 1 {
		t.Errorf("a repeated key stayed active:\n%s", out)
	}
	cleared := applyEnvEdits("EVE_CLIENT_SECRET=hunter2\n", map[string]string{"EVE_CLIENT_SECRET": ""})
	if strings.Contains(cleared, "hunter2") {
		t.Errorf("clearing a secret left it in the file: %q", cleared)
	}
}

func TestBoltFramesKeepOneSize(t *testing.T) {
	for _, rows := range []int{8, 12, 16} {
		want := boltCols(rows)
		for f := 0; f < boltFrameCount; f++ {
			lines := renderBolt(rows, f)
			if len(lines) != rows {
				t.Fatalf("rows %d frame %d has %d lines", rows, f, len(lines))
			}
			for _, l := range lines {
				if got := len([]rune(stripANSI(l))); got != want {
					t.Fatalf("rows %d frame %d line is %d wide, want %d", rows, f, got, want)
				}
			}
		}
	}
}

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func stripANSI(s string) string { return ansiRE.ReplaceAllString(s, "") }
