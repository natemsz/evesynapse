package app

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"
)

// TestSDEArchiveReadsJSONLinesTables pins how an uploaded dump is
// read: JSON Lines tables (in the SDE API's shape, or with the "_key"
// ids CCP's zip writes) are found under any folder and other entries
// are ignored; a zip holding CSV files, no table, or not a zip at all
// is refused.
func TestSDEArchiveReadsJSONLinesTables(t *testing.T) {
	build := func(files map[string]string) []byte {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		for name, body := range files {
			w, err := zw.Create(name)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}

	arc, err := openSDEArchive(build(map[string]string{
		"eve-online-static-data/categories.jsonl": `{"_key":6,"name":{"en":"Ship","de":"Schiff"},"published":true}` + "\n",
		"eve-online-static-data/_sde.jsonl":       `{"_key":"sde"}` + "\n",
		"README.txt":                              "notes",
	}))
	if err != nil {
		t.Fatalf("open JSON Lines zip: %v", err)
	}
	if got := arc.names(); len(got) != 1 || got[0] != "categories.jsonl" {
		t.Fatalf("names = %v, want [categories.jsonl]", got)
	}
	parsed := &parsedSDE{}
	if found, err := arc.parse("categories", parsed); !found || err != nil {
		t.Fatalf("parse categories: found %v, err %v", found, err)
	}
	if len(parsed.categories) != 1 || parsed.categories[0].categoryID != 6 || parsed.categories[0].name != "Ship" {
		t.Fatalf("categories = %+v, want one named Ship", parsed.categories)
	}
	if found, _ := arc.parse("types", parsed); found {
		t.Fatal("types reported found in a zip without it")
	}

	for _, name := range []string{"dump/invTypes.csv", "dump/invGroups.csv.bz2"} {
		_, err := openSDEArchive(build(map[string]string{name: "x", "types.jsonl": "{}"}))
		if err == nil || !strings.Contains(err.Error(), "CSV dumps are not imported") {
			t.Fatalf("a zip holding %s: err %v, want the CSV refusal", name, err)
		}
	}
	if _, err := openSDEArchive(build(map[string]string{"notes.txt": "x"})); err == nil {
		t.Fatal("a zip without dump files was accepted")
	}
	if _, err := openSDEArchive([]byte("plain text")); err == nil {
		t.Fatal("a file that is not a zip was accepted")
	}
}
