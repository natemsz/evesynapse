package app

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// ---------------------------------------------------------------------------
// Character management + header switcher (Phase 1A). One account can
// link dozens of characters; these are the tools to run that fleet:
// the /characters/ page (link health, tags, unlink) and the topbar
// switcher that chooses which character the per-character pages
// act on.
// ---------------------------------------------------------------------------

// charactersView is the /characters/ page model.
type charactersView struct {
	Chars         []managedCharacter
	ConfirmUnlink *managedCharacter // set by ?confirm_unlink=<id>
	Linked        int               // characters in the ok state
	Relink        int               // characters needing a fresh sign-in
}

// managedCharacter is one row of the management page.
type managedCharacter struct {
	ID           int64
	Name         string
	PortraitURL  string
	CorpID       int64
	CorpName     string // "" when unknown
	Tags         string
	State        string // link_state as stored
	StateLabel   string // "Linked" | "Re-link needed"
	StateDetail  string // why a re-link is needed ("" when linked)
	PINotEnabled bool   // planetary scope missing on this login; re-link enables PI
	Since        string // link_state_at, when parked
	Snapshots    int    // stored snapshot rows
	Fresh        int    // of those, inside their cache window
	Newest       string // newest fetched_at, "—" when none
	Active       bool   // the session's acting character
}

// portraitURL is the CCP image-server portrait for a character.
func portraitURL(characterID int64, size int) string {
	return fmt.Sprintf("https://images.evetech.net/characters/%d/portrait?size=%d", characterID, size)
}

// switcherEntries builds the header switcher model for the
// signed-in account: every linked character, the acting one
// marked. The acting character is the session's pick when it is
// still linked, else the first linked character — the same
// fallback the per-character pages use. A nil result (signed out,
// dev session, no characters, DB trouble) hides the switcher.
func (app *Application) switcherEntries(ctx context.Context) []switcherEntry {
	userID := int64(app.sessions.GetInt(ctx, sessionUserID))
	if userID == 0 {
		return nil
	}
	characters, err := app.queries.ListCharactersByUser(ctx, userID)
	if err != nil || len(characters) == 0 {
		return nil
	}
	acting := app.actingCharacterID(ctx, characters)
	entries := make([]switcherEntry, 0, len(characters))
	for _, ch := range characters {
		entries = append(entries, switcherEntry{
			ID:          ch.CharacterID,
			Name:        ch.Name,
			Tags:        ch.Tags,
			PortraitURL: portraitURL(ch.CharacterID, 32),
			Active:      ch.CharacterID == acting,
			Relink:      !characterSyncs(ch),
		})
	}
	return entries
}

// actingCharacterID resolves the session's acting character
// against the account's linked characters: the session pick when
// still linked, else the first linked character (0 when none).
func (app *Application) actingCharacterID(ctx context.Context, characters []db.Character) int64 {
	if sid := int64(app.sessions.GetInt(ctx, sessionCharacterID)); sid != 0 {
		for _, ch := range characters {
			if ch.CharacterID == sid {
				return sid
			}
		}
	}
	if len(characters) > 0 {
		return characters[0].CharacterID
	}
	return 0
}

// handleCharacters renders the management page: link health,
// freshness, tags, and unlink for every character on the account.
func (app *Application) handleCharacters(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := pageData{
		LoggedIn:      true,
		CharacterName: app.sessions.GetString(ctx, sessionCharacterName),
		SSOConfigured: app.cfg.SSOConfigured(),
	}

	userID := int64(app.sessions.GetInt(ctx, sessionUserID))
	if userID == 0 {
		// Dev-login sessions carry no user; nothing to manage.
		data.CharactersPage = &charactersView{}
		app.render(ctx, w, http.StatusOK, "characters.html", data)
		return
	}

	characters, err := app.queries.ListCharactersByUser(ctx, userID)
	if err != nil {
		log.Printf("characters: list for user %d: %v", userID, err)
		data.Error = "Could not load your characters; check the server log."
		app.render(ctx, w, http.StatusOK, "characters.html", data)
		return
	}

	view := &charactersView{}
	acting := app.actingCharacterID(ctx, characters)
	confirmID, _ := strconv.ParseInt(r.URL.Query().Get("confirm_unlink"), 10, 64)
	for _, ch := range characters {
		row := app.managedCharacterRow(ctx, ch, acting)
		if row.State == linkStateOK {
			view.Linked++
		} else {
			view.Relink++
		}
		if confirmID != 0 && ch.CharacterID == confirmID {
			confirm := row
			view.ConfirmUnlink = &confirm
		}
		view.Chars = append(view.Chars, row)
	}
	data.CharactersPage = view
	app.render(ctx, w, http.StatusOK, "characters.html", data)
}

