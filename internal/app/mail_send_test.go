package app

// Tests for sending mail (POST /mail/send/). ESI answers a sent
// mail with 201 and the new mail's ID as a bare number; the handler
// must report that as sent. Reading the answer as an object made it
// report every sent mail as refused, which invites a second send.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// mailSendTransport plays the two ESI endpoints a send uses: the
// public name lookup and the character's mail endpoint.
type mailSendTransport struct {
	mailStatus int
	mailBody   string

	mu       sync.Mutex
	payloads []string // bodies of the mail POSTs received
	tokens   []string // their Authorization headers
}

func (s *mailSendTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	respond := func(code int, body string) (*http.Response, error) {
		return &http.Response{
			StatusCode: code,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	}
	raw, _ := io.ReadAll(req.Body)
	switch {
	case req.Method == http.MethodPost && req.URL.Path == "/universe/ids/":
		if strings.Contains(string(raw), "Member One") {
			return respond(http.StatusOK, `{"characters":[{"id":93300001,"name":"Member One"}]}`)
		}
		return respond(http.StatusOK, `{}`)
	case req.Method == http.MethodPost && req.URL.Path == "/characters/90000001/mail/":
		s.mu.Lock()
		s.payloads = append(s.payloads, string(raw))
		s.tokens = append(s.tokens, req.Header.Get("Authorization"))
		s.mu.Unlock()
		return respond(s.mailStatus, s.mailBody)
	}
	return respond(http.StatusInternalServerError, `{"error":"unexpected request"}`)
}

func (s *mailSendTransport) sent() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.payloads...)
}

func TestMailSend(t *testing.T) {
	transport := &mailSendTransport{mailStatus: http.StatusCreated, mailBody: `424242`}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	grantScopes(t, q, user.ID, fixtureCharA, "Fixture Alpha", mailSendScope)
	stranger, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create second user: %v", err)
	}
	seedCharacter(t, q, stranger.ID, fixtureCharB, "Someone Else")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")

	send := func(character, to, subject, body string) string {
		t.Helper()
		code, page := postForm(t, app, cookie, "/mail/send/", url.Values{
			"character": {character}, "to": {to}, "subject": {subject}, "body": {body},
		})
		if code != http.StatusOK {
			t.Fatalf("POST /mail/send/: status %d", code)
		}
		return page
	}

	// Sent: ESI's 201 with a bare mail ID is success.
	page := send("90000001", "Member One", "Fleet tonight", "Form up at 19:00.")
	mustContain(t, "/mail/send/ (sent)", page, "Mail sent.")
	if strings.Contains(page, "EVE refused the mail") {
		t.Fatal("a mail ESI accepted was reported as refused")
	}
	sent := transport.sent()
	if len(sent) != 1 {
		t.Fatalf("%d mail(s) posted to ESI, want exactly 1", len(sent))
	}
	var payload struct {
		Subject    string `json:"subject"`
		Body       string `json:"body"`
		Recipients []struct {
			ID   int64  `json:"recipient_id"`
			Type string `json:"recipient_type"`
		} `json:"recipients"`
	}
	if err := json.Unmarshal([]byte(sent[0]), &payload); err != nil {
		t.Fatalf("mail payload %q: %v", sent[0], err)
	}
	if payload.Subject != "Fleet tonight" || payload.Body != "Form up at 19:00." ||
		len(payload.Recipients) != 1 || payload.Recipients[0].ID != 93300001 || payload.Recipients[0].Type != "character" {
		t.Fatalf("mail payload = %+v", payload)
	}
	if transport.tokens[0] != "Bearer fixture" {
		t.Fatalf("mail was posted with Authorization %q, want the sender's token", transport.tokens[0])
	}

	// Refused for a missing scope: the form comes back saying so,
	// with what was typed still in it.
	transport.mailStatus, transport.mailBody = http.StatusForbidden, `{"error":"token is not valid for scope"}`
	page = send("90000001", "Member One", "Second try", "Still there?")
	mustContain(t, "/mail/send/ (no scope)", page, "sign in again", "Second try", "Still there?")
	if strings.Contains(page, "Mail sent.") {
		t.Fatal("a refused mail was reported as sent")
	}

	// Nothing reaches ESI for a recipient that does not exist, a
	// missing field, or a character that is someone else's.
	before := len(transport.sent())
	mustContain(t, "/mail/send/ (unknown recipient)", send("90000001", "Nobody By That Name", "s", "b"), "No character, corporation, or alliance named")
	mustContain(t, "/mail/send/ (no subject)", send("90000001", "Member One", "", "b"), "Enter a subject.")
	mustContain(t, "/mail/send/ (not yours)", send("90000002", "Member One", "s", "b"), "isn&#39;t one of yours")
	if after := len(transport.sent()); after != before {
		t.Fatalf("%d mail(s) were posted for requests that should have been stopped", after-before)
	}
}
