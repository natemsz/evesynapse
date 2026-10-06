package app

import "testing"

// Issues 23/24: admin is tied to specific EVE SSO character IDs
// via EVE_ADMIN_CHARACTER_IDS.
func TestParseAdminCharIDs(t *testing.T) {
	set := parseAdminCharIDs("12345, 67890")
	if !set[12345] || !set[67890] {
		t.Fatalf("expected both IDs in set, got %v", set)
	}
	if len(set) != 2 {
		t.Fatalf("expected 2 IDs, got %v", set)
	}

	// Malformed and empty entries are skipped.
	set = parseAdminCharIDs("12345,,abc,0,-7, 999 ")
	if !set[12345] || !set[999] {
		t.Fatalf("expected valid IDs kept, got %v", set)
	}
	if len(set) != 2 {
		t.Fatalf("expected 2 valid IDs, got %v", set)
	}

	// Empty input: nobody is admin.
	if len(parseAdminCharIDs("")) != 0 {
		t.Fatal("empty input should yield empty set")
	}
}

func TestIsAdminCharacter(t *testing.T) {
	cfg := Config{adminCharIDs: parseAdminCharIDs("12345")}
	if !cfg.IsAdminCharacter(12345) {
		t.Fatal("12345 should be admin")
	}
	if cfg.IsAdminCharacter(99999) {
		t.Fatal("99999 should not be admin")
	}
	if cfg.IsAdminCharacter(0) {
		t.Fatal("0 should not be admin")
	}
}
