package app

import (
	"bufio"
	"compress/bzip2"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// ---------------------------------------------------------------------------
// EVE static data (SDE): a local copy of CCP's static data export,
// imported from Fuzzwork's community CSV conversion and refreshed
// rarely (patch-day cadence: automatic weekly check, manual button
// on the Sync page). Type/group/station/system names resolve from
// the sde_* tables first (see internal/esi); the ESI drip-feed
// caches stay as fallback for anything the SDE lacks.
//
// The dump layout changed at some point: the CSVs now live under
// /dump/latest/csv/ as plain .csv (previously /dump/latest/ as
// .csv.bz2). The downloader copes with both: it tries "<name>.csv"
// then "<name>.csv.bz2", and sniffs bzip2 magic bytes either way.
// EVE_SDE_BASE_URL overrides the base for mirrors.
// ---------------------------------------------------------------------------

// sdeFileNames are the dump tables EveSynapse imports: the six
// base tables, plus the four industry tables the build planner
// (schema 011) reads. industryActivitySkills.csv is deliberately
// NOT in this list: it only seasons the planner with required
// skills, so a dump or mirror lacking it must not fail the whole
// import — importSDE fetches it best-effort instead.
var sdeFileNames = []string{
	"invTypes.csv",
	"invGroups.csv",
	"invCategories.csv",
	"staStations.csv",
	"mapSolarSystems.csv",
	"mapRegions.csv",
	"industryBlueprints.csv",
	"industryActivity.csv",
	"industryActivityProducts.csv",
	"industryActivityMaterials.csv",
}

// sdeSkillsFileName is the optional fifth industry file (see
// sdeFileNames).
const sdeSkillsFileName = "industryActivitySkills.csv"

// sdeHTTPClient downloads the dump files. No total timeout (the
// types table is ~20 MB); each request carries its own context
// deadline instead. The shared loginHTTPClient's 10s timeout is
// far too short for this.
var sdeHTTPClient = &http.Client{}

const sdeUserAgent = "EveSynapse SDE importer (github.com/natemsz/evesynapse)"

// sdeStatus is the importer's user-visible state (Sync page),
// guarded by app.sdeMu exactly like the worker status.
type sdeStatus struct {
	Running    bool
	Phase      string // "checking" | "downloading" | "importing" while Running
	Progress   string // human progress line while Running
	Message    string // last completed outcome ("Imported 51,234 rows", "Up to date…")
	LastError  string // last failure, "" when the last run succeeded
	FinishedAt time.Time
}

func (app *Application) snapshotSDEStatus() sdeStatus {
	app.sdeMu.Lock()
	defer app.sdeMu.Unlock()
	return app.sde
}

func (app *Application) updateSDEStatus(fn func(*sdeStatus)) {
	app.sdeMu.Lock()
	defer app.sdeMu.Unlock()
	fn(&app.sde)
}

// sdeOpBegin claims the single SDE operation slot; concurrent
// attempts collapse into the running one and get false.
func (app *Application) sdeOpBegin(phase string) bool {
	claimed := false
	app.updateSDEStatus(func(s *sdeStatus) {
		if s.Running {
			return
		}
		s.Running = true
		s.Phase = phase
		s.Progress = ""
		claimed = true
	})
	return claimed
}

func (app *Application) sdeOpEnd() {
	app.updateSDEStatus(func(s *sdeStatus) {
		s.Running = false
		s.Phase = ""
		s.Progress = ""
		s.FinishedAt = time.Now()
	})
}

// startSDEImport launches a full import in the background (false
// when one is already running). Used for the first-boot import,
// when the sde_* tables are still empty.
func (app *Application) startSDEImport(reason string) bool {
	if !app.sdeOpBegin("downloading") {
		return false
	}
	go func() {
		defer app.sdeOpEnd()
		log.Printf("sde: import starting (%s) from %s", reason, app.cfg.SDEBaseURL())
		if err := app.importSDE(app.workerCtx); err != nil {
			log.Printf("sde: import failed: %v", err)
			app.updateSDEStatus(func(s *sdeStatus) { s.LastError = err.Error() })
			return
		}
		app.updateSDEStatus(func(s *sdeStatus) { s.LastError = "" })
	}()
	return true
}

// startSDECheck launches the update check in the background: HEAD
// the remote files, compare ETag/Last-Modified against the stored
// markers, and import only when something changed. This is the
// weekly worker check and the Sync page's manual button alike.
func (app *Application) startSDECheck(reason string) bool {
	if !app.sdeOpBegin("checking") {
		return false
	}
	go func() {
		defer app.sdeOpEnd()
		ctx := app.workerCtx

		changed, err := app.sdeRemoteChanged(ctx)
		if err != nil {
			log.Printf("sde: update check failed (%s): %v", reason, err)
			app.updateSDEStatus(func(s *sdeStatus) { s.LastError = err.Error() })
			return
		}
		now := time.Now().UTC().Format(time.RFC3339)
		if err := app.queries.UpsertSDEMeta(ctx, db.UpsertSDEMetaParams{Key: "last_check_at", Value: now}); err != nil {
			log.Printf("sde: record check time: %v", err)
		}
		if !changed {
			log.Printf("sde: static data up to date (checked %s)", reason)
			app.updateSDEStatus(func(s *sdeStatus) {
				s.LastError = ""
				s.Message = "Up to date — remote dump unchanged (checked " + now + ")"
			})
			return
		}
		log.Printf("sde: remote dump changed; importing (%s)", reason)
		if err := app.importSDE(ctx); err != nil {
			log.Printf("sde: import failed: %v", err)
			app.updateSDEStatus(func(s *sdeStatus) { s.LastError = err.Error() })
			return
		}
		app.updateSDEStatus(func(s *sdeStatus) { s.LastError = "" })
	}()
	return true
}

// sdeMaintenance is the worker's SDE hook (boot + hourly): import
// when the tables are empty, otherwise run the weekly check when
// it falls due. It only ever starts background work.
func (app *Application) sdeMaintenance(ctx context.Context) {
	count, err := app.queries.CountSDETypes(ctx)
	if err != nil {
		log.Printf("sde: maintenance: count types: %v", err)
		return
	}
	if count == 0 {
		log.Printf("sde: no static data yet — starting initial import")
		app.startSDEImport("initial")
		return
	}
	// One-time backfills: databases imported before schema 008
	// have no market-group/published values, and anything stored
	// before schema 011 lacks the planner's industry tables, so
	// re-import once (the store writes the marker when it lands).
	// Retries on later ticks while an import keeps failing.
	if ver, _ := app.sdeMeta(ctx, "sde_import_version"); ver != "3" {
		log.Printf("sde: static data predates current columns — re-importing to backfill")
		app.startSDEImport("schema-011 backfill")
		return
	}
	// Even with a current marker, an empty planner table (say the
	// tables were cleared by hand) refills here rather than
	// waiting for the weekly tick.
	if n, err := app.queries.CountSDEBlueprints(ctx); err == nil && n == 0 {
		log.Printf("sde: planner tables empty — importing industry data")
		app.startSDEImport("planner backfill")
		return
	}
	if last, err := app.sdeMeta(ctx, "last_check_at"); err == nil && last != "" {
		if t, perr := time.Parse(time.RFC3339, last); perr == nil && time.Since(t) < 7*24*time.Hour {
			return // checked within the week
		}
	}
	app.startSDECheck("weekly")
}

// sdeMeta reads one sde_meta value ("" when absent).
func (app *Application) sdeMeta(ctx context.Context, key string) (string, error) {
	return app.queries.GetSDEMeta(ctx, key)
}

// ---------------------------------------------------------------------------
// Remote freshness check.
// ---------------------------------------------------------------------------

// fileMarker is one remote file's freshness markers.
type fileMarker struct {
	etag         string
	lastModified string
}

// normalizeETag strips Apache mod_deflate's "-gzip" suffix: the
// ETag of a compressed GET response carries it while the same
// file's HEAD response doesn't, so unnormalized values would never
// compare equal.
func normalizeETag(etag string) string {
	inner := strings.Trim(etag, `"`)
	inner = strings.TrimSuffix(inner, "-gzip")
	if strings.HasPrefix(etag, `"`) {
		return `"` + inner + `"`
	}
	return inner
}

// sdeRemoteChanged reports whether any of the dump files
// differs from the markers recorded at the last import. ETag wins
// when both sides have one; Last-Modified is the fallback. Missing
// stored markers (never imported) count as changed.
func (app *Application) sdeRemoteChanged(ctx context.Context) (bool, error) {
	for _, name := range sdeFileNames {
		remote, err := app.headSDEFile(ctx, name)
		if err != nil {
			return false, fmt.Errorf("check %s: %w", name, err)
		}
		storedETag, _ := app.sdeMeta(ctx, "etag:"+name)
		storedLM, _ := app.sdeMeta(ctx, "last_modified:"+name)
		switch {
		case storedETag == "" && storedLM == "":
			return true, nil // never imported (or markers lost)
		case storedETag != "" && remote.etag != "":
			if normalizeETag(storedETag) != normalizeETag(remote.etag) {
				return true, nil
			}
		case storedLM != remote.lastModified:
			return true, nil
		}
	}
	return false, nil
}

// headSDEFile HEADs one dump file, trying the plain .csv first and
// the .csv.bz2 name second, and returns its freshness markers.
func (app *Application) headSDEFile(ctx context.Context, name string) (fileMarker, error) {
	for _, candidate := range []string{name, name + ".bz2"} {
		hctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		req, err := http.NewRequestWithContext(hctx, http.MethodHead, app.cfg.SDEBaseURL()+candidate, nil)
		if err != nil {
			cancel()
			return fileMarker{}, err
		}
		req.Header.Set("User-Agent", sdeUserAgent)
		resp, err := sdeHTTPClient.Do(req)
		if err != nil {
			cancel()
			return fileMarker{}, err
		}
		resp.Body.Close()
		cancel()
		if resp.StatusCode == http.StatusNotFound {
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return fileMarker{}, fmt.Errorf("HEAD %s: status %d", candidate, resp.StatusCode)
		}
		return fileMarker{etag: resp.Header.Get("ETag"), lastModified: resp.Header.Get("Last-Modified")}, nil
	}
	return fileMarker{}, fmt.Errorf("neither %s nor %s.bz2 found at %s", name, name, app.cfg.SDEBaseURL())
}

// ---------------------------------------------------------------------------
// Import: download + parse all dump tables, then replace the sde_*
// contents in one transaction (deletes + bulk inserts + meta). The
// tables are never touched until every file downloaded and parsed
// cleanly, so a failed import leaves the previous data intact.
// ---------------------------------------------------------------------------

func (app *Application) importSDE(ctx context.Context) error {
	base := app.cfg.SDEBaseURL()

	parsed := &parsedSDE{markers: make(map[string]fileMarker)}
	for i, name := range sdeFileNames {
		app.updateSDEStatus(func(s *sdeStatus) {
			s.Phase = "downloading"
			s.Progress = fmt.Sprintf("Downloading %s (%d of %d)…", name, i+1, len(sdeFileNames))
		})
		marker, err := app.fetchAndParseSDEFile(ctx, base, name, parsed)
		if err != nil {
			return err
		}
		parsed.markers[name] = marker
	}

	// The skills file is best-effort (see sdeFileNames): its
	// absence or a parse hiccup must not fail an otherwise good
	// import — the planner just shows no required skills.
	if marker, err := app.fetchAndParseSDEFile(ctx, base, sdeSkillsFileName, parsed); err != nil {
		log.Printf("sde: optional %s unavailable, continuing without it: %v", sdeSkillsFileName, err)
	} else {
		parsed.markers[sdeSkillsFileName] = marker
	}
	parsed.buildIndustryRows()

	// A real dump never has an empty table; an empty parse means
	// the file layout changed under us, and storing it would wipe
	// good data for bad. The planner's blueprint table is held to
	// the same rule (the skills table is not: it is optional).
	if len(parsed.types) == 0 || len(parsed.groups) == 0 || len(parsed.categories) == 0 ||
		len(parsed.stations) == 0 || len(parsed.systems) == 0 || len(parsed.regions) == 0 ||
		len(parsed.blueprints) == 0 {
		return fmt.Errorf("parsed dump has empty table(s): %d types, %d groups, %d categories, %d stations, %d systems, %d regions, %d blueprints",
			len(parsed.types), len(parsed.groups), len(parsed.categories),
			len(parsed.stations), len(parsed.systems), len(parsed.regions),
			len(parsed.blueprints))
	}

	app.updateSDEStatus(func(s *sdeStatus) {
		s.Phase = "importing"
		s.Progress = fmt.Sprintf("Storing %s types, %s stations, %s systems…",
			esi.FormatInt(int64(len(parsed.types))),
			esi.FormatInt(int64(len(parsed.stations))),
			esi.FormatInt(int64(len(parsed.systems))))
	})
	total, err := app.storeSDE(ctx, base, parsed)
	if err != nil {
		return err
	}

	msg := fmt.Sprintf("Imported %s rows from %s", esi.FormatInt(total), base)
	log.Printf("sde: %s", msg)
	app.updateSDEStatus(func(s *sdeStatus) { s.Message = msg })
	return nil
}

// parsedSDE holds one full dump in memory, ready for the replace
// transaction, plus each file's remote freshness markers. The
// industry tables parse into intermediate maps (the blueprint row
// joins three files) and are flattened by buildIndustryRows once
// every file is in.
type parsedSDE struct {
	types      []sdeTypeRow
	groups     []sdeGroupRow
	categories []sdeCategoryRow
	stations   []sdeStationRow
	systems    []sdeSystemRow
	regions    []sdeRegionRow
	markers    map[string]fileMarker

	// Industry (schema 011): per-blueprint joins over the four
	// industry files, keyed by blueprint type ID, plus the flat
	// material/skill rows and the joined blueprint rows.
	indMaxLimit map[int64]int64
	indTime     map[int64]int64 // activityID 1 seconds per run
	indProduct  map[int64]sdeIndustryProduct
	indMats     map[[2]int64]int64 // (blueprint, material) → base qty per run
	indSkills   map[[2]int64]int64 // (blueprint, skill) → required level
	blueprints  []sdeBlueprintRow
	bpMaterials []sdeBlueprintMaterialRow
	bpSkills    []sdeBlueprintSkillRow
}

type sdeIndustryProduct struct {
	productTypeID int64
	quantity      int64 // units produced per run
}

type sdeBlueprintRow struct {
	blueprintTypeID          int64
	productTypeID            int64
	productQuantity          int64
	maxProductionLimit       int64
	manufacturingTimeSeconds int64
}

type sdeBlueprintMaterialRow struct {
	blueprintTypeID int64
	materialTypeID  int64
	quantity        int64
}

type sdeBlueprintSkillRow struct {
	blueprintTypeID int64
	skillTypeID     int64
	level           int64
}

type sdeTypeRow struct {
	typeID        int64
	name          string
	groupID       int64
	marketGroupID int64 // 0 = cannot be listed on the market
	published     int64 // 1 unless the dump explicitly says 0
}

type sdeGroupRow struct {
	groupID    int64
	name       string
	categoryID int64
}

type sdeCategoryRow struct {
	categoryID int64
	name       string
}

type sdeStationRow struct {
	stationID int64
	name      string
	systemID  int64
}

type sdeSystemRow struct {
	systemID int64
	name     string
	regionID int64
	security float64
}

type sdeRegionRow struct {
	regionID int64
	name     string
}

// fetchAndParseSDEFile downloads one dump file (plain .csv, falling
// back to .csv.bz2; bzip2 content is sniffed by magic bytes either
// way) and streams it through the table's CSV parser into parsed.
func (app *Application) fetchAndParseSDEFile(ctx context.Context, base, name string, parsed *parsedSDE) (fileMarker, error) {
	var lastErr error
	for _, candidate := range []string{name, name + ".bz2"} {
		dctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		req, err := http.NewRequestWithContext(dctx, http.MethodGet, base+candidate, nil)
		if err != nil {
			cancel()
			return fileMarker{}, err
		}
		req.Header.Set("User-Agent", sdeUserAgent)
		resp, err := sdeHTTPClient.Do(req)
		if err != nil {
			cancel()
			return fileMarker{}, fmt.Errorf("download %s: %w", candidate, err)
		}
		if resp.StatusCode == http.StatusNotFound {
			resp.Body.Close()
			cancel()
			lastErr = fmt.Errorf("download %s: status 404", candidate)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			cancel()
			return fileMarker{}, fmt.Errorf("download %s: status %d", candidate, resp.StatusCode)
		}

		marker := fileMarker{etag: resp.Header.Get("ETag"), lastModified: resp.Header.Get("Last-Modified")}
		err = parseSDEFile(name, resp.Body, parsed)
		resp.Body.Close()
		cancel()
		if err != nil {
			return fileMarker{}, err
		}
		return marker, nil
	}
	return fileMarker{}, lastErr
}

// parseSDEFile decompresses (when bzip2) and parses one CSV dump
// into the matching parsed slice. Columns are matched by header
// name, never by position.
func parseSDEFile(name string, body io.Reader, parsed *parsedSDE) error {
	br := bufio.NewReader(body)
	// The dump files open with a UTF-8 BOM ahead of the first
	// (quoted) header name; left in place it keeps the CSV parser
	// from recognizing that first field as quoted, so strip it.
	if magic, err := br.Peek(3); err == nil && len(magic) == 3 &&
		magic[0] == 0xEF && magic[1] == 0xBB && magic[2] == 0xBF {
		_, _ = br.Discard(3)
	}
	if magic, err := br.Peek(3); err == nil && string(magic) == "BZh" {
		body = bzip2.NewReader(br)
	} else {
		body = br
	}

	cr := csv.NewReader(io.LimitReader(body, 512<<20))
	cr.FieldsPerRecord = -1 // tolerate ragged rows; fields are checked individually
	cr.LazyQuotes = true

	header, err := cr.Read()
	if err != nil {
		return fmt.Errorf("parse %s: read header: %w", name, err)
	}
	idx := csvHeaderIndex(header)

	switch name {
	case "invTypes.csv":
		parsed.types, err = parseSDETypes(cr, idx)
	case "invGroups.csv":
		parsed.groups, err = parseSDEGroups(cr, idx)
	case "invCategories.csv":
		parsed.categories, err = parseSDECategories(cr, idx)
	case "staStations.csv":
		parsed.stations, err = parseSDEStations(cr, idx)
	case "mapSolarSystems.csv":
		parsed.systems, err = parseSDESystems(cr, idx)
	case "mapRegions.csv":
		parsed.regions, err = parseSDERegions(cr, idx)
	case "industryBlueprints.csv":
		err = parseSDEIndustryBlueprints(cr, idx, parsed)
	case "industryActivity.csv":
		err = parseSDEIndustryActivity(cr, idx, parsed)
	case "industryActivityProducts.csv":
		err = parseSDEIndustryActivityProducts(cr, idx, parsed)
	case "industryActivityMaterials.csv":
		err = parseSDEIndustryActivityMaterials(cr, idx, parsed)
	case sdeSkillsFileName:
		err = parseSDEIndustryActivitySkills(cr, idx, parsed)
	default:
		err = fmt.Errorf("unknown SDE file %q", name)
	}
	if err != nil {
		return fmt.Errorf("parse %s: %w", name, err)
	}
	return nil
}

// csvHeaderIndex maps header names to positions (the dump's BOM
// is stripped before parsing; the trim here is belt-and-braces).
func csvHeaderIndex(header []string) map[string]int {
	idx := make(map[string]int, len(header))
	for i, h := range header {
		idx[strings.Trim(strings.TrimPrefix(h, "\uFEFF"), " \"")] = i
	}
	return idx
}

// csvField returns the named column of one record.
func csvField(rec []string, idx map[string]int, name string) (string, error) {
	i, ok := idx[name]
	if !ok {
		return "", fmt.Errorf("missing column %q", name)
	}
	if i >= len(rec) {
		return "", fmt.Errorf("short row: no column %q", name)
	}
	return rec[i], nil
}

// csvID parses a required integer column.
func csvID(rec []string, idx map[string]int, name string) (int64, error) {
	raw, err := csvField(rec, idx, name)
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("column %q: %q is not an integer", name, raw)
	}
	return n, nil
}

