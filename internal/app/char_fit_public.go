package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"html"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/fit"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Public fits: the fits anyone here has made public. The browser and
// the fit pages sit outside the auth middleware: anonymous readers
// get the same stored rows, and every filter, sort and page is a
// SQL predicate with LIMIT/OFFSET. Role matches a fit's tags
// against the fleet-role taxonomy; there is no stored role column.
// ---------------------------------------------------------------------------

const (
	publicFitsPath = "/fittings/public/"
	// publicFitsPerPage bounds every browser page; the count
	// query beside the list query feeds the page links.
	publicFitsPerPage = 24
)

// publicFitSorts are the browser's sort orders in display order;
// the first is the default. Keys are allowlisted: only these ever
// reach the ORDER BY.
var publicFitSorts = []struct{ Key, Label string }{
	{"rank", "Rank"},
	{"views", "Most viewed"},
	{"new", "Newest"},
	{"old", "Oldest"},
	{"name", "Name A-Z"},
	{"namedesc", "Name Z-A"},
	{"pricelo", "Price low-high"},
	{"pricehi", "Price high-low"},
}

// publicFitFilters is the browser's parsed address. IncludeName
// and ExcludeName are what was typed; Include and Exclude are the
// resolved type IDs (0 when blank or unresolvable, with a Note).
type publicFitFilters struct {
	Q           string
	Role        string
	Hull        string
	HullQ       string
	MinStars    float64
	Include     int64
	IncludeName string
	Exclude     int64
	ExcludeName string
	Faction     string
	MaxPrice    float64
	Alpha       bool
	Tag         string
	Sort        string
	Page        int
	Note        string
}

// parsePublicFitFilters reads the browser address; unknown sorts
// fall back to rank, bad numbers to blank.
func (app *Application) parsePublicFitFilters(ctx context.Context, query url.Values) publicFitFilters {
	f := publicFitFilters{
		Q:       strings.TrimSpace(query.Get("q")),
		Role:    strings.TrimSpace(query.Get("role")),
		Hull:    strings.TrimSpace(query.Get("hull")),
		HullQ:   strings.TrimSpace(query.Get("hullq")),
		Faction: strings.TrimSpace(query.Get("faction")),
		Tag:     strings.TrimSpace(query.Get("tag")),
		Sort:    strings.TrimSpace(query.Get("sort")),
	}
	if f.Sort == "" {
		f.Sort = "rank"
	}
	known := false
	for _, s := range publicFitSorts {
		if s.Key == f.Sort {
			known = true
		}
	}
	if !known {
		f.Sort = "rank"
	}
	if v, err := strconv.ParseFloat(strings.TrimSpace(query.Get("stars")), 64); err == nil && v > 0 {
		if v > 5 {
			v = 5
		}
		f.MinStars = v
	}
	f.IncludeName = strings.TrimSpace(query.Get("include"))
	f.ExcludeName = strings.TrimSpace(query.Get("exclude"))
	if f.IncludeName != "" {
		if id, ok := app.resolveFitModuleType(ctx, f.IncludeName); ok {
			f.Include = id
		} else {
			f.Note = "No module named " + strconv.Quote(f.IncludeName) + "; that filter was ignored."
		}
	}
	if f.ExcludeName != "" {
		if id, ok := app.resolveFitModuleType(ctx, f.ExcludeName); ok {
			f.Exclude = id
		} else {
			note := "No module named " + strconv.Quote(f.ExcludeName) + "; that filter was ignored."
			if f.Note != "" {
				f.Note += " " + note
			} else {
				f.Note = note
			}
		}
	}
	switch f.Faction {
	case "none":
	default:
		f.Faction = "any"
	}
	if v, err := strconv.ParseFloat(strings.TrimSpace(query.Get("maxprice")), 64); err == nil && v > 0 {
		f.MaxPrice = v
	}
	f.Alpha = query.Get("alpha") == "1"
	if p, err := strconv.Atoi(strings.TrimSpace(query.Get("page"))); err == nil && p > 1 {
		f.Page = p
	} else {
		f.Page = 1
	}
	return f
}

// resolveFitModuleType maps a typed module name to its type ID:
// exact SDE name match only, so a filter never silently means a
// different module than the one named.
func (app *Application) resolveFitModuleType(ctx context.Context, name string) (int64, bool) {
	if name == "" {
		return 0, false
	}
	row, err := app.queries.GetSDETypeByName(ctx, name)
	if err != nil || row.TypeID <= 0 {
		return 0, false
	}
	return row.TypeID, true
}

