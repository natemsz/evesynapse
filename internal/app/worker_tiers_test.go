package app

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"evesynapse/internal/esi"
)

func TestTierHolds(t *testing.T) {
	// Somebody looking: as often as ESI allows.
	for _, kind := range []string{esi.SnapLocation, esi.SnapWallet} {
		if tierHold(tierActive, kind) != 0 {
			t.Errorf("an active account's %s is held back", kind)
		}
	}
	if tierFurtherHold(tierActive) != 0 {
		t.Error("an active account's further datasets are held back")
	}
	// An open account's other characters: position kept current enough
	// for the fleet overview, the rest eased off.
	if tierHold(tierWatched, esi.SnapLocation) != 2*time.Minute || tierHold(tierWatched, esi.SnapWallet) != 5*time.Minute || tierFurtherHold(tierWatched) != 5*time.Minute {
		t.Error("the watched tier's holds are not 2 minutes for position and 5 for the rest")
	}
	// Position is held longer than the rest, and dormant longer than recent.
	if !(tierHold(tierRecent, esi.SnapLocation) > tierHold(tierRecent, esi.SnapWallet)) ||
		!(tierHold(tierDormant, esi.SnapLocation) > tierHold(tierDormant, esi.SnapWallet)) ||
		!(tierHold(tierDormant, esi.SnapWallet) > tierHold(tierRecent, esi.SnapWallet)) ||
		!(tierFurtherHold(tierDormant) > tierFurtherHold(tierRecent)) {
		t.Error("the holds are not ordered: position longer than the rest, dormant longer than recent")
	}
	// Notifications must not wait long even for a dormant account.
	if tierHold(tierDormant, esi.SnapSkillqueue) > 30*time.Minute || tierFurtherHold(tierDormant) > 30*time.Minute {
		t.Error("a dormant account's notifications would be more than half an hour late")
	}
}

func TestActivityLog(t *testing.T) {
	var a activityLog
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	if a.tier(1, now) != tierDormant {
		t.Fatal("an account never seen is not dormant")
	}
	if !a.noteActivity(1, now) {
		t.Fatal("a first request is not reported as a return")
	}
	if a.noteActivity(1, now.Add(time.Minute)) {
		t.Fatal("a second request a minute later is reported as a return")
	}
	for at, want := range map[time.Duration]warmTier{
		5 * time.Minute:  tierActive,
		11 * time.Minute: tierActive, // seen again at one minute
		30 * time.Minute: tierRecent,
		23 * time.Hour:   tierRecent,
		26 * time.Hour:   tierDormant,
	} {
		if got := a.tier(1, now.Add(at)); got != want {
			t.Errorf("%v after first being seen: %v, want %v", at, got, want)
		}
	}
	if !a.noteActivity(1, now.Add(time.Hour)) {
		t.Fatal("coming back after an hour is not reported as a return")
	}
	if a.tier(2, now) != tierDormant {
		t.Fatal("one account's activity counted for another")
	}

	// Further datasets: due at first, then held for the tier's time.
	if !a.furtherDue(7, 5*time.Minute, now) {
		t.Fatal("never gone through, and not due")
	}
	a.furtherDone(7, now)
	if a.furtherDue(7, 5*time.Minute, now.Add(4*time.Minute)) || !a.furtherDue(7, 5*time.Minute, now.Add(5*time.Minute)) {
		t.Fatal("the hold on further datasets is not five minutes")
	}
	if !a.furtherDue(7, 0, now) {
		t.Fatal("with no hold, not due")
	}
}

// tierESI remembers which requests the worker made about a character.
// It answers each as a failure, which is enough: the test is about
// whether the worker asks at all.
type tierESI struct {
	mu    sync.Mutex
	paths []string
}

func (s *tierESI) RoundTrip(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	s.paths = append(s.paths, req.URL.Path)
	s.mu.Unlock()
	return (&countingTransport{}).RoundTrip(req)
}

