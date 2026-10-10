package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Doctrines.
//
// A doctrine is a corporation's named set of fits for a kind of fleet.
// Every member with a character in the corporation can read its
// doctrines; its directors and CEO keep them, with whoever they hand
// that to (corp_permissions.go).
//
// A fit in a doctrine is a copy of a fit document from the fitting
// tool: one of the keeper's saved fits, a public fit, a fit from
// another doctrine, or pasted EFT text. Being a copy, it opens in the
// fitting tool for anyone in the corporation, whether or not the fit
// it came from is public or still exists.
//
// An op can name a doctrine (ops.go), and ship replacement shows
// whether a lost ship was one of the doctrine's (corp_srp.go).
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

func doctrineURL(id int64) string { return doctrinesPath + strconv.FormatInt(id, 10) }

// doctrineTags reads a tags field into the stored form: the tags as
// typed, comma-separated, without repeats.
func doctrineTags(raw string) string {
	tags := parseFitTags(raw)
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

// doctrineFitView is one fit of a doctrine on a page.
type doctrineFitView struct {
	ID         int64
	Name       string
	Ship       string
	ShipTypeID int64
	Role       string
	Note       string
	EFT        string // the fit as text the game imports; the doctrine's own page only
}

// doctrineCard is one doctrine in a list.
type doctrineCard struct {
	ID          int64
	Name        string
	Category    string
	Description string
	Tags        []string
	Fits        []doctrineFitView
}

// doctrineCorpView is the corporation a doctrines page is about.
type doctrineCorpView struct {
	ID        int64
	Name      string
	Manages   bool
	Directs   bool
	Total     int // doctrines before the page's filters
	Doctrines []doctrineCard
}

// doctrinesView is the /doctrines/ page: one corporation's doctrines.
type doctrinesView struct {
	CorpOptions []corpOption
	Corp        *doctrineCorpView // nil when the account is in no player corporation
	Q           string
	Category    string
	Tag         string
	Filtered    bool
	Categories  []string
	Tags        []string
}

// doctrineFitViews turns stored fits into rows, by doctrine.
func (app *Application) doctrineFitViews(ctx context.Context, doctrineIDs []int64) map[int64][]doctrineFitView {
	out := map[int64][]doctrineFitView{}
	if len(doctrineIDs) == 0 {
		return out
	}
	fits, err := app.queries.ListDoctrineFits(ctx, doctrineIDs)
	if err != nil {
		logging.Errorf("doctrines: fits: %v", err)
		return out
	}
	for _, fit := range fits {
		out[fit.DoctrineID] = append(out[fit.DoctrineID], doctrineFitView{
			ID: fit.ID, Name: fit.Name, Ship: app.typeNameOrID(ctx, fit.ShipTypeID), ShipTypeID: fit.ShipTypeID,
			Role: fit.FleetRole, Note: fit.Note,
		})
	}
	return out
}

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

func (app *Application) handleDoctrines(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.page(ctx)
	userID := app.userID(ctx)
	query := r.URL.Query()
	view := &doctrinesView{
		Q: strings.TrimSpace(query.Get("q")), Category: strings.TrimSpace(query.Get("category")), Tag: strings.TrimSpace(query.Get("tag")),
	}
	view.Filtered = view.Q != "" || view.Category != "" || view.Tag != ""
	data.Doctrines = view

	corp, options := app.pickCorporation(ctx, r, app.playerCorps(ctx, userID))
	if corp == 0 {
		app.render(ctx, w, http.StatusOK, "doctrines.html", data)
		return
	}
	st, err := app.discordStandingFor(ctx, userID)
	if err != nil {
		logging.Errorf("doctrines: standing of user %d: %v", userID, err)
		data.Error = "Could not load the doctrines; check the server log."
	}
	cv := &doctrineCorpView{ID: corp, Name: app.corpDisplayName(ctx, corp), Directs: st.directs(corp)}
	cv.Manages = app.corpPermits(ctx, st, corp, permDoctrines)
	view.CorpOptions, view.Corp = options, cv

	doctrines, err := app.queries.ListDoctrinesForCorporations(ctx, []int64{corp})
	if err != nil {
		logging.Errorf("doctrines: list for corporation %d: %v", corp, err)
	}
	ids := make([]int64, 0, len(doctrines))
	for _, d := range doctrines {
		ids = append(ids, d.ID)
	}
	fits := app.doctrineFitViews(ctx, ids)
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
	app.render(ctx, w, http.StatusOK, "doctrines.html", data)
}

// handleDoctrineSuggest feeds the search boxes of the doctrine pages
// (GET /doctrines/suggest?scope=&corporation=&q=). What it offers
// depends on the box: doctrines and what they are filed under, a
// corporation's fits, its categories, or the account's own saved fits.
// A corporation's names go only to an account with a character in it.
func (app *Application) handleDoctrineSuggest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	query := r.URL.Query()
	q := strings.TrimSpace(query.Get("q"))
	needle := strings.ToLower(q)
	var out []suggestItem
	if len(q) < 2 {
		writeSuggestJSON(w, out)
		return
	}
	if query.Get("scope") == "myfits" {
		rows, err := app.queries.SearchLocalFittings(ctx, db.SearchLocalFittingsParams{UserID: userID, Q: q})
		if err != nil {
			logging.Errorf("doctrines: suggest saved fits: %v", err)
		}
		for _, row := range rows {
			if !row.IsDraft {
				out = append(out, suggestItem{ID: row.ID, Name: row.Name, Label: row.ShipName})
			}
		}
		writeSuggestJSON(w, out)
		return
	}

	corp, _ := strconv.ParseInt(query.Get("corporation"), 10, 64)
	inside := false
	for _, mine := range app.playerCorps(ctx, userID) {
		inside = inside || mine == corp
	}
	if !inside {
		writeSuggestJSON(w, out)
		return
	}
	if query.Get("scope") == "fits" {
		browser, cards := app.corpFitBrowser(ctx, corp, app.doctrineChosenFor(ctx, userID, query.Get("for")), url.Values{})
		writeSuggestJSON(w, browser.suggestFits(cards, q))
		return
	}

	doctrines, err := app.queries.ListDoctrinesForCorporations(ctx, []int64{corp})
	if err != nil {
		logging.Errorf("doctrines: suggest for corporation %d: %v", corp, err)
	}
	seen := map[string]bool{}
	add := func(name, label, address string) {
		key := label + "\n" + strings.ToLower(name)
		if name != "" && !seen[key] && len(out) < suggestBrowseMost && strings.Contains(strings.ToLower(name), needle) {
			seen[key] = true
			out = append(out, suggestItem{Name: name, Label: label, URL: address})
		}
	}
	list := corpAddress(doctrinesPath, corp)
	if query.Get("scope") == "categories" {
		for _, d := range doctrines {
			add(d.Category, "Category", "")
		}
		writeSuggestJSON(w, out)
		return
	}
	ids := make([]int64, 0, len(doctrines))
	for _, d := range doctrines {
		ids = append(ids, d.ID)
		label := "Doctrine"
		if d.Category != "" {
			label = "Doctrine · " + d.Category
		}
		add(d.Name, label, doctrineURL(d.ID))
	}
	fits := app.doctrineFitViews(ctx, ids)
	for _, d := range doctrines {
		for _, fit := range fits[d.ID] {
			add(fit.Name, "Fit in "+d.Name, doctrineURL(d.ID))
			add(fit.Ship, "Ship", list+"&q="+url.QueryEscape(fit.Ship))
		}
	}
	for _, d := range doctrines {
		add(d.Category, "Category", list+"&category="+url.QueryEscape(d.Category))
		for _, tag := range doctrineTagList(d.Tags) {
			add(tag, "Tag", list+"&tag="+url.QueryEscape(tag))
		}
	}
	writeSuggestJSON(w, out)
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

// handleDoctrineCreate makes a doctrine for a corporation
// (POST /doctrines/create).
func (app *Application) handleDoctrineCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	_ = r.ParseForm()
	corp, _ := strconv.ParseInt(r.Form.Get("corporation"), 10, 64)
	back := app.flashBack(w, r, corpAddress(doctrinesPath, corp))
	st, err := app.discordStandingFor(ctx, userID)
	if err != nil || corp == 0 || !app.corpPermits(ctx, st, corp, permDoctrines) {
		back("That corporation's doctrines are not yours to keep.")
		return
	}
	name, category, description, tags, problem := doctrineFields(r)
	if problem != "" {
		back(problem)
		return
	}
	id, err := app.queries.CreateDoctrine(ctx, db.CreateDoctrineParams{
		CorporationID: corp, Name: name, Category: category, Description: description, Tags: tags,
		CreatedBy: app.srpActorFor(ctx, userID, corp).CharacterID, CreatedAt: time.Now().UTC(),
	})
	if errors.Is(err, sql.ErrNoRows) {
		back("There is already a doctrine called " + name + ".")
		return
	}
	if err != nil {
		logging.Errorf("doctrines: create for corporation %d: %v", corp, err)
		back("The doctrine could not be saved; check the server log.")
		return
	}
	logging.Infof("doctrines: user %d made doctrine %d (%s) for corporation %d", userID, id, name, corp)
	app.flash(ctx, "Doctrine made. Add its fits below.")
	http.Redirect(w, r, doctrineURL(id), http.StatusSeeOther)
}

