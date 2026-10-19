package appraise

import (
	"strings"
	"testing"
)

func TestAnonHistoryCookieSigning(t *testing.T) {
	secret := []byte("test-secret")
	tok := strings.Repeat("a", 48)
	value := signAnonTokens(secret, []string{tok})
	got := verifyAnonTokens(secret, value)
	if len(got) != 1 || got[0] != tok {
		t.Fatalf("verify = %v, want the signed token", got)
	}
	// An unsigned token, a tampered payload, and a signature
	// from another secret all verify as no history.
	if got := verifyAnonTokens(secret, tok); len(got) != 0 {
		t.Errorf("unsigned token verified as history: %v", got)
	}
	if got := verifyAnonTokens(secret, strings.Repeat("b", 48)+value[len(tok):]); len(got) != 0 {
		t.Errorf("tampered payload verified as history: %v", got)
	}
	if got := verifyAnonTokens([]byte("other-secret"), value); len(got) != 0 {
		t.Errorf("foreign signature verified as history: %v", got)
	}
	if got := verifyAnonTokens(secret, "not-a-cookie"); len(got) != 0 {
		t.Errorf("garbage verified as history: %v", got)
	}
}

func TestSnapshotHasItemLines(t *testing.T) {
	if snapshotHasItemLines(nil) || snapshotHasItemLines(&snapshot{}) {
		t.Error("an empty snapshot must not be storable")
	}
	if !snapshotHasItemLines(&snapshot{Lines: []snapLine{{TypeID: 34}}}) {
		t.Error("a snapshot with a resolved line must be storable")
	}
}
