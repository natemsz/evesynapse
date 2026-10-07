package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Public corporation & alliance pages (/corporation/, /alliance/):
// the destinations every resolved corporation and alliance name
// now links to. Everything on them is public ESI data, so they
// follow the public pilot page's posture exactly: the queue
// tables (schema 026) hold one assembled payload per organization,
// viewing one without a record notes a 'pending' row at viewed
// priority, and the worker drains those from ESI's public
// endpoints inside the cycle budget. Renders read stored rows
// only — they never fetch — and a first visit shows the loading
// state that live-fills through the same never-give-up poller as
// the pilot page. Ready records go stale after a week and refresh
// in the background; an ESI 404 settles as 'missing'.
// ---------------------------------------------------------------------------

// Organization record states (corporation_records / alliance_records).
const (
	orgStatePending = "pending"
	orgStateReady   = "ready"
	orgStateMissing = "missing"
)

// Drain bounds: a handful of organizations per cycle is plenty —
// these are lookups a human just asked for, not fleet sync.
const (
	orgStaleAfter                  = 7 * 24 * time.Hour
	maxOrgDrainsPerCycle           = 5
	maxAllianceMemberNamesPerDrain = 12 // mirrors the pilot history cap
)

// corporationRecordPayload is the stored public record behind
// /corporation/: the corporation profile, with the alliance's
// name/ticker baked in when the corporation belongs to one, so
// renders never chase names.
type corporationRecordPayload struct {
	Corp     esi.Corporation `json:"corp"`
	Alliance esi.Alliance    `json:"alliance"`
}

// allianceMemberCorp is one member corporation of an alliance;
// Name is baked when the drain could resolve it, "" until then.
type allianceMemberCorp struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// allianceRecordPayload is the stored public record behind
// /alliance/: the alliance profile plus its member corporations.
type allianceRecordPayload struct {
	Alliance     esi.Alliance         `json:"alliance"`
	Corporations []allianceMemberCorp `json:"corporations"`
}

// corporationPageView is the /corporation/ page body.
type corporationPageView struct {
	CorporationID   int64
	State           string // "loading" | "missing" | "ready"
	LogoURL         string
	Name            string
	Ticker          string
	MemberCount     string
	Founded         string
	TaxRate         string
	CEOID           int64
	CEOName         string
	CEOPending      bool // CEO name still on its way; the cell polls for it
	AllianceID      int64
	AllianceLine    string // name [ticker], "" when not in an alliance
	AlliancePending bool   // alliance name still on its way; the cell polls for it
	HomeStationID   int64
	HomeStation     string
	HomePending     bool // home-station name still on its way; the cell polls for it
	HasDescription  bool
	Description     template.HTML // sanitized, same rules as mail bodies
	ViewerChars     map[int64]bool
}

// allianceMemberView is one member-corporation line.
type allianceMemberView struct {
	CorpID      int64
	Name        string
	NamePending bool // corporation name still on its way; the cell polls for it
}

// alliancePageView is the /alliance/ page body.
type alliancePageView struct {
	AllianceID         int64
	State              string // "loading" | "missing" | "ready"
	LogoURL            string
	Name               string
	Ticker             string
	Founded            string
	CreatorID          int64
	CreatorName        string
	CreatorPending     bool // creator name still on its way; the cell polls for it
	CreatorCorpID      int64
	CreatorCorp        string
	CreatorCorpPending bool // creator-corporation name still on its way
	ExecutorCorpID     int64
	ExecutorCorp       string
	ExecutorPending    bool // executor-corporation name still on its way
	MemberCount        int
	Members            []allianceMemberView
	ViewerChars        map[int64]bool
}

// handleCorporationPage renders one corporation's public record.
func (app *Application) handleCorporationPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := pageData{
		LoggedIn:      true,
		CharacterName: app.sessions.GetString(ctx, sessionCharacterName),
		SSOConfigured: app.cfg.SSOConfigured(),
	}

	id, err := strconv.ParseInt(r.URL.Query().Get("corporation"), 10, 64)
	if err != nil || id <= 0 {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	data.Corporation = app.loadCorporationView(ctx, id)
	if data.Corporation != nil && data.Corporation.State == "loading" {
		app.notePageWant(ctx, pageWantCorporation, id, 0)
	}
	app.render(ctx, w, http.StatusOK, "corporation.html", data)
}

