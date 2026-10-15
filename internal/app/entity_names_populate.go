package app

// `evesynapse -populate-names`: fills entity_names in bulk from ESI so
// every alliance and every corporation in one has its name stored
// before anyone asks. A starting server runs it once by itself when the
// table is next to empty (a fresh install); run it by hand any time —
// it is safe while the server runs. Characters have no
// public list, so their names keep arriving as the app meets them.

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"evesynapse/internal/logging"
)

// populateCorpFetchers is how many alliance member lists are fetched
// at once.
const populateCorpFetchers = 6

// RunPopulateNames is the -populate-names command.
func RunPopulateNames(args []string, cfg Config, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintln(stderr, "Usage: evesynapse -populate-names")
		return 2
	}
	cfg.noWorker = true // the running server has the worker
	a, err := New(cfg)
	if err != nil {
		fmt.Fprintln(stderr, "evesynapse:", err)
		return 1
	}
	defer a.Close() // flushes the name writer
	ctx := context.Background()
	if err := a.populateNames(ctx, stdout); err != nil {
		fmt.Fprintln(stderr, "evesynapse: populate names:", err)
		return 1
	}
	return 0
}

// populateNamesFloor is the corporation-name count under which a
// starting server fills the table itself: a fresh install, or one
// from before entity_names.
const populateNamesFloor = 1000

// populateNamesIfEmpty runs the bulk fill once when the table is next
// to empty, logging its progress. Cancelled with the worker.
func (app *Application) populateNamesIfEmpty(ctx context.Context) {
	var n int
	if err := app.db.QueryRowContext(ctx, "SELECT count(*) FROM entity_names WHERE kind = 'corporation'").Scan(&n); err != nil || n >= populateNamesFloor {
		return
	}
	logging.Infof("names: %d corporation names stored; filling the table from ESI in the background", n)
	if err := app.populateNames(ctx, logWriter{}); err != nil && ctx.Err() == nil {
		logging.Warnf("names: background fill: %v (run evesynapse -populate-names to retry)", err)
	}
}

// logWriter turns the fill's progress lines into log lines.
type logWriter struct{}

func (logWriter) Write(p []byte) (int, error) {
	logging.Infof("names: %s", strings.TrimSpace(string(p)))
	return len(p), nil
}

// populateNames resolves every alliance and its member corporations.
// Names reach entity_names through the client's name sink.
func (app *Application) populateNames(ctx context.Context, out io.Writer) error {
	start := time.Now()
	var alliances []int64
	if err := app.esi.Get(ctx, "", "/alliances/", &alliances); err != nil {
		return fmt.Errorf("list alliances: %w", err)
	}
	fmt.Fprintf(out, "%d alliances; resolving their names…\n", len(alliances))
	unlimited := &fetchBudget{left: 1 << 30}
	allianceNames, limited := app.resolveUniverseNamesLimited(ctx, unlimited, alliances)
	if limited {
		return fmt.Errorf("ESI error limit reached resolving alliance names; run again in a minute")
	}
	fmt.Fprintf(out, "%d alliance names stored. Listing member corporations…\n", len(allianceNames))

	// Member lists, a few at a time.
	var (
		mu    sync.Mutex
		corps = make(map[int64]bool)
		next  = make(chan int64)
		wg    sync.WaitGroup
		done  int
	)
	for i := 0; i < populateCorpFetchers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range next {
				var ids []int64
				if err := app.esi.Get(ctx, "", fmt.Sprintf("/alliances/%d/corporations/", id), &ids); err != nil {
					continue // one alliance short; the next run or the worker fills it
				}
				mu.Lock()
				for _, c := range ids {
					corps[c] = true
				}
				done++
				if done%250 == 0 {
					fmt.Fprintf(out, "  %d of %d alliances listed\n", done, len(alliances))
				}
				mu.Unlock()
			}
		}()
	}
	for _, id := range alliances {
		if ctx.Err() != nil {
			break
		}
		next <- id
	}
	close(next)
	wg.Wait()

	ids := make([]int64, 0, len(corps))
	for id := range corps {
		ids = append(ids, id)
	}
	fmt.Fprintf(out, "%d member corporations; resolving their names…\n", len(ids))
	corpNames, limited := app.resolveUniverseNamesLimited(ctx, unlimited, ids)
	if limited {
		return fmt.Errorf("ESI error limit reached resolving corporation names (%d stored so far); run again in a minute", len(corpNames))
	}
	fmt.Fprintf(out, "Done in %s: %d alliance and %d corporation names stored.\n",
		time.Since(start).Round(time.Second), len(allianceNames), len(corpNames))
	return nil
}
