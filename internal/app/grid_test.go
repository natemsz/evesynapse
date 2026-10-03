package app

// Tests for the home grid engine: the packing solver (both
// column modes), the layout v1→v2 storage formats, the span
// action, and the span classes the server renders. Same hermetic
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

// gridItems builds layout items from a shorthand: a widget id,
// with a trailing "!" marking the wide span preference.
func gridItems(spec ...string) []homeLayoutItem {
	items := make([]homeLayoutItem, len(spec))
	for i, s := range spec {
		item := homeLayoutItem{ID: strings.TrimSuffix(s, "!")}
		if strings.HasSuffix(s, "!") {
			item.Span = spanWide
		}
		items[i] = item
	}
	return items
}

// TestSolveHomeSpans pins the one packing rule, at both column
// counts the CSS serves: 6 (desktop) and 2 (phone).
func TestSolveHomeSpans(t *testing.T) {
	cases := []struct {
		name  string
		items []homeLayoutItem
		want6 []int
		want2 []int
	}{
		{"empty", gridItems(), []int{}, []int{}},
		{"lone flex stretches", gridItems("server"), []int{6}, []int{2}},
		{"flex pair halves", gridItems("market", "skills"), []int{3, 3}, []int{1, 1}},
		{"third flex drops to its own stretched row", gridItems("market", "skills", "server"), []int{3, 3, 6}, []int{1, 1, 2}},
		{"flex stranded before a full row stretches", gridItems("market", "fleet", "skills"), []int{6, 6, 6}, []int{2, 2, 2}},
		{"full then lone flex", gridItems("fleet", "market"), []int{6, 6}, []int{2, 2}},
		{"wide flex owns its row", gridItems("market!"), []int{6}, []int{2}},
		{"wide flex breaks the pair around it", gridItems("market", "skills!", "server"), []int{6, 6, 6}, []int{2, 2, 2}},
		{"two pairs around a full row", gridItems("market", "skills", "fleet", "server", "networth"), []int{3, 3, 6, 3, 3}, []int{1, 1, 2, 1, 1}},
		{"two fulls", gridItems("fleet", "attention"), []int{6, 6}, []int{2, 2}},
	}
	for _, tc := range cases {
		if got := solveHomeSpans(tc.items, 6); !reflect.DeepEqual(got, tc.want6) {
			t.Errorf("%s: solve(6) = %v, want %v", tc.name, got, tc.want6)
		}
		if got := solveHomeSpans(tc.items, 2); !reflect.DeepEqual(got, tc.want2) {
			t.Errorf("%s: solve(2) = %v, want %v", tc.name, got, tc.want2)
		}
	}
}

// TestParseHomeLayoutFormats: the v1 id array every old build
// wrote loads identically to the v2 object array, encoding
// always lands on v2, and a v2→parse round trip is stable.
func TestParseHomeLayoutFormats(t *testing.T) {
	v1 := parseHomeLayout(`["fleet","market"]`)
	v2 := parseHomeLayout(`[{"id":"fleet"},{"id":"market"}]`)
	if !reflect.DeepEqual(v1, v2) {
		t.Fatalf("v1 %v and v2 %v layouts differ", v1, v2)
	}
	if got := encodeHomeLayout(v1); got != `[{"id":"fleet"},{"id":"market"}]` {
		t.Fatalf("encode(v1-parsed) = %q, want canonical v2", got)
	}

	mixed := gridItems("fleet", "market!", "skills")
	if got := encodeHomeLayout(mixed); got != `[{"id":"fleet"},{"id":"market","span":"wide"},{"id":"skills"}]` {
		t.Fatalf("encode(mixed) = %q", got)
	}
	if back := parseHomeLayout(encodeHomeLayout(mixed)); !reflect.DeepEqual(back, mixed) {
		t.Fatalf("encode→parse round trip: got %v, want %v", back, mixed)
	}
	if got := encodeHomeLayout(nil); got != `[]` {
		t.Fatalf("encode(nil) = %q, want []", got)
	}
}