// handleAlliancePage renders one alliance's public record.
func (app *Application) handleAlliancePage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := pageData{
		LoggedIn:      true,
		CharacterName: app.sessions.GetString(ctx, sessionCharacterName),
		SSOConfigured: app.cfg.SSOConfigured(),
	}

	id, err := strconv.ParseInt(r.URL.Query().Get("alliance"), 10, 64)
	if err != nil || id <= 0 {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	data.Alliance = app.loadAllianceView(ctx, id)
	if data.Alliance != nil && data.Alliance.State == "loading" {
		app.notePageWant(ctx, pageWantAlliance, id, 0)
	}
	app.render(ctx, w, http.StatusOK, "alliance.html", data)
}

// handleCorporationFragment re-renders just the corporation
// record body from the corporation_records queue.
func (app *Application) handleCorporationFragment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := strconv.ParseInt(r.URL.Query().Get("corporation"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "bad corporation fragment request", http.StatusBadRequest)
		return
	}
	app.renderFragment(w, "corporation.html", "corporation-body", app.loadCorporationView(ctx, id))
}

// handleAllianceFragment re-renders just the alliance record
// body from the alliance_records queue.
func (app *Application) handleAllianceFragment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := strconv.ParseInt(r.URL.Query().Get("alliance"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "bad alliance fragment request", http.StatusBadRequest)
		return
	}
	app.renderFragment(w, "alliance.html", "alliance-body", app.loadAllianceView(ctx, id))
}

// loadCorporationView builds the render model for a corporation's
// record from the corporation_records queue, noting (and bumping)
// the want when no settled record exists. Cache-only; shared by
// the page and its live-region fragment.
func (app *Application) loadCorporationView(ctx context.Context, id int64) *corporationPageView {
	view := &corporationPageView{
		CorporationID: id,
		State:         "loading",
		LogoURL:       fmt.Sprintf("https://images.evetech.net/corporations/%d/logo?size=128", id),
		ViewerChars:   app.viewerCharSet(ctx),
	}

	rec, err := app.queries.GetCorporationRecord(ctx, id)
	switch {
	case err == nil && rec.State == orgStateReady && rec.Payload != "":
		if built, ok := app.buildCorporationView(ctx, id, rec.Payload); ok {
			view = built
		} else {
			// Corrupt payload: requeue rather than strand the
			// page on the loading state forever.
			logging.Warnf("corporation: unreadable payload for %d; requeueing", id)
			if serr := app.queries.SetCorporationRecord(ctx, db.SetCorporationRecordParams{
				CorporationID: id, Payload: "", State: orgStatePending, FetchedAt: sql.NullTime{},
			}); serr != nil {
				logging.Errorf("corporation: requeue %d: %v", id, serr)
			}
		}
	case err == nil && rec.State == orgStateMissing:
		view.State = "missing"
	default:
		// No record yet, a pending one, or a read error: (re)note
		// the want so the worker fills it on a coming cycle.
		if qerr := app.queries.UpsertCorporationWant(ctx, id); qerr != nil {
			logging.Errorf("corporation: note want for %d: %v", id, qerr)
		}
	}

	return view
}

