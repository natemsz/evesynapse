package app

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// ---------------------------------------------------------------------------
// Shared per-character page plumbing (module sweep). The Character,
// Fittings and Killmails pages all switch characters the way Assets
// and Skills do; the helpers here keep that logic in one place.
// ---------------------------------------------------------------------------

// pickCharacter resolves the character-switcher state for a
// per-character page: the signed-in user's characters, the active
// one (an explicit ?character= the user owns wins, then the session
// character, then the first linked character), and the switcher
// links pointing at path. A nil slice with a nil error means the
// session carries no user or the user has no characters yet.
func (app *Application) pickCharacter(ctx context.Context, r *http.Request, path string) ([]db.Character, db.Character, []assetCharLink, error) {
	userID := int64(app.sessions.GetInt(ctx, sessionUserID))
	if userID == 0 {
		// Dev-login sessions carry no user; nothing to show.
		return nil, db.Character{}, nil, nil
	}

	characters, err := app.queries.ListCharactersByUser(ctx, userID)
	if err != nil {
		return nil, db.Character{}, nil, err
	}
	if len(characters) == 0 {
		return nil, db.Character{}, nil, nil
	}

	active := characters[0]
	pick := func(id int64) bool {
		for _, ch := range characters {
			if ch.CharacterID == id {
				active = ch
				return true
			}
		}
		return false
	}
	if want, _ := strconv.ParseInt(r.URL.Query().Get("character"), 10, 64); want == 0 || !pick(want) {
		if sid := int64(app.sessions.GetInt(ctx, sessionCharacterID)); sid == 0 || !pick(sid) {
			active = characters[0]
		}
	}

	links := make([]assetCharLink, 0, len(characters))
	for _, ch := range characters {
		links = append(links, assetCharLink{
			ID:     ch.CharacterID,
			Name:   ch.Name,
			Active: ch.CharacterID == active.CharacterID,
		})
	}
	return characters, active, links, nil
}

// locationTitle renders a station, structure, or solar-system
// location the way the character pages show it: the SDE/cache name
// when known, an honest "#<id>" fallback otherwise. Player
// structures resolve only when a name was cached by other means —
// their names need an ESI scope this app does not hold.
func (app *Application) locationTitle(ctx context.Context, id int64, locType string) string {
	if name, ok := app.esi.CachedPlaceName(ctx, id); ok {
		return name
	}
	switch locType {
	case "station":
		return fmt.Sprintf("Station #%d", id)
	case "structure":
		return fmt.Sprintf("Structure #%d", id)
	default:
		return fmt.Sprintf("System #%d", id)
	}
}

// humanDuration renders a duration compactly for "time remaining"
// labels: "2d 3h", "3h 12m", "45m", "<1m". Non-positive durations
// render "<1m"; callers that care check the sign themselves.
func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return "<1m"
	}
	days := int(d / (24 * time.Hour))
	hours := int((d % (24 * time.Hour)) / time.Hour)
	mins := int((d % time.Hour) / time.Minute)
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, mins)
	default:
		return fmt.Sprintf("%dm", mins)
	}
}

// ---------------------------------------------------------------------------
// Character page: live state (location/ship/online), jump fatigue,
// clones and implants for one character, all from snapshots the
// worker keeps warm. Every section degrades independently, exactly
// like the home sheet.
// ---------------------------------------------------------------------------

// jumpCloneView is one jump clone line of the Character page.
type jumpCloneView struct {
	Name     string   // pilot-given clone name, "" when unnamed
	Location string   // resolved location title
	Implants []string // implant names in slot order; empty = no implants
}

// implantRow is one active-implant line; Slot is 1-based (EVE
// implant slots run 1..10, the ESI array is in slot order).
type implantRow struct {
	Slot int
	Name string
}