// TestParseHomeLayoutNormalization: corrupted entries drop out
// one by one (unknown ids, dupes, junk scalars, invalid spans)
// instead of failing the whole layout; unparseable storage and
// empty storage fall back to the default, and a saved empty
// array still means "everything off".
func TestParseHomeLayoutNormalization(t *testing.T) {
	if got := parseHomeLayout(`["bogus","fleet","fleet","nope"]`); !reflect.DeepEqual(got, gridItems("fleet")) {
		t.Errorf("unknown/dupe: got %v", got)
	}
	if got := parseHomeLayout(`[{"id":"market","span":"huge"}]`); !reflect.DeepEqual(got, gridItems("market")) {
		t.Errorf("invalid span should drop back to auto: got %v", got)
	}
	if got := parseHomeLayout(`[42,null,{"span":"wide"},{"id":"skills"},true]`); !reflect.DeepEqual(got, gridItems("skills")) {
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

// TestHomeGridRenderSpans: the server-rendered classes match the
// solver for representative layouts — seeding through the v1
// storage format proves old users' layouts survive untouched.
func TestHomeGridRenderSpans(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")

	setLayout := func(raw string) {
		t.Helper()
		if err := q.SetUserHomeLayout(ctx, db.SetUserHomeLayoutParams{HomeLayout: raw, ID: user.ID}); err != nil {
			t.Fatalf("seed layout: %v", err)
		}
	}

	// A lone flex module (v1 seed) stretches its whole row.
	setLayout(`["server"]`)
	code, body := getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET /: status %d", code)
	}
	mustContain(t, "/ lone flex", body,
		`<section class="card span6" data-widget="server" data-size="flex" data-span="">`)

	// Two flex modules pair at half width.
	setLayout(`["market","skills"]`)
	code, body = getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET /: status %d", code)
	}
	mustContain(t, "/ flex pair", body,
		`<section class="card span3" data-widget="market"`,
		`<section class="card span3" data-widget="skills"`)

	// A wide-marked flex module owns its row, and the flex
	// module after it is stranded alone — so it stretches too.
	setLayout(`[{"id":"market","span":"wide"},{"id":"skills"}]`)
	code, body = getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET /: status %d", code)
	}
	mustContain(t, "/ wide flex", body,
		`<section class="card span6" data-widget="market" data-size="flex" data-span="wide">`,
		`<section class="card span6" data-widget="skills"`)

	// The full-width pair stays full, flex modules after pair up.
	setLayout(`["fleet","attention","networth","market","server"]`)
	code, body = getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET /: status %d", code)
	}
	mustContain(t, "/ default-ish", body,
		`<section class="card span6" data-widget="fleet" data-size="full"`,
		`<section class="card span6" data-widget="attention" data-size="full"`,
		`<section class="card span3" data-widget="networth"`,
		`<section class="card span3" data-widget="market"`,
		`<section class="card span6" data-widget="server"`)

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handlers made %d outbound calls, want 0", got)
	}
}

// TestHomeLayoutSpanAction: the span save behind the resize
// toggle (and its no-JS form equivalent) — wide persists as a
// v2 entry and renders, auto clears it, invalid values never
// reach storage, and anonymous POSTs still bounce.
func TestHomeLayoutSpanAction(t *testing.T) {
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

	// Wide, as a plain form POST (the no-JS path): 303, and the
	// preference persists in v2 form.
	code, _ := doLayoutPost(t, app, url.Values{
		"action": {"span"}, "widget": {widgetMarket}, "span": {"wide"},
	}, cookie, false)
	if code != http.StatusSeeOther {
		t.Fatalf("form span POST: status %d, want 303", code)
	}
	if got := saved(); !strings.Contains(got, `{"id":"market","span":"wide"}`) {
		t.Fatalf("saved layout = %q, want market persisted wide", got)
	}
	code, body := getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET /: status %d", code)
	}
	mustContain(t, "/ after span wide", body,
		`<section class="card span6" data-widget="market" data-size="flex" data-span="wide">`)

	// Back to auto, as the resize toggle's XHR: bare 200.
	code, body = doLayoutPost(t, app, url.Values{
		"action": {"span"}, "widget": {widgetMarket}, "span": {"auto"},
	}, cookie, true)
	if code != http.StatusOK || body != `{"ok":true}` {
		t.Fatalf("XHR span POST: status %d body %q, want 200 ok", code, body)
	}
	if got := saved(); strings.Contains(got, `"span":"wide"`) || !strings.Contains(got, `{"id":"market"}`) {
		t.Fatalf("saved layout = %q, want market back to auto", got)
	}

	// An invalid span value is not a layout change.
	before := saved()
	code, _ = doLayoutPost(t, app, url.Values{
		"action": {"span"}, "widget": {widgetMarket}, "span": {"enormous"},
	}, cookie, true)
	if code != http.StatusOK {
		t.Fatalf("XHR bad-span POST: status %d", code)
	}
	if got := saved(); got != before {
		t.Fatalf("bad span value changed storage: %q -> %q", before, got)
	}

	// Span on a module that is not on the home: no-op, and the
	// module does not appear.
	if err := q.SetUserHomeLayout(ctx, db.SetUserHomeLayoutParams{HomeLayout: `[{"id":"fleet"}]`, ID: user.ID}); err != nil {
		t.Fatalf("seed layout: %v", err)
	}
	code, _ = doLayoutPost(t, app, url.Values{
		"action": {"span"}, "widget": {widgetMarket}, "span": {"wide"},
	}, cookie, true)
	if code != http.StatusOK {
		t.Fatalf("XHR span POST (not on home): status %d", code)
	}
	if got := saved(); got != `[{"id":"fleet"}]` {
		t.Fatalf("saved layout = %q, want fleet only", got)
	}

	// Anonymous span POSTs bounce like every layout action.
	code, _, _ = doReq(t, app, http.MethodPost, "/home/layout",
		url.Values{"action": {"span"}, "widget": {widgetMarket}, "span": {"wide"}})
	if code != http.StatusSeeOther {
		t.Fatalf("anonymous span POST: status %d, want 303", code)
	}

	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("handlers made %d outbound calls, want 0", got)
	}
}
