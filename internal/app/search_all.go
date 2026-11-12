package app

import (
	"context"
	json "github.com/goccy/go-json"
	"net/http"
	"strings"
	"sync"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// searchLimits caps each kind of hit one search gathers: the top
// bar's few, or the results page's many.
type searchLimits struct {
	characters, items, systems, regions, orgs, pilots int
}

// topbarLimits are the top bar's and the quick-jump palette's.
var topbarLimits = searchLimits{characters: 4, items: 6, systems: 4, regions: 3, orgs: 4, pilots: 4}

// searchSystem is a system hit with what the results page filters
// systems by.
type searchSystem struct {
	searchHit
	RegionID   int64
	RegionName string
	Security   float64
}

// searchSet is everything one query finds, kind by kind, local
// tables only: the signed-in user's characters, items, systems,
// regions, warmed corporation, alliance and pilot records, and a
// pilot name no local tier knows yet (Pending, or a pilot resolved
// a moment ago and still warming, in Pilots).
type searchSet struct {
	Characters, Items, Regions, Corporations, Alliances, Pilots []searchHit
	Systems                                                     []searchSystem
	Pending                                                     bool
	// Fix is the corrected query the static data tiers answer when
	// the query as typed found none of them (search_typos.go); "" when
	// they answer it as typed. People and organizations are never
	// corrected.
	Fix string
	// guesses are the pages a pick is likely to open next, for the
	// typing warm-up (setTypingGuesses).
	guesses []typingGuess
}

// searchQuery is a request's query as typed, trimmed.
func searchQuery(r *http.Request) string {
	return strings.TrimSpace(r.URL.Query().Get("q"))
}

// searchAll gathers one query's hits under lim. It never fetches:
// a name the app does not know yet is noted for the worker
// (notePilotNameSearch), which only a signed-in search may do.
// The signed-in user's own characters resolve first (the pilot
// tier needs the own-id set); every other tier then gathers
// concurrently, from the in-memory index when a snapshot exists
// and from SQL when it does not. The set is assembled in the
// fixed tier order, and typing guesses in the order the tiers
// have always noted them: items, corporations, alliances, pilots.
func (app *Application) searchAll(ctx context.Context, q string, userID int64, lim searchLimits) searchSet {
	var set searchSet
	ownIDs := map[int64]bool{}
	if userID > 0 {
		if chars, err := app.queries.ListCharactersByUser(ctx, userID); err == nil {
			for _, ch := range chars {
				ownIDs[ch.CharacterID] = true
				if _, ok := nameMatchTier(ch.Name, q); ok && ch.Name != "" {
					set.Characters = append(set.Characters, searchHit{
						Kind: "character", ID: ch.CharacterID, Name: ch.Name, Label: "Your character",
					})
				}
			}
			sortHitsByName(set.Characters, q)
			set.Characters = capHits(set.Characters, lim.characters)
		} else {
			logging.Errorf("search: list characters for user %d: %v", userID, err)
		}
	}

	var (
		itemFix, systemFix, regionFix     string
		itemHits, regionHits              []searchHit
		corpHits, allianceHits, pilotHits []searchHit
		systems                           []searchSystem
		itemGuesses, corpGuesses          []typingGuess
		allianceGuesses, pilotGuesses     []typingGuess
	)
	var wg sync.WaitGroup
	wg.Add(6)
	go func() {
		defer wg.Done()
		itemHits, itemGuesses, itemFix = app.searchItemTier(ctx, q, lim.items)
	}()
	go func() {
		defer wg.Done()
		systems, systemFix = app.searchSystemTier(ctx, q, lim.systems)
	}()
	go func() {
		defer wg.Done()
		regionHits, regionFix = app.searchRegionTier(ctx, q, lim.regions)
	}()
	go func() {
		defer wg.Done()
		corpHits, corpGuesses = app.searchCorporationTier(ctx, q, lim.orgs)
	}()
	go func() {
		defer wg.Done()
		allianceHits, allianceGuesses = app.searchAllianceTier(ctx, q, lim.orgs)
	}()
	go func() {
		defer wg.Done()
		pilotHits, pilotGuesses = app.searchPilotTier(ctx, q, ownIDs, lim.pilots)
	}()
	wg.Wait()

	set.Items = itemHits
	set.Systems = systems
	for _, fix := range []string{itemFix, systemFix, regionFix} {
		if fix != "" {
			set.Fix = fix
			break
		}
	}
	set.Regions = regionHits
	set.Corporations = corpHits
	set.Alliances = allianceHits
	set.Pilots = pilotHits
	set.guesses = append(set.guesses, itemGuesses...)
	set.guesses = append(set.guesses, corpGuesses...)
	set.guesses = append(set.guesses, allianceGuesses...)
	set.guesses = append(set.guesses, pilotGuesses...)

	// No character of either kind matched: if the query reads as
	// a full pilot name, note the one-per-name resolution want and
	// say the search is under way instead of silently offering
	// nothing; a name resolved a moment ago answers as its pilot.
	if userID > 0 && len(set.Characters) == 0 && len(set.Pilots) == 0 {
		pending, hit := app.notePilotNameSearch(ctx, q, set.Items)
		set.Pending = pending
		if hit != nil && !ownIDs[hit.ID] {
			set.Pilots = append(set.Pilots, *hit)
		}
	}
	return set
}

// searchItemTier is the items tier: the shared suggestion feed,
// plus the warming guesses landing on any of these should trigger
// (a jump to a result should land on a warming chart, not a cold
// one).
func (app *Application) searchItemTier(ctx context.Context, q string, limItems int) (hits []searchHit, guesses []typingGuess, fix string) {
	items, fix := app.suggestTypesFix(ctx, q, suggestPoolAll, int64(limItems))
	for i, it := range items {
		hits = append(hits, searchHit{Kind: "item", ID: it.ID, Name: it.Name, Label: it.Label})
		if i < suggestedWarmed {
			guesses = append(guesses, typingGuess{kind: pageWantHistory, id: it.ID, region: defaultMarketRegion})
		}
	}
	return hits, guesses, fix
}

// searchSystemTier is the systems tier: the local star map. A
// system names its region and carries its security status.
func (app *Application) searchSystemTier(ctx context.Context, q string, limSystems int) ([]searchSystem, string) {
	if snap := app.searchSnapshot(); snap != nil {
		out := snap.searchSystems(q, int64(limSystems))
		if len(out) == 0 && snap.vocab != nil {
			if fix, ok := snap.vocab.correct(q); ok {
				return snap.searchSystems(fix, int64(limSystems)), fix
			}
		}
		return out, ""
	}
	var out []searchSystem
	if rows, err := app.queries.SuggestSDESystems(ctx, db.SuggestSDESystemsParams{Q: q, Lim: int64(limSystems)}); err == nil {
		for _, row := range rows {
			sec, tier := secRound(row.Security)
			out = append(out, searchSystem{
				searchHit: searchHit{Kind: "system", ID: row.SystemID, Name: row.Name, Label: "System · " + row.RegionName,
					Sec: sec, SecTier: tier},
				RegionID: row.RegionID, RegionName: row.RegionName, Security: row.Security,
			})
		}
	} else {
		logging.Errorf("search: systems for %q: %v", q, err)
	}
	return out, ""
}

func (app *Application) searchRegionTier(ctx context.Context, q string, limRegions int) ([]searchHit, string) {
	if snap := app.searchSnapshot(); snap != nil {
		out := snap.searchRegions(q, int64(limRegions))
		if len(out) == 0 && snap.vocab != nil {
			if fix, ok := snap.vocab.correct(q); ok {
				return snap.searchRegions(fix, int64(limRegions)), fix
			}
		}
		return out, ""
	}
	var out []searchHit
	if rows, err := app.queries.SuggestSDERegions(ctx, db.SuggestSDERegionsParams{Q: q, Lim: int64(limRegions)}); err == nil {
		for _, row := range rows {
			out = append(out, searchHit{Kind: "region", ID: row.RegionID, Name: row.Name, Label: "Region"})
		}
	} else {
		logging.Errorf("search: regions for %q: %v", q, err)
	}
	return out, ""
}

// searchCorporationTier is the corporations tier: the warmed
// public records, so a name the app knows links straight to its
// record page.
func (app *Application) searchCorporationTier(ctx context.Context, q string, limOrgs int) (hits []searchHit, guesses []typingGuess) {
	if indexed, ok := app.searchIndexCorporations(q, limOrgs); ok {
		hits = indexed
	} else if rows, err := app.queries.SearchCorporationRecordsByName(ctx, q); err == nil {
		for _, row := range rows {
			payload, ok := decodeCorpPayload(row.Payload)
			if !ok {
				continue
			}
			if _, ok := nameMatchTier(payload.Corp.Name, q); !ok {
				continue
			}
			label := "Corporation"
			if payload.Corp.Ticker != "" {
				label = "[" + payload.Corp.Ticker + "]"
			}
			hits = append(hits, searchHit{
				Kind: "corporation", ID: row.CorporationID, Name: payload.Corp.Name, Label: label,
			})
		}
		sortHitsByName(hits, q)
		hits = capHits(hits, limOrgs)
	} else {
		logging.Errorf("search: corporation records for %q: %v", q, err)
	}
	for _, h := range hits {
		guesses = append(guesses, typingGuess{kind: pageWantCorporation, id: h.ID})
	}
	return hits, guesses
}

func (app *Application) searchAllianceTier(ctx context.Context, q string, limOrgs int) (hits []searchHit, guesses []typingGuess) {
	if indexed, ok := app.searchIndexAlliances(q, limOrgs); ok {
		hits = indexed
	} else if rows, err := app.queries.SearchAllianceRecordsByName(ctx, q); err == nil {
		for _, row := range rows {
			payload, ok := decodeAlliancePayload(row.Payload)
			if !ok {
				continue
			}
			if _, ok := nameMatchTier(payload.Alliance.Name, q); !ok {
				continue
			}
			label := "Alliance"
			if payload.Alliance.Ticker != "" {
				label = "[" + payload.Alliance.Ticker + "]"
			}
			hits = append(hits, searchHit{
				Kind: "alliance", ID: row.AllianceID, Name: payload.Alliance.Name, Label: label,
			})
		}
		sortHitsByName(hits, q)
		hits = capHits(hits, limOrgs)
	} else {
		logging.Errorf("search: alliance records for %q: %v", q, err)
	}
	for _, h := range hits {
		guesses = append(guesses, typingGuess{kind: pageWantAlliance, id: h.ID})
	}
	return hits, guesses
}

// searchPilotTier is the pilots tier: the warmed pilot records,
// skipping the searcher's own characters (they answer in the
// characters tier).
func (app *Application) searchPilotTier(ctx context.Context, q string, ownIDs map[int64]bool, limPilots int) (hits []searchHit, guesses []typingGuess) {
	if indexed, ok := app.searchIndexPilots(q, ownIDs, limPilots); ok {
		hits = indexed
	} else if rows, err := app.queries.SearchPilotRecordsByName(ctx, q); err == nil {
		for _, row := range rows {
			if ownIDs[row.CharacterID] || row.Payload == "" {
				continue
			}
			var payload pilotPayload
			if jerr := json.Unmarshal([]byte(row.Payload), &payload); jerr != nil || payload.Profile.Name == "" {
				continue
			}
			if _, ok := nameMatchTier(payload.Profile.Name, q); !ok {
				continue
			}
			label := "Pilot"
			if payload.Corp.Name != "" {
				label = payload.Corp.Name
			}
			hits = append(hits, searchHit{
				Kind: "pilot", ID: row.CharacterID, Name: payload.Profile.Name, Label: label,
			})
		}
		sortHitsByName(hits, q)
		hits = capHits(hits, limPilots)
	} else {
		logging.Errorf("search: pilot records for %q: %v", q, err)
	}
	for _, h := range hits {
		guesses = append(guesses, typingGuess{kind: pageWantPilot, id: h.ID})
	}
	return hits, guesses
}

func capHits(hits []searchHit, n int) []searchHit {
	if len(hits) > n {
		return hits[:n]
	}
	return hits
}

// handleTopbarSearch serves GET /search.json?q=: the signed-in
// user's characters first (they're the highest-intent match),
// then items, systems, regions, corporations, alliances, and
// warmed pilot records. It also backs the Ctrl+K quick-jump
// palette, which renders the same hits grouped by kind. Every hit
// notes the page a pick would open as a typing guess, so whichever
// result the user jumps to is already warming.
func (app *Application) handleTopbarSearch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := searchQuery(r)
	hits := []searchHit{}
	if len(q) < 2 {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(hits)
		return
	}
	userID := app.userID(ctx)
	set := app.searchAll(ctx, q, userID, topbarLimits)
	hits = append(hits, set.Characters...)
	hits = append(hits, set.Items...)
	for _, s := range set.Systems {
		hits = append(hits, s.searchHit)
	}
	hits = append(hits, set.Regions...)
	hits = append(hits, set.Corporations...)
	hits = append(hits, set.Alliances...)
	hits = append(hits, set.Pilots...)
	if userID > 0 { // anonymous typing warms nothing ahead of a visit
		app.setTypingGuesses(ctx, app.typingBox(ctx, "topbar"), set.guesses, time.Now().UTC())
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if set.Pending {
		hits = append(hits, searchHit{Kind: "pilot-pending", Name: "Searching for “" + q + "”…", Label: "Pilot"})
		// The box re-asks while a name resolves; a cached pending
		// answer would hide the real suggestion when it lands.
		w.Header().Set("Cache-Control", "no-store")
	} else {
		w.Header().Set("Cache-Control", "private, max-age=30")
	}
	if r.URL.Query().Get("all") == "1" {
		// The banner box's last row: the results page, every kind
		// of match with its filters (the palette adds its own).
		hits = append(hits, searchHit{Kind: "all", Name: "All results for “" + q + "”", Label: "Search"})
	}
	_ = json.NewEncoder(w).Encode(hits)
}
