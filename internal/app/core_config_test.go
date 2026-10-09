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

	// A numeric prefix with trailing junk is not an ID: the old
	// Sscanf %d parse read "123abc" as 123 and granted the wrong
	// character.
	set = parseAdminCharIDs("123abc, 45xyz, 0x12, +77")
	if len(set) != 0 {
		t.Fatalf("expected no IDs from %q, got %v", "123abc, 45xyz, 0x12, +77", set)
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

func TestParseDBConns(t *testing.T) {
	for raw, want := range map[string]int{"": 0, " ": 0, "40": 40, " 60 ": 60, "2": minDBConns, "99999": mostDBConns, "plenty": 0, "0": 0, "-4": 0} {
		if got := parseDBConns(raw); got != want {
			t.Errorf("parseDBConns(%q) = %d, want %d", raw, got, want)
		}
	}
}
