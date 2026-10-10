package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// lookupFixture is a site with an administrator and two other
// accounts, three characters between them, two with similar names.
type lookupFixture struct {
	app    *Application
	q      *db.Queries
	cookie *http.Cookie
	admin  db.User
	other  db.User
}

func newLookupFixture(t *testing.T) *lookupFixture {
	t.Helper()
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	admin, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	other, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seedCharacter(t, q, admin.ID, fixtureCharA, "Fixture Alpha")
	seedCharacter(t, q, other.ID, fixtureCharB, "Zed Hauler")
	seedCharacter(t, q, other.ID, 90000003, "Zed Hauler II")
	grantTestAdmin(app, fixtureCharA)
	seedSnapshot(t, q, fixtureCharB, esi.SnapSkills, esi.Skills{})
	return &lookupFixture{app: app, q: q, admin: admin, other: other,
		cookie: sessionCookie(t, app, admin.ID, fixtureCharA, "Fixture Alpha")}
}

// newFormRequest posts a form and hands back the whole answer, for the
// tests that need where it redirects to.
func newFormRequest(t *testing.T, app *Application, cookie *http.Cookie, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	return rec
}

// TestSyncShowsOneCharacter: the Sync page no longer prints every
// character. It opens on the reader's own, and any other is found by
// name or id through the search box; text that fits several lists them
// to pick from.
func TestSyncShowsOneCharacter(t *testing.T) {
	f := newLookupFixture(t)

	code, body := getPage(t, f.app, f.cookie, "/sync/")
	if code != http.StatusOK {
		t.Fatalf("GET /sync/: %d", code)
	}
	mustContain(t, "/sync/", body, "<h2>Worker</h2>", "ESI error budget: not reported yet", "<h2>Character data</h2>",
		`<input type="search" name="character" id="sync-character-q"`, `<ul class="suggest" id="sync-character-suggest"`,
		"<h3>Fixture Alpha</h3>", `<a href="/admin/?account=`, "Re-warm Fixture Alpha now")
	if strings.Contains(body, "Zed Hauler") {
		t.Fatal("the Sync page shows a character nobody asked for")
	}

	// By exact name, though another name starts the same way.
	_, body = getPage(t, f.app, f.cookie, "/sync/?character="+url.QueryEscape("zed hauler"))
	mustContain(t, "by exact name", body, "<h3>Zed Hauler</h3>", "1 fresh,", `value="Zed Hauler"`)
	if strings.Contains(body, "<h3>Fixture Alpha</h3>") || strings.Contains(body, "Which one?") {
		t.Fatal("an exact name did not go straight to that character alone")
	}
	// By id.
	_, body = getPage(t, f.app, f.cookie, "/sync/?character=90000003")
	mustContain(t, "by id", body, "<h3>Zed Hauler II</h3>")
	// Part of a name that fits two: a list, and nobody's data.
	_, body = getPage(t, f.app, f.cookie, "/sync/?character=zed")
	mustContain(t, "ambiguous", body, "Which one?", `<a href="/sync/?character=90000002">Zed Hauler</a>`, `<a href="/sync/?character=90000003">Zed Hauler II</a>`)
	if strings.Contains(body, "<h3>") && strings.Contains(body, "<h3>Zed") {
		t.Fatal("an ambiguous search showed a character's data")
	}
	// Nobody.
	_, body = getPage(t, f.app, f.cookie, "/sync/?character=nobody+at+all")
	mustContain(t, "not found", body, "No character is called that")
	// What LIKE gives a meaning to is matched as typed.
	_, body = getPage(t, f.app, f.cookie, "/sync/?character="+url.QueryEscape("%%"))
	mustContain(t, "wildcards", body, "No character is called that")

	// Re-warming one character works on anyone's, and comes back to it.
	f.app.takePriorityCharacters()
	req := newFormRequest(t, f.app, f.cookie, "/sync/warm?character=90000003", url.Values{})
	if req.Code != http.StatusSeeOther || req.Header().Get("Location") != "/sync/?character=90000003" {
		t.Fatalf("warm one: %d to %q", req.Code, req.Header().Get("Location"))
	}
	if ids := f.app.takePriorityCharacters(); len(ids) != 1 || ids[0] != 90000003 {
		t.Fatalf("warm one put %v first, want 90000003", ids)
	}
	// With no character it is the reader's own account, not everyone.
	newFormRequest(t, f.app, f.cookie, "/sync/warm", url.Values{})
	if ids := f.app.takePriorityCharacters(); len(ids) != 1 || ids[0] != fixtureCharA {
		t.Fatalf("warm mine put %v first, want only the reader's character", ids)
	}
}

