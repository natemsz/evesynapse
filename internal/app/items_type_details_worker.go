package app

// Item descriptions: the worker's drain of the type_details queue. The
// item page notes a want for a type whose description the static
// data lacks (items.go); this settles it from the static data, which
// is where ESI's type descriptions now live.

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// refreshTypeDetails drains the item-description wants the item
// details page notes (a type_details row with no fetched_at),
// storing the public type payload's description. A type ESI no
// longer knows settles with an empty description so the page
// stops asking. Called from refreshCycle.
func (app *Application) refreshTypeDetails(ctx context.Context, allowance *fetchBudget) (drained int, limited bool) {
	app.fetchMu.Lock()
	defer app.fetchMu.Unlock()

	now := time.Now().UTC()
	ids, err := app.queries.ListTypeDetailWants(ctx, db.ListTypeDetailWantsParams{
		TypingPriority: wantTyping,
		TypingAssumed:  typingAssumedHit,
		TypingWeight:   typingAssumedWeight,
		TypingSince:    now.Add(-typingLearnWindow),
		TypingLimit:    int64(app.typingSlice()),
		RowLimit:       maxTypeDetailsPerCycle,
	})
	if err != nil {
		logging.Errorf("worker: type details: list wants: %v", err)
		return 0, false
	}
	for _, id := range ids {
		if ctx.Err() != nil || !allowance.take() {
			break
		}
		settled, limited := app.fetchOneTypeDetail(ctx, id, now)
		if settled {
			drained++
		}
		if limited {
			return drained, true
		}
	}
	return drained, false
}

// fetchOneTypeDetail reads one type's description from the static
// data and stores it. A type the static data does not hold settles
// with an empty description so the page stops asking. limited is
// never set: the static data spends no ESI budget.
func (app *Application) fetchOneTypeDetail(ctx context.Context, id int64, now time.Time) (settled, limited bool) {
	description := ""
	row, err := app.queries.GetSDEType(ctx, id)
	switch {
	case err == nil:
		description = row.Description
	case !errors.Is(err, pgx.ErrNoRows):
		logging.Errorf("worker: type details: read type %d: %v", id, err)
		return false, false
	}
	if serr := app.queries.SetTypeDetail(ctx, db.SetTypeDetailParams{
		TypeID: id, Description: description, FetchedAt: timeSet(now),
	}); serr != nil {
		logging.Errorf("worker: type details: store %d: %v", id, serr)
		return false, false
	}
	return true, false
}
