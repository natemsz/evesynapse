package app

import (
	"context"
	"fmt"
	"hash/fnv"
	"sync"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// Typing-intent guesses (schema 028): while someone types in a
// search box, the top suggestions are queued ahead of the pick at
// the typing ring — below a record somebody has open, above the
// proactive orbit. Each keystroke replaces the previous one for
// that box, so queued guesses never accumulate (the ledger of
// what was guessed and what was opened does, by design); what they fetch still
// passes every shared budget gate, and the drains serve them from
// their own capped slice (typingSlice), never ahead of real work.

const (
	// maxTypingSlice caps one drain's typing-tier rows: min(12, a
	// tenth of the cycle budget), so a wrong guess can never
	// delay real work.
	maxTypingSlice = 12
	// typingGuessHitWindow is how long after a guess the pick
	// still counts as its hit (the prefetch tuning signal).
	typingGuessHitWindow = 10 * time.Minute
	// typingLearnWindow is how far back the hit rates the drains
	// order typing rows by reach. typingAssumedHit is the rate an
	// entity with no record in the window reads at, counting for
	// typingAssumedWeight guesses, so a cold start drains newest
	// first and one lucky hit cannot crown an entity.
	typingLearnWindow   = 7 * 24 * time.Hour
	typingAssumedHit    = 0.30
	typingAssumedWeight = 20.0
)

// typingSlice is one drain's typing-tier budget: a tenth of the
// cycle's fetch budget, at most maxTypingSlice. Callers pass it
// into the drain query, which enforces it.
func (app *Application) typingSlice() int {
	if slice := app.fetchesPerCycle() / 10; slice < maxTypingSlice {
		return slice
	}
	return maxTypingSlice
}

// typingLockStripes is how many locks the boxes share. A box
// always maps to the same one, so its own keystrokes queue up
// behind each other; two boxes share one only by coincidence.
const typingLockStripes = 64

// typingLock returns the lock that orders one box's keystrokes.
func (app *Application) typingLock(box string) *sync.Mutex {
	h := fnv.New32a()
	_, _ = h.Write([]byte(box))
	return &app.typingLocks[h.Sum32()%typingLockStripes]
}

// typingGuess is one thing a keystroke guessed the pilot will
// open: a record kind, its ID, and the region for a history.
type typingGuess struct {
	kind   pageWantKind
	id     int64
	region int64
}

// setTypingGuesses makes guesses the box's current ones: what its
// previous keystroke guessed and this one does not is superseded,
// and these are noted, as one step under the box's lock. The caller has finished its
// searching by now; the lock covers these queue writes and
// nothing slower, and only this box waits on it.
func (app *Application) setTypingGuesses(ctx context.Context, box string, guesses []typingGuess, now time.Time) {
	lock := app.typingLock(box)
	lock.Lock()
	defer lock.Unlock()
	app.replaceTypingGuesses(ctx, box, guesses, now)
	for _, g := range guesses {
		if g.id <= 0 {
			continue
		}
		switch g.kind {
		case pageWantPilot:
			app.noteTypingPilotGuess(ctx, box, g.id, now)
		case pageWantCorporation:
			app.noteTypingCorporationGuess(ctx, box, g.id, now)
		case pageWantAlliance:
			app.noteTypingAllianceGuess(ctx, box, g.id, now)
		case pageWantHistory:
			app.noteTypingHistoryGuess(ctx, box, g.region, g.id, now)
		case pageWantTypeDescription:
			app.noteTypingDetailGuess(ctx, box, g.id, now)
		}
	}
}

// typingBox names one search box's guess slot: the box on the
// page plus whose fingers are in it, so two pilots typing at
// once never evict each other's guesses.
func (app *Application) typingBox(ctx context.Context, name string) string {
	return fmt.Sprintf("%d|%s", app.userID(ctx), name)
}

// typingLedgerKeep is how long a guess stays in the ledger: well
// past the learn window, so the rates never read a thinned edge.
const typingLedgerKeep = 30 * 24 * time.Hour

// replaceTypingGuesses moves a box on to a new keystroke: the
// guesses it was making and no longer makes (everything current
// that is not in keep) are superseded, and their queue rows
// dropped. Superseded, not deleted: the ledger row stays, with
// whether the pilot opened it, so the hit rates read the whole
// window instead of each box's last keystroke. A guess still in
// keep is the same guess as before and is left alone.
//
// Only queue rows still waiting at the typing ring go: one the
// pilot opened has been bumped to viewed, and one already
// holding data is a record now, not a guess. Only
// setTypingGuesses calls this, under the box's lock, so two
// keystroke sequences in one box cannot interleave.
func (app *Application) replaceTypingGuesses(ctx context.Context, box string, keep []typingGuess, now time.Time) {
	// One stamp names this keystroke's superseded rows for the
	// queue deletes below, at the database's precision.
	at := now.UTC().Truncate(time.Microsecond)
	kept := make([]string, 0, len(keep))
	for _, g := range keep {
		kept = append(kept, fmt.Sprintf("%s|%d|%d", g.kind, g.id, g.region))
	}
	if err := app.queries.SupersedeTypingGuesses(ctx, db.SupersedeTypingGuessesParams{Box: box, SupersededAt: at, Kept: kept}); err != nil {
		logging.Errorf("typing: supersede guesses for box %q: %v", box, err)
		return
	}
	if err := app.queries.DeleteTypingPilotGuesses(ctx, db.DeleteTypingPilotGuessesParams{Box: box, TypingPriority: wantTyping, SupersededAt: at}); err != nil {
		logging.Errorf("typing: drop pilot guesses for box %q: %v", box, err)
	}
	if err := app.queries.DeleteTypingCorporationGuesses(ctx, db.DeleteTypingCorporationGuessesParams{Box: box, TypingPriority: wantTyping, SupersededAt: at}); err != nil {
		logging.Errorf("typing: drop corporation guesses for box %q: %v", box, err)
	}
	if err := app.queries.DeleteTypingAllianceGuesses(ctx, db.DeleteTypingAllianceGuessesParams{Box: box, TypingPriority: wantTyping, SupersededAt: at}); err != nil {
		logging.Errorf("typing: drop alliance guesses for box %q: %v", box, err)
	}
	if err := app.queries.DeleteTypingHistoryGuesses(ctx, db.DeleteTypingHistoryGuessesParams{Box: box, TypingPriority: wantTyping, SupersededAt: at}); err != nil {
		logging.Errorf("typing: drop history guesses for box %q: %v", box, err)
	}
	if err := app.queries.DeleteTypingDetailGuesses(ctx, db.DeleteTypingDetailGuessesParams{Box: box, TypingPriority: wantTyping, SupersededAt: at}); err != nil {
		logging.Errorf("typing: drop detail guesses for box %q: %v", box, err)
	}
	app.pruneTypingLedger(ctx, now)
}

// pruneTypingLedger drops guesses older than typingLedgerKeep,
// at most once an hour.
func (app *Application) pruneTypingLedger(ctx context.Context, now time.Time) {
	last := app.typingPrunedAt.Load()
	if now.Unix()-last < 3600 || !app.typingPrunedAt.CompareAndSwap(last, now.Unix()) {
		return
	}
	if err := app.queries.PruneTypingGuesses(ctx, now.Add(-typingLedgerKeep)); err != nil {
		logging.Errorf("typing: prune guess ledger: %v", err)
	}
}

// noteTypingGuess records one guess in the box's ledger. A guess
// the box is already making is left as it is: it is still the
// same guess. The queue row goes with it (see the noteTyping*
// helpers): ledger without a queue row would never drain, and a
// queue row without ledger would never be replaced.
func (app *Application) noteTypingGuess(ctx context.Context, box string, kind pageWantKind, entityID, regionID int64, now time.Time) {
	if err := app.queries.InsertTypingGuess(ctx, db.InsertTypingGuessParams{
		Box: box, Kind: string(kind), EntityID: entityID, RegionID: regionID, NotedAt: now,
	}); err != nil {
		logging.Errorf("typing: note %s guess %d for box %q: %v", kind, entityID, box, err)
	}
}

// noteTypingPilotGuess queues a pilot record a box guessed. The
// GREATEST keeps a ring the pilot earned by opening it.
func (app *Application) noteTypingPilotGuess(ctx context.Context, box string, id int64, now time.Time) {
	app.noteTypingGuess(ctx, box, pageWantPilot, id, 0, now)
	if err := app.queries.UpsertPilotWant(ctx, db.UpsertPilotWantParams{
		CharacterID: id, Priority: wantTyping, NotedAt: timeSet(now),
	}); err != nil {
		logging.Errorf("typing: note pilot guess %d for box %q: %v", id, box, err)
	}
}

// noteTypingCorporationGuess queues a corporation record a box guessed.
func (app *Application) noteTypingCorporationGuess(ctx context.Context, box string, id int64, now time.Time) {
	app.noteTypingGuess(ctx, box, pageWantCorporation, id, 0, now)
	if err := app.queries.UpsertCorporationWant(ctx, db.UpsertCorporationWantParams{
		CorporationID: id, Priority: wantTyping, NotedAt: timeSet(now),
	}); err != nil {
		logging.Errorf("typing: note corporation guess %d for box %q: %v", id, box, err)
	}
}

// noteTypingAllianceGuess queues an alliance record a box guessed.
func (app *Application) noteTypingAllianceGuess(ctx context.Context, box string, id int64, now time.Time) {
	app.noteTypingGuess(ctx, box, pageWantAlliance, id, 0, now)
	if err := app.queries.UpsertAllianceWant(ctx, db.UpsertAllianceWantParams{
		AllianceID: id, Priority: wantTyping, NotedAt: timeSet(now),
	}); err != nil {
		logging.Errorf("typing: note alliance guess %d for box %q: %v", id, box, err)
	}
}

// noteTypingHistoryGuess queues a price history a box guessed.
func (app *Application) noteTypingHistoryGuess(ctx context.Context, box string, regionID, typeID int64, now time.Time) {
	app.noteTypingGuess(ctx, box, pageWantHistory, typeID, regionID, now)
	if err := app.queries.UpsertMarketHistoryWant(ctx, db.UpsertMarketHistoryWantParams{
		RegionID: regionID, TypeID: typeID, LastRequestedAt: now, Priority: wantTyping,
	}); err != nil {
		logging.Errorf("typing: note history guess %d in region %d for box %q: %v", typeID, regionID, box, err)
	}
}

// noteTypingDetailGuess queues an item description a box guessed.
func (app *Application) noteTypingDetailGuess(ctx context.Context, box string, typeID int64, now time.Time) {
	app.noteTypingGuess(ctx, box, pageWantTypeDescription, typeID, 0, now)
	if err := app.queries.UpsertTypeDetailWant(ctx, db.UpsertTypeDetailWantParams{
		TypeID: typeID, Priority: wantTyping, NotedAt: timeSet(now),
	}); err != nil {
		logging.Errorf("typing: note detail guess %d for box %q: %v", typeID, box, err)
	}
}

// markGuessHit records a guess the pilot opened: called from the
// entity page handlers (warm or cold — the open is what counts),
// never from the guessing itself. Only the first open inside the
// window counts.
func (app *Application) markGuessHit(ctx context.Context, kind pageWantKind, entityID, regionID int64) {
	if isPrefetch(ctx) {
		return // a prefetch is not an open (core_prefetch.go)
	}
	now := time.Now().UTC()
	if err := app.queries.MarkTypingGuessHits(ctx, db.MarkTypingGuessHitsParams{
		Kind: string(kind), EntityID: entityID, RegionID: regionID,
		Now: now, Since: now.Add(-typingGuessHitWindow),
	}); err != nil {
		logging.Errorf("typing: mark %s hit %d: %v", kind, entityID, err)
	}
}
