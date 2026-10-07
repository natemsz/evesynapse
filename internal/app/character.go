package app

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
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
			Tags:   ch.Tags,
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
	CloneID     int64        // ESI jump_clone_id, targets rename posts
	Num         int          // 1-based position ("Jump Clone 1")
	Name        string       // ESI clone name, "" when unnamed
	CustomName  string       // pilot-given name from clone_names, "" when none
	DisplayName string       // CustomName, else Name, else "Jump Clone N"
	Location    string       // resolved location title
	Implants    []implantRow // implants in slot order; empty = no implants
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
	SystemID      int64    // links the system name to its page
	SystemSec     string   // "0.9", "" when the SDE lacks the system
	DockedAt      string   // station/structure title, "" when in space
	DockedRef     placeRef // DockedAt classified for the link policy (station, structure, or plain text)
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
	CorpID         int64
	CorpName       string
	Birthday       string
	SecurityStatus string

	// Wallet snapshot.
	WalletKnown bool
	ISK         string

	// Skills + queue snapshots: the full skill sheet now lives
	// here (the standalone /skills/ page redirects). Groups are
	// per-category, heaviest skill first; the queue carries the
	// training/queued state for the 5-box level indicators.
	SkillsKnown     bool
	TotalSP         string
	UnallocatedSP   string
	SkillsCount     int
	LevelVCount     int
	SkillGroups     []skillGroupSection
	QueueKnown      bool
	Training        string // "Skill V — finishes …", "" = not training
	TrainingFinish  string // raw finish, drives the live countdown
	TrainingLeft    string
	TrainingSkillID int64 // skill currently training, 0 when idle
	TrainingLevel   int   // level currently training
	Queue           []skillQueueRow
	CompletesAt     string // finish time of the last queue entry

	// Browse (skill catalog from the SDE graph, with plan
	// quick-add forms); BrowseWarming marks the pre-import state.
	Browse        []browseSkillGroup
	BrowseWarming bool
	Plans         []skillPlanSummary

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
		logging.Errorf("character: list characters: %v", err)
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
	userID := int64(app.sessions.GetInt(ctx, sessionUserID))

	app.fillCharacterStatus(ctx, ch, view)
	app.fillCharacterLocation(ctx, ch, view)
	app.fillCharacterShip(ctx, ch, view)
	app.fillCharacterFatigue(ctx, ch, view)
	app.fillCharacterImplantsAndClones(ctx, ch, view, userID)
	app.fillCharacterIdentity(ctx, ch, view)
	app.fillCharacterWallet(ctx, ch, view)
	trainedByID := app.fillCharacterSkills(ctx, ch, view, userID)
	app.fillCharacterQueue(ctx, ch, view, trainedByID)
	app.fillCharacterPI(ctx, ch, view)
	app.fillCharacterLoaded(ctx, ch, view)
}

// fillCharacterStatus: online state and login history.
func (app *Application) fillCharacterStatus(ctx context.Context, ch db.Character, view *characterView) {
	// Status.
	var online esi.Online
	if app.loadCorpSnapshot(ctx, ch.CharacterID, esi.SnapOnline, &online) {
		view.OnlineKnown = true
		view.Online = online.Online
		view.LastLogin = formatFinish(online.LastLogin)
		view.LastLogout = formatFinish(online.LastLogout)
		view.Logins = esi.FormatInt(online.Logins)
	}
}

// fillCharacterLocation: the system the character is in and where
// they are docked.
func (app *Application) fillCharacterLocation(ctx context.Context, ch db.Character, view *characterView) {
	// Location.
	var loc esi.Location
	if app.loadCorpSnapshot(ctx, ch.CharacterID, esi.SnapLocation, &loc) {
		view.LocationKnown = true
		view.SystemName = app.locationTitle(ctx, loc.SolarSystemID, "solar_system")
		if sys, err := app.queries.GetSDESystem(ctx, loc.SolarSystemID); err == nil {
			view.SystemSec = fmt.Sprintf("%.1f", sys.Security)
			// The system name links to its page only while the
			// SDE can render that page; until then it stays text.
			view.SystemID = loc.SolarSystemID
		}
		if loc.StationID > 0 {
			view.DockedAt = app.locationTitle(ctx, loc.StationID, "station")
			view.DockedRef = app.linkPlace(ctx, loc.StationID, view.DockedAt)
		} else if loc.StructureID > 0 {
			view.DockedAt = app.locationTitle(ctx, loc.StructureID, "structure")
			view.DockedRef = app.linkPlace(ctx, loc.StructureID, view.DockedAt)
		}
	}
}

// fillCharacterShip: the ship being flown.
func (app *Application) fillCharacterShip(ctx context.Context, ch db.Character, view *characterView) {
	// Ship.
	var ship esi.Ship
	if app.loadCorpSnapshot(ctx, ch.CharacterID, esi.SnapShip, &ship) {
		view.ShipKnown = true
		view.ShipTypeName = app.typeNameOrID(ctx, ship.ShipTypeID)
		view.ShipTypeID = ship.ShipTypeID
		view.ShipName = ship.ShipName
	}
}

