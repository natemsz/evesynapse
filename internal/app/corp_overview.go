package app

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"regexp"
	"sort"
	"strings"

	"net/http"

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
	// Pending: nothing is stored about the corporation yet. Its
	// record has been asked for, and the block shows what there is.
	Pending bool
}

// corpCharacterRef is one of the user's characters in a
// corporation, for the overview's "Your characters" line.
type corpCharacterRef struct {
	ID   int64
	Name string
}

// corporationOverview builds a corporation's block from stored data:
// its public record (intel_org_worker.go), else what one of the
// account's characters in it has stored of its own corporation.
// Nothing is fetched to draw the page. A record not held yet is
// queued, like any other thing a page could not show, and the block
// is marked pending.
func (app *Application) corporationOverview(ctx context.Context, corpID, characterID int64) corpView {
	view := corpView{
		ID:      corpID,
		Name:    fmt.Sprintf("Corporation #%d", corpID),
		LogoURL: fmt.Sprintf("https://images.evetech.net/corporations/%d/logo?size=128", corpID),
	}
	var corp esi.Corporation
	var alliance esi.Alliance
	held := false
	if rec, err := app.queries.GetCorporationRecord(ctx, corpID); err == nil && rec.State == orgStateReady && rec.Payload != "" {
		var payload corporationRecordPayload
		if json.Unmarshal([]byte(rec.Payload), &payload) == nil && payload.Corp.Name != "" {
			corp, alliance, held = payload.Corp, payload.Alliance, true
		}
	}
	if !held {
		app.notePageWant(ctx, pageWantCorporation, corpID, 0)
		held = characterID != 0 && app.loadCorpSnapshot(ctx, characterID, esi.SnapCorpInfo, &corp) && corp.Name != ""
	}
	if !held {
		view.Pending = true
		return view
	}

	view.Name, view.Ticker = corp.Name, corp.Ticker
	view.MemberCount = esi.FormatInt(corp.MemberCount)
	view.TaxRate = fmt.Sprintf("%.1f%%", corp.TaxRate*100)
	view.Description = plainTextDescription(corp.Description)
	if len(corp.DateFounded) >= 10 {
		view.Founded = corp.DateFounded[:10]
	}
	if corp.CEOID > 0 {
		view.CEOID = corp.CEOID
		view.CEOPortraitURL = fmt.Sprintf("https://images.evetech.net/characters/%d/portrait?size=64", corp.CEOID)
		view.CEOName = app.displayCharacter(ctx, corp.CEOID)
	}
	if corp.AllianceID > 0 {
		view.AllianceID = corp.AllianceID
		name, ticker := alliance.Name, alliance.Ticker
		if name == "" {
			name, _ = app.resolvedAllianceName(ctx, corp.AllianceID)
		}
		switch {
		case name == "":
			view.Alliance = fmt.Sprintf("Alliance #%d", corp.AllianceID)
		case ticker != "":
			view.Alliance = fmt.Sprintf("%s [%s]", name, ticker)
		default:
			view.Alliance = name
		}
	}
	if corp.HomeStationID > 0 {
		view.HomeStation = app.locationTitle(ctx, corp.HomeStationID, "station")
	}
	return view
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

// handleCorporations renders the Corporation page: one corporation the
// signed-in user's linked characters belong to, picked among them.
// Which corporation each character is in comes from its stored
// profile, and the corporation's own details from what is stored of
// it (corporationOverview): the page fetches nothing.
func (app *Application) handleCorporations(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.page(ctx)

	userID := app.userID(ctx)
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
	var offered []int64 // in the order of the account's characters
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
			offered = append(offered, pub.CorporationID)
		}
	}

	// One corporation to a page, picked with the selector (corp_select.go).
	if corpID, options := app.pickCorporation(ctx, r, offered); corpID != 0 {
		view := app.corporationOverview(ctx, corpID, linkChar[corpID])
		chars := byCorp[corpID]
		sort.Slice(chars, func(i, j int) bool { return chars[i].Name < chars[j].Name })
		view.Characters = chars
		view.LinkCharacterID = linkChar[corpID]
		data.Corps = []corpView{view}
		data.CorpOptions = options
	}

	app.render(ctx, w, http.StatusOK, "corporations.html", data)
}