// csvIDOrZero parses an integer column, defaulting to 0 (secondary
// links like group_id may legitimately be empty/0 in the dump).
func csvIDOrZero(rec []string, idx map[string]int, name string) int64 {
	raw, err := csvField(rec, idx, name)
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	return n
}

// eachCSVRow walks the remaining records of a dump file. Rows whose
// primary key doesn't parse are skipped (counted), other parse
// errors abort the import — a dump we can't read whole is not a
// dump to half-store.
func eachCSVRow(cr *csv.Reader, fn func(rec []string) error) (skipped int, err error) {
	for {
		rec, rerr := cr.Read()
		if errors.Is(rerr, io.EOF) {
			return skipped, nil
		}
		if rerr != nil {
			return skipped, rerr
		}
		if ferr := fn(rec); ferr != nil {
			if errors.Is(ferr, errSkipRow) {
				skipped++
				continue
			}
			return skipped, ferr
		}
	}
}

var errSkipRow = errors.New("skip row")

func parseSDETypes(cr *csv.Reader, idx map[string]int) ([]sdeTypeRow, error) {
	var rows []sdeTypeRow
	_, err := eachCSVRow(cr, func(rec []string) error {
		id, err := csvID(rec, idx, "typeID")
		if err != nil {
			return errSkipRow
		}
		name, err := csvField(rec, idx, "typeName")
		if err != nil {
			return err
		}
		// published is only ever an explicit 0 in the dump; an
		// empty or absent value still means the type is live, so
		// anything but a literal "0" counts as published.
		published := int64(1)
		if raw, ferr := csvField(rec, idx, "published"); ferr == nil && strings.TrimSpace(raw) == "0" {
			published = 0
		}
		rows = append(rows, sdeTypeRow{
			typeID:        id,
			name:          name,
			groupID:       csvIDOrZero(rec, idx, "groupID"),
			marketGroupID: csvIDOrZero(rec, idx, "marketGroupID"),
			published:     published,
		})
		return nil
	})
	return rows, err
}

