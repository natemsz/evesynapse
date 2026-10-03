package app

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// pageData is the view model shared by the templates.
type pageData struct {
	LoggedIn      bool
	CharacterName string
	SSOConfigured bool
	AutoRefresh   bool   // base.html emits a meta-refresh (Sync page)
	Error         string // friendly, user-safe banner (never internals)
	Character     *characterSheet
	Corps         []corpView
	Assets        *assetsView
	AssetsChars   []assetCharLink
	Market        *marketView
	Skills        *skillsView
	SkillsChars   []assetCharLink
	Sync          *syncView
	Users         []db.User
	Characters    []db.Character
	Snapshots     []adminSnapshotRow
	WorkerStatus  string
}

// characterSheet is what the home page shows for the signed-in
// character: identity from the session/DB, live data from ESI. The
// wallet/skills/queue blocks each carry their own OK flag so one
// failing ESI endpoint dims only its own section.
type characterSheet struct {
	Name            string
	PortraitURL     string
	CorporationName string
	Birthday        string // YYYY-MM-DD
	SecurityStatus  float64
	Fetched         bool // true when the live ESI data loaded
	Unavailable     bool // signed in, but ESI couldn't be reached

	// Wallet block.
	ISKOK bool
	ISK   string // formatted balance

	// Skills block.
	SkillsOK      bool
	TotalSP       string // formatted
	UnallocatedSP string // formatted, "" when zero
	Skills        []skillRow
	SkillsShown   int
	SkillsCount   int

	// Skill-queue block.
	QueueOK  bool
	Training string // "Skill Name V — finishes 2026-10-03 14:22 UTC"; "" = paused/empty
}

// skillRow is one line of the home-page skills table.
type skillRow struct {
	Name    string
	Trained string // trained level, roman
	Active  string // active level, roman
	SP      string // formatted
}

// adminSnapshotRow is one line of the admin snapshots overview.
type adminSnapshotRow struct {
	CharacterID   int64
	CharacterName string
	Kind          string
	FetchedAt     string
	CachedUntil   string // "—" when unset
}