// buildCorporationView turns a stored payload into the render
// model. The CEO's name and the home-station name resolve from
// the same local tiers every other page uses (and note their own
// wants while unresolved).
func (app *Application) buildCorporationView(ctx context.Context, id int64, payload string) (*corporationPageView, bool) {
	var p corporationRecordPayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return nil, false
	}
	view := &corporationPageView{
		CorporationID: id,
		State:         "ready",
		LogoURL:       fmt.Sprintf("https://images.evetech.net/corporations/%d/logo?size=128", id),
		Name:          p.Corp.Name,
		Ticker:        p.Corp.Ticker,
		MemberCount:   esi.FormatInt(p.Corp.MemberCount),
		TaxRate:       fmt.Sprintf("%.1f%%", p.Corp.TaxRate*100),
		ViewerChars:   app.viewerCharSet(ctx),
	}
	if len(p.Corp.DateFounded) >= 10 {
		view.Founded = p.Corp.DateFounded[:10]
	}
	if p.Corp.CEOID > 0 {
		view.CEOID = p.Corp.CEOID
		view.CEOName = app.displayCharacter(ctx, p.Corp.CEOID)
		view.CEOPending = app.characterLabelPending(ctx, p.Corp.CEOID)
	}
	if p.Corp.AllianceID > 0 {
		view.AllianceID = p.Corp.AllianceID
		if p.Alliance.Name != "" {
			view.AllianceLine = p.Alliance.Name
			if p.Alliance.Ticker != "" {
				view.AllianceLine += " [" + p.Alliance.Ticker + "]"
			}
		} else {
			view.AllianceLine = app.allianceDisplayName(ctx, p.Corp.AllianceID)
			view.AlliancePending = app.allianceNamePending(ctx, p.Corp.AllianceID)
		}
	}
	if p.Corp.HomeStationID > 0 {
		view.HomeStationID = p.Corp.HomeStationID
		if name, ok := app.esi.CachedPlaceName(ctx, p.Corp.HomeStationID); ok && name != "" {
			view.HomeStation = name
		} else if name := app.resolvedStructureTitle(ctx, p.Corp.HomeStationID); name != "" {
			view.HomeStation = name
		} else {
			if isStructureID(p.Corp.HomeStationID) {
				app.notePageWantFromContext(ctx, pageWantStructure, p.Corp.HomeStationID)
			} else {
				app.notePageWantFromContext(ctx, pageWantPlace, p.Corp.HomeStationID)
			}
			view.HomeStation = fmt.Sprintf("Station #%d", p.Corp.HomeStationID)
			view.HomePending = true
		}
	}
	if p.Corp.Description != "" {
		view.HasDescription = true
		view.Description = sanitizeMailHTML(p.Corp.Description)
	}
	return view, true
}

// loadAllianceView builds the render model for an alliance's
// record from the alliance_records queue, noting (and bumping)
// the want when no settled record exists. Cache-only; shared by
// the page and its live-region fragment.
func (app *Application) loadAllianceView(ctx context.Context, id int64) *alliancePageView {
	view := &alliancePageView{
		AllianceID:  id,
		State:       "loading",
		LogoURL:     fmt.Sprintf("https://images.evetech.net/alliances/%d/logo?size=128", id),
		ViewerChars: app.viewerCharSet(ctx),
	}

	rec, err := app.queries.GetAllianceRecord(ctx, id)
	switch {
	case err == nil && rec.State == orgStateReady && rec.Payload != "":
		if built, ok := app.buildAllianceView(ctx, id, rec.Payload); ok {
			view = built
		} else {
			logging.Warnf("alliance: unreadable payload for %d; requeueing", id)
			if serr := app.queries.SetAllianceRecord(ctx, db.SetAllianceRecordParams{
				AllianceID: id, Payload: "", State: orgStatePending, FetchedAt: sql.NullTime{},
			}); serr != nil {
				logging.Errorf("alliance: requeue %d: %v", id, serr)
			}
		}
	case err == nil && rec.State == orgStateMissing:
		view.State = "missing"
	default:
		if qerr := app.queries.UpsertAllianceWant(ctx, id); qerr != nil {
			logging.Errorf("alliance: note want for %d: %v", id, qerr)
		}
	}

	return view
}

// buildAllianceView turns a stored payload into the render model.
func (app *Application) buildAllianceView(ctx context.Context, id int64, payload string) (*alliancePageView, bool) {
	var p allianceRecordPayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return nil, false
	}
	view := &alliancePageView{
		AllianceID:  id,
		State:       "ready",
		LogoURL:     fmt.Sprintf("https://images.evetech.net/alliances/%d/logo?size=128", id),
		Name:        p.Alliance.Name,
		Ticker:      p.Alliance.Ticker,
		MemberCount: len(p.Corporations),
		ViewerChars: app.viewerCharSet(ctx),
	}
	if len(p.Alliance.DateFounded) >= 10 {
		view.Founded = p.Alliance.DateFounded[:10]
	}
	if p.Alliance.CreatorID > 0 {
		view.CreatorID = p.Alliance.CreatorID
		view.CreatorName = app.displayCharacter(ctx, p.Alliance.CreatorID)
		view.CreatorPending = app.characterLabelPending(ctx, p.Alliance.CreatorID)
	}
	if p.Alliance.CreatorCorporationID > 0 {
		view.CreatorCorpID = p.Alliance.CreatorCorporationID
		view.CreatorCorp = app.corpDisplayName(ctx, p.Alliance.CreatorCorporationID)
		view.CreatorCorpPending = app.corpNamePending(ctx, p.Alliance.CreatorCorporationID)
	}
	if p.Alliance.ExecutorCorporationID > 0 {
		view.ExecutorCorpID = p.Alliance.ExecutorCorporationID
		view.ExecutorCorp = app.corpDisplayName(ctx, p.Alliance.ExecutorCorporationID)
		view.ExecutorPending = app.corpNamePending(ctx, p.Alliance.ExecutorCorporationID)
	}
	for _, member := range p.Corporations {
		row := allianceMemberView{CorpID: member.ID, Name: member.Name}
		if name, ok := app.esi.CachedCorpName(member.ID); ok && name != "" {
			row.Name = name
		}
		if row.Name == "" {
			row.Name = app.corpDisplayName(ctx, member.ID)
		}
		row.NamePending = row.Name == fmt.Sprintf("Corporation #%d", member.ID) &&
			app.corpNamePending(ctx, member.ID)
		view.Members = append(view.Members, row)
	}
	return view, true
}