func parseSDEGroups(cr *csv.Reader, idx map[string]int) ([]sdeGroupRow, error) {
	var rows []sdeGroupRow
	_, err := eachCSVRow(cr, func(rec []string) error {
		id, err := csvID(rec, idx, "groupID")
		if err != nil {
			return errSkipRow
		}
		name, err := csvField(rec, idx, "groupName")
		if err != nil {
			return err
		}
		rows = append(rows, sdeGroupRow{groupID: id, name: name, categoryID: csvIDOrZero(rec, idx, "categoryID")})
		return nil
	})
	return rows, err
}

func parseSDECategories(cr *csv.Reader, idx map[string]int) ([]sdeCategoryRow, error) {
	var rows []sdeCategoryRow
	_, err := eachCSVRow(cr, func(rec []string) error {
		id, err := csvID(rec, idx, "categoryID")
		if err != nil {
			return errSkipRow
		}
		name, err := csvField(rec, idx, "categoryName")
		if err != nil {
			return err
		}
		rows = append(rows, sdeCategoryRow{categoryID: id, name: name})
		return nil
	})
	return rows, err
}

func parseSDEStations(cr *csv.Reader, idx map[string]int) ([]sdeStationRow, error) {
	var rows []sdeStationRow
	_, err := eachCSVRow(cr, func(rec []string) error {
		id, err := csvID(rec, idx, "stationID")
		if err != nil {
			return errSkipRow
		}
		name, err := csvField(rec, idx, "stationName")
		if err != nil {
			return err
		}
		rows = append(rows, sdeStationRow{stationID: id, name: name, systemID: csvIDOrZero(rec, idx, "solarSystemID")})
		return nil
	})
	return rows, err
}

