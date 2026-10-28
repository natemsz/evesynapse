package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Search & findability (Next-1): one local-SDE suggestion feed
// behind every autocomplete box in the app, plus the top banner's
// global search. Everything here reads local tables only — no
// ESI, no token — so handlers keep the zero-outbound-call rule.
//
// Suggestion pools pick which slice of the type table a box
// searches. Every pool shares one floor: published
// types with a market group — things a player can actually
// obtain on the market or through contracts. Unpublished and
// untradeable database rows never surface in a search box; the
// Items DB explorer keeps its own explicit toggle for digging
// through those. The broad pool widens the floor in one place:
// blueprint-category types (SDE category 9 — blueprints and
// reaction formulas) carry no market group because they change
// hands through contracts alone, so 'all' admits their
// published rows too. On top of the floor:
//   market  — the floor alone (the Market page's behaviour)
//   planner — types a blueprint builds
//   skills  — skill types (the skill-plan add box)
//   all     — the floor plus contract-only blueprint types,
//             broadly labelled (Items DB, top banner)
// ---------------------------------------------------------------------------

const (
	suggestPoolMarket  = "market"
	suggestPoolPlanner = "planner"
	suggestPoolSkills  = "skills"
	suggestPoolAll     = "all"
)

// normalizeSuggestPool folds an unknown pool to the full feed, so
// one queue path serves every pool nobody defined. The guess
// ledger keys on the box, so this is also what keeps a junk pool
// value from minting orphan boxes whose guesses no keystroke ever
// replaces.
func normalizeSuggestPool(pool string) string {
	switch pool {
	case suggestPoolMarket, suggestPoolPlanner, suggestPoolSkills, suggestPoolAll:
		return pool
	default:
		return suggestPoolAll
	}
}

// suggestDefaultLimit caps one suggestion response; boxes ask
// for a dozen or fewer so a dropdown stays thumb-sized.
const suggestDefaultLimit = 12

// suggestItem is one type suggestion: the type plus the
// category/group label the dropdown shows beside the name.
// Kind is the fitting family (ship/high/medium/low/rig/
// subsystem/drone) so the fitting UI can group and route picks.
type suggestItem struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Label string `json:"label,omitempty"`
	Kind  string `json:"kind,omitempty"`
	// URL is where a pick goes, for a box whose suggestions are links.
	URL string `json:"url,omitempty"`
	// Value is what a pick stands for, where an id alone does not say.
	Value string `json:"value,omitempty"`
	// Missing marks an item the chosen pilot lacks the skills to use.
	Missing bool `json:"missing,omitempty"`
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
	pool = normalizeSuggestPool(pool)
	if limit < 1 || limit > 25 {
		limit = suggestDefaultLimit
	}
	rows, err := app.queries.SuggestSDETypesShared(ctx, db.SuggestSDETypesSharedParams{
		Q: q, Pool: pool, Lim: limit,
	})
	if err != nil {
		logging.Errorf("search: suggest %q (pool %s): %v", q, pool, err)
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
	pool := normalizeSuggestPool(q.Get("pool"))
	items := app.suggestTypes(r.Context(), q.Get("q"), pool, limit)
	if app.userID(r.Context()) > 0 { // anonymous suggestions warm nothing
		app.noteSuggested(r.Context(), app.typingBox(r.Context(), "items:"+pool), pool, items)
	}
	writeSuggestJSON(w, items)
}

// handleMarketSuggest serves the Market search box's live
// suggestions. It predates the shared feed and keeps its exact
// behaviour (market pool, ten rows, no label) on the shared core.
func (app *Application) handleMarketSuggest(w http.ResponseWriter, r *http.Request) {
	items := app.suggestTypes(r.Context(), r.URL.Query().Get("q"), suggestPoolMarket, 10)
	if app.userID(r.Context()) > 0 { // anonymous suggestions warm nothing
		app.noteSuggested(r.Context(), app.typingBox(r.Context(), "market"), suggestPoolMarket, items)
	}
	writeSuggestJSON(w, items)
}

// suggestedWarmed is how many of a box's suggestions are queued while
// it is being typed into: the ones at the top, which is where the pick
// usually is.
const suggestedWarmed = 4

// noteSuggested queues what the top suggestions' pages will want, so
// that the one picked is often warm before the pick: a market
// suggestion's price history in The Forge, an item suggestion's
// details. Each keystroke replaces the box's previous guesses, so
// guesses never accumulate across keystrokes. Queue writes only;
// the worker does the fetching.
func (app *Application) noteSuggested(ctx context.Context, box, pool string, items []suggestItem) {
	if len(items) > suggestedWarmed {
		items = items[:suggestedWarmed]
	}
	guesses := make([]typingGuess, 0, len(items))
	for _, it := range items {
		switch pool {
		case suggestPoolMarket:
			guesses = append(guesses, typingGuess{kind: pageWantHistory, id: it.ID, region: defaultMarketRegion})
		default:
			guesses = append(guesses, typingGuess{kind: pageWantTypeDescription, id: it.ID})
		}
	}
	app.setTypingGuesses(ctx, box, guesses, time.Now().UTC())
}

