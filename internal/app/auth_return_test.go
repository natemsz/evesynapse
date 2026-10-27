package app

import "testing"

func TestSafeReturnPath(t *testing.T) {
	for p, want := range map[string]bool{
		"/intel/workbench/": true, "/intel/workbench/12/?x=1": true, "/": true,
		"": false, "//evil.example/": false, `/\evil.example`: false, "https://evil.example/": false, "intel/": false,
	} {
		if got := safeReturnPath(p); got != want {
			t.Errorf("safeReturnPath(%q) = %v, want %v", p, got, want)
		}
	}
}