func parseSDESystems(cr *csv.Reader, idx map[string]int) ([]sdeSystemRow, error) {
	var rows []sdeSystemRow
	_, err := eachCSVRow(cr, func(rec []string) error {
		id, err := csvID(rec, idx, "solarSystemID")
		if err != nil {
			return errSkipRow
		}
		name, err := csvField(rec, idx, "solarSystemName")
		if err != nil {
			return err
		}
		security := 0.0
		if raw, ferr := csvField(rec, idx, "security"); ferr == nil {
			security, _ = strconv.ParseFloat(strings.TrimSpace(raw), 64)
		}
		rows = append(rows, sdeSystemRow{
			systemID: id,
			name:     name,
			regionID: csvIDOrZero(rec, idx, "regionID"),
			security: security,
		})
		return nil
	})
	return rows, err
}

func parseSDERegions(cr *csv.Reader, idx map[string]int) ([]sdeRegionRow, error) {
	var rows []sdeRegionRow
	_, err := eachCSVRow(cr, func(rec []string) error {
		id, err := csvID(rec, idx, "regionID")
		if err != nil {
			return errSkipRow
		}
		name, err := csvField(rec, idx, "regionName")
		if err != nil {
			return err
		}
		rows = append(rows, sdeRegionRow{regionID: id, name: name})
		return nil
	})
	return rows, err
}

