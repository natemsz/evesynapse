package app

import (
	"context"
	"sync"
	"time"

	db "evesynapse/internal/db/sqlc"
)

// The notification pass reads every stored dataset of every character
// of an account to see what is newly true. Done for every account
// every minute, that is the whole database read once a minute, mostly
// to find that nothing happened: an account nobody has open is only
// refreshed every half hour (worker_tiers.go).
//
// So after a worker cycle an account's data is read only when:
//
//   - new data was stored for one of its characters in that cycle, or
//   - it has not been read for notifyQuietRecheck. Some news comes with
//     the clock and not with a fetch (a skill finishing, a job or an
//     extractor running out), so nothing is left unread for long.
//
// In between, only its ops are looked at: a new op is somebody else's
// doing, and a reminder is due to the minute. Both are a couple of
// small queries.
//
// What that costs: for an account with no new data, a notification
// that comes with the clock can be up to notifyQuietRecheck late. An
// account somebody has open gets new data every cycle and is not
// affected.

// notifyQuietRecheck is the longest an account's data goes unread.
const notifyQuietRecheck = 5 * time.Minute

// notifyQuietLog is when each account's data was last read by the
// notification pass.
type notifyQuietLog struct {
	mu   sync.Mutex
	read map[int64]time.Time
}

// due reports whether the account's data should be read now, and if so
// notes that it was. An account not seen before is read at once, and
// is given a place in the recheck round by its id so that the accounts
// nothing is happening to do not all come round in the same cycle.
func (l *notifyQuietLog) due(userID int64, changed bool, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.read == nil {
		l.read = map[int64]time.Time{}
	}
	last, known := l.read[userID]
	switch {
	case !known:
		spread := time.Duration(userID%int64(notifyQuietRecheck/time.Minute)) * time.Minute
		l.read[userID] = now.Add(-spread)
		return true
	case changed || now.Sub(last) >= notifyQuietRecheck:
		l.read[userID] = now
		return true
	}
	return false
}

// forget has the account's data read again next time: its pass failed.
func (l *notifyQuietLog) forget(userID int64) {
	l.mu.Lock()
	delete(l.read, userID)
	l.mu.Unlock()
}

// notifyCycle is the notification pass as the worker runs it after a
// cycle: every account's ops, and the data of the accounts that are due
// (above).
func (app *Application) notifyCycle(ctx context.Context, characters []db.Character, now time.Time) (created int) {
	changed := app.esi.TakeChangedCharacters()
	return app.notifyAccounts(ctx, characters, now, func(userID int64, chars []db.Character) bool {
		hasNew := false
		for _, ch := range chars {
			if changed[ch.CharacterID] {
				hasNew = true
				break
			}
		}
		return !app.notifyQuiet.due(userID, hasNew, now)
	})
}