// publicFitListParams renders the filters into the list and count
// queries' shared shape.
func (f publicFitFilters) listParams(limit int64) db.ListPublicFitsParams {
	return db.ListPublicFitsParams{
		Sort: f.Sort, Q: f.Q, Role: f.Role, Hull: f.Hull, Hullq: f.HullQ,
		Minstars: f.MinStars, Include: f.Include, Exclude: f.Exclude,
		Faction: f.Faction, Maxprice: f.MaxPrice, Alpha: f.Alpha, Tag: f.Tag,
		Rlimit: limit, Roffset: int64(f.Page-1) * limit,
	}
}

func (f publicFitFilters) countParams() db.CountPublicFitsParams {
	return db.CountPublicFitsParams{
		Sort: f.Sort, Q: f.Q, Role: f.Role, Hull: f.Hull, Hullq: f.HullQ,
		Minstars: f.MinStars, Include: f.Include, Exclude: f.Exclude,
		Faction: f.Faction, Maxprice: f.MaxPrice, Alpha: f.Alpha, Tag: f.Tag,
	}
}

// publicFitCard is one fit on the browser or the fit page header:
// stored aggregates only, never a per-render computation.
type publicFitCard struct {
	ID          int64
	Name        string
	Ship        string
	ShipTypeID  int64
	Hull        string
	Author      string
	Mine        bool
	Stars       float64
	StarCount   int64
	Views       int64
	Price       float64
	PriceKnown  bool
	PriceLine   string
	Tags        []string
	Updated     time.Time
	UpdatedLine string
	FitURL      string
	// OpenToolURL opens the fit in the simulator (own fits via
	// ?local=, others via ?public=): the doctrine keepers' flow.
	OpenToolURL string
	// AddField names this card's hidden input on the keeper's
	// add-to-doctrine form: local for own fits, public otherwise.
	AddField string
}

// publicFitCardOf renders one list row; tags decode from the
// stored document.
func publicFitCardOf(row db.ListPublicFitsRow, userID int64) publicFitCard {
	card := publicFitCard{
		ID: row.ID, Name: row.Name, Ship: row.ShipName, ShipTypeID: row.ShipTypeID,
		Hull: row.Hull, Author: row.AuthorName, Mine: userID > 0 && row.UserID == userID,
		Views: row.ViewCount, Price: row.TotalPrice, PriceKnown: row.TotalPrice > 0,
		Updated: row.UpdatedAt, UpdatedLine: rfc3339(row.UpdatedAt),
		FitURL: publicFitsPath + "fit/" + idString(row.ID) + "/",
	}
	if card.Mine {
		card.OpenToolURL = "/fittings/?local=" + idString(row.ID) + "#fit-editor"
		card.AddField = "local"
	} else {
		card.OpenToolURL = "/fittings/?public=" + idString(row.ID) + "#fit-editor"
		card.AddField = "public"
	}
	if card.PriceKnown {
		card.PriceLine = esi.FormatISK(row.TotalPrice)
	}
	if card.Ship == "" {
		card.Ship = "Type #" + idString(row.ShipTypeID)
	}
	if row.RatingCount > 0 {
		card.Stars = float64(row.RatingSum) / float64(row.RatingCount)
		card.StarCount = row.RatingCount
	}
	var doc fitDoc
	if json.Unmarshal([]byte(row.ItemsJson), &doc) == nil {
		card.Tags = doc.Tags
	}
	return card
}

type publicFitChip struct {
	Label string
	Count int64
	URL   string
	On    bool
}

// publicFitsView is the browser page: the filter bar's current
// state, facet chips, sort links, one bounded page of cards.
type publicFitsView struct {
	Filters    publicFitFilters
	Cards      []publicFitCard
	Total      int64
	Page       int
	Pages      int
	HasPrev    bool
	HasNext    bool
	PrevURL    string
	NextURL    string
	FirstURL   string
	Roles      []string
	Sorts      []publicFitChip
	Hulls      []publicFitChip
	Tags       []publicFitChip
	SuggestURL string
	Empty      string
	// For is the doctrine a keeper is choosing fits for, when
	// the page was opened from one (?for=, keepers only).
	For  *opChoice
	Self string
}

