package esi

// ---------------------------------------------------------------------------
// Durable name misses (schema 100). The negative caches for names ESI
// has definitively refused (charNameMisses, typeIDMisses) used to
// live in-process only, so every restart re-asked every known-bad ID
// once: deleted characters (404), invalid ids (422), type ids the
// universe route refuses. Each miss is now written to esi_name_misses
// as well, and a process loads the recent ones the first time it
// asks. A miss is trusted for nameMissTrust, then asked once more.
// ---------------------------------------------------------------------------

import (
	"context"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// nameMissTrust is how long a stored miss is believed: long enough
// that restarts never re-ask, short enough that an id reused or
// wrongly refused is asked again within the month.
const nameMissTrust = 30 * 24 * time.Hour

const (
	nameMissCharacter = "character"
	nameMissType      = "type"
)

// loadNameMisses fills the negative caches from the stored misses,
// once per process. A failed read leaves them as they are: the
// worst case is the old behaviour, each bad id asked once more.
func (c *Client) loadNameMisses() {
	c.nameMissOnce.Do(func() {
		if c.queries == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		rows, err := c.queries.ListNameMissesSince(ctx, time.Now().Add(-nameMissTrust))
		if err != nil {
			logging.Errorf("esi: load name misses: %v", err)
			return
		}
		c.charNamesMu.Lock()
		for _, r := range rows {
			if r.Kind == nameMissCharacter {
				c.charNameMisses[r.EntityID] = true
			}
		}
		c.charNamesMu.Unlock()
		c.typeIDMissMu.Lock()
		if c.typeIDMisses == nil {
			c.typeIDMisses = make(map[int64]bool)
		}
		for _, r := range rows {
			if r.Kind == nameMissType {
				c.typeIDMisses[r.EntityID] = true
			}
		}
		c.typeIDMissMu.Unlock()
	})
}

// storeNameMiss writes one miss to the durable table, best effort.
func (c *Client) storeNameMiss(kind string, id int64) {
	if c.queries == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.queries.UpsertNameMiss(ctx, db.UpsertNameMissParams{
		Kind: kind, EntityID: id, MissedAt: time.Now().UTC(),
	}); err != nil {
		logging.Errorf("esi: store %s name miss %d: %v", kind, id, err)
	}
}

// TypeIDMissed reports whether ESI has definitively refused to name
// this type id (a 400, 404 or 422 from /universe/types/{id}/).
func (c *Client) TypeIDMissed(id int64) bool {
	c.loadNameMisses()
	c.typeIDMissMu.Lock()
	defer c.typeIDMissMu.Unlock()
	return c.typeIDMisses[id]
}

// StoreTypeIDMiss records a definitive refusal for a type id, in
// process and in the durable table, so no pass asks about it again.
func (c *Client) StoreTypeIDMiss(id int64) {
	c.loadNameMisses()
	c.typeIDMissMu.Lock()
	if c.typeIDMisses == nil {
		c.typeIDMisses = make(map[int64]bool)
	}
	known := c.typeIDMisses[id]
	c.typeIDMisses[id] = true
	c.typeIDMissMu.Unlock()
	if !known {
		c.storeNameMiss(nameMissType, id)
	}
}