// characterView is the Character page body for one character. Each
// section carries its own Known flag so one missing snapshot dims
// only its own block. Loaded is true when any section loaded;
// Warming marks the cold-start case (no snapshots at all yet).
type characterView struct {
	CharacterName string
	Loaded        bool
	Warming       bool

	// Status (online snapshot).
	OnlineKnown bool
	Online      bool
	LastLogin   string
	LastLogout  string
	Logins      string

	// Location + ship snapshots.
	LocationKnown bool
	SystemName    string
	SystemSec     string // "0.9", "" when the SDE lacks the system
	DockedAt      string // station/structure title, "" when in space
	ShipKnown     bool
	ShipTypeName  string
	ShipName      string

	// Fatigue snapshot.
	FatigueKnown bool
	LastJump     string
	FatigueUntil string
	FatigueLeft  string // human remaining, "ready" once expired

	// Implants + clones snapshots.
	ImplantsKnown bool
	Implants      []implantRow // active implants in slot order
	ClonesKnown   bool
	HomeLocation  string
	LastCloneJump string
	JumpClones    []jumpCloneView
}

// handleCharacter renders the Character page for one of the
// signed-in user's characters (switchable via ?character=).
func (app *Application) handleCharacter(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := pageData{
		LoggedIn:      true,
		CharacterName: app.sessions.GetString(ctx, sessionCharacterName),
		SSOConfigured: app.cfg.SSOConfigured(),
	}

	_, active, links, err := app.pickCharacter(ctx, r, "/character/")
	if err != nil {
		log.Printf("character: list characters: %v", err)
		data.Error = "Could not load character data; check the server log."
		app.render(w, http.StatusOK, "character.html", data)
		return
	}
	if links == nil {
		app.render(w, http.StatusOK, "character.html", data)
		return
	}
	data.CharChars = links

	view := &characterView{CharacterName: active.Name}
	data.CharacterPage = view
	app.fillCharacterView(ctx, active, view)

	app.render(w, http.StatusOK, "character.html", data)
}

