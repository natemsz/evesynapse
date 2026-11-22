package calendar

import (
	"context"
	"evesynapse/internal/app/module"
	"fmt"
	"sort"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// ---------------------------------------------------------------------------
// The calendar's month view: one grid holding both kinds of entry,
// the character's in-game events and the fleets the account can see.
// EVE time throughout, weeks from Monday.
// ---------------------------------------------------------------------------

// Entry kinds, used as CSS classes.
const (
	calKindOp    = "op"
	calKindEvent = "event"
)

// calEntry is one thing on one day.
type calEntry struct {
	Time      string // "19:00"
	Title     string
	URL       string
	Kind      string
	Cancelled bool
	// Mine is the account's own answer where it has given one: an op
	// sign-up ("yes"), or the in-game response ("Accepted").
	Mine string
	at   time.Time
}

type calDay struct {
	Day     int
	Date    string // 2006-01-02
	InMonth bool
	Today   bool
	Entries []calEntry
}

// calMonth is the grid and what sits around it.
type calMonth struct {
	Title    string // "October 2026"
	Month    string // "2026-10"
	Prev     string
	Next     string
	Weekdays []string
	Weeks    [][]calDay
}

const calMonthLayout = "2006-01"

// parseCalMonth reads ?month=2026-10; anything else is this month.
func parseCalMonth(raw string, now time.Time) time.Time {
	if t, err := time.ParseInLocation(calMonthLayout, raw, time.UTC); err == nil && t.Year() >= 2003 && t.Year() <= now.Year()+5 {
		return t
	}
	now = now.UTC()
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// calGridRange is the span of days a month's grid shows: whole weeks,
// Monday first, covering the month.
func calGridRange(month time.Time) (from, to time.Time) {
	from = month.AddDate(0, 0, -((int(month.Weekday()) + 6) % 7))
	last := month.AddDate(0, 1, -1)
	to = last.AddDate(0, 0, 7-((int(last.Weekday())+6)%7))
	return from, to
}

// buildCalMonth lays entries out on the month's grid.
func buildCalMonth(month, now time.Time, entries []calEntry) *calMonth {
	from, to := calGridRange(month)
	byDay := map[string][]calEntry{}
	for _, e := range entries {
		key := e.at.UTC().Format("2006-01-02")
		byDay[key] = append(byDay[key], e)
	}
	m := &calMonth{
		Title:    month.Format("January 2006"),
		Month:    month.Format(calMonthLayout),
		Prev:     month.AddDate(0, -1, 0).Format(calMonthLayout),
		Next:     month.AddDate(0, 1, 0).Format(calMonthLayout),
		Weekdays: []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"},
	}
	today := now.UTC().Format("2006-01-02")
	var week []calDay
	for day := from; day.Before(to); day = day.AddDate(0, 0, 1) {
		key := day.Format("2006-01-02")
		list := byDay[key]
		sort.SliceStable(list, func(i, j int) bool { return list[i].at.Before(list[j].at) })
		week = append(week, calDay{
			Day: day.Day(), Date: key, InMonth: day.Month() == month.Month(), Today: key == today, Entries: list,
		})
		if len(week) == 7 {
			m.Weeks = append(m.Weeks, week)
			week = nil
		}
	}
	return m
}

// calendarEntries gathers what the grid shows between from and to: the
// fleets the account can see, with its own answer on each, and the
// character's in-game events.
func (app *mod) calendarEntries(ctx context.Context, userID int64, active db.Character, events esi.CalendarEventSummaries, from, to time.Time, month string) []calEntry {
	var entries []calEntry

	for _, fleet := range app.deps.CalendarFleets(ctx, userID, from, to) {
		entries = append(entries, calEntry{
			Time: fleet.StartsAt.UTC().Format("15:04"), Title: fleet.Title, URL: module.OpURL(fleet.ID),
			Kind: calKindOp, Cancelled: fleet.Cancelled, Mine: fleet.Answer, at: fleet.StartsAt,
		})
	}

	for _, ev := range events {
		at, ok := module.ParseRFC3339(ev.EventDate)
		if !ok || at.Before(from) || !at.Before(to) {
			continue
		}
		title := ev.Title
		if title == "" {
			title = "Calendar event"
		}
		// Only an answer that was given is shown; "not responded" on
		// every unanswered event would be noise.
		mine := ""
		switch ev.EventResponse {
		case "accepted", "declined", "tentative":
			mine = ev.EventResponse
		}
		entries = append(entries, calEntry{
			Time: at.UTC().Format("15:04"), Title: title,
			URL:  fmt.Sprintf("/calendar/?character=%d&month=%s&event=%d", active.CharacterID, month, ev.EventID),
			Kind: calKindEvent, Mine: mine, at: at,
		})
	}
	return entries
}
