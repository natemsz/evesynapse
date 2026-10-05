// Package pgtest gives EveSynapse's test binaries one shared
// embedded Postgres 16 per test process (the same major version
// the live box runs), started lazily on the first FreshDSN call,
// so `go test ./...` self-provisions with no external database
// and CI needs no service. Each test gets its own empty database
// on that server; the app's openDB applies the schema on first
// open, exactly like a fresh install.
package pgtest

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	startOnce sync.Once
	server    *embeddedpostgres.EmbeddedPostgres
	adminDSN  string
	port      int
	startErr  error
	dbSeq     atomic.Int64
)

// TestMain is the shared TestMain body for packages that use
// FreshDSN: run the tests, then stop the embedded server.
func TestMain(m *testing.M) int {
	code := m.Run()
	if server != nil {
		_ = server.Stop()
	}
	return code
}

// FreshDSN creates an empty database for one test on the shared
// embedded server and registers its drop. Cleanup runs after the
// test's own cleanups (LIFO), so handles opened on the DSN should
// be closed by cleanups the test registers after this call;
// DROP ... WITH (FORCE) covers any stragglers regardless.
func FreshDSN(t *testing.T) string {
	t.Helper()
	admin := startServer(t)

	name := fmt.Sprintf("evetest_%d", dbSeq.Add(1))
	pool, err := pgxpool.New(context.Background(), admin)
	if err != nil {
		t.Fatalf("pgtest: admin pool: %v", err)
	}
	if _, err := pool.Exec(context.Background(), "CREATE DATABASE "+name); err != nil {
		pool.Close()
		t.Fatalf("pgtest: create database %s: %v", name, err)
	}
	pool.Close()

	t.Cleanup(func() { dropDatabase(name) })
	return fmt.Sprintf("postgres://postgres:postgres@127.0.0.1:%d/%s?sslmode=disable", port, name)
}

// DSNFor returns the connection URL for a database name on the
// shared embedded server without creating it (the
// missing-database tests point a DSN at a name that was never
// minted).
func DSNFor(t *testing.T, name string) string {
	t.Helper()
	startServer(t)
	return fmt.Sprintf("postgres://postgres:postgres@127.0.0.1:%d/%s?sslmode=disable", port, name)
}

func startServer(t *testing.T) string {
	t.Helper()
	startOnce.Do(func() {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			startErr = err
			return
		}
		port = ln.Addr().(*net.TCPAddr).Port
		_ = ln.Close()

		cfg := embeddedpostgres.DefaultConfig().
			Version(embeddedpostgres.V16).
			Port(uint32(port)).
			Database("postgres").
			RuntimePath(filepath.Join(os.TempDir(), fmt.Sprintf("evesynapse-pgtest-%d", os.Getpid())))
		server = embeddedpostgres.NewDatabase(cfg)
		if err := server.Start(); err != nil {
			startErr = fmt.Errorf("start embedded postgres: %w", err)
			server = nil
			return
		}
		adminDSN = fmt.Sprintf("postgres://postgres:postgres@127.0.0.1:%d/postgres?sslmode=disable", port)
	})
	if startErr != nil {
		t.Fatalf("pgtest: %v", startErr)
	}
	return adminDSN
}

func dropDatabase(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		return
	}
	defer pool.Close()
	_, _ = pool.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
}
