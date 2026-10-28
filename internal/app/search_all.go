package app

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
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

	// Items. Guess, same as the search boxes: a jump to any of
	// these should land on a warming chart, not a cold one.
	for i, it := range app.suggestTypes(ctx, q, suggestPoolAll, int64(lim.items)) {
		set.Items = append(set.Items, searchHit{Kind: "item", ID: it.ID, Name: it.Name, Label: it.Label})
		if i < suggestedWarmed {
			set.guesses = append(set.guesses, typingGuess{kind: pageWantHistory, id: it.ID, region: defaultMarketRegion})
		}
	}

	// Systems and regions, from the local star map. A system names
	// its region and carries its security status.
	if rows, err := app.queries.SuggestSDESystems(ctx, db.SuggestSDESystemsParams{Q: q, Lim: int64(lim.systems)}); err == nil {
		for _, row := range rows {
			sec, tier := secRound(row.Security)
			set.Systems = append(set.Systems, searchSystem{
				searchHit: searchHit{Kind: "system", ID: row.SystemID, Name: row.Name, Label: "System · " + row.RegionName,
					Sec: sec, SecTier: tier},
				RegionID: row.RegionID, RegionName: row.RegionName, Security: row.Security,
			})
		}
	} else {
		logging.Errorf("search: systems for %q: %v", q, err)
	}
	if rows, err := app.queries.SuggestSDERegions(ctx, db.SuggestSDERegionsParams{Q: q, Lim: int64(lim.regions)}); err == nil {
		for _, row := range rows {
			set.Regions = append(set.Regions, searchHit{Kind: "region", ID: row.RegionID, Name: row.Name, Label: "Region"})
		}
	} else {
		logging.Errorf("search: regions for %q: %v", q, err)
	}

	// Corporations and alliances whose public records have
	// already been warmed answer here too, so a name the app
	// knows links straight to its record page.
	if rows, err := app.queries.SearchCorporationRecordsByName(ctx, q); err == nil {
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
			set.Corporations = append(set.Corporations, searchHit{
				Kind: "corporation", ID: row.CorporationID, Name: payload.Corp.Name, Label: label,
			})
		}
		sortHitsByName(set.Corporations, q)
		set.Corporations = capHits(set.Corporations, lim.orgs)
		for _, h := range set.Corporations {
			set.guesses = append(set.guesses, typingGuess{kind: pageWantCorporation, id: h.ID})
		}
	} else {
		logging.Errorf("search: corporation records for %q: %v", q, err)
	}
	if rows, err := app.queries.SearchAllianceRecordsByName(ctx, q); err == nil {
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
			set.Alliances = append(set.Alliances, searchHit{
				Kind: "alliance", ID: row.AllianceID, Name: payload.Alliance.Name, Label: label,
			})
		}
		sortHitsByName(set.Alliances, q)
		set.Alliances = capHits(set.Alliances, lim.orgs)
		for _, h := range set.Alliances {
			set.guesses = append(set.guesses, typingGuess{kind: pageWantAlliance, id: h.ID})
		}
	} else {
		logging.Errorf("search: alliance records for %q: %v", q, err)
	}

	if rows, err := app.queries.SearchPilotRecordsByName(ctx, q); err == nil {
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
			set.Pilots = append(set.Pilots, searchHit{
				Kind: "pilot", ID: row.CharacterID, Name: payload.Profile.Name, Label: label,
			})
		}
		sortHitsByName(set.Pilots, q)
		set.Pilots = capHits(set.Pilots, lim.pilots)
		for _, h := range set.Pilots {
			set.guesses = append(set.guesses, typingGuess{kind: pageWantPilot, id: h.ID})
		}
	} else {
		logging.Errorf("search: pilot records for %q: %v", q, err)
	}

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
