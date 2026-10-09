package app

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"evesynapse/internal/esi"
)

// calendarAnswerESI takes the PUT that answers an event and records
// its path and body. status 0 answers 204.
type calendarAnswerESI struct {
	mu     sync.Mutex
	status int
	puts   []string
}

func (s *calendarAnswerESI) RoundTrip(req *http.Request) (*http.Response, error) {
	respond := func(code int, body string) (*http.Response, error) {
		return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	}
	if req.Method != http.MethodPut {
		return respond(http.StatusInternalServerError, `{"error":"unexpected"}`)
	}
	body, _ := io.ReadAll(req.Body)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.puts = append(s.puts, req.URL.Path+" "+string(body))
	if s.status != 0 {
		return respond(s.status, `{"error":"event is over"}`)
	}
	return respond(http.StatusNoContent, "")
}

func (s *calendarAnswerESI) sent() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.puts...)
}

// TestCalendarEventsCanBeAnswered: an open in-game event offers the
// three answers to a character that granted the scope and a sign-in
// to one that did not; an answer goes to EVE and shows on the page at
// once; nobody answers for another account's character.
func TestCalendarEventsCanBeAnswered(t *testing.T) {
	transport := &calendarAnswerESI{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha") // no calendar scopes
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")
	at := time.Now().UTC().Add(48 * time.Hour).Format(time.RFC3339)
	seedSnapshot(t, q, fixtureCharA, esi.SnapCalendar, esi.CalendarEventSummaries{
		{EventID: 77, EventDate: at, Title: "Moon mining", EventResponse: "not_responded"},
		{EventID: 78, EventDate: at, Title: "Something else", EventResponse: "not_responded"},
	})
	seedSnapshot(t, q, fixtureCharA, esi.CalendarEventKind(77), esi.CalendarEvent{EventID: 77, Date: at, Title: "Moon mining", Response: "not_responded"})
	seedSnapshot(t, q, fixtureCharA, esi.CalendarAttendeesKind(77), esi.CalendarAttendees{
		{CharacterID: fixtureCharA, EventResponse: "not_responded"},
		{CharacterID: 95000001, EventResponse: "declined"},
	})
	form := func(answer string) url.Values {
		return url.Values{"character": {"90000001"}, "event": {"77"}, "response": {answer}}
	}
	const open = "/calendar/?character=90000001&event=77"

	// Without the scope: a sign-in, no form, and no call to EVE.
	_, body := getPage(t, app, cookie, open)
	mustContain(t, "event without the scope", body, `href="/auth/eve?character=90000001&amp;module=calendar"`, "Sign in again to answer events")
	if strings.Contains(body, `action="/calendar/respond"`) {
		t.Error("an answer form is offered to a character that cannot answer")
	}
	if code, page := postForm(t, app, cookie, "/calendar/respond", form("accepted")); code != http.StatusForbidden || !strings.Contains(page, "permission to answer calendar events") {
		t.Fatalf("answer without the scope: %d %q", code, page)
	}

	// With it: the three answers, none marked yet.
	grantScopes(t, q, user.ID, fixtureCharA, "Fixture Alpha", "esi-calendar.read_calendar_events.v1 "+calendarRespondScope)
	_, body = getPage(t, app, cookie, open)
	mustContain(t, "event with the scope", body, `action="/calendar/respond"`,
		`value="accepted" required> Accept`, `value="tentative" required> Tentative`, `value="declined" required> Decline`)

	// Anything but the three answers is refused before EVE is asked.
	for _, bad := range []string{"", "not_responded", "yes"} {
		if code, _ := postForm(t, app, cookie, "/calendar/respond", form(bad)); code != http.StatusBadRequest {
			t.Errorf("answer %q: status %d, want 400", bad, code)
		}
	}
	// Another account cannot answer for this character.
	stranger, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seedCharacter(t, q, stranger.ID, fixtureCharB, "Fixture Beta")
	if code, _ := postForm(t, app, sessionCookie(t, app, stranger.ID, fixtureCharB, "Fixture Beta"), "/calendar/respond", form("accepted")); code != http.StatusForbidden {
		t.Errorf("another account's answer: status %d, want 403", code)
	}
	if sent := transport.sent(); len(sent) != 0 {
		t.Fatalf("EVE was asked before any valid answer: %v", sent)
	}

	// A real answer: one PUT, and the stored copies say so at once.
	answer := form("tentative")
	answer.Set("month", "2026-10")
	code, _ := postForm(t, app, cookie, "/calendar/respond", answer)
	if code != http.StatusSeeOther {
		t.Fatalf("answer: status %d, want a redirect", code)
	}
	if sent := transport.sent(); len(sent) != 1 || sent[0] != `/characters/90000001/calendar/77/ {"response":"tentative"}` {
		t.Fatalf("EVE was sent %v", sent)
	}
	var events esi.CalendarEventSummaries
	if !app.loadCorpSnapshot(ctx, fixtureCharA, esi.SnapCalendar, &events) || events[0].EventResponse != "tentative" || events[1].EventResponse != "not_responded" {
		t.Fatalf("stored events after answering: %+v", events)
	}
	var attendees esi.CalendarAttendees
	if !app.loadCorpSnapshot(ctx, fixtureCharA, esi.CalendarAttendeesKind(77), &attendees) || attendees[0].EventResponse != "tentative" || attendees[1].EventResponse != "declined" {
		t.Fatalf("stored attendees after answering: %+v", attendees)
	}
	_, body = getPage(t, app, cookie, open)
	mustContain(t, "event after answering", body, `value="tentative" checked required> Tentative`)
	if strings.Contains(body, `value="accepted" checked`) {
		t.Error("the wrong answer is marked")
	}

	// EVE refuses: said plainly, and the stored answer is left alone.
	transport.status = http.StatusBadRequest
	if code, page := postForm(t, app, cookie, "/calendar/respond", form("declined")); code != http.StatusBadGateway || !strings.Contains(page, "EVE refused the answer") {
		t.Fatalf("refused answer: %d %q", code, page)
	}
	if !app.loadCorpSnapshot(ctx, fixtureCharA, esi.SnapCalendar, &events) || events[0].EventResponse != "tentative" {
		t.Fatalf("a refused answer was stored: %+v", events)
	}
}
