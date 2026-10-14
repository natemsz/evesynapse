package fit

import (
	"context"
	"os"
	"testing"

	"evesynapse/internal/pgtest"
	"evesynapse/internal/store"
)

// TestMain wires the package's tests to the shared embedded Postgres
// (started lazily by the first pgtest.FreshDSN call, stopped on the
// way out). Every test here opens the full schema, so it is built once
// in a template database and each test's database is a copy of it.
func TestMain(m *testing.M) {
	os.Exit(pgtest.TestMainWithTemplate(m, func(dsn string) error {
		conn, pool, err := store.Open(context.Background(), dsn)
		if err != nil {
			return err
		}
		conn.Close()
		pool.Close()
		return nil
	}))
}
