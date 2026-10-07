package app

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Mail / calendar / contacts worker pass (Phase 2): keeps the
// mail header list, label set, mailing lists, calendar summaries
// and contact list warm for one character, warms mail bodies and
// calendar event details behind them, all under the worker
// cycle's shared fetch allowance.
//
// The scopes behind these endpoints (read_mail,
// read_calendar_events, read_contacts) have been requested since
// the scope list was built, so a 403 still means a login older
// than the list: it is recorded and backed off exactly like the
// economy cluster's stale-scope refusals.
// ---------------------------------------------------------------------------

// maxMailBodiesPerCycle bounds body fetches per character per
// cycle. Bodies are immutable, so a backlog drains at this rate
// and then never costs a fetch again.
const maxMailBodiesPerCycle = 20

// maxCalendarDetailsPerCycle bounds calendar event detail +
// attendee fetches per character per cycle (both kinds counted).
const maxCalendarDetailsPerCycle = 10

// commsSnapshotKinds lists the list-level kinds in fetch order:
// mail first (its bodies warm behind it), then calendar, contacts.
func commsSnapshotKinds() []string {
	return []string{
		esi.SnapMail, esi.SnapMailLabels, esi.SnapMailLists,
		esi.SnapCalendar, esi.SnapContacts,
	}
}

// refreshCommsSnapshots runs the mail/calendar/contacts pass for
// one character and reports how many fetches stored data and
// whether ESI's error limit stopped it.
func (app *Application) refreshCommsSnapshots(ctx context.Context, ch db.Character, allowance *fetchBudget) (refreshed int, limited bool) {
	for _, kind := range commsSnapshotKinds() {
		if ctx.Err() != nil {
			return refreshed, false
		}
		switch app.fetchCommsKind(ctx, ch, kind, allowance) {
		case corpFetchOK:
			refreshed++
		case corpFetchLimited:
			return refreshed, true
		}
	}

	bodies, ltd := app.warmMailBodies(ctx, ch, allowance)
	refreshed += bodies
	if ltd {
		return refreshed, true
	}
	details, ltd := app.warmCalendarDetails(ctx, ch, allowance)
	return refreshed + details, ltd
}

// fetchCommsKind refreshes one list-level kind when it is due,
// spending one unit of the cycle allowance per fetch. Outcomes
// are recorded exactly like fetchCharKind (economy_worker.go):
// a 403 is a stale-scope login, logged once and backed off.
func (app *Application) fetchCommsKind(ctx context.Context, ch db.Character, kind string, allowance *fetchBudget) corpFetchOutcome {
	snap, serr := app.queries.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: ch.CharacterID, Kind: kind})
	switch {
	case serr == nil && esi.SnapshotFresh(snap):
		return corpFetchSkipped
	case serr != nil && !errors.Is(serr, sql.ErrNoRows):
		logging.Errorf("worker: read %s snapshot for character %d: %v", kind, ch.CharacterID, serr)
	}

	// A recorded 403 backs the kind off instead of retrying
	// every minute (same rule as the economy cluster).
	if state, err := app.queries.GetSnapshotFetchState(ctx, db.GetSnapshotFetchStateParams{CharacterID: ch.CharacterID, Kind: kind}); err == nil &&
		state.State == fetchStateError && strings.HasPrefix(state.Detail, forbiddenDetailPrefix) {
		if time.Since(state.AttemptedAt) < roleMissingBackoff {
			return corpFetchSkipped
		}
	}

	if !allowance.take() {
		return corpFetchSkipped
	}
	if err := app.esi.FetchAndStoreSnapshot(ctx, ch, kind); err != nil {
		switch {
		case errors.Is(err, esi.ErrErrorLimit):
			logging.Warnf("worker: ESI error limit hit refreshing %s for character %d; backing off until next cycle", kind, ch.CharacterID)
			return corpFetchLimited
		case esi.IsForbidden(err):
			detail := forbiddenDetailPrefix + " — this character's login predates the current scope list; sign in again to re-grant scopes."
			app.recordCorpFetchState(ctx, ch.CharacterID, kind, fetchStateError, detail)
			logging.Infof("worker: %s for character %d refused by ESI (403); recorded as a stale-scope login", kind, ch.CharacterID)
			return corpFetchFailed
		default:
			logging.Errorf("worker: refresh %s for character %d: %v", kind, ch.CharacterID, err)
			app.recordCorpFetchState(ctx, ch.CharacterID, kind, fetchStateError, err.Error())
			return corpFetchFailed
		}
	}
	app.recordCorpFetchState(ctx, ch.CharacterID, kind, fetchStateOK, "")
	return corpFetchOK
}

