// Package pgtest gives EveSynapse's test binaries one shared
// embedded Postgres 16 per test process (the same major version
// the live box runs), started lazily on the first FreshDSN call,
// so `go test ./...` self-provisions with no external database
// and CI needs no service. Each test gets its own empty database
// on that server; the app's store.Open applies the schema on first
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
	// runtimeDir is where the embedded server unpacks its binaries
	// and keeps its data for this test process.
	runtimeDir string
	// managed is set once TestMain is running the package's tests,
	// which is what guarantees the server gets stopped afterwards.
	managed atomic.Bool

	// The template database (TestMainWithTemplate): the package's
	// schema built once, cloned for every FreshDSN instead of
	// migrating an empty database per test.
	prepare      func(dsn string) error
	templateOnce sync.Once
	templateErr  error

	// adminPool is one connection pool to the server's maintenance
	// database, shared by every create and drop.
	adminPool *pgxpool.Pool

	// clones holds databases already copied from the template: one
	// producer goroutine keeps it topped up while tests run, so a
	// test usually takes a ready one instead of waiting on a copy.
	clones     chan clone
	stopClones chan struct{}
	clonesDone chan struct{}
)

// drops queues finished tests' databases for the background dropper.
var (
	drops    chan string
	dropOnce sync.Once
)

// clone is one prepared copy of the template, or why it failed.
type clone struct {
	name string
	err  error
}

// clonePrefetch is how many copies wait ready ahead of the tests;
// cloneProducers copy in parallel (a copy is mostly waiting on disk).
const (
	clonePrefetch  = 4
	cloneProducers = 3
)

// TestMain is the shared TestMain body for packages that use FreshDSN:
// run the tests, then stop the embedded server and remove its runtime
// directory (about 150 MB unpacked).
//
// Every package whose tests call FreshDSN or DSNFor needs a TestMain
// that calls this one (see internal/app/main_test.go). Without it
// nothing would stop the server, so those calls refuse to start one.
func TestMain(m *testing.M) int {
	return TestMainWithTemplate(m, nil)
}

// templateName is the database FreshDSN clones once prepared.
const templateName = "evetest_template"

// TestMainWithTemplate is TestMain for packages whose tests all open
// the full schema: prepare runs once, on the first FreshDSN, against a
// template database (typically store.Open, closed again), and every
// FreshDSN after that is a copy of it — a file copy, not 40-odd schema
// steps per test. Packages that test the migrations themselves use
// TestMain and get empty databases.
func TestMainWithTemplate(m *testing.M, prep func(dsn string) error) int {
	prepare = prep
	managed.Store(true)
	code := m.Run()
	if stopClones != nil {
		close(stopClones)
		<-clonesDone
	}
	if adminPool != nil {
		adminPool.Close()
	}
	if server != nil {
		_ = server.Stop()
	}
	if runtimeDir != "" {
		_ = os.RemoveAll(runtimeDir)
	}
	return code
}

// FreshDSN creates a database for one test on the shared embedded
// server — empty, or a copy of the template under
// TestMainWithTemplate — and registers its drop. Cleanup runs after the
// test's own cleanups (LIFO), so handles opened on the DSN should
// be closed by cleanups the test registers after this call;
// DROP ... WITH (FORCE) covers any stragglers regardless.
func FreshDSN(t *testing.T) string {
	t.Helper()
	startServer(t)
	var name string
	if prepare != nil {
		ensureTemplate(t)
		c := <-clones
		if c.err != nil {
			t.Fatalf("pgtest: copy template: %v", c.err)
		}
		name = c.name
	} else {
		name = fmt.Sprintf("evetest_%d", dbSeq.Add(1))
		if _, err := adminPool.Exec(context.Background(), "CREATE DATABASE "+name); err != nil {
			t.Fatalf("pgtest: create database %s: %v", name, err)
		}
	}
	t.Cleanup(func() { dropDatabase(name) })
	return fmt.Sprintf("postgres://postgres:postgres@127.0.0.1:%d/%s?sslmode=disable", port, name)
}

