package app

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"evesynapse/internal/esi"
)

// corpView is one corporation block on the Corporation page. All
// strings are display-ready (formatted, resolved, sanitized) so the
// template stays dumb.
type corpView struct {
	ID             int64
	Name           string
	Ticker         string
	LogoURL        string
	MemberCount    string // thousands-separated
	CEOName        string
	CEOPortraitURL string
	Alliance       string // "Name [TICK]", "" when not in an alliance
	TaxRate        string // "10.0%"
	Founded        string // YYYY-MM-DD
	HomeStation    string
	Description    string // plain text, template escapes it
	Characters     []string
	// LinkCharacterID is one of the user's characters in this
	// corporation; the template links the corporation subpages
	// with it (those pages follow the selected character's corp).
	LinkCharacterID int64
}

// corpCacheEntry is a built corpView (minus the per-user Characters
// list) plus the moment it goes stale, taken from ESI's Expires
// header so we poll no faster than CCP allows.
type corpCacheEntry struct {
	view      corpView
	expiresAt time.Time
}

// corporation returns the corpView for a corporation, serving the
// in-memory cache while it's inside ESI's cache window. A failed
// refresh falls back to the stale entry when one exists; with no
// entry at all the error propagates to the caller.
func (app *Application) corporation(ctx context.Context, corpID int64) (corpView, error) {
	app.corpMu.Lock()
	entry, ok := app.corpCache[corpID]
	app.corpMu.Unlock()

	if ok && time.Now().Before(entry.expiresAt) {
		return entry.view, nil
	}

	view, expiresAt, err := app.fetchCorporation(ctx, corpID)
	if err != nil {
		if ok {
			log.Printf("corporations: refresh corporation %d failed (%v); serving stale entry", corpID, err)
			return entry.view, nil
		}
		return corpView{}, err
	}

	app.corpMu.Lock()
	app.corpCache[corpID] = corpCacheEntry{view: view, expiresAt: expiresAt}
	app.corpMu.Unlock()
	return view, nil
}

// fetchCorporation builds a corpView from public ESI endpoints.
// Corporation facts come from GET /corporations/{id}/ (whose Expires
// header drives the cache); the CEO name, alliance and home-station
// resolutions each degrade to blank rather than failing the page.
func (app *Application) fetchCorporation(ctx context.Context, corpID int64) (corpView, time.Time, error) {
	body, header, err := app.esi.FetchRaw(ctx, "", fmt.Sprintf("/corporations/%d/", corpID))
	if err != nil {
		return corpView{}, time.Time{}, err
	}
	var corp esi.Corporation
	if err := json.Unmarshal(body, &corp); err != nil {
		return corpView{}, time.Time{}, fmt.Errorf("decode corporation %d: %w", corpID, err)
	}

	expiresAt := time.Now().Add(5 * time.Minute)
	if exp := header.Get("Expires"); exp != "" {
		if t, perr := http.ParseTime(exp); perr == nil {
			expiresAt = t
		}
	}

	view := corpView{
		ID:          corpID,
		Name:        corp.Name,
		Ticker:      corp.Ticker,
		LogoURL:     fmt.Sprintf("https://images.evetech.net/corporations/%d/logo?size=128", corpID),
		MemberCount: esi.FormatInt(corp.MemberCount),
		TaxRate:     fmt.Sprintf("%.1f%%", corp.TaxRate*100),
		Description: plainTextDescription(corp.Description),
	}
	if len(corp.DateFounded) >= 10 {
		view.Founded = corp.DateFounded[:10]
	}

	if corp.CEOID > 0 {
		view.CEOPortraitURL = fmt.Sprintf("https://images.evetech.net/characters/%d/portrait?size=64", corp.CEOID)
		var ceo esi.Character
		if err := app.esi.Get(ctx, "", fmt.Sprintf("/characters/%d/", corp.CEOID), &ceo); err == nil {
			view.CEOName = ceo.Name
		} else {
			log.Printf("corporations: CEO lookup for corporation %d: %v", corpID, err)
		}
	}

	if corp.AllianceID > 0 {
		var ally esi.Alliance
		if err := app.esi.Get(ctx, "", fmt.Sprintf("/alliances/%d/", corp.AllianceID), &ally); err == nil {
			if ally.Ticker != "" {
				view.Alliance = fmt.Sprintf("%s [%s]", ally.Name, ally.Ticker)
			} else {
				view.Alliance = ally.Name
			}
		} else {
			log.Printf("corporations: alliance lookup %d for corporation %d: %v", corp.AllianceID, corpID, err)
		}
	}

	if corp.HomeStationID > 0 {
		var station esi.Station
		if err := app.esi.Get(ctx, "", fmt.Sprintf("/universe/stations/%d/", corp.HomeStationID), &station); err == nil {
			view.HomeStation = station.Name
		} else {
			log.Printf("corporations: station lookup %d for corporation %d: %v", corp.HomeStationID, corpID, err)
		}
	}

	return view, expiresAt, nil
}

