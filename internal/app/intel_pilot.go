package app

import (
	"context"
	"database/sql"
	"encoding/json"
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
// Public pilot page (/pilot/): what EveSynapse shows for a
// character who is NOT one of the signed-in user's linked
// characters — the destination every resolved stranger name links
// to (wallet counterparties, contract issuers, mail senders, …).
//
// Everything on it is public ESI data: the character profile
// (name, corporation, alliance, faction, birthday, security
// status, description, title) and the employment history. These
// pilots have no token relationship with this instance, so they
// cannot live in character_snapshots (keyed on linked characters
// with a foreign key): the page is backed by the pilot_records
// queue instead. Viewing a pilot without a record notes a
// 'pending' row; the worker drains those from ESI's public
// endpoints inside the cycle budget and stores one assembled
// payload (corp/alliance/faction names baked in, so renders never
// chase names). Ready records go stale after a week and refresh
// in the background — the page keeps showing the last known
// record meanwhile. An ESI 404 settles as 'missing'.
//
// This file is the page. The worker's side (draining the queue,
// noting who the deployment's data mentions) is intel_pilot_worker.go.
// ---------------------------------------------------------------------------

// Pilot record states (pilot_records.state).
const (
	pilotStatePending = "pending"
	pilotStateReady   = "ready"
	pilotStateMissing = "missing"
)

// Drain bounds: a handful of strangers per cycle is plenty —
// these are lookups a human just asked for, not fleet sync.
const (
	pilotStaleAfter             = 7 * 24 * time.Hour
	maxPilotDrainsPerCycle      = 5
	maxPilotNameResolutions     = 3 // per pass; one ESI name lookup each
	pilotNameWantRetryDelay     = 15 * time.Minute
	maxHistoryCorpNamesPerDrain = 12
	maxTypeDetailsPerCycle      = 8
	// maxOrbitPilotsPerCycle bounds how many newly seen
	// counterparties one cycle notes for proactive warming; the
	// queue drains a few per cycle (and viewed pilots outrank
	// the orbit), so a big roster converges over a few cycles.
	maxOrbitPilotsPerCycle = 50
)

// pilotPayload is the stored public record: the profile, the
// corporation/alliance payloads, the faction's display name, and
// the employment history with names baked in, oldest stint first.
type pilotPayload struct {
	Profile     esi.Character     `json:"profile"`
	Corp        esi.Corporation   `json:"corp"`
	Alliance    esi.Alliance      `json:"alliance"`
	FactionName string            `json:"faction_name"`
	History     []pilotHistoryRow `json:"history"`
}

// pilotHistoryRow is one employment stint; End is the next
// stint's start, "" while the stint is current.
type pilotHistoryRow struct {
	CorpID   int64  `json:"corp_id"`
	CorpName string `json:"corp_name"`
	Start    string `json:"start"` // RFC3339, as ESI sent it
	End      string `json:"end"`
}

type pilotHistoryView struct {
	CorpID   int64
	CorpName string
	From     string
	To       string // "Present" for the current stint
}

// pilotView is the /pilot/ page body.
type pilotView struct {
	CharacterID    int64
	State          string // "loading" | "missing" | "ready"
	PortraitURL    string
	Name           string
	Title          string
	CorpID         int64
	CorpLine       string // name [ticker], "" when unknown
	AllianceID     int64
	AllianceLine   string
	FactionName    string
	Security       string
	Birthday       string
	HasDescription bool
	Description    template.HTML // sanitized, same rules as mail bodies
	History        []pilotHistoryView
}

// handlePilot renders a stranger's public record. One of the
// viewer's own characters redirects to its full sheet instead.
func (app *Application) handlePilot(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.page(ctx)

	id, err := strconv.ParseInt(r.URL.Query().Get("character"), 10, 64)
	if err != nil || id <= 0 {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	// Own characters belong on their full character sheet.
	if app.isOwnCharacter(ctx, id) {
		http.Redirect(w, r, "/character/?character="+strconv.FormatInt(id, 10), http.StatusSeeOther)
		return
	}

	app.markGuessHit(ctx, pageWantPilot, id, 0)
	data.Pilot = app.loadPilotView(ctx, id)
	if data.Pilot != nil && data.Pilot.State == "loading" {
		app.notePageWant(ctx, pageWantPilot, id, 0)
	}
	app.render(ctx, w, http.StatusOK, "pilot.html", data)
}

// isOwnCharacter reports whether id is one of the signed-in
// user's linked characters.
func (app *Application) isOwnCharacter(ctx context.Context, id int64) bool {
	userID := app.userID(ctx)
	if userID == 0 {
		return false
	}
	chars, err := app.queries.ListCharactersByUser(ctx, userID)
	if err != nil {
		return false
	}
	for _, ch := range chars {
		if ch.CharacterID == id {
			return true
		}
	}
	return false
}

// loadPilotView builds the render model for a stranger's record
// from the pilot_records queue, noting (and bumping) the want
// when no settled record exists. Cache-only; shared by the page
// and its live-region fragment.
func (app *Application) loadPilotView(ctx context.Context, id int64) *pilotView {
	view := &pilotView{CharacterID: id, PortraitURL: portraitURL(id, 128), State: "loading"}

	rec, err := app.queries.GetPilotRecord(ctx, id)
	switch {
	case err == nil && rec.State == pilotStateReady && rec.Payload != "":
		if built, ok := buildPilotView(id, rec.Payload); ok {
			view = built
		} else {
			// Corrupt payload: requeue rather than strand the
			// page on the loading state forever.
			logging.Warnf("pilot: unreadable payload for %d; requeueing", id)
			if serr := app.queries.SetPilotRecord(ctx, db.SetPilotRecordParams{
				CharacterID: id, Payload: "", State: pilotStatePending, FetchedAt: sql.NullTime{},
			}); serr != nil {
				logging.Errorf("pilot: requeue %d: %v", id, serr)
			}
		}
	case err == nil && rec.State == pilotStateMissing:
		view.State = "missing"
	default:
		// No record yet, a pending one, or a read error: (re)note
		// the want so the worker fills it on a coming cycle.
		if qerr := app.wantPilot(ctx, id, wantViewed); qerr != nil {
			logging.Errorf("pilot: note want for %d: %v", id, qerr)
		}
	}

	return view
}

// buildPilotView turns a stored payload into the render model.
func buildPilotView(id int64, payload string) (*pilotView, bool) {
	var p pilotPayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return nil, false
	}
	view := &pilotView{
		CharacterID: id,
		State:       "ready",
		PortraitURL: portraitURL(id, 128),
		Name:        p.Profile.Name,
		Title:       p.Profile.Title,
		FactionName: p.FactionName,
	}
	if p.Corp.Name != "" {
		view.CorpID = p.Profile.CorporationID
		view.CorpLine = p.Corp.Name
		if p.Corp.Ticker != "" {
			view.CorpLine += " [" + p.Corp.Ticker + "]"
		}
	}
	if p.Alliance.Name != "" {
		view.AllianceID = p.Profile.AllianceID
		view.AllianceLine = p.Alliance.Name
		if p.Alliance.Ticker != "" {
			view.AllianceLine += " [" + p.Alliance.Ticker + "]"
		}
	}
	view.Security = fmt.Sprintf("%.2f", p.Profile.SecurityStatus)
	if t, err := time.Parse(time.RFC3339, p.Profile.Birthday); err == nil {
		view.Birthday = t.UTC().Format("2006-01-02")
	}
	if p.Profile.Description != "" {
		view.HasDescription = true
		view.Description = sanitizeMailHTML(p.Profile.Description)
	}
	for _, stint := range p.History {
		row := pilotHistoryView{CorpID: stint.CorpID, CorpName: stint.CorpName, To: "Present"}
		if row.CorpName == "" {
			// An unresolved stint stays plain text rather than
			// linking a placeholder label at the corporation page.
			row.CorpName = "Unknown corporation"
			row.CorpID = 0
		}
		if t, err := time.Parse(time.RFC3339, stint.Start); err == nil {
			row.From = t.UTC().Format("2006-01-02")
		}
		if stint.End != "" {
			if t, err := time.Parse(time.RFC3339, stint.End); err == nil {
				row.To = t.UTC().Format("2006-01-02")
			}
		}
		view.History = append(view.History, row)
	}
	return view, true
}