// resolvedCorpName reads the corporation-name tiers without
// queueing work: the name, and whether the answer is settled (a
// settled empty name means EVE has no such corporation).
func (app *Application) resolvedCorpName(ctx context.Context, corpID int64) (string, bool) {
	if name, ok := app.esi.CachedCorpName(corpID); ok && name != "" {
		return name, true
	}
	rec, err := app.queries.GetCorporationRecord(ctx, corpID)
	if err != nil {
		return "", false
	}
	if rec.State == orgStateReady && rec.Payload != "" {
		var payload corporationRecordPayload
		if jerr := json.Unmarshal([]byte(rec.Payload), &payload); jerr == nil && payload.Corp.Name != "" {
			app.esi.StoreCorpName(corpID, payload.Corp.Name)
			return payload.Corp.Name, true
		}
	}
	if rec.State == orgStateMissing {
		return "", true
	}
	return "", false
}

// corpNamePending reports whether a corporation label is still
// waiting on its first local answer.
func (app *Application) corpNamePending(ctx context.Context, corpID int64) bool {
	_, settled := app.resolvedCorpName(ctx, corpID)
	return !settled
}

// corpDisplayName resolves a corporation's display name from the
// local tiers — the in-process cache, then a ready organization
// record — noting a viewed-priority want while it is unresolved
// so the name (and its page) warm in the background. The honest
// "Corporation #<id>" fallback is the answer until then. Shared
// by every surface that renders a corporation name.
func (app *Application) corpDisplayName(ctx context.Context, corpID int64) string {
	if name, settled := app.resolvedCorpName(ctx, corpID); settled && name != "" {
		return name
	}
	app.notePageWantFromContext(ctx, pageWantCorporation, corpID)
	return fmt.Sprintf("Corporation #%d", corpID)
}

// resolvedAllianceName is resolvedCorpName for alliances.
func (app *Application) resolvedAllianceName(ctx context.Context, allianceID int64) (string, bool) {
	if name, ok := app.esi.CachedAllianceName(allianceID); ok && name != "" {
		return name, true
	}
	rec, err := app.queries.GetAllianceRecord(ctx, allianceID)
	if err != nil {
		return "", false
	}
	if rec.State == orgStateReady && rec.Payload != "" {
		var payload allianceRecordPayload
		if jerr := json.Unmarshal([]byte(rec.Payload), &payload); jerr == nil && payload.Alliance.Name != "" {
			app.esi.StoreAllianceName(allianceID, payload.Alliance.Name)
			return payload.Alliance.Name, true
		}
	}
	if rec.State == orgStateMissing {
		return "", true
	}
	return "", false
}

// allianceNamePending reports whether an alliance label is
// still waiting on its first local answer.
func (app *Application) allianceNamePending(ctx context.Context, allianceID int64) bool {
	_, settled := app.resolvedAllianceName(ctx, allianceID)
	return !settled
}

// allianceDisplayName is corpDisplayName for alliances.
func (app *Application) allianceDisplayName(ctx context.Context, allianceID int64) string {
	if name, settled := app.resolvedAllianceName(ctx, allianceID); settled && name != "" {
		return name
	}
	app.notePageWantFromContext(ctx, pageWantAlliance, allianceID)
	return fmt.Sprintf("Alliance #%d", allianceID)
}

