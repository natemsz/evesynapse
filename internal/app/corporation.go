package app

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
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
	CEOID          int64
	CEOPortraitURL string
	AllianceID     int64
	Alliance       string // "Name [TICK]", "" when not in an alliance
	TaxRate        string // "10.0%"
	Founded        string // YYYY-MM-DD
	HomeStation    string
	Description    string // plain text, template escapes it
	Characters     []corpCharacterRef
	// LinkCharacterID is one of the user's characters in this
	// corporation; the template links the corporation subpages
	// with it (those pages follow the selected character's corp).
	LinkCharacterID int64
}

// corpCharacterRef is one of the user's characters in a
// corporation, for the overview's "Your characters" line.
type corpCharacterRef struct {
	ID   int64
	Name string
}

// corpCacheEntry is a built corpView (minus the per-user Characters
// list) plus the moment it goes stale, taken from ESI's Expires
// header so we poll no faster than CCP allows.
type corpCacheEntry struct {
	view      corpView
	expiresAt time.Time
}

// corpCall is one in-flight corporation refresh. Readers arriving
// while it runs wait on it instead of each firing their own ESI
// request — singleflight, hand-rolled for this one call site, so
// x/sync stays out of go.mod.
type corpCall struct {
	done      chan struct{}
	view      corpView
	expiresAt time.Time
	err       error
}

// corporation returns the corpView for a corporation, serving the
// in-memory cache while it's inside ESI's cache window. A failed
// refresh falls back to the stale entry when one exists; with no
// entry at all the error propagates to the caller. Concurrent
// readers of an expired entry share one refresh.
func (app *Application) corporation(ctx context.Context, corpID int64) (corpView, error) {
	app.corpMu.Lock()
	entry, ok := app.corpCache[corpID]
	if ok && time.Now().Before(entry.expiresAt) {
		app.corpMu.Unlock()
		return entry.view, nil
	}
	if app.corpCalls == nil {
		app.corpCalls = make(map[int64]*corpCall)
	}
	if call, waiting := app.corpCalls[corpID]; waiting {
		app.corpMu.Unlock()
		select {
		case <-call.done:
		case <-ctx.Done():
			if ok {
				return entry.view, nil
			}
			return corpView{}, ctx.Err()
		}
		if call.err != nil {
			if ok {
				logging.Warnf("corporations: refresh corporation %d failed (%v); serving stale entry", corpID, call.err)
				return entry.view, nil
			}
			return corpView{}, call.err
		}
		return call.view, nil
	}
	call := &corpCall{done: make(chan struct{})}
	app.corpCalls[corpID] = call
	app.corpMu.Unlock()

	call.view, call.expiresAt, call.err = app.fetchCorporation(ctx, corpID)

	app.corpMu.Lock()
	delete(app.corpCalls, corpID)
	if call.err == nil {
		app.corpCache[corpID] = corpCacheEntry{view: call.view, expiresAt: call.expiresAt}
	}
	app.corpMu.Unlock()
	close(call.done)

	if call.err != nil {
		if ok {
			logging.Warnf("corporations: refresh corporation %d failed (%v); serving stale entry", corpID, call.err)
			return entry.view, nil
		}
		return corpView{}, call.err
	}
	return call.view, nil
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
		view.CEOID = corp.CEOID
		view.CEOPortraitURL = fmt.Sprintf("https://images.evetech.net/characters/%d/portrait?size=64", corp.CEOID)
		var ceo esi.Character
		if err := app.esi.Get(ctx, "", fmt.Sprintf("/characters/%d/", corp.CEOID), &ceo); err == nil {
			view.CEOName = ceo.Name
		} else {
			logging.Errorf("corporations: CEO lookup for corporation %d: %v", corpID, err)
		}
	}

	if corp.AllianceID > 0 {
		view.AllianceID = corp.AllianceID
		var ally esi.Alliance
		if err := app.esi.Get(ctx, "", fmt.Sprintf("/alliances/%d/", corp.AllianceID), &ally); err == nil {
			if ally.Ticker != "" {
				view.Alliance = fmt.Sprintf("%s [%s]", ally.Name, ally.Ticker)
			} else {
				view.Alliance = ally.Name
			}
		} else {
			logging.Errorf("corporations: alliance lookup %d for corporation %d: %v", corp.AllianceID, corpID, err)
		}
	}

	if corp.HomeStationID > 0 {
		var station esi.Station
		if err := app.esi.Get(ctx, "", fmt.Sprintf("/universe/stations/%d/", corp.HomeStationID), &station); err == nil {
			view.HomeStation = station.Name
		} else {
			logging.Errorf("corporations: station lookup %d for corporation %d: %v", corp.HomeStationID, corpID, err)
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
		app.render(ctx, w, http.StatusOK, "corporations.html", data)
		return
	}

	characters, err := app.queries.ListCharactersByUser(ctx, userID)
	if err != nil {
		logging.Errorf("corporations: list characters for user %d: %v", userID, err)
		data.Error = "Could not load corporation data; check the server log."
		app.render(ctx, w, http.StatusOK, "corporations.html", data)
		return
	}

	// Group the user's characters by corporation, using cached
	// profile snapshots (not live ESI) to avoid N outbound calls.
	byCorp := make(map[int64][]corpCharacterRef)
	linkChar := make(map[int64]int64)
	for _, ch := range characters {
		var pub esi.Character
		if !app.loadCorpSnapshot(ctx, ch.CharacterID, esi.SnapProfile, &pub) {
			continue
		}
		if pub.CorporationID == 0 {
			continue
		}
		name := ch.Name
		if name == "" {
			name = pub.Name
		}
		byCorp[pub.CorporationID] = append(byCorp[pub.CorporationID], corpCharacterRef{ID: ch.CharacterID, Name: name})
		if _, seen := linkChar[pub.CorporationID]; !seen {
			linkChar[pub.CorporationID] = ch.CharacterID
		}
	}

	for corpID, chars := range byCorp {
		view, err := app.corporation(ctx, corpID)
		if err != nil {
			logging.Errorf("corporations: build corporation %d: %v", corpID, err)
			continue
		}
		sort.Slice(chars, func(i, j int) bool { return chars[i].Name < chars[j].Name })
		view.Characters = chars
		view.LinkCharacterID = linkChar[corpID]
		data.Corps = append(data.Corps, view)
	}
	sort.Slice(data.Corps, func(i, j int) bool { return data.Corps[i].Name < data.Corps[j].Name })

	app.render(ctx, w, http.StatusOK, "corporations.html", data)
}
