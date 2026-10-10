package app

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Current-page urgency: whatever a signed-in page is showing gets
// fetched first. Render code notes a "page want" for every entity
// it had to render unresolved; the note both enqueues the matching
// background queue at viewed priority (pilot records, type
// details, structure names, market history — handlers only write
// queue rows, they never fetch) and registers the want against
// the page so the top-banner sync indicator can count what this
// page is still waiting on. The urgent drain already runs viewed
// (priority 1) pilot wants ahead of the proactively noted orbit,
// so a page view jumps the queue without bypassing the shared
// fetch budget or the 420/429 backoff.
//
// Fragment and status reads are cache-only: they consult stored
// rows and in-process caches and never touch the network.
// ---------------------------------------------------------------------------

type pageWantKind string

const (
	pageWantCharacter       pageWantKind = "character"
	pageWantPilot           pageWantKind = "pilot"
	pageWantTypeDescription pageWantKind = "type_description"
	pageWantStructure       pageWantKind = "structure"
	pageWantHistory         pageWantKind = "market_history"
	pageWantCorporation     pageWantKind = "corporation"
	pageWantAlliance        pageWantKind = "alliance"
	pageWantPlace           pageWantKind = "place"
	// pageWantGuidePrices: kill content rendered with no price
	// guide to value it; settled once prices are held anywhere
	// (fresh from ESI, the worker's stored mirror, or a memory
	// still valid) so the pending veil lifts.
	pageWantGuidePrices pageWantKind = "guide_prices"
)

// pageWantTTL bounds how long the banner indicator waits on one
// page want. The durable queue rows outlive it (the worker keeps
// draining them); the indicator just stops claiming the page is
// still loading after a few minutes.
const pageWantTTL = 3 * time.Minute

// How close a wanted public record is to somebody: the priority of its
// queue row, and the first thing the drains order by. Among records
// equally close, the one asked for most lately goes first.
//
// Stored (pilot_records.priority and its fellows): the order is what
// matters, and the numbers do not change.
const (
	wantHop    int64 = 0 // one relationship beyond a character's own records: the corporation of somebody it traded with
	wantOrbit  int64 = 1 // named by a character's own records
	wantViewed int64 = 2 // on a page somebody has open, or being typed into a search
)

// wantPilot, wantCorporation and wantAlliance queue a public record at
// a closeness. Queue writes only: nothing is fetched here.
func (app *Application) wantPilot(ctx context.Context, id, closeness int64) error {
	return app.queries.UpsertPilotWant(ctx, db.UpsertPilotWantParams{CharacterID: id, Priority: closeness, NotedAt: timeSet(time.Now().UTC())})
}

func (app *Application) wantCorporation(ctx context.Context, id, closeness int64) error {
	return app.queries.UpsertCorporationWant(ctx, db.UpsertCorporationWantParams{CorporationID: id, Priority: closeness, NotedAt: timeSet(time.Now().UTC())})
}

func (app *Application) wantAlliance(ctx context.Context, id, closeness int64) error {
	return app.queries.UpsertAllianceWant(ctx, db.UpsertAllianceWantParams{AllianceID: id, Priority: closeness, NotedAt: timeSet(time.Now().UTC())})
}

type pageWant struct {
	Kind     pageWantKind
	ID       int64
	RegionID int64 // market history wants are per (type, region)
	NotedAt  time.Time
}

type pageWantCtxKey struct{}

func withPageScope(ctx context.Context, scope string) context.Context {
	return context.WithValue(ctx, pageWantCtxKey{}, scope)
}

// pageScope identifies "this signed-in user's view of this page"
// (user id + request URI). Empty outside a page render.
func pageScope(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	scope, _ := ctx.Value(pageWantCtxKey{}).(string)
	return scope
}

// pageScopeExemptPath reports whether a path renders fragments or
// status rather than a page: those reads must stay write-free, so
// they never carry a page scope.
func pageScopeExemptPath(p string) bool {
	return strings.HasPrefix(p, "/static/") ||
		strings.Contains(p, "fragment") ||
		strings.HasSuffix(p, "/page-status") ||
		p == "/healthz"
}

