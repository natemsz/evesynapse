package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// ---------------------------------------------------------------------------
// Killmails page: the character's most recent kills and losses.
// The recent list is a snapshot; the detail payloads behind it are
// warmed into the killmail_details store by the worker. This page
// reads the store only — a detail that has not landed yet renders
// as a "details warming" row instead of blocking on ESI.
// ---------------------------------------------------------------------------

// maxKillmailsShown caps the killmail list (the recent-list
// endpoint can return far more than a page wants).
const maxKillmailsShown = 50

// killmailRow is one list line. Rows without a stored detail yet
// carry Warming=true and placeholder fields.
type killmailRow struct {
	Time      string // formatted kill time, "—" while warming
	System    string
	Kill      bool // badge: victim is someone else
	Loss      bool // badge: victim is the viewing character
	Victim    string
	Ship      string // victim ship type name
	FinalBlow string // final-blow attacker display
	Involved  string // attacker count, "—" while warming
	Value     string // estimated destroyed+dropped value, bare number; "—" when unpriced
	Warming   bool
}

// killmailsView is the Killmails page body for one character.
type killmailsView struct {
	CharacterName string
	Loaded        bool
	Warming       bool // no recent-list snapshot yet at all
	Rows          []killmailRow
}

// handleKillmails renders the Killmails page for one of the
// signed-in user's characters (switchable via ?character=).
func (app *Application) handleKillmails(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := pageData{
		LoggedIn:      true,
		CharacterName: app.sessions.GetString(ctx, sessionCharacterName),
		SSOConfigured: app.cfg.SSOConfigured(),
	}

	_, active, links, err := app.pickCharacter(ctx, r, "/killmails/")
	if err != nil {
		log.Printf("killmails: list characters: %v", err)
		data.Error = "Could not load killmail data; check the server log."
		app.render(w, http.StatusOK, "killmails.html", data)
		return
	}
	if links == nil {
		app.render(w, http.StatusOK, "killmails.html", data)
		return
	}
	data.KillmailChars = links

	view := &killmailsView{CharacterName: active.Name}
	data.Killmails = view

	var refs []esi.KillmailRef
	if err := app.esi.GetCached(ctx, active, esi.SnapKillmails, &refs); err != nil {
		log.Printf("killmails: load recent list for character %d: %v", active.CharacterID, err)
		// No snapshot row at all = cold start: the worker is still
		// importing this character, which the Sync page shows live.
		if _, serr := app.queries.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: active.CharacterID, Kind: esi.SnapKillmails}); errors.Is(serr, sql.ErrNoRows) {
			view.Warming = true
		}
		app.render(w, http.StatusOK, "killmails.html", data)
		return
	}
	view.Loaded = true

	if len(refs) > maxKillmailsShown {
		refs = refs[:maxKillmailsShown]
	}

	// Estimated values reuse the Market page's /markets/prices/
	// cache when it has been populated; nothing here fetches.
	prices := app.cachedPrices()

	for _, ref := range refs {
		view.Rows = append(view.Rows, app.killmailRow(ctx, active.CharacterID, ref, prices))
	}

	app.render(w, http.StatusOK, "killmails.html", data)
}

// killmailRow builds one list line from the stored detail. A
// missing detail (worker has not warmed it yet) yields the
// warming placeholder row.
func (app *Application) killmailRow(ctx context.Context, characterID int64, ref esi.KillmailRef, prices map[int64]esi.MarketPrice) killmailRow {
	row := killmailRow{
		Time:      "—",
		System:    "—",
		Victim:    "—",
		Ship:      "—",
		FinalBlow: "—",
		Involved:  "—",
		Value:     "—",
		Warming:   true,
	}

	stored, err := app.queries.GetKillmailDetail(ctx, ref.KillmailID)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("killmails: read detail %d: %v", ref.KillmailID, err)
		}
		return row
	}
	var km esi.Killmail
	if err := json.Unmarshal([]byte(stored.Payload), &km); err != nil {
		log.Printf("killmails: decode detail %d: %v", ref.KillmailID, err)
		return row
	}
	row.Warming = false

	row.Time = formatFinish(km.KillmailTime)
	row.System = app.locationTitle(ctx, km.SolarSystemID, "solar_system")
	if km.Victim.CharacterID == characterID {
		row.Loss = true
	} else {
		row.Kill = true
	}
	row.Victim = characterDisplay(app.esi, km.Victim.CharacterID)
	row.Ship = app.typeNameOrID(ctx, km.Victim.ShipTypeID)
	row.Involved = fmt.Sprintf("%d", len(km.Attackers))
	for _, a := range km.Attackers {
		if a.FinalBlow {
			row.FinalBlow = characterDisplay(app.esi, a.CharacterID)
			break
		}
	}

	// Estimated value: destroyed + dropped quantities at CCP's
	// average prices, over the victim's item list. Items without a
	// known price are skipped; nothing priced → "—".
	if prices != nil {
		var total float64
		priced := false
		for _, it := range km.Victim.Items {
			qty := it.QuantityDestroyed + it.QuantityDropped
			if qty <= 0 {
				continue
			}
			if p, ok := prices[it.ItemTypeID]; ok && p.AveragePrice > 0 {
				total += float64(qty) * p.AveragePrice
				priced = true
			}
		}
		if priced {
			row.Value = esi.FormatISK(total)
		}
	}
	return row
}

// characterDisplay renders a killmail participant: the worker-
// warmed name when cached, an honest "Character #<id>" otherwise,
// and "NPC" for the character-less victims/attackers CCP reports
// with ID 0. Cache-only: renders never wait on ESI for names.
func characterDisplay(client *esi.Client, characterID int64) string {
	if characterID <= 0 {
		return "NPC"
	}
	if name, ok := client.CachedCharacterName(characterID); ok {
		return name
	}
	return fmt.Sprintf("Character #%d", characterID)
}
