package app

// Global notices (notices.go): only an administrator reaches the
// Notices tab and its actions, and publishing is rate-limited.

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestNoticeRoutesAreAdminOnly(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	grantTestAdmin(app, fixtureCharA)
	ctx := context.Background()
	other, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	seedCharacter(t, q, other.ID, fixtureMember, "Plain Member")
	asOther := sessionCookie(t, app, other.ID, fixtureMember, "Plain Member")
	anonymous := &http.Cookie{Name: "evesynapse_session", Value: "none", Path: "/"}

	form := url.Values{"title": {"Hello"}, "severity": {"info"}, "ends": {time.Now().UTC().Add(time.Hour).Format(noticeEndsLayout)}, "id": {"1"}, "body": {"x"}}
	for _, who := range []struct {
		name   string
		cookie *http.Cookie
	}{{"a non-admin account", asOther}, {"an anonymous visitor", anonymous}} {
		if code, _ := getPage(t, app, who.cookie, "/admin/notices/"); code != http.StatusForbidden {
			t.Errorf("GET /admin/notices/ from %s: status %d, want 403", who.name, code)
		}
		for _, path := range []string{"/admin/notices/save", "/admin/notices/retire", "/admin/notices/preview"} {
			if code, _ := postForm(t, app, who.cookie, path, form); code != http.StatusForbidden {
				t.Errorf("POST %s from %s: status %d, want 403", path, who.name, code)
			}
		}
	}
	var n int
	if err := app.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM global_notices`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("notices after refused requests: %d (%v), want 0", n, err)
	}
}

func TestNoticePublishIsRateLimited(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	grantTestAdmin(app, fixtureCharA)
	ctx := context.Background()
	admin, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create admin account: %v", err)
	}
	seedCharacter(t, q, admin.ID, fixtureCharA, "Admin Main")
	cookie := sessionCookie(t, app, admin.ID, fixtureCharA, "Admin Main")

	ends := time.Now().UTC().Add(2 * time.Hour).Format(noticeEndsLayout)
	count := func() int {
		var n int
		if err := app.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM global_notices`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// Each one is retired straight after, so the live cap is not
	// what stops the last.
	for i := 1; i <= noticePublishPerAccount; i++ {
		form := url.Values{"title": {"Notice " + strconv.Itoa(i)}, "severity": {"info"}, "ends": {ends}}
		if code, _ := postForm(t, app, cookie, "/admin/notices/save", form); code != http.StatusSeeOther {
			t.Fatalf("publish %d: status %d", i, code)
		}
		if got := count(); got != i {
			t.Fatalf("after publish %d: %d notices", i, got)
		}
		var id int64
		if err := app.db.QueryRowContext(ctx, `SELECT MAX(id) FROM global_notices`).Scan(&id); err != nil {
			t.Fatal(err)
		}
		postForm(t, app, cookie, "/admin/notices/retire", url.Values{"id": {strconv.FormatInt(id, 10)}})
	}
	form := url.Values{"title": {"One too many"}, "severity": {"info"}, "ends": {ends}}
	postForm(t, app, cookie, "/admin/notices/save", form)
	if got := count(); got != noticePublishPerAccount {
		t.Fatalf("publish past the hourly limit was saved: %d notices", got)
	}
	_, body := getPage(t, app, cookie, "/admin/notices/")
	if !strings.Contains(body, "the most allowed") {
		t.Error("the refused publish left no message saying why")
	}
}