// pageWantScopeMiddleware stamps every signed-in page GET with
// its scope so the render helpers can note wants for whatever
// they fail to resolve.
func (app *Application) pageWantScopeMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && !pageScopeExemptPath(r.URL.Path) {
			if userID := app.userID(r.Context()); userID != 0 {
				scope := fmt.Sprintf("%d|%s", userID, r.URL.RequestURI())
				r = r.WithContext(withPageScope(r.Context(), scope))
			}
		}
		next.ServeHTTP(w, r)
	})
}

// notePageWant enqueues the background queue row for one
// unresolved entity a page referenced and registers it against
// the page scope (when rendering a page) for the sync indicator.
// Writes only; safe to call from any handler.
func (app *Application) notePageWant(ctx context.Context, kind pageWantKind, id int64, regionID int64) {
	if id <= 0 {
		return
	}
	switch kind {
	case pageWantCharacter, pageWantPilot:
		if err := app.wantPilot(ctx, id, wantViewed); err != nil {
			logging.Errorf("pagewant: note pilot want for %d: %v", id, err)
		}
	case pageWantTypeDescription:
		if _, settled := app.descriptionState(ctx, id); !settled {
			if err := app.queries.UpsertTypeDetailWant(ctx, id); err != nil {
				logging.Errorf("pagewant: note type detail want for %d: %v", id, err)
			}
		}
	case pageWantStructure:
		app.noteStructureIDs(ctx, id)
	case pageWantHistory:
		if regionID > 0 {
			if err := app.queries.UpsertMarketHistoryWant(ctx, db.UpsertMarketHistoryWantParams{
				RegionID: regionID, TypeID: id,
				LastRequestedAt: time.Now().UTC(),
			}); err != nil {
				logging.Errorf("pagewant: note history want for type %d in region %d: %v", id, regionID, err)
			}
		}
	case pageWantCorporation:
		if err := app.wantCorporation(ctx, id, wantViewed); err != nil {
			logging.Errorf("pagewant: note corporation want for %d: %v", id, err)
		}
	case pageWantAlliance:
		if err := app.wantAlliance(ctx, id, wantViewed); err != nil {
			logging.Errorf("pagewant: note alliance want for %d: %v", id, err)
		}
	case pageWantPlace:
		// No dedicated queue exists for place labels yet; they are
		// tracked so the indicator is honest while the background
		// warmers (pilot profiles, intel snapshots, SDE places)
		// fill the caches they read from.
	case pageWantGuidePrices:
		// The want is a single global row: the price guide is
		// shared by every viewer, so one note covers them all;
		// the urgent drain answers it with a guide refresh when
		// ESI's cache window allows.
		if err := app.queries.NoteGuidePriceWant(ctx, time.Now().UTC()); err != nil {
			logging.Errorf("pagewant: note guide price want: %v", err)
		}
	}

	scope := pageScope(ctx)
	if scope == "" {
		return
	}
	key := fmt.Sprintf("%s:%d:%d", kind, id, regionID)
	app.pageWantMu.Lock()
	defer app.pageWantMu.Unlock()
	if app.pageWants == nil {
		app.pageWants = make(map[string]map[string]pageWant)
	}
	set := app.pageWants[scope]
	if set == nil {
		set = make(map[string]pageWant)
		app.pageWants[scope] = set
	}
	if existing, ok := set[key]; ok {
		existing.NotedAt = time.Now()
		set[key] = existing
		return
	}
	set[key] = pageWant{Kind: kind, ID: id, RegionID: regionID, NotedAt: time.Now()}
}

// notePageWantFromContext is notePageWant for render helpers that
// only have a context: a no-op outside a page render (worker
// passes, fragment reads, tests).
func (app *Application) notePageWantFromContext(ctx context.Context, kind pageWantKind, id int64) {
	if pageScope(ctx) == "" {
		return
	}
	app.notePageWant(ctx, kind, id, 0)
}

