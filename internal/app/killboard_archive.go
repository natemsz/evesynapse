package app

// ---------------------------------------------------------------------------
// Archive restore: the attacker restore's bulk source. EVE Ref
// publishes one archive a day of CCP's killmails, attackers included,
// back to 2007-12-05. ESI serves killmails one at a time under a
// 3,600 per 15 minutes budget that live ingest shares, so millions of
// kills missing their attacker rows would take weeks from it; a day's
// archive is one download of a few megabytes.
//
// One loop, newest day first, after the stats rebuild's first pass.
// For each day it lists the counted kills with no attacker rows; a
// day with none is passed over without a download. Each kill found
// in the day's file gets its attacker rows written back and its ships
// flown added to the daily aggregate (writeBackKillAttackers). A kill
// past retention keeps only that count: its attacker rows go again
// in the same transaction, so the tables never grow past what
// retention allows. The day cursor is stored with the day's writes,
// so each kill is counted once and a restart resumes where it
// stopped. A day the archive lacks is passed over; its kills inside
// retention are left for the ESI restore (killboard_restore.go).
// ---------------------------------------------------------------------------

import (
	"archive/tar"
	"compress/bzip2"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	goccyjson "github.com/goccy/go-json"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

const (
	// killArchiveTimeout bounds one day's download.
	killArchiveTimeout = 5 * time.Minute
	// killArchivePause is the gap between two downloads.
	killArchivePause = 2 * time.Second
	// killArchiveLag is how many days back the walk starts: the
	// newest days' files are published late, and live ingest
	// stores those kills with their attackers anyway.
	killArchiveLag = 2
	// killArchiveMaxMail bounds one killmail file in an archive.
	killArchiveMaxMail = 4 << 20
	// killArchiveUserAgent names the reader to the archive host.
	killArchiveUserAgent = "EveSynapse killmail archive reader (+https://evesynapse.app)"
)

// killArchiveFirstDay is the first day the archives cover.
var killArchiveFirstDay = time.Date(2007, 12, 5, 0, 0, 0, 0, time.UTC)

// errKillStatsRebuilding is returned while a stats rebuild runs:
// write-backs wait for it rather than count into tables about to be
// replaced.
var errKillStatsRebuilding = errors.New("kill stats rebuild running")

// restoredKill is one killmail read back from a source, ready to
// write.
type restoredKill struct {
	id  int64
	at  time.Time
	km  killmailFull
	raw []byte
}

// runKillArchiveRestore is the archive loop. It returns once the walk
// reaches the first archive day, or when ctx ends.
func (app *Application) runKillArchiveRestore(ctx context.Context) {
	base := app.cfg.KillArchiveBaseURL()
	if base == "" {
		return
	}
	client := &http.Client{Timeout: killArchiveTimeout}
	days, kills := 0, 0
	for ctx.Err() == nil {
		if !app.killStatsRebuilt(ctx) || app.killStatsRebuilding.Load() {
			if !killfeedSleepCtx(ctx, killRestoreBackoff) {
				return
			}
			continue
		}
		st, err := app.queries.GetKillArchiveRestoreState(ctx)
		if err != nil {
			if ctx.Err() == nil {
				logging.Errorf("killboard archive: read state: %v", err)
			}
			if !killfeedSleepCtx(ctx, killRestoreBackoff) {
				return
			}
			continue
		}
		if st.DoneAt.Valid {
			return
		}
		today := time.Now().UTC().Truncate(24 * time.Hour)
		day := today.AddDate(0, 0, -killArchiveLag)
		if st.Day.Valid {
			day = st.Day.Time.UTC().AddDate(0, 0, -1)
		}
		if day.Before(killArchiveFirstDay) {
			if err := app.queries.MarkKillArchiveRestoreDone(ctx); err != nil {
				logging.Errorf("killboard archive: mark done: %v", err)
				continue
			}
			logging.Infof("killboard archive: done, %d days read, %d kills written back", days, kills)
			return
		}
		n, fetched, err := app.restoreArchiveDay(ctx, client, base, day)
		if err != nil {
			if ctx.Err() == nil {
				logging.Warnf("killboard archive: %s: %v", day.Format("2006-01-02"), err)
			}
			if !killfeedSleepCtx(ctx, killRestoreBackoff) {
				return
			}
			continue
		}
		kills += n
		if fetched {
			days++
			if days%25 == 0 {
				logging.Infof("killboard archive: at %s, %d days read, %d kills written back", day.Format("2006-01-02"), days, kills)
			}
			if !killfeedSleepCtx(ctx, killArchivePause) {
				return
			}
		}
	}
}

// restoreArchiveDay restores one day's kills that have no attacker
// rows and moves the cursor to that day. fetched reports whether the
// day's file was downloaded.
func (app *Application) restoreArchiveDay(ctx context.Context, client *http.Client, base string, day time.Time) (n int, fetched bool, err error) {
	missing, err := app.queries.ListDayKillsMissingAttackers(ctx, db.ListDayKillsMissingAttackersParams{
		DayStart: day, DayEnd: day.AddDate(0, 0, 1),
	})
	if err != nil {
		return 0, false, fmt.Errorf("list kills: %w", err)
	}
	if len(missing) == 0 {
		return 0, false, app.queries.SetKillArchiveRestoreDay(ctx, day)
	}
	want := make(map[int64]time.Time, len(missing))
	for _, m := range missing {
		want[m.KillmailID] = m.KillmailTime
	}
	found, err := readKillArchiveDay(ctx, client, base, day, want)
	if err != nil {
		return 0, true, err
	}
	if len(found) < len(want) {
		logging.Infof("killboard archive: %s: %d of %d kills missing attackers are not in the archive", day.Format("2006-01-02"), len(want)-len(found), len(want))
	}
	return len(found), true, app.writeBackKillAttackers(ctx, found, &day)
}

// readKillArchiveDay downloads one day's archive and returns the
// wanted kills in it. A day the archive does not have is empty.
func readKillArchiveDay(ctx context.Context, client *http.Client, base string, day time.Time, want map[int64]time.Time) ([]restoredKill, error) {
	url := fmt.Sprintf("%s/%d/killmails-%s.tar.bz2", base, day.Year(), day.Format("2006-01-02"))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", killArchiveUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("archive answered %d", resp.StatusCode)
	}
	tr := tar.NewReader(bzip2.NewReader(resp.Body))
	var out []restoredKill
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read archive: %w", err)
		}
		name := path.Base(h.Name)
		if h.Typeflag != tar.TypeReg || !strings.HasSuffix(name, ".json") {
			continue
		}
		id, err := strconv.ParseInt(strings.TrimSuffix(name, ".json"), 10, 64)
		if err != nil {
			continue
		}
		at, ok := want[id]
		if !ok {
			continue
		}
		raw, err := io.ReadAll(io.LimitReader(tr, killArchiveMaxMail))
		if err != nil {
			return nil, fmt.Errorf("read kill %d: %w", id, err)
		}
		var km killmailFull
		if err := goccyjson.Unmarshal(raw, &km); err != nil || km.KillmailID != id {
			logging.Warnf("killboard archive: kill %d: unreadable", id)
			continue
		}
		out = append(out, restoredKill{id: id, at: at, km: km, raw: raw})
	}
}

