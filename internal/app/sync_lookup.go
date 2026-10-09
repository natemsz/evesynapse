package app

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Finding one character among all of them. The Sync and Admin pages
// show one character (or account) at a time, found through a search box
// that suggests as you type.
//
// Both pages are for administrators only, so the lookup covers every
// character on the site, not just the reader's own.
// ---------------------------------------------------------------------------

const (
	// lookupSuggestions is how many names the search box suggests.
	lookupSuggestions = 10
	// lookupMatches is how many are listed when the text typed and
	// submitted fits more than one character.
	lookupMatches = 25
)

// lookupMatch is one character a search found.
type lookupMatch struct {
	ID     int64
	Name   string
	UserID int64
}

// characterLookupView is the search box and what came of the search.
type characterLookupView struct {
	Query    string        // what was asked for, to put back in the box
	Matches  []lookupMatch // more than one fits: pick one
	More     bool          // and there were more than are listed
	NotFound bool
}

// likePattern escapes the characters LIKE gives a meaning to, so that
// what was typed is matched as it stands.
func likePattern(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// findCharacters is the lookup: names containing q, or the character
// with that id. Fewer than two characters of text find nothing, unless
// they are a number.
func (app *Application) findCharacters(ctx context.Context, q string, limit int64) []db.FindCharactersRow {
	q = strings.TrimSpace(q)
	if _, err := strconv.ParseInt(q, 10, 64); err != nil && len([]rune(q)) < 2 {
		return nil
	}
	rows, err := app.queries.FindCharacters(ctx, db.FindCharactersParams{Pattern: likePattern(q), Exact: q, RowLimit: limit})
	if err != nil {
		logging.Errorf("lookup: find characters: %v", err)
		return nil
	}
	return rows
}

// lookupCharacter resolves what was typed or picked to one character.
// A number is a character id. Text that fits exactly one character, or
// is exactly one character's name, is that character; text that fits
// several comes back as a list to pick from.
func (app *Application) lookupCharacter(ctx context.Context, raw string) (*db.Character, characterLookupView) {
	view := characterLookupView{Query: strings.TrimSpace(raw)}
	if view.Query == "" {
		return nil, view
	}
	get := func(id int64) *db.Character {
		ch, err := app.queries.GetCharacter(ctx, id)
		if err != nil {
			return nil
		}
		return &ch
	}
	if id, err := strconv.ParseInt(view.Query, 10, 64); err == nil && id > 0 {
		if ch := get(id); ch != nil {
			view.Query = ch.Name
			return ch, view
		}
	}
	rows := app.findCharacters(ctx, view.Query, lookupMatches+1)
	exact := func(i int) bool { return i < len(rows) && strings.EqualFold(rows[i].Name, view.Query) }
	switch {
	case len(rows) == 0:
		view.NotFound = true
		return nil, view
	case len(rows) == 1, exact(0) && !exact(1):
		if ch := get(rows[0].CharacterID); ch != nil {
			view.Query = ch.Name
			return ch, view
		}
		view.NotFound = true
		return nil, view
	}
	if len(rows) > lookupMatches {
		rows, view.More = rows[:lookupMatches], true
	}
	for _, row := range rows {
		view.Matches = append(view.Matches, lookupMatch{ID: row.CharacterID, Name: row.Name, UserID: row.UserID})
	}
	return nil, view
}

// handleCharacterSuggest serves GET /admin/suggest?q=: the names the
// Sync and Admin search boxes offer while something is typed. Each
// says which account the character belongs to.
func (app *Application) handleCharacterSuggest(w http.ResponseWriter, r *http.Request) {
	var out []suggestItem
	for _, row := range app.findCharacters(r.Context(), r.URL.Query().Get("q"), lookupSuggestions) {
		out = append(out, suggestItem{ID: row.CharacterID, Name: row.Name, Label: fmt.Sprintf("account %d", row.UserID)})
	}
	writeSuggestJSON(w, out)
}