// resolvedCharacterName answers a character's name from every
// local tier — in-process cache, a ready pilot record, a linked
// character row — without fetching. settled reports whether the
// local tiers have a final answer (a name, or a record that the
// character does not answer), so callers can stop waiting.
func (app *Application) resolvedCharacterName(ctx context.Context, characterID int64) (name string, settled bool) {
	if n, ok := app.esi.CachedCharacterName(characterID); ok && n != "" {
		return n, true
	}
	if rec, err := app.queries.GetPilotRecord(ctx, characterID); err == nil {
		switch rec.State {
		case pilotStateReady:
			if rec.Payload != "" {
				var payload pilotPayload
				if jerr := json.Unmarshal([]byte(rec.Payload), &payload); jerr == nil && payload.Profile.Name != "" {
					app.esi.StoreCharacterName(characterID, payload.Profile.Name)
					return payload.Profile.Name, true
				}
			}
			return "", true
		case pilotStateMissing:
			return "", true
		}
	}
	if ch, err := app.queries.GetCharacter(ctx, characterID); err == nil && ch.Name != "" {
		return ch.Name, true
	}
	if app.esi.CharacterNameMissed(characterID) {
		return "", true
	}
	return "", false
}

// displayCharacter is characterDisplay with page urgency: on a
// page, an unresolved character both falls back to the honest
// "#<id>" label and leaves a viewed-priority want behind.
func (app *Application) displayCharacter(ctx context.Context, characterID int64) string {
	if name, settled := app.resolvedCharacterName(ctx, characterID); settled && name != "" {
		return name
	} else if settled {
		return fmt.Sprintf("Character #%d", characterID)
	}
	app.notePageWantFromContext(ctx, pageWantCharacter, characterID)
	return fmt.Sprintf("Character #%d", characterID)
}

// characterLabelPending reports whether a character label is
// still waiting on its first local answer (used to render the
// live region instead of the bare fallback).
func (app *Application) characterLabelPending(ctx context.Context, characterID int64) bool {
	_, settled := app.resolvedCharacterName(ctx, characterID)
	return !settled
}

// pageWantSettled reports whether one tracked want has its answer
// in the local tiers now (or a settled "there is none").
func (app *Application) pageWantSettled(ctx context.Context, want pageWant) bool {
	switch want.Kind {
	case pageWantCharacter, pageWantPilot:
		_, settled := app.resolvedCharacterName(ctx, want.ID)
		return settled
	case pageWantTypeDescription:
		_, settled := app.descriptionState(ctx, want.ID)
		return settled
	case pageWantStructure:
		if name := app.resolvedStructureTitle(ctx, want.ID); name != "" {
			return true
		}
		row, err := app.queries.GetStructureName(ctx, want.ID)
		return err == nil && row.State == esi.StructureMissing
	case pageWantHistory:
		return len(app.recentHistoryRows(ctx, want.RegionID, want.ID, 1)) > 0 ||
			app.historyFetchSettled(ctx, want.RegionID, want.ID)
	case pageWantCorporation:
		if name, ok := app.esi.CachedCorpName(want.ID); ok && name != "" {
			return true
		}
		rec, err := app.queries.GetCorporationRecord(ctx, want.ID)
		return err == nil && rec.State != orgStatePending
	case pageWantAlliance:
		if name, ok := app.esi.CachedAllianceName(want.ID); ok && name != "" {
			return true
		}
		rec, err := app.queries.GetAllianceRecord(ctx, want.ID)
		return err == nil && rec.State != orgStatePending
	case pageWantPlace:
		_, ok := app.esi.CachedPlaceName(ctx, want.ID)
		return ok
	case pageWantGuidePrices:
		return app.valuationPrices(ctx) != nil
	}
	return true
}