// resolvedPlaceName reads the place-name tiers without queueing
// work: the SDE/cache station or system name, then a resolved
// structure title; settled with an empty name when the structure
// queue has a settled miss for the id.
func (app *Application) resolvedPlaceName(ctx context.Context, id int64) (string, bool) {
	if name, ok := app.esi.CachedPlaceName(ctx, id); ok && name != "" {
		return name, true
	}
	if name := app.resolvedStructureTitle(ctx, id); name != "" {
		return name, true
	}
	if row, err := app.queries.GetStructureName(ctx, id); err == nil && row.State == esi.StructureMissing {
		return "", true
	}
	return "", false
}

// refreshCorporationRecords drains the corporation queue inside
// the cycle budget: 'pending' rows first, then ready rows gone
// stale. Called from refreshCycle.
func (app *Application) refreshCorporationRecords(ctx context.Context, allowance *fetchBudget) (drained int, limited bool) {
	app.fetchMu.Lock()
	defer app.fetchMu.Unlock()
	return app.drainCorporationPass(ctx, allowance, maxOrgDrainsPerCycle, time.Now().UTC())
}

// refreshAllianceRecords is refreshCorporationRecords for the
// alliance queue.
func (app *Application) refreshAllianceRecords(ctx context.Context, allowance *fetchBudget) (drained int, limited bool) {
	app.fetchMu.Lock()
	defer app.fetchMu.Unlock()
	return app.drainAlliancePass(ctx, allowance, maxOrgDrainsPerCycle, time.Now().UTC())
}

// drainCorporationPass fills up to limit due corporation records.
// Callers hold the shared fetch lock (the cycle wrappers above,
// the urgent drain).
func (app *Application) drainCorporationPass(ctx context.Context, allowance *fetchBudget, limit int, now time.Time) (drained int, limited bool) {
	ids, err := app.queries.ListCorporationDrains(ctx, db.ListCorporationDrainsParams{
		StaleCutoff: now.Add(-orgStaleAfter),
		DrainLimit:  int64(limit),
	})
	if err != nil {
		logging.Errorf("worker: corporation records: list drains: %v", err)
		return 0, false
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		settled, ltd := app.drainCorporationRecord(ctx, id, allowance, now)
		if ltd {
			return drained, true
		}
		if settled {
			drained++
		}
	}
	return drained, false
}

// drainAlliancePass is drainCorporationPass for alliances.
func (app *Application) drainAlliancePass(ctx context.Context, allowance *fetchBudget, limit int, now time.Time) (drained int, limited bool) {
	ids, err := app.queries.ListAllianceDrains(ctx, db.ListAllianceDrainsParams{
		StaleCutoff: now.Add(-orgStaleAfter),
		DrainLimit:  int64(limit),
	})
	if err != nil {
		logging.Errorf("worker: alliance records: list drains: %v", err)
		return 0, false
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		settled, ltd := app.drainAllianceRecord(ctx, id, allowance, now)
		if ltd {
			return drained, true
		}
		if settled {
			drained++
		}
	}
	return drained, false
}

// drainCorporationRecord assembles and stores one corporation's
// public record from ESI's public endpoints (no token involved
// anywhere). A 404 settles the record as 'missing'.
func (app *Application) drainCorporationRecord(ctx context.Context, id int64, allowance *fetchBudget, now time.Time) (settled bool, limited bool) {

	if !allowance.take() {
		return false, false
	}
	var corp esi.Corporation
	if err := app.esi.Get(ctx, "", fmt.Sprintf("/corporations/%d/", id), &corp); err != nil {
		if errors.Is(err, esi.ErrErrorLimit) {
			logging.Warnf("worker: corporation records: ESI error limit hit resolving corporation %d; backing off until next cycle", id)
			return false, true
		}
		if code, has := esi.StatusCode(err); has && code == http.StatusNotFound {
			if serr := app.queries.SetCorporationRecord(ctx, db.SetCorporationRecordParams{
				CorporationID: id, Payload: "", State: orgStateMissing, FetchedAt: timeSet(now),
			}); serr != nil {
				logging.Errorf("worker: corporation records: record miss for %d: %v", id, serr)
				return false, false
			}
			return true, false
		}
		logging.Errorf("worker: corporation records: fetch corporation %d: %v", id, err)
		return false, false
	}
	app.esi.StoreCorpName(id, corp.Name)

	payload := corporationRecordPayload{Corp: corp}

	// The alliance's name + ticker ride along so the corporation
	// page (and every corp line elsewhere) never chases them.
	if corp.AllianceID > 0 && allowance.take() {
		var alliance esi.Alliance
		if err := app.esi.Get(ctx, "", fmt.Sprintf("/alliances/%d/", corp.AllianceID), &alliance); err != nil {
			if errors.Is(err, esi.ErrErrorLimit) {
				return false, true
			}
			logging.Errorf("worker: corporation records: fetch alliance %d for corporation %d: %v", corp.AllianceID, id, err)
		} else {
			payload.Alliance = alliance
			app.esi.StoreAllianceName(corp.AllianceID, alliance.Name)
		}
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		logging.Errorf("worker: corporation records: encode payload for %d: %v", id, err)
		return false, false
	}
	if err := app.queries.SetCorporationRecord(ctx, db.SetCorporationRecordParams{
		CorporationID: id, Payload: string(encoded), State: orgStateReady, FetchedAt: timeSet(now),
	}); err != nil {
		logging.Errorf("worker: corporation records: store record for %d: %v", id, err)
		return false, false
	}
	return true, false
}