// ---------------------------------------------------------------------------
// Industry files (schema 011, the build planner). All four keep
// only the manufacturing activity (activityID 1): invention,
// research and reactions live in the same files under other
// activity IDs and are not the planner's business. The blueprint
// row joins industryBlueprints (production limit),
// industryActivity (base time) and industryActivityProducts (what
// one run makes); buildIndustryRows flattens the maps into sorted
// rows once every file is parsed.
// ---------------------------------------------------------------------------

func parseSDEIndustryBlueprints(cr *csv.Reader, idx map[string]int, parsed *parsedSDE) error {
	if parsed.indMaxLimit == nil {
		parsed.indMaxLimit = make(map[int64]int64)
	}
	_, err := eachCSVRow(cr, func(rec []string) error {
		id, err := csvID(rec, idx, "typeID")
		if err != nil {
			return errSkipRow
		}
		parsed.indMaxLimit[id] = csvIDOrZero(rec, idx, "maxProductionLimit")
		return nil
	})
	return err
}

func parseSDEIndustryActivity(cr *csv.Reader, idx map[string]int, parsed *parsedSDE) error {
	if parsed.indTime == nil {
		parsed.indTime = make(map[int64]int64)
	}
	_, err := eachCSVRow(cr, func(rec []string) error {
		if csvIDOrZero(rec, idx, "activityID") != 1 {
			return nil // manufacturing only
		}
		id, err := csvID(rec, idx, "typeID")
		if err != nil {
			return errSkipRow
		}
		parsed.indTime[id] = csvIDOrZero(rec, idx, "time")
		return nil
	})
	return err
}