func (app *Application) render(w http.ResponseWriter, status int, page string, data pageData) {
	ts, err := template.New("base").ParseFS(templatesFS, "templates/base.html", "templates/"+page)
	if err != nil {
		log.Printf("parse template %s: %v", page, err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	buf := new(bytes.Buffer)
	if err := ts.ExecuteTemplate(buf, "base", data); err != nil {
		log.Printf("execute template %s: %v", page, err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

// friendlyLoginError maps the ?error= codes produced by the SSO
// callback to safe, human messages. Unknown codes render nothing — the
// raw parameter is never echoed back to the page.
func friendlyLoginError(code string) string {
	switch code {
	case "denied":
		return "EVE login was cancelled or denied. You can try again whenever you're ready."
	case "state":
		return "That login attempt couldn't be verified — it may have expired. Please try logging in again."
	case "exchange", "verify":
		return "EVE login failed while completing sign-in. Please try again."
	case "save":
		return "You signed in at EVE, but saving your character failed here. Please try again."
	case "unconfigured":
		return "EVE SSO is not configured on this server yet."
	default:
		return ""
	}
}

func (app *Application) handleHome(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := pageData{
		LoggedIn:      app.sessions.GetBool(ctx, sessionAuthenticated),
		CharacterName: app.sessions.GetString(ctx, sessionCharacterName),
		SSOConfigured: app.cfg.SSOConfigured(),
		Error:         friendlyLoginError(r.URL.Query().Get("error")),
	}
	if data.LoggedIn {
		data.Character = app.loadCharacterSheet(ctx)
	}
	app.render(w, http.StatusOK, "home.html", data)
}

// loadCharacterSheet assembles the home-page character block.
//
// Identity comes from the public character endpoint; wallet, skills
// and skill queue come through the snapshot cache (getCached), which
// serves ESI-cached payloads and only calls out within ESI's cache
// window — the worker keeps those snapshots warm in the background.
// Every section degrades independently: a dead endpoint dims its own
// block, never the whole page.
func (app *Application) loadCharacterSheet(ctx context.Context) *characterSheet {
	sheet := &characterSheet{
		Name: app.sessions.GetString(ctx, sessionCharacterName),
	}

	characterID := int64(app.sessions.GetInt(ctx, sessionCharacterID))
	if characterID == 0 {
		// Dev-login or pre-SSO session: name only, nothing to fetch.
		return sheet
	}
	sheet.PortraitURL = fmt.Sprintf("https://images.evetech.net/characters/%d/portrait?size=128", characterID)

	character, err := app.queries.GetCharacter(ctx, characterID)
	if err != nil {
		log.Printf("home: load character %d: %v", characterID, err)
		sheet.Unavailable = true
		return sheet
	}
	if character.Name != "" {
		sheet.Name = character.Name
	}

	// One valid token for the whole page: this refreshes (and
	// persists the rotation) when the stored token is near expiry.
	token, err := app.validAccessToken(ctx, character)
	if err != nil {
		log.Printf("home: no valid token for character %d: %v", characterID, err)
		sheet.Unavailable = true
		return sheet
	}

	var pub esi.Character
	if err := app.esi.Get(ctx, token, fmt.Sprintf("/characters/%d/", characterID), &pub); err != nil {
		log.Printf("home: ESI character fetch for %d failed: %v", characterID, err)
		sheet.Unavailable = true
		return sheet
	}
	if pub.Name != "" {
		sheet.Name = pub.Name
	}
	sheet.SecurityStatus = pub.SecurityStatus
	if len(pub.Birthday) >= 10 {
		sheet.Birthday = pub.Birthday[:10]
	}

	// Corporation name is a separate public endpoint; a failure here
	// just leaves the field blank, the rest of the sheet still renders.
	var corp esi.Corporation
	if err := app.esi.Get(ctx, "", fmt.Sprintf("/corporations/%d/", pub.CorporationID), &corp); err == nil {
		sheet.CorporationName = corp.Name
	} else {
		log.Printf("home: ESI corporation fetch for %d failed: %v", pub.CorporationID, err)
	}

	sheet.Fetched = true
	app.loadWalletSection(ctx, character, sheet)
	app.loadSkillsSection(ctx, character, sheet)
	app.loadQueueSection(ctx, character, sheet)
	return sheet
}

// loadWalletSection fills the wallet block; failures only dim it.
func (app *Application) loadWalletSection(ctx context.Context, ch db.Character, sheet *characterSheet) {
	var balance float64
	if err := app.esi.GetCached(ctx, ch, esi.SnapWallet, &balance); err != nil {
		log.Printf("home: wallet for character %d: %v", ch.CharacterID, err)
		return
	}
	sheet.ISK = esi.FormatISK(balance)
	sheet.ISKOK = true
}

// loadSkillsSection fills the skills block: totals plus the heaviest
// 25 skills, names resolved via the type-name cache.
func (app *Application) loadSkillsSection(ctx context.Context, ch db.Character, sheet *characterSheet) {
	var skills esi.Skills
	if err := app.esi.GetCached(ctx, ch, esi.SnapSkills, &skills); err != nil {
		log.Printf("home: skills for character %d: %v", ch.CharacterID, err)
		return
	}

	sheet.TotalSP = esi.FormatInt(skills.TotalSP)
	if skills.UnallocatedSP > 0 {
		sheet.UnallocatedSP = esi.FormatInt(skills.UnallocatedSP)
	}
	sheet.SkillsCount = len(skills.Skills)

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
		sheet.Skills = append(sheet.Skills, skillRow{
			Name:    name,
			Trained: esi.RomanLevel(s.TrainedSkillLevel),
			Active:  esi.RomanLevel(s.ActiveSkillLevel),
			SP:      esi.FormatInt(s.SkillpointsInSkill),
		})
	}
	sheet.SkillsShown = len(sheet.Skills)
	sheet.SkillsOK = true
}

// loadQueueSection fills the "currently training" line from the first
// queue entry (position 0). An empty queue is a valid state, not an
// error: QueueOK stays true and Training stays empty.
func (app *Application) loadQueueSection(ctx context.Context, ch db.Character, sheet *characterSheet) {
	var queue esi.Skillqueue
	if err := app.esi.GetCached(ctx, ch, esi.SnapSkillqueue, &queue); err != nil {
		log.Printf("home: skill queue for character %d: %v", ch.CharacterID, err)
		return
	}
	sheet.QueueOK = true

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
		}
		if finish != "" {
			sheet.Training = fmt.Sprintf("%s %s — finishes %s", name, esi.RomanLevel(entry.FinishedLevel), finish)
		} else {
			sheet.Training = fmt.Sprintf("%s %s", name, esi.RomanLevel(entry.FinishedLevel))
		}
		return
	}
}

func (app *Application) handleAdmin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := pageData{
		LoggedIn:      true,
		CharacterName: app.sessions.GetString(ctx, sessionCharacterName),
		SSOConfigured: app.cfg.SSOConfigured(),
		WorkerStatus:  app.workerStatusText(),
	}

	users, err := app.queries.ListUsers(ctx)
	if err != nil {
		log.Printf("admin: list users: %v", err)
		data.Error = "Could not load admin data; check the server log."
	} else {
		data.Users = users
	}

	characters, err := app.queries.ListAllCharacters(ctx)
	if err != nil {
		log.Printf("admin: list characters: %v", err)
		data.Error = "Could not load admin data; check the server log."
	} else {
		data.Characters = characters
	}

	// Snapshot cache overview: per character, which ESI kinds are
	// cached and when each expires.
	for _, ch := range data.Characters {
		snaps, err := app.queries.ListSnapshotsByCharacter(ctx, ch.CharacterID)
		if err != nil {
			log.Printf("admin: list snapshots for character %d: %v", ch.CharacterID, err)
			continue
		}
		for _, snap := range snaps {
			until := "—"
			if snap.CachedUntil.Valid && snap.CachedUntil.String != "" {
				until = snap.CachedUntil.String
			}
			data.Snapshots = append(data.Snapshots, adminSnapshotRow{
				CharacterID:   ch.CharacterID,
				CharacterName: ch.Name,
				Kind:          snap.Kind,
				FetchedAt:     snap.FetchedAt,
				CachedUntil:   until,
			})
		}
	}

	app.render(w, http.StatusOK, "admin.html", data)
}
