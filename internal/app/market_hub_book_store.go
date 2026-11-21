package app

// Hub books across a restart. A clean shutdown writes the held books
// to the service's cache directory and the next start reads them
// back, so its first sweep writes only what changed and the market
// surfaces keep reading memory. A book is restored only while the
// region's stored fetch stamp is still the one it was saved with:
// any later write to the region means the file no longer describes
// the stored rows. The file is removed once read, so it is never
// used twice.

import (
	"bufio"
	"context"
	"encoding/gob"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"evesynapse/internal/logging"
)

const (
	hubBookFileName   = "hub-books.gob"
	hubBookFileFormat = 1
)

type hubBookFile struct {
	Format  int
	Regions map[int64]hubBookFileRegion
}

type hubBookFileRegion struct {
	AttemptedAt  time.Time // the region's fetch stamp when saved
	Orders       int
	SweptAt      time.Time
	LastModified time.Time
	Expires      time.Time
	Day          string
	AvgVolume    map[int64]float64
	Types        map[int64]hubBookFileType
}

type hubBookFileType struct {
	Sells, Buys []bookOrder
}

// saveHubBooks writes every held book whose region last stored
// cleanly. The worker has stopped by the time it runs.
func (app *Application) saveHubBooks(ctx context.Context) {
	dir := app.cfg.hubBookDir
	if dir == "" || app.cfg.lowRAMMode {
		return
	}
	app.hubBooksMu.RLock()
	books := make(map[int64]*hubBook, len(app.hubBooks))
	for regionID, b := range app.hubBooks {
		books[regionID] = b
	}
	app.hubBooksMu.RUnlock()
	if len(books) == 0 {
		return
	}
	file := hubBookFile{Format: hubBookFileFormat, Regions: make(map[int64]hubBookFileRegion, len(books))}
	for regionID, b := range books {
		state, err := app.queries.GetMarketFetchState(ctx, regionSweepKind(regionID))
		if err != nil || state.State != fetchStateOK {
			continue
		}
		types := make(map[int64]hubBookFileType, len(b.types))
		for typeID, tb := range b.types {
			types[typeID] = hubBookFileType{Sells: tb.sells, Buys: tb.buys}
		}
		file.Regions[regionID] = hubBookFileRegion{
			AttemptedAt: state.AttemptedAt, Orders: b.orders, SweptAt: b.sweptAt,
			LastModified: b.lastModified, Expires: b.expires, Day: b.day,
			AvgVolume: b.avgVolume, Types: types,
		}
	}
	if err := writeHubBookFile(dir, file); err != nil {
		logging.Errorf("market: save hub books: %v", err)
		return
	}
	logging.Infof("market: saved %d hub books for the next start", len(file.Regions))
}

func writeHubBookFile(dir string, file hubBookFile) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, hubBookFileName+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	w := bufio.NewWriterSize(tmp, 1<<20)
	if err := gob.NewEncoder(w).Encode(file); err != nil {
		tmp.Close()
		return err
	}
	if err := w.Flush(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, hubBookFileName))
}

// restoreHubBooks reads the saved books back before the first sweep.
// A missing, unreadable or outdated file only means the first sweep
// writes the region whole.
func (app *Application) restoreHubBooks(ctx context.Context) {
	dir := app.cfg.hubBookDir
	if dir == "" || app.cfg.lowRAMMode {
		return
	}
	path := filepath.Join(dir, hubBookFileName)
	f, err := os.Open(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			logging.Warnf("market: restore hub books: %v", err)
		}
		return
	}
	defer os.Remove(path)
	defer f.Close()
	var file hubBookFile
	if err := gob.NewDecoder(bufio.NewReaderSize(f, 1<<20)).Decode(&file); err != nil {
		logging.Warnf("market: restore hub books: %v", err)
		return
	}
	if file.Format != hubBookFileFormat {
		return
	}
	restored := 0
	for regionID, saved := range file.Regions {
		state, err := app.queries.GetMarketFetchState(ctx, regionSweepKind(regionID))
		if err != nil || state.State != fetchStateOK || !state.AttemptedAt.Equal(saved.AttemptedAt) {
			continue
		}
		b := &hubBook{
			types: make(map[int64]*typeBook, len(saved.Types)), orders: saved.Orders,
			sweptAt: saved.SweptAt, lastModified: saved.LastModified, expires: saved.Expires,
			day: saved.Day, avgVolume: saved.AvgVolume,
		}
		for typeID, t := range saved.Types {
			b.types[typeID] = &typeBook{sells: t.Sells, buys: t.Buys}
		}
		app.holdHubBook(regionID, b)
		app.hubBooksMu.Lock()
		if app.hubWindows == nil {
			app.hubWindows = make(map[int64]esiWindow)
		}
		app.hubWindows[regionID] = esiWindow{lastModified: saved.LastModified, expires: saved.Expires}
		app.hubBooksMu.Unlock()
		restored++
	}
	logging.Infof("market: restored %d of %d saved hub books", restored, len(file.Regions))
}
