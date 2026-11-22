package doctrines

import (
	"context"
	"database/sql"
	"errors"
	"evesynapse/internal/app/module"
	"evesynapse/internal/humanize"
	"fmt"
	json "github.com/goccy/go-json"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/fit"
	"evesynapse/internal/fitview"
	"evesynapse/internal/logging"
	"evesynapse/internal/standing"
	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------------------
// Doctrines.
//
// A doctrine is a named set of fits for a kind of fleet, owned by a
// corporation, alliance or community. The owner's members can read it,
// and a keeper can open it to anyone with its link. Whoever runs the
// owner keeps its doctrines, with whoever they hand that to
// (owner_permissions.go).
//
// A fit in a doctrine is a copy of a fit document from the fitting
// tool: one of the keeper's saved fits, a public fit, a fit from
// another doctrine, or pasted EFT text. Being a copy, it opens in the
// fitting tool for anyone who can read the doctrine, whether or not
// the fit it came from is public or still exists.
//
// A fleet can fly a doctrine (fleets.go) and keeps a copy of it from
// its start, and ship replacement shows whether a lost ship was in
// that copy.
// ---------------------------------------------------------------------------

const doctrinesPath = "/doctrines/"

const (
	doctrineNameMax     = 80
	doctrineCategoryMax = 40
	doctrineTextMax     = 2000
	doctrineNoteMax     = 200
	doctrineTagsMost    = 12
	doctrineFitsMost    = 40
	doctrineEFTMax      = 16 << 10
)

// doctrineTags reads a tags field into the stored form: the tags as
// typed, comma-separated, without repeats.
func doctrineTags(raw string) string {
	tags := fitview.ParseFitTags(raw)
	if len(tags) > doctrineTagsMost {
		tags = tags[:doctrineTagsMost]
	}
	return strings.Join(tags, ",")
}

func doctrineTagList(stored string) []string {
	if stored == "" {
		return nil
	}
	return strings.Split(stored, ",")
}

// hasTag reports whether tags hold one, whatever its case.
func hasTag(tags []string, want string) bool {
	for _, tag := range tags {
		if strings.EqualFold(tag, want) {
			return true
		}
	}
	return false
}

// doctrineCard is one doctrine in a list.
type doctrineCard struct {
	ID          int64
	Name        string
	Category    string
	Description string
	Tags        []string
	Fits        []module.DoctrineFitView
}

// doctrineOwnerView is the owner a doctrines page is about.
type doctrineOwnerView struct {
	Key     string
	Kind    string
	Name    string
	Manages bool
	// HandOver is where the people who run the owner choose who else
	// keeps its doctrines; empty for anyone else.
	HandOver  string
	Total     int // doctrines before the page's filters
	Doctrines []doctrineCard
}

// doctrinesView is the /doctrines/ page: one owner's doctrines.
type doctrinesView struct {
	OwnerOptions []module.OwnerOption
	Owner        *doctrineOwnerView // nil when the account belongs to no owner
	Q            string
	Category     string
	Tag          string
	Filtered     bool
	Categories   []string
	Tags         []string
}

// doctrineFitViews turns stored fits into rows, by doctrine.
// matches reports whether a doctrine passes the page's filters: the
// words in its name, category, description, tags, or the name or ship
// of one of its fits.
func (c doctrineCard) matches(q, category, tag string) bool {
	if category != "" && !strings.EqualFold(c.Category, category) {
		return false
	}
	if tag != "" && !hasTag(c.Tags, tag) {
		return false
	}
	if q == "" {
		return true
	}
	hay := []string{c.Name, c.Category, c.Description, strings.Join(c.Tags, " ")}
	for _, fit := range c.Fits {
		hay = append(hay, fit.Name, fit.Ship)
	}
	return strings.Contains(strings.ToLower(strings.Join(hay, "\n")), strings.ToLower(q))
}

func (app *mod) handleDoctrines(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := &PageData{ShellData: app.deps.PageShell(ctx)}
	userID := app.deps.UserID(ctx)
	query := r.URL.Query()
	view := &doctrinesView{
		Q: strings.TrimSpace(query.Get("q")), Category: strings.TrimSpace(query.Get("category")), Tag: strings.TrimSpace(query.Get("tag")),
	}
	view.Filtered = view.Q != "" || view.Category != "" || view.Tag != ""
	data.Doctrines = view

	owner, options, found := app.deps.PickOwner(ctx, r, app.deps.DoctrineOwners(ctx, userID))
	if !found {
		app.deps.Render(ctx, w, http.StatusOK, "doctrines.html", data)
		return
	}
	st, err := app.deps.StandingFor(ctx, userID)
	if err != nil {
		logging.Errorf("doctrines: standing of user %d: %v", userID, err)
		data.Error = "Could not load the doctrines; check the server log."
	}
	cv := &doctrineOwnerView{Key: owner.Key(), Kind: owner.Kind, Name: app.deps.OwnerName(ctx, owner)}
	cv.Manages = app.deps.OwnerPermits(ctx, st, owner, permDoctrines)
	if app.deps.HoldsProgramme(ctx, st, owner, permDoctrines) {
		cv.HandOver = standing.Address("/fleets/permissions/", owner.Key())
		if owner.Kind == standing.OwnerCorporation {
			cv.HandOver = module.CorpAddress("/corporations/settings/", owner.ID)
		}
	}
	view.OwnerOptions, view.Owner = options, cv

	doctrines, err := app.queries.ListDoctrinesForOwner(ctx, db.ListDoctrinesForOwnerParams{OwnerKind: owner.Kind, OwnerID: owner.ID})
	if err != nil {
		logging.Errorf("doctrines: list for %s: %v", owner.Key(), err)
	}
	ids := make([]int64, 0, len(doctrines))
	for _, d := range doctrines {
		ids = append(ids, d.ID)
	}
	fits := app.deps.DoctrineFitViews(ctx, ids)
	categories, tags := map[string]bool{}, map[string]bool{}
	for _, d := range doctrines {
		cv.Total++
		card := doctrineCard{ID: d.ID, Name: d.Name, Category: d.Category, Description: d.Description, Tags: doctrineTagList(d.Tags), Fits: fits[d.ID]}
		if d.Category != "" {
			categories[d.Category] = true
		}
		for _, tag := range card.Tags {
			tags[tag] = true
		}
		if card.matches(view.Q, view.Category, view.Tag) {
			cv.Doctrines = append(cv.Doctrines, card)
		}
	}
	view.Categories, view.Tags = sortedKeys(categories), sortedKeys(tags)
	app.deps.Render(ctx, w, http.StatusOK, "doctrines.html", data)
}

// handleDoctrineSuggest feeds the search boxes of the doctrine pages
// (GET /doctrines/suggest?scope=&owner=&q=). What it offers depends on
// the box: doctrines and what they are filed under, an owner's fits,
// its categories, or the account's own saved fits. An owner's names go
// only to an account that belongs to it.
func (app *mod) handleDoctrineSuggest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.deps.UserID(ctx)
	query := r.URL.Query()
	q := strings.TrimSpace(query.Get("q"))
	needle := strings.ToLower(q)
	var out []module.SuggestItem
	if len(q) < 2 {
		app.deps.WriteSuggest(w, out)
		return
	}
	if query.Get("scope") == "myfits" {
		rows, err := app.queries.SearchLocalFittings(ctx, db.SearchLocalFittingsParams{UserID: userID, Q: q})
		if err != nil {
			logging.Errorf("doctrines: suggest saved fits: %v", err)
		}
		for _, row := range rows {
			if !row.IsDraft {
				out = append(out, module.SuggestItem{ID: row.ID, Name: row.Name, Label: row.ShipName + " · saved here", Value: "local:" + idString(row.ID)})
			}
		}
		for _, mine := range app.accountEVEFits(ctx, userID) {
			ship := app.deps.TypeNameOrID(ctx, mine.fit.ShipTypeID)
			if len(out) < suggestBrowseMost && strings.Contains(strings.ToLower(mine.fit.Name+"\n"+ship), needle) {
				out = append(out, module.SuggestItem{
					ID: mine.fit.FittingID, Name: mine.fit.Name, Label: ship + " · in game on " + mine.character.Name,
					Value: "eve:" + idString(mine.character.CharacterID) + ":" + idString(mine.fit.FittingID),
				})
			}
		}
		app.deps.WriteSuggest(w, out)
		return
	}

	owner, named := submittedOwner(query.Get("owner"), query.Get("corporation"))
	if !named || !standing.Includes(app.deps.DoctrineOwners(ctx, userID), owner) {
		app.deps.WriteSuggest(w, out)
		return
	}
	if query.Get("scope") == "fits" {
		browser, cards := app.corpFitBrowser(ctx, owner, app.deps.DoctrineChosenFor(ctx, userID, query.Get("for")), url.Values{})
		app.deps.WriteSuggest(w, browser.suggestFits(cards, q))
		return
	}

	doctrines, err := app.queries.ListDoctrinesForOwner(ctx, db.ListDoctrinesForOwnerParams{OwnerKind: owner.Kind, OwnerID: owner.ID})
	if err != nil {
		logging.Errorf("doctrines: suggest for %s: %v", owner.Key(), err)
	}
	seen := map[string]bool{}
	add := func(name, label, address string) {
		key := label + "\n" + strings.ToLower(name)
		if name != "" && !seen[key] && len(out) < suggestBrowseMost && strings.Contains(strings.ToLower(name), needle) {
			seen[key] = true
			out = append(out, module.SuggestItem{Name: name, Label: label, URL: address})
		}
	}
	list := standing.Address(doctrinesPath, owner.Key())
	if query.Get("scope") == "categories" {
		for _, d := range doctrines {
			add(d.Category, "Category", "")
		}
		app.deps.WriteSuggest(w, out)
		return
	}
	ids := make([]int64, 0, len(doctrines))
	for _, d := range doctrines {
		ids = append(ids, d.ID)
		label := "Doctrine"
		if d.Category != "" {
			label = "Doctrine · " + d.Category
		}
		add(d.Name, label, module.DoctrineURL(d.ID))
	}
	fits := app.deps.DoctrineFitViews(ctx, ids)
	for _, d := range doctrines {
		for _, fit := range fits[d.ID] {
			add(fit.Name, "Fit in "+d.Name, module.DoctrineURL(d.ID))
			add(fit.Ship, "Ship", list+"&q="+url.QueryEscape(fit.Ship))
		}
	}
	for _, d := range doctrines {
		add(d.Category, "Category", list+"&category="+url.QueryEscape(d.Category))
		for _, tag := range doctrineTagList(d.Tags) {
			add(tag, "Tag", list+"&tag="+url.QueryEscape(tag))
		}
	}
	app.deps.WriteSuggest(w, out)
}