func parseSDEIndustryActivityProducts(cr *csv.Reader, idx map[string]int, parsed *parsedSDE) error {
	if parsed.indProduct == nil {
		parsed.indProduct = make(map[int64]sdeIndustryProduct)
	}
	_, err := eachCSVRow(cr, func(rec []string) error {
		if csvIDOrZero(rec, idx, "activityID") != 1 {
			return nil // manufacturing only
		}
		id, err := csvID(rec, idx, "typeID")
		if err != nil {
			return errSkipRow
		}
		// The dump carries exactly one manufacturing product per
		// blueprint; first row wins should that ever change.
		if _, seen := parsed.indProduct[id]; seen {
			return nil
		}
		qty := csvIDOrZero(rec, idx, "quantity")
		if qty < 1 {
			qty = 1
		}
		parsed.indProduct[id] = sdeIndustryProduct{
			productTypeID: csvIDOrZero(rec, idx, "productTypeID"),
			quantity:      qty,
		}
		return nil
	})
	return err
}

func parseSDEIndustryActivityMaterials(cr *csv.Reader, idx map[string]int, parsed *parsedSDE) error {
	if parsed.indMats == nil {
		parsed.indMats = make(map[[2]int64]int64)
	}
	_, err := eachCSVRow(cr, func(rec []string) error {
		if csvIDOrZero(rec, idx, "activityID") != 1 {
			return nil // manufacturing only
		}
		bpID, err := csvID(rec, idx, "typeID")
		if err != nil {
			return errSkipRow
		}
		matID := csvIDOrZero(rec, idx, "materialTypeID")
		if matID == 0 {
			return errSkipRow
		}
		// Duplicate (blueprint, material) rows add up rather than
		// colliding on the primary key at store time.
		parsed.indMats[[2]int64{bpID, matID}] += csvIDOrZero(rec, idx, "quantity")
		return nil
	})
	return err
}

func parseSDEIndustryActivitySkills(cr *csv.Reader, idx map[string]int, parsed *parsedSDE) error {
	if parsed.indSkills == nil {
		parsed.indSkills = make(map[[2]int64]int64)
	}
	_, err := eachCSVRow(cr, func(rec []string) error {
		if csvIDOrZero(rec, idx, "activityID") != 1 {
			return nil // manufacturing only
		}
		bpID, err := csvID(rec, idx, "typeID")
		if err != nil {
			return errSkipRow
		}
		skillID := csvIDOrZero(rec, idx, "skillID")
		if skillID == 0 {
			return errSkipRow
		}
		key := [2]int64{bpID, skillID}
		if lvl := csvIDOrZero(rec, idx, "level"); lvl > parsed.indSkills[key] {
			parsed.indSkills[key] = lvl
		}
		return nil
	})
	return err
}