var (
	brTagRe  = regexp.MustCompile(`(?i)<br\s*/?>`)
	anyTagRe = regexp.MustCompile(`<[^>]*>`)
)

// plainTextDescription flattens an ESI corporation description (EVE
// HTML: <font>, <br>, showinfo links) to plain text. The template
// escapes on render, so nothing here needs to be markup-safe.
func plainTextDescription(s string) string {
	s = brTagRe.ReplaceAllString(s, "\n")
	s = anyTagRe.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	return strings.TrimSpace(s)
}

// handleCorporations renders the Corporation page: one block per
// corporation the signed-in user's linked characters belong to.
// Membership is resolved live — the characters table has no
// corporation_id column, and public character sheets carry it — and
// the corporation data itself comes from the public corp endpoints,
// so this page needs no characters' tokens at all.
func (app *Application) handleCorporations(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := pageData{
		LoggedIn:      true,
		CharacterName: app.sessions.GetString(ctx, sessionCharacterName),
		SSOConfigured: app.cfg.SSOConfigured(),
	}

	userID := int64(app.sessions.GetInt(ctx, sessionUserID))
	if userID == 0 {
		// Dev-login sessions carry no user; nothing to group.
		app.render(w, http.StatusOK, "corporations.html", data)
		return
	}

	characters, err := app.queries.ListCharactersByUser(ctx, userID)
	if err != nil {
		log.Printf("corporations: list characters for user %d: %v", userID, err)
		data.Error = "Could not load corporation data; check the server log."
		app.render(w, http.StatusOK, "corporations.html", data)
		return
	}

	// Group the user's characters by corporation.
	byCorp := make(map[int64][]string)
	linkChar := make(map[int64]int64)
	for _, ch := range characters {
		var pub esi.Character
		if err := app.esi.Get(ctx, "", fmt.Sprintf("/characters/%d/", ch.CharacterID), &pub); err != nil {
			log.Printf("corporations: public fetch for character %d: %v", ch.CharacterID, err)
			continue
		}
		if pub.CorporationID == 0 {
			continue
		}
		name := ch.Name
		if name == "" {
			name = pub.Name
		}
		byCorp[pub.CorporationID] = append(byCorp[pub.CorporationID], name)
		if _, seen := linkChar[pub.CorporationID]; !seen {
			linkChar[pub.CorporationID] = ch.CharacterID
		}
	}

	for corpID, names := range byCorp {
		view, err := app.corporation(ctx, corpID)
		if err != nil {
			log.Printf("corporations: build corporation %d: %v", corpID, err)
			continue
		}
		sort.Strings(names)
		view.Characters = names
		view.LinkCharacterID = linkChar[corpID]
		data.Corps = append(data.Corps, view)
	}
	sort.Slice(data.Corps, func(i, j int) bool { return data.Corps[i].Name < data.Corps[j].Name })

	app.render(w, http.StatusOK, "corporations.html", data)
}