// fillCharacterFatigue: jump fatigue and when it clears.
func (app *Application) fillCharacterFatigue(ctx context.Context, ch db.Character, view *characterView) {
	// Jump fatigue.
	var fatigue esi.Fatigue
	if app.loadCorpSnapshot(ctx, ch.CharacterID, esi.SnapFatigue, &fatigue) {
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
}

// fillCharacterImplantsAndClones: the active implants and the jump
// clones with theirs. userID picks whose clone names to show.
func (app *Application) fillCharacterImplantsAndClones(ctx context.Context, ch db.Character, view *characterView, userID int64) {
	// Implants and clones feed one name-resolution pass so implant
	// names come from a single cache lookup.
	var implants esi.Implants
	implantsLoaded := false
	if app.loadCorpSnapshot(ctx, ch.CharacterID, esi.SnapImplants, &implants) {
		implantsLoaded = true
	}
	var clones esi.Clones
	clonesLoaded := false
	if app.loadCorpSnapshot(ctx, ch.CharacterID, esi.SnapClones, &clones) {
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
		// Pilot-given clone names (ESI has no naming API).
		customNames := map[int64]string{}
		if userID != 0 {
			if rows, err := app.queries.ListCloneNames(ctx, db.ListCloneNamesParams{UserID: userID, CharacterID: ch.CharacterID}); err != nil {
				logging.Errorf("character: clone names for character %d: %v", ch.CharacterID, err)
			} else {
				for _, r := range rows {
					customNames[r.CloneID] = r.CustomName
				}
			}
		}
		for i, jc := range clones.JumpClones {
			jcv := jumpCloneView{
				CloneID:  jc.JumpCloneID,
				Num:      i + 1,
				Name:     jc.Name,
				Location: app.locationTitle(ctx, jc.LocationID, jc.LocationType),
			}
			if cn, ok := customNames[jc.JumpCloneID]; ok && cn != "" {
				jcv.CustomName = cn
			}
			switch {
			case jcv.CustomName != "":
				jcv.DisplayName = jcv.CustomName
			case jcv.Name != "":
				jcv.DisplayName = jcv.Name
			default:
				jcv.DisplayName = fmt.Sprintf("Jump Clone %d", jcv.Num)
			}
			for i, id := range jc.Implants {
				jcv.Implants = append(jcv.Implants, implantRow{Slot: i + 1, Name: implantName(id), TypeID: id})
			}
			view.JumpClones = append(view.JumpClones, jcv)
		}
	}
}

// fillCharacterIdentity: birthday, security status and corporation.
func (app *Application) fillCharacterIdentity(ctx context.Context, ch db.Character, view *characterView) {
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
			view.CorpID = corpID
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
}

// fillCharacterWallet: the wallet balance.
func (app *Application) fillCharacterWallet(ctx context.Context, ch db.Character, view *characterView) {
	// Wallet.
	var balance float64
	if app.loadCorpSnapshot(ctx, ch.CharacterID, esi.SnapWallet, &balance) {
		view.WalletKnown = true
		view.ISK = esi.FormatISK(balance)
	}
}

// fillCharacterSkills: the skill sheet and, for a signed-in user, the
// browse catalog. It returns each trained skill's level, which the
// queue section marks its rows with.
func (app *Application) fillCharacterSkills(ctx context.Context, ch db.Character, view *characterView, userID int64) map[int64]int {
	// Skills: the full sheet (per-category groups) now lives on
	// this page; the builders in skills.go do the heavy lifting
	// against a throwaway skillsView, and this block copies the
	// results onto the character view.
	var skills esi.Skills
	trainedByID := map[int64]int{}
	if app.loadCorpSnapshot(ctx, ch.CharacterID, esi.SnapSkills, &skills) {
		sv := &skillsView{CharacterName: ch.Name, CharacterID: ch.CharacterID}
		app.fillSkillSections(ctx, sv, skills)
		view.SkillsKnown = true
		view.TotalSP = sv.TotalSP
		view.UnallocatedSP = sv.UnallocatedSP
		view.SkillsCount = sv.SkillsKnown
		view.LevelVCount = sv.LevelVCount
		view.SkillGroups = sv.Groups
		for _, s := range skills.Skills {
			trainedByID[s.SkillID] = s.TrainedSkillLevel
		}
		// Browse catalog (plan quick-add) rides along when the
		// user is known.
		if userID != 0 {
			bv := &skillsView{CharacterName: ch.Name, CharacterID: ch.CharacterID}
			app.fillBrowse(ctx, bv, ch, skills, userID)
			view.Browse = bv.Browse
			view.BrowseWarming = bv.BrowseWarming
			view.Plans = bv.Plans
		}
	}
	return trainedByID
}

// fillCharacterQueue: the skill queue and what is training now.
func (app *Application) fillCharacterQueue(ctx context.Context, ch db.Character, view *characterView, trainedByID map[int64]int) {
	// Queue: the full table plus the currently-training line
	// (position 0). An empty queue is a valid state: QueueKnown
	// true, Training "". Rows are enriched with trained levels
	// and training/queued state for the 5-box indicators, and the
	// matching group rows get the same markers.
	var queue esi.Skillqueue
	if app.loadCorpSnapshot(ctx, ch.CharacterID, esi.SnapSkillqueue, &queue) {
		view.QueueKnown = true
		qv := &skillsView{}
		app.fillQueue(ctx, qv, queue)
		view.Queue = qv.Queue
		view.Training = qv.Training
		view.CompletesAt = qv.CompletesAt
		queued := map[int64]int{}
		for i := range view.Queue {
			row := &view.Queue[i]
			row.TrainedLvl = trainedByID[row.SkillID]
			if row.Num == 1 {
				row.State = "training"
				view.TrainingSkillID = row.SkillID
				view.TrainingLevel = row.LevelNum
			} else {
				row.State = "queued"
			}
			queued[row.SkillID] = row.LevelNum
		}
		for gi := range view.SkillGroups {
			for si := range view.SkillGroups[gi].Skills {
				sr := &view.SkillGroups[gi].Skills[si]
				switch {
				case sr.TypeID == view.TrainingSkillID && view.TrainingSkillID != 0:
					sr.NextState = "training"
					sr.NextLvl = view.TrainingLevel
				case queued[sr.TypeID] > 0:
					sr.NextState = "queued"
					sr.NextLvl = queued[sr.TypeID]
				}
			}
		}
		for _, entry := range queue {
			if entry.QueuePosition != 0 {
				continue
			}
			if t, err := time.Parse(time.RFC3339, entry.FinishDate); err == nil {
				view.TrainingFinish = t.UTC().Format(time.RFC3339)
				if left := time.Until(t); left > 0 {
					view.TrainingLeft = "in " + humanDuration(left)
				} else {
					view.TrainingLeft = "done"
				}
			}
			break
		}
	}
}

// fillCharacterPI: the planetary industry summary.
func (app *Application) fillCharacterPI(ctx context.Context, ch db.Character, view *characterView) {
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
}

// fillCharacterLoaded: whether any section produced data and, when
// none did, whether that is because the worker is still warming
// this character.
func (app *Application) fillCharacterLoaded(ctx context.Context, ch db.Character, view *characterView) {
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
		if snaps, err := app.queries.ListSnapshotMetaByCharacter(ctx, ch.CharacterID); err == nil {
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

// ---------------------------------------------------------------------------
// Clone renaming: ESI exposes no custom clone names, so the sheet
// stores pilot-given labels in clone_names (keyed by the ESI
// jump_clone_id). An empty name clears the stored label.
// ---------------------------------------------------------------------------

// handleCloneRename stores or clears a pilot-given name for one of
// the signed-in user's jump clones, then returns to the character
// sheet's clones section.
func (app *Application) handleCloneRename(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := int64(app.sessions.GetInt(ctx, sessionUserID))
	if err := r.ParseForm(); err != nil || userID == 0 {
		http.Redirect(w, r, "/character/", http.StatusSeeOther)
		return
	}
	characterID, _ := strconv.ParseInt(r.FormValue("character"), 10, 64)
	cloneID, _ := strconv.ParseInt(r.FormValue("clone"), 10, 64)
	name := strings.TrimSpace(r.FormValue("name"))
	if len([]rune(name)) > 60 {
		name = string([]rune(name)[:60])
	}

	// The clone must belong to a character this user owns.
	owned := false
	if characters, err := app.queries.ListCharactersByUser(ctx, userID); err == nil {
		for _, ch := range characters {
			if ch.CharacterID == characterID {
				owned = true
				break
			}
		}
	} else {
		logging.Errorf("character: rename clone list characters for user %d: %v", userID, err)
	}
	back := fmt.Sprintf("/character/?character=%d#clones", characterID)
	if !owned || characterID == 0 || cloneID == 0 {
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}

	if name == "" {
		if err := app.queries.DeleteCloneName(ctx, db.DeleteCloneNameParams{
			UserID: userID, CharacterID: characterID, CloneID: cloneID,
		}); err != nil {
			logging.Errorf("character: delete clone name: %v", err)
		}
	} else if err := app.queries.UpsertCloneName(ctx, db.UpsertCloneNameParams{
		UserID: userID, CharacterID: characterID, CloneID: cloneID, CustomName: name,
	}); err != nil {
		logging.Errorf("character: upsert clone name: %v", err)
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}