// buildIndustryRows flattens the parsed industry maps into the
// sorted row slices storeSDE inserts. A blueprint becomes a
// planner row only when the dump gives it a manufacturing
// product; the production limit and base time default to 0 when
// their files don't mention the blueprint.
func (parsed *parsedSDE) buildIndustryRows() {
	parsed.blueprints = parsed.blueprints[:0]
	for bpID, prod := range parsed.indProduct {
		if prod.productTypeID == 0 {
			continue
		}
		parsed.blueprints = append(parsed.blueprints, sdeBlueprintRow{
			blueprintTypeID:          bpID,
			productTypeID:            prod.productTypeID,
			productQuantity:          prod.quantity,
			maxProductionLimit:       parsed.indMaxLimit[bpID],
			manufacturingTimeSeconds: parsed.indTime[bpID],
		})
	}
	sort.Slice(parsed.blueprints, func(i, j int) bool {
		return parsed.blueprints[i].blueprintTypeID < parsed.blueprints[j].blueprintTypeID
	})

	parsed.bpMaterials = parsed.bpMaterials[:0]
	for key, qty := range parsed.indMats {
		if qty <= 0 {
			continue
		}
		parsed.bpMaterials = append(parsed.bpMaterials, sdeBlueprintMaterialRow{
			blueprintTypeID: key[0], materialTypeID: key[1], quantity: qty,
		})
	}
	sort.Slice(parsed.bpMaterials, func(i, j int) bool {
		if parsed.bpMaterials[i].blueprintTypeID != parsed.bpMaterials[j].blueprintTypeID {
			return parsed.bpMaterials[i].blueprintTypeID < parsed.bpMaterials[j].blueprintTypeID
		}
		return parsed.bpMaterials[i].materialTypeID < parsed.bpMaterials[j].materialTypeID
	})

	parsed.bpSkills = parsed.bpSkills[:0]
	for key, lvl := range parsed.indSkills {
		if lvl <= 0 {
			continue
		}
		parsed.bpSkills = append(parsed.bpSkills, sdeBlueprintSkillRow{
			blueprintTypeID: key[0], skillTypeID: key[1], level: lvl,
		})
	}
	sort.Slice(parsed.bpSkills, func(i, j int) bool {
		if parsed.bpSkills[i].blueprintTypeID != parsed.bpSkills[j].blueprintTypeID {
			return parsed.bpSkills[i].blueprintTypeID < parsed.bpSkills[j].blueprintTypeID
		}
		return parsed.bpSkills[i].skillTypeID < parsed.bpSkills[j].skillTypeID
	})
}

