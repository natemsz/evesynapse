package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Answering in-game calendar events (POST /calendar/respond): accept,
// tentative or decline, sent to EVE with PUT
// /characters/{id}/calendar/{event_id}/, which needs the
// esi-calendar.respond_calendar_events.v1 scope. A character linked
// before that scope was asked for is offered a sign-in instead of a
// form that cannot work.
// ---------------------------------------------------------------------------

// calendarAnswer is one of the answers EVE takes.
type calendarAnswer struct {
	Value    string // what ESI calls it
	Label    string
	Selected bool
}

// calendarAnswers is the choice offered on an event, with the
// character's current answer marked.
func calendarAnswers(current string) []calendarAnswer {
	out := []calendarAnswer{
		{Value: "accepted", Label: "Accept"},
		{Value: "tentative", Label: "Tentative"},
		{Value: "declined", Label: "Decline"},
	}
	for i := range out {
		out[i].Selected = out[i].Value == current
	}
	return out
}

func validCalendarAnswer(v string) bool {
	return v == "accepted" || v == "tentative" || v == "declined"
}

// calendarEventURL is the calendar page with one event open.
func calendarEventURL(characterID int64, month string, eventID int64) string {
	q := url.Values{}
	q.Set("character", strconv.FormatInt(characterID, 10))
	if month != "" {
		q.Set("month", month)
	}
	q.Set("event", strconv.FormatInt(eventID, 10))
	return "/calendar/?" + q.Encode()
}

func (app *Application) handleCalendarRespond(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	charID, _ := strconv.ParseInt(r.Form.Get("character"), 10, 64)
	eventID, _ := strconv.ParseInt(r.Form.Get("event"), 10, 64)
	answer := r.Form.Get("response")
	if charID <= 0 || eventID <= 0 || !validCalendarAnswer(answer) {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	ch, err := app.queries.GetCharacter(ctx, charID)
	if err != nil || ch.UserID != userID {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	if !characterHasScope(ch, calendarRespondScope) {
		http.Error(w, mailRelinkNotice(ch.Name, "answer calendar events"), http.StatusForbidden)
		return
	}
	token, err := app.validAccessToken(ctx, ch)
	if err != nil {
		logging.Errorf("calendar respond: token for character %d: %v", charID, err)
		http.Error(w, "Could not reach EVE. Sign in again if it keeps failing.", http.StatusBadGateway)
		return
	}
	path := fmt.Sprintf("/characters/%d/calendar/%d/", charID, eventID)
	if err := app.esi.PutJSONAuthed(ctx, token, path, map[string]string{"response": answer}); err != nil {
		var se *esi.StatusError
		if errors.As(err, &se) && se.Code == http.StatusForbidden {
			http.Error(w, mailRelinkNotice(ch.Name, "answer calendar events"), http.StatusForbidden)
			return
		}
		logging.Errorf("calendar respond: ESI PUT %s: %v", path, err)
		http.Error(w, "EVE refused the answer"+esiRefusalDetail(err)+".", http.StatusBadGateway)
		return
	}
	// EVE took it. The page renders from stored copies, so record the
	// answer there too, or it shows the old one until the worker's
	// next refresh.
	app.setCalendarAnswerLocally(ctx, charID, eventID, answer)
	month := r.Form.Get("month")
	if _, err := time.ParseInLocation(calMonthLayout, month, time.UTC); err != nil {
		month = ""
	}
	http.Redirect(w, r, calendarEventURL(charID, month, eventID), http.StatusSeeOther)
}

// setCalendarAnswerLocally writes the character's answer into the
// stored event list, the event itself and its attendee list.
func (app *Application) setCalendarAnswerLocally(ctx context.Context, characterID, eventID int64, answer string) {
	app.rewriteSnapshot(ctx, characterID, esi.SnapCalendar, func(payload string) (string, bool) {
		var events esi.CalendarEventSummaries
		if json.Unmarshal([]byte(payload), &events) != nil {
			return "", false
		}
		for i := range events {
			if events[i].EventID == eventID {
				events[i].EventResponse = answer
				out, err := json.Marshal(events)
				return string(out), err == nil
			}
		}
		return "", false
	})
	app.rewriteSnapshot(ctx, characterID, esi.CalendarEventKind(eventID), func(payload string) (string, bool) {
		var ev esi.CalendarEvent
		if json.Unmarshal([]byte(payload), &ev) != nil {
			return "", false
		}
		ev.Response = answer
		out, err := json.Marshal(ev)
		return string(out), err == nil
	})
	app.rewriteSnapshot(ctx, characterID, esi.CalendarAttendeesKind(eventID), func(payload string) (string, bool) {
		var attendees esi.CalendarAttendees
		if json.Unmarshal([]byte(payload), &attendees) != nil {
			return "", false
		}
		for i := range attendees {
			if attendees[i].CharacterID == characterID {
				attendees[i].EventResponse = answer
				out, err := json.Marshal(attendees)
				return string(out), err == nil
			}
		}
		return "", false
	})
}