// submittedOwner reads the owner a form or address names: its key, or
// a corporation id from an older link.
func submittedOwner(key, corporation string) (standing.Owner, bool) {
	if owner, ok := standing.ParseAnyOwner(key); ok {
		return owner, true
	}
	if id, err := strconv.ParseInt(corporation, 10, 64); err == nil && id > 0 {
		return standing.CorporationOwner(id), true
	}
	return standing.Owner{}, false
}

// doctrineVisibility reads who a form says can read a doctrine; anything
// but an explicit choice to open it stays with the members.
func doctrineVisibility(raw string) string {
	if raw == module.DoctrinePublic {
		return module.DoctrinePublic
	}
	return module.DoctrineMembers
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i]) < strings.ToLower(out[j]) })
	return out
}

// doctrineFields reads and checks the fields a doctrine is made of.
func doctrineFields(r *http.Request) (name, category, description, tags, problem string) {
	name = strings.Join(strings.Fields(r.Form.Get("name")), " ")
	category = strings.Join(strings.Fields(r.Form.Get("category")), " ")
	description = strings.TrimSpace(r.Form.Get("description"))
	tags = doctrineTags(r.Form.Get("tags"))
	switch {
	case name == "" || len(name) > doctrineNameMax:
		problem = fmt.Sprintf("Give the doctrine a name of up to %d characters.", doctrineNameMax)
	case len(category) > doctrineCategoryMax || len(description) > doctrineTextMax:
		problem = fmt.Sprintf("Keep the category under %d characters and the description under %d.", doctrineCategoryMax, doctrineTextMax)
	}
	return
}