// publicFitURLParam is one browser address parameter: its key,
// the filter's current value, and the default, which is omitted
// like an empty value.
type publicFitURLParam struct {
	key     string
	current string
	def     string
}

// publicFitURL is the browser address with some choices changed;
// empty values take a choice off, page resets unless kept.
func (f publicFitFilters) publicFitURL(changes map[string]string, keepPage bool) string {
	num := func(v float64) string {
		if v <= 0 {
			return ""
		}
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	alpha := ""
	if f.Alpha {
		alpha = "1"
	}
	page := ""
	if keepPage && f.Page > 1 {
		page = strconv.Itoa(f.Page)
	}
	params := []publicFitURLParam{
		{"q", f.Q, ""},
		{"role", f.Role, ""},
		{"hull", f.Hull, ""},
		{"hullq", f.HullQ, ""},
		{"include", f.IncludeName, ""},
		{"exclude", f.ExcludeName, ""},
		{"faction", f.Faction, "any"},
		{"tag", f.Tag, ""},
		{"sort", f.Sort, "rank"},
		{"stars", num(f.MinStars), ""},
		{"maxprice", num(f.MaxPrice), ""},
		{"alpha", alpha, ""},
		{"page", page, ""},
	}
	query := url.Values{}
	for _, p := range params {
		v := p.current
		if c, ok := changes[p.key]; ok {
			v = c
		}
		if v == "" || v == p.def {
			continue
		}
		query.Set(p.key, v)
	}
	if len(query) == 0 {
		return publicFitsPath
	}
	return publicFitsPath + "?" + query.Encode()
}

// handlePublicFits renders the anonymous fit browser: one bounded
// SQL page of stored rows, facet chips from bounded aggregate
// reads, no per-user queries when nobody is signed in.
func (app *Application) handlePublicFits(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.publicPage(ctx)
	userID := app.userID(ctx)
	filters := app.parsePublicFitFilters(ctx, r.URL.Query())

	total, err := app.queries.CountPublicFits(ctx, filters.countParams())
	if err != nil {
		logging.Errorf("public fits: count: %v", err)
		data.Error = "Could not load public fits; check the server log."
		app.render(ctx, w, http.StatusOK, "fittings_public.html", data)
		return
	}
	pages := int((total + publicFitsPerPage - 1) / publicFitsPerPage)
	if pages < 1 {
		pages = 1
	}
	if filters.Page > pages {
		filters.Page = pages
	}
	page := filters.Page
	rows, err := app.queries.ListPublicFits(ctx, filters.listParams(publicFitsPerPage))
	if err != nil {
		logging.Errorf("public fits: list: %v", err)
		data.Error = "Could not load public fits; check the server log."
		app.render(ctx, w, http.StatusOK, "fittings_public.html", data)
		return
	}
	cards := make([]publicFitCard, 0, len(rows))
	for _, row := range rows {
		cards = append(cards, publicFitCardOf(row, userID))
	}
	// doctrineChosenFor returns nil unless the account keeps the
	// doctrine, so a non-nil For is the keeper flag itself.
	chosen := app.doctrineChosenFor(ctx, userID, r.URL.Query().Get("for"))
	view := &publicFitsView{
		Filters: filters, Cards: cards, Total: total, Page: page, Pages: pages,
		Roles:      opFleetRoles,
		SuggestURL: publicFitsPath + "suggest",
		Empty:      `Nobody has made a fit public yet. To share one of yours, tick "Make public" on it in the fitting tool.`,
		For:        chosen,
	}
	view.Self = filters.publicFitURL(map[string]string{}, true)
	if page > 1 {
		view.HasPrev = true
		view.PrevURL = filters.publicFitURL(map[string]string{"page": strconv.Itoa(page - 1)}, true)
	}
	if int64(page*publicFitsPerPage) < total {
		view.HasNext = true
		view.NextURL = filters.publicFitURL(map[string]string{"page": strconv.Itoa(page + 1)}, true)
	}
	view.FirstURL = filters.publicFitURL(map[string]string{}, false)
	for _, s := range publicFitSorts {
		view.Sorts = append(view.Sorts, publicFitChip{
			Label: s.Label, On: s.Key == filters.Sort,
			URL: filters.publicFitURL(map[string]string{"sort": s.Key}, false),
		})
	}
	if hulls, err := app.queries.PublicFitHullFacets(ctx); err == nil {
		for _, h := range hulls {
			chip := publicFitChip{Label: h.Hull, Count: h.Fits, On: h.Hull == filters.Hull}
			if chip.On {
				chip.URL = filters.publicFitURL(map[string]string{"hull": ""}, false)
			} else {
				chip.URL = filters.publicFitURL(map[string]string{"hull": h.Hull}, false)
			}
			view.Hulls = append(view.Hulls, chip)
		}
	} else {
		logging.Errorf("public fits: hull facets: %v", err)
	}
	if tags, err := app.queries.PublicFitTagFacets(ctx); err == nil {
		for _, tg := range tags {
			chip := publicFitChip{Label: tg.Tag, Count: tg.Fits, On: strings.EqualFold(tg.Tag, filters.Tag)}
			if chip.On {
				chip.URL = filters.publicFitURL(map[string]string{"tag": ""}, false)
			} else {
				chip.URL = filters.publicFitURL(map[string]string{"tag": tg.Tag}, false)
			}
			view.Tags = append(view.Tags, chip)
		}
	} else {
		logging.Errorf("public fits: tag facets: %v", err)
	}
	data.PublicFits = view
	app.render(ctx, w, http.StatusOK, "fittings_public.html", data)
}

// handlePublicFitSuggest feeds the public browser search box
// (GET /fittings/public/suggest?q=): one bounded SQL read.
func (app *Application) handlePublicFitSuggest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(q) < 2 {
		writeSuggestJSON(w, nil)
		return
	}
	rows, err := app.queries.SearchPublicFittings(ctx, db.SearchPublicFittingsParams{Q: q, UserID: userID})
	if err != nil {
		logging.Errorf("public fits: suggest: %v", err)
		writeSuggestJSON(w, nil)
		return
	}
	out := make([]suggestItem, 0, len(rows))
	for _, row := range rows {
		ship := row.ShipName
		if ship == "" {
			ship = app.typeNameOrID(ctx, row.ShipTypeID)
		}
		label := ship
		if author := row.AuthorName; author != "" {
			label += " · by " + author
		}
		out = append(out, suggestItem{
			Name: row.Name, Label: label,
			URL: publicFitsPath + "fit/" + idString(row.ID) + "/",
		})
	}
	writeSuggestJSON(w, out)
}