// TestCharacterSuggest: the search boxes' suggestions are names
// containing what was typed, an exact one first, each with its
// account; they are for administrators only.
func TestCharacterSuggest(t *testing.T) {
	f := newLookupFixture(t)
	code, body := getPage(t, f.app, f.cookie, "/admin/suggest?q=zed")
	if code != http.StatusOK {
		t.Fatalf("suggest: %d", code)
	}
	if i, j := strings.Index(body, `"name":"Zed Hauler"`), strings.Index(body, `"name":"Zed Hauler II"`); i < 0 || j < 0 || i > j {
		t.Fatalf("suggestions %s; want both haulers, the shorter name first", body)
	}
	mustContain(t, "suggest", body, `"id":90000002`, `"label":"account `)
	if _, body = getPage(t, f.app, f.cookie, "/admin/suggest?q=hauler+ii"); !strings.Contains(body, "Zed Hauler II") || strings.Contains(body, `"name":"Zed Hauler"`) {
		t.Fatalf("a narrower search: %s", body)
	}
	for _, q := range []string{"", "z", "%25", "_"} {
		if _, body = getPage(t, f.app, f.cookie, "/admin/suggest?q="+q); strings.TrimSpace(body) != "[]" {
			t.Errorf("suggest %q: %s, want nothing", q, body)
		}
	}
	asOther := sessionCookie(t, f.app, f.other.ID, fixtureCharB, "Zed Hauler")
	if code, _ := getPage(t, f.app, asOther, "/admin/suggest?q=fixture"); code != http.StatusForbidden {
		t.Fatalf("suggest as a non-admin: %d, want 403", code)
	}
}

// TestAdminShowsOneAccount: the Admin page gives the totals and the
// newest accounts, and one account in full only when it is looked up.
// Nobody's scopes or datasets are printed unasked.
func TestAdminShowsOneAccount(t *testing.T) {
	f := newLookupFixture(t)
	code, body := getPage(t, f.app, f.cookie, "/admin/")
	if code != http.StatusOK {
		t.Fatalf("GET /admin/: %d", code)
	}
	mustContain(t, "/admin/", body, "<h2>Overview</h2>", "<tr><td>Accounts</td><td>2</td></tr>", "<tr><td>Linked characters</td><td>3</td></tr>",
		"<h2>Find an account</h2>", `<input type="search" name="q" id="admin-q"`, "<h2>Newest accounts</h2>", `href="/sync/"`)
	for _, gone := range []string{"Zed Hauler", "Fixture Alpha</td>", "esi-", "Snapshot cache", "Background jobs"} {
		if strings.Contains(body, gone) {
			t.Errorf("the Admin page prints %q without being asked", gone)
		}
	}

	if _, err := f.app.db.ExecContext(context.Background(), `UPDATE characters SET scopes = 'esi-skills.read_skills.v1 esi-wallet.read_character_wallet.v1' WHERE character_id = 90000003`); err != nil {
		t.Fatal(err)
	}
	_, body = getPage(t, f.app, f.cookie, "/admin/?q=90000003")
	mustContain(t, "an account", body, "<h3>Account ", "<strong>Zed Hauler II</strong>", "<td>Zed Hauler</td>",
		`<a href="/sync/?character=90000002">Sync</a>`,
		// What a character granted is there, folded away until asked for.
		`<details class="scope-details"><summary>2 scopes</summary>`, "<li>esi-wallet.read_character_wallet.v1</li>", "none recorded")
	if strings.Contains(body, "Fixture Alpha</td>") {
		t.Fatal("another account's character is listed with this one")
	}
	// A parked character says why.
	if err := f.q.SetCharacterLinkState(context.Background(), db.SetCharacterLinkStateParams{
		CharacterID: fixtureCharB, LinkState: linkStateTokenDead, LinkStateAt: timeSet(time.Now()),
	}); err != nil {
		t.Fatal(err)
	}
	_, body = getPage(t, f.app, f.cookie, "/admin/?account="+strconv.FormatInt(f.other.ID, 10))
	mustContain(t, "by account id", body, "No: its EVE sign-in has expired or was revoked", "<tr><td>Not syncing (have to sign in again)</td><td><strong>1</strong></td></tr>")
	_, body = getPage(t, f.app, f.cookie, "/admin/?account=999999")
	mustContain(t, "no such account", body, "No account has that id.")
}