// handleDoctrineCreate makes a doctrine for an owner
// (POST /doctrines/create).
func (app *mod) handleDoctrineCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.deps.UserID(ctx)
	_ = r.ParseForm()
	owner, named := submittedOwner(r.Form.Get("owner"), r.Form.Get("corporation"))
	path := doctrinesPath
	if named {
		path = standing.Address(doctrinesPath, owner.Key())
	}
	back := app.deps.FlashBack(w, r, path)
	st, err := app.deps.StandingFor(ctx, userID)
	if err != nil || !named || !app.deps.OwnerPermits(ctx, st, owner, permDoctrines) {
		back("Those doctrines are not yours to keep.")
		return
	}
	name, category, description, tags, problem := doctrineFields(r)
	if problem != "" {
		back(problem)
		return
	}
	corp := int64(0)
	if owner.Kind == standing.OwnerCorporation {
		corp = owner.ID
	}
	id, err := app.queries.CreateDoctrine(ctx, db.CreateDoctrineParams{
		CorporationID: corp, OwnerKind: owner.Kind, OwnerID: owner.ID, Visibility: doctrineVisibility(r.Form.Get("visibility")),
		Name: name, Category: category, Description: description, Tags: tags,
		CreatedBy: app.deps.OwnerActorFor(ctx, userID, owner), CreatedAt: time.Now().UTC(),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		back("There is already a doctrine called " + name + ".")
		return
	}
	if err != nil {
		logging.Errorf("doctrines: create for %s: %v", owner.Key(), err)
		back("The doctrine could not be saved; check the server log.")
		return
	}
	logging.Infof("doctrines: user %d made doctrine %d (%s) for %s", userID, id, name, owner.Key())
	app.deps.Flash(ctx, "Doctrine made. Add its fits below.")
	http.Redirect(w, r, module.DoctrineURL(id), http.StatusSeeOther)
}

// doctrineOwnerOf is who a doctrine belongs to.
func doctrineOwnerOf(d db.Doctrine) standing.Owner {
	return standing.Owner{Kind: d.OwnerKind, ID: d.OwnerID}
}

