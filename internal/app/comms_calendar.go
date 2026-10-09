package app

import (
	"net/http"
	"sort"
	"strconv"
	"time"

	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Calendar page (/calendar/): a month grid of the character's in-game
// events and the corporations' ops (comms_calendar_month.go, ops.go),
// then the upcoming in-game events with a detail block, rendered
// cache-only from the worker-warmed calendar snapshots. An open event
// can be answered from the page (comms_calendar_respond.go).
// ---------------------------------------------------------------------------

type calendarRow struct {
	ID        int64
	Date      string
	DateRaw   string // RFC3339, drives the live countdown
	Title     string
	Response  string
	Important bool
}

type calendarAttendeeRow struct {
	Name     string
	ID       int64
	Response string
}

type calendarDetail struct {
	Title       string
	Date        string
	Duration    string
	Owner       string // ESI's owner_name, verbatim
	OwnerID     int64
	OwnerIsChar bool   // owner_type == "character"
	Text        string // plain text; template-escaped at render
	Attendees   []calendarAttendeeRow
	Warming     bool // detail snapshot not yet warmed
	// Answering the event: its id, the choices with the current one
	// marked, and whether the character granted the scope for it.
	EventID    int64
	Answers    []calendarAnswer
	CanRespond bool
	RelinkURL  string
}

type calendarView struct {
	CharacterID   int64
	CharacterName string
	Events        econSectionState
	Rows          []calendarRow
	Detail        *calendarDetail
	// Month is the grid of the month being looked at, holding the
	// character's in-game events and the corporations' ops together
	// (comms_calendar_month.go). CanCreate: the account may create ops.
	Month     *calMonth
	CanCreate bool
}

func (app *Application) handleCalendar(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.page(ctx)

	_, active, links, err := app.pickCharacter(ctx, r, "/calendar/")
	if err != nil {
		logging.Errorf("calendar: list characters: %v", err)
		data.Error = "Could not load calendar events; check the server log."
		app.render(ctx, w, http.StatusOK, "calendar.html", data)
		return
	}
	if links == nil {
		app.render(ctx, w, http.StatusOK, "calendar.html", data)
		return
	}
	data.CalendarChars = links

	view := &calendarView{CharacterID: active.CharacterID, CharacterName: active.Name}
	data.Calendar = view

	var events esi.CalendarEventSummaries
	view.Events = app.econSection(ctx, active.CharacterID, esi.SnapCalendar, &events)
	if view.Events.Loaded {
		sorted := append(esi.CalendarEventSummaries(nil), events...)
		sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].EventDate < sorted[j].EventDate })
		for _, ev := range sorted {
			view.Rows = append(view.Rows, calendarRow{
				ID:        ev.EventID,
				Date:      formatFinish(ev.EventDate),
				DateRaw:   ev.EventDate,
				Title:     ev.Title,
				Response:  humanizeEnum(ev.EventResponse),
				Important: ev.Importance > 0,
			})
		}
	}

	// The month grid: in-game events and ops in one view.
	{
		userID := app.userID(ctx)
		members := app.opMembers(ctx, userID)
		now := time.Now()
		month := parseCalMonth(r.URL.Query().Get("month"), now)
		from, to := calGridRange(month)
		view.Month = buildCalMonth(month, now, app.calendarEntries(ctx, userID, members, active, events, from, to, month.Format(calMonthLayout)))
		view.CanCreate = len(opCorps(members, true)) > 0
	}

	// An open event renders its detail under the list.
	if eventID, _ := strconv.ParseInt(r.URL.Query().Get("event"), 10, 64); eventID > 0 {
		detail := &calendarDetail{
			EventID:    eventID,
			CanRespond: characterHasScope(active, calendarRespondScope),
			RelinkURL:  relinkURL("calendar", active.CharacterID),
		}
		current := ""
		for _, ev := range events {
			if ev.EventID == eventID {
				current = ev.EventResponse
			}
		}
		detail.Answers = calendarAnswers(current)
		var ev esi.CalendarEvent
		if app.loadCorpSnapshot(ctx, active.CharacterID, esi.CalendarEventKind(eventID), &ev) {
			detail.Title = ev.Title
			detail.Date = formatFinish(ev.Date)
			detail.Owner = ev.OwnerName
			detail.OwnerID = ev.OwnerID
			detail.OwnerIsChar = ev.OwnerType == "character"
			detail.Text = ev.Text
			if ev.Duration > 0 {
				detail.Duration = humanDuration(time.Duration(ev.Duration) * time.Minute)
			}
			var attendees esi.CalendarAttendees
			if app.loadCorpSnapshot(ctx, active.CharacterID, esi.CalendarAttendeesKind(eventID), &attendees) {
				for _, a := range attendees {
					detail.Attendees = append(detail.Attendees, calendarAttendeeRow{
						Name:     app.displayCharacter(ctx, a.CharacterID),
						ID:       a.CharacterID,
						Response: humanizeEnum(a.EventResponse),
					})
				}
				sort.Slice(detail.Attendees, func(i, j int) bool { return detail.Attendees[i].Name < detail.Attendees[j].Name })
			}
		} else {
			detail.Warming = true
			// Title from the summary list when we have it.
			for _, row := range view.Rows {
				if row.ID == eventID {
					detail.Title = row.Title
					detail.Date = row.Date
					break
				}
			}
		}
		view.Detail = detail
	}

	app.render(ctx, w, http.StatusOK, "calendar.html", data)
}
