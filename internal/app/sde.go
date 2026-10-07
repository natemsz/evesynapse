package app

import (
	"bufio"
	"compress/bzip2"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
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

// sdeFileNames are the dump tables EveSynapse imports: the base
// tables (including the schema-024 market browse tree), plus the
// industry tables the build planner (schema 011) reads.
// industryActivitySkills.csv is deliberately NOT in this list: it
// only seasons the planner with required skills, so a dump or
// mirror lacking it must not fail the whole import — importSDE
// fetches it best-effort instead.
var sdeFileNames = []string{
	"invTypes.csv",
	"invGroups.csv",
	"invCategories.csv",
	"invMarketGroups.csv",
	"staStations.csv",
	"mapSolarSystems.csv",
	"mapRegions.csv",
	"industryBlueprints.csv",
	"industryActivity.csv",
	"industryActivityProducts.csv",
	"industryActivityMaterials.csv",
	// dgmTypeAttributes.csv carries every dogma attribute row
	// (~1.2M): the skill planner reads a dozen skill-graph
	// attributes from it, and the fitting simulator (schema 029)
	// reads the rest, so the import keeps all rows. Streamed, as
	// before.
	"dgmTypeAttributes.csv",
	// Fitting simulator (schema 029): attribute metadata (names,
	// stackable flags, defaults), the effect set with its
	// modifiers decoded from JSON, and the type -> effect links.
	"dgmAttributeTypes.csv",
	"dgmEffects.csv",
	"dgmTypeEffects.csv",
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

// recoverSDEPanic is the deferred guard on the background SDE
// operations (see the panic-containment note in worker.go): a
// panic there is logged with its stack and recorded as the
// operation's failure, so the Sync page shows it and the process
// keeps serving. The import replaces its tables in one
// transaction, so a run that died leaves the previous data intact.
func (app *Application) recoverSDEPanic(op string) {
	if r := recover(); r != nil {
		logging.Errorf("sde: PANIC in %s (recovered): %v\n%s", op, r, debug.Stack())
		app.updateSDEStatus(func(s *sdeStatus) {
			s.LastError = "internal error during " + op + " — see the server log"
		})
	}
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
		defer app.recoverSDEPanic("import")
		logging.Infof("sde: import starting (%s) from %s", reason, app.cfg.SDEBaseURL())
		if err := app.importSDE(app.workerCtx); err != nil {
			logging.Errorf("sde: import failed: %v", err)
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
		defer app.recoverSDEPanic("update check")
		ctx := app.workerCtx

		changed, err := app.sdeRemoteChanged(ctx)
		if err != nil {
			logging.Errorf("sde: update check failed (%s): %v", reason, err)
			app.updateSDEStatus(func(s *sdeStatus) { s.LastError = err.Error() })
			return
		}
		now := time.Now().UTC().Format(time.RFC3339)
		if err := app.queries.UpsertSDEMeta(ctx, db.UpsertSDEMetaParams{Key: "last_check_at", Value: now}); err != nil {
			logging.Errorf("sde: record check time: %v", err)
		}
		if !changed {
			logging.Infof("sde: static data up to date (checked %s)", reason)
			app.updateSDEStatus(func(s *sdeStatus) {
				s.LastError = ""
				s.Message = "Up to date — remote dump unchanged (checked " + now + ")"
			})
			return
		}
		logging.Infof("sde: remote dump changed; importing (%s)", reason)
		if err := app.importSDE(ctx); err != nil {
			logging.Errorf("sde: import failed: %v", err)
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
		logging.Errorf("sde: maintenance: count types: %v", err)
		return
	}
	if count == 0 {
		logging.Infof("sde: no static data yet — starting initial import")
		app.startSDEImport("initial")
		return
	}
	// One-time backfills: databases imported before schema 008
	// have no market-group/published values, databases stored
	// before schema 011 lack the planner's industry tables,
	// anything before schema 012 lacks the dogma skill graph,
	// anything before schema 024 lacks the market browse tree,
	// and anything before schema 029 lacks the full dogma
	// attribute/effect set, so re-import once (the store writes
	// the marker when it lands). Retries on later ticks while an
	// import keeps failing.
	if ver, _ := app.sdeMeta(ctx, "sde_import_version"); ver != "7" {
		logging.Infof("sde: static data predates current columns — re-importing to backfill")
		app.startSDEImport("schema-029 backfill")
		return
	}
	// Even with a current marker, an empty planner table (say the
	// tables were cleared by hand) refills here rather than
	// waiting for the weekly tick.
	if n, err := app.queries.CountSDEBlueprints(ctx); err == nil && n == 0 {
		logging.Infof("sde: planner tables empty — importing industry data")
		app.startSDEImport("planner backfill")
		return
	}
	if n, err := app.queries.CountSDESkillMeta(ctx); err == nil && n == 0 {
		logging.Infof("sde: skill graph tables empty — importing dogma data")
		app.startSDEImport("skill graph backfill")
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
		logging.Warnf("sde: optional %s unavailable, continuing without it: %v", sdeSkillsFileName, err)
	} else {
		parsed.markers[sdeSkillsFileName] = marker
	}
	parsed.buildIndustryRows()
	parsed.buildSkillRows()

	// A real dump never has an empty table; an empty parse means
	// the file layout changed under us, and storing it would wipe
	// good data for bad. The planner's blueprint table and the
	// skill graph are held to the same rule (the blueprint
	// skills table is not: it is optional).
	if len(parsed.types) == 0 || len(parsed.groups) == 0 || len(parsed.categories) == 0 ||
		len(parsed.marketGroups) == 0 ||
		len(parsed.stations) == 0 || len(parsed.systems) == 0 || len(parsed.regions) == 0 ||
		len(parsed.blueprints) == 0 || len(parsed.skillMeta) == 0 || len(parsed.skillReqs) == 0 ||
		len(parsed.typeAttrs) == 0 || len(parsed.attrTypes) == 0 || len(parsed.effects) == 0 ||
		len(parsed.modifiers) == 0 || len(parsed.typeEffects) == 0 {
		return fmt.Errorf("parsed dump has empty table(s): %d types, %d groups, %d categories, %d market groups, %d stations, %d systems, %d regions, %d blueprints, %d skill meta, %d requirements, %d type attributes, %d attribute types, %d effects, %d modifiers, %d type effects",
			len(parsed.types), len(parsed.groups), len(parsed.categories), len(parsed.marketGroups),
			len(parsed.stations), len(parsed.systems), len(parsed.regions),
			len(parsed.blueprints), len(parsed.skillMeta), len(parsed.skillReqs),
			len(parsed.typeAttrs), len(parsed.attrTypes), len(parsed.effects),
			len(parsed.modifiers), len(parsed.typeEffects))
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
	logging.Infof("sde: %s", msg)
	app.updateSDEStatus(func(s *sdeStatus) { s.Message = msg })
	return nil
}

// parsedSDE holds one full dump in memory, ready for the replace
// transaction, plus each file's remote freshness markers. The
// industry tables parse into intermediate maps (the blueprint row
// joins three files) and are flattened by buildIndustryRows once
// every file is in.
type parsedSDE struct {
	types        []sdeTypeRow
	groups       []sdeGroupRow
	categories   []sdeCategoryRow
	marketGroups []sdeMarketGroupRow
	stations     []sdeStationRow
	systems      []sdeSystemRow
	regions      []sdeRegionRow
	markers      map[string]fileMarker

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

	// Skill graph (schema 012): the dogma attribute rows that
	// matter, keyed type → attribute → value, flattened by
	// buildSkillRows into the meta/requirement tables.
	dogma     map[int64]map[int64]float64
	skillMeta []sdeSkillMetaRow
	skillReqs []sdeRequirementRow

	// Fitting simulator (schema 029): the full dogma attribute
	// rows (kept whole, unlike the skill graph's filtered view),
	// attribute metadata, effects with decoded modifiers, and
	// type -> effect links.
	typeAttrs   []sdeTypeAttributeRow
	attrTypes   []sdeAttributeTypeRow
	effects     []sdeEffectRow
	modifiers   []sdeEffectModifierRow
	typeEffects []sdeTypeEffectRow
}

type sdeTypeAttributeRow struct {
	typeID      int64
	attributeID int64
	value       float64
}

type sdeAttributeTypeRow struct {
	attributeID  int64
	name         string
	stackable    int64
	highIsGood   int64
	unitID       int64
	defaultValue float64
}

type sdeEffectRow struct {
	effectID int64
	name     string
	category int64
}

// sdeEffectModifierRow is one modifier decoded from an effect's
// modifierInfo JSON: (domain, func, modified, modifying,
// operation) plus the group / required-skill selectors (0 when
// the modifier carries none).
type sdeEffectModifierRow struct {
	effectID      int64
	domain        string
	fn            string
	modifiedAttr  int64
	modifyingAttr int64
	operation     int64
	groupID       int64
	skillTypeID   int64
}

type sdeTypeEffectRow struct {
	typeID    int64
	effectID  int64
	isDefault int64
}

type sdeSkillMetaRow struct {
	typeID        int64
	rank          float64
	primaryAttr   int64
	secondaryAttr int64
}

type sdeRequirementRow struct {
	typeID      int64
	skillTypeID int64
	level       int64
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
	description   string // invTypes flavor text; "" when the dump has none
	groupID       int64
	marketGroupID int64 // 0 = cannot be listed on the market
	published     int64 // 1 unless the dump explicitly says 0
	mass          float64
	volume        float64
	capacity      float64
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

// sdeMarketGroupRow is one invMarketGroups row (schema 024): the
// market browse tree. parentGroupID is 0 at the top level; the
// dump spells the parent column parentMarketGroupID (older
// references shorten it to parentGroupID — the parser accepts
// both).
type sdeMarketGroupRow struct {
	marketGroupID int64
	parentGroupID int64 // 0 = top level
	name          string
	iconID        int64 // dump icon reference, kept as data
	hasTypes      int64 // 1 when types list directly in this group
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
	case "invMarketGroups.csv":
		parsed.marketGroups, err = parseSDEMarketGroups(cr, idx)
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
	case "dgmTypeAttributes.csv":
		err = parseSDEDogmaAttributes(cr, idx, parsed)
	case "dgmAttributeTypes.csv":
		parsed.attrTypes, err = parseSDEAttributeTypes(cr, idx)
	case "dgmEffects.csv":
		err = parseSDEEffects(cr, idx, parsed)
	case "dgmTypeEffects.csv":
		err = parseSDETypeEffects(cr, idx, parsed)
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
		// The invTypes description rides along so item pages can
		// render flavor text from the local dump instead of a
		// per-type ESI fetch. Optional: a dump without the column
		// (or an empty cell) just leaves the ESI fallback in charge.
		description, _ := csvField(rec, idx, "description")
		// mass/volume/capacity are the type's physical facts
		// (schema 029: dogma has no ship mass attribute, and
		// drone-bay checks need volumes). Optional columns:
		// a dump without them stores zeros.
		floatCol := func(name string) float64 {
			raw, ferr := csvField(rec, idx, name)
			if ferr != nil {
				return 0
			}
			v, _ := strconv.ParseFloat(strings.TrimSpace(raw), 64)
			return v
		}
		rows = append(rows, sdeTypeRow{
			typeID:        id,
			name:          name,
			description:   description,
			groupID:       csvIDOrZero(rec, idx, "groupID"),
			marketGroupID: csvIDOrZero(rec, idx, "marketGroupID"),
			published:     published,
			mass:          floatCol("mass"),
			volume:        floatCol("volume"),
			capacity:      floatCol("capacity"),
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

// parseSDEMarketGroups reads invMarketGroups.csv. The parent
// column is parentMarketGroupID in the current dump (shortened to
// parentGroupID in some references); top-level groups carry an
// empty or 0 parent, normalized to 0 here. iconID and hasTypes
// ride along when the dump provides them.
func parseSDEMarketGroups(cr *csv.Reader, idx map[string]int) ([]sdeMarketGroupRow, error) {
	parentColumn := "parentMarketGroupID"
	if _, ok := idx[parentColumn]; !ok {
		parentColumn = "parentGroupID"
	}
	var rows []sdeMarketGroupRow
	_, err := eachCSVRow(cr, func(rec []string) error {
		id, err := csvID(rec, idx, "marketGroupID")
		if err != nil {
			return errSkipRow
		}
		name, err := csvField(rec, idx, "marketGroupName")
		if err != nil {
			return err
		}
		hasTypes := int64(0)
		if raw, ferr := csvField(rec, idx, "hasTypes"); ferr == nil {
			if trimmed := strings.TrimSpace(raw); trimmed == "1" || strings.EqualFold(trimmed, "true") {
				hasTypes = 1
			}
		}
		rows = append(rows, sdeMarketGroupRow{
			marketGroupID: id,
			parentGroupID: csvIDOrZero(rec, idx, parentColumn),
			name:          name,
			iconID:        csvIDOrZero(rec, idx, "iconID"),
			hasTypes:      hasTypes,
		})
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

// ---------------------------------------------------------------------------
// Dogma attributes (schema 012, the skill graph). dgmTypeAttributes
// carries ~1.2M rows, but the skill planner only ever reads a
// dozen attribute IDs, verified against the dump's own
// dgmAttributeTypes names and live rows (Gunnery 3300: 180=167
// Perception, 181=168 Willpower, 275=1.0):
//
//   275 skillTimeConstant   — the skill rank multiplier
//   180 primaryAttribute    — value is a character attribute ID
//   181 secondaryAttribute    (164-168: charisma/intelligence/
//                             memory/perception/willpower)
//
// Required skills come in (skill, level) attribute pairs — five
// pairs carry rows in the current dump; the sixth pair exists in
// the attribute table but is unused so far. Every pair found is
// honored, so a ship listing five prerequisites expands fully:
//
//   182/277, 183/278, 184/279, 1285/1286, 1289/1287, 1290/1288
//          (requiredSkillN / requiredSkillNLevel — note pair 5's
//          level attribute has the smaller number)
// ---------------------------------------------------------------------------

// dogmaSkillAttrPairs maps each requiredSkillN attribute to its
// requiredSkillNLevel attribute.
var dogmaSkillAttrPairs = [][2]int64{
	{182, 277}, {183, 278}, {184, 279},
	{1285, 1286}, {1289, 1287}, {1290, 1288},
}

const (
	dogmaAttrRank      = 275
	dogmaAttrPrimary   = 180
	dogmaAttrSecondary = 181
)

// dogmaWantedAttrs is the skill graph's view of the dogma file
// (the fitting simulator keeps every row; see schema 029).
var dogmaWantedAttrs = func() map[int64]bool {
	m := map[int64]bool{dogmaAttrRank: true, dogmaAttrPrimary: true, dogmaAttrSecondary: true}
	for _, pair := range dogmaSkillAttrPairs {
		m[pair[0]] = true
		m[pair[1]] = true
	}
	return m
}()

func parseSDEDogmaAttributes(cr *csv.Reader, idx map[string]int, parsed *parsedSDE) error {
	if parsed.dogma == nil {
		parsed.dogma = make(map[int64]map[int64]float64)
	}
	_, err := eachCSVRow(cr, func(rec []string) error {
		attrID, err := csvID(rec, idx, "attributeID")
		if err != nil {
			return errSkipRow
		}
		typeID, err := csvID(rec, idx, "typeID")
		if err != nil {
			return errSkipRow
		}
		// Values land in valueInt or valueFloat depending on the
		// attribute (skill IDs arrive as floats: 3386.0); anything
		// unparseable is a row we cannot use.
		raw, ferr := csvField(rec, idx, "valueInt")
		if ferr != nil || strings.TrimSpace(raw) == "" {
			if raw, ferr = csvField(rec, idx, "valueFloat"); ferr != nil {
				return errSkipRow
			}
		}
		value, perr := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if perr != nil {
			return errSkipRow
		}
		// The fitting simulator (schema 029) keeps every row; the
		// skill graph keeps only its filtered view of the same pass.
		parsed.typeAttrs = append(parsed.typeAttrs, sdeTypeAttributeRow{
			typeID: typeID, attributeID: attrID, value: value,
		})
		if !dogmaWantedAttrs[attrID] {
			return nil // the other million rows are not the skill graph's business
		}
		attrs := parsed.dogma[typeID]
		if attrs == nil {
			attrs = make(map[int64]float64, 4)
			parsed.dogma[typeID] = attrs
		}
		attrs[attrID] = value
		return nil
	})
	return err
}

// ---------------------------------------------------------------------------
// Fitting simulator files (schema 029): attribute metadata, the
// effect set with decoded modifiers, and type -> effect links.
// ---------------------------------------------------------------------------

// parseSDEAttributeTypes reads dgmAttributeTypes.csv: attribute
// names, the stackable flag (drives stacking-penalty grouping in
// the fitting engine), highIsGood, unit, and the default value an
// attribute carries when a type does not set it.
func parseSDEAttributeTypes(cr *csv.Reader, idx map[string]int) ([]sdeAttributeTypeRow, error) {
	var rows []sdeAttributeTypeRow
	_, err := eachCSVRow(cr, func(rec []string) error {
		id, err := csvID(rec, idx, "attributeID")
		if err != nil {
			return errSkipRow
		}
		name, err := csvField(rec, idx, "attributeName")
		if err != nil {
			return err
		}
		defaultValue := 0.0
		if raw, ferr := csvField(rec, idx, "defaultValue"); ferr == nil {
			defaultValue, _ = strconv.ParseFloat(strings.TrimSpace(raw), 64)
		}
		rows = append(rows, sdeAttributeTypeRow{
			attributeID:  id,
			name:         name,
			stackable:    csvBoolOrOne(rec, idx, "stackable"),
			highIsGood:   csvBoolOrOne(rec, idx, "highIsGood"),
			unitID:       csvIDOrZero(rec, idx, "unitID"),
			defaultValue: defaultValue,
		})
		return nil
	})
	return rows, err
}

// csvBoolOrOne reads a boolean-ish dump column (1/0, true/false,
// empty) into 1 or 0; an absent column defaults to 1 (the dogma
// default for both stackable and highIsGood).
func csvBoolOrOne(rec []string, idx map[string]int, name string) int64 {
	raw, err := csvField(rec, idx, name)
	if err != nil {
		return 1
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "0", "false":
		return 0
	}
	return 1
}

// sdeModifierJSON is one modifier as dgmEffects.csv carries it in
// the modifierInfo JSON column. Fields are pointers where the
// dump legitimately omits them (EffectStopper rows carry no
// operation; most modifiers carry no group/skill selector).
type sdeModifierJSON struct {
	Domain               *string `json:"domain"`
	Func                 *string `json:"func"`
	ModifiedAttributeID  *int64  `json:"modifiedAttributeID"`
	ModifyingAttributeID *int64  `json:"modifyingAttributeID"`
	Operation            *int64  `json:"operation"`
	GroupID              *int64  `json:"groupID"`
	SkillTypeID          *int64  `json:"skillTypeID"`
}

// parseSDEEffects reads dgmEffects.csv. Only rows whose
// modifierInfo is non-empty are kept (the fitting-relevant set:
// ~94% of the dump), with each modifier decoded into a row.
// Rows whose modifier JSON fails to decode abort the import --
// a silently partial effect set would compute wrong fits.
func parseSDEEffects(cr *csv.Reader, idx map[string]int, parsed *parsedSDE) error {
	_, err := eachCSVRow(cr, func(rec []string) error {
		id, err := csvID(rec, idx, "effectID")
		if err != nil {
			return errSkipRow
		}
		info, ferr := csvField(rec, idx, "modifierInfo")
		if ferr != nil {
			return ferr
		}
		if strings.TrimSpace(info) == "" {
			return nil // no modifiers: not a fitting effect
		}
		var mods []sdeModifierJSON
		if jerr := json.Unmarshal([]byte(info), &mods); jerr != nil {
			return fmt.Errorf("effect %d: decode modifierInfo: %w", id, jerr)
		}
		name, ferr := csvField(rec, idx, "effectName")
		if ferr != nil {
			return ferr
		}
		kept := false
		for _, m := range mods {
			// Modifiers without both attributes or an operation
			// (the EffectStopper rows) cannot be applied by the
			// engine and are not stored; the engine reports the
			// shapes it skips.
			if m.ModifiedAttributeID == nil || m.ModifyingAttributeID == nil || m.Operation == nil {
				continue
			}
			row := sdeEffectModifierRow{
				effectID:      id,
				modifiedAttr:  *m.ModifiedAttributeID,
				modifyingAttr: *m.ModifyingAttributeID,
				operation:     *m.Operation,
			}
			if m.Domain != nil {
				row.domain = *m.Domain
			}
			if m.Func != nil {
				row.fn = *m.Func
			}
			if m.GroupID != nil {
				row.groupID = *m.GroupID
			}
			if m.SkillTypeID != nil {
				row.skillTypeID = *m.SkillTypeID
			}
			parsed.modifiers = append(parsed.modifiers, row)
			kept = true
		}
		if kept {
			parsed.effects = append(parsed.effects, sdeEffectRow{
				effectID: id,
				name:     name,
				category: csvIDOrZero(rec, idx, "effectCategory"),
			})
		}
		return nil
	})
	return err
}

// parseSDETypeEffects reads dgmTypeEffects.csv: the type ->
// effect links with isDefault flags.
func parseSDETypeEffects(cr *csv.Reader, idx map[string]int, parsed *parsedSDE) error {
	_, err := eachCSVRow(cr, func(rec []string) error {
		typeID, err := csvID(rec, idx, "typeID")
		if err != nil {
			return errSkipRow
		}
		effectID, err := csvID(rec, idx, "effectID")
		if err != nil {
			return errSkipRow
		}
		parsed.typeEffects = append(parsed.typeEffects, sdeTypeEffectRow{
			typeID:    typeID,
			effectID:  effectID,
			isDefault: csvBoolOrOne(rec, idx, "isDefault"),
		})
		return nil
	})
	return err
}

// buildSkillRows flattens the filtered dogma rows into the
// schema-012 tables. Skill meta covers published category-16
// types (the EVE skill category) that carry a skillTimeConstant —
// dogma's own definition of a trainable skill. Requirements cover
// every type in the dump (modules/ships/charges carry their own
// required skills), with duplicate (type, skill) rows across the
// six pairs collapsing to the highest level.
func (parsed *parsedSDE) buildSkillRows() {
	groupCategory := make(map[int64]int64, len(parsed.groups))
	for _, g := range parsed.groups {
		groupCategory[g.groupID] = g.categoryID
	}

	parsed.skillMeta = parsed.skillMeta[:0]
	for _, t := range parsed.types {
		if t.published != 1 || groupCategory[t.groupID] != 16 {
			continue
		}
		attrs := parsed.dogma[t.typeID]
		rank, ok := attrs[dogmaAttrRank]
		if !ok {
			continue // no training-time constant: not a skill
		}
		if rank <= 0 {
			rank = 1
		}
		parsed.skillMeta = append(parsed.skillMeta, sdeSkillMetaRow{
			typeID:        t.typeID,
			rank:          rank,
			primaryAttr:   int64(attrs[dogmaAttrPrimary]),
			secondaryAttr: int64(attrs[dogmaAttrSecondary]),
		})
	}
	sort.Slice(parsed.skillMeta, func(i, j int) bool {
		return parsed.skillMeta[i].typeID < parsed.skillMeta[j].typeID
	})

	levels := make(map[[2]int64]int64)
	for typeID, attrs := range parsed.dogma {
		for _, pair := range dogmaSkillAttrPairs {
			skillID := int64(attrs[pair[0]])
			if skillID <= 0 {
				continue
			}
			level := int64(attrs[pair[1]])
			if level < 1 {
				level = 1
			}
			if level > 5 {
				level = 5
			}
			key := [2]int64{typeID, skillID}
			if level > levels[key] {
				levels[key] = level
			}
		}
	}
	parsed.skillReqs = parsed.skillReqs[:0]
	for key, level := range levels {
		parsed.skillReqs = append(parsed.skillReqs, sdeRequirementRow{
			typeID: key[0], skillTypeID: key[1], level: level,
		})
	}
	sort.Slice(parsed.skillReqs, func(i, j int) bool {
		if parsed.skillReqs[i].typeID != parsed.skillReqs[j].typeID {
			return parsed.skillReqs[i].typeID < parsed.skillReqs[j].typeID
		}
		return parsed.skillReqs[i].skillTypeID < parsed.skillReqs[j].skillTypeID
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

	for _, table := range []string{"sde_types", "sde_type_physics", "sde_groups", "sde_categories", "sde_market_groups", "sde_stations", "sde_systems", "sde_regions",
		"sde_blueprints", "sde_blueprint_materials", "sde_blueprint_skills",
		"sde_skill_meta", "sde_requirements",
		"sde_type_attributes", "sde_attribute_types", "sde_effects", "sde_effect_modifiers", "sde_type_effects"} {
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

	if err := insert("INSERT INTO sde_types (type_id, name, group_id, market_group_id, published, description) VALUES ($1, $2, $3, $4, $5, $6)", len(parsed.types), func(i int) []any {
		r := parsed.types[i]
		return []any{r.typeID, r.name, r.groupID, r.marketGroupID, r.published, r.description}
	}); err != nil {
		return 0, err
	}
	if err := insert("INSERT INTO sde_type_physics (type_id, mass, volume, capacity) VALUES ($1, $2, $3, $4)", len(parsed.types), func(i int) []any {
		r := parsed.types[i]
		return []any{r.typeID, r.mass, r.volume, r.capacity}
	}); err != nil {
		return 0, err
	}
	if err := insert("INSERT INTO sde_groups (group_id, name, category_id) VALUES ($1, $2, $3)", len(parsed.groups), func(i int) []any {
		r := parsed.groups[i]
		return []any{r.groupID, r.name, r.categoryID}
	}); err != nil {
		return 0, err
	}
	if err := insert("INSERT INTO sde_categories (category_id, name) VALUES ($1, $2)", len(parsed.categories), func(i int) []any {
		r := parsed.categories[i]
		return []any{r.categoryID, r.name}
	}); err != nil {
		return 0, err
	}
	if err := insert("INSERT INTO sde_market_groups (market_group_id, parent_group_id, name, icon_id, has_types) VALUES ($1, $2, $3, $4, $5)", len(parsed.marketGroups), func(i int) []any {
		r := parsed.marketGroups[i]
		return []any{r.marketGroupID, r.parentGroupID, r.name, r.iconID, r.hasTypes}
	}); err != nil {
		return 0, err
	}
	if err := insert("INSERT INTO sde_stations (station_id, name, system_id) VALUES ($1, $2, $3)", len(parsed.stations), func(i int) []any {
		r := parsed.stations[i]
		return []any{r.stationID, r.name, r.systemID}
	}); err != nil {
		return 0, err
	}
	if err := insert("INSERT INTO sde_systems (system_id, name, region_id, security) VALUES ($1, $2, $3, $4)", len(parsed.systems), func(i int) []any {
		r := parsed.systems[i]
		return []any{r.systemID, r.name, r.regionID, r.security}
	}); err != nil {
		return 0, err
	}
	if err := insert("INSERT INTO sde_regions (region_id, name) VALUES ($1, $2)", len(parsed.regions), func(i int) []any {
		r := parsed.regions[i]
		return []any{r.regionID, r.name}
	}); err != nil {
		return 0, err
	}
	if err := insert("INSERT INTO sde_blueprints (blueprint_type_id, product_type_id, product_quantity, max_production_limit, manufacturing_time_seconds) VALUES ($1, $2, $3, $4, $5)", len(parsed.blueprints), func(i int) []any {
		r := parsed.blueprints[i]
		return []any{r.blueprintTypeID, r.productTypeID, r.productQuantity, r.maxProductionLimit, r.manufacturingTimeSeconds}
	}); err != nil {
		return 0, err
	}
	if err := insert("INSERT INTO sde_blueprint_materials (blueprint_type_id, material_type_id, quantity) VALUES ($1, $2, $3)", len(parsed.bpMaterials), func(i int) []any {
		r := parsed.bpMaterials[i]
		return []any{r.blueprintTypeID, r.materialTypeID, r.quantity}
	}); err != nil {
		return 0, err
	}
	if err := insert("INSERT INTO sde_blueprint_skills (blueprint_type_id, skill_type_id, level) VALUES ($1, $2, $3)", len(parsed.bpSkills), func(i int) []any {
		r := parsed.bpSkills[i]
		return []any{r.blueprintTypeID, r.skillTypeID, r.level}
	}); err != nil {
		return 0, err
	}
	if err := insert("INSERT INTO sde_skill_meta (type_id, rank, primary_attr, secondary_attr) VALUES ($1, $2, $3, $4)", len(parsed.skillMeta), func(i int) []any {
		r := parsed.skillMeta[i]
		return []any{r.typeID, r.rank, r.primaryAttr, r.secondaryAttr}
	}); err != nil {
		return 0, err
	}
	if err := insert("INSERT INTO sde_requirements (type_id, skill_type_id, level) VALUES ($1, $2, $3)", len(parsed.skillReqs), func(i int) []any {
		r := parsed.skillReqs[i]
		return []any{r.typeID, r.skillTypeID, r.level}
	}); err != nil {
		return 0, err
	}
	if err := insert("INSERT INTO sde_type_attributes (type_id, attribute_id, value) VALUES ($1, $2, $3)", len(parsed.typeAttrs), func(i int) []any {
		r := parsed.typeAttrs[i]
		return []any{r.typeID, r.attributeID, r.value}
	}); err != nil {
		return 0, err
	}
	if err := insert("INSERT INTO sde_attribute_types (attribute_id, name, stackable, high_is_good, unit_id, default_value) VALUES ($1, $2, $3, $4, $5, $6)", len(parsed.attrTypes), func(i int) []any {
		r := parsed.attrTypes[i]
		return []any{r.attributeID, r.name, r.stackable, r.highIsGood, r.unitID, r.defaultValue}
	}); err != nil {
		return 0, err
	}
	if err := insert("INSERT INTO sde_effects (effect_id, name, category) VALUES ($1, $2, $3)", len(parsed.effects), func(i int) []any {
		r := parsed.effects[i]
		return []any{r.effectID, r.name, r.category}
	}); err != nil {
		return 0, err
	}
	if err := insert("INSERT INTO sde_effect_modifiers (effect_id, domain, func, modified_attr, modifying_attr, operation, group_id, skill_type_id) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)", len(parsed.modifiers), func(i int) []any {
		r := parsed.modifiers[i]
		return []any{r.effectID, r.domain, r.fn, r.modifiedAttr, r.modifyingAttr, r.operation, r.groupID, r.skillTypeID}
	}); err != nil {
		return 0, err
	}
	if err := insert("INSERT INTO sde_type_effects (type_id, effect_id, is_default) VALUES ($1, $2, $3)", len(parsed.typeEffects), func(i int) []any {
		r := parsed.typeEffects[i]
		return []any{r.typeID, r.effectID, r.isDefault}
	}); err != nil {
		return 0, err
	}

	total := int64(len(parsed.types) + len(parsed.types) + len(parsed.groups) + len(parsed.categories) +
		len(parsed.marketGroups) +
		len(parsed.stations) + len(parsed.systems) + len(parsed.regions) +
		len(parsed.blueprints) + len(parsed.bpMaterials) + len(parsed.bpSkills) +
		len(parsed.skillMeta) + len(parsed.skillReqs) +
		len(parsed.typeAttrs) + len(parsed.attrTypes) + len(parsed.effects) +
		len(parsed.modifiers) + len(parsed.typeEffects))
	now := time.Now().UTC().Format(time.RFC3339)

	tq := db.New(tx)
	meta := []db.UpsertSDEMetaParams{
		{Key: "source_base", Value: base},
		{Key: "imported_at", Value: now},
		{Key: "last_check_at", Value: now},
		{Key: "total_rows", Value: strconv.FormatInt(total, 10)},
		// Marker that the schema-008 market columns are populated,
		// the schema-011 planner tables from 3, the schema-012
		// skill graph from 4, the schema-018 bulk item
		// descriptions from 5, the schema-024 market browse
		// tree from 6, and the schema-029 full dogma set from 7
		// (sdeMaintenance backfills once when it's behind).
		{Key: "sde_import_version", Value: "7"},
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
		{"Market groups", app.queries.CountSDEMarketGroups},
		{"Stations", app.queries.CountSDEStations},
		{"Systems", app.queries.CountSDESystems},
		{"Regions", app.queries.CountSDERegions},
		{"Blueprints (planner)", app.queries.CountSDEBlueprints},
		{"Skills (plan graph)", app.queries.CountSDESkillMeta},
		{"Skill requirements", app.queries.CountSDERequirements},
		{"Dogma attributes", app.queries.CountSDETypeAttributes},
		{"Attribute types", app.queries.CountSDEAttributeTypes},
		{"Dogma effects", app.queries.CountSDEEffects},
		{"Effect modifiers", app.queries.CountSDEEffectModifiers},
		{"Type effects", app.queries.CountSDETypeEffects},
	}
	for _, c := range counts {
		n, err := c.fn(ctx)
		if err != nil {
			logging.Errorf("sync: count SDE %s: %v", c.name, err)
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