// writeBackKillAttackers writes read-back kills in one transaction:
// their attacker rows, their ships flown added to the daily
// aggregate, and their killmail while inside retention. A kill past
// retention keeps only the count: its attacker rows go again before
// the commit. day, when set, moves the archive cursor in the same
// transaction.
func (app *Application) writeBackKillAttackers(ctx context.Context, kills []restoredKill, day *time.Time) error {
	tx, err := app.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// Shared, like the fold: a rebuild waits for this to finish,
	// and this does not count while a rebuild runs.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock_shared($1)", killStatsLockKey); err != nil {
		return err
	}
	if app.killStatsRebuilding.Load() {
		return errKillStatsRebuilding
	}
	q := db.New(tx)
	cutoff := time.Now().UTC().Add(-killDetailRetention)
	ids := make([]int64, 0, len(kills))
	var aged []int64
	for _, k := range kills {
		if ap := killAttackersParams(k.km); len(ap.Seqs) > 0 {
			if err := q.InsertKillAttackers(ctx, ap); err != nil {
				return fmt.Errorf("store attackers %d: %w", k.id, err)
			}
		}
		ids = append(ids, k.id)
		if k.at.Before(cutoff) {
			aged = append(aged, k.id)
			continue
		}
		if err := q.InsertKillDetail(ctx, db.InsertKillDetailParams{
			KillmailID: k.id, Payload: compressKillDetail(k.raw), StoredAt: time.Now().UTC(),
		}); err != nil {
			return fmt.Errorf("store detail %d: %w", k.id, err)
		}
	}
	if len(ids) > 0 {
		if err := q.AddKillShipsFlown(ctx, ids); err != nil {
			return fmt.Errorf("count ships: %w", err)
		}
	}
	if len(aged) > 0 {
		if err := q.DeleteKillAttackersFor(ctx, aged); err != nil {
			return fmt.Errorf("drop aged attackers: %w", err)
		}
	}
	if day != nil {
		if err := q.SetKillArchiveRestoreDay(ctx, *day); err != nil {
			return fmt.Errorf("cursor: %w", err)
		}
	}
	return tx.Commit(ctx)
}

// killArchiveRestoreDone reports whether the archive walk has reached
// the first archive day. The ESI restore and the retention prune wait
// for it.
func (app *Application) killArchiveRestoreDone(ctx context.Context) bool {
	if app.cfg.KillArchiveBaseURL() == "" {
		return true
	}
	st, err := app.queries.GetKillArchiveRestoreState(ctx)
	return err == nil && st.DoneAt.Valid
}
