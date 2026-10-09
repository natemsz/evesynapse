package app

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Planetary industry page (/planets/): per-character colonies from
// the worker-warmed colonies + layout snapshots, cache-only like
// every other page.
//
// Staleness honesty: extractor expiry, cycle time, quantity per
// cycle and head counts are fixed at pin install in ESI and safe
// to count down from. Stored contents amounts and last_cycle_start
// are only recalculated when the colony is viewed in the game
// client, so they are never shown — the page footnotes why. A
// character whose login predates the planetary scope sees the
// recorded "not enabled — re-link" state, never a warming loop.
// ---------------------------------------------------------------------------

// extractorInfo is one extractor pin distilled for display and
// for the home widget/attention builders: reliable (install-time)
// facts only.
type extractorInfo struct {
	ProductID   int64
	QtyPerCycle int64
	CycleTime   int64 // seconds
	Heads       int
	Expiry      time.Time
	ExpiryOK    bool
}

// layoutExtractors extracts the extractor pins of a colony layout
// (the ones carrying extractor_details), sorted soonest expiry
// first (unknown expiries last).
func layoutExtractors(layout esi.PlanetLayout) []extractorInfo {
	var out []extractorInfo
	for _, pin := range layout.Pins {
		if pin.ExtractorDetails == nil {
			continue
		}
		info := extractorInfo{
			ProductID:   pin.ExtractorDetails.ProductTypeID,
			QtyPerCycle: pin.ExtractorDetails.QtyPerCycle,
			CycleTime:   pin.ExtractorDetails.CycleTime,
			Heads:       len(pin.ExtractorDetails.Heads),
		}
		if t, ok := parseRFC3339(pin.ExpiryTime); ok {
			info.Expiry, info.ExpiryOK = t, true
		}
		out = append(out, info)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ExpiryOK != out[j].ExpiryOK {
			return out[i].ExpiryOK
		}
		return out[i].Expiry.Before(out[j].Expiry)
	})
	return out
}

// loadPlanetLayout decodes one planet's stored layout snapshot.
func (app *Application) loadPlanetLayout(ctx context.Context, characterID, planetID int64) (esi.PlanetLayout, bool) {
	var layout esi.PlanetLayout
	if !app.loadCorpSnapshot(ctx, characterID, esi.PlanetLayoutKind(planetID), &layout) {
		return esi.PlanetLayout{}, false
	}
	return layout, true
}

// piSummary distills one character's PI state for the character
// page and home surfaces: colony count, expired extractor count,
// and the soonest extractor expiry across the warmed layouts.
// Known=false means no colonies snapshot exists yet; a recorded
// scope refusal is reported through NotEnabled instead.
type piSummary struct {
	Known      bool
	NotEnabled bool
	Colonies   int
	Expired    int
	Soonest    time.Time
	SoonestOK  bool
}

// characterPISummary builds the summary from local rows only: the
// colonies snapshot plus whichever layout snapshots have warmed.
func (app *Application) characterPISummary(ctx context.Context, ch db.Character) piSummary {
	var colonies esi.Colonies
	if !app.loadCorpSnapshot(ctx, ch.CharacterID, esi.SnapPlanets, &colonies) {
		return piSummary{NotEnabled: app.piNotEnabled(ctx, ch)}
	}
	summary := piSummary{Known: true, Colonies: len(colonies)}
	now := time.Now()
	for _, colony := range colonies {
		layout, ok := app.loadPlanetLayout(ctx, ch.CharacterID, colony.PlanetID)
		if !ok {
			continue
		}
		for _, ex := range layoutExtractors(layout) {
			if !ex.ExpiryOK {
				continue
			}
			if ex.Expiry.Before(now) {
				summary.Expired++
			}
			if !summary.SoonestOK || ex.Expiry.Before(summary.Soonest) {
				summary.Soonest, summary.SoonestOK = ex.Expiry, true
			}
		}
	}
	return summary
}

