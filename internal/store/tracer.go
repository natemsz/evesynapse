package store

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"evesynapse/internal/logging"
)

// slowQueryThreshold is how long a query runs before it is logged.
const slowQueryThreshold = 500 * time.Millisecond

// slowQuerySQLMax caps the SQL text on a slow-query line, in bytes.
const slowQuerySQLMax = 500

// handleStats is a connection pool's load at one moment.
type handleStats struct{ inUse, idle, max int }

// slowQueryTracer is a pgx QueryTracer that logs, at warn level, every
// query that runs for slowQueryThreshold or longer, with the load of
// the handle it ran on, so a query slowed by a saturated pool reads
// differently from one slowed by a bad plan. Faster queries log
// nothing. It logs the SQL and the argument count, never argument
// values: they can carry user data.
type slowQueryTracer struct {
	handle string
	stats  atomic.Pointer[func() handleStats]
}

type queryStartKey struct{}

type queryStart struct {
	at   time.Time
	sql  string
	args int
}

// setStats names the function that reads the handle's load. The handle
// is built from the config that carries the tracer, so it is set after.
func (t *slowQueryTracer) setStats(f func() handleStats) { t.stats.Store(&f) }

func (t *slowQueryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, queryStartKey{}, queryStart{at: time.Now(), sql: d.SQL, args: len(d.Args)})
}

func (t *slowQueryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryEndData) {
	st, ok := ctx.Value(queryStartKey{}).(queryStart)
	if !ok {
		return
	}
	t.observe(time.Since(st.at), st.sql, st.args, d.Err)
}

func (t *slowQueryTracer) observe(took time.Duration, sql string, args int, err error) {
	if took < slowQueryThreshold {
		return
	}
	var load handleStats
	if f := t.stats.Load(); f != nil {
		load = (*f)()
	}
	logging.Warnf("slow query: %s on %s (pool: %d in use, %d idle, %d max; %d args%s): %s",
		took.Round(time.Millisecond), t.handle, load.inUse, load.idle, load.max,
		args, queryOutcome(err), clipSQL(sql))
}

// queryOutcome names how a slow query failed, if it did. It gives the
// SQLSTATE rather than the server's message, which can quote values.
func queryOutcome(err error) string {
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		return ""
	case errors.As(err, &pgErr):
		return "; failed " + pgErr.Code
	case errors.Is(err, context.Canceled):
		return "; canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "; deadline exceeded"
	default:
		return "; failed"
	}
}

// clipSQL folds a query onto one line and cuts it to slowQuerySQLMax
// bytes, on a character boundary. sqlc's "-- name: X" comment leads
// generated queries, so the query's name survives the cut.
func clipSQL(sql string) string {
	s := strings.Join(strings.Fields(sql), " ")
	if len(s) <= slowQuerySQLMax {
		return s
	}
	cut := slowQuerySQLMax
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}
