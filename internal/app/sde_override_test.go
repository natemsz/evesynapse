package app

import (
	"archive/zip"
	"bytes"
	"testing"
)

// TestSDEArchiveReadsDumpFilesFromAZip pins how an uploaded dump is
// read: dump files are found under any folder, a .bz2 name maps to
// its plain name, other entries are ignored, and a zip without any
// dump file is refused.
func TestSDEArchiveReadsDumpFilesFromAZip(t *testing.T) {
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

	data := build(map[string]string{
		"dump/invCategories.csv": "categoryID,categoryName,iconID,published\n6,Ship,,1\n",
		"dump/README.txt":        "notes",
		"invGroups.csv.bz2":      "not opened in this test",
	})
	arc, err := openSDEArchive(data)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got := arc.names(); len(got) != 2 || got[0] != "invCategories.csv" || got[1] != "invGroups.csv" {
		t.Fatalf("names = %v, want [invCategories.csv invGroups.csv]", got)
	}
	parsed := &parsedSDE{markers: map[string]fileMarker{}}
	found, err := arc.parse("invCategories.csv", parsed)
	if !found || err != nil {
		t.Fatalf("parse categories: found %v, err %v", found, err)
	}
	if len(parsed.categories) != 1 {
		t.Fatalf("categories = %d, want 1", len(parsed.categories))
	}
	if found, _ := arc.parse("invTypes.csv", parsed); found {
		t.Fatal("invTypes.csv reported found in a zip without it")
	}

	if _, err := openSDEArchive(build(map[string]string{"notes.txt": "x"})); err == nil {
		t.Fatal("a zip without dump files was accepted")
	}
	if _, err := openSDEArchive([]byte("plain text")); err == nil {
		t.Fatal("a file that is not a zip was accepted")
	}
}