// doctrineAccess loads the doctrine named in the address and says what
// the session may do with it: read it and keep it. Keeping takes an
// account.
func (app *mod) doctrineAccess(r *http.Request) (d db.Doctrine, reads, keeps bool) {
	ctx := r.Context()
	id, _ := strconv.ParseInt(chi.URLParam(r, "doctrineID"), 10, 64)
	d, err := app.queries.GetDoctrine(ctx, id)
	if err != nil {
		return d, false, false
	}
	userID := app.deps.UserID(ctx)
	owner := doctrineOwnerOf(d)
	reads = app.deps.ReadsDoctrine(ctx, userID, owner, d.Visibility)
	if userID == 0 {
		return d, reads, false
	}
	st, err := app.deps.StandingFor(ctx, userID)
	if err != nil {
		return d, reads, false
	}
	return d, reads, app.deps.OwnerPermits(ctx, st, owner, permDoctrines)
}

// doctrineView is one doctrine's own page.
type doctrineView struct {
	doctrineCard
	TagsText string
	Owner    string
	OwnerKey string
	// Member: the viewer belongs to the owner, so its list is theirs.
	Member bool
	Public bool
	// ShareURL is the address to pass on, for a keeper of a public doctrine.
	ShareURL string
	Keeps    bool
	Full     bool // no room for another fit
	Roles    []string
}

func (app *mod) handleDoctrine(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	w.Header().Set("X-Robots-Tag", "noindex")
	d, reads, keeps := app.doctrineAccess(r)
	userID := app.deps.UserID(ctx)
	if !reads {
		// The same answer whether it is private or does not exist.
		if userID == 0 {
			http.NotFound(w, r)
			return
		}
		app.deps.Flash(ctx, "That doctrine is private, or does not exist.")
		http.Redirect(w, r, doctrinesPath, http.StatusSeeOther)
		return
	}
	owner := doctrineOwnerOf(d)
	data := &PageData{ShellData: app.deps.PublicShell(ctx)}
	view := &doctrineView{
		ID: d.ID, Name: d.Name, Category: d.Category, Description: d.Description, Tags: doctrineTagList(d.Tags),
		TagsText: strings.Join(doctrineTagList(d.Tags), ", "),
		Owner:    app.deps.OwnerName(ctx, owner), OwnerKey: owner.Key(), Keeps: keeps, Roles: app.deps.FleetRoles(),
		Member: userID != 0 && standing.Includes(app.deps.DoctrineOwners(ctx, userID), owner),
		Public: d.Visibility == module.DoctrinePublic,
	}
	if view.Public && keeps {
		view.ShareURL = app.deps.PublicPageURL(module.DoctrineURL(d.ID))
		if view.ShareURL == "" {
			view.ShareURL = module.DoctrineURL(d.ID)
		}
	}
	data.Doctrine = view
	fits, err := app.queries.ListDoctrineFits(ctx, []int64{d.ID})
	if err != nil {
		logging.Errorf("doctrines: fits of %d: %v", d.ID, err)
	}
	for _, fit := range fits {
		fv := module.DoctrineFitView{
			ID: fit.ID, Name: fit.Name, Ship: app.deps.TypeNameOrID(ctx, fit.ShipTypeID), ShipTypeID: fit.ShipTypeID,
			Role: fit.FleetRole, Note: fit.Note,
		}
		var doc fitview.FitDoc
		if json.Unmarshal([]byte(fit.ItemsJson), &doc) == nil {
			doc.Name = fit.Name
			fv.EFT = app.deps.FitBuilder().FormatEFT(ctx, &doc)
		}
		view.Fits = append(view.Fits, fv)
	}
	view.Full = len(view.Fits) >= doctrineFitsMost
	app.deps.Render(ctx, w, http.StatusOK, "doctrine.html", data)
}

// keptDoctrine loads the doctrine named in the address for an account
// that keeps it; anyone else is sent back with a word.
func (app *mod) keptDoctrine(w http.ResponseWriter, r *http.Request) (db.Doctrine, bool) {
	d, _, keeps := app.doctrineAccess(r)
	if !keeps {
		app.deps.FlashBack(w, r, doctrinesPath)("That doctrine is not yours to change.")
		return d, false
	}
	// Fleets already flying it keep it as it stands now.
	if err := app.deps.SnapshotFleetsOfDoctrine(r.Context(), d.ID); err != nil {
		logging.Errorf("doctrines: keep fleets' copy of %d: %v", d.ID, err)
		app.deps.FlashBack(w, r, module.DoctrineURL(d.ID))("The doctrine could not be changed. Try again in a moment.")
		return d, false
	}
	return d, true
}

