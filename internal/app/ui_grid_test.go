package app

// Tests for the home grid: module sizes (catalog defaults and
// stored sizes), the layout storage formats (v1 ids, v2 spans, v3
// sizes), the size action, and the classes the server renders. Same hermetic
// contract as the overview suite — seeded snapshots, counting
// transport, zero outbound calls.

import (
	"context"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	db "evesynapse/internal/db/sqlc"
)

// TestParseHomeLayoutFormats: every stored format loads. v1 ids and
// v2 objects without a span read as default sizes; a v2 wide span
// reads as the full grid width; v3 sizes load as stored, held to the
// grid; encoding always writes v3, omitting default sizes.
func TestParseHomeLayoutFormats(t *testing.T) {
	v1 := parseHomeLayout(`["fleet","market"]`)
	v2 := parseHomeLayout(`[{"id":"fleet"},{"id":"market"}]`)
	if !reflect.DeepEqual(v1, v2) {
		t.Fatalf("v1 %v and v2 %v layouts differ", v1, v2)
	}
	if got := encodeHomeLayout(v1); got != `[{"id":"fleet"},{"id":"market"}]` {
		t.Fatalf("encode(v1-parsed) = %q", got)
	}
	wide := parseHomeLayout(`[{"id":"market","span":"wide"}]`)
	if want := []homeLayoutItem{{ID: "market", W: 3}}; !reflect.DeepEqual(wide, want) {
		t.Fatalf("v2 wide = %v, want %v", wide, want)
	}
	v3 := parseHomeLayout(`[{"id":"market","w":2,"h":3},{"id":"skills","w":9,"h":-1}]`)
	if want := []homeLayoutItem{{ID: "market", W: 2, H: 3}, {ID: "skills", W: 3}}; !reflect.DeepEqual(v3, want) {
		t.Fatalf("v3 = %v, want %v (sizes held to 1-3, unset left unset)", v3, want)
	}
	if got := encodeHomeLayout(v3); got != `[{"id":"market","w":2,"h":3},{"id":"skills","w":3}]` {
		t.Fatalf("encode(v3) = %q", got)
	}
	if got := encodeHomeLayout(nil); got != `[]` {
		t.Fatalf("encode(nil) = %q, want []", got)
	}
}

// TestWidgetSizeDefaults: every catalog module has a default size
// inside the grid, and a stored size overrides it per axis.
func TestWidgetSizeDefaults(t *testing.T) {
	for _, def := range homeWidgetCatalog {
		if def.W < 1 || def.W > homeGridMax || def.H < 1 || def.H > homeGridMax {
			t.Errorf("widget %q default size %dx%d is outside the grid", def.ID, def.W, def.H)
		}
	}
	if w, h := widgetSize(homeLayoutItem{ID: widgetFleet}); w != 3 || h != 2 {
		t.Errorf("fleet default = %dx%d, want 3x2", w, h)
	}
	if w, h := widgetSize(homeLayoutItem{ID: widgetFleet, H: 3}); w != 3 || h != 3 {
		t.Errorf("fleet with a stored height = %dx%d, want 3x3", w, h)
	}
	if homeCap(1) >= homeCap(2) || homeCap(2) >= homeCap(3) {
		t.Errorf("taller modules must show more: caps %d, %d, %d", homeCap(1), homeCap(2), homeCap(3))
	}
}

// TestParseHomeLayoutNormalization: corrupted entries drop out one
// by one; unparseable and empty storage fall back to the default,
// and a saved empty array still means "everything off".
func TestParseHomeLayoutNormalization(t *testing.T) {
	if got := parseHomeLayout(`["bogus","fleet","fleet","nope"]`); !reflect.DeepEqual(got, []homeLayoutItem{{ID: "fleet"}}) {
		t.Errorf("unknown/dupe: got %v", got)
	}
	if got := parseHomeLayout(`[42,null,{"span":"wide"},{"id":"skills"},true]`); !reflect.DeepEqual(got, []homeLayoutItem{{ID: "skills"}}) {
		t.Errorf("junk entries: got %v", got)
	}
	if got := parseHomeLayout(`{"id":"fleet"}`); !reflect.DeepEqual(got, defaultHomeLayoutItems()) {
		t.Errorf("non-array storage: got %v, want default", got)
	}
	if got := parseHomeLayout(`not json at all`); !reflect.DeepEqual(got, defaultHomeLayoutItems()) {
		t.Errorf("garbage storage: got %v, want default", got)
	}
	if got := parseHomeLayout(""); !reflect.DeepEqual(got, defaultHomeLayoutItems()) {
		t.Errorf("empty storage: got %v, want default", got)
	}
	if got := parseHomeLayout(`[]`); len(got) != 0 {
		t.Errorf("saved empty array: got %v, want everything off", got)
	}
}