// publicPage is the page data anonymous-safe routes start from:
// the session's login state, never assumed.
func (app *Application) publicPage(ctx context.Context) pageData {
	loggedIn := app.sessions.GetBool(ctx, sessionAuthenticated)
	return pageData{
		LoggedIn:      loggedIn,
		CharacterName: app.sessions.GetString(ctx, sessionCharacterName),
		SSOConfigured: app.cfg.SSOConfigured(),
	}
}

// publicFitView is one stable public fit page: the stored fit,
// its modules grouped server-side, the EFT text, the rating
// state, and absolute share links. Sim is the same All-V
// workbench view the fitting tool renders, shown read-only.
type publicFitView struct {
	Card        publicFitCard
	Description string
	Sections    []fittingGroup
	Sim         *fitSimView
	EFT         string
	AvgStars    float64
	StarCount   int64
	MyStars     int64
	PageURL     string
	ShareX      string
	ShareReddit string
}

// publicFitSections groups a stored document's items the way the
// saved-fits list does, resolving every name from the local
// caches. Classification needs the dogma snapshot; without it
// every line lands in Other rather than vanishing.
func (app *Application) publicFitSections(ctx context.Context, doc *fitDoc) []fittingGroup {
	typeIDs := []int64{doc.ShipTypeID}
	for _, it := range doc.Items {
		typeIDs = append(typeIDs, it.TypeID)
	}
	names := app.esi.CachedTypeNames(ctx, typeIDs)
	nameOf := func(typeID int64) string {
		if n, ok := names[typeID]; ok && n != "" {
			return n
		}
		return "Type #" + idString(typeID)
	}
	familyOf := map[int64]string{}
	if snap, err := fit.LoadSnapshot(ctx, app.queries, typeIDs); err == nil && snap != nil {
		for _, id := range typeIDs {
			familyOf[id] = fitSlotFamilyOf(snap, id)
		}
	}
	familyLabel := map[string]string{
		fitFamilyHigh: "High slots", fitFamilyMedium: "Medium slots",
		fitFamilyLow: "Low slots", fitFamilyRig: "Rig slots",
		fitFamilySubsystem: "Subsystem slots", fitFamilyDrone: "Drone bay",
		fitFamilyCargo: "Cargo hold",
	}
	byFamily := map[string][]fittingItemRow{}
	order := []string{}
	for _, it := range doc.Items {
		family := familyOf[it.TypeID]
		if _, ok := familyLabel[family]; !ok {
			family = "other"
		}
		if _, ok := byFamily[family]; !ok {
			order = append(order, family)
		}
		byFamily[family] = append(byFamily[family], fittingItemRow{
			Name: nameOf(it.TypeID), TypeID: it.TypeID,
			Quantity: esi.FormatInt(int64(it.Qty)),
		})
	}
	ordered := []string{fitFamilyHigh, fitFamilyMedium, fitFamilyLow, fitFamilyRig, fitFamilySubsystem, fitFamilyDrone, fitFamilyCargo}
	seen := map[string]bool{}
	var sections []fittingGroup
	for _, family := range ordered {
		items, ok := byFamily[family]
		if !ok {
			continue
		}
		seen[family] = true
		sections = append(sections, fittingGroup{Label: familyLabel[family], Items: items})
	}
	for _, family := range order {
		if seen[family] {
			continue
		}
		sections = append(sections, fittingGroup{Label: "Other", Items: byFamily[family]})
	}
	return sections
}

