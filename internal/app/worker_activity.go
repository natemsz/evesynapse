package app

import (
	"context"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// What the activity log (worker_tiers.go) keeps in the database: when
// each account was last seen, so that a restart does not forget it and
// an account gone for a week can be told from one gone for the
// afternoon. And what it reads from there each cycle: which accounts
// are being told things somewhere other than the site.

// seenSaveEvery is how often, at most, an account's last-seen time is
// written to its row while it is in use. The tiers turn on a day and a
// week, so the hour this can be out by changes nothing.
const seenSaveEvery = time.Hour

// shouldSave reports whether the account's last-seen time should be
// written now, and notes that it is being.
func (a *activityLog) shouldSave(userID int64, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if last, known := a.saved[userID]; known && now.Sub(last) < seenSaveEvery {
		return false
	}
	if a.saved == nil {
		a.saved = map[int64]time.Time{}
	}
	a.saved[userID] = now
	return true
}

// saveSeen writes the account's last-seen time, if it is time to.
func (app *Application) saveSeen(ctx context.Context, userID int64, now time.Time) {
	if !app.activity.shouldSave(userID, now) {
		return
	}
	if err := app.queries.TouchUserSeen(ctx, db.TouchUserSeenParams{ID: userID, LastSeenAt: now}); err != nil {
		logging.Errorf("activity: record that account %d was seen: %v", userID, err)
	}
}

// remember gives the log an account's stored last-seen time, unless it
// has seen the account more recently itself.
func (a *activityLog) remember(userID int64, at time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.seen == nil {
		a.seen = map[int64]time.Time{}
	}
	if a.saved == nil {
		a.saved = map[int64]time.Time{}
	}
	if at.After(a.seen[userID]) {
		a.seen[userID] = at
	}
	a.saved[userID] = at
}

// loadActivity reads when every account was last seen, at start-up.
// Unreadable, the log starts empty: every account is dormant until it
// is next seen, and none is asleep.
func (app *Application) loadActivity(ctx context.Context) {
	rows, err := app.queries.ListUsersLastSeen(ctx)
	if err != nil {
		logging.Errorf("activity: read when accounts were last seen: %v", err)
		return
	}
	for _, row := range rows {
		app.activity.remember(row.ID, row.LastSeenAt)
	}
}

// refreshListening reads which accounts have a browser subscribed to
// push or a Discord account linked. Unreadable, nothing is known and
// no account is treated as asleep.
func (app *Application) refreshListening(ctx context.Context) {
	ids, err := app.queries.ListUsersBeingNotified(ctx)
	set := make(map[int64]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	if err != nil {
		logging.Errorf("activity: read which accounts are notified elsewhere: %v", err)
	}
	app.activity.mu.Lock()
	app.activity.listening, app.activity.listeningKnown = set, err == nil
	app.activity.mu.Unlock()
}
