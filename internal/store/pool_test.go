package store

import (
	"context"
	"database/sql"
	"sync"
	"testing"

	"evesynapse/internal/pgtest"
)

// TestSizePool: Open holds up to the default number of connections and
// SizePool changes that; and connections handed back are kept, not
// closed down to database/sql's two.
func TestSizePool(t *testing.T) {
	ctx := context.Background()
	conn, pool, err := Open(ctx, pgtest.FreshDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close(); conn.Close() })
	if got := conn.Stats().MaxOpenConnections; got != DefaultPoolSize {
		t.Fatalf("Open allows %d connections, want %d", got, DefaultPoolSize)
	}
	SizePool(conn, 8)
	if got := conn.Stats().MaxOpenConnections; got != 8 {
		t.Fatalf("after SizePool(8): %d connections allowed", got)
	}

	// Six at once, all handed back: all six are still there.
	held := make([]*sql.Conn, 6)
	var wg sync.WaitGroup
	for i := range held {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := conn.Conn(ctx)
			if err != nil {
				t.Error(err)
				return
			}
			held[i] = c
		}()
	}
	wg.Wait()
	for _, c := range held {
		if c != nil {
			c.Close()
		}
	}
	if stats := conn.Stats(); stats.Idle != 6 || stats.MaxIdleClosed != 0 {
		t.Fatalf("after six were handed back: %d idle, %d closed for being idle; want all six kept", stats.Idle, stats.MaxIdleClosed)
	}
}
