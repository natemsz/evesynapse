package app

// Close ordering: Close cancels the worker, waits for it to
// fully stop, then closes the database handles, so no worker
// pass ever queries a closed handle (an updater-driven restart
// would otherwise fill the log with "sql: database is closed").

import (
	"bytes"
	"context"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"evesynapse/internal/pgtest"
)

// syncLogBuf is a goroutine-safe log sink (the worker may log
// while the test reads).
type syncLogBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncLogBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncLogBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestCloseWaitsForWorkerBeforeClosingDB(t *testing.T) {
	logs := &syncLogBuf{}
	log.SetOutput(logs)
	defer log.SetOutput(log.Writer())

	cfg := Config{
		databaseURL: pgtest.FreshDSN(t),
		// Refuse-fast SDE source: the worker's first SDE
		// maintenance tick tries an import against it and
		// fails immediately instead of downloading anything.
		sdeBaseURL: "http://127.0.0.1:1/",
	}
	application, err := New(cfg)
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	closed := make(chan struct{})
	go func() {
		application.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(60 * time.Second):
		t.Fatal("Close did not return: worker never stopped")
	}

	// The worker fully stopped before Close returned.
	select {
	case <-application.workerDone:
	default:
		t.Fatal("workerDone not closed when Close returned")
	}
	// And only then did the handles close.
	if err := application.db.PingContext(context.Background()); err == nil {
		t.Fatal("db still open after Close")
	}
	if err := application.pool.Ping(context.Background()); err == nil {
		t.Fatal("pool still open after Close")
	}
	if strings.Contains(logs.String(), "database is closed") {
		t.Fatalf("shutdown logged the old failure mode:\n%s", logs.String())
	}
}