// fitDocPrice totals a document from the guide mirror (adjusted
// ISK per unit): hull plus every module line. 0 when no prices
// are warmed yet; the browser treats 0 as unknown.
func (app *Application) fitDocPrice(ctx context.Context, doc *fitDoc) float64 {
	prices := app.valuationPrices(ctx)
	if len(prices) == 0 {
		return 0
	}
	priceOf := func(typeID int64) float64 {
		if p, ok := prices[typeID]; ok {
			return p.AdjustedPrice
		}
		return 0
	}
	total := priceOf(doc.ShipTypeID)
	for _, it := range doc.Items {
		total += float64(it.Qty) * priceOf(it.TypeID)
	}
	return total
}

// headMeta composes escaped description and Open Graph tags for a
// public page; values are attribute-escaped at build time.
func headMeta(title, description, pageURL string) template.HTML {
	esc := html.EscapeString
	var b strings.Builder
	b.WriteString(`<meta name="description" content="` + esc(description) + `">` + "\n")
	b.WriteString(`<meta property="og:title" content="` + esc(title) + `">` + "\n")
	b.WriteString(`<meta property="og:description" content="` + esc(description) + `">` + "\n")
	b.WriteString(`<meta property="og:type" content="website">` + "\n")
	b.WriteString(`<meta property="og:url" content="` + esc(pageURL) + `">` + "\n")
	return template.HTML(b.String())
}

