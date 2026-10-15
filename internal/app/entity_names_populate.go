package app

// `evesynapse -populate-names`: fills entity_names in bulk from ESI so
// every alliance and every corporation the app can reach has its name
// stored before anyone asks: each alliance's members, its creator and
// executor corporations (often in no alliance any more), the NPC
// corporations, and every corporation ID already in the database. ESI
// has no list of all corporations, so a player corporation in no
// alliance that the app has never met still arrives the usual way. A starting server runs it once by itself when the
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

	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// populateCorpFetchers is how many alliances are read at once (their
// member list and their record).
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

// populateNames resolves every alliance and every corporation it can
// reach (see the file comment). Names reach entity_names through the
// client's name sink.
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
	fmt.Fprintf(out, "%d alliance names stored. Reading each alliance's corporations…\n", len(allianceNames))

	// Member lists and alliance records (creator and executor
	// corporations), a few alliances at a time.
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
					ids = nil // one alliance short; the next run or the worker fills it
				}
				var rec esi.Alliance
				if err := app.esi.Get(ctx, "", fmt.Sprintf("/alliances/%d/", id), &rec); err == nil {
					ids = append(ids, rec.CreatorCorporationID, rec.ExecutorCorporationID)
				}
				mu.Lock()
				for _, c := range ids {
					if c > 0 {
						corps[c] = true
					}
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

	fromAlliances := len(corps)

	// The NPC corporations: a list ESI does publish.
	var npc []int64
	if err := app.esi.Get(ctx, "", "/corporations/npccorps/", &npc); err != nil {
		fmt.Fprintf(out, "  NPC corporation list unavailable (%v); skipping them\n", err)
	}
	for _, c := range npc {
		corps[c] = true
	}

	// Every corporation ID the app already holds anywhere.
	known, err := app.knownCorporationIDs(ctx)
	if err != nil {
		fmt.Fprintf(out, "  stored corporation IDs unavailable (%v); skipping them\n", err)
	}
	for _, c := range known {
		corps[c] = true
	}

	ids := make([]int64, 0, len(corps))
	for id := range corps {
		ids = append(ids, id)
	}
	fmt.Fprintf(out, "%d corporations (%d from alliances, %d NPC, %d already known here); resolving their names…\n",
		len(ids), fromAlliances, len(npc), len(known))
	corpNames, limited := app.resolveUniverseNamesLimited(ctx, unlimited, ids)
	if limited {
		return fmt.Errorf("ESI error limit reached resolving corporation names (%d stored so far); run again in a minute", len(corpNames))
	}
	fmt.Fprintf(out, "Done in %s: %d alliance and %d corporation names stored.\n",
		time.Since(start).Round(time.Second), len(allianceNames), len(corpNames))
	return nil
}

// knownCorporationIDs is every corporation ID stored anywhere in the
// database: public records (and the queue behind them), characters'
// corporations, structure owners, and corporation features (ops,
// ship replacement, doctrines, Discord servers).
func (app *Application) knownCorporationIDs(ctx context.Context) ([]int64, error) {
	rows, err := app.db.QueryContext(ctx, `
		SELECT corporation_id FROM corporation_records
		UNION SELECT corporation_id FROM character_corporations
		UNION SELECT owner_corporation_id FROM structure_context
		UNION SELECT corporation_id FROM ops
		UNION SELECT corporation_id FROM srp_requests
		UNION SELECT corporation_id FROM doctrines`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if id > 0 {
			ids = append(ids, id)
		}
	}
	return ids, rows.Err()
}