// doctrineAccess loads the doctrine named in the address and says
// what the account may do with it: read it (a character in the
// corporation) and keep it.
func (app *Application) doctrineAccess(r *http.Request) (d db.Doctrine, reads, keeps bool) {
	ctx := r.Context()
	id, _ := strconv.ParseInt(chi.URLParam(r, "doctrineID"), 10, 64)
	d, err := app.queries.GetDoctrine(ctx, id)
	if err != nil {
		return d, false, false
	}
	st, err := app.discordStandingFor(ctx, app.userID(ctx))
	if err != nil {
		return d, false, false
	}
	return d, st.corps[d.CorporationID], app.corpPermits(ctx, st, d.CorporationID, permDoctrines)
}

// doctrineView is one doctrine's own page.
type doctrineView struct {
	doctrineCard
	TagsText    string
	Corporation string
	CorpID      int64
	Keeps       bool
	Full        bool // no room for another fit
	Roles       []string
	MyFits      []opChoice // the keeper's saved fits, to add
}

func (app *Application) handleDoctrine(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	d, reads, keeps := app.doctrineAccess(r)
	if !reads {
		app.flash(ctx, "That doctrine is not one of your corporation's.")
		http.Redirect(w, r, doctrinesPath, http.StatusSeeOther)
		return
	}
	data := app.page(ctx)
	view := &doctrineView{
		doctrineCard: doctrineCard{ID: d.ID, Name: d.Name, Category: d.Category, Description: d.Description, Tags: doctrineTagList(d.Tags)},
		TagsText:     strings.Join(doctrineTagList(d.Tags), ", "),
		Corporation:  app.corpDisplayName(ctx, d.CorporationID), CorpID: d.CorporationID, Keeps: keeps, Roles: opFleetRoles,
	}
	data.Doctrine = view
	fits, err := app.queries.ListDoctrineFits(ctx, []int64{d.ID})
	if err != nil {
		logging.Errorf("doctrines: fits of %d: %v", d.ID, err)
	}
	for _, fit := range fits {
		fv := doctrineFitView{
			ID: fit.ID, Name: fit.Name, Ship: app.typeNameOrID(ctx, fit.ShipTypeID), ShipTypeID: fit.ShipTypeID,
			Role: fit.FleetRole, Note: fit.Note,
		}
		var doc fitDoc
		if json.Unmarshal([]byte(fit.ItemsJson), &doc) == nil {
			doc.Name = fit.Name
			fv.EFT = app.formatEFT(ctx, &doc)
		}
		view.Fits = append(view.Fits, fv)
	}
	view.Full = len(view.Fits) >= doctrineFitsMost
	if keeps {
		mine, _ := app.queries.ListLocalFittings(ctx, app.userID(ctx))
		for _, fit := range mine {
			if !fit.IsDraft {
				view.MyFits = append(view.MyFits, opChoice{ID: fit.ID, Name: fit.Name + " (" + app.typeNameOrID(ctx, fit.ShipTypeID) + ")"})
			}
		}
	}
	app.render(ctx, w, http.StatusOK, "doctrine.html", data)
}

