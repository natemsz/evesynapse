package app

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/fit"
	"evesynapse/internal/logging"
	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------------------
// EVE static data (SDE): a local copy of CCP's static data export,
// imported from CCP's SDE API (sde_api.go, JSON Lines tables parsed
// in sde_jsonl.go) and checked for a new release after each downtime,
// see sdeCheckDue (manual button on the Sync page). Type/group/station/system names
// resolve from the sde_* tables first (see internal/esi); the ESI
// drip-feed caches stay as fallback for anything the SDE lacks. A
// JSON Lines dump uploaded on the Sync page (sde_override.go) can
// stand in for the API's tables.
// ---------------------------------------------------------------------------

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
			s.LastError = "internal error during " + op + "; see the server log"
		})
	}
}

// startSDEImport launches a full import in the background (false
// when one is already running). Used for the first-boot import,
// when the sde_* tables are still empty, and after an upload.
func (app *Application) startSDEImport(reason string) bool {
	if !app.sdeOpBegin("downloading") {
		return false
	}
	go func() {
		defer app.sdeOpEnd()
		defer app.recoverSDEPanic("import")
		logging.Infof("sde: import starting (%s) from %s", reason, app.cfg.SDEAPIURL())
		if err := app.importSDE(app.workerCtx); err != nil {
			logging.Errorf("sde: import failed: %v", err)
			app.updateSDEStatus(func(s *sdeStatus) { s.LastError = err.Error() })
			return
		}
		app.updateSDEStatus(func(s *sdeStatus) { s.LastError = "" })
	}()
	return true
}

// startSDECheck launches the update check in the background: read
// the SDE API's current release, compare each table's version with
// the one stored at the last import, and import only when one of
// them moved. This is the worker's check and the Sync page's manual
// button alike.
func (app *Application) startSDECheck(reason string) bool {
	if !app.sdeOpBegin("checking") {
		return false
	}
	go func() {
		defer app.sdeOpEnd()
		defer app.recoverSDEPanic("update check")
		ctx := app.workerCtx

		rel, err := app.fetchSDERelease(ctx)
		var changed []string
		if err == nil {
			changed, err = app.sdeAPIChangedTables(ctx, rel)
		}
		if err != nil {
			logging.Errorf("sde: update check failed (%s): %v", reason, err)
			app.updateSDEStatus(func(s *sdeStatus) { s.LastError = err.Error() })
			return
		}
		now := time.Now().UTC().Format(time.RFC3339)
		if err := app.queries.UpsertSDEMeta(ctx, db.UpsertSDEMetaParams{Key: "last_check_at", Value: now}); err != nil {
			logging.Errorf("sde: record check time: %v", err)
		}
		if len(changed) == 0 {
			logging.Infof("sde: static data up to date with %s (checked %s)", rel.label(), reason)
			app.updateSDEStatus(func(s *sdeStatus) {
				s.LastError = ""
				s.Message = "Up to date with " + rel.label() + " (checked " + now + ")"
			})
			return
		}
		logging.Infof("sde: %s changes %s; importing (%s)", rel.label(), strings.Join(changed, ", "), reason)
		// The API moved past an upload: import the release and
		// retire the upload.
		ov := app.loadSDEOverride(ctx)
		if err := app.importSDEFrom(ctx, nil); err != nil {
			logging.Errorf("sde: import failed: %v", err)
			app.updateSDEStatus(func(s *sdeStatus) { s.LastError = err.Error() })
			return
		}
		if ov != nil {
			app.dropSDEOverride(ctx, ov.sha)
			logging.Infof("sde: SDE API changed after upload %q; upload retired", ov.label)
		}
		app.updateSDEStatus(func(s *sdeStatus) { s.LastError = "" })
	}()
	return true
}

// sdeMaintenance is the worker's periodic SDE hook. It decides
// between "import now" (empty tables, or a previous import's column
// set predates the current one) and an update check when sdeCheckDue
// says one is due.
func (app *Application) sdeMaintenance(ctx context.Context) {
	count, err := app.queries.CountSDETypes(ctx)
	if err != nil {
		logging.Errorf("sde: maintenance: count types: %v", err)
		return
	}
	if count == 0 {
		logging.Infof("sde: no static data yet, starting initial import")
		app.startSDEImport("initial")
		return
	}
	// The universe map rebuilds when its builder moved on (a no-op
	// until the SDE carries geometry).
	app.ensureUniverseMap(ctx)
	// A stored import whose column set predates sdeImportVersion
	// re-imports once (the store writes the marker when it lands);
	// retries on later ticks while an import keeps failing.
	if ver, _ := app.sdeMeta(ctx, "sde_import_version"); ver != sdeImportVersion {
		logging.Infof("sde: static data predates current columns, re-importing to backfill")
		app.startSDEImport("column backfill")
		return
	}
	// Even with a current marker, an empty planner table (say the
	// tables were cleared by hand) refills here rather than
	// waiting for the next check.
	if n, err := app.queries.CountSDEBlueprints(ctx); err == nil && n == 0 {
		logging.Infof("sde: planner tables empty, importing industry data")
		app.startSDEImport("planner backfill")
		return
	}
	if n, err := app.queries.CountSDESkillMeta(ctx); err == nil && n == 0 {
		logging.Infof("sde: skill graph tables empty, importing dogma data")
		app.startSDEImport("skill graph backfill")
		return
	}
	if !sdeCheckDue(time.Now(), app.sdeMetaTime(ctx, "last_check_at"), app.sdeMetaTime(ctx, "imported_at")) {
		return
	}
	app.startSDECheck("scheduled")
}

