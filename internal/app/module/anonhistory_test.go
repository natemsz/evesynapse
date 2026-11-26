package module

import (
	"strings"
	"testing"
)

func TestTokenHistoryCookieSigning(t *testing.T) {
	secret := []byte("test-secret")
	tok := strings.Repeat("a", 48)
	value := SignTokens(secret, []string{tok})
	got := VerifyTokens(secret, value)
	if len(got) != 1 || got[0] != tok {
		t.Fatalf("verify = %v, want the signed token", got)
	}
	// An unsigned token, a tampered payload, and a signature
	// from another secret all verify as no history.
	if got := VerifyTokens(secret, tok); len(got) != 0 {
		t.Errorf("unsigned token verified as history: %v", got)
	}
	if got := VerifyTokens(secret, strings.Repeat("b", 48)+value[len(tok):]); len(got) != 0 {
		t.Errorf("tampered payload verified as history: %v", got)
	}
	if got := VerifyTokens([]byte("other-secret"), value); len(got) != 0 {
		t.Errorf("foreign signature verified as history: %v", got)
	}
	if got := VerifyTokens(secret, "not-a-cookie"); len(got) != 0 {
		t.Errorf("garbage verified as history: %v", got)
	}
}
