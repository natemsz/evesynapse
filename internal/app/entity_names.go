package app

// ---------------------------------------------------------------------------
// Entity names: every character, corporation and alliance name the
// app has learned, kept in entity_names (schema 047). Names barely
// change, so they are always to hand:
//
//   - at startup the whole table loads into the ESI client's name
//     caches, so a restart serves names instantly;
//   - every name the caches learn or see change goes to the table
//     through a batching writer (the client's name sink), never a
//     write per name on a request path;
//   - pages that list many organizations ask the table once for the
//     names the caches lack (namesFor);
//   - `evesynapse -populate-names` fills the table in bulk from ESI:
//     every alliance and every corporation in one.
// ---------------------------------------------------------------------------

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// entityName is one learned name on its way to the table.
type entityName struct {
	Kind string
	ID   int64
	Name string
}

// Writer bounds: a batch goes out when it is this big or this old.
const (
	entityNameBatch    = 400
	entityNameFlushAge = 3 * time.Second
	entityNameQueue    = 8192
)

// nameWriter batches learned names into entity_names. Enqueue never
// blocks a caller: a full queue drops the name (it is still in the
// in-process cache and is learned again later).
type nameWriter struct {
	db     *sql.DB
	in     chan entityName
	done   chan struct{}
	mu     sync.RWMutex // guards closed against a send racing close
	closed bool
}

func newNameWriter(db *sql.DB) *nameWriter {
	w := &nameWriter{db: db, in: make(chan entityName, entityNameQueue), done: make(chan struct{})}
	go w.run()
	return w
}

func (w *nameWriter) enqueue(kind string, id int64, name string) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.closed {
		return
	}
	select {
	case w.in <- entityName{Kind: kind, ID: id, Name: name}:
	default:
	}
}

// close flushes what is queued and stops the writer; names heard
// after it are dropped.
func (w *nameWriter) close() {
	w.mu.Lock()
	if !w.closed {
		w.closed = true
		close(w.in)
	}
	w.mu.Unlock()
	<-w.done
}

func (w *nameWriter) run() {
	defer close(w.done)
	tick := time.NewTicker(entityNameFlushAge)
	defer tick.Stop()
	batch := make([]entityName, 0, entityNameBatch)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := upsertEntityNames(context.Background(), w.db, batch); err != nil {
			logging.Errorf("names: store %d names: %v", len(batch), err)
		}
		batch = batch[:0]
	}
	for {
		select {
		case n, ok := <-w.in:
			if !ok {
				flush()
				return
			}
			batch = append(batch, n)
			if len(batch) >= entityNameBatch {
				flush()
			}
		case <-tick.C:
			flush()
		}
	}
}

// upsertEntityNames writes names in one statement per batch; a row
// is rewritten only when its name changed.
func upsertEntityNames(ctx context.Context, db *sql.DB, names []entityName) error {
	if len(names) == 0 {
		return nil
	}
	// One row per key per statement: ON CONFLICT cannot touch a row
	// twice. The last name heard wins.
	last := make(map[[2]any]int, len(names))
	for i, n := range names {
		last[[2]any{n.Kind, n.ID}] = i
	}
	var b strings.Builder
	b.WriteString("INSERT INTO entity_names (kind, id, name) VALUES ")
	args := make([]any, 0, 3*len(last))
	for i, n := range names {
		if last[[2]any{n.Kind, n.ID}] != i {
			continue
		}
		if len(args) > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "($%d, $%d, $%d)", len(args)+1, len(args)+2, len(args)+3)
		args = append(args, n.Kind, n.ID, n.Name)
	}
	b.WriteString(" ON CONFLICT (kind, id) DO UPDATE SET name = excluded.name, updated_at = now()" +
		" WHERE entity_names.name <> excluded.name")
	_, err := db.ExecContext(ctx, b.String(), args...)
	return err
}

// loadEntityNames primes the ESI client's caches with the whole table.
func (app *Application) loadEntityNames(ctx context.Context) {
	start := time.Now()
	rows, err := app.db.QueryContext(ctx, "SELECT kind, id, name FROM entity_names")
	if err != nil {
		logging.Errorf("names: load: %v", err)
		return
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var n entityName
		if err := rows.Scan(&n.Kind, &n.ID, &n.Name); err != nil {
			logging.Errorf("names: load row: %v", err)
			return
		}
		app.esi.PrimeName(n.Kind, n.ID, n.Name)
		count++
	}
	if err := rows.Err(); err != nil {
		logging.Errorf("names: load: %v", err)
	}
	logging.Infof("names: %d character, corporation and alliance names loaded in %s", count, time.Since(start).Round(time.Millisecond))
}

// namesFor answers names of one kind for many IDs: the in-process
// cache first, then one query for the rest (whose answers prime the
// cache). IDs nobody has named yet are absent from the result.
func (app *Application) namesFor(ctx context.Context, kind string, ids []int64) map[int64]string {
	out := make(map[int64]string, len(ids))
	var missing []int64
	for _, id := range ids {
		var name string
		var ok bool
		switch kind {
		case esi.NameCorporation:
			name, ok = app.esi.CachedCorpName(id)
		case esi.NameAlliance:
			name, ok = app.esi.CachedAllianceName(id)
		case esi.NameCharacter:
			name, ok = app.esi.CachedCharacterName(id)
		}
		if ok && name != "" {
			out[id] = name
		} else {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 || app.db == nil {
		return out
	}
	var b strings.Builder
	b.WriteString("SELECT id, name FROM entity_names WHERE kind = $1 AND id IN (")
	args := []any{kind}
	for i, id := range missing {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "$%d", i+2)
		args = append(args, id)
	}
	b.WriteString(")")
	rows, err := app.db.QueryContext(ctx, b.String(), args...)
	if err != nil {
		logging.Errorf("names: look up %d %s names: %v", len(missing), kind, err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var name string
		if rows.Scan(&id, &name) == nil && name != "" {
			out[id] = name
			app.esi.PrimeName(kind, id, name)
		}
	}
	return out
}
