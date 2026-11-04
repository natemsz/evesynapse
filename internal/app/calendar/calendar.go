package calendar

import (
	"evesynapse/internal/app/module"
	"evesynapse/internal/humanize"
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
	RespKey   string // ESI's response: accepted | declined | tentative | not_responded
	Important bool
}

type calendarFilter struct {
	Label string
	Count int
	Href  string
	On    bool
}

type calendarAttendeeRow struct {
	Name     string
	ID       int64
	Response string
}

type calendarDetail struct {
	Title       string
	Date        string
	DateRaw     string // RFC3339: the local time and the countdown
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
	Events        module.EconSectionState
	Rows          []calendarRow
	Detail        *calendarDetail
	// Month is the grid of the month being looked at, holding the
	// character's in-game events and the corporations' ops together
	// (comms_calendar_month.go). CanCreate: the account may create ops.
	Month     *calMonth
	CanCreate bool
	Filters   []calendarFilter // the upcoming list's response filters
	Response  string           // the response filter in force
	Awaiting  int              // upcoming events with no answer yet
	Next      *calendarRow     // the next event
}

func (app *mod) handleCalendar(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := &PageData{ShellData: app.deps.PageShell(ctx)}

	_, active, links, err := app.deps.PickCharacter(ctx, r, "/calendar/")
	if err != nil {
		logging.Errorf("calendar: list characters: %v", err)
		data.Error = "Could not load calendar events; check the server log."
		app.deps.Render(ctx, w, http.StatusOK, "calendar.html", data)
		return
	}
	if links == nil {
		app.deps.Render(ctx, w, http.StatusOK, "calendar.html", data)
		return
	}
	data.CalendarChars = links

	view := &calendarView{CharacterID: active.CharacterID, CharacterName: active.Name}
	data.Calendar = view

	var events esi.CalendarEventSummaries
	view.Events = app.deps.EconSection(ctx, active.CharacterID, esi.SnapCalendar, &events)
	if view.Events.Loaded {
		sorted := append(esi.CalendarEventSummaries(nil), events...)
		sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].EventDate < sorted[j].EventDate })
		for _, ev := range sorted {
			view.Rows = append(view.Rows, calendarRow{
				ID:        ev.EventID,
				Date:      humanize.Finish(ev.EventDate),
				DateRaw:   ev.EventDate,
				Title:     ev.Title,
				Response:  app.deps.HumanizeEnum(ev.EventResponse),
				RespKey:   ev.EventResponse,
				Important: ev.Importance > 0,
			})
		}
		// Response filters, counted over every upcoming event, and the
		// head's summary: how many wait for an answer, and what is next.
		want := r.URL.Query().Get("response")
		counts := map[string]int{}
		for _, row := range view.Rows {
			counts[row.RespKey]++
		}
		view.Awaiting = counts["not_responded"]
		if len(view.Rows) > 0 {
			next := view.Rows[0]
			view.Next = &next
		}
		base := "/calendar/?character=" + strconv.FormatInt(active.CharacterID, 10)
		view.Filters = append(view.Filters, calendarFilter{Label: "All events", Count: len(view.Rows), Href: base, On: want == ""})
		for _, f := range []struct{ key, label string }{{"not_responded", "No answer yet"}, {"accepted", "Accepted"}, {"tentative", "Tentative"}, {"declined", "Declined"}} {
			if counts[f.key] == 0 && want != f.key {
				continue
			}
			view.Filters = append(view.Filters, calendarFilter{Label: f.label, Count: counts[f.key], Href: base + "&response=" + f.key, On: want == f.key})
		}
		if want != "" {
			kept := view.Rows[:0]
			for _, row := range view.Rows {
				if row.RespKey == want {
					kept = append(kept, row)
				}
			}
			view.Rows = kept
		}
		view.Response = want
	}

	// The month grid: in-game events and ops in one view.
	{
		userID := app.deps.UserID(ctx)
		now := time.Now()
		month := parseCalMonth(r.URL.Query().Get("month"), now)
		from, to := calGridRange(month)
		view.Month = buildCalMonth(month, now, app.calendarEntries(ctx, userID, active, events, from, to, month.Format(calMonthLayout)))
		view.CanCreate = len(app.deps.OpCorps(ctx, userID, true)) > 0
	}

	// An open event renders its detail under the list.
	if eventID, _ := strconv.ParseInt(r.URL.Query().Get("event"), 10, 64); eventID > 0 {
		detail := &calendarDetail{
			EventID:    eventID,
			CanRespond: module.CharacterHasScope(active, module.CalendarRespondScope),
			RelinkURL:  app.deps.RelinkURL("calendar", active.CharacterID),
		}
		current := ""
		for _, ev := range events {
			if ev.EventID == eventID {
				current = ev.EventResponse
			}
		}
		detail.Answers = calendarAnswers(current)
		var ev esi.CalendarEvent
		if app.deps.LoadCorpSnapshot(ctx, active.CharacterID, esi.CalendarEventKind(eventID), &ev) {
			detail.Title = ev.Title
			detail.Date = humanize.Finish(ev.Date)
			detail.DateRaw = ev.Date
			detail.Owner = ev.OwnerName
			detail.OwnerID = ev.OwnerID
			detail.OwnerIsChar = ev.OwnerType == "character"
			detail.Text = ev.Text
			if ev.Duration > 0 {
				detail.Duration = humanize.Duration(time.Duration(ev.Duration) * time.Minute)
			}
			var attendees esi.CalendarAttendees
			if app.deps.LoadCorpSnapshot(ctx, active.CharacterID, esi.CalendarAttendeesKind(eventID), &attendees) {
				for _, a := range attendees {
					detail.Attendees = append(detail.Attendees, calendarAttendeeRow{
						Name:     app.deps.DisplayCharacter(ctx, a.CharacterID),
						ID:       a.CharacterID,
						Response: app.deps.HumanizeEnum(a.EventResponse),
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
					detail.DateRaw = row.DateRaw
					break
				}
			}
		}
		view.Detail = detail
	}

	app.deps.Render(ctx, w, http.StatusOK, "calendar.html", data)
}