// sdeImportVersion is the importer's column-set marker (see
// sdeMaintenance): an import stored under an older one re-imports.
const sdeImportVersion = "22"

// sdeMeta reads one sde_meta value ("" when absent).
func (app *Application) sdeMeta(ctx context.Context, key string) (string, error) {
	return app.queries.GetSDEMeta(ctx, key)
}

// sdeMetaTime reads one sde_meta timestamp, zero when absent or
// unreadable.
func (app *Application) sdeMetaTime(ctx context.Context, key string) time.Time {
	v, err := app.sdeMeta(ctx, key)
	if err != nil || v == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}
	}
	return t
}

// ---------------------------------------------------------------------------
// Import: read every table, then replace the sde_* contents in one
// transaction (deletes + bulk inserts + meta). The tables are never
// touched until every required table read and parsed cleanly, so a
// failed import leaves the previous data intact.
// ---------------------------------------------------------------------------

func (app *Application) importSDE(ctx context.Context) error {
	ov := app.loadSDEOverride(ctx)
	err := app.importSDEFrom(ctx, ov)
	if err != nil && ov != nil {
		// A broken upload must not block every later import: drop
		// it, so the next one reads the SDE API.
		app.dropSDEOverride(ctx, ov.sha)
		return fmt.Errorf("uploaded dump %q dropped: %w", ov.label, err)
	}
	return err
}

// importSDEFrom imports the static data: from the SDE API's current
// release (ov nil), or from a JSON Lines upload with the tables it
// lacks from the API.
func (app *Application) importSDEFrom(ctx context.Context, ov *sdeArchive) error {
	parsed := &parsedSDE{versions: make(map[string]string)}
	// The current release, when the API answers. An upload records
	// its table versions too, so the check imports again only once
	// the API moves past them.
	rel, relErr := app.fetchSDERelease(ctx)
	if rel != nil {
		for _, t := range sdeAPITables {
			if v := rel.Tables[t.name].Version; v != "" {
				parsed.versions[t.name] = v
			}
		}
		parsed.release = rel
	}

	if rel == nil && ov == nil {
		return relErr
	}
	var source string
	switch {
	case ov == nil:
		source = app.cfg.SDEAPIURL() + " " + rel.label()
		parsed.build = rel.Release
	case rel == nil:
		source = ov.source()
		parsed.build = "upload " + ov.sha[:12]
	default:
		source = ov.source() + ", other tables from " + rel.label()
		parsed.build = "upload " + ov.sha[:12]
	}
	if err := app.parseSDEAPITables(ctx, rel, relErr, ov, parsed); err != nil {
		return err
	}
	parsed.finishJSONL()
	parsed.buildIndustryRows()
	parsed.buildInventionRows()
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
	total, err := app.storeSDE(ctx, source, parsed)
	if err != nil {
		return err
	}
	// The search index's SDE part is stale from this moment;
	// every import trigger (the manual sync and the background
	// synchronizer) lands here, so this one hook covers them.
	app.invalidateSearchIndexSDE()
	app.invalidateSDETopology()
	fit.ResetSkillDogma()
	fit.ResetBurstData()
	// The universe map is built from what just landed.
	if err := app.rebuildUniverseMap(ctx); err != nil {
		logging.Errorf("map: rebuild after import: %v", err)
	}

	msg := fmt.Sprintf("Imported %s rows from %s", esi.FormatInt(total), source)
	logging.Infof("sde: %s", msg)
	app.updateSDEStatus(func(s *sdeStatus) { s.Message = msg })
	return nil
}

// parseSDEAPITables reads every SDE API table: from the upload when
// it holds the table, else the kept copy or a download from rel. A
// required table that cannot be read fails the import; an optional
// one is skipped and keeps the rows stored before.
func (app *Application) parseSDEAPITables(ctx context.Context, rel *sdeRelease, relErr error, ov *sdeArchive, parsed *parsedSDE) error {
	var downloaded []string
	for i, t := range sdeAPITables {
		app.updateSDEStatus(func(s *sdeStatus) {
			s.Phase = "downloading"
			s.Progress = fmt.Sprintf("Reading %s (%d of %d)…", t.name, i+1, len(sdeAPITables))
		})
		if ov != nil {
			found, err := ov.parse(t.name, parsed)
			if found {
				if err != nil {
					return err
				}
				continue
			}
		}
		var err error
		if rel == nil {
			err = fmt.Errorf("%s is not in the upload and the SDE API is unreachable: %w", t.name, relErr)
		} else {
			var body []byte
			var fresh bool
			body, fresh, err = app.sdeAPITableBody(ctx, rel, t.name)
			if err != nil {
				// A table that failed to download is tried again at
				// the next check.
				delete(parsed.versions, t.name)
			} else if err = parseSDEJSONLTable(t.name, bytes.NewReader(body), parsed); err == nil && fresh {
				app.keepSDETable(ctx, t.name, rel.Tables[t.name].Version, body)
				downloaded = append(downloaded, t.name)
			}
		}
		if err != nil {
			if t.required {
				return err
			}
			logging.Warnf("sde: optional table %s unavailable, keeping the rows it feeds: %v", t.name, err)
			parsed.dropJSONLTable(t.name)
		}
	}
	if len(downloaded) > 0 {
		logging.Infof("sde: downloaded %d changed tables (%s); the rest read from the kept copies", len(downloaded), strings.Join(downloaded, ", "))
	}
	return nil
}

