package dotenv

import (
	"os"
	"path/filepath"
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

	Load(path)

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
	Load(filepath.Join(t.TempDir(), "missing.env"))
}