// warmMailBodies fetches the bodies behind the character's stored
// mail headers that are not stored yet — unread mail first, then
// the rest newest-first (headers arrive newest-first), at most
// maxMailBodiesPerCycle per cycle. Bodies never change once
// delivered, so a stored body is never refetched; presence of the
// snapshot row is the whole freshness test. It reports how many
// were stored and whether ESI's error limit stopped the pass.
func (app *Application) warmMailBodies(ctx context.Context, ch db.Character, allowance *fetchBudget) (fetched int, limited bool) {
	var headers esi.MailHeaders
	if !app.loadCorpSnapshot(ctx, ch.CharacterID, esi.SnapMail, &headers) {
		return 0, false // no header list yet; nothing to warm
	}

	ordered := make([]esi.MailHeader, 0, len(headers))
	for _, h := range headers {
		if !h.IsRead {
			ordered = append(ordered, h)
		}
	}
	for _, h := range headers {
		if h.IsRead {
			ordered = append(ordered, h)
		}
	}

	for _, h := range ordered {
		if fetched >= maxMailBodiesPerCycle {
			break
		}
		if h.MailID <= 0 {
			continue
		}
		kind := esi.MailBodyKind(h.MailID)
		if _, err := app.queries.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: ch.CharacterID, Kind: kind}); err == nil {
			continue // immutable: already stored
		} else if !errors.Is(err, sql.ErrNoRows) {
			logging.Errorf("worker: read %s snapshot for character %d: %v", kind, ch.CharacterID, err)
			continue
		}

		if !allowance.take() {
			break
		}
		if err := app.esi.FetchAndStoreSnapshot(ctx, ch, kind); err != nil {
			if errors.Is(err, esi.ErrErrorLimit) {
				return fetched, true
			}
			if ctx.Err() == nil {
				logging.Errorf("worker: mail body %d for character %d: %v", h.MailID, ch.CharacterID, err)
			}
			continue
		}
		fetched++
	}
	return fetched, false
}

// warmCalendarDetails fetches the detail and attendee snapshots
// behind the events in the character's calendar-list snapshot
// whose stored snapshots are missing or stale, at most
// maxCalendarDetailsPerCycle fetches per cycle, in the list's
// chronological order (soonest events first). It reports how many
// fetches stored data and whether ESI's error limit stopped it.
func (app *Application) warmCalendarDetails(ctx context.Context, ch db.Character, allowance *fetchBudget) (fetched int, limited bool) {
	var events esi.CalendarEventSummaries
	if !app.loadCorpSnapshot(ctx, ch.CharacterID, esi.SnapCalendar, &events) {
		return 0, false // no event list yet; nothing to detail
	}

	for _, ev := range events {
		if ev.EventID <= 0 {
			continue
		}
		for _, kind := range []string{esi.CalendarEventKind(ev.EventID), esi.CalendarAttendeesKind(ev.EventID)} {
			if fetched >= maxCalendarDetailsPerCycle {
				return fetched, false
			}
			snap, serr := app.queries.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: ch.CharacterID, Kind: kind})
			switch {
			case serr == nil && esi.SnapshotFresh(snap):
				continue
			case serr != nil && !errors.Is(serr, sql.ErrNoRows):
				logging.Errorf("worker: read %s snapshot for character %d: %v", kind, ch.CharacterID, serr)
				continue
			}
			if !allowance.take() {
				return fetched, false
			}
			if err := app.esi.FetchAndStoreSnapshot(ctx, ch, kind); err != nil {
				if errors.Is(err, esi.ErrErrorLimit) {
					return fetched, true
				}
				if ctx.Err() == nil {
					logging.Errorf("worker: refresh %s for character %d: %v", kind, ch.CharacterID, err)
				}
				continue
			}
			fetched++
		}
	}
	return fetched, false
}