// keptDoctrine loads the doctrine named in the address for an account
// that keeps it; anyone else is sent back with a word.
func (app *Application) keptDoctrine(w http.ResponseWriter, r *http.Request) (db.Doctrine, bool) {
	d, _, keeps := app.doctrineAccess(r)
	if !keeps {
		app.flashBack(w, r, doctrinesPath)("That doctrine is not yours to change.")
	}
	return d, keeps
}

func (app *Application) handleDoctrineSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_ = r.ParseForm()
	d, ok := app.keptDoctrine(w, r)
	if !ok {
		return
	}
	back := app.flashBack(w, r, doctrineURL(d.ID))
	name, category, description, tags, problem := doctrineFields(r)
	if problem != "" {
		back(problem)
		return
	}
	n, err := app.queries.UpdateDoctrine(ctx, db.UpdateDoctrineParams{
		ID: d.ID, Name: name, Category: category, Description: description, Tags: tags, UpdatedAt: time.Now().UTC(),
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

func (app *Application) handleDoctrineDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	d, ok := app.keptDoctrine(w, r)
	if !ok {
		return
	}
	if err := app.queries.DeleteDoctrine(ctx, d.ID); err != nil {
		logging.Errorf("doctrines: delete %d: %v", d.ID, err)
	}
	logging.Infof("doctrines: user %d deleted doctrine %d (%s)", app.userID(ctx), d.ID, d.Name)
	app.flashBack(w, r, corpAddress(doctrinesPath, d.CorporationID))("Deleted " + d.Name + ". Ops that named it keep its name.")
}

// doctrineFitSource finds the fit document a keeper is adding: one of
// their own saved fits, a public fit, a fit of a doctrine they can
// read, or pasted EFT text (skipped counts its lines that were not
// understood).
func (app *Application) doctrineFitSource(r *http.Request) (doc *fitDoc, problem string, skipped int) {
	ctx := r.Context()
	userID := app.userID(ctx)
	read := func(raw string) (*fitDoc, string, int) {
		var doc fitDoc
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
	// Typed, not picked from the suggestions: the saved fit of that name.
	if name := strings.TrimSpace(r.Form.Get("fit_name")); name != "" {
		rows, _ := app.queries.SearchLocalFittings(ctx, db.SearchLocalFittingsParams{UserID: userID, Q: name})
		for _, row := range rows {
			if strings.EqualFold(row.Name, name) && !row.IsDraft {
				return read(row.ItemsJson)
			}
		}
		return nil, "None of your saved fits is called " + name + ".", 0
	}
	if id, _ := strconv.ParseInt(r.Form.Get("public"), 10, 64); id > 0 {
		row, err := app.queries.GetPublicFitting(ctx, id)
		if err != nil {
			return nil, "That public fit is gone.", 0
		}
		return read(row.ItemsJson)
	}
	if id, _ := strconv.ParseInt(r.Form.Get("copy"), 10, 64); id > 0 {
		if doc, ok := app.doctrineFitDoc(ctx, userID, id); ok {
			return doc, "", 0
		}
		return nil, "That fit is not in one of your corporation's doctrines.", 0
	}
	if text := strings.TrimSpace(r.Form.Get("eft")); text != "" {
		if len(text) > doctrineEFTMax {
			return nil, "That is too long to be one fit.", 0
		}
		doc, notes := app.parseEFT(ctx, text)
		return doc, "", len(notes)
	}
	return nil, "Pick a fit, or paste one.", 0
}

// doctrineFitDoc reads a doctrine's fit for an account with a
// character in the doctrine's corporation.
func (app *Application) doctrineFitDoc(ctx context.Context, userID, fitID int64) (*fitDoc, bool) {
	row, err := app.queries.GetDoctrineFit(ctx, fitID)
	if err != nil {
		return nil, false
	}
	inside := false
	for _, corp := range app.srpCorps(ctx, userID) {
		inside = inside || corp == row.CorporationID
	}
	var doc fitDoc
	if !inside || json.Unmarshal([]byte(row.ItemsJson), &doc) != nil {
		return nil, false
	}
	doc.Name = row.Name
	return &doc, true
}

// handleDoctrineFitAdd copies a fit into a doctrine
// (POST /doctrines/{doctrineID}/fits/add).
func (app *Application) handleDoctrineFitAdd(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	_ = r.ParseForm()
	d, ok := app.keptDoctrine(w, r)
	if !ok {
		return
	}
	path := doctrineURL(d.ID)
	if next := r.Form.Get("next"); strings.HasPrefix(next, doctrineFitsPath+"?") || strings.HasPrefix(next, publicFitsPath+"?") {
		path = next
	}
	back := app.flashBack(w, r, path)
	if n, err := app.queries.CountDoctrineFits(ctx, d.ID); err != nil || n >= doctrineFitsMost {
		back(fmt.Sprintf("A doctrine holds up to %d fits.", doctrineFitsMost))
		return
	}
	doc, problem, skipped := app.doctrineFitSource(r)
	if problem != "" {
		back(problem)
		return
	}
	sanitizeFitDoc(doc)
	if doc.ShipTypeID <= 0 {
		back("That fit has no ship EveSynapse knows. Pasted text has to start with a line like [Guardian, Fleet logi].")
		return
	}
	role := r.Form.Get("fleet_role")
	if !validFleetRole(role) {
		role = ""
	}
	name := clip(strings.Join(strings.Fields(doc.Name), " "), doctrineNameMax)
	if name == "" {
		name = app.typeNameOrID(ctx, doc.ShipTypeID)
	}
	doc.Name = name
	raw, err := json.Marshal(doc)
	if err == nil {
		_, err = app.queries.AddDoctrineFit(ctx, db.AddDoctrineFitParams{
			DoctrineID: d.ID, Name: name, ShipTypeID: doc.ShipTypeID, ItemsJson: string(raw), FleetRole: role,
			Note:    clip(strings.TrimSpace(r.Form.Get("note")), doctrineNoteMax),
			AddedBy: app.srpActorFor(ctx, userID, d.CorporationID).CharacterID, AddedAt: time.Now().UTC(),
		})
	}
	if err != nil {
		logging.Errorf("doctrines: add fit to %d: %v", d.ID, err)
		back("The fit could not be added; check the server log.")
		return
	}
	message := name + " added to " + d.Name + "."
	if skipped > 0 {
		message += " " + plural(skipped, "line") + " of the pasted text could not be read and " + map[bool]string{true: "was", false: "were"}[skipped == 1] + " left out."
	}
	back(message)
}

func validFleetRole(role string) bool {
	for _, known := range opFleetRoles {
		if known == role {
			return true
		}
	}
	return false
}

func (app *Application) handleDoctrineFitRemove(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	d, ok := app.keptDoctrine(w, r)
	if !ok {
		return
	}
	fitID, _ := strconv.ParseInt(chi.URLParam(r, "fitID"), 10, 64)
	if err := app.queries.DeleteDoctrineFit(ctx, db.DeleteDoctrineFitParams{ID: fitID, DoctrineID: d.ID}); err != nil {
		logging.Errorf("doctrines: remove fit %d from %d: %v", fitID, d.ID, err)
	}
	app.flashBack(w, r, doctrineURL(d.ID))("Fit removed.")
}

// ---------------------------------------------------------------------------
// Corporate fits: every fit in one corporation's doctrines, in the fit
// browser (fit_browser.go). Public fits have their own page in the
// fitting tool (char_fit_public.go).
// ---------------------------------------------------------------------------

const doctrineFitsPath = "/doctrines/fits/"

type corpFitsView struct {
	CorpOptions []corpOption
	CorpID      int64 // 0 when the account is in no player corporation
	CorpName    string
	Directs     bool
	Browser     *fitBrowserView
}

// doctrineChosenFor reads the "for" of an address: the doctrine a
// keeper is choosing fits for. Nil unless the account keeps it.
func (app *Application) doctrineChosenFor(ctx context.Context, userID int64, raw string) *opChoice {
	id, _ := strconv.ParseInt(raw, 10, 64)
	if id <= 0 {
		return nil
	}
	d, err := app.queries.GetDoctrine(ctx, id)
	if err != nil {
		return nil
	}
	st, err := app.discordStandingFor(ctx, userID)
	if err != nil || !app.corpPermits(ctx, st, d.CorporationID, permDoctrines) {
		return nil
	}
	return &opChoice{ID: d.ID, Name: d.Name}
}

// corpFitBrowser sets up the browser for one corporation's fits, as
// an address filters it, and loads the fits.
func (app *Application) corpFitBrowser(ctx context.Context, corp int64, chosen *opChoice, query url.Values) (*fitBrowser, []fitCard) {
	doctrines, err := app.queries.ListDoctrinesForCorporations(ctx, []int64{corp})
	if err != nil {
		logging.Errorf("corporate fits: doctrines of corporation %d: %v", corp, err)
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
			logging.Errorf("corporate fits: fits of corporation %d: %v", corp, err)
		}
		for _, fit := range fits {
			d := byID[fit.DoctrineID]
			var doc fitDoc
			_ = json.Unmarshal([]byte(fit.ItemsJson), &doc)
			tags := doctrineTagList(d.Tags)
			for _, tag := range doc.Tags {
				if !hasTag(tags, tag) {
					tags = append(tags, tag)
				}
			}
			cards = append(cards, fitCard{
				ID: fit.ID, Name: fit.Name, Ship: app.typeNameOrID(ctx, fit.ShipTypeID), ShipTypeID: fit.ShipTypeID,
				Role: fit.FleetRole, DoctrineID: d.ID, Doctrine: d.Name, DoctrineURL: doctrineURL(d.ID), Category: d.Category,
				OpenURL: "/fittings/?doctrine=" + idString(fit.ID) + "#fit-editor", AddField: "copy", Updated: fit.AddedAt, tags: tags,
			})
		}
		app.hullClasses(ctx, cards)
	}

	fixed := url.Values{"corporation": {idString(corp)}}
	if chosen != nil {
		fixed.Set("for", idString(chosen.ID))
	}
	return &fitBrowser{
		Path: doctrineFitsPath, Fixed: fixed, Q: strings.TrimSpace(query.Get("q")), Sort: query.Get("sort"),
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

func (app *Application) handleCorpFits(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.page(ctx)
	userID := app.userID(ctx)
	view := &corpFitsView{}
	data.CorpFits = view
	corp, options := app.pickCorporation(ctx, r, app.playerCorps(ctx, userID))
	if corp == 0 {
		app.render(ctx, w, http.StatusOK, "corp_fits.html", data)
		return
	}
	view.CorpOptions, view.CorpID, view.CorpName = options, corp, app.corpDisplayName(ctx, corp)
	if st, err := app.discordStandingFor(ctx, userID); err == nil {
		view.Directs = st.directs(corp)
	}
	chosen := app.doctrineChosenFor(ctx, userID, r.URL.Query().Get("for"))
	browser, cards := app.corpFitBrowser(ctx, corp, chosen, r.URL.Query())
	view.Browser = browser.run(cards)
	view.Browser.For = chosen
	view.Browser.Placeholder = "Search fits, ships and doctrines…"
	view.Browser.SuggestURL = "/doctrines/suggest?scope=fits&" + browser.Fixed.Encode()
	view.Browser.Empty = "No corporate fits yet. A fit shows here once it is in one of the corporation's doctrines."
	app.render(ctx, w, http.StatusOK, "corp_fits.html", data)
}
