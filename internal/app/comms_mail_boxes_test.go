package app

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"evesynapse/internal/esi"
)

// TestMailBoxViews: the mail list separates what the character
// received from what he sent. ESI has no sent folder: the snapshot
// holds his own copies of list mail he sent, sender = himself and
// flagged unread, so direction is derived from the sender. The
// landing view is the inbox (received only); the Sent view shows
// his mail alone, never bold, with the recipients in place of the
// sender; label chips filter within whichever box is open.
func TestMailBoxViews(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")

	seedSnapshot(t, q, fixtureCharA, esi.SnapMailLabels, esi.MailLabels{
		TotalUnreadCount: 2,
		Labels:           []esi.MailLabel{{LabelID: 4, Name: "Corp", UnreadCount: 2}},
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapMailLists, esi.MailLists{
		{MailingListID: 555, Name: "Corp List"},
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapMail, esi.MailHeaders{
		{MailID: 21, From: 93300001, Subject: "Received unread", IsRead: false, Labels: []int64{4}, Timestamp: "2026-10-17T20:00:00Z"},
		{MailID: 22, From: fixtureCharA, Subject: "Own sent list mail", IsRead: false, Labels: []int64{4}, Timestamp: "2026-10-17T21:00:00Z",
			Recipients: []esi.MailRecipient{{RecipientID: 555, RecipientType: "mailing_list"}}},
		{MailID: 23, From: 93300001, Subject: "Received read", IsRead: true, Timestamp: "2026-10-16T09:00:00Z"},
	})

	mustNotContain := func(path, body string, wants ...string) {
		t.Helper()
		for _, want := range wants {
			if strings.Contains(body, want) {
				t.Errorf("GET %s: body unexpectedly contains %q", path, want)
			}
		}
	}

	// Landing (no box param) is the inbox: received mail only,
	// the received unread one bold, the Inbox chip active.
	code, body := getPage(t, app, cookie, "/mail/?character=90000001")
	if code != http.StatusOK {
		t.Fatalf("mail page: status %d", code)
	}
	mustContain(t, "/mail/", body,
		"Received unread", "Received read",
		`<strong><a href="/mail/?character=90000001&amp;box=inbox&amp;mail=21">Received unread</a></strong>`,
		`<a class="chip active" href="/mail/?character=90000001&amp;box=inbox">Inbox</a>`,
	)
	mustNotContain("/mail/", body, "Own sent list mail")

	// Sent view: only his own mail, rendered read (never bold
	// despite the snapshot's unread flag), recipients in the
	// second column, SENT marker on the row.
	code, body = getPage(t, app, cookie, "/mail/?character=90000001&box=sent")
	if code != http.StatusOK {
		t.Fatalf("sent view: status %d", code)
	}
	mustContain(t, "/mail/?box=sent", body,
		"Own sent list mail", "Corp List", "<th>To</th>",
		`<span class="badge wait">SENT</span>`,
		`<a class="chip active" href="/mail/?character=90000001&amp;box=sent">Sent</a>`,
		`box=sent&amp;label=4`,
	)
	mustNotContain("/mail/?box=sent", body,
		"Received unread", "Received read",
		`<strong><a href="/mail/?character=90000001&amp;box=sent&amp;mail=22">`,
	)

	// All mail: everything, the sent row still not bold and still
	// carrying its SENT marker; the received unread stays bold.
	code, body = getPage(t, app, cookie, "/mail/?character=90000001&box=all")
	if code != http.StatusOK {
		t.Fatalf("all view: status %d", code)
	}
	mustContain(t, "/mail/?box=all", body,
		"Received unread", "Own sent list mail", "Received read",
		`<strong><a href="/mail/?character=90000001&amp;box=all&amp;mail=21">Received unread</a></strong>`,
		`<span class="badge wait">SENT</span>`,
		`<a class="chip active" href="/mail/?character=90000001&amp;box=all">All mail</a>`,
	)
	mustNotContain("/mail/?box=all", body,
		`<strong><a href="/mail/?character=90000001&amp;box=all&amp;mail=22">`,
	)

	// Label chips compose with the box filter: the label narrows
	// whichever box is open, it does not replace it.
	_, body = getPage(t, app, cookie, "/mail/?character=90000001&box=all&label=4")
	mustContain(t, "/mail/?box=all&label=4", body, "Received unread", "Own sent list mail")
	mustNotContain("/mail/?box=all&label=4", body, "Received read")

	_, body = getPage(t, app, cookie, "/mail/?character=90000001&label=4")
	mustContain(t, "/mail/?label=4", body, "Received unread")
	mustNotContain("/mail/?label=4", body, "Own sent list mail", "Received read")

	_, body = getPage(t, app, cookie, "/mail/?character=90000001&box=sent&label=4")
	mustContain(t, "/mail/?box=sent&label=4", body, "Own sent list mail")
	mustNotContain("/mail/?box=sent&label=4", body, "Received unread", "Received read")
}
