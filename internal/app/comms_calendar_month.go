package app

import (
	"context"
	"fmt"
	"sort"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// The calendar's month view: one grid holding both kinds of entry,
// the character's in-game events and the ops of the corporations the
// account has a character in. EVE time throughout, weeks from Monday.
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

// calendarEntries gathers what the grid shows between from and to:
// the corporations' ops, with the account's own answer on each, and
// the character's in-game events.
func (app *Application) calendarEntries(ctx context.Context, userID int64, members []opMember, active db.Character, events esi.CalendarEventSummaries, from, to time.Time, month string) []calEntry {
	var entries []calEntry

	if corps := opCorps(members, false); len(corps) > 0 {
		ops, err := app.queries.ListOpsForCorporationsBetween(ctx, db.ListOpsForCorporationsBetweenParams{
			CorporationIds: corps, FromTime: from, ToTime: to,
		})
		if err != nil {
			logging.Errorf("calendar: ops for user %d: %v", userID, err)
		}
		ids := make([]int64, 0, len(ops))
		for _, op := range ops {
			ids = append(ids, op.ID)
		}
		mine := map[int64]string{}
		if len(ids) > 0 {
			rows, err := app.queries.ListOpSignupsForOps(ctx, ids)
			if err != nil {
				logging.Errorf("calendar: sign-ups for user %d: %v", userID, err)
			}
			// With several characters signed up, "yes" outranks
			// "maybe", which outranks "no".
			rank := map[string]int{opNo: 1, opMaybe: 2, opYes: 3}
			for _, row := range rows {
				if row.UserID == userID && rank[row.Response] > rank[mine[row.OpID]] {
					mine[row.OpID] = row.Response
				}
			}
		}
		for _, op := range ops {
			entries = append(entries, calEntry{
				Time: op.StartsAt.UTC().Format("15:04"), Title: op.Title, URL: opURL(op.ID),
				Kind: calKindOp, Cancelled: op.CancelledAt.Valid, Mine: mine[op.ID], at: op.StartsAt,
			})
		}
	}

	for _, ev := range events {
		at, ok := parseRFC3339(ev.EventDate)
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