// planetDisplayName renders a colony planet: the resolved name
// when the cache has one (in-process place cache, then the
// durable planet_names table), the honest id fallback until
// then. An unresolved id is noted as a want so the worker
// resolves it in the background; the page never waits on it.
// Every PI surface — colonies page, home widget, attention and
// briefing lines — renders planets through here.
func (app *Application) planetDisplayName(ctx context.Context, planetID int64) string {
	if name, ok := app.esi.CachedPlanetName(ctx, planetID); ok && name != "" {
		return name
	}
	app.notePlanetIDs(ctx, planetID)
	return fmt.Sprintf("Planet #%d", planetID)
}

// ---------------------------------------------------------------------------
// View models.
// ---------------------------------------------------------------------------

// extractorRow is one extractor line of a colony.
type extractorRow struct {
	Product       string
	ProductTypeID int64
	Qty           string
	Cycle         string
	Heads         int
	Expires       string // formatted expiry
	FinishRaw     string // RFC3339, drives the live countdown
	Left          string // "in 2d 3h" while running
	Expired       bool
}

// factoryRow is one factory line of a colony.
type factoryRow struct {
	Schematic string
	PinType   string
	PinTypeID int64
	Cycle     string // schematic cycle time, when the cache has it
}

// colonyRow is one colony block: planet identity plus the
// distilled extractor/factory detail of its warmed layout.
type colonyRow struct {
	Planet        string
	System        string
	SystemRef     placeRef // System classified for the link policy
	Type          string   // planet type, display-cased
	Upgrade       int64
	Pins          int64
	Updated       string // last_update, formatted
	LayoutWarming bool   // colonies row known, layout not yet
	Extractors    []extractorRow
	Factories     []factoryRow
	Others        string // tally of the remaining pins by type
}

// planetsView is the Planets page body for one character.
type planetsView struct {
	CharacterName string
	Colonies      econSectionState
	Rows          []colonyRow
}

func (app *Application) handlePlanets(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.page(ctx)

	_, active, links, err := app.pickCharacter(ctx, r, "/planets/")
	if err != nil {
		logging.Errorf("planets: list characters: %v", err)
		data.Error = "Could not load planetary industry data; check the server log."
		app.render(ctx, w, http.StatusOK, "planets.html", data)
		return
	}
	if links == nil {
		app.render(ctx, w, http.StatusOK, "planets.html", data)
		return
	}
	data.PlanetsChars = links

	view := &planetsView{CharacterName: active.Name}
	data.Planets = view

	var colonies esi.Colonies
	if app.loadCorpSnapshot(ctx, active.CharacterID, esi.SnapPlanets, &colonies) {
		view.Colonies = econSectionState{Loaded: true}
	} else if app.piNotEnabled(ctx, active) {
		view.Colonies = econSectionState{Note: "Planetary industry is not enabled for this character yet — sign in again to re-link and approve the planetary scope. EVE's consent screen calls it “manage your planetary installations”; EveSynapse only ever reads your colonies."}
	} else if state, _, found := app.corpKindState(ctx, active.CharacterID, esi.SnapPlanets); found && state == fetchStateError {
		view.Colonies = econSectionState{Note: "Colony data could not be loaded — see the Sync page for status."}
	} else {
		view.Colonies = econSectionState{Warming: true}
	}
	if !view.Colonies.Loaded {
		app.render(ctx, w, http.StatusOK, "planets.html", data)
		return
	}

	now := time.Now()
	rows := make([]colonyRow, 0, len(colonies))
	for _, colony := range colonies {
		row := colonyRow{
			Planet:  app.planetDisplayName(ctx, colony.PlanetID),
			System:  app.locationTitle(ctx, colony.SolarSystemID, "solar_system"),
			Type:    displayPlanetType(colony.PlanetType),
			Upgrade: colony.UpgradeLevel,
			Pins:    colony.NumPins,
			Updated: formatFinish(colony.LastUpdate),
		}
		row.SystemRef = app.linkPlace(ctx, colony.SolarSystemID, row.System)
		layout, ok := app.loadPlanetLayout(ctx, active.CharacterID, colony.PlanetID)
		if !ok {
			row.LayoutWarming = true
			rows = append(rows, row)
			continue
		}
		row.Extractors, row.Factories, row.Others = app.colonyLayoutRows(ctx, layout, now)
		rows = append(rows, row)
	}
	// System, then planet: the way a pilot scans a PI alt.
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].System != rows[j].System {
			return rows[i].System < rows[j].System
		}
		return rows[i].Planet < rows[j].Planet
	})
	view.Rows = rows

	app.render(ctx, w, http.StatusOK, "planets.html", data)
}