// parsedSDE holds one full dump in memory, ready for the replace
// transaction, plus where it came from. The
// industry tables parse into intermediate maps (the blueprint row
// joins three files) and are flattened by buildIndustryRows once
// every file is in.
type parsedSDE struct {
	types        []sdeTypeRow
	groups       []sdeGroupRow
	categories   []sdeCategoryRow
	marketGroups []sdeMarketGroupRow
	stations     []sdeStationRow
	npcCorps     []sdeNPCCorpRow
	systems      []sdeSystemRow
	regions      []sdeRegionRow

	// build names the dump in the rows that record it (the SDE API
	// release, or the upload). versions holds the SDE API version of
	// every table the import read or skipped as unchanged, stored so
	// the next check can tell what moved; release is the SDE API
	// release, nil when the API did not answer.
	build    string
	versions map[string]string
	release  *sdeRelease

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

	// Invention (schema 117): activityID 8 in the same files, the T2
	// blueprint copy an invention makes (runs per copy) and the
	// datacores it takes per attempt, for profit per blueprint.
	invProduct   map[[2]int64]int64 // (T1 blueprint, T2 blueprint) -> runs per copy
	invMats      map[[2]int64]int64 // (T1 blueprint, material) -> qty per attempt
	invention    []sdeInventionRow
	invMaterials []sdeBlueprintMaterialRow

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

	// Abyssal roll ranges (schema 055): one row per
	// (mutaplasmid, attribute) the dynamic-item dataset
	// carries. Empty when the dump predates the dataset.
	dynRanges []sdeDynRangeRow

	// Mutaplasmid applicability mappings (schema 063): one
	// row per (mutaplasmid, base type) the mappings dataset
	// carries. Empty when the dump predates the dataset.
	dynMappings []sdeDynMappingRow

	// Reprocessing outputs (schema 066): one row per (type,
	// output material) from invTypeMaterials.
	reprocessing []sdeReprocessRow

	// The universe map (schema 072): constellations and the
	// stargate graph. Empty when the dump lacks the file.
	constellations []sdeConstellationRow
	jumps          []sdeJumpRow

	// Celestials (schema 090): the named in-system places a
	// kill's location can point at. Empty when the dump lacks
	// the file.
	celestials []sdeCelestialRow

	// The item sheet (schema 140): units, traits, variation links
	// and tier names. Each is empty when the dump lacks its file.
	units      []sdeUnitRow
	traits     []sdeTraitRow
	metaTypes  []sdeMetaTypeRow
	metaGroups []sdeMetaGroupRow

	// The buff tables (schema 145): empty when the dump lacks them.
	dbuffs    []sdeDbuffRow
	dbuffMods []sdeDbuffModRow

	// Cosmetic character titles (schema 161): empty when the dump lacks characterTitles.
	titles []sdeCharacterTitleRow

	// Factions and PI schematics (schema 179): empty when the dump
	// lacks them, which keeps the stored rows.
	factions   []sdeFactionRow
	schematics []sdeSchematicRow

	// jl holds a JSON Lines dump's cross-table rows until finishJSONL.
	jl *sdeJSONLState
}

// sdeFactionRow is one faction and its executor and militia
// corporations.
type sdeFactionRow struct {
	factionID, corporationID, militiaCorporationID int64
	name                                           string
}

// sdeSchematicRow is one planetary industry schematic: its name and
// the seconds one cycle takes.
type sdeSchematicRow struct {
	schematicID, cycleTime int64
	name                   string
}

// sdeCharacterTitleRow maps a cosmetic title UUID to its display name.
type sdeCharacterTitleRow struct {
	titleID string
	name    string
}

// sdeDbuffRow is one buff collection: its name, how several values
// of it combine, and its operation.
type sdeDbuffRow struct {
	dbuffID     int64
	displayName string
	aggregate   string
	operation   string
}

// sdeDbuffModRow is one attribute a buff changes: kind "item" (the
// ship), "location" (everything fitted), "group" (fitted modules of
// groupID) or "skill" (fitted modules requiring skillID).
type sdeDbuffModRow struct {
	dbuffID     int64
	kind        string
	attributeID int64
	groupID     int64
	skillID     int64
}

