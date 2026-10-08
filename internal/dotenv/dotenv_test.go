package dotenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadFillsGapsOnly: every accepted line shape sets its key, the
// lines that are not settings are skipped, and a key the real
// environment already has is left alone.
func TestLoadFillsGapsOnly(t *testing.T) {
	const file = `# a comment, then a blank line

DOTENV_TEST_PLAIN=one
export DOTENV_TEST_EXPORTED=two
DOTENV_TEST_DOUBLE="three three"
DOTENV_TEST_SINGLE='four'
  DOTENV_TEST_SPACED  =  five
DOTENV_TEST_EMPTY=
DOTENV_TEST_PRESENT=from-the-file
this line is not a setting
=no key
`
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"DOTENV_TEST_PLAIN":    "one",
		"DOTENV_TEST_EXPORTED": "two",
		"DOTENV_TEST_DOUBLE":   "three three",
		"DOTENV_TEST_SINGLE":   "four",
		"DOTENV_TEST_SPACED":   "five",
		"DOTENV_TEST_EMPTY":    "",
		"DOTENV_TEST_PRESENT":  "from-the-environment",
	}
	for key := range want {
		// Load sets variables for the whole process: start from a
		// clean slate and leave one behind.
		os.Unsetenv(key)
		t.Cleanup(func() { os.Unsetenv(key) })
	}
	t.Setenv("DOTENV_TEST_PRESENT", "from-the-environment")

	if err := Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}

	for key, value := range want {
		got, present := os.LookupEnv(key)
		if !present || got != value {
			t.Errorf("%s = %q (set: %v), want %q", key, got, present, value)
		}
	}
}

// TestLoadWithoutAFile: no .env file is the normal case on a box
// configured through its service unit, and is not an error.
func TestLoadWithoutAFile(t *testing.T) {
	if err := Load(filepath.Join(t.TempDir(), "missing.env")); err != nil {
		t.Fatalf("Load of a missing file: %v", err)
	}
}

// TestLoadUnreadable: a .env that exists but cannot be read is an
// error, not a silent fall back to defaults. A directory in place
// of the file reads as one on every platform (no permission games
// needed).
func TestLoadUnreadable(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".env"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Load(filepath.Join(dir, ".env")); err == nil {
		t.Fatal("Load of an unreadable .env succeeded, want an error")
	}
}

// TestLoadLongLine: a value past the scanner's 64KB default still
// loads, with the lines after it. The old loader silently dropped
// the file from the long line on.
func TestLoadLongLine(t *testing.T) {
	key := "DOTENV_TEST_LONG"
	os.Unsetenv(key)
	t.Cleanup(func() { os.Unsetenv(key) })
	os.Unsetenv("DOTENV_TEST_AFTER")
	t.Cleanup(func() { os.Unsetenv("DOTENV_TEST_AFTER") })

	file := key + "=" + strings.Repeat("v", 200*1024) + "\nDOTENV_TEST_AFTER=yes\n"
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := os.Getenv(key); len(got) != 200*1024 {
		t.Fatalf("long value loaded %d bytes, want %d", len(got), 200*1024)
	}
	if os.Getenv("DOTENV_TEST_AFTER") != "yes" {
		t.Fatal("the line after a long value did not load")
	}
}