func (app *mod) handleDoctrineSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_ = r.ParseForm()
	d, ok := app.keptDoctrine(w, r)
	if !ok {
		return
	}
	back := app.deps.FlashBack(w, r, module.DoctrineURL(d.ID))
	name, category, description, tags, problem := doctrineFields(r)
	if problem != "" {
		back(problem)
		return
	}
	n, err := app.queries.UpdateDoctrine(ctx, db.UpdateDoctrineParams{
		ID: d.ID, Name: name, Category: category, Description: description, Tags: tags,
		Visibility: doctrineVisibility(r.Form.Get("visibility")), UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		logging.Errorf("doctrines: save %d: %v", d.ID, err)
		back("The doctrine could not be saved; check the server log.")
		return
	}
	if n == 0 {
		back("There is already a doctrine called " + name + ".")
		return
	}
	back("Saved.")
}

func (app *mod) handleDoctrineDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	d, ok := app.keptDoctrine(w, r)
	if !ok {
		return
	}
	if err := app.queries.DeleteDoctrine(ctx, d.ID); err != nil {
		logging.Errorf("doctrines: delete %d: %v", d.ID, err)
	}
	logging.Infof("doctrines: user %d deleted doctrine %d (%s)", app.deps.UserID(ctx), d.ID, d.Name)
	app.deps.FlashBack(w, r, standing.Address(doctrinesPath, doctrineOwnerOf(d).Key()))("Deleted " + d.Name + ". Fleets that named it keep its name.")
}

// doctrineFitSource finds the fit document a keeper is adding: one of
// their own saved fits, a public fit, a fit of a doctrine they can
// read, or pasted EFT text (skipped counts its lines that were not
// understood).
func (app *mod) doctrineFitSource(r *http.Request) (doc *fitview.FitDoc, problem string, skipped int) {
	ctx := r.Context()
	userID := app.deps.UserID(ctx)
	read := func(raw string) (*fitview.FitDoc, string, int) {
		var doc fitview.FitDoc
		if json.Unmarshal([]byte(raw), &doc) != nil {
			return nil, "That fit could not be read.", 0
		}
		return &doc, "", 0
	}
	if id, _ := strconv.ParseInt(r.Form.Get("fit"), 10, 64); id > 0 {
		row, err := app.queries.GetLocalFitting(ctx, db.GetLocalFittingParams{ID: id, UserID: userID})
		if err != nil {
			return nil, "That is not one of your saved fits.", 0
		}
		return read(row.ItemsJson)
	}
	// Picked from the suggestions: a fit saved here, or an in-game fit
	// of one of the account's characters.
	if pick := r.Form.Get("pick"); pick != "" {
		kind, rest, _ := strings.Cut(pick, ":")
		switch kind {
		case "local":
			id, _ := strconv.ParseInt(rest, 10, 64)
			if row, err := app.queries.GetLocalFitting(ctx, db.GetLocalFittingParams{ID: id, UserID: userID}); err == nil {
				return read(row.ItemsJson)
			}
		case "eve":
			character, fitting, _ := strings.Cut(rest, ":")
			for _, mine := range app.accountEVEFits(ctx, userID) {
				if idString(mine.character.CharacterID) == character && idString(mine.fit.FittingID) == fitting {
					return app.deps.FitBuilder().FitDocFromESI(ctx, mine.fit), "", 0
				}
			}
		}
		return nil, "That fit is not one of yours any more. Pick it from the list again.", 0
	}
	// Typed, not picked: the fit of that name, saved here or in game.
	if name := strings.TrimSpace(r.Form.Get("fit_name")); name != "" {
		rows, _ := app.queries.SearchLocalFittings(ctx, db.SearchLocalFittingsParams{UserID: userID, Q: name})
		for _, row := range rows {
			if strings.EqualFold(row.Name, name) && !row.IsDraft {
				return read(row.ItemsJson)
			}
		}
		for _, mine := range app.accountEVEFits(ctx, userID) {
			if strings.EqualFold(mine.fit.Name, name) {
				return app.deps.FitBuilder().FitDocFromESI(ctx, mine.fit), "", 0
			}
		}
		return nil, "None of your fits is called " + name + ". Pick one from the list.", 0
	}
	if id, _ := strconv.ParseInt(r.Form.Get("public"), 10, 64); id > 0 {
		row, err := app.queries.GetPublicFitting(ctx, id)
		if err != nil {
			return nil, "That public fit is gone.", 0
		}
		return read(row.ItemsJson)
	}
	if id, _ := strconv.ParseInt(r.Form.Get("copy"), 10, 64); id > 0 {
		if doc, ok := app.deps.DoctrineFitDoc(ctx, userID, id); ok {
			return doc, "", 0
		}
		return nil, "That fit is not in a doctrine you can read.", 0
	}
	if text := strings.TrimSpace(r.Form.Get("eft")); text != "" {
		if len(text) > doctrineEFTMax {
			return nil, "That is too long to be one fit.", 0
		}
		doc, notes := app.deps.FitBuilder().ParseEFT(ctx, text)
		return doc, "", len(notes)
	}
	return nil, "Pick a fit, or paste one.", 0
}