// ---------------------------------------------------------------------------
// Top banner global search: one box over items, the signed-in
// user's own characters, and strangers whose public records have
// already been warmed. When a full pilot name isn't in the local
// data yet, the search notes a name-resolution want (a queue
// write only — the request path still makes no outbound calls)
// and answers with a "searching" row; the worker resolves the
// name and warms the pilot record, and a later search turns the
// row into the real pilot suggestion.
// ---------------------------------------------------------------------------

// searchHit is one top-banner result. Kind drives the link:
// item → its details page, character → the character sheet,
// pilot → the public pilot page, corporation → the corporation
// page, alliance → the alliance page. A "pilot-pending" hit is
// the not-yet-resolved name search: it carries no link and is
// never offered as a pick.
type searchHit struct {
	Kind  string `json:"kind"` // "item" | "character" | "pilot" | "corporation" | "alliance" | "system" | "region" | "pilot-pending" | "all"
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Label string `json:"label,omitempty"`
	// Sec and SecTier are a system's security status as EVE shows it
	// and its color step (0 null to 10 for 1.0); systems only.
	Sec     string `json:"sec,omitempty"`
	SecTier int    `json:"secTier,omitempty"`
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

// normalizePilotName folds a typed pilot name to the key its
// resolution want is stored under: case-insensitive, with runs
// of whitespace collapsed, so "Unwarmed  Stranger" and
// "unwarmed stranger" are one want, not two.
func normalizePilotName(name string) string {
	return strings.ToLower(strings.Join(strings.Fields(name), " "))
}

// plausiblePilotName reports whether a search string could be a
// complete character name worth an exact-name lookup: 3–37
// characters of the letters, digits, spaces, and punctuation
// EVE names allow. Partial words pass this test too; each one
// is a distinct want that settles as 'missing' once, so typing
// never re-asks about the same string twice.
func plausiblePilotName(name string) bool {
	runes := []rune(strings.Join(strings.Fields(name), " "))
	if len(runes) < 3 || len(runes) > 37 {
		return false
	}
	hasLetter := false
	for _, r := range runes {
		switch {
		case unicode.IsLetter(r):
			hasLetter = true
		case unicode.IsDigit(r) || r == ' ' || r == '-' || r == '\'' || r == '.':
		default:
			return false
		}
	}
	return hasLetter
}

// notePilotNameSearch records (or consults) the name-resolution
// want for one query. A want under way answers with the
// "searching" row (pending) and nudges the name lookup awake, so
// the name resolves within a moment rather than on the next pass;
// a name already resolved to a character whose record is still
// warming answers with that pilot at once (hit), the record
// filling in behind it. A name ESI has settled as unknown stays
// quiet. The handler itself never fetches: the lookup is the
// worker's (urgentNameLookup).
func (app *Application) notePilotNameSearch(ctx context.Context, q string, itemHits []searchHit) (pending bool, hit *searchHit) {
	if !plausiblePilotName(q) {
		return false, nil
	}
	// An exact item match means the user is almost certainly
	// after the item; don't spend a name lookup on it.
	for _, hit := range itemHits {
		if strings.EqualFold(hit.Name, q) {
			return false, nil
		}
	}
	normalized := normalizePilotName(q)
	want, err := app.queries.GetPilotNameWant(ctx, normalized)
	if err == nil {
		switch want.State {
		case "missing":
			return false, nil
		case "ready":
			if want.CharacterID <= 0 {
				return true, nil
			}
			rec, rerr := app.queries.GetPilotRecord(ctx, want.CharacterID)
			if rerr == nil && (rec.State == pilotStateMissing || rec.State == pilotStateReady) {
				return false, nil
			}
			name, ok := app.esi.CachedCharacterName(want.CharacterID)
			if !ok {
				name = want.DisplayName
			}
			return false, &searchHit{Kind: "pilot", ID: want.CharacterID, Name: name, Label: "Pilot"}
		default: // pending, or an error waiting out its backoff
			app.nudgeNameLookup()
			return true, nil
		}
	}
	if !errors.Is(err, sql.ErrNoRows) {
		logging.Errorf("search: read pilot name want %q: %v", normalized, err)
		return false, nil
	}
	display := strings.Join(strings.Fields(q), " ")
	if qerr := app.queries.UpsertPilotNameWant(ctx, db.UpsertPilotNameWantParams{
		NormalizedName: normalized,
		DisplayName:    display,
		RequestedAt:    time.Now().UTC(),
	}); qerr != nil {
		logging.Errorf("search: note pilot name want %q: %v", normalized, qerr)
		return false, nil
	}
	app.nudgeNameLookup()
	return true, nil
}
