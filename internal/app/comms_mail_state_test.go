package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// TestMarkMailReadLocally: after ESI accepts a mark-as-read, the
// stored copies the mail page renders from show it read: the header
// flag, the label badges (one fewer unread, on the mail's own labels
// only), the total, and the open mail's body snapshot. A mail that
// was already read changes no counts, and an unknown mail changes
// nothing.
func TestMarkMailReadLocally(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")

	seedSnapshot(t, q, fixtureCharA, esi.SnapMail, esi.MailHeaders{
		{MailID: 11, Subject: "unread in inbox", IsRead: false, Labels: []int64{1}},
		{MailID: 12, Subject: "unread in inbox and corp", IsRead: false, Labels: []int64{1, 4}},
		{MailID: 13, Subject: "already read", IsRead: true, Labels: []int64{1}},
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapMailLabels, esi.MailLabels{
		TotalUnreadCount: 2,
		Labels: []esi.MailLabel{
			{LabelID: 1, Name: "Inbox", UnreadCount: 2},
			{LabelID: 4, Name: "Corp", UnreadCount: 1},
			{LabelID: 8, Name: "Other", UnreadCount: 5},
		},
	})
	seedSnapshot(t, q, fixtureCharA, esi.MailBodyKind(12), esi.Mail{Subject: "unread in inbox and corp", Read: false})

	headers := func() map[int64]bool {
		var h esi.MailHeaders
		if !app.loadCorpSnapshot(ctx, fixtureCharA, esi.SnapMail, &h) {
			t.Fatal("mail headers snapshot is gone")
		}
		read := map[int64]bool{}
		for _, m := range h {
			read[m.MailID] = m.IsRead
		}
		return read
	}
	labelCounts := func() (int64, map[int64]int64) {
		var l esi.MailLabels
		if !app.loadCorpSnapshot(ctx, fixtureCharA, esi.SnapMailLabels, &l) {
			t.Fatal("mail labels snapshot is gone")
		}
		counts := map[int64]int64{}
		for _, lab := range l.Labels {
			counts[lab.LabelID] = lab.UnreadCount
		}
		return l.TotalUnreadCount, counts
	}

	app.markMailReadLocally(ctx, fixtureCharA, 12)
	if got := headers(); !got[12] || got[11] || !got[13] {
		t.Fatalf("headers read flags = %v, want only 12 changed (13 was already read)", got)
	}
	total, counts := labelCounts()
	if total != 1 || counts[1] != 1 || counts[4] != 0 || counts[8] != 5 {
		t.Fatalf("labels after marking 12 read: total %d, counts %v; want total 1, inbox 1, corp 0, other 5", total, counts)
	}
	var body esi.Mail
	if !app.loadCorpSnapshot(ctx, fixtureCharA, esi.MailBodyKind(12), &body) || !body.Read {
		t.Fatalf("open mail's body snapshot not marked read: %+v", body)
	}

	// Already read: nothing to count down.
	app.markMailReadLocally(ctx, fixtureCharA, 13)
	if total, counts := labelCounts(); total != 1 || counts[1] != 1 {
		t.Fatalf("marking an already-read mail changed counts: total %d, %v", total, counts)
	}
	// Marking the same mail twice must not count down twice.
	app.markMailReadLocally(ctx, fixtureCharA, 12)
	if total, counts := labelCounts(); total != 1 || counts[1] != 1 || counts[4] != 0 {
		t.Fatalf("marking 12 twice counted down twice: total %d, %v", total, counts)
	}
	// Unknown mail: no change, no panic.
	app.markMailReadLocally(ctx, fixtureCharA, 999)
	if got := headers(); got[11] {
		t.Fatalf("unknown mail marked something read: %v", got)
	}
}

// TestEsiRefusalDetail: what the reader is told when ESI refuses a
// write names the status and ESI's own words, and says nothing for a
// failure that was not an ESI answer.
func TestEsiRefusalDetail(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{&esi.StatusError{Method: "POST", Path: "/x/", Code: http.StatusBadRequest, Detail: "recipient is invalid"}, " (HTTP 400: recipient is invalid)"},
		{&esi.StatusError{Method: "POST", Path: "/x/", Code: http.StatusBadGateway}, " (HTTP 502)"},
		{errors.New("dial tcp: connection refused"), ""},
	}
	for _, c := range cases {
		if got := esiRefusalDetail(c.err); got != c.want {
			t.Errorf("esiRefusalDetail(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

// grantScopes seeds (or re-seeds) a character holding exactly these
// ESI scopes.
func grantScopes(t *testing.T, q *db.Queries, userID, characterID int64, name, scopes string) {
	t.Helper()
	if _, err := q.UpsertCharacter(context.Background(), db.UpsertCharacterParams{
		CharacterID: characterID, UserID: userID, Name: name,
		AccessToken: "fixture", RefreshToken: "fixture",
		TokenExpiry: mustNullTime("2999-01-01T00:00:00Z"),
		Scopes:      scopes, LinkState: "ok",
	}); err != nil {
		t.Fatalf("seed character %d: %v", characterID, err)
	}
}

// markReadTransport accepts the mark-as-read PUT and records it.
type markReadTransport struct {
	mu   sync.Mutex
	puts []string
}

func (s *markReadTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodPut {
		s.mu.Lock()
		s.puts = append(s.puts, req.URL.Path)
		s.mu.Unlock()
		return &http.Response{StatusCode: http.StatusNoContent, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
	}
	return &http.Response{StatusCode: http.StatusInternalServerError, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":"unexpected"}`)), Request: req}, nil
}

// TestMailActionsNeedTheirScopes: a character that never granted a
// mail scope gets a plain "sign in again" answer up front, with no
// ESI call made, and the compose page and the open mail offer the
// sign-in instead of a form that cannot work. With the scope, mark
// as read goes through and the mail list shows it read at once.
func TestMailActionsNeedTheirScopes(t *testing.T) {
	transport := &markReadTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha") // no mail scopes
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")
	seedSnapshot(t, q, fixtureCharA, esi.SnapMail, esi.MailHeaders{
		{MailID: 12, Subject: "Hello there", IsRead: false, Labels: []int64{1}},
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapMailLabels, esi.MailLabels{TotalUnreadCount: 1, Labels: []esi.MailLabel{{LabelID: 1, Name: "Inbox", UnreadCount: 1}}})
	seedSnapshot(t, q, fixtureCharA, esi.MailBodyKind(12), esi.Mail{Subject: "Hello there", Body: "hi", From: 93300001})

	// Without the scopes.
	code, body := getPage(t, app, cookie, "/mail/compose/")
	if code != http.StatusOK {
		t.Fatalf("compose page: status %d", code)
	}
	mustContain(t, "/mail/compose/", body, "Sign in again")
	if strings.Contains(body, `action="/mail/send/"`) {
		t.Error("compose page offers a send form to a character that cannot send")
	}
	code, page := postForm(t, app, cookie, "/mail/send/", url.Values{
		"character": {"90000001"}, "to": {"Anyone"}, "subject": {"s"}, "body": {"b"},
	})
	if code != http.StatusOK {
		t.Fatalf("send without scope: status %d", code)
	}
	mustContain(t, "/mail/send/", page, "linked before EveSynapse asked for permission to send mail")
	code, page = postForm(t, app, cookie, "/mail/read/", url.Values{"character": {"90000001"}, "mail": {"12"}})
	if code != http.StatusForbidden || !strings.Contains(page, "permission to mark mail read") {
		t.Fatalf("mark read without scope: %d %q", code, page)
	}
	_, body = getPage(t, app, cookie, "/mail/?character=90000001&mail=12")
	mustContain(t, "/mail/ (open mail)", body, "Sign in again to mark mail read")

	// With the organize scope: ESI gets the PUT and the list shows it read.
	grantScopes(t, q, user.ID, fixtureCharA, "Fixture Alpha", mailOrganizeScope)
	_, body = getPage(t, app, cookie, "/mail/?character=90000001&mail=12")
	mustContain(t, "/mail/ (open mail)", body, "Mark as read")
	req := httptest.NewRequest(http.MethodPost, "/mail/read/", strings.NewReader(url.Values{"character": {"90000001"}, "mail": {"12"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("mark read: status %d, body %q", rec.Code, rec.Body.String())
	}
	transport.mu.Lock()
	puts := append([]string(nil), transport.puts...)
	transport.mu.Unlock()
	if len(puts) != 1 || puts[0] != "/characters/90000001/mail/12/" {
		t.Fatalf("ESI PUTs = %v, want one for mail 12", puts)
	}
	var headers esi.MailHeaders
	if !app.loadCorpSnapshot(ctx, fixtureCharA, esi.SnapMail, &headers) || len(headers) != 1 || !headers[0].IsRead {
		t.Fatalf("stored mail list after mark read: %+v", headers)
	}
	var labels esi.MailLabels
	if !app.loadCorpSnapshot(ctx, fixtureCharA, esi.SnapMailLabels, &labels) || labels.TotalUnreadCount != 0 || labels.Labels[0].UnreadCount != 0 {
		t.Fatalf("stored label counts after mark read: %+v", labels)
	}
}