// storeSDE replaces the sde_* contents with the parsed dump in one
// transaction: deletes, then bulk inserts via prepared statements
// (importer volume — tens of thousands of rows — is why these are
// hand-rolled rather than sqlc; the meta writes do use sqlc against
// the transaction). Returns the total imported row count.
func (app *Application) storeSDE(ctx context.Context, base string, parsed *parsedSDE) (int64, error) {
	tx, err := app.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin SDE replace: %w", err)
	}
	defer tx.Rollback()

	for _, table := range []string{"sde_types", "sde_groups", "sde_categories", "sde_stations", "sde_systems", "sde_regions",
		"sde_blueprints", "sde_blueprint_materials", "sde_blueprint_skills"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table); err != nil {
			return 0, fmt.Errorf("clear %s: %w", table, err)
		}
	}

	insert := func(query string, n int, fill func(i int) []any) error {
		stmt, err := tx.PrepareContext(ctx, query)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for i := 0; i < n; i++ {
			if _, err := stmt.ExecContext(ctx, fill(i)...); err != nil {
				return fmt.Errorf("insert row %d of %s: %w", i, query, err)
			}
		}
		return nil
	}

	if err := insert("INSERT INTO sde_types (type_id, name, group_id, market_group_id, published) VALUES (?, ?, ?, ?, ?)", len(parsed.types), func(i int) []any {
		r := parsed.types[i]
		return []any{r.typeID, r.name, r.groupID, r.marketGroupID, r.published}
	}); err != nil {
		return 0, err
	}
	if err := insert("INSERT INTO sde_groups (group_id, name, category_id) VALUES (?, ?, ?)", len(parsed.groups), func(i int) []any {
		r := parsed.groups[i]
		return []any{r.groupID, r.name, r.categoryID}
	}); err != nil {
		return 0, err
	}
	if err := insert("INSERT INTO sde_categories (category_id, name) VALUES (?, ?)", len(parsed.categories), func(i int) []any {
		r := parsed.categories[i]
		return []any{r.categoryID, r.name}
	}); err != nil {
		return 0, err
	}
	if err := insert("INSERT INTO sde_stations (station_id, name, system_id) VALUES (?, ?, ?)", len(parsed.stations), func(i int) []any {
		r := parsed.stations[i]
		return []any{r.stationID, r.name, r.systemID}
	}); err != nil {
		return 0, err
	}
	if err := insert("INSERT INTO sde_systems (system_id, name, region_id, security) VALUES (?, ?, ?, ?)", len(parsed.systems), func(i int) []any {
		r := parsed.systems[i]
		return []any{r.systemID, r.name, r.regionID, r.security}
	}); err != nil {
		return 0, err
	}
	if err := insert("INSERT INTO sde_regions (region_id, name) VALUES (?, ?)", len(parsed.regions), func(i int) []any {
		r := parsed.regions[i]
		return []any{r.regionID, r.name}
	}); err != nil {
		return 0, err
	}
	if err := insert("INSERT INTO sde_blueprints (blueprint_type_id, product_type_id, product_quantity, max_production_limit, manufacturing_time_seconds) VALUES (?, ?, ?, ?, ?)", len(parsed.blueprints), func(i int) []any {
		r := parsed.blueprints[i]
		return []any{r.blueprintTypeID, r.productTypeID, r.productQuantity, r.maxProductionLimit, r.manufacturingTimeSeconds}
	}); err != nil {
		return 0, err
	}
	if err := insert("INSERT INTO sde_blueprint_materials (blueprint_type_id, material_type_id, quantity) VALUES (?, ?, ?)", len(parsed.bpMaterials), func(i int) []any {
		r := parsed.bpMaterials[i]
		return []any{r.blueprintTypeID, r.materialTypeID, r.quantity}
	}); err != nil {
		return 0, err
	}
	if err := insert("INSERT INTO sde_blueprint_skills (blueprint_type_id, skill_type_id, level) VALUES (?, ?, ?)", len(parsed.bpSkills), func(i int) []any {
		r := parsed.bpSkills[i]
		return []any{r.blueprintTypeID, r.skillTypeID, r.level}
	}); err != nil {
		return 0, err
	}

	total := int64(len(parsed.types) + len(parsed.groups) + len(parsed.categories) +
		len(parsed.stations) + len(parsed.systems) + len(parsed.regions) +
		len(parsed.blueprints) + len(parsed.bpMaterials) + len(parsed.bpSkills))
	now := time.Now().UTC().Format(time.RFC3339)

	tq := db.New(tx)
	meta := []db.UpsertSDEMetaParams{
		{Key: "source_base", Value: base},
		{Key: "imported_at", Value: now},
		{Key: "last_check_at", Value: now},
		{Key: "total_rows", Value: strconv.FormatInt(total, 10)},
		// Marker that the schema-008 market columns are populated
		// and, from 3, the schema-011 planner tables too
		// (sdeMaintenance backfills once when it's behind).
		{Key: "sde_import_version", Value: "3"},
	}
	for name, m := range parsed.markers {
		meta = append(meta,
			db.UpsertSDEMetaParams{Key: "etag:" + name, Value: m.etag},
			db.UpsertSDEMetaParams{Key: "last_modified:" + name, Value: m.lastModified},
		)
	}
	for _, m := range meta {
		if err := tq.UpsertSDEMeta(ctx, m); err != nil {
			return 0, fmt.Errorf("write SDE meta %s: %w", m.Key, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit SDE replace: %w", err)
	}
	return total, nil
}

// ---------------------------------------------------------------------------
// Sync page view.
// ---------------------------------------------------------------------------

// sdeTableCount is one row of the Sync page's SDE table summary.
type sdeTableCount struct {
	Name string
	Rows string // formatted count
}

// sdeFileMarker is one dump file's stored remote Last-Modified.
type sdeFileMarker struct {
	File         string
	LastModified string // "—" when unknown
}

// sdeView is the Sync page's static-data block.
type sdeView struct {
	Imported   bool
	ImportedAt string // RFC3339, "" when never
	SourceBase string
	Running    bool
	State      string // headline: "Importing…", "Imported", "Not imported yet", …
	Progress   string
	Message    string
	LastError  string
	Tables     []sdeTableCount
	Files      []sdeFileMarker
}

// loadSDEView builds the Sync page's SDE block from the tables, the
// import meta, and the live importer status.
func (app *Application) loadSDEView(ctx context.Context) *sdeView {
	status := app.snapshotSDEStatus()
	view := &sdeView{
		SourceBase: app.cfg.SDEBaseURL(),
		Running:    status.Running,
		Progress:   status.Progress,
		Message:    status.Message,
		LastError:  status.LastError,
	}

	counts := []struct {
		name string
		fn   func(context.Context) (int64, error)
	}{
		{"Types", app.queries.CountSDETypes},
		{"Groups", app.queries.CountSDEGroups},
		{"Categories", app.queries.CountSDECategories},
		{"Stations", app.queries.CountSDEStations},
		{"Systems", app.queries.CountSDESystems},
		{"Regions", app.queries.CountSDERegions},
		{"Blueprints (planner)", app.queries.CountSDEBlueprints},
	}
	for _, c := range counts {
		n, err := c.fn(ctx)
		if err != nil {
			log.Printf("sync: count SDE %s: %v", c.name, err)
			n = 0
		}
		view.Tables = append(view.Tables, sdeTableCount{Name: c.name, Rows: esi.FormatInt(n)})
		if c.name == "Types" {
			view.Imported = n > 0
		}
	}

	if importedAt, err := app.sdeMeta(ctx, "imported_at"); err == nil {
		view.ImportedAt = importedAt
	}
	if source, err := app.sdeMeta(ctx, "source_base"); err == nil && source != "" {
		view.SourceBase = source
	}
	for _, name := range sdeFileNames {
		lm, _ := app.sdeMeta(ctx, "last_modified:"+name)
		if lm == "" {
			lm = "—"
		}
		view.Files = append(view.Files, sdeFileMarker{File: name, LastModified: lm})
	}

	switch {
	case status.Running && status.Phase == "checking":
		view.State = "Checking for updates…"
	case status.Running:
		view.State = "Importing…"
	case view.Imported:
		view.State = "Imported"
	default:
		view.State = "Not imported yet"
	}
	return view
}