// managedCharacterRow assembles one management row: corporation
// and freshness come from local rows only (the worker-resolved
// corp mapping and the corp-info snapshot, the snapshot table).
func (app *Application) managedCharacterRow(ctx context.Context, ch db.Character, acting int64) managedCharacter {
	row := managedCharacter{
		ID:          ch.CharacterID,
		Name:        ch.Name,
		PortraitURL: portraitURL(ch.CharacterID, 64),
		Tags:        ch.Tags,
		State:       ch.LinkState,
		Newest:      "—",
		Active:      ch.CharacterID == acting,
	}
	switch ch.LinkState {
	case linkStateTokenDead:
		row.StateLabel = "Re-link needed"
		row.StateDetail = "EVE rejected this character's saved login (revoked or expired). Sign the character in again to resume syncing."
	case linkStateOwnerChanged:
		row.StateLabel = "Re-link needed"
		row.StateDetail = "This character changed EVE accounts since it was linked. Sign it in again to confirm you control it; syncing is paused until then."
	default:
		row.StateLabel = "Linked"
	}
	if ch.LinkStateAt.Valid {
		row.Since = ch.LinkStateAt.String
	}

	if mapping, err := app.queries.GetCharacterCorporation(ctx, ch.CharacterID); err == nil {
		row.CorpID = mapping.CorporationID
		row.CorpName = fmt.Sprintf("Corporation #%d", mapping.CorporationID)
		var info struct {
			Name string `json:"name"`
		}
		if app.loadCorpSnapshot(ctx, ch.CharacterID, esi.SnapCorpInfo, &info) && info.Name != "" {
			row.CorpName = info.Name
		}
	}

	// Planetary industry enablement (Phase 2): the link itself
	// is healthy, but its scope grant predates the planetary
	// scope, so colonies stay dark until a fresh sign-in.
	row.PINotEnabled = app.piNotEnabled(ctx, ch)

	snaps, err := app.queries.ListSnapshotsByCharacter(ctx, ch.CharacterID)
	if err == nil {
		row.Snapshots = len(snaps)
		for _, snap := range snaps {
			if esi.SnapshotFresh(snap) {
				row.Fresh++
			}
			if snap.FetchedAt > row.Newest || row.Newest == "—" {
				row.Newest = snap.FetchedAt
			}
		}
	}
	return row
}

// handleCharacterSwitch sets the session's acting character (the
// header switcher's target). Ownership is verified against the
// account; the browser returns to the page it came from.
func (app *Application) handleCharacterSwitch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := int64(app.sessions.GetInt(ctx, sessionUserID))
	characterID, _ := strconv.ParseInt(r.URL.Query().Get("character"), 10, 64)
	if userID != 0 && characterID != 0 {
		if ch, err := app.queries.GetCharacter(ctx, characterID); err == nil && ch.UserID == userID {
			app.sessions.Put(ctx, sessionCharacterID, int(characterID))
			app.sessions.Put(ctx, sessionCharacterName, ch.Name)
		}
	}
	target := "/"
	if ref := r.Header.Get("Referer"); ref != "" {
		if u, err := url.Parse(ref); err == nil && u.Path != "" && !strings.HasPrefix(u.Path, "//") {
			target = u.Path
			if u.RawQuery != "" {
				target += "?" + u.RawQuery
			}
		}
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// handleCharacterTags saves one character's free-text tags.
func (app *Application) handleCharacterTags(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := int64(app.sessions.GetInt(ctx, sessionUserID))
	if userID == 0 {
		http.Redirect(w, r, "/characters/", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/characters/", http.StatusSeeOther)
		return
	}
	characterID, _ := strconv.ParseInt(r.FormValue("character_id"), 10, 64)
	tags := normalizeTags(r.FormValue("tags"))
	if characterID != 0 {
		if err := app.queries.SetCharacterTags(ctx, db.SetCharacterTagsParams{
			Tags:        tags,
			CharacterID: characterID,
			UserID:      userID,
		}); err != nil {
			log.Printf("characters: set tags for character %d: %v", characterID, err)
		}
	}
	http.Redirect(w, r, "/characters/", http.StatusSeeOther)
}

// normalizeTags tidies a free-text tag string: trims, collapses
// runs of whitespace, and caps the length so the switcher stays
// readable.
func normalizeTags(raw string) string {
	tags := strings.Join(strings.Fields(raw), " ")
	if len(tags) > 140 {
		tags = strings.TrimSpace(tags[:140])
	}
	return tags
}

// handleCharacterUnlink removes a character from the account: the
// character row (and with it, by cascade, its snapshots, fetch
// state, corp mapping and detail stores) is deleted. The confirm
// step lives on the management page (?confirm_unlink=<id>); this
// POST is the destructive half. If the character was the acting
// one, the session falls back to the first remaining character —
// or to a clean empty state when it was the last one.
func (app *Application) handleCharacterUnlink(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := int64(app.sessions.GetInt(ctx, sessionUserID))
	if userID == 0 {
		http.Redirect(w, r, "/characters/", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/characters/", http.StatusSeeOther)
		return
	}
	characterID, _ := strconv.ParseInt(r.FormValue("character_id"), 10, 64)
	if characterID == 0 {
		http.Redirect(w, r, "/characters/", http.StatusSeeOther)
		return
	}

	if err := app.queries.DeleteCharacter(ctx, db.DeleteCharacterParams{
		CharacterID: characterID,
		UserID:      userID,
	}); err != nil {
		log.Printf("characters: unlink character %d for user %d: %v", characterID, userID, err)
		http.Redirect(w, r, "/characters/", http.StatusSeeOther)
		return
	}
	log.Printf("characters: user %d unlinked character %d (tokens and snapshots deleted)", userID, characterID)

	// Acting-character fallback: only when the removed character
	// was the session's pick.
	if int64(app.sessions.GetInt(ctx, sessionCharacterID)) == characterID {
		remaining, err := app.queries.ListCharactersByUser(ctx, userID)
		if err == nil && len(remaining) > 0 {
			app.sessions.Put(ctx, sessionCharacterID, int(remaining[0].CharacterID))
			app.sessions.Put(ctx, sessionCharacterName, remaining[0].Name)
		} else {
			app.sessions.Put(ctx, sessionCharacterID, 0)
			app.sessions.Put(ctx, sessionCharacterName, "")
		}
	}
	http.Redirect(w, r, "/characters/", http.StatusSeeOther)
}
