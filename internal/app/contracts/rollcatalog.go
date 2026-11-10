package contracts

import (
	"context"
	"sync"
	"time"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// The roll sliders' catalog rows (every abyssal family's rollable
// attributes with their base modules) come from a join over the
// whole abyssal corpus and the SDE. It changes only when a new
// family, mutaplasmid or base module is first seen, so the rows
// are held in memory and re-read by the worker; a search render
// never runs the join.

// rollCatalogMaxAge is how old the held rows may get before the
// worker reads them again.
const rollCatalogMaxAge = 30 * time.Minute

type rollCatalogRows struct {
	mu     sync.Mutex
	rows   []db.ListAbyssalFamilyMetaVariantsRow
	readAt time.Time
}

// rollCatalogs holds one application's rows, keyed by its query
// handle.
var rollCatalogs sync.Map // *db.Queries -> *rollCatalogRows

func rollCatalogFor(q *db.Queries) *rollCatalogRows {
	if c, ok := rollCatalogs.Load(q); ok {
		return c.(*rollCatalogRows)
	}
	c, _ := rollCatalogs.LoadOrStore(q, &rollCatalogRows{})
	return c.(*rollCatalogRows)
}

// read runs the join and holds its rows.
func (c *rollCatalogRows) read(ctx context.Context, q *db.Queries) error {
	rows, err := q.ListAbyssalFamilyMetaVariants(ctx)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.rows, c.readAt = rows, time.Now()
	c.mu.Unlock()
	return nil
}

// familyMetaVariants is the held catalog rows. Before the worker's
// first read (a fresh start) the caller's render reads them once.
func (app *mod) familyMetaVariants(ctx context.Context) ([]db.ListAbyssalFamilyMetaVariantsRow, error) {
	c := rollCatalogFor(app.queries)
	c.mu.Lock()
	rows, held := c.rows, !c.readAt.IsZero()
	c.mu.Unlock()
	if held {
		return rows, nil
	}
	if err := c.read(ctx, app.queries); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rows, nil
}

// refreshRollCatalog is the worker job: it re-reads the rows once
// they are older than rollCatalogMaxAge, off the cycle.
func refreshRollCatalog(ctx context.Context, deps module.Deps) {
	q := deps.DB()
	c := rollCatalogFor(q)
	c.mu.Lock()
	fresh := !c.readAt.IsZero() && time.Since(c.readAt) < rollCatalogMaxAge
	c.mu.Unlock()
	if fresh {
		return
	}
	deps.Background(func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		if err := c.read(rctx, q); err != nil {
			logging.Errorf("contracts: roll catalog: %v", err)
		}
	})
}