// fillCharacterView loads every Character-page section from the
// snapshot cache. A section whose snapshot can't be produced stays
// dimmed; the rest render.
func (app *Application) fillCharacterView(ctx context.Context, ch db.Character, view *characterView) {
	// Status.
	var online esi.Online
	if err := app.esi.GetCached(ctx, ch, esi.SnapOnline, &online); err != nil {
		log.Printf("character: online for character %d: %v", ch.CharacterID, err)
	} else {
		view.OnlineKnown = true
		view.Online = online.Online
		view.LastLogin = formatFinish(online.LastLogin)
		view.LastLogout = formatFinish(online.LastLogout)
		view.Logins = esi.FormatInt(online.Logins)
	}

	// Location.
	var loc esi.Location
	if err := app.esi.GetCached(ctx, ch, esi.SnapLocation, &loc); err != nil {
		log.Printf("character: location for character %d: %v", ch.CharacterID, err)
	} else {
		view.LocationKnown = true
		view.SystemName = app.locationTitle(ctx, loc.SolarSystemID, "solar_system")
		if sys, err := app.queries.GetSDESystem(ctx, loc.SolarSystemID); err == nil {
			view.SystemSec = fmt.Sprintf("%.1f", sys.Security)
		}
		if loc.StationID > 0 {
			view.DockedAt = app.locationTitle(ctx, loc.StationID, "station")
		} else if loc.StructureID > 0 {
			view.DockedAt = app.locationTitle(ctx, loc.StructureID, "structure")
		}
	}

	// Ship.
	var ship esi.Ship
	if err := app.esi.GetCached(ctx, ch, esi.SnapShip, &ship); err != nil {
		log.Printf("character: ship for character %d: %v", ch.CharacterID, err)
	} else {
		view.ShipKnown = true
		view.ShipTypeName = app.typeNameOrID(ctx, ship.ShipTypeID)
		view.ShipName = ship.ShipName
	}

	// Jump fatigue.
	var fatigue esi.Fatigue
	if err := app.esi.GetCached(ctx, ch, esi.SnapFatigue, &fatigue); err != nil {
		log.Printf("character: fatigue for character %d: %v", ch.CharacterID, err)
	} else {
		view.FatigueKnown = true
		view.LastJump = formatFinish(fatigue.LastJumpDate)
		view.FatigueUntil = formatFinish(fatigue.JumpFatigueExpireDate)
		if t, err := time.Parse(time.RFC3339, fatigue.JumpFatigueExpireDate); err == nil {
			if left := time.Until(t); left > 0 {
				view.FatigueLeft = humanDuration(left) + " remaining"
			} else {
				view.FatigueLeft = "ready"
			}
		}
	}

	// Implants and clones feed one name-resolution pass so implant
	// names come from a single cache lookup.
	var implants esi.Implants
	implantsLoaded := false
	if err := app.esi.GetCached(ctx, ch, esi.SnapImplants, &implants); err != nil {
		log.Printf("character: implants for character %d: %v", ch.CharacterID, err)
	} else {
		implantsLoaded = true
	}
	var clones esi.Clones
	clonesLoaded := false
	if err := app.esi.GetCached(ctx, ch, esi.SnapClones, &clones); err != nil {
		log.Printf("character: clones for character %d: %v", ch.CharacterID, err)
	} else {
		clonesLoaded = true
	}

	implantIDs := make([]int64, 0, len(implants))
	implantIDs = append(implantIDs, implants...)
	if clonesLoaded {
		for _, jc := range clones.JumpClones {
			implantIDs = append(implantIDs, jc.Implants...)
		}
	}
	implantNames := app.esi.CachedTypeNames(ctx, implantIDs)
	implantName := func(id int64) string {
		if name, ok := implantNames[id]; ok {
			return name
		}
		return fmt.Sprintf("Type #%d", id)
	}

	if implantsLoaded {
		view.ImplantsKnown = true
		for i, id := range implants {
			view.Implants = append(view.Implants, implantRow{Slot: i + 1, Name: implantName(id)})
		}
	}
	if clonesLoaded {
		view.ClonesKnown = true
		view.HomeLocation = app.locationTitle(ctx, clones.HomeLocation.LocationID, clones.HomeLocation.LocationType)
		view.LastCloneJump = formatFinish(clones.LastCloneJumpDate)
		for _, jc := range clones.JumpClones {
			jcv := jumpCloneView{
				Name:     jc.Name,
				Location: app.locationTitle(ctx, jc.LocationID, jc.LocationType),
			}
			for _, id := range jc.Implants {
				jcv.Implants = append(jcv.Implants, implantName(id))
			}
			view.JumpClones = append(view.JumpClones, jcv)
		}
	}

	view.Loaded = view.OnlineKnown || view.LocationKnown || view.ShipKnown ||
		view.FatigueKnown || view.ImplantsKnown || view.ClonesKnown
	if !view.Loaded {
		// No section produced data: cold start (worker still
		// importing this character's live-state snapshots) gets
		// the warming copy, anything else the generic note.
		sectionKinds := map[string]bool{
			esi.SnapLocation: true, esi.SnapShip: true, esi.SnapOnline: true,
			esi.SnapClones: true, esi.SnapImplants: true, esi.SnapFatigue: true,
		}
		if snaps, err := app.queries.ListSnapshotsByCharacter(ctx, ch.CharacterID); err == nil {
			any := false
			for _, snap := range snaps {
				if sectionKinds[snap.Kind] {
					any = true
					break
				}
			}
			view.Warming = !any
		}
	}
}

// typeNameOrID resolves one type ID from the local caches, falling
// back to the honest "Type #<id>" placeholder.
func (app *Application) typeNameOrID(ctx context.Context, id int64) string {
	if name := app.esi.CachedTypeName(ctx, id); name != "" {
		return name
	}
	return fmt.Sprintf("Type #%d", id)
}