// ensureTemplate builds the template database once — created empty,
// handed to prepare, then left with no connections so it can be
// copied — and starts the producer that keeps copies ready.
func ensureTemplate(t *testing.T) {
	t.Helper()
	templateOnce.Do(func() {
		ctx := context.Background()
		if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+templateName); err != nil {
			templateErr = fmt.Errorf("create template: %w", err)
			return
		}
		dsn := fmt.Sprintf("postgres://postgres:postgres@127.0.0.1:%d/%s?sslmode=disable", port, templateName)
		if err := prepare(dsn); err != nil {
			templateErr = fmt.Errorf("prepare template: %w", err)
			return
		}
		// Anything prepare left open would block every copy.
		_, _ = adminPool.Exec(ctx, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()", templateName)
		clones = make(chan clone, clonePrefetch)
		stopClones = make(chan struct{})
		clonesDone = make(chan struct{})
		var producers sync.WaitGroup
		for i := 0; i < cloneProducers; i++ {
			producers.Add(1)
			go func() {
				defer producers.Done()
				produceClones()
			}()
		}
		go func() {
			producers.Wait()
			close(clonesDone)
		}()
	})
	if templateErr != nil {
		t.Fatalf("pgtest: %v", templateErr)
	}
}

// produceClones copies the template ahead of demand until stopped.
// Copies left over at the end go with the server's runtime directory.
func produceClones() {
	for {
		name := fmt.Sprintf("evetest_%d", dbSeq.Add(1))
		_, err := adminPool.Exec(context.Background(), "CREATE DATABASE "+name+" TEMPLATE "+templateName)
		select {
		case clones <- clone{name: name, err: err}:
		case <-stopClones:
			return
		}
	}
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
	if !managed.Load() {
		t.Fatalf("pgtest: this package has no TestMain calling pgtest.TestMain, so the embedded Postgres it starts would be left running. Add one: see internal/app/main_test.go.")
	}
	startOnce.Do(func() {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			startErr = err
			return
		}
		port = ln.Addr().(*net.TCPAddr).Port
		_ = ln.Close()

		runtimeDir = filepath.Join(os.TempDir(), fmt.Sprintf("evesynapse-pgtest-%d", os.Getpid()))

		// First start downloads the Postgres binaries; a
		// transient fetch failure (CI runners share egress
		// addresses that Maven Central sometimes throttles)
		// must not fail the whole suite, so retry a few
		// times with a fresh instance before giving up.
		var lastErr error
		for attempt := 1; attempt <= 4; attempt++ {
			cfg := embeddedpostgres.DefaultConfig().
				Version(embeddedpostgres.V16).
				Port(uint32(port)).
				Database("postgres").
				RuntimePath(runtimeDir).
				// A throwaway server: nothing it holds must survive a
				// crash, so it skips the durability work.
				StartParameters(map[string]string{
					"fsync":              "off",
					"synchronous_commit": "off",
					"full_page_writes":   "off",
				})
			candidate := embeddedpostgres.NewDatabase(cfg)
			if err := candidate.Start(); err != nil {
				lastErr = err
				_ = candidate.Stop()
				if attempt < 4 {
					time.Sleep(10 * time.Second)
				}
				continue
			}
			server = candidate
			lastErr = nil
			break
		}
		if lastErr != nil {
			startErr = fmt.Errorf("start embedded postgres: %w", lastErr)
			server = nil
			return
		}
		adminDSN = fmt.Sprintf("postgres://postgres:postgres@127.0.0.1:%d/postgres?sslmode=disable", port)
		pool, err := pgxpool.New(context.Background(), adminDSN)
		if err != nil {
			startErr = fmt.Errorf("admin pool: %w", err)
			return
		}
		adminPool = pool
	})
	if startErr != nil {
		t.Fatalf("pgtest: %v", startErr)
	}
	return adminDSN
}

// dropDatabase hands a finished test's database to the background
// dropper: a drop waits on the disk, and no test should wait on the
// one before it. Whatever is still queued when the tests end goes
// with the server's runtime directory.
func dropDatabase(name string) {
	dropOnce.Do(func() {
		drops = make(chan string, 1024)
		go func() {
			for name := range drops {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				_, _ = adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
				cancel()
			}
		}()
	})
	select {
	case drops <- name:
	default:
		// A full queue: drop inline rather than leak the database.
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _ = adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	}
}