// pendingPageWantCount counts one page scope's unresolved wants,
// forgetting settled and expired ones as it goes.
func (app *Application) pendingPageWantCount(ctx context.Context, scope string) int {
	app.pageWantMu.Lock()
	set := app.pageWants[scope]
	keys := make([]string, 0, len(set))
	wants := make([]pageWant, 0, len(set))
	for key, want := range set {
		keys = append(keys, key)
		wants = append(wants, want)
	}
	app.pageWantMu.Unlock()

	pending := 0
	now := time.Now()
	for i, want := range wants {
		if now.Sub(want.NotedAt) > pageWantTTL || app.pageWantSettled(ctx, want) {
			app.pageWantMu.Lock()
			if set := app.pageWants[scope]; set != nil {
				delete(set, keys[i])
				if len(set) == 0 {
					delete(app.pageWants, scope)
				}
			}
			app.pageWantMu.Unlock()
			continue
		}
		pending++
	}
	return pending
}

// handlePageSyncStatus is the banner indicator's poll: how much
// of the page the user is looking at is still on its way. Purely
// local reads (the in-memory tracker plus stored rows); it never
// enqueues and never fetches.
func (app *Application) handlePageSyncStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	page := r.URL.Query().Get("page")
	pending := 0
	if userID != 0 && page != "" {
		pending = app.pendingPageWantCount(ctx, fmt.Sprintf("%d|%s", userID, page))
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"pending": pending,
		"loading": pending > 0,
	})
}

// viewerCharSet builds the names-are-links viewer set for
// fragment handlers from the session's linked characters.
func (app *Application) viewerCharSet(ctx context.Context) map[int64]bool {
	set := make(map[int64]bool)
	for _, e := range app.switcherEntries(ctx) {
		set[e.ID] = true
	}
	return set
}

// labelFragment answers a pending name label's poll: the linked name
// once it is known, the plain fallback once it is known there is none
// (so the polling stops), and the loading line until then. Cache-only:
// the page that rendered the label already left the want.
func labelFragment(w http.ResponseWriter, r *http.Request, fallback func(id int64) string,
	resolve func(id int64) (name string, settled bool), link func(id int64, name string) template.HTML) {
	id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "bad label fragment request", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	name, settled := resolve(id)
	switch {
	case settled && name != "":
		fmt.Fprintf(w, `<span data-poll-state="ready">%s</span>`, link(id, name))
	case settled:
		fmt.Fprintf(w, `<span data-poll-state="ready">%s</span>`, fallback(id))
	default:
		fmt.Fprintf(w, `<span data-poll-state="pending"><span class="loading-pulse" aria-hidden="true"></span> Loading name for %s…</span>`, fallback(id))
	}
}

// numbered is the fallback label for an id with no name: "Character #123".
func numbered(kind string) func(int64) string {
	return func(id int64) string { return fmt.Sprintf("%s #%d", kind, id) }
}

func (app *Application) handleCharacterLabelFragment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	labelFragment(w, r, numbered("Character"),
		func(id int64) (string, bool) { return app.resolvedCharacterName(ctx, id) },
		func(id int64, name string) template.HTML { return charLink(app.viewerCharSet(ctx), id, name) })
}

func (app *Application) handleCorporationLabelFragment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	labelFragment(w, r, numbered("Corporation"),
		func(id int64) (string, bool) { return app.resolvedCorpName(ctx, id) }, corpLink)
}

func (app *Application) handleAllianceLabelFragment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	labelFragment(w, r, numbered("Alliance"),
		func(id int64) (string, bool) { return app.resolvedAllianceName(ctx, id) }, allianceLink)
}

// placeFallbackLabel is the unresolved label for a place id:
// structures and stations read differently even unresolved.
func placeFallbackLabel(id int64) string {
	if isStructureID(id) {
		return fmt.Sprintf("Structure #%d", id)
	}
	return fmt.Sprintf("Station #%d", id)
}

func (app *Application) handlePlaceLabelFragment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	labelFragment(w, r, placeFallbackLabel,
		func(id int64) (string, bool) { return app.resolvedPlaceName(ctx, id) },
		func(id int64, name string) template.HTML { return placeLink(app.linkPlace(ctx, id, name)) })
}
