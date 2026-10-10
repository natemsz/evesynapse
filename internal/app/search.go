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
// through those. On top of the floor:
//   market  — the floor alone (the Market page's behaviour)
//   planner — types a blueprint builds
//   skills  — skill types (the skill-plan add box)
//   all     — the floor alone, broadly labelled (Items DB,
//             top banner)
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
	items := app.suggestTypes(r.Context(), q.Get("q"), q.Get("pool"), limit)
	app.noteSuggested(r.Context(), app.typingBox(r.Context(), "items:"+q.Get("pool")), q.Get("pool"), items)
	writeSuggestJSON(w, items)
}

// handleMarketSuggest serves the Market search box's live
// suggestions. It predates the shared feed and keeps its exact
// behaviour (market pool, ten rows, no label) on the shared core.
func (app *Application) handleMarketSuggest(w http.ResponseWriter, r *http.Request) {
	items := app.suggestTypes(r.Context(), r.URL.Query().Get("q"), suggestPoolMarket, 10)
	app.noteSuggested(r.Context(), app.typingBox(r.Context(), "market"), suggestPoolMarket, items)
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
	app.replaceTypingGuesses(ctx, box)
	now := time.Now().UTC()
	for _, it := range items {
		if it.ID <= 0 {
			continue
		}
		switch pool {
		case suggestPoolMarket:
			app.noteTypingHistoryGuess(ctx, box, defaultMarketRegion, it.ID, now)
		default:
			app.noteTypingDetailGuess(ctx, box, it.ID, now)
		}
	}
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
	Kind  string `json:"kind"` // "item" | "character" | "pilot" | "corporation" | "alliance" | "pilot-pending"
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
// then items, corporations, alliances, and warmed pilot
// records. It also backs the Ctrl+K quick-jump palette, which
// renders the same hits grouped by kind. Every item hit notes
// the market-history prefetch wants the market page's search
// notes, so whichever result the user jumps to already has its
// chart warming.
func (app *Application) handleTopbarSearch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	hits := []searchHit{}
	if len(q) < 2 {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(hits)
		return
	}
	userID := app.userID(ctx)

	ownIDs := map[int64]bool{}
	ownHitCount := 0
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
		ownHitCount = len(charHits)
		hits = append(hits, charHits...)
	} else {
		logging.Errorf("search: list characters for user %d: %v", userID, err)
	}

	itemHits := []searchHit{}
	itemMatches := []marketMatch{}
	for _, it := range app.suggestTypes(ctx, q, suggestPoolAll, 6) {
		hit := searchHit{Kind: "item", ID: it.ID, Name: it.Name, Label: it.Label}
		itemHits = append(itemHits, hit)
		hits = append(hits, hit)
		itemMatches = append(itemMatches, marketMatch{ID: it.ID, Name: it.Name})
	}
	// Guess, same as the search boxes: a jump to any of these
	// items should land on a warming chart, not a cold one. Each
	// keystroke replaces the box's previous guesses.
	box := app.typingBox(ctx, "topbar")
	app.replaceTypingGuesses(ctx, box)
	now := time.Now().UTC()
	for i, m := range itemMatches {
		if i >= suggestedWarmed {
			break
		}
		app.noteTypingHistoryGuess(ctx, box, defaultMarketRegion, m.ID, now)
	}

	// Corporations and alliances whose public records have
	// already been warmed answer here too, so a name the app
	// knows links straight to its record page.
	if rows, err := app.queries.SearchCorporationRecordsByName(ctx, q); err == nil {
		corpHits := []searchHit{}
		for _, row := range rows {
			var payload corporationRecordPayload
			if jerr := json.Unmarshal([]byte(row.Payload), &payload); jerr != nil || payload.Corp.Name == "" {
				continue
			}
			if _, ok := nameMatchTier(payload.Corp.Name, q); !ok {
				continue
			}
			label := "Corporation"
			if payload.Corp.Ticker != "" {
				label = "[" + payload.Corp.Ticker + "]"
			}
			corpHits = append(corpHits, searchHit{
				Kind: "corporation", ID: row.CorporationID, Name: payload.Corp.Name, Label: label,
			})
		}
		sortHitsByName(corpHits, q)
		if len(corpHits) > 4 {
			corpHits = corpHits[:4]
		}
		hits = append(hits, corpHits...)
		for _, h := range corpHits {
			app.noteTypingCorporationGuess(ctx, box, h.ID, now)
		}
	} else {
		logging.Errorf("search: corporation records for %q: %v", q, err)
	}

	if rows, err := app.queries.SearchAllianceRecordsByName(ctx, q); err == nil {
		allianceHits := []searchHit{}
		for _, row := range rows {
			var payload allianceRecordPayload
			if jerr := json.Unmarshal([]byte(row.Payload), &payload); jerr != nil || payload.Alliance.Name == "" {
				continue
			}
			if _, ok := nameMatchTier(payload.Alliance.Name, q); !ok {
				continue
			}
			label := "Alliance"
			if payload.Alliance.Ticker != "" {
				label = "[" + payload.Alliance.Ticker + "]"
			}
			allianceHits = append(allianceHits, searchHit{
				Kind: "alliance", ID: row.AllianceID, Name: payload.Alliance.Name, Label: label,
			})
		}
		sortHitsByName(allianceHits, q)
		if len(allianceHits) > 4 {
			allianceHits = allianceHits[:4]
		}
		hits = append(hits, allianceHits...)
		for _, h := range allianceHits {
			app.noteTypingAllianceGuess(ctx, box, h.ID, now)
		}
	} else {
		logging.Errorf("search: alliance records for %q: %v", q, err)
	}

	pilotHitCount := 0
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
		pilotHitCount = len(pilotHits)
		hits = append(hits, pilotHits...)
		for _, h := range pilotHits {
			app.noteTypingPilotGuess(ctx, box, h.ID, now)
		}
	} else {
		logging.Errorf("search: pilot records for %q: %v", q, err)
	}

	// No local character of either kind matched: if the query
	// reads as a full pilot name, note the one-per-name
	// resolution want and say the search is under way instead of
	// silently offering nothing.
	pendingNameSearch := false
	if ownHitCount == 0 && pilotHitCount == 0 && app.notePilotNameSearch(ctx, q, itemHits) {
		pendingNameSearch = true
		hits = append(hits, searchHit{
			Kind:  "pilot-pending",
			Name:  "Searching for “" + q + "”…",
			Label: "Pilot",
		})
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if pendingNameSearch {
		// The box re-asks while a name warms; a cached pending
		// answer would hide the real suggestion when it lands.
		w.Header().Set("Cache-Control", "no-store")
	} else {
		w.Header().Set("Cache-Control", "private, max-age=30")
	}
	_ = json.NewEncoder(w).Encode(hits)
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
// want for one topbar query and reports whether the answer
// should carry the "searching" row: a want under way, or one
// resolved to a character whose record is still warming. A name
// ESI has settled as unknown stays quiet. Writes only.
func (app *Application) notePilotNameSearch(ctx context.Context, q string, itemHits []searchHit) bool {
	if !plausiblePilotName(q) {
		return false
	}
	// An exact item match means the user is almost certainly
	// after the item; don't spend a name lookup on it.
	for _, hit := range itemHits {
		if strings.EqualFold(hit.Name, q) {
			return false
		}
	}
	normalized := normalizePilotName(q)
	want, err := app.queries.GetPilotNameWant(ctx, normalized)
	if err == nil {
		switch want.State {
		case "missing":
			return false
		case "ready":
			if want.CharacterID <= 0 {
				return true
			}
			rec, rerr := app.queries.GetPilotRecord(ctx, want.CharacterID)
			if rerr == nil && (rec.State == pilotStateMissing || rec.State == pilotStateReady) {
				return false
			}
			return true
		default: // pending, or an error waiting out its backoff
			return true
		}
	}
	if !errors.Is(err, sql.ErrNoRows) {
		logging.Errorf("search: read pilot name want %q: %v", normalized, err)
		return false
	}
	display := strings.Join(strings.Fields(q), " ")
	if qerr := app.queries.UpsertPilotNameWant(ctx, db.UpsertPilotNameWantParams{
		NormalizedName: normalized,
		DisplayName:    display,
		RequestedAt:    time.Now().UTC(),
	}); qerr != nil {
		logging.Errorf("search: note pilot name want %q: %v", normalized, qerr)
		return false
	}
	return true
}