// eveFit is one in-game fitting of one of an account's characters.
type eveFit struct {
	character db.Character
	fit       esi.Fitting
}

// accountEVEFits lists the in-game fittings of an account's
// characters, from stored data only.
func (app *mod) accountEVEFits(ctx context.Context, userID int64) []eveFit {
	characters, err := app.queries.ListCharactersByUser(ctx, userID)
	if err != nil {
		return nil
	}
	var out []eveFit
	for _, ch := range characters {
		var fittings esi.Fittings
		if app.deps.LoadCorpSnapshot(ctx, ch.CharacterID, esi.SnapFittings, &fittings) {
			for _, fit := range fittings {
				out = append(out, eveFit{character: ch, fit: fit})
			}
		}
	}
	return out
}

// handleDoctrineFitAdd copies a fit into a doctrine
// (POST /doctrines/{doctrineID}/fits/add).
func (app *mod) handleDoctrineFitAdd(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.deps.UserID(ctx)
	_ = r.ParseForm()
	d, ok := app.keptDoctrine(w, r)
	if !ok {
		return
	}
	path := module.DoctrineURL(d.ID)
	if next := r.Form.Get("next"); strings.HasPrefix(next, doctrineFitsPath+"?") || strings.HasPrefix(next, module.PublicFitsPath+"?") {
		path = next
	}
	back := app.deps.FlashBack(w, r, path)
	if n, err := app.queries.CountDoctrineFits(ctx, d.ID); err != nil || n >= doctrineFitsMost {
		back(fmt.Sprintf("A doctrine holds up to %d fits.", doctrineFitsMost))
		return
	}
	doc, problem, skipped := app.doctrineFitSource(r)
	if problem != "" {
		back(problem)
		return
	}
	fitview.SanitizeFitDoc(doc)
	if doc.ShipTypeID <= 0 {
		back("That fit has no ship EveSynapse knows. Pasted text has to start with a line like [Guardian, Fleet logi].")
		return
	}
	role := r.Form.Get("fleet_role")
	if !app.validFleetRole(role) {
		role = ""
	}
	name := humanize.Clip(strings.Join(strings.Fields(doc.Name), " "), doctrineNameMax)
	if name == "" {
		name = app.deps.TypeNameOrID(ctx, doc.ShipTypeID)
	}
	doc.Name = name
	raw, err := json.Marshal(doc)
	if err == nil {
		_, err = app.queries.AddDoctrineFit(ctx, db.AddDoctrineFitParams{
			DoctrineID: d.ID, Name: name, ShipTypeID: doc.ShipTypeID, ItemsJson: string(raw), FleetRole: role,
			Note:    humanize.Clip(strings.TrimSpace(r.Form.Get("note")), doctrineNoteMax),
			AddedBy: app.deps.OwnerActorFor(ctx, userID, doctrineOwnerOf(d)), AddedAt: time.Now().UTC(),
			BoostsJson: app.fitBoostsJSON(ctx, doc),
		})
	}
	if err != nil {
		logging.Errorf("doctrines: add fit to %d: %v", d.ID, err)
		back("The fit could not be added; check the server log.")
		return
	}
	message := name + " added to " + d.Name + "."
	if skipped > 0 {
		message += " " + humanize.Plural(skipped, "line") + " of the pasted text could not be read and " + map[bool]string{true: "was", false: "were"}[skipped == 1] + " left out."
	}
	back(message)
}

// fitBoostsJSON is the fleet boosts a doctrine fit gives, as stored
// with it; NULL when the burst data cannot be read, so the first read
// works them out.
func (app *mod) fitBoostsJSON(ctx context.Context, doc *fitview.FitDoc) sql.NullString {
	bursts, err := fit.Bursts(ctx, app.queries)
	if err != nil {
		logging.Errorf("doctrines: burst data: %v", err)
		return sql.NullString{}
	}
	boosts := fitview.FitDocBoosts(doc, bursts)
	if boosts == nil {
		boosts = []fitview.FitBoost{}
	}
	raw, err := json.Marshal(boosts)
	if err != nil {
		return sql.NullString{}
	}
	return sql.NullString{String: string(raw), Valid: true}
}

// doctrineBoosts is one doctrine whose fits give fleet boosts, for
// the fitting tool: its boosts, each burst charge once.
type doctrineBoosts struct {
	ID     int64              `json:"id"`
	Name   string             `json:"name"`
	Corp   string             `json:"corp"`
	Boosts []fitview.FitBoost `json:"boosts"`
}

