package app

import (
	"log"
	"net/http"
	"sort"
	"strconv"
	"time"

	"evesynapse/internal/esi"
)

// ---------------------------------------------------------------------------
// Calendar page (/calendar/): the character's upcoming events
// (ESI's next 50 chronological summaries) with a detail block —
// text, owner, duration, attendees — rendered cache-only from
// the worker-warmed calendar snapshots. Read-only: responding to
// events needs esi-calendar.respond_calendar_events.v1, which the
// app deliberately never requested.
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
}

type calendarView struct {
	CharacterID   int64
	CharacterName string
	Events        econSectionState
	Rows          []calendarRow
	Detail        *calendarDetail
}

func (app *Application) handleCalendar(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := pageData{
		LoggedIn:      true,
		CharacterName: app.sessions.GetString(ctx, sessionCharacterName),
		SSOConfigured: app.cfg.SSOConfigured(),
	}

	_, active, links, err := app.pickCharacter(ctx, r, "/calendar/")
	if err != nil {
		log.Printf("calendar: list characters: %v", err)
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

	// An open event renders its detail under the list.
	if eventID, _ := strconv.ParseInt(r.URL.Query().Get("event"), 10, 64); eventID > 0 {
		detail := &calendarDetail{}
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
						Name:     characterDisplay(app.esi, a.CharacterID),
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