// about counts the requests made about a character, and forgets them.
func (s *tierESI) about(characterID int64) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, p := range s.paths {
		if strings.Contains(p, fmt.Sprintf("/characters/%d/", characterID)) {
			n++
		}
	}
	s.paths = nil
	return n
}

// TestWorkerWarmsByActivity: with every dataset past ESI's cache
// window, how much is fetched depends on whether anyone is looking. A
// dormant account's character, refreshed a minute ago, costs nothing
// at all, not even a look at its token; a recent one gets its ordinary
// data after five minutes but not its position; an account that comes
// back is refreshed in full on the next cycle; and with tiers switched
// off everything is refreshed every cycle as before.
func TestWorkerWarmsByActivity(t *testing.T) {
	transport := &tierESI{}
	app, conn, q := buildCorpTestApp(t, transport)
	app.cfg.workerTiersOff = false
	ctx := t.Context()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ch := seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	cookie := sessionCookie(t, app, user.ID, ch.CharacterID, ch.Name)
	for _, kind := range coreSnapshotKinds {
		seedSnapshot(t, q, ch.CharacterID, kind, `{}`)
	}
	// Every dataset is past its cache window, and was fetched `ago` ago.
	fetched := func(ago time.Duration) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, `UPDATE character_snapshots SET cached_until = $1, fetched_at = $2 WHERE character_id = $3`,
			time.Now().Add(-time.Second), time.Now().Add(-ago), ch.CharacterID); err != nil {
			t.Fatal(err)
		}
	}
	c := &cycleState{app: app, allowance: &fetchBudget{left: maxFetchesPerCycle}}
	now := time.Now()

	fetched(time.Minute)
	for tier, wantDue := range map[warmTier]bool{tierActive: true, tierRecent: false, tierDormant: false} {
		if _, due := c.coreFreshness(ctx, ch, tier, now); due != wantDue {
			t.Errorf("fetched a minute ago, %v: due=%v, want %v", tier, due, wantDue)
		}
	}
	fetched(10 * time.Minute)
	fresh, due := c.coreFreshness(ctx, ch, tierRecent, now)
	if !due || fresh[esi.SnapWallet] || !fresh[esi.SnapLocation] {
		t.Errorf("fetched ten minutes ago, recent: due=%v, wallet fresh=%v, location fresh=%v; want the wallet due and the position held", due, fresh[esi.SnapWallet], fresh[esi.SnapLocation])
	}
	if _, due = c.coreFreshness(ctx, ch, tierDormant, now); due {
		t.Error("fetched ten minutes ago, dormant: something is due")
	}
	fetched(40 * time.Minute)
	fresh, due = c.coreFreshness(ctx, ch, tierDormant, now)
	if !due || fresh[esi.SnapWallet] || !fresh[esi.SnapLocation] {
		t.Errorf("fetched forty minutes ago, dormant: due=%v, wallet fresh=%v, location fresh=%v", due, fresh[esi.SnapWallet], fresh[esi.SnapLocation])
	}

	// A whole cycle. Never seen, so dormant; refreshed a minute ago,
	// its other datasets gone through just now: nothing is asked.
	fetched(time.Minute)
	app.activity.furtherDone(ch.CharacterID, time.Now())
	app.refreshCycle(ctx)
	if n := transport.about(ch.CharacterID); n != 0 {
		t.Fatalf("a dormant character with nothing due cost %d request(s)", n)
	}
	timing := app.snapshotWorkerStatus().Timing
	if timing.Dormant != 1 || timing.Active != 0 || timing.Overdue != 0 || timing.NeverFetched {
		t.Fatalf("with one dormant character: %+v; want it counted as dormant and no lateness reported", timing)
	}

	// The account opens a page: active, and at the front of the queue.
	if code, _ := getPage(t, app, cookie, "/characters/"); code != http.StatusOK {
		t.Fatalf("GET /characters/: %d", code)
	}
	if app.tierOf(ch, time.Now()) != tierActive {
		t.Fatal("an account that just made a request is not active")
	}
	// Its other character is watched, not active: only the one being
	// looked at is kept to the second. Opening a page about the alt
	// makes that one active too. And naming a character in an address
	// does nothing for an account that does not have the site open.
	alt := seedCharacter(t, q, user.ID, fixtureCharB, "Fixture Alt")
	if got := app.tierOf(alt, time.Now()); got != tierWatched {
		t.Fatalf("the open account's other character is %v, want watched", got)
	}
	if code, _ := getPage(t, app, cookie, "/characters/?character=90000002"); code != http.StatusOK {
		t.Fatalf("GET with ?character=: %d", code)
	}
	if got := app.tierOf(alt, time.Now()); got != tierActive {
		t.Fatalf("a character a page was opened for is %v, want active", got)
	}
	stranger, serr := q.CreateUser(ctx)
	if serr != nil {
		t.Fatal(serr)
	}
	theirs := seedCharacter(t, q, stranger.ID, 90000003, "Fixture Stranger")
	getPage(t, app, cookie, "/characters/?character=90000003")
	if got := app.tierOf(theirs, time.Now()); got != tierDormant {
		t.Fatalf("another account's character, named in somebody else's address, is %v, want dormant", got)
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM characters WHERE character_id IN (90000002, 90000003)`); err != nil {
		t.Fatal(err)
	}
	app.takePriorityCharacters()
	app.markCharacterPriority(ch.CharacterID)
	if ids := app.takePriorityCharacters(); len(ids) != 1 || ids[0] != ch.CharacterID {
		t.Fatalf("a returning account's characters were not put first: %v", ids)
	}
	transport.about(ch.CharacterID)
	app.refreshCycle(ctx)
	if n := transport.about(ch.CharacterID); n == 0 {
		t.Fatal("an active character with everything past its cache window was not refreshed")
	}
	// It has only just become active: what it had was the dormant
	// tier's data, held back on purpose, and this cycle refreshed it.
	// That is not the worker running late.
	if timing = app.snapshotWorkerStatus().Timing; timing.Active != 1 || timing.Overdue != 0 {
		t.Fatalf("a character's first active cycle: %+v; want it counted as active and no lateness reported", timing)
	}
	// Still active a cycle later with data past its window: that is.
	if _, err := conn.ExecContext(ctx, `UPDATE character_snapshots SET cached_until = $1 WHERE character_id = $2`, time.Now().Add(-3*time.Minute), ch.CharacterID); err != nil {
		t.Fatal(err)
	}
	app.refreshCycle(ctx)
	if timing = app.snapshotWorkerStatus().Timing; timing.Active != 1 || timing.Overdue < 3*time.Minute || timing.Overdue > 4*time.Minute {
		t.Fatalf("an active character three minutes past its window: %+v; want about three minutes of lateness", timing)
	}

	// Tiers off: a character nobody is looking at is refreshed like
	// any other.
	app.activity = activityLog{}
	app.activity.furtherDone(ch.CharacterID, time.Now())
	app.cfg.workerTiersOff = true
	fetched(time.Minute)
	app.refreshCycle(ctx)
	if n := transport.about(ch.CharacterID); n == 0 {
		t.Fatal("with tiers off, an unseen character was not refreshed")
	}
}

// TestNoteTier: whether a character was active at the cycle before.
func TestNoteTier(t *testing.T) {
	var a activityLog
	if a.noteTier(1, true) {
		t.Fatal("a character never seen before was active before")
	}
	if !a.noteTier(1, true) {
		t.Fatal("active last cycle is not remembered")
	}
	if a.noteTier(2, false) || a.noteTier(2, true) {
		t.Fatal("a character that was not active is remembered as active")
	}
	if !a.noteTier(1, false) || a.noteTier(1, true) {
		t.Fatal("leaving the active tier is not remembered")
	}
}
