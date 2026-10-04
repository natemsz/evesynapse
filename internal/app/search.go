package app

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"

	db "evesynapse/internal/db/sqlc"
)

// ---------------------------------------------------------------------------
// Search & findability (Next-1): one local-SDE suggestion feed
// behind every autocomplete box in the app, plus the top banner's
// global search. Everything here reads local tables only — no
// ESI, no token — so handlers keep the zero-outbound-call rule.
//
// Suggestion pools pick which slice of the type table a box
// searches:
//   market  — tradeable types only (the Market page's original
//             behaviour: a market group and published)
//   planner — published types a blueprint builds
//   skills  — skill types (the skill-plan add box)
//   all     — every type in the database (Items DB, top banner)
// ---------------------------------------------------------------------------

const (
	suggestPoolMarket  = "market"
	suggestPoolPlanner = "planner"
	suggestPoolSkills  = "skills"
	suggestPoolAll     = "all"
)

// suggestDefaultLimit caps one suggestion response; boxes ask
// for a dozen or fewer so a dropdown stays thumb-sized.
const suggestDefaultLimit = 12

// suggestItem is one type suggestion: the type plus the
// category/group label the dropdown shows beside the name.
type suggestItem struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Label string `json:"label,omitempty"`
}

// suggestTypes runs the shared feed: prefix matches first, then
// other substring matches, alphabetical inside each tier. Short
// or blank queries return an empty (never nil) slice so JSON
// responses encode as [].
func (app *Application) suggestTypes(ctx context.Context, q, pool string, limit int64) []suggestItem {
	out := []suggestItem{}
	q = strings.TrimSpace(q)
	if len(q) < 2 {
		return out
	}
	switch pool {
	case suggestPoolMarket, suggestPoolPlanner, suggestPoolSkills, suggestPoolAll:
	default:
		pool = suggestPoolAll
	}
	if limit < 1 || limit > 25 {
		limit = suggestDefaultLimit
	}
	rows, err := app.queries.SuggestSDETypesShared(ctx, db.SuggestSDETypesSharedParams{
		Q: q, Pool: pool, Lim: limit,
	})
	if err != nil {
		log.Printf("search: suggest %q (pool %s): %v", q, pool, err)
		return out
	}
	for _, row := range rows {
		out = append(out, suggestItem{ID: row.TypeID, Name: row.Name, Label: suggestLabel(row.CategoryName, row.GroupName)})
	}
	return out
}

// suggestLabel composes the "Category · Group" context shown
// beside a suggestion (either part may be missing).
func suggestLabel(category, group string) string {
	switch {
	case category != "" && group != "" && category != group:
		return category + " · " + group
	case group != "":
		return group
	default:
		return category
	}
}

func writeSuggestJSON(w http.ResponseWriter, rows []suggestItem) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "private, max-age=30")
	if rows == nil {
		rows = []suggestItem{}
	}
	_ = json.NewEncoder(w).Encode(rows)
}

// handleItemSearchJSON serves GET /items/search.json?q=&pool=:
// the shared feed for the Items DB search box and, by pool, the
// planner and skill-plan pickers.
func (app *Application) handleItemSearchJSON(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := int64(suggestDefaultLimit)
	if raw := q.Get("limit"); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			limit = n
		}
	}
	writeSuggestJSON(w, app.suggestTypes(r.Context(), q.Get("q"), q.Get("pool"), limit))
}

// handleMarketSuggest serves the Market search box's live
// suggestions. It predates the shared feed and keeps its exact
// behaviour (market pool, ten rows, no label) on the shared core.
func (app *Application) handleMarketSuggest(w http.ResponseWriter, r *http.Request) {
	writeSuggestJSON(w, app.suggestTypes(r.Context(), r.URL.Query().Get("q"), suggestPoolMarket, 10))
}

// ---------------------------------------------------------------------------
// Top banner global search: one box over items, the signed-in
// user's own characters, and strangers whose public records have
// already been warmed. It only ever surfaces names the local
// data already knows — nothing is fetched, nothing is queued;
// unwarmed strangers simply aren't in the list yet.
// ---------------------------------------------------------------------------

// searchHit is one top-banner result. Kind drives the link:
// item → its details page, character → the character sheet,
// pilot → the public pilot page.
type searchHit struct {
	Kind  string `json:"kind"` // "item" | "character" | "pilot"
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Label string `json:"label,omitempty"`
}

func nameMatchTier(name, q string) (tier int, ok bool) {
	lowerName, lowerQ := strings.ToLower(name), strings.ToLower(q)
	switch {
	case strings.HasPrefix(lowerName, lowerQ):
		return 0, true
	case strings.Contains(lowerName, lowerQ):
		return 1, true
	}
	return 0, false
}

func sortHitsByName(hits []searchHit, q string) {
	sort.SliceStable(hits, func(i, j int) bool {
		ti, _ := nameMatchTier(hits[i].Name, q)
		tj, _ := nameMatchTier(hits[j].Name, q)
		if ti != tj {
			return ti < tj
		}
		return strings.ToLower(hits[i].Name) < strings.ToLower(hits[j].Name)
	})
}

// handleTopbarSearch serves GET /search.json?q=: the signed-in
// user's characters first (they're the highest-intent match),
// then items, then warmed pilot records.
func (app *Application) handleTopbarSearch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	hits := []searchHit{}
	if len(q) < 2 {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(hits)
		return
	}
	userID := int64(app.sessions.GetInt(ctx, sessionUserID))

	ownIDs := map[int64]bool{}
	if chars, err := app.queries.ListCharactersByUser(ctx, userID); err == nil {
		charHits := []searchHit{}
		for _, ch := range chars {
			ownIDs[ch.CharacterID] = true
			if _, ok := nameMatchTier(ch.Name, q); ok && ch.Name != "" {
				charHits = append(charHits, searchHit{
					Kind: "character", ID: ch.CharacterID, Name: ch.Name, Label: "Your character",
				})
			}
		}
		sortHitsByName(charHits, q)
		if len(charHits) > 4 {
			charHits = charHits[:4]
		}
		hits = append(hits, charHits...)
	} else {
		log.Printf("search: list characters for user %d: %v", userID, err)
	}

	for _, it := range app.suggestTypes(ctx, q, suggestPoolAll, 6) {
		hits = append(hits, searchHit{Kind: "item", ID: it.ID, Name: it.Name, Label: it.Label})
	}

	if rows, err := app.queries.SearchPilotRecordsByName(ctx, q); err == nil {
		pilotHits := []searchHit{}
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
			pilotHits = append(pilotHits, searchHit{
				Kind: "pilot", ID: row.CharacterID, Name: payload.Profile.Name, Label: label,
			})
		}
		sortHitsByName(pilotHits, q)
		if len(pilotHits) > 4 {
			pilotHits = pilotHits[:4]
		}
		hits = append(hits, pilotHits...)
	} else {
		log.Printf("search: pilot records for %q: %v", q, err)
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "private, max-age=30")
	_ = json.NewEncoder(w).Encode(hits)
}