// TestWorkerFiguresAsRows: the worker's figures are also given a row
// each, which is how the Sync page sets them out.
func TestWorkerFiguresAsRows(t *testing.T) {
	at := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	one := workerTiming{
		At: at, Took: 5700 * time.Millisecond, Characters: 9, Active: 1, Watched: 7, Recent: 1, Fetches: 72, Budget: 960, Overdue: 59 * time.Second,
		Phases:  []phaseTiming{{"characters", 5100 * time.Millisecond}, {"names", 300 * time.Millisecond}, {"ordering", time.Millisecond}},
		Budgets: []esi.RateHeadroom{{Group: "char-killmail", Remaining: 27, Limit: "30/15m"}},
	}
	v := workerTimingViewFor(workerStatus{Timing: one, Recent: []workerTiming{one, one}})
	row := func(rows []statRow, label string) string {
		for _, r := range rows {
			if r.Label == label {
				return r.Value
			}
		}
		return "(no such row)"
	}
	for label, want := range map[string]string{
		"Took": "5.7s", "Characters": "9: 1 active, 7 watched, 1 recent, 0 dormant",
		"Fetches": "72 of 960 allowed", "Stalest active data": "59s past its refresh time",
	} {
		if got := row(v.LastRows, label); got != want {
			t.Errorf("last cycle, %s: %q, want %q", label, got, want)
		}
	}
	if v.HourTitle != "Last 2 cycles" || row(v.HourRows, "Fetches a cycle") != "average 72.0, most 72" ||
		row(v.HourRows, "Characters on average") != "1.0 active, 7.0 watched, 1.0 recent, 0.0 dormant" {
		t.Errorf("recent cycles: %q %+v", v.HourTitle, v.HourRows)
	}
	if len(v.PhaseRows) != 2 || v.PhaseRows[0] != (statRow{"characters", "5.1s"}) {
		t.Errorf("phases: %+v; want the two that took time, longest first", v.PhaseRows)
	}
	if len(v.BudgetRows) != 1 || v.BudgetRows[0] != (budgetRow{Group: "char-killmail", Left: 27, Limit: "30/15m"}) {
		t.Errorf("budgets: %+v", v.BudgetRows)
	}
}

func TestErrorBudgetWords(t *testing.T) {
	now := time.Now()
	for want, got := range map[string]string{
		"not reported yet":                 errorBudgetWords(0, 0),
		"full (the last window has reset)": errorBudgetWords(40, now.Add(-time.Minute).Unix()),
		"87 errors left, resets in":        errorBudgetWords(87, now.Add(30*time.Second).Unix()),
	} {
		if !strings.HasPrefix(got, want) {
			t.Errorf("got %q, want it to start %q", got, want)
		}
	}
}