// drainAllianceRecord assembles and stores one alliance's public
// record: the profile plus the member-corporation list, with as
// many member names baked in as the pass budget allows (the rest
// resolve through the corporation queue, like pilot employment
// histories). A 404 settles the record as 'missing'.
func (app *Application) drainAllianceRecord(ctx context.Context, id int64, allowance *fetchBudget, now time.Time) (settled bool, limited bool) {

	if !allowance.take() {
		return false, false
	}
	var alliance esi.Alliance
	if err := app.esi.Get(ctx, "", fmt.Sprintf("/alliances/%d/", id), &alliance); err != nil {
		if errors.Is(err, esi.ErrErrorLimit) {
			logging.Warnf("worker: alliance records: ESI error limit hit resolving alliance %d; backing off until next cycle", id)
			return false, true
		}
		if code, has := esi.StatusCode(err); has && code == http.StatusNotFound {
			if serr := app.queries.SetAllianceRecord(ctx, db.SetAllianceRecordParams{
				AllianceID: id, Payload: "", State: orgStateMissing, FetchedAt: timeSet(now),
			}); serr != nil {
				logging.Errorf("worker: alliance records: record miss for %d: %v", id, serr)
				return false, false
			}
			return true, false
		}
		logging.Errorf("worker: alliance records: fetch alliance %d: %v", id, err)
		return false, false
	}
	app.esi.StoreAllianceName(id, alliance.Name)

	payload := allianceRecordPayload{Alliance: alliance}

	if allowance.take() {
		var corpIDs []int64
		if err := app.esi.Get(ctx, "", fmt.Sprintf("/alliances/%d/corporations/", id), &corpIDs); err != nil {
			if errors.Is(err, esi.ErrErrorLimit) {
				return false, true
			}
			// The member list failing is not fatal to the record:
			// store the profile with an empty list.
			logging.Warnf("worker: alliance records: fetch member corporations for %d: %v (storing profile only)", id, err)
		} else {
			nameFetches := 0
			for _, corpID := range corpIDs {
				member := allianceMemberCorp{ID: corpID}
				if name, ok := app.esi.CachedCorpName(corpID); ok && name != "" {
					member.Name = name
				} else if nameFetches < maxAllianceMemberNamesPerDrain && allowance.take() {
					nameFetches++
					var corp esi.Corporation
					if err := app.esi.Get(ctx, "", fmt.Sprintf("/corporations/%d/", corpID), &corp); err != nil {
						if errors.Is(err, esi.ErrErrorLimit) {
							return false, true
						}
						logging.Errorf("worker: alliance records: fetch member corporation %d for alliance %d: %v", corpID, id, err)
					} else {
						member.Name = corp.Name
						app.esi.StoreCorpName(corpID, corp.Name)
					}
				}
				payload.Corporations = append(payload.Corporations, member)
			}
		}
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		logging.Errorf("worker: alliance records: encode payload for %d: %v", id, err)
		return false, false
	}
	if err := app.queries.SetAllianceRecord(ctx, db.SetAllianceRecordParams{
		AllianceID: id, Payload: string(encoded), State: orgStateReady, FetchedAt: timeSet(now),
	}); err != nil {
		logging.Errorf("worker: alliance records: store record for %d: %v", id, err)
		return false, false
	}
	return true, false
}
