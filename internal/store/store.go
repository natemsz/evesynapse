// Package store is the app's database: it opens Postgres, brings the
// schema up to date, and hands back the connections everything else
// runs on. The schema itself lives here too, as the numbered steps in
// schema_pg (schema.go lists and applies them). Queries are not in
// this package: sqlc generates them into internal/db/sqlc from the
// same schema files.
package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib" // pgx as a database/sql driver
)

// Open opens Postgres at dsn and returns the two handles the app
// runs on: a database/sql DB over the pgx stdlib driver (every
// sqlc query and every hand-rolled statement rides it) and a
// pgxpool that exists only to back the scs session store
// (pgxstore). It then brings the schema up to date (migrateSchema):
// a fresh database gets the collapsed Postgres baseline
// (schema_pg/001_baseline.sql) and every later numbered step, so a
// fresh install comes up with the full schema on first boot, and an
// existing install gains whichever steps it is missing.
func Open(ctx context.Context, dsn string) (*sql.DB, *pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, nil, err
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		pool.Close()
		return nil, nil, err
	}
	conn := stdlib.OpenDB(*cfg, stdlib.OptionAfterConnect(readTimesInUTC))
	conn.SetMaxOpenConns(20)
	if err := conn.PingContext(ctx); err != nil {
		conn.Close()
		pool.Close()
		return nil, nil, err
	}
	if err := migrateSchema(ctx, conn); err != nil {
		conn.Close()
		pool.Close()
		return nil, nil, err
	}
	return conn, pool, nil
}

// readTimesInUTC runs on every new connection and makes each
// timestamptz it reads come back in UTC. Left alone the driver hands
// times back in the machine's own zone, so a page would print one
// thing on a UTC server and another on a laptop. EVE runs on UTC and
// so does everything here; the instant itself is the same either way.
func readTimesInUTC(_ context.Context, c *pgx.Conn) error {
	c.TypeMap().RegisterType(&pgtype.Type{
		Name:  "timestamptz",
		OID:   pgtype.TimestamptzOID,
		Codec: &pgtype.TimestamptzCodec{ScanLocation: time.UTC},
	})
	return nil
}