// TestHomeGridSizes: the size action stores a size, the page renders
// it as grid classes, a taller module shows more rows before More,
// and bad or off-home sizes change nothing.
func TestHomeGridSizes(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")
	saved := func() string {
		t.Helper()
		raw, err := q.GetUserHomeLayout(ctx, user.ID)
		if err != nil {
			t.Fatalf("read layout: %v", err)
		}
		return raw
	}
	if err := q.SetUserHomeLayout(ctx, db.SetUserHomeLayoutParams{HomeLayout: `[{"id":"fleet"},{"id":"market","span":"wide"},{"id":"skills"}]`, ID: user.ID}); err != nil {
		t.Fatalf("seed layout: %v", err)
	}
	_, body := getPage(t, app, cookie, "/")
	mustContain(t, "/ defaults", body,
		`<section class="card gw3 gh2" data-widget="fleet" data-gw="3" data-gh="2">`,
		// An old wide span is the full width now.
		`<section class="card gw3 gh1" data-widget="market" data-gw="3" data-gh="1">`,
		`<section class="card gw1 gh1" data-widget="skills" data-gw="1" data-gh="1">`)

	// Resize, as the picker's XHR: stored, rendered.
	code, resp := doLayoutPost(t, app, url.Values{"action": {"size"}, "widget": {widgetSkills}, "w": {"2"}, "h": {"3"}}, cookie, true)
	if code != http.StatusOK || resp != `{"ok":true}` {
		t.Fatalf("size POST: status %d body %q", code, resp)
	}
	if got := saved(); !strings.Contains(got, `{"id":"skills","w":2,"h":3}`) {
		t.Fatalf("saved layout = %q, want skills 2x3", got)
	}
	_, body = getPage(t, app, cookie, "/")
	mustContain(t, "/ resized", body, `<section class="card gw2 gh3" data-widget="skills" data-gw="2" data-gh="3">`)

	// Out of the grid, unparsable, or not on the home: no change.
	before := saved()
	for _, v := range []url.Values{
		{"action": {"size"}, "widget": {widgetSkills}, "w": {"4"}, "h": {"1"}},
		{"action": {"size"}, "widget": {widgetSkills}, "w": {"1"}, "h": {"0"}},
		{"action": {"size"}, "widget": {widgetSkills}, "w": {"x"}, "h": {"1"}},
		{"action": {"size"}, "widget": {widgetPI}, "w": {"2"}, "h": {"2"}},
	} {
		if code, _ := doLayoutPost(t, app, v, cookie, true); code != http.StatusOK {
			t.Fatalf("size POST %v: status %d", v, code)
		}
	}
	if got := saved(); got != before {
		t.Fatalf("invalid sizes changed storage: %q -> %q", before, got)
	}
	// The no-JS form path redirects back to Customize.
	if code, _ := doLayoutPost(t, app, url.Values{"action": {"size"}, "widget": {widgetSkills}, "w": {"1"}, "h": {"1"}}, cookie, false); code != http.StatusSeeOther {
		t.Fatalf("form size POST: status %d, want 303", code)
	}
	// Anonymous POSTs bounce like every layout action.
	if code, _, _ := doReq(t, app, http.MethodPost, "/home/layout", url.Values{"action": {"size"}, "widget": {widgetSkills}, "w": {"2"}, "h": {"2"}}); code != http.StatusSeeOther {
		t.Fatalf("anonymous size POST: status %d, want 303", code)
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handlers made %d outbound calls, want 0", got)
	}
}