// handleDoctrineBoosts lists the doctrines of the owners the account
// belongs to whose fits give fleet boosts (GET
// /doctrines/boosts.json), so the fitting tool can fly a fit inside
// one. The boosts are stored with each fit when it is added; a fit
// from before that is worked out once here and stored.
func (app *mod) handleDoctrineBoosts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	out := []doctrineBoosts{}
	write := func() {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]any{"doctrines": out})
	}
	owners := app.deps.DoctrineOwners(ctx, app.deps.UserID(ctx))
	if len(owners) == 0 {
		write()
		return
	}
	params := db.ListDoctrinesForOwnersParams{OwnerKinds: make([]string, len(owners)), OwnerIds: make([]int64, len(owners))}
	for i, owner := range owners {
		params.OwnerKinds[i], params.OwnerIds[i] = owner.Kind, owner.ID
	}
	doctrines, err := app.queries.ListDoctrinesForOwners(ctx, params)
	if err != nil || len(doctrines) == 0 {
		write()
		return
	}
	ids := make([]int64, 0, len(doctrines))
	for _, d := range doctrines {
		ids = append(ids, d.ID)
	}
	fits, err := app.queries.ListDoctrineFitBoosts(ctx, ids)
	if err != nil {
		logging.Errorf("doctrines: fit boosts: %v", err)
		write()
		return
	}
	byDoctrine := map[int64][]fitview.FitBoost{}
	for _, f := range fits {
		stored := f.BoostsJson
		if !stored.Valid {
			var doc fitview.FitDoc
			if json.Unmarshal([]byte(f.ItemsJson), &doc) != nil {
				continue
			}
			if stored = app.fitBoostsJSON(ctx, &doc); stored.Valid {
				if err := app.queries.SetDoctrineFitBoosts(ctx, db.SetDoctrineFitBoostsParams{ID: f.ID, BoostsJson: stored}); err != nil {
					logging.Errorf("doctrines: store boosts of fit %d: %v", f.ID, err)
				}
			}
		}
		var boosts []fitview.FitBoost
		if json.Unmarshal([]byte(stored.String), &boosts) != nil {
			continue
		}
		for _, b := range boosts {
			if !slices.ContainsFunc(byDoctrine[f.DoctrineID], func(o fitview.FitBoost) bool { return o.Charge == b.Charge }) &&
				len(byDoctrine[f.DoctrineID]) < fitview.FitMaxBoosts {
				byDoctrine[f.DoctrineID] = append(byDoctrine[f.DoctrineID], b)
			}
		}
	}
	for _, d := range doctrines {
		if b := byDoctrine[d.ID]; len(b) > 0 {
			out = append(out, doctrineBoosts{ID: d.ID, Name: d.Name, Corp: app.deps.OwnerName(ctx, doctrineOwnerOf(d)), Boosts: b})
		}
	}
	write()
}

func (app *mod) validFleetRole(role string) bool {
	return slices.Contains(app.deps.FleetRoles(), role)
}

func (app *mod) handleDoctrineFitRemove(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	d, ok := app.keptDoctrine(w, r)
	if !ok {
		return
	}
	fitID, _ := strconv.ParseInt(chi.URLParam(r, "fitID"), 10, 64)
	if err := app.queries.DeleteDoctrineFit(ctx, db.DeleteDoctrineFitParams{ID: fitID, DoctrineID: d.ID}); err != nil {
		logging.Errorf("doctrines: remove fit %d from %d: %v", fitID, d.ID, err)
	}
	app.deps.FlashBack(w, r, module.DoctrineURL(d.ID))("Fit removed.")
}

// ---------------------------------------------------------------------------
// Loadouts: every fit in one owner's doctrines, in the fit
// browser (fit_browser.go). Public fits have their own page in the
// fitting tool (char_fit_public.go).
// ---------------------------------------------------------------------------

const doctrineFitsPath = "/doctrines/fits/"

type corpFitsView struct {
	Title        string
	OwnerOptions []module.OwnerOption
	OwnerKey     string // empty when the account belongs to no owner
	OwnerName    string
	Browser      *fitBrowserView
}