// sdeUnitRow is one attribute unit and the label the game shows.
type sdeUnitRow struct {
	unitID      int64
	displayName string
}

// sdeTraitRow is one ship or subsystem bonus line. skillID -1 is a
// role bonus; hasBonus is false for a line with no figure.
type sdeTraitRow struct {
	traitID    int64
	typeID     int64
	skillID    int64
	bonus      float64
	hasBonus   bool
	bonusText  string
	unitID     int64
	importance int64
}

// sdeMetaTypeRow links a variation to its base type (0 for a base).
type sdeMetaTypeRow struct {
	typeID       int64
	parentTypeID int64
	metaGroupID  int64
}

// sdeMetaGroupRow names one variation tier.
type sdeMetaGroupRow struct {
	metaGroupID int64
	name        string
}

// sdeReprocessRow is one reprocessing output: the quantity of
// one material one portion of a type yields.
type sdeReprocessRow struct {
	typeID         int64
	materialTypeID int64
	quantity       int64
}

// sdeDynRangeRow is one mutaplasmid roll range: the
// multipliers a mutator can apply to one attribute.
type sdeDynRangeRow struct {
	mutatorTypeID int64
	attributeID   int64
	minMult       float64
	maxMult       float64
}

// sdeDynMappingRow is one mutaplasmid applicability mapping:
// a base type the mutator can be applied to, and the type an
// item of the base type becomes.
type sdeDynMappingRow struct {
	mutatorTypeID    int64
	applicableTypeID int64
	resultingTypeID  int64
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
	// The item sheet's columns (schema 140): the name the game
	// shows, whether it shows the attribute at all and at zero,
	// its section and its icon.
	displayName     string
	published       int64
	displayWhenZero int64
	categoryID      int64
	iconID          int64
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
	portionSize   int64 // units in one reprocessing portion; 0 when none
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
// references shorten it to parentGroupID, the parser accepts
// both).
type sdeMarketGroupRow struct {
	marketGroupID int64
	parentGroupID int64 // 0 = top level
	name          string
	iconID        int64 // dump icon reference, kept as data
	hasTypes      int64 // 1 when types list directly in this group
}

type sdeStationRow struct {
	stationID     int64
	name          string
	systemID      int64
	corporationID int64
}

// sdeNPCCorpRow is one NPC corporation from npcCorporations: the
// corporation and its faction, so
// the fee math can apply the character's faction standing
// with a station's owner.
type sdeNPCCorpRow struct {
	corporationID int64
	factionID     int64
}

type sdeSystemRow struct {
	systemID        int64
	name            string
	regionID        int64
	security        float64
	constellationID int64
	x, y, z         float64
	pos2dx, pos2dy  float64
	factionID       int64
	securityClass   string
}

// sdeConstellationRow is one constellation (schema 072).
type sdeConstellationRow struct {
	constellationID int64
	name            string
	regionID        int64
}

// sdeJumpRow is one stargate link, one direction (schema 072).
type sdeJumpRow struct {
	from, to int64
}

// sdeCelestialRow is one named in-system place (schema 090).
type sdeCelestialRow struct {
	itemID   int64
	systemID int64
	typeID   int64
	radius   float64 // metres
	name     string
}

type sdeRegionRow struct {
	regionID int64
	name     string
}

// ---------------------------------------------------------------------------
// Industry (schema 011, the build planner). The planner keeps only
// each blueprint's manufacturing activity: invention, research and
// reactions sit beside it in the blueprints table and are not the
// planner's business. buildIndustryRows flattens the parsed maps
// into sorted rows once the table is parsed.
// ---------------------------------------------------------------------------

// buildIndustryRows flattens the parsed industry maps into the
// sorted row slices storeSDE inserts. A blueprint becomes a
// planner row only when the dump gives it a manufacturing
// product; the production limit and base time default to 0 when
// the dump leaves them out.
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
// Dogma attributes (schema 012, the skill graph). typeDogma carries
// ~1.2M attribute values, but the skill planner only ever reads a
// dozen attribute IDs, verified against the dump's own
// dogmaAttributes names and live values (Gunnery 3300: 180=167
// Perception, 181=168 Willpower, 275=1.0):
//
//   275 skillTimeConstant, the skill rank multiplier
//   180 primaryAttribute, value is a character attribute ID
//   181 secondaryAttribute    (164-168: charisma/intelligence/
//                             memory/perception/willpower)
//
// Required skills come in (skill, level) attribute pairs, five
// pairs carry rows in the current dump; the sixth pair exists in
// the attribute table but is unused so far. Every pair found is
// honored, so a ship listing five prerequisites expands fully:
//
//   182/277, 183/278, 184/279, 1285/1286, 1289/1287, 1290/1288
//          (requiredSkillN / requiredSkillNLevel, note pair 5's
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

// ---------------------------------------------------------------------------
// Fitting simulator tables (schema 029): attribute metadata, the
// effect set with decoded modifiers, and type -> effect links.
// ---------------------------------------------------------------------------

// sdeModifierJSON is one entry of a dogmaEffects record's
// modifierInfo. Fields are pointers where the dump legitimately
// omits them (EffectStopper entries carry no
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

// buildSkillRows flattens the filtered dogma rows into the
// schema-012 tables. Skill meta covers published category-16
// types (the EVE skill category) that carry a skillTimeConstant,
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
// (importer volume, tens of thousands of rows, is why these are
// hand-rolled rather than sqlc; the meta writes do use sqlc against
// the transaction). Returns the total imported row count.
func (app *Application) storeSDE(ctx context.Context, base string, parsed *parsedSDE) (int64, error) {
	tx, err := app.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin SDE replace: %w", err)
	}
	defer tx.Rollback(ctx)

	for _, table := range []string{"sde_types", "sde_type_physics", "sde_reprocessing", "sde_groups", "sde_categories", "sde_market_groups", "sde_stations", "sde_npc_corporations", "sde_systems", "sde_regions",
		"sde_blueprints", "sde_blueprint_materials", "sde_blueprint_skills", "sde_invention", "sde_invention_materials",
		"sde_skill_meta", "sde_requirements",
		"sde_type_attributes", "sde_attribute_types", "sde_effects", "sde_effect_modifiers", "sde_type_effects"} {
		if _, err := tx.Exec(ctx, "DELETE FROM "+table); err != nil {
			return 0, fmt.Errorf("clear %s: %w", table, err)
		}
	}

	insert := func(query string, n int, fill func(i int) []any) error {
		batch := &pgx.Batch{}
		for i := 0; i < n; i++ {
			batch.Queue(query, fill(i)...)
		}
		br := tx.SendBatch(ctx, batch)
		defer br.Close()
		for i := 0; i < n; i++ {
			if _, err := br.Exec(); err != nil {
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
	if err := insert("INSERT INTO sde_type_physics (type_id, mass, volume, capacity, portion_size) VALUES ($1, $2, $3, $4, $5)", len(parsed.types), func(i int) []any {
		r := parsed.types[i]
		return []any{r.typeID, r.mass, r.volume, r.capacity, r.portionSize}
	}); err != nil {
		return 0, err
	}
	if err := insert("INSERT INTO sde_reprocessing (type_id, material_type_id, quantity) VALUES ($1, $2, $3)", len(parsed.reprocessing), func(i int) []any {
		r := parsed.reprocessing[i]
		return []any{r.typeID, r.materialTypeID, r.quantity}
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
	if err := insert("INSERT INTO sde_stations (station_id, name, system_id, corporation_id) VALUES ($1, $2, $3, $4)", len(parsed.stations), func(i int) []any {
		r := parsed.stations[i]
		return []any{r.stationID, r.name, r.systemID, r.corporationID}
	}); err != nil {
		return 0, err
	}
	if err := insert("INSERT INTO sde_npc_corporations (corporation_id, faction_id) VALUES ($1, $2)", len(parsed.npcCorps), func(i int) []any {
		r := parsed.npcCorps[i]
		return []any{r.corporationID, r.factionID}
	}); err != nil {
		return 0, err
	}
	if err := insert("INSERT INTO sde_systems (system_id, name, region_id, security, constellation_id, x, y, z, position2d_x, position2d_y, faction_id, security_class) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)", len(parsed.systems), func(i int) []any {
		r := parsed.systems[i]
		return []any{r.systemID, r.name, r.regionID, r.security, r.constellationID, r.x, r.y, r.z, r.pos2dx, r.pos2dy, r.factionID, r.securityClass}
	}); err != nil {
		return 0, err
	}
	// The map's constellations and stargate graph (schema 072)
	// replace only when this import carried them, like the
	// abyssal datasets below.
	if len(parsed.constellations) > 0 {
		if _, err := tx.Exec(ctx, "DELETE FROM sde_constellations"); err != nil {
			return 0, fmt.Errorf("clear sde_constellations: %w", err)
		}
		if err := insert("INSERT INTO sde_constellations (constellation_id, name, region_id) VALUES ($1, $2, $3)", len(parsed.constellations), func(i int) []any {
			r := parsed.constellations[i]
			return []any{r.constellationID, r.name, r.regionID}
		}); err != nil {
			return 0, err
		}
	}
	if len(parsed.jumps) > 0 {
		if _, err := tx.Exec(ctx, "DELETE FROM sde_system_jumps"); err != nil {
			return 0, fmt.Errorf("clear sde_system_jumps: %w", err)
		}
		if err := insert("INSERT INTO sde_system_jumps (from_system_id, to_system_id) VALUES ($1, $2) ON CONFLICT DO NOTHING", len(parsed.jumps), func(i int) []any {
			r := parsed.jumps[i]
			return []any{r.from, r.to}
		}); err != nil {
			return 0, err
		}
	}
	// The item sheet's tables (schema 140), each replaced only when
	// this import carried its file, like the map data.
	sheetTables := []struct {
		table string
		n     int
		query string
		fill  func(i int) []any
	}{
		{"sde_units", len(parsed.units), "INSERT INTO sde_units (unit_id, display_name) VALUES ($1, $2) ON CONFLICT DO NOTHING", func(i int) []any {
			r := parsed.units[i]
			return []any{r.unitID, r.displayName}
		}},
		{"sde_traits", len(parsed.traits), "INSERT INTO sde_traits (trait_id, type_id, skill_id, bonus, bonus_text, unit_id, importance) VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT DO NOTHING", func(i int) []any {
			r := parsed.traits[i]
			var bonus *float64
			if r.hasBonus {
				bonus = &r.bonus
			}
			return []any{r.traitID, r.typeID, r.skillID, bonus, r.bonusText, r.unitID, r.importance}
		}},
		{"sde_meta_types", len(parsed.metaTypes), "INSERT INTO sde_meta_types (type_id, parent_type_id, meta_group_id) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING", func(i int) []any {
			r := parsed.metaTypes[i]
			return []any{r.typeID, r.parentTypeID, r.metaGroupID}
		}},
		{"sde_meta_groups", len(parsed.metaGroups), "INSERT INTO sde_meta_groups (meta_group_id, name) VALUES ($1, $2) ON CONFLICT DO NOTHING", func(i int) []any {
			r := parsed.metaGroups[i]
			return []any{r.metaGroupID, r.name}
		}},
		{"sde_character_titles", len(parsed.titles), "INSERT INTO sde_character_titles (title_id, name) VALUES ($1, $2) ON CONFLICT DO NOTHING", func(i int) []any {
			r := parsed.titles[i]
			return []any{r.titleID, r.name}
		}},
		{"sde_factions", len(parsed.factions), "INSERT INTO sde_factions (faction_id, name, corporation_id, militia_corporation_id) VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING", func(i int) []any {
			r := parsed.factions[i]
			return []any{r.factionID, r.name, r.corporationID, r.militiaCorporationID}
		}},
		{"sde_schematics", len(parsed.schematics), "INSERT INTO sde_schematics (schematic_id, name, cycle_time) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING", func(i int) []any {
			r := parsed.schematics[i]
			return []any{r.schematicID, r.name, r.cycleTime}
		}},
	}
	for _, st := range sheetTables {
		if st.n == 0 {
			continue
		}
		if _, err := tx.Exec(ctx, "DELETE FROM "+st.table); err != nil {
			return 0, fmt.Errorf("clear %s: %w", st.table, err)
		}
		if err := insert(st.query, st.n, st.fill); err != nil {
			return 0, err
		}
	}
	// The buff tables (schema 145), replaced together when this
	// import carried all five files.
	if len(parsed.dbuffs) > 0 {
		for _, table := range []string{"sde_dbuffs", "sde_dbuff_modifiers"} {
			if _, err := tx.Exec(ctx, "DELETE FROM "+table); err != nil {
				return 0, fmt.Errorf("clear %s: %w", table, err)
			}
		}
		if err := insert("INSERT INTO sde_dbuffs (dbuff_id, display_name, aggregate, operation) VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING", len(parsed.dbuffs), func(i int) []any {
			r := parsed.dbuffs[i]
			return []any{r.dbuffID, r.displayName, r.aggregate, r.operation}
		}); err != nil {
			return 0, err
		}
		if err := insert("INSERT INTO sde_dbuff_modifiers (dbuff_id, kind, attribute_id, group_id, skill_id) VALUES ($1, $2, $3, $4, $5)", len(parsed.dbuffMods), func(i int) []any {
			r := parsed.dbuffMods[i]
			return []any{r.dbuffID, r.kind, r.attributeID, r.groupID, r.skillID}
		}); err != nil {
			return 0, err
		}
	}
	// Variation compare tables (schema 141), rebuilt whenever the
	// import carried the variation links.
	if len(parsed.metaTypes) > 0 {
		varCols, varRows := buildVariations(parsed)
		for _, table := range []string{"sde_variation_columns", "sde_variations"} {
			if _, err := tx.Exec(ctx, "DELETE FROM "+table); err != nil {
				return 0, fmt.Errorf("clear %s: %w", table, err)
			}
		}
		if err := insert("INSERT INTO sde_variation_columns (root_type_id, ord, attribute_id) VALUES ($1, $2, $3)", len(varCols), func(i int) []any {
			c := varCols[i]
			return []any{c.rootTypeID, c.ord, c.attributeID}
		}); err != nil {
			return 0, err
		}
		if err := insert("INSERT INTO sde_variations (type_id, root_type_id, meta_group_id, meta_level, vals, tier) VALUES ($1, $2, $3, $4, $5, $6)", len(varRows), func(i int) []any {
			r := varRows[i]
			return []any{r.typeID, r.rootTypeID, r.metaGroupID, r.metaLevel, r.vals, r.tier}
		}); err != nil {
			return 0, err
		}
	}

	// The workbench's Browse tree (schema 142), rebuilt whenever
	// the import carried the dogma effects it sorts types by.
	if len(parsed.typeEffects) > 0 {
		browseGroups, browseTypes := buildFitBrowse(parsed)
		for _, table := range []string{"fit_browse_groups", "fit_browse_types"} {
			if _, err := tx.Exec(ctx, "DELETE FROM "+table); err != nil {
				return 0, fmt.Errorf("clear %s: %w", table, err)
			}
		}
		if err := insert("INSERT INTO fit_browse_groups (family, market_group_id, parent_group_id, name, type_count) VALUES ($1, $2, $3, $4, $5)", len(browseGroups), func(i int) []any {
			g := browseGroups[i]
			return []any{g.family, g.marketGroupID, g.parentGroupID, g.name, g.typeCount}
		}); err != nil {
			return 0, err
		}
		if err := insert("INSERT INTO fit_browse_types (family, type_id, market_group_id, name) VALUES ($1, $2, $3, $4)", len(browseTypes), func(i int) []any {
			t := browseTypes[i]
			return []any{t.family, t.typeID, t.marketGroupID, t.name}
		}); err != nil {
			return 0, err
		}
	}

	// Celestials (schema 090), replaced only when this import
	// carried the denormalize file, like the map data above.
	if len(parsed.celestials) > 0 {
		if _, err := tx.Exec(ctx, "DELETE FROM sde_celestials"); err != nil {
			return 0, fmt.Errorf("clear sde_celestials: %w", err)
		}
		if err := insert("INSERT INTO sde_celestials (item_id, system_id, name, type_id, radius) VALUES ($1, $2, $3, $4, $5)", len(parsed.celestials), func(i int) []any {
			r := parsed.celestials[i]
			return []any{r.itemID, r.systemID, r.name, r.typeID, r.radius}
		}); err != nil {
			return 0, err
		}
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
	if err := insert("INSERT INTO sde_invention (blueprint_type_id, product_type_id, runs) VALUES ($1, $2, $3)", len(parsed.invention), func(i int) []any {
		r := parsed.invention[i]
		return []any{r.blueprintTypeID, r.productTypeID, r.runs}
	}); err != nil {
		return 0, err
	}
	if err := insert("INSERT INTO sde_invention_materials (blueprint_type_id, material_type_id, quantity) VALUES ($1, $2, $3)", len(parsed.invMaterials), func(i int) []any {
		r := parsed.invMaterials[i]
		return []any{r.blueprintTypeID, r.materialTypeID, r.quantity}
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
	if err := insert("INSERT INTO sde_attribute_types (attribute_id, name, stackable, high_is_good, unit_id, default_value, display_name, published, display_when_zero, category_id, icon_id) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)", len(parsed.attrTypes), func(i int) []any {
		r := parsed.attrTypes[i]
		return []any{r.attributeID, r.name, r.stackable, r.highIsGood, r.unitID, r.defaultValue, r.displayName, r.published, r.displayWhenZero, r.categoryID, r.iconID}
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

	// The mutaplasmid roll ranges (schema 055) replace only
	// when this import actually carried the dataset: a dump
	// without it leaves the stored ranges untouched rather
	// than wiping them for an absence (see sdeRangesFileName).
	// sde_build records the dump's build, so a row can always
	// say which dump it came from.
	build := parsed.build
	if len(parsed.dynRanges) > 0 {
		if _, err := tx.Exec(ctx, "DELETE FROM abyssal_ranges"); err != nil {
			return 0, fmt.Errorf("clear abyssal_ranges: %w", err)
		}
		if err := insert("INSERT INTO abyssal_ranges (mutator_type_id, attribute_id, min_mult, max_mult, sde_build) VALUES ($1, $2, $3, $4, $5)", len(parsed.dynRanges), func(i int) []any {
			r := parsed.dynRanges[i]
			return []any{r.mutatorTypeID, r.attributeID, r.minMult, r.maxMult, build}
		}); err != nil {
			return 0, err
		}
	}

	// The mutaplasmid applicability mappings (schema 063)
	// replace under the same rule as the ranges above: only
	// an import that carried the dataset touches the stored
	// rows.
	if len(parsed.dynMappings) > 0 {
		if _, err := tx.Exec(ctx, "DELETE FROM abyssal_mappings"); err != nil {
			return 0, fmt.Errorf("clear abyssal_mappings: %w", err)
		}
		if err := insert("INSERT INTO abyssal_mappings (mutator_type_id, applicable_type_id, resulting_type_id, sde_build) VALUES ($1, $2, $3, $4)", len(parsed.dynMappings), func(i int) []any {
			r := parsed.dynMappings[i]
			return []any{r.mutatorTypeID, r.applicableTypeID, r.resultingTypeID, build}
		}); err != nil {
			return 0, err
		}
	}

	total := int64(len(parsed.types) + len(parsed.types) + len(parsed.groups) + len(parsed.categories) +
		len(parsed.marketGroups) +
		len(parsed.stations) + len(parsed.systems) + len(parsed.regions) +
		len(parsed.blueprints) + len(parsed.bpMaterials) + len(parsed.bpSkills) +
		len(parsed.skillMeta) + len(parsed.skillReqs) +
		len(parsed.typeAttrs) + len(parsed.attrTypes) + len(parsed.effects) +
		len(parsed.modifiers) + len(parsed.typeEffects) + len(parsed.dynRanges) +
		len(parsed.dynMappings) +
		len(parsed.units) + len(parsed.traits) + len(parsed.metaTypes) + len(parsed.metaGroups) +
		len(parsed.dbuffs) + len(parsed.dbuffMods) + len(parsed.titles))
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
		// tree from 6, the schema-029 full dogma set from 7,
		// every effect (not just modifier-bearing ones) from 8,
		// the mutaplasmid roll ranges (schema 055) from 9, and
		// the mutaplasmid applicability mappings (schema 063)
		// from 10, and the universe map's geometry and stargate
		// graph (schema 072) from 11, and the item sheet's
		// attribute columns, units, traits and variation links
		// (schema 140) from 15, and the variation compare tables
		// (schema 141) from 16, and the buff tables (schema 145)
		// from 20 (sdeMaintenance backfills once when
		// it's behind).
		{Key: "sde_import_version", Value: sdeImportVersion},
	}
	if rel := parsed.release; rel != nil {
		meta = append(meta,
			db.UpsertSDEMetaParams{Key: "release", Value: rel.Release},
			db.UpsertSDEMetaParams{Key: "release_build", Value: strconv.FormatInt(rel.BuildNumber, 10)},
			db.UpsertSDEMetaParams{Key: "release_date", Value: rel.ReleaseDate},
		)
	}
	// The versions replace the last import's whole set, so a table
	// the importer stopped reading leaves none behind.
	if _, err := tx.Exec(ctx, "DELETE FROM sde_meta WHERE key LIKE 'version:%'"); err != nil {
		return 0, fmt.Errorf("clear SDE versions: %w", err)
	}
	for table, v := range parsed.versions {
		meta = append(meta, db.UpsertSDEMetaParams{Key: "version:" + table, Value: v})
	}
	for _, m := range meta {
		if err := tq.UpsertSDEMeta(ctx, m); err != nil {
			return 0, fmt.Errorf("write SDE meta %s: %w", m.Key, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
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

// sdeTableVersion is one SDE API table and the version imported.
type sdeTableVersion struct {
	Table   string
	Version string // shortened; "" when never imported from the API
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
	Release    string // the SDE API release imported, "" when none
	Tables     []sdeTableCount
	Versions   []sdeTableVersion
	Override   *sdeOverrideView // the active upload, nil when none
}

// loadSDEView builds the Sync page's SDE block from the tables, the
// import meta, and the live importer status.
func (app *Application) loadSDEView(ctx context.Context) *sdeView {
	status := app.snapshotSDEStatus()
	view := &sdeView{
		SourceBase: app.cfg.SDEAPIURL(),
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

	view.Override = app.loadSDEOverrideView(ctx)
	if importedAt, err := app.sdeMeta(ctx, "imported_at"); err == nil {
		view.ImportedAt = importedAt
	}
	if source, err := app.sdeMeta(ctx, "source_base"); err == nil && source != "" {
		view.SourceBase = source
	}
	if rel, err := app.sdeMeta(ctx, "release"); err == nil && rel != "" {
		build, _ := app.sdeMeta(ctx, "release_build")
		date, _ := app.sdeMeta(ctx, "release_date")
		view.Release = fmt.Sprintf("%s, build %s, published %s", rel, build, date)
	}
	versions, err := app.storedSDEVersions(ctx)
	if err != nil {
		logging.Errorf("sync: %v", err)
	}
	for _, t := range sdeAPITables {
		v := versions[t.name]
		if len(v) > 12 {
			v = v[:12]
		}
		view.Versions = append(view.Versions, sdeTableVersion{Table: t.name, Version: v})
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

// sdeInventionRow is one invention: a T1 blueprint, the T2 blueprint
// copy it makes, and that copy's runs.
type sdeInventionRow struct {
	blueprintTypeID, productTypeID, runs int64
}

// buildInventionRows flattens the invention maps into sorted rows.
func (parsed *parsedSDE) buildInventionRows() {
	parsed.invention = parsed.invention[:0]
	for key, runs := range parsed.invProduct {
		parsed.invention = append(parsed.invention, sdeInventionRow{blueprintTypeID: key[0], productTypeID: key[1], runs: runs})
	}
	sort.Slice(parsed.invention, func(i, j int) bool {
		a, b := parsed.invention[i], parsed.invention[j]
		if a.blueprintTypeID != b.blueprintTypeID {
			return a.blueprintTypeID < b.blueprintTypeID
		}
		return a.productTypeID < b.productTypeID
	})
	parsed.invMaterials = parsed.invMaterials[:0]
	for key, qty := range parsed.invMats {
		if qty > 0 {
			parsed.invMaterials = append(parsed.invMaterials, sdeBlueprintMaterialRow{blueprintTypeID: key[0], materialTypeID: key[1], quantity: qty})
		}
	}
	sort.Slice(parsed.invMaterials, func(i, j int) bool {
		a, b := parsed.invMaterials[i], parsed.invMaterials[j]
		if a.blueprintTypeID != b.blueprintTypeID {
			return a.blueprintTypeID < b.blueprintTypeID
		}
		return a.materialTypeID < b.materialTypeID
	})
}
