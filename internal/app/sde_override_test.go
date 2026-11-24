package app

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"
)

// TestSDEArchiveReadsEitherDumpFormat pins how an uploaded dump is
// read: JSON Lines tables (in the SDE API's shape, or with the "_key"
// ids CCP's zip writes) and CSV files are found under any folder and
// other entries are ignored; a CSV dump must hold every file the
// import needs, a .bz2 name maps to its plain name, and a zip mixing
// both formats, holding no dump file, or not a zip at all is refused.
func TestSDEArchiveReadsEitherDumpFormat(t *testing.T) {
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
	if arc.format != sdeFormatJSONL {
		t.Fatalf("format = %q, want %q", arc.format, sdeFormatJSONL)
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

	csvDump := map[string]string{"dump/README.txt": "notes"}
	for _, name := range sdeFileNames {
		csvDump["dump/"+name] = "x\n"
	}
	csvDump["dump/invGroups.csv.bz2"] = csvDump["dump/invGroups.csv"]
	delete(csvDump, "dump/invGroups.csv")
	csvDump["dump/invCategories.csv"] = "categoryID,categoryName,iconID,published\n6,Ship,,1\n"
	arc, err = openSDEArchive(build(csvDump))
	if err != nil {
		t.Fatalf("open CSV zip: %v", err)
	}
	if arc.format != sdeFormatCSV || len(arc.files) != len(sdeFileNames) || arc.files["invGroups.csv"] == nil {
		t.Fatalf("CSV zip read as %q with %d files, want %q with %d including invGroups.csv", arc.format, len(arc.files), sdeFormatCSV, len(sdeFileNames))
	}
	parsed = &parsedSDE{}
	if found, err := arc.parse("invCategories.csv", parsed); !found || err != nil || len(parsed.categories) != 1 {
		t.Fatalf("parse invCategories.csv: found %v, err %v, %d rows", found, err, len(parsed.categories))
	}

	delete(csvDump, "dump/invTypes.csv")
	if _, err := openSDEArchive(build(csvDump)); err == nil || !strings.Contains(err.Error(), "invTypes.csv") {
		t.Fatalf("a CSV dump without invTypes.csv: err %v, want one naming the missing file", err)
	}
	if _, err := openSDEArchive(build(map[string]string{"invTypes.csv": "x", "types.jsonl": "{}"})); err == nil {
		t.Fatal("a zip mixing CSV files and JSON Lines tables was accepted")
	}
	if _, err := openSDEArchive(build(map[string]string{"notes.txt": "x"})); err == nil {
		t.Fatal("a zip without dump files was accepted")
	}
	if _, err := openSDEArchive([]byte("plain text")); err == nil {
		t.Fatal("a file that is not a zip was accepted")
	}
}
