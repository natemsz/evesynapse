package app

import (
	"strings"
	"testing"
	"time"

	"evesynapse/internal/esi"
)

// TestAsleepTier: an account not seen for a week is asleep, but only
// once it is known that its notifications go nowhere but the site; one
// with a browser subscribed or Discord linked stays dormant. Coming
// back wakes it at once.
func TestAsleepTier(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	var a activityLog
	a.remember(1, now.Add(-8*24*time.Hour))
	a.remember(2, now.Add(-6*24*time.Hour))
	a.remember(3, now.Add(-30*24*time.Hour))

	if got := a.tier(1, now); got != tierDormant {
		t.Fatalf("before it is known who is being notified: %v, want dormant", got)
	}
	a.listening, a.listeningKnown = map[int64]bool{3: true}, true
	for id, want := range map[int64]warmTier{1: tierAsleep, 2: tierDormant, 3: tierDormant, 4: tierDormant} {
		if got := a.tier(id, now); got != want {
			t.Errorf("account %d: %v, want %v", id, got, want)
		}
	}
	if !a.noteActivity(1, now) || a.tier(1, now) != tierActive {
		t.Fatal("an asleep account that made a request is not active again")
	}

	if tierHold(tierAsleep, esi.SnapWallet) != 2*time.Hour || tierHold(tierAsleep, esi.SnapLocation) != 6*time.Hour || tierFurtherHold(tierAsleep) != 2*time.Hour {
		t.Error("the asleep tier's holds are not 6 hours for position and 2 for the rest")
	}
	if tierAsleep.String() != "asleep" {
		t.Errorf("the tier is called %q", tierAsleep.String())
	}

	// The Sync page counts them, and says nothing of them when there
	// are none.
	timing := workerTiming{At: now, Characters: 3, Dormant: 1, Asleep: 2, Budget: 120}
	if v := workerTimingViewFor(workerStatus{Timing: timing}); !strings.Contains(v.Last, "3 characters (0 active, 0 watched, 0 recent, 1 dormant, 2 asleep)") {
		t.Errorf("the Sync line: %q", v.Last)
	}
	timing.Asleep = 0
	if v := workerTimingViewFor(workerStatus{Timing: timing}); strings.Contains(v.Last, "asleep") {
		t.Errorf("the Sync line mentions asleep with none: %q", v.Last)
	}
}

// TestSeenIsSavedHourly: an account's last-seen time is written at
// most once an hour, and what was loaded at start-up counts as written.
func TestSeenIsSavedHourly(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	var a activityLog
	if !a.shouldSave(1, now) {
		t.Fatal("the first sighting is not saved")
	}
	if a.shouldSave(1, now.Add(59*time.Minute)) {
		t.Fatal("saved again within the hour")
	}
	if !a.shouldSave(1, now.Add(61*time.Minute)) {
		t.Fatal("not saved again after an hour")
	}
	a.remember(2, now.Add(-10*time.Minute))
	if a.shouldSave(2, now) {
		t.Fatal("an account loaded as seen ten minutes ago is written again at once")
	}
	// What the log saw itself is not put back by an older stored time.
	a.noteActivity(3, now)
	a.remember(3, now.Add(-48*time.Hour))
	if a.tier(3, now) != tierActive {
		t.Fatal("an older stored time replaced a newer sighting")
	}
}

// TestActivitySurvivesARestart: a request records when the account was
// seen; a fresh start reads it back, so an account seen today is not
// dormant after a restart and one gone for a week is asleep, unless a
// browser of its is subscribed to push.
func TestActivitySurvivesARestart(t *testing.T) {
	app, conn, q := buildCorpTestApp(t, &countingTransport{})
	app.cfg.workerTiersOff = false
	ctx := t.Context()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ch := seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	now := time.Now()

	app.saveSeen(ctx, user.ID, now.Add(-2*time.Hour))
	restart := func() {
		app.activity = activityLog{}
		app.loadActivity(ctx)
		app.refreshListening(ctx)
	}
	restart()
	if got := app.tierOf(ch, now); got != tierRecent {
		t.Fatalf("seen two hours before a restart: %v, want recent", got)
	}

	if _, err := conn.ExecContext(ctx, `UPDATE users SET last_seen_at = now() - interval '8 days' WHERE id = $1`, user.ID); err != nil {
		t.Fatal(err)
	}
	restart()
	if got := app.tierOf(ch, now); got != tierAsleep {
		t.Fatalf("not seen for eight days, notified nowhere: %v, want asleep", got)
	}

	if _, err := conn.ExecContext(ctx, `INSERT INTO push_subscriptions (user_id, endpoint, p256dh, auth) VALUES ($1, 'https://push.example/fixture', 'k', 'a')`, user.ID); err != nil {
		t.Fatal(err)
	}
	app.refreshListening(ctx)
	if got := app.tierOf(ch, now); got != tierDormant {
		t.Fatalf("not seen for eight days but subscribed to push: %v, want dormant", got)
	}
}