// colonyLayoutRows distills one layout into display rows: the
// extractors (with expiry state), the factories (schematic names
// from the warmed schematic cache — ESI exposes no input/output
// bill for a schematic, so none is invented), and a tally of the
// remaining pins by type.
func (app *Application) colonyLayoutRows(ctx context.Context, layout esi.PlanetLayout, now time.Time) ([]extractorRow, []factoryRow, string) {
	var extractors []extractorRow
	for _, ex := range layoutExtractors(layout) {
		row := extractorRow{
			Product:       app.typeNameOrID(ctx, ex.ProductID),
			ProductTypeID: ex.ProductID,
			Qty:           esi.FormatInt(ex.QtyPerCycle),
			Cycle:         humanDuration(time.Duration(ex.CycleTime) * time.Second),
			Heads:         ex.Heads,
		}
		if ex.ExpiryOK {
			row.Expires = formatFinish(ex.Expiry.UTC().Format(time.RFC3339))
			row.FinishRaw = ex.Expiry.UTC().Format(time.RFC3339)
			if ex.Expiry.Before(now) {
				row.Expired = true
			} else {
				row.Left = "in " + humanDuration(time.Until(ex.Expiry))
			}
		}
		extractors = append(extractors, row)
	}

	var factories []factoryRow
	tally := make(map[string]int)
	for _, pin := range layout.Pins {
		switch {
		case pin.ExtractorDetails != nil:
			// Already rendered above.
		case pin.FactoryDetails != nil || pin.SchematicID > 0:
			schematicID := pin.SchematicID
			if pin.FactoryDetails != nil && pin.FactoryDetails.SchematicID > 0 {
				schematicID = pin.FactoryDetails.SchematicID
			}
			row := factoryRow{PinType: app.typeNameOrID(ctx, pin.TypeID), PinTypeID: pin.TypeID}
			if schematic, ok := app.esi.CachedSchematic(schematicID); ok && schematic.SchematicName != "" {
				row.Schematic = schematic.SchematicName
				if schematic.CycleTime > 0 {
					row.Cycle = humanDuration(time.Duration(schematic.CycleTime) * time.Second)
				}
			} else {
				row.Schematic = fmt.Sprintf("Schematic #%d", schematicID)
			}
			factories = append(factories, row)
		default:
			tally[app.typeNameOrID(ctx, pin.TypeID)]++
		}
	}
	sort.Slice(factories, func(i, j int) bool { return factories[i].Schematic < factories[j].Schematic })

	var others []string
	if len(tally) > 0 {
		names := make([]string, 0, len(tally))
		for name := range tally {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if tally[name] > 1 {
				others = append(others, fmt.Sprintf("%s ×%d", name, tally[name]))
			} else {
				others = append(others, name)
			}
		}
	}
	return extractors, factories, strings.Join(others, " · ")
}

// displayPlanetType renders ESI's lowercase planet-type enum the
// way the game capitalizes it.
func displayPlanetType(raw string) string {
	if raw == "" {
		return ""
	}
	return strings.ToUpper(raw[:1]) + raw[1:]
}

// piNotEnabled reports whether PI is dark for this character
// because its login predates the planetary scope: a recorded
// colonies refusal stands and the granted scopes still lack the
// scope. A character that re-linked (scope present) is never
// flagged — the worker retries within the cycle.
func (app *Application) piNotEnabled(ctx context.Context, ch db.Character) bool {
	if characterHasScope(ch, planetScope) {
		return false
	}
	state, detail, found := app.corpKindState(ctx, ch.CharacterID, esi.SnapPlanets)
	return found && state == fetchStateError && strings.HasPrefix(detail, piScopeDetail)
}
