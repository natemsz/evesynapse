package app

// Anonymous Item Browser: /items/ serves logged-out traffic while
// the rest of /items/ stays behind the login. Market browse data
// is shared by nature, so the anonymous render depends on no
// session or character state.

import (
	"net/http"
	"testing"
)

func TestItemsBrowserAnonymous(t *testing.T) {
	app, conn, _ := buildCorpTestApp(t, &countingTransport{})
	seedNext1SDE(t, conn)

	// Top-level browse renders real content with no session.
	code, body := getSwapPage(t, app, nil, "/items/")
	if code != http.StatusOK {
		t.Fatalf("GET /items/ (anonymous): status %d, want 200", code)
	}
	mustContain(t, "/items/ (anonymous)", body,
		`<main id="main-content">`,
		"Item Database",
		"Fixture Category",
		`<a href="/items/category/900/">`,
	)

	// Search mode is browse data too.
	code, body = getSwapPage(t, app, nil, "/items/?q=tritanium")
	if code != http.StatusOK {
		t.Fatalf("GET /items/?q=tritanium (anonymous): status %d, want 200", code)
	}
	mustContain(t, "/items/?q=tritanium (anonymous)", body,
		"Tritanium",
		"Fixture Minerals",
	)

	// Everything else under /items/ still bounces logged-out
	// visitors home.
	for _, path := range []string{
		"/items/category/900/",
		"/items/group/910/",
		"/items/type/34/",
		"/items/search.json?q=tri",
		"/items/description-fragment?type=34",
	} {
		code, _ := getSwapPage(t, app, nil, path)
		if code != http.StatusSeeOther {
			t.Errorf("GET %s (anonymous): status %d, want 303", path, code)
		}
	}
}
