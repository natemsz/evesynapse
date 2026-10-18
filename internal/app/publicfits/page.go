package publicfits

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
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
	// publicFitsPerPage bounds every browser page; the total
	// riding the list query feeds the page links.
	publicFitsPerPage = 24
	// publicFitDefaultSort: every public fit, newest first.
	publicFitDefaultSort = "new"
)

// publicFitAges are the Updated filter's choices in days, in
// display order; anything else means any time.
var publicFitAges = []int{14, 30, 90}

// publicFitSorts are the browser's sort orders in display order;
// the first is the default (publicFitDefaultSort). Keys are
// allowlisted: only these ever reach the ORDER BY. No sort hides
// a fit: age is the Updated filter's business.
var publicFitSorts = []struct{ Key, Label string }{
	{publicFitDefaultSort, "Newest"},
	{"rank", "Rank"},
	{"views", "Most viewed"},
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
	// SinceDays keeps fits updated within that many days; 0 is
	// any time, the default.
	SinceDays int
	Sort      string
	Page      int
	Note      string
}

// parsePublicFitFilters reads the browser address; unknown sorts
// fall back to the default, bad numbers to blank.
func (app *page) parsePublicFitFilters(ctx context.Context, query url.Values) publicFitFilters {
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
		f.Sort = publicFitDefaultSort
	}
	known := false
	for _, s := range publicFitSorts {
		if s.Key == f.Sort {
			known = true
		}
	}
	if !known {
		f.Sort = publicFitDefaultSort
	}
	if days, err := strconv.Atoi(strings.TrimSpace(query.Get("updated"))); err == nil {
		for _, age := range publicFitAges {
			if days == age {
				f.SinceDays = days
			}
		}
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
func (app *page) resolveFitModuleType(ctx context.Context, name string) (int64, bool) {
	if name == "" {
		return 0, false
	}
	row, err := app.queries.GetSDETypeByName(ctx, name)
	if err != nil || row.TypeID <= 0 {
		return 0, false
	}
	return row.TypeID, true
}

// listParams renders the filters into the list query's shape; the
// filtered total rides every returned row (COUNT(*) OVER()).
func (f publicFitFilters) listParams(limit int64) db.ListPublicFitsParams {
	return db.ListPublicFitsParams{
		Sort: f.Sort, Q: f.Q, Role: f.Role, Hull: f.Hull, Hullq: f.HullQ,
		Minstars: f.MinStars, Include: f.Include, Exclude: f.Exclude,
		Faction: f.Faction, Maxprice: f.MaxPrice, Alpha: f.Alpha, Tag: f.Tag,
		Sincedays: int32(f.SinceDays),
		Rlimit:    limit, Roffset: int64(f.Page-1) * limit,
	}
}

// publicFitsView is the browser page: the filter bar's current
// state, facet chips, sort links, one bounded page of cards.
type publicFitsView struct {
	Filters     publicFitFilters
	Cards       []module.PublicFitCard
	Total       int64
	Page        int
	Pages       int
	HasPrev     bool
	HasNext     bool
	PrevURL     string
	NextURL     string
	FirstURL    string
	Roles       []string
	Sorts       []facetChip
	Hulls       []facetChip
	Tags        []facetChip
	Ages        []int // the Updated filter's choices, in days
	DefaultSort string
	SuggestURL  string
	Empty       string
	// For is the doctrine a keeper is choosing fits for, when
	// the page was opened from one (?for=, keepers only).
	For  *module.NamedChoice
	Self string
}

// browser describes the public browser's address, so its links come from
// one address builder. Defaults (newest first, any faction, page one) are
// left out of the address.
func (f publicFitFilters) browser(keepPage bool) *browserAddr {
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
	updated := ""
	if f.SinceDays > 0 {
		updated = strconv.Itoa(f.SinceDays)
	}
	page := ""
	if keepPage && f.Page > 1 {
		page = strconv.Itoa(f.Page)
	}
	faction := f.Faction
	if faction == "any" {
		faction = ""
	}
	filter := func(key, value string) addrFilter { return addrFilter{Key: key, Value: value} }
	return &browserAddr{
		Path: module.PublicFitsPath, Q: f.Q, Include: f.IncludeName, Exclude: f.ExcludeName,
		Sort: f.Sort, DefaultSort: publicFitSorts[0].Key,
		Filters: []addrFilter{
			filter("role", f.Role), filter("hull", f.Hull), filter("hullq", f.HullQ),
			filter("faction", faction), filter("tag", f.Tag), filter("updated", updated),
			filter("stars", num(f.MinStars)), filter("maxprice", num(f.MaxPrice)),
			filter("alpha", alpha), filter("page", page),
		},
	}
}

// browserAddr is the public browser's address: where it lives and the
// choices every link keeps.
type browserAddr struct {
	Path        string
	Q           string
	Include     string
	Exclude     string
	Sort        string
	DefaultSort string
	Filters     []addrFilter
}

type addrFilter struct{ Key, Value string }

// address is the browser's address with some choices changed; an empty
// value takes a choice off.
func (b *browserAddr) address(changes map[string]string) string {
	query := url.Values{}
	set := func(key, value string) {
		if changed, ok := changes[key]; ok {
			value = changed
		}
		if value != "" {
			query.Set(key, value)
		}
	}
	set("q", b.Q)
	set("include", b.Include)
	set("exclude", b.Exclude)
	if b.Sort != b.DefaultSort {
		set("sort", b.Sort)
	} else {
		set("sort", "")
	}
	for _, f := range b.Filters {
		set(f.Key, f.Value)
	}
	if len(query) == 0 {
		return b.Path
	}
	return b.Path + "?" + query.Encode()
}

// facetChip is one link in the browser: a filter value with its count,
// a sort order, or a chosen filter to take off again.
type facetChip struct {
	Label string
	Count int
	URL   string
	On    bool
}

// publicFitURL is the browser address with some choices changed;
// empty values take a choice off, page resets unless kept. A change
// to a default value takes the choice off too.
func (f publicFitFilters) publicFitURL(changes map[string]string, keepPage bool) string {
	norm := make(map[string]string, len(changes))
	for k, v := range changes {
		if (k == "sort" && v == publicFitDefaultSort) || (k == "faction" && v == "any") {
			v = ""
		}
		norm[k] = v
	}
	return f.browser(keepPage).address(norm)
}

// handlePublicFits renders the anonymous fit browser: one bounded
// SQL page of stored rows, facet chips from bounded aggregate
// reads, no per-user queries when nobody is signed in.
func (app *page) handlePublicFits(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := &PageData{ShellData: app.deps.PublicShell(ctx)}
	data.HeadMeta = app.deps.HeadMeta(module.PageMeta{
		Title:         "Public Fits",
		Description:   "Browse community EVE Online ship fittings shared by their authors, with prices and stats for every loadout.",
		CanonicalPath: "/fittings/public/",
	})
	userID := app.deps.UserID(ctx)
	filters := app.parsePublicFitFilters(ctx, r.URL.Query())

	// One query serves the page: the filtered total rides every row
	// (COUNT(*) OVER()). A page past the end returns no rows to read
	// the total from, so probe the first page once, then clamp and
	// list the clamped page — the same page the old count-first flow
	// rendered for an out-of-range request.
	rows, err := app.queries.ListPublicFits(ctx, filters.listParams(publicFitsPerPage))
	if err != nil {
		logging.Errorf("public fits: list: %v", err)
		data.Error = "Could not load public fits; check the server log."
		app.deps.Render(ctx, w, http.StatusOK, "fittings_public.html", data)
		return
	}
	var total int64
	var firstPage []db.ListPublicFitsRow
	if len(rows) > 0 {
		total = rows[0].TotalCount
	} else if filters.Page > 1 {
		probe := filters
		probe.Page = 1
		firstPage, err = app.queries.ListPublicFits(ctx, probe.listParams(publicFitsPerPage))
		if err != nil {
			logging.Errorf("public fits: list: %v", err)
			data.Error = "Could not load public fits; check the server log."
			app.deps.Render(ctx, w, http.StatusOK, "fittings_public.html", data)
			return
		}
		if len(firstPage) > 0 {
			total = firstPage[0].TotalCount
		}
	}
	pages := int((total + publicFitsPerPage - 1) / publicFitsPerPage)
	if pages < 1 {
		pages = 1
	}
	if filters.Page > pages {
		filters.Page = pages
		if filters.Page == 1 && firstPage != nil {
			rows = firstPage
		} else {
			rows, err = app.queries.ListPublicFits(ctx, filters.listParams(publicFitsPerPage))
			if err != nil {
				logging.Errorf("public fits: list: %v", err)
				data.Error = "Could not load public fits; check the server log."
				app.deps.Render(ctx, w, http.StatusOK, "fittings_public.html", data)
				return
			}
		}
	}
	page := filters.Page
	cards := make([]module.PublicFitCard, 0, len(rows))
	for _, row := range rows {
		cards = append(cards, module.PublicFitCardOf(row, userID))
	}
	// doctrineChosenFor returns nil unless the account keeps the
	// doctrine, so a non-nil For is the keeper flag itself.
	chosen := app.deps.DoctrineChosenFor(ctx, userID, r.URL.Query().Get("for"))
	view := &publicFitsView{
		Filters: filters, Cards: cards, Total: total, Page: page, Pages: pages,
		Roles: app.deps.FleetRoles(),
		Ages:  publicFitAges, DefaultSort: publicFitDefaultSort,
		SuggestURL: module.PublicFitsPath + "suggest",
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
		view.Sorts = append(view.Sorts, facetChip{
			Label: s.Label, On: s.Key == filters.Sort,
			URL: filters.publicFitURL(map[string]string{"sort": s.Key}, false),
		})
	}
	if hulls, err := app.queries.PublicFitHullFacets(ctx); err == nil {
		for _, h := range hulls {
			chip := facetChip{Label: h.Hull, Count: int(h.Fits), On: h.Hull == filters.Hull}
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
			chip := facetChip{Label: tg.Tag, Count: int(tg.Fits), On: strings.EqualFold(tg.Tag, filters.Tag)}
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
	app.deps.Render(ctx, w, http.StatusOK, "fittings_public.html", data)
}

// handlePublicFitSuggest feeds the public browser search box
// (GET /fittings/public/suggest?q=): one bounded SQL read.
func (app *page) handlePublicFitSuggest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.deps.UserID(ctx)
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(q) < 2 {
		app.deps.WriteSuggest(w, nil)
		return
	}
	rows, err := app.queries.SearchPublicFittings(ctx, db.SearchPublicFittingsParams{Q: q, UserID: userID})
	if err != nil {
		logging.Errorf("public fits: suggest: %v", err)
		app.deps.WriteSuggest(w, nil)
		return
	}
	out := make([]module.SuggestItem, 0, len(rows))
	for _, row := range rows {
		ship := row.ShipName
		if ship == "" {
			ship = app.deps.TypeNameOrID(ctx, row.ShipTypeID)
		}
		label := ship
		if author := row.AuthorName; author != "" {
			label += " · by " + author
		}
		out = append(out, module.SuggestItem{
			Name: row.Name, Label: label,
			URL: module.PublicFitsPath + "fit/" + idString(row.ID) + "/",
		})
	}
	app.deps.WriteSuggest(w, out)
}

// publicFitView is one stable public fit page: the stored fit,
// its modules grouped server-side, the EFT text, the rating
// state, and absolute share links. Sim is the same All-V
// workbench view the fitting tool renders, shown read-only.
// Sections, Sim and EFT come from the stored page.
type publicFitView struct {
	Card        module.PublicFitCard
	Description string
	Sections    any // the stored page's module groups (core's fitting view)
	Sim         any // the stored read-only workbench view
	EFT         string
	AvgStars    float64
	StarCount   int64
	MyStars     int64
	PageURL     string
	// Preparing: the stored page for this version of the fit is
	// not built yet (the worker builds it within a cycle or two).
	Preparing    bool
	ShareDiscord string // a ready-to-paste Discord message: the fit, its ship and the link (Discord has no share URL)
	ShareReddit  string
}

// handlePublicFitPage renders one stable public fit page:
// server-rendered modules, EFT, share links and the rating
// widget. Anonymous readers see everything but the rate form.
func (app *page) handlePublicFitPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := &PageData{ShellData: app.deps.PublicShell(ctx)}
	userID := app.deps.UserID(ctx)

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
		app.deps.Render(ctx, w, http.StatusOK, "fit_public.html", data)
		return
	}
	// Counted in memory; the worker writes views in batches. A
	// prefetch is not a view (core_prefetch.go).
	views := row.ViewCount + app.deps.FitViewsPending(ctx, fitID)
	var doc fitDoc
	if json.Unmarshal([]byte(row.ItemsJson), &doc) != nil {
		http.NotFound(w, r)
		return
	}
	ship := app.deps.TypeNameOrID(ctx, row.ShipTypeID)
	author := row.AuthorName
	// The page prices the fit now, the same way the fitting tool's
	// stats panel does: hull plus every line, one composition.
	if doc.ShipTypeID == 0 {
		doc.ShipTypeID = row.ShipTypeID
	}
	body := app.deps.PublicFitBody(ctx, fitID, row.UpdatedAt, row.ItemsJson, row.ShipTypeID)
	card := module.PublicFitCard{
		ID: row.ID, Name: row.Name, Ship: ship, ShipTypeID: row.ShipTypeID,
		Author: author, Mine: userID > 0 && row.UserID == userID,
		Views: views, Price: body.PriceRaw, PriceKnown: body.PriceKnown,
		Updated: row.UpdatedAt, FitURL: module.PublicFitsPath + "fit/" + idString(row.ID) + "/",
		Tags: doc.Tags,
	}
	if body.PriceKnown {
		card.PriceLine = body.PriceTotal + " ISK"
	}
	if row.RatingCount > 0 {
		card.Stars = float64(row.RatingSum) / float64(row.RatingCount)
		card.StarCount = row.RatingCount
	}
	view := &publicFitView{
		Card: card, Description: doc.Description,
		AvgStars: card.Stars, StarCount: card.StarCount,
		PageURL: app.deps.PublicPageURL(card.FitURL),
	}
	// The modules, simulation and EFT come from the stored page the
	// fitting side keeps; a view never runs the engine.
	view.Sections, view.Sim, view.EFT, view.Preparing = body.Sections, body.Sim, body.EFT, body.Preparing
	// Links out need an absolute address to point back at.
	if view.PageURL != "" {
		view.ShareDiscord = row.Name + " (" + ship + ") — " + view.PageURL
		view.ShareReddit = "https://www.reddit.com/submit?url=" + url.QueryEscape(view.PageURL) + "&title=" + url.QueryEscape(row.Name+" ("+ship+")")
	}
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
	data.HeadMeta = app.deps.HeadMeta(module.PageMeta{
		Title:        row.Name + " (" + ship + ")",
		Description:  blurb,
		CanonicalURL: view.PageURL,
	})
	data.PublicFit = view
	app.deps.Render(ctx, w, http.StatusOK, "fit_public.html", data)
}

