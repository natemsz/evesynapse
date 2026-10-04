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
	if want, _ := strconv.ParseInt(r.URL.Query().Get("character"), 10, 64); want != 0 && pick(want) {
		// An explicit pick becomes the session's acting
		// character, so the header switcher and every other
		// page agree on who is being viewed.
		app.sessions.Put(ctx, sessionCharacterID, int(active.CharacterID))
		app.sessions.Put(ctx, sessionCharacterName, active.Name)
	} else if sid := int64(app.sessions.GetInt(ctx, sessionCharacterID)); sid == 0 || !pick(sid) {
		active = characters[0]
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
// structures show the name the worker resolved for them once it
// lands (structures.go); until then the same "#<id>" fallback.
func (app *Application) locationTitle(ctx context.Context, id int64, locType string) string {
	if name, ok := app.esi.CachedPlaceName(ctx, id); ok {
		return name
	}
	switch locType {
	case "station":
		app.notePageWantFromContext(ctx, pageWantPlace, id)
		return fmt.Sprintf("Station #%d", id)
	case "structure":
		if name := app.resolvedStructureTitle(ctx, id); name != "" {
			return name
		}
		// Join the background structure-name queue (when this is
		// a page render) so the name lands without a click.
		app.notePageWantFromContext(ctx, pageWantStructure, id)
		return fmt.Sprintf("Structure #%d", id)
	default:
		app.notePageWantFromContext(ctx, pageWantPlace, id)
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
	Name     string       // pilot-given clone name, "" when unnamed
	Location string       // resolved location title
	Implants []implantRow // implants in slot order; empty = no implants
}

// implantRow is one active-implant line; Slot is 1-based (EVE
// implant slots run 1..10, the ESI array is in slot order).
type implantRow struct {
	Slot   int
	Name   string
	TypeID int64
}

// characterView is the Character page body for one character. Each
// section carries its own Known flag so one missing snapshot dims
// only its own block. Loaded is true when any section loaded;
// Warming marks the cold-start case (no snapshots at all yet).
type characterView struct {
	CharacterID   int64
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
	ShipTypeID    int64
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

	// Identity (profile snapshot): the name/birthday/security/
	// corporation block the old home sheet fetched live. Since
	// Phase 1B it comes from the worker-warmed profile snapshot,
	// so this page — like every other — renders cache-only.
	IdentityKnown  bool
	PortraitURL    string
	CorpName       string
	Birthday       string
	SecurityStatus string

	// Wallet snapshot.
	WalletKnown bool
	ISK         string

	// Skills + queue snapshots (the rest of the old home sheet).
	SkillsKnown    bool
	TotalSP        string
	UnallocatedSP  string
	Skills         []skillRow
	SkillsShown    int
	SkillsCount    int
	QueueKnown     bool
	Training       string // "Skill V — finishes …", "" = not training
	TrainingFinish string // raw finish, drives the live countdown
	TrainingLeft   string

	// Planetary industry summary (Phase 2): colony count plus
	// the soonest extractor expiry, from the colonies + layout
	// snapshots. PINotEnabled marks the recorded scope refusal.
	PIKnown      bool
	PINotEnabled bool
	PIColonies   int
	PIExpired    int
	PISoonest    string // formatted soonest extractor expiry
	PISoonestIn  string // "in 2d 3h" while it is still ahead
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
		app.render(ctx, w, http.StatusOK, "character.html", data)
		return
	}
	if links == nil {
		app.render(ctx, w, http.StatusOK, "character.html", data)
		return
	}
	data.CharChars = links

	view := &characterView{CharacterID: active.CharacterID, CharacterName: active.Name}
	view.PortraitURL = portraitURL(active.CharacterID, 128)
	data.CharacterPage = view
	app.fillCharacterView(ctx, active, view)

	app.render(ctx, w, http.StatusOK, "character.html", data)
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
		view.ShipTypeID = ship.ShipTypeID
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
			view.Implants = append(view.Implants, implantRow{Slot: i + 1, Name: implantName(id), TypeID: id})
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
			for i, id := range jc.Implants {
				jcv.Implants = append(jcv.Implants, implantRow{Slot: i + 1, Name: implantName(id), TypeID: id})
			}
			view.JumpClones = append(view.JumpClones, jcv)
		}
	}

	// Identity: the profile snapshot the worker now warms (Phase
	// 1B) — name/birthday/security/corporation. This is the block
	// the old home sheet fetched live at render; it is a plain
	// snapshot read here like everything else. The corporation
	// name resolves from the cached corp record (warmed by the
	// worker), falling back to the recorded corp id.
	var profile esi.Character
	if app.loadCorpSnapshot(ctx, ch.CharacterID, esi.SnapProfile, &profile) {
		view.IdentityKnown = true
		if len(profile.Birthday) >= 10 {
			view.Birthday = profile.Birthday[:10]
		}
		view.SecurityStatus = fmt.Sprintf("%.2f", profile.SecurityStatus)
		corpID := profile.CorporationID
		if corpID == 0 {
			if mapping, err := app.queries.GetCharacterCorporation(ctx, ch.CharacterID); err == nil {
				corpID = mapping.CorporationID
			}
		}
		if corpID > 0 {
			// The worker warms this character's own corp_info
			// snapshot, so the name is a local read; fall back
			// to the recorded id until it lands.
			var info esi.Corporation
			if app.loadCorpSnapshot(ctx, ch.CharacterID, esi.SnapCorpInfo, &info) && info.Name != "" {
				view.CorpName = info.Name
			} else {
				view.CorpName = fmt.Sprintf("Corporation #%d", corpID)
			}
		}
	}

	// Wallet.
	var balance float64
	if app.loadCorpSnapshot(ctx, ch.CharacterID, esi.SnapWallet, &balance) {
		view.WalletKnown = true
		view.ISK = esi.FormatISK(balance)
	}

	// Skills: totals plus the heaviest 25, same shape the old
	// home sheet showed.
	var skills esi.Skills
	if app.loadCorpSnapshot(ctx, ch.CharacterID, esi.SnapSkills, &skills) {
		view.SkillsKnown = true
		view.TotalSP = esi.FormatInt(skills.TotalSP)
		if skills.UnallocatedSP > 0 {
			view.UnallocatedSP = esi.FormatInt(skills.UnallocatedSP)
		}
		view.SkillsCount = len(skills.Skills)
		ids := esi.SortedSkillIDs(skills.Skills)
		shown := ids
		if len(shown) > 25 {
			shown = shown[:25]
		}
		names := app.esi.CachedTypeNames(ctx, shown)
		byID := make(map[int64]esi.Skill, len(skills.Skills))
		for _, s := range skills.Skills {
			byID[s.SkillID] = s
		}
		for _, id := range shown {
			s := byID[id]
			name, ok := names[id]
			if !ok {
				name = fmt.Sprintf("Type #%d", id)
			}
			view.Skills = append(view.Skills, skillRow{
				Name:    name,
				TypeID:  id,
				Trained: esi.RomanLevel(s.TrainedSkillLevel),
				Active:  esi.RomanLevel(s.ActiveSkillLevel),
				SP:      esi.FormatInt(s.SkillpointsInSkill),
			})
		}
		view.SkillsShown = len(view.Skills)
	}

	// Queue: the currently-training line (position 0). An empty
	// queue is a valid state: QueueKnown true, Training "".
	var queue esi.Skillqueue
	if app.loadCorpSnapshot(ctx, ch.CharacterID, esi.SnapSkillqueue, &queue) {
		view.QueueKnown = true
		for _, entry := range queue {
			if entry.QueuePosition != 0 {
				continue
			}
			name := app.esi.CachedTypeName(ctx, entry.SkillID)
			if name == "" {
				name = fmt.Sprintf("Type #%d", entry.SkillID)
			}
			finish := entry.FinishDate
			if t, err := time.Parse(time.RFC3339, entry.FinishDate); err == nil {
				finish = t.UTC().Format("2006-01-02 15:04 UTC")
				view.TrainingFinish = t.UTC().Format(time.RFC3339)
				if left := time.Until(t); left > 0 {
					view.TrainingLeft = "in " + humanDuration(left)
				} else {
					view.TrainingLeft = "done"
				}
			}
			if finish != "" {
				view.Training = fmt.Sprintf("%s %s — finishes %s", name, esi.RomanLevel(entry.FinishedLevel), finish)
			} else {
				view.Training = fmt.Sprintf("%s %s", name, esi.RomanLevel(entry.FinishedLevel))
			}
			break
		}
	}

	// Planetary industry summary: colonies + soonest extractor
	// expiry from the PI snapshots (worker-warmed; the section
	// links to the full colonies page).
	pi := app.characterPISummary(ctx, ch)
	view.PIKnown = pi.Known
	view.PINotEnabled = pi.NotEnabled
	view.PIColonies = pi.Colonies
	view.PIExpired = pi.Expired
	if pi.SoonestOK {
		view.PISoonest = formatFinish(pi.Soonest.UTC().Format(time.RFC3339))
		if left := time.Until(pi.Soonest); left > 0 {
			view.PISoonestIn = "in " + humanDuration(left)
		}
	}

	view.Loaded = view.OnlineKnown || view.LocationKnown || view.ShipKnown ||
		view.FatigueKnown || view.ImplantsKnown || view.ClonesKnown ||
		view.IdentityKnown || view.WalletKnown || view.SkillsKnown || view.QueueKnown ||
		view.PIKnown || view.PINotEnabled
	if !view.Loaded {
		// No section produced data: cold start (worker still
		// importing this character's live-state snapshots) gets
		// the warming copy, anything else the generic note.
		sectionKinds := map[string]bool{
			esi.SnapLocation: true, esi.SnapShip: true, esi.SnapOnline: true,
			esi.SnapClones: true, esi.SnapImplants: true, esi.SnapFatigue: true,
			esi.SnapProfile: true, esi.SnapWallet: true, esi.SnapSkills: true,
			esi.SnapSkillqueue: true,
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
	// A type the SDE lacks can still be named by its ESI type
	// payload; on a page that becomes a current-page want.
	app.notePageWantFromContext(ctx, pageWantTypeDescription, id)
	return fmt.Sprintf("Type #%d", id)
}