// corpFitBrowser sets up the browser for one owner's fits, as an
// address filters it, and loads the fits.
func (app *mod) corpFitBrowser(ctx context.Context, owner standing.Owner, chosen *module.NamedChoice, query url.Values) (*fitBrowser, []fitCard) {
	doctrines, err := app.queries.ListDoctrinesForOwner(ctx, db.ListDoctrinesForOwnerParams{OwnerKind: owner.Kind, OwnerID: owner.ID})
	if err != nil {
		logging.Errorf("loadouts: doctrines of %s: %v", owner.Key(), err)
	}
	byID := map[int64]db.Doctrine{}
	names := map[string]string{}
	ids := make([]int64, 0, len(doctrines))
	for _, d := range doctrines {
		byID[d.ID] = d
		names[idString(d.ID)] = d.Name
		ids = append(ids, d.ID)
	}
	var cards []fitCard
	if len(ids) > 0 {
		fits, err := app.queries.ListDoctrineFits(ctx, ids)
		if err != nil {
			logging.Errorf("loadouts: fits of %s: %v", owner.Key(), err)
		}
		// The module and charge names behind every card's
		// search text resolve in one batched lookup, not one
		// single-type lookup per item per fit.
		docs := make([]fitview.FitDoc, len(fits))
		var typeIDs []int64
		for i, fit := range fits {
			_ = json.Unmarshal([]byte(fit.ItemsJson), &docs[i])
			for _, item := range docs[i].Items {
				typeIDs = append(typeIDs, item.TypeID)
			}
			for chargeID := range docs[i].Charges {
				typeIDs = append(typeIDs, chargeID)
			}
		}
		namesByID := app.deps.CachedTypeNames(ctx, typeIDs)
		for i, fit := range fits {
			d := byID[fit.DoctrineID]
			doc := docs[i]
			tags := doctrineTagList(d.Tags)
			for _, tag := range doc.Tags {
				if !hasTag(tags, tag) {
					tags = append(tags, tag)
				}
			}
			var modNames []string
			for _, item := range doc.Items {
				if name := namesByID[item.TypeID]; name != "" {
					modNames = append(modNames, name)
				}
			}
			for _, chargeID := range doc.LoadedCharges() {
				if name := namesByID[chargeID]; name != "" {
					modNames = append(modNames, name)
				}
			}
			cards = append(cards, fitCard{
				ID: fit.ID, Name: fit.Name, Ship: app.deps.TypeNameOrID(ctx, fit.ShipTypeID), ShipTypeID: fit.ShipTypeID,
				Role: fit.FleetRole, DoctrineID: d.ID, Doctrine: d.Name, DoctrineURL: module.DoctrineURL(d.ID), Category: d.Category,
				OpenURL: "/fittings/?doctrine=" + idString(fit.ID) + "#fit-editor", AddField: "copy", Updated: fit.AddedAt, tags: tags,
				moduleHay: strings.ToLower(strings.Join(modNames, "\n")),
			})
		}
		app.hullClasses(ctx, cards)
	}

	fixed := url.Values{"owner": {owner.Key()}}
	if chosen != nil {
		fixed.Set("for", idString(chosen.ID))
	}
	return &fitBrowser{
		Path: doctrineFitsPath, Fixed: fixed, Q: strings.TrimSpace(query.Get("q")), Sort: query.Get("sort"),
		Include: strings.TrimSpace(query.Get("include")), Exclude: strings.TrimSpace(query.Get("exclude")),
		Sorts: []browseSort{
			{"doctrine", "Doctrine", byFold(func(c *fitCard) string { return c.Doctrine })},
			sortFitName, sortFitShip,
			{"part", "Part", byFold(func(c *fitCard) string { return c.Role })},
		},
		Filters: []fitFilter{
			{Key: "part", Title: "Part", Value: query.Get("part"), Of: func(c *fitCard) []string { return []string{c.Role} }},
			{Key: "doctrine", Title: "Doctrine", Value: query.Get("doctrine"), Most: 16,
				Of: func(c *fitCard) []string { return []string{idString(c.DoctrineID)} }, Label: func(id string) string { return names[id] }},
			{Key: "category", Title: "Category", Value: strings.TrimSpace(query.Get("category")), Most: 12, Of: func(c *fitCard) []string { return []string{c.Category} }},
			filterHull(strings.TrimSpace(query.Get("hull"))),
			filterTag(strings.TrimSpace(query.Get("tag"))),
		},
	}, cards
}

func (app *mod) handleCorpFits(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := &PageData{ShellData: app.deps.PageShell(ctx)}
	userID := app.deps.UserID(ctx)
	view := &corpFitsView{Title: "Corporate Loadouts"}
	data.CorpFits = view
	owner, options, found := app.deps.PickOwner(ctx, r, app.deps.DoctrineOwners(ctx, userID))
	if !found {
		app.deps.Render(ctx, w, http.StatusOK, "corp_fits.html", data)
		return
	}
	if owner.Kind != standing.OwnerCorporation {
		view.Title = "Loadouts"
	}
	view.OwnerOptions, view.OwnerKey, view.OwnerName = options, owner.Key(), app.deps.OwnerName(ctx, owner)
	chosen := app.deps.DoctrineChosenFor(ctx, userID, r.URL.Query().Get("for"))
	browser, cards := app.corpFitBrowser(ctx, owner, chosen, r.URL.Query())
	view.Browser = browser.run(cards)
	view.Browser.For = chosen
	view.Browser.Placeholder = "Search fits, ships and doctrines…"
	view.Browser.SuggestURL = "/doctrines/suggest?scope=fits&" + browser.Fixed.Encode()
	view.Browser.Empty = "No fits yet. A fit shows here once it is in one of " + view.OwnerName + "'s doctrines."
	app.deps.Render(ctx, w, http.StatusOK, "corp_fits.html", data)
}

// permDoctrines is the owner permission that lets an account keep
// doctrines (core's access model names it "doctrines").
const permDoctrines = "doctrines"