// handlePublicFitPage renders one stable public fit page:
// server-rendered modules, EFT, share links and the rating
// widget. Anonymous readers see everything but the rate form.
func (app *Application) handlePublicFitPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.publicPage(ctx)
	userID := app.userID(ctx)

	fitID, err := strconv.ParseInt(chi.URLParam(r, "fitID"), 10, 64)
	if err != nil || fitID <= 0 {
		http.NotFound(w, r)
		return
	}
	row, err := app.queries.GetPublicFitting(ctx, fitID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		logging.Errorf("public fit %d: load: %v", fitID, err)
		data.Error = "Could not load that fit; check the server log."
		app.render(ctx, w, http.StatusOK, "fit_public.html", data)
		return
	}
	if err := app.queries.BumpFitViewCount(ctx, fitID); err != nil {
		logging.Errorf("public fit %d: count view: %v", fitID, err)
	}
	var doc fitDoc
	if json.Unmarshal([]byte(row.ItemsJson), &doc) != nil {
		http.NotFound(w, r)
		return
	}
	ship := app.typeNameOrID(ctx, row.ShipTypeID)
	author := row.AuthorName
	card := publicFitCard{
		ID: row.ID, Name: row.Name, Ship: ship, ShipTypeID: row.ShipTypeID,
		Author: author, Mine: userID > 0 && row.UserID == userID,
		Views: row.ViewCount + 1, Price: row.TotalPrice, PriceKnown: row.TotalPrice > 0,
		Updated: row.UpdatedAt, FitURL: publicFitsPath + "fit/" + idString(row.ID) + "/",
		Tags: doc.Tags,
	}
	if row.RatingCount > 0 {
		card.Stars = float64(row.RatingSum) / float64(row.RatingCount)
		card.StarCount = row.RatingCount
	}
	view := &publicFitView{
		Card: card, Description: doc.Description,
		Sections: app.publicFitSections(ctx, &doc),
		Sim:      app.buildFitSimView(ctx, &doc, 0, "All V", 0),
		EFT:      app.formatEFT(ctx, &doc),
		AvgStars: card.Stars, StarCount: card.StarCount,
		PageURL: app.siteOrigin(r) + card.FitURL,
	}
	view.ShareX = "https://x.com/intent/post?text=" + url.QueryEscape(row.Name+" ("+ship+")") + "&url=" + url.QueryEscape(view.PageURL)
	view.ShareReddit = "https://www.reddit.com/submit?url=" + url.QueryEscape(view.PageURL) + "&title=" + url.QueryEscape(row.Name+" ("+ship+")")
	if userID > 0 {
		if mine, err := app.queries.GetFitRating(ctx, db.GetFitRatingParams{FitID: fitID, UserID: userID}); err == nil {
			view.MyStars = int64(mine.Stars)
		} else if !errors.Is(err, sql.ErrNoRows) {
			logging.Errorf("public fit %d: own rating: %v", fitID, err)
		}
	}
	blurb := ship
	if author != "" {
		blurb += " by " + author
	}
	if doc.Description != "" {
		blurb += " - " + doc.Description
	}
	data.HeadMeta = headMeta(row.Name+" ("+ship+")", blurb, view.PageURL)
	data.PublicFit = view
	app.render(ctx, w, http.StatusOK, "fit_public.html", data)
}

// handlePublicFitRate records one account's rating of a public
// fit (POST, login required): re-rating updates the same row,
// and the fit's aggregates move in the same transaction.
func (app *Application) handlePublicFitRate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	fitID, err := strconv.ParseInt(chi.URLParam(r, "fitID"), 10, 64)
	if err != nil || fitID <= 0 {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "That rating couldn't be read.", http.StatusBadRequest)
		return
	}
	stars, err := strconv.Atoi(strings.TrimSpace(r.Form.Get("stars")))
	if err != nil || stars < 1 || stars > 5 {
		http.Error(w, "Rate 1 to 5 stars.", http.StatusBadRequest)
		return
	}
	if _, err := app.queries.GetPublicFitting(ctx, fitID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		logging.Errorf("public fit %d: rate lookup: %v", fitID, err)
		http.Error(w, "That rating couldn't be saved.", http.StatusInternalServerError)
		return
	}
	tx, err := app.db.BeginTx(ctx, nil)
	if err != nil {
		logging.Errorf("public fit %d: rate tx: %v", fitID, err)
		http.Error(w, "That rating couldn't be saved.", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	qtx := app.queries.WithTx(tx)
	var dCount, dSum int64
	if old, err := qtx.GetFitRating(ctx, db.GetFitRatingParams{FitID: fitID, UserID: userID}); err == nil {
		dSum = int64(stars) - int64(old.Stars)
	} else if errors.Is(err, sql.ErrNoRows) {
		dCount = 1
		dSum = int64(stars)
	} else {
		logging.Errorf("public fit %d: rate read: %v", fitID, err)
		http.Error(w, "That rating couldn't be saved.", http.StatusInternalServerError)
		return
	}
	if err := qtx.UpsertFitRating(ctx, db.UpsertFitRatingParams{FitID: fitID, UserID: userID, Stars: int16(stars)}); err != nil {
		logging.Errorf("public fit %d: rate upsert: %v", fitID, err)
		http.Error(w, "That rating couldn't be saved.", http.StatusInternalServerError)
		return
	}
	if err := qtx.ApplyFitRatingAggregate(ctx, db.ApplyFitRatingAggregateParams{RatingCount: dCount, RatingSum: dSum, ID: fitID}); err != nil {
		logging.Errorf("public fit %d: rate aggregate: %v", fitID, err)
		http.Error(w, "That rating couldn't be saved.", http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		logging.Errorf("public fit %d: rate commit: %v", fitID, err)
		http.Error(w, "That rating couldn't be saved.", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, publicFitsPath+"fit/"+idString(fitID)+"/", http.StatusSeeOther)
}
