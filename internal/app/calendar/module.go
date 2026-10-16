// Package calendar is the Calendar module: the character calendar: in-game events and the corporation ops in one month grid, with answering events. It reads the
// snapshots core keeps fetched; the fetch itself is core's.
package calendar

import (
	"evesynapse/internal/app/module"
	"evesynapse/internal/esi"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	db "evesynapse/internal/db/sqlc"
)

// Module is the Calendar module's registration.
var Module = module.Module{
	ID:       "calendar",
	Title:    "Calendar",
	Requires: []string{},
	// This is the feature's own declaration: the same scopes core's manifest
	// names for the data feed it keeps fetched (a test pins that they agree).
	Scopes: []module.Scope{
		{Scope: "esi-calendar.read_calendar_events.v1", Unlocks: "Upcoming events and attendees."},
		{Scope: "esi-calendar.respond_calendar_events.v1", Unlocks: "Accept or decline events."},
	},
	EnhanceScopes:  []module.Scope{},
	Snapshots:      []string{esi.SnapCalendar, esi.SnapCalendarEventPrefix, esi.SnapCalendarAttPrefix},
	NotifyKinds:    []module.NotifyKindDecl{{ID: "calendar", Title: "New in-game calendar event", After: "mail"}},
	BriefingTopics: []module.BriefingTopicDecl{{ID: "calendar", Title: "Calendar events"}},
	Activities:     []string{"pilots"},
	Nav: []module.NavSection{{
		Section: "pilot",
		Entries: []module.NavEntry{
			{Href: "/calendar/", Label: "Calendar", Pages: []string{"calendar.html"}, After: "/mail/"},
		},
	}},
	Routes: func(r chi.Router, deps module.Deps) {
		h := func(f func(*mod, http.ResponseWriter, *http.Request)) http.HandlerFunc {
			return func(w http.ResponseWriter, req *http.Request) { f(newMod(deps), w, req) }
		}
		auth := r.With(deps.RequireAuth())
		auth.Get("/calendar/", h((*mod).handleCalendar))
		auth.Post("/calendar/respond", h((*mod).handleCalendarRespond))
	},
	Pages: []module.Page{
		{Template: "calendar.html", Section: "pilot"},
	},
}

// PageData is the view model passed to the Calendar templates.
type PageData struct {
	module.ShellData
	Calendar      *calendarView
	CalendarChars []module.CharLink
}

// Shell returns the embedded ShellData pointer for render.
func (p *PageData) Shell() *module.ShellData { return &p.ShellData }

// mod is one request's view of the foundation.
type mod struct {
	deps    module.Deps
	queries *db.Queries
}

func newMod(deps module.Deps) *mod { return &mod{deps: deps, queries: deps.DB()} }

// MonthLayout, ParseMonth and GridRange are the month grid's address and
// range rules, exported so the ops tests pin how the grid and the ops
// sign-up links agree.
const MonthLayout = calMonthLayout

func ParseMonth(raw string, now time.Time) time.Time { return parseCalMonth(raw, now) }

func GridRange(month time.Time) (from, to time.Time) { return calGridRange(month) }
