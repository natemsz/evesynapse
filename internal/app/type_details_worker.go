package app

// Item descriptions: the worker's drain of the type_details queue. The
// item page notes a want for a type whose description the static
// data lacks (items.go); this fetches it from ESI and stores it.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
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
	ids, err := app.queries.ListTypeDetailWants(ctx, maxTypeDetailsPerCycle)
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

// fetchOneTypeDetail downloads one type's public payload and
// stores its description. A type ESI no longer knows settles
// with an empty description so the page stops asking. limited
// reports ESI's stop signal.
func (app *Application) fetchOneTypeDetail(ctx context.Context, id int64, now time.Time) (settled, limited bool) {
	var t esi.Type
	err := app.esi.Get(ctx, "", fmt.Sprintf("/universe/types/%d/", id), &t)
	switch {
	case err == nil:
		if serr := app.queries.SetTypeDetail(ctx, db.SetTypeDetailParams{
			TypeID: id, Description: t.Description, FetchedAt: timeSet(now),
		}); serr != nil {
			logging.Errorf("worker: type details: store %d: %v", id, serr)
			return false, false
		}
		return true, false
	case errors.Is(err, esi.ErrErrorLimit):
		logging.Warnf("worker: type details: ESI error limit hit fetching type %d; backing off", id)
		return false, true
	default:
		if code, has := esi.StatusCode(err); has && code == http.StatusNotFound {
			if serr := app.queries.SetTypeDetail(ctx, db.SetTypeDetailParams{
				TypeID: id, Description: "", FetchedAt: timeSet(now),
			}); serr != nil {
				logging.Errorf("worker: type details: settle miss %d: %v", id, serr)
				return false, false
			}
			return true, false
		}
		logging.Errorf("worker: type details: fetch type %d: %v", id, err)
		return false, false
	}
}
