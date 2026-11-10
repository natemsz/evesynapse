package app

import (
	"context"
	json "github.com/goccy/go-json"
	"sort"
	"strings"
	"sync"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// The in-memory search index: a snapshot of everything the search
// bar matches against (the published SDE types with their pool
// facts, systems, regions, and the warmed corporation, alliance
// and pilot records' name fields), so a keystroke reads memory
// instead of scanning the record and SDE tables and decoding
// payloads per query.
//
// Freshness keeps today's contract: a record is searchable as
// soon as it is stored (the store paths upsert into the live
// snapshot in the same pass, see the noteSearch*Record hooks),
// and the SDE part is invalidated where an import lands
// (importSDE). The snapshot is built once at worker startup
// (runWorker kicks off the build) and stays resident; whole
// snapshot rebuilds are only a long backstop. A search that
// finds no snapshot, or one past its age, starts a background
// build and serves the SQL path meanwhile; searches never block
// on a build, and a search never falls back to SQL just because
// the snapshot is old while one is resident. An application
// without a worker context (a bare test literal) never builds in
// the background; it serves the SQL path until a build is run
// explicitly.
//
// One deliberate difference from the SQL path: the record name
// searches shortlist the first 100 matches by id before ranking,
// so a genuine match past the window never surfaces. The index
// ranks every match, so for very common text the best matches
// win instead of the lowest ids.
// ---------------------------------------------------------------------------

// searchIndexMaxAge is the backstop rebuild age: a snapshot older
// than this is rebuilt in the background on the next search.
// It is a long backstop only. Freshness comes from the record
// hooks (noteSearch*Record upserts into the live snapshot as
// records are stored) and from SDE invalidation
// (invalidateSearchIndexSDE), so the snapshot stays resident
// and serving between rebuilds instead of dropping out.
const searchIndexMaxAge = 24 * time.Hour

type searchIndexType struct {
	id         int64
	name       string
	lower      string
	group      string
	category   string
	categoryID int64
	market     bool // market_group_id > 0
	blueprint  bool // a blueprint builds it
	skill      bool // it carries skill meta
}

// inPool applies the shared feed's floor and pool filters (see
// SuggestSDETypesShared): the market floor for every pool, the
// broad pool's contract-only blueprint widening, then the
// planner and skills pools' own facts.
func (e searchIndexType) inPool(pool string) bool {
	if !e.market && !(pool == suggestPoolAll && e.categoryID == 9) {
		return false
	}
	switch pool {
	case suggestPoolPlanner:
		return e.blueprint
	case suggestPoolSkills:
		return e.skill
	}
	return true
}

type searchIndexSystem struct {
	id         int64
	name       string
	lower      string
	regionID   int64
	regionName string
	security   float64
}

type searchIndexRegion struct {
	id    int64
	name  string
	lower string
}

type searchIndexOrg struct {
	name   string
	lower  string
	ticker string
}

type searchIndexPilot struct {
	name     string
	lower    string
	corpName string
}

// searchSnapshot is one built index. The SDE slices never change
// after the swap; the record maps are kept current by the store
// hooks under the index lock, so record reads hold it.
type searchSnapshot struct {
	builtAt   time.Time
	types     []searchIndexType
	systems   []searchIndexSystem
	regions   []searchIndexRegion
	corps     map[int64]searchIndexOrg
	alliances map[int64]searchIndexOrg
	pilots    map[int64]searchIndexPilot
}

type searchRecordKind uint8

const (
	searchRecordPilot searchRecordKind = iota
	searchRecordCorporation
	searchRecordAlliance
)

type searchRecordKey struct {
	kind searchRecordKind
	id   int64
}

// searchRecordDelta is one record's latest search entry as the
// store paths report it: the name fields for a ready record, or
// a drop for anything else (un-readied, missing, undecodable).
// Deltas noted while a build runs are folded into the snapshot
// it swaps in, so a store is never lost to a rebuild.
type searchRecordDelta struct {
	name     string
	ticker   string
	corpName string
	drop     bool
}

func (d searchRecordDelta) applyTo(snap *searchSnapshot, key searchRecordKey) {
	switch key.kind {
	case searchRecordPilot:
		if d.drop {
			delete(snap.pilots, key.id)
		} else {
			snap.pilots[key.id] = searchIndexPilot{name: d.name, lower: strings.ToLower(d.name), corpName: d.corpName}
		}
	case searchRecordCorporation:
		if d.drop {
			delete(snap.corps, key.id)
		} else {
			snap.corps[key.id] = searchIndexOrg{name: d.name, lower: strings.ToLower(d.name), ticker: d.ticker}
		}
	case searchRecordAlliance:
		if d.drop {
			delete(snap.alliances, key.id)
		} else {
			snap.alliances[key.id] = searchIndexOrg{name: d.name, lower: strings.ToLower(d.name), ticker: d.ticker}
		}
	}
}

// searchIndexState is the Application's index: the live snapshot,
// the build in flight, and the record deltas a build must not
// lose. The zero value is ready to use.
type searchIndexState struct {
	mu       sync.RWMutex
	snap     *searchSnapshot
	building bool
	sdeStale bool
	pending  map[searchRecordKey]searchRecordDelta
}

// searchSnapshot returns the live snapshot, starting a background
// rebuild when there is none or it has gone stale. It returns nil
// when no snapshot has landed yet; callers serve the SQL path.
// In low-RAM mode (LOW_RAM_MODE=1) it always returns nil without
// starting a build, so every search path serves its SQL fallback.
func (app *Application) searchSnapshot() *searchSnapshot {
	if app.cfg.lowRAMMode {
		return nil
	}
	idx := &app.searchIdx
	idx.mu.RLock()
	snap := idx.snap
	stale := snap == nil || idx.sdeStale || time.Since(snap.builtAt) > searchIndexMaxAge
	idx.mu.RUnlock()
	if stale {
		app.kickSearchIndexBuild()
	}
	return snap
}

// kickSearchIndexBuild starts one background build when none is
// running. It needs the worker context: without one there is no
// background lifecycle and the SQL path serves. In low-RAM mode
// (LOW_RAM_MODE=1) it never starts a build.
func (app *Application) kickSearchIndexBuild() {
	if app.cfg.lowRAMMode {
		return
	}
	if app.workerCtx == nil {
		return
	}
	idx := &app.searchIdx
	idx.mu.Lock()
	if idx.building {
		idx.mu.Unlock()
		return
	}
	idx.building = true
	idx.mu.Unlock()
	go func() {
		defer func() {
			idx.mu.Lock()
			idx.building = false
			idx.mu.Unlock()
		}()
		if err := app.buildSearchIndex(app.workerCtx); err != nil {
			logging.Warnf("search index: build: %v", err)
		}
	}()
}

// buildSearchIndex reads the whole search surface once and swaps
// the snapshot whole. Record deltas noted up to the swap are
// folded in, so nothing stored while the build ran is lost.
func (app *Application) buildSearchIndex(ctx context.Context) error {
	typeRows, err := app.queries.ListSearchIndexTypes(ctx)
	if err != nil {
		return err
	}
	systemRows, err := app.queries.ListSearchIndexSystems(ctx)
	if err != nil {
		return err
	}
	regionRows, err := app.queries.ListSearchIndexRegions(ctx)
	if err != nil {
		return err
	}
	pilotRows, err := app.queries.ListSearchIndexPilotRecords(ctx)
	if err != nil {
		return err
	}
	corpRows, err := app.queries.ListSearchIndexCorporationRecords(ctx)
	if err != nil {
		return err
	}
	allianceRows, err := app.queries.ListSearchIndexAllianceRecords(ctx)
	if err != nil {
		return err
	}

	snap := &searchSnapshot{
		builtAt:   time.Now(),
		corps:     make(map[int64]searchIndexOrg, len(corpRows)),
		alliances: make(map[int64]searchIndexOrg, len(allianceRows)),
		pilots:    make(map[int64]searchIndexPilot, len(pilotRows)),
	}
	for _, row := range typeRows {
		snap.types = append(snap.types, searchIndexType{
			id: row.TypeID, name: row.Name, lower: strings.ToLower(row.Name),
			group: row.GroupName, category: row.CategoryName, categoryID: row.CategoryID,
			market: row.MarketGroupID > 0, blueprint: row.HasBlueprint, skill: row.HasSkillMeta,
		})
	}
	for _, row := range systemRows {
		snap.systems = append(snap.systems, searchIndexSystem{
			id: row.SystemID, name: row.Name, lower: strings.ToLower(row.Name),
			regionID: row.RegionID, regionName: row.RegionName, security: row.Security,
		})
	}
	for _, row := range regionRows {
		snap.regions = append(snap.regions, searchIndexRegion{id: row.RegionID, name: row.Name, lower: strings.ToLower(row.Name)})
	}
	for _, row := range pilotRows {
		if name, corpName, ok := searchPilotEntry(row.Payload); ok {
			snap.pilots[row.CharacterID] = searchIndexPilot{name: name, lower: strings.ToLower(name), corpName: corpName}
		}
	}
	for _, row := range corpRows {
		if name, ticker, ok := searchCorpEntry(row.Payload); ok {
			snap.corps[row.CorporationID] = searchIndexOrg{name: name, lower: strings.ToLower(name), ticker: ticker}
		}
	}
	for _, row := range allianceRows {
		if name, ticker, ok := searchAllianceEntry(row.Payload); ok {
			snap.alliances[row.AllianceID] = searchIndexOrg{name: name, lower: strings.ToLower(name), ticker: ticker}
		}
	}

	idx := &app.searchIdx
	idx.mu.Lock()
	for key, delta := range idx.pending {
		delta.applyTo(snap, key)
	}
	idx.pending = make(map[searchRecordKey]searchRecordDelta)
	idx.snap = snap
	idx.sdeStale = false
	idx.mu.Unlock()
	return nil
}

// invalidateSearchIndexSDE marks the snapshot's SDE part stale
// and starts the rebuild: called where a new SDE becomes live
// (importSDE), whichever trigger ran the import. In low-RAM mode
// (LOW_RAM_MODE=1) there is no snapshot to mark stale.
func (app *Application) invalidateSearchIndexSDE() {
	if app.cfg.lowRAMMode {
		return
	}
	idx := &app.searchIdx
	idx.mu.Lock()
	idx.sdeStale = true
	idx.mu.Unlock()
	app.kickSearchIndexBuild()
}

// searchPilotEntry reads a stored pilot record's search fields
// with the same rules the SQL search path applies: an undecodable
// payload or an empty profile name is not a search entry.
func searchPilotEntry(payload string) (name, corpName string, ok bool) {
	var p pilotPayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil || p.Profile.Name == "" {
		return "", "", false
	}
	return p.Profile.Name, p.Corp.Name, true
}

func searchCorpEntry(payload string) (name, ticker string, ok bool) {
	p, ok := decodeCorpPayload(payload)
	if !ok {
		return "", "", false
	}
	return p.Corp.Name, p.Corp.Ticker, true
}

func searchAllianceEntry(payload string) (name, ticker string, ok bool) {
	p, ok := decodeAlliancePayload(payload)
	if !ok {
		return "", "", false
	}
	return p.Alliance.Name, p.Alliance.Ticker, true
}

// noteSearchRecord folds one store's delta into the live
// snapshot and remembers it for the build in flight, if any.
// In low-RAM mode (LOW_RAM_MODE=1) it returns at once: with no
// builds running, pending would otherwise grow one entry per
// record store and never drain.
func (app *Application) noteSearchRecord(key searchRecordKey, delta searchRecordDelta) {
	if app.cfg.lowRAMMode {
		return
	}
	idx := &app.searchIdx
	idx.mu.Lock()
	if idx.pending == nil {
		idx.pending = make(map[searchRecordKey]searchRecordDelta)
	}
	idx.pending[key] = delta
	if idx.snap != nil {
		delta.applyTo(idx.snap, key)
	}
	idx.mu.Unlock()
}

// noteSearchPilotRecord reports a pilot record store to the
// index: a ready record with a readable profile is searchable
// from this moment, anything else leaves the index.
func (app *Application) noteSearchPilotRecord(id int64, state, payload string) {
	delta := searchRecordDelta{drop: true}
	if state == pilotStateReady && payload != "" {
		if name, corpName, ok := searchPilotEntry(payload); ok {
			delta = searchRecordDelta{name: name, corpName: corpName}
			app.noteEntity(id, entityDirectoryEntry{ID: id, Name: name, Kind: "pilot"}, false)
		} else {
			app.noteEntity(id, entityDirectoryEntry{}, true)
		}
	} else {
		app.noteEntity(id, entityDirectoryEntry{}, true)
	}
	app.noteSearchRecord(searchRecordKey{kind: searchRecordPilot, id: id}, delta)
}

func (app *Application) noteSearchCorporationRecord(id int64, state, payload string) {
	delta := searchRecordDelta{drop: true}
	if state == orgStateReady && payload != "" {
		if name, ticker, ok := searchCorpEntry(payload); ok {
			delta = searchRecordDelta{name: name, ticker: ticker}
			app.noteEntity(id, entityDirectoryEntry{ID: id, Name: name, Ticker: ticker, Kind: "corporation"}, false)
		} else {
			app.noteEntity(id, entityDirectoryEntry{}, true)
		}
	} else {
		app.noteEntity(id, entityDirectoryEntry{}, true)
	}
	app.noteSearchRecord(searchRecordKey{kind: searchRecordCorporation, id: id}, delta)
}

func (app *Application) noteSearchAllianceRecord(id int64, state, payload string) {
	delta := searchRecordDelta{drop: true}
	if state == orgStateReady && payload != "" {
		if name, ticker, ok := searchAllianceEntry(payload); ok {
			delta = searchRecordDelta{name: name, ticker: ticker}
			app.noteEntity(id, entityDirectoryEntry{ID: id, Name: name, Ticker: ticker, Kind: "alliance"}, false)
		} else {
			app.noteEntity(id, entityDirectoryEntry{}, true)
		}
	} else {
		app.noteEntity(id, entityDirectoryEntry{}, true)
	}
	app.noteSearchRecord(searchRecordKey{kind: searchRecordAlliance, id: id}, delta)
}

// setPilotRecord stores a pilot record and notes it to the
// search index in the same pass, so the record is searchable as
// soon as it lands.
func (app *Application) setPilotRecord(ctx context.Context, params db.SetPilotRecordParams) error {
	if err := app.queries.SetPilotRecord(ctx, params); err != nil {
		return err
	}
	app.noteSearchPilotRecord(params.CharacterID, params.State, params.Payload)
	return nil
}

func (app *Application) setCorporationRecord(ctx context.Context, params db.SetCorporationRecordParams) error {
	if err := app.queries.SetCorporationRecord(ctx, params); err != nil {
		return err
	}
	app.noteSearchCorporationRecord(params.CorporationID, params.State, params.Payload)
	return nil
}

func (app *Application) setAllianceRecord(ctx context.Context, params db.SetAllianceRecordParams) error {
	if err := app.queries.SetAllianceRecord(ctx, params); err != nil {
		return err
	}
	app.noteSearchAllianceRecord(params.AllianceID, params.State, params.Payload)
	return nil
}

// ---------------------------------------------------------------------------
// Snapshot searches: the SQL queries' match, ordering and labels,
// over memory. Each returns hits in the order the SQL path's
// post-processing produces, capped the same way.
// ---------------------------------------------------------------------------

// suggestTypes serves the shared suggestion feed from the
// snapshot: substring match, prefix tier first then name, the
// pool filters, the caller's limit.
func (snap *searchSnapshot) suggestTypes(q, pool string, limit int64) []suggestItem {
	out := []suggestItem{}
	lowerQ := strings.ToLower(q)
	type match struct {
		e    searchIndexType
		tier int
	}
	var matches []match
	for _, e := range snap.types {
		if !strings.Contains(e.lower, lowerQ) || !e.inPool(pool) {
			continue
		}
		tier := 1
		if strings.HasPrefix(e.lower, lowerQ) {
			tier = 0
		}
		matches = append(matches, match{e: e, tier: tier})
	}
	// Order by lower(name), then id, the SQL path's ORDER BY
	// exactly. Raw name order would follow the database
	// collation there and byte order here, and the two disagree
	// (under en_US, "of Elsewhere" sorts before "Skill").
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].tier != matches[j].tier {
			return matches[i].tier < matches[j].tier
		}
		if matches[i].e.lower != matches[j].e.lower {
			return matches[i].e.lower < matches[j].e.lower
		}
		return matches[i].e.id < matches[j].e.id
	})
	for _, m := range matches {
		if int64(len(out)) >= limit {
			break
		}
		out = append(out, suggestItem{ID: m.e.id, Name: m.e.name, Label: suggestLabel(m.e.category, m.e.group)})
	}
	return out
}