// handlePublicFitRate records one account's rating of a public
// fit (POST, login required): re-rating updates the same row,
// and the fit's aggregates are recounted in the same transaction.
func (app *page) handlePublicFitRate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.deps.UserID(ctx)
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
	tx, err := app.deps.SQLDB().BeginTx(ctx, nil)
	if err != nil {
		logging.Errorf("public fit %d: rate tx: %v", fitID, err)
		http.Error(w, "That rating couldn't be saved.", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	qtx := app.queries.WithTx(tx)
	// The fit row is locked first, so ratings of one fit never
	// interleave, and the aggregates are then recounted from the
	// rating rows rather than moved by a delta. However often one
	// rating is posted, the fit counts it once.
	if _, err := qtx.LockPublicFitForRating(ctx, fitID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		logging.Errorf("public fit %d: rate lookup: %v", fitID, err)
		http.Error(w, "That rating couldn't be saved.", http.StatusInternalServerError)
		return
	}
	if err := qtx.UpsertFitRating(ctx, db.UpsertFitRatingParams{FitID: fitID, UserID: userID, Stars: int16(stars)}); err != nil {
		logging.Errorf("public fit %d: rate upsert: %v", fitID, err)
		http.Error(w, "That rating couldn't be saved.", http.StatusInternalServerError)
		return
	}
	if err := qtx.RecountFitRatings(ctx, fitID); err != nil {
		logging.Errorf("public fit %d: rate aggregate: %v", fitID, err)
		http.Error(w, "That rating couldn't be saved.", http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		logging.Errorf("public fit %d: rate commit: %v", fitID, err)
		http.Error(w, "That rating couldn't be saved.", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, module.PublicFitsPath+"fit/"+idString(fitID)+"/", http.StatusSeeOther)
}

// fitDoc is the part of a stored fit document this module reads: the
// description and tags a page shows. The fit itself is the fitting
// side's business.
type fitDoc struct {
	Description string   `json:"description,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	ShipTypeID  int64    `json:"shipTypeId"`
}

func idString(id int64) string { return strconv.FormatInt(id, 10) }
