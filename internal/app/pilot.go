package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"sort"
	"strconv"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
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
	maxHistoryCorpNamesPerDrain = 12
	maxTypeDetailsPerCycle      = 8
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
	CorpLine       string // name [ticker], "" when unknown
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
	data := pageData{
		LoggedIn:      true,
		CharacterName: app.sessions.GetString(ctx, sessionCharacterName),
		SSOConfigured: app.cfg.SSOConfigured(),
	}

	id, err := strconv.ParseInt(r.URL.Query().Get("character"), 10, 64)
	if err != nil || id <= 0 {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	// Own characters belong on their full character sheet.
	if userID := int64(app.sessions.GetInt(ctx, sessionUserID)); userID != 0 {
		if chars, cerr := app.queries.ListCharactersByUser(ctx, userID); cerr == nil {
			for _, ch := range chars {
				if ch.CharacterID == id {
					http.Redirect(w, r, "/character/?character="+strconv.FormatInt(id, 10), http.StatusSeeOther)
					return
				}
			}
		}
	}

	view := &pilotView{CharacterID: id, PortraitURL: portraitURL(id, 128), State: "loading"}

	rec, err := app.queries.GetPilotRecord(ctx, id)
	switch {
	case err == nil && rec.State == pilotStateReady && rec.Payload != "":
		if built, ok := buildPilotView(id, rec.Payload); ok {
			view = built
		} else {
			// Corrupt payload: requeue rather than strand the
			// page on the loading state forever.
			log.Printf("pilot: unreadable payload for %d; requeueing", id)
			if serr := app.queries.SetPilotRecord(ctx, db.SetPilotRecordParams{
				CharacterID: id, Payload: "", State: pilotStatePending, FetchedAt: "",
			}); serr != nil {
				log.Printf("pilot: requeue %d: %v", id, serr)
			}
		}
	case err == nil && rec.State == pilotStateMissing:
		view.State = "missing"
	default:
		// No record yet, a pending one, or a read error: (re)note
		// the want so the worker fills it on a coming cycle.
		if qerr := app.queries.UpsertPilotWant(ctx, id); qerr != nil {
			log.Printf("pilot: note want for %d: %v", id, qerr)
		}
	}

	data.Pilot = view
	app.render(ctx, w, http.StatusOK, "pilot.html", data)
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
		view.CorpLine = p.Corp.Name
		if p.Corp.Ticker != "" {
			view.CorpLine += " [" + p.Corp.Ticker + "]"
		}
	}
	if p.Alliance.Name != "" {
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
		row := pilotHistoryView{CorpName: stint.CorpName, To: "Present"}
		if row.CorpName == "" {
			row.CorpName = "Unknown corporation"
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

// refreshPilotRecords drains the pilot queue inside the cycle
// budget: 'pending' rows first, then ready rows gone stale.
// Returns how many records it settled and whether ESI's error
// limit stopped the pass. Called from refreshCycle.
func (app *Application) refreshPilotRecords(ctx context.Context, allowance *fetchBudget) (drained int, limited bool) {
	now := time.Now().UTC()
	ids, err := app.queries.ListPilotDrains(ctx, db.ListPilotDrainsParams{
		StaleCutoff: now.Add(-pilotStaleAfter).Format(time.RFC3339),
		DrainLimit:  maxPilotDrainsPerCycle,
	})
	if err != nil {
		log.Printf("worker: pilot records: list drains: %v", err)
		return 0, false
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		settled, ltd := app.drainPilotRecord(ctx, id, allowance, now)
		if ltd {
			return drained, true
		}
		if settled {
			drained++
		}
	}
	return drained, false
}

// drainPilotRecord assembles and stores one pilot's public record
// from ESI's public endpoints (no token involved anywhere). A 404
// on the profile settles the record as 'missing'.
func (app *Application) drainPilotRecord(ctx context.Context, id int64, allowance *fetchBudget, now time.Time) (settled bool, limited bool) {
	stamp := now.Format(time.RFC3339)

	if !allowance.take() {
		return false, false
	}
	var profile esi.Character
	if err := app.esi.Get(ctx, "", fmt.Sprintf("/characters/%d/", id), &profile); err != nil {
		if errors.Is(err, esi.ErrErrorLimit) {
			log.Printf("worker: pilot records: ESI error limit hit resolving pilot %d; backing off until next cycle", id)
			return false, true
		}
		if code, has := esi.StatusCode(err); has && code == http.StatusNotFound {
			if serr := app.queries.SetPilotRecord(ctx, db.SetPilotRecordParams{
				CharacterID: id, Payload: "", State: pilotStateMissing, FetchedAt: stamp,
			}); serr != nil {
				log.Printf("worker: pilot records: record miss for %d: %v", id, serr)
				return false, false
			}
			return true, false
		}
		log.Printf("worker: pilot records: fetch profile %d: %v", id, err)
		return false, false
	}
	app.esi.StoreCharacterName(id, profile.Name)

	if !allowance.take() {
		return false, false
	}
	history := esi.CorpHistory{}
	if err := app.esi.Get(ctx, "", fmt.Sprintf("/characters/%d/corporationhistory/", id), &history); err != nil {
		if errors.Is(err, esi.ErrErrorLimit) {
			log.Printf("worker: pilot records: ESI error limit hit fetching history for %d; backing off until next cycle", id)
			return false, true
		}
		// History failing is not fatal to the record: store the
		// profile with whatever history we could not get as empty.
		log.Printf("worker: pilot records: fetch history %d: %v (storing profile only)", id, err)
		history = esi.CorpHistory{}
	}

	payload := pilotPayload{Profile: profile}

	// Current corporation (name + ticker), alliance, faction —
	// each is one public GET, skipped when the profile has none.
	if profile.CorporationID > 0 && allowance.take() {
		var corp esi.Corporation
		if err := app.esi.Get(ctx, "", fmt.Sprintf("/corporations/%d/", profile.CorporationID), &corp); err != nil {
			if errors.Is(err, esi.ErrErrorLimit) {
				return false, true
			}
			log.Printf("worker: pilot records: fetch corp %d for pilot %d: %v", profile.CorporationID, id, err)
		} else {
			payload.Corp = corp
			app.esi.StoreCorpName(profile.CorporationID, corp.Name)
		}
	}
	if profile.AllianceID > 0 && allowance.take() {
		var alliance esi.Alliance
		if err := app.esi.Get(ctx, "", fmt.Sprintf("/alliances/%d/", profile.AllianceID), &alliance); err != nil {
			if errors.Is(err, esi.ErrErrorLimit) {
				return false, true
			}
			log.Printf("worker: pilot records: fetch alliance %d for pilot %d: %v", profile.AllianceID, id, err)
		} else {
			payload.Alliance = alliance
			app.esi.StoreAllianceName(profile.AllianceID, alliance.Name)
		}
	}
	if profile.FactionID > 0 && allowance.take() {
		var factions []esi.Faction
		if err := app.esi.Get(ctx, "", "/universe/factions/", &factions); err != nil {
			if errors.Is(err, esi.ErrErrorLimit) {
				return false, true
			}
			log.Printf("worker: pilot records: fetch factions for pilot %d: %v", id, err)
		} else {
			for _, f := range factions {
				if f.FactionID == profile.FactionID {
					payload.FactionName = f.Name
					break
				}
			}
		}
	}

	// Employment history: oldest first, each stint ending where
	// the next begins. Corp names resolve from the name cache
	// first; the worker spends a few extra fetches on the rest.
	stints := make([]esi.CorpHistoryEntry, len(history))
	copy(stints, history)
	sort.SliceStable(stints, func(i, j int) bool { return stints[i].StartDate < stints[j].StartDate })
	historyNameFetches := 0
	for i, stint := range stints {
		row := pilotHistoryRow{CorpID: stint.CorporationID, Start: stint.StartDate}
		if i+1 < len(stints) {
			row.End = stints[i+1].StartDate
		}
		if name, ok := app.esi.CachedCorpName(stint.CorporationID); ok {
			row.CorpName = name
		} else if stint.CorporationID > 0 && historyNameFetches < maxHistoryCorpNamesPerDrain && allowance.take() {
			historyNameFetches++
			var corp esi.Corporation
			if err := app.esi.Get(ctx, "", fmt.Sprintf("/corporations/%d/", stint.CorporationID), &corp); err != nil {
				if errors.Is(err, esi.ErrErrorLimit) {
					return false, true
				}
				log.Printf("worker: pilot records: fetch history corp %d for pilot %d: %v", stint.CorporationID, id, err)
			} else {
				row.CorpName = corp.Name
				app.esi.StoreCorpName(stint.CorporationID, corp.Name)
			}
		}
		payload.History = append(payload.History, row)
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		log.Printf("worker: pilot records: encode payload for %d: %v", id, err)
		return false, false
	}
	if err := app.queries.SetPilotRecord(ctx, db.SetPilotRecordParams{
		CharacterID: id, Payload: string(encoded), State: pilotStateReady, FetchedAt: stamp,
	}); err != nil {
		log.Printf("worker: pilot records: store record for %d: %v", id, err)
		return false, false
	}
	return true, false
}

// refreshTypeDetails drains the item-description wants the item
// details page notes (a type_details row with no fetched_at),
// storing the public type payload's description. A type ESI no
// longer knows settles with an empty description so the page
// stops asking. Called from refreshCycle.
func (app *Application) refreshTypeDetails(ctx context.Context, allowance *fetchBudget) (drained int, limited bool) {
	stamp := time.Now().UTC().Format(time.RFC3339)
	ids, err := app.queries.ListTypeDetailWants(ctx, maxTypeDetailsPerCycle)
	if err != nil {
		log.Printf("worker: type details: list wants: %v", err)
		return 0, false
	}
	for _, id := range ids {
		if ctx.Err() != nil || !allowance.take() {
			break
		}
		var t esi.Type
		err := app.esi.Get(ctx, "", fmt.Sprintf("/universe/types/%d/", id), &t)
		switch {
		case err == nil:
			if serr := app.queries.SetTypeDetail(ctx, db.SetTypeDetailParams{
				TypeID: id, Description: t.Description, FetchedAt: stamp,
			}); serr != nil {
				log.Printf("worker: type details: store %d: %v", id, serr)
				continue
			}
			drained++
		case errors.Is(err, esi.ErrErrorLimit):
			log.Printf("worker: type details: ESI error limit hit fetching type %d; backing off until next cycle", id)
			return drained, true
		default:
			if code, has := esi.StatusCode(err); has && code == http.StatusNotFound {
				if serr := app.queries.SetTypeDetail(ctx, db.SetTypeDetailParams{
					TypeID: id, Description: "", FetchedAt: stamp,
				}); serr != nil {
					log.Printf("worker: type details: settle miss %d: %v", id, serr)
					continue
				}
				drained++
				continue
			}
			log.Printf("worker: type details: fetch type %d: %v", id, err)
		}
	}
	return drained, false
}