func (snap *searchSnapshot) searchSystems(q string, lim int64) []searchSystem {
	lowerQ := strings.ToLower(q)
	type match struct {
		e    searchIndexSystem
		tier int
	}
	var matches []match
	for _, e := range snap.systems {
		if !strings.Contains(e.lower, lowerQ) {
			continue
		}
		tier := 1
		if strings.HasPrefix(e.lower, lowerQ) {
			tier = 0
		}
		matches = append(matches, match{e: e, tier: tier})
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].tier != matches[j].tier {
			return matches[i].tier < matches[j].tier
		}
		return matches[i].e.name < matches[j].e.name
	})
	out := []searchSystem{}
	for _, m := range matches {
		if int64(len(out)) >= lim {
			break
		}
		sec, tier := secRound(m.e.security)
		out = append(out, searchSystem{
			searchHit: searchHit{Kind: "system", ID: m.e.id, Name: m.e.name, Label: "System · " + m.e.regionName,
				Sec: sec, SecTier: tier},
			RegionID: m.e.regionID, RegionName: m.e.regionName, Security: m.e.security,
		})
	}
	return out
}

func (snap *searchSnapshot) searchRegions(q string, lim int64) []searchHit {
	lowerQ := strings.ToLower(q)
	type match struct {
		e    searchIndexRegion
		tier int
	}
	var matches []match
	for _, e := range snap.regions {
		if !strings.Contains(e.lower, lowerQ) {
			continue
		}
		tier := 1
		if strings.HasPrefix(e.lower, lowerQ) {
			tier = 0
		}
		matches = append(matches, match{e: e, tier: tier})
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].tier != matches[j].tier {
			return matches[i].tier < matches[j].tier
		}
		return matches[i].e.name < matches[j].e.name
	})
	out := []searchHit{}
	for _, m := range matches {
		if int64(len(out)) >= lim {
			break
		}
		out = append(out, searchHit{Kind: "region", ID: m.e.id, Name: m.e.name, Label: "Region"})
	}
	return out
}

