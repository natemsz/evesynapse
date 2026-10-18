package app

import "testing"

// TestSafeLocalNext: a redirect target stays on the site only when
// it is a path under a single leading slash — scheme-relative and
// backslash forms leave the site in a browser, and CR/LF would
// break out of the Location header.
func TestSafeLocalNext(t *testing.T) {
	for _, n := range []string{
		"/ok",
		"/",
		"/a/b?x=1",
		"/market/?type=34",
	} {
		if !safeLocalNext(n) {
			t.Errorf("safeLocalNext(%q) = false, want true", n)
		}
	}
	for _, n := range []string{
		"",
		"https://evil.com",
		"//evil.com",
		"/\\evil.com",
		"/\\\\evil.com",
		"javascript:alert(1)",
		"ok",
		"/ok\r\nLocation: https://evil.com",
		"/ok\n",
	} {
		if safeLocalNext(n) {
			t.Errorf("safeLocalNext(%q) = true, want false", n)
		}
	}
}