// searchIndexCorporations reads the snapshot's corporation
// entries under the index lock. ok is false when no snapshot
// exists, telling the caller to serve the SQL path.
func (app *Application) searchIndexCorporations(q string, lim int) (hits []searchHit, ok bool) {
	idx := &app.searchIdx
	idx.mu.RLock()
	snap := idx.snap
	if snap == nil {
		idx.mu.RUnlock()
		return nil, false
	}
	type found struct {
		id int64
		e  searchIndexOrg
	}
	var founds []found
	for id, e := range snap.corps {
		if _, ok := nameMatchTier(e.name, q); ok {
			founds = append(founds, found{id: id, e: e})
		}
	}
	idx.mu.RUnlock()
	sort.Slice(founds, func(i, j int) bool { return founds[i].id < founds[j].id })
	for _, f := range founds {
		label := "Corporation"
		if f.e.ticker != "" {
			label = "[" + f.e.ticker + "]"
		}
		hits = append(hits, searchHit{Kind: "corporation", ID: f.id, Name: f.e.name, Label: label})
	}
	sortHitsByName(hits, q)
	return capHits(hits, lim), true
}

func (app *Application) searchIndexAlliances(q string, lim int) (hits []searchHit, ok bool) {
	idx := &app.searchIdx
	idx.mu.RLock()
	snap := idx.snap
	if snap == nil {
		idx.mu.RUnlock()
		return nil, false
	}
	type found struct {
		id int64
		e  searchIndexOrg
	}
	var founds []found
	for id, e := range snap.alliances {
		if _, ok := nameMatchTier(e.name, q); ok {
			founds = append(founds, found{id: id, e: e})
		}
	}
	idx.mu.RUnlock()
	sort.Slice(founds, func(i, j int) bool { return founds[i].id < founds[j].id })
	for _, f := range founds {
		label := "Alliance"
		if f.e.ticker != "" {
			label = "[" + f.e.ticker + "]"
		}
		hits = append(hits, searchHit{Kind: "alliance", ID: f.id, Name: f.e.name, Label: label})
	}
	sortHitsByName(hits, q)
	return capHits(hits, lim), true
}

func (app *Application) searchIndexPilots(q string, ownIDs map[int64]bool, lim int) (hits []searchHit, ok bool) {
	idx := &app.searchIdx
	idx.mu.RLock()
	snap := idx.snap
	if snap == nil {
		idx.mu.RUnlock()
		return nil, false
	}
	type found struct {
		id int64
		e  searchIndexPilot
	}
	var founds []found
	for id, e := range snap.pilots {
		if ownIDs[id] {
			continue
		}
		if _, ok := nameMatchTier(e.name, q); ok {
			founds = append(founds, found{id: id, e: e})
		}
	}
	idx.mu.RUnlock()
	sort.Slice(founds, func(i, j int) bool { return founds[i].id < founds[j].id })
	for _, f := range founds {
		label := "Pilot"
		if f.e.corpName != "" {
			label = f.e.corpName
		}
		hits = append(hits, searchHit{Kind: "pilot", ID: f.id, Name: f.e.name, Label: label})
	}
	sortHitsByName(hits, q)
	return capHits(hits, lim), true
}
