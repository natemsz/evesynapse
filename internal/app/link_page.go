package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// The sign-up and character-link chooser (/auth/link).
//
// Three layers, one computation:
//
//  1. The question: "What do you want to do with EveSynapse?" as activity
//     cards. The registry's own declarations turn the answers into
//     features (link_features.go); there is no table of answers.
//  2. The reveal, before any redirect to EVE: the features that will be
//     turned on, in plain language, with the number of permissions EVE
//     will ask for. Features can be unticked (a feature another one needs
//     stays, and says why); extra permissions are opt-ins beneath.
//  3. Advanced: every feature with its own tick and every enhancement as
//     its own toggle, and the same summary.
//
// The server works everything out; the page only carries choices. The link
// to EVE (/auth/eve) is built from ids, never from scope strings the
// browser sends. An account remembers its last choice, so the next
// character link opens on the reveal, preselected.
// ---------------------------------------------------------------------------

const sessionLinkChoice = "link_choice"

// linkView is the chooser page.
type linkView struct {
	Step       string // "cards", "reveal" or "advanced"
	Activities []linkActivityCard
	Features   []linkFeatureView
	Groups     []linkGroupView // advanced: every feature, grouped
	Popular    []linkEnhancementView
	Count      int // permissions EVE will ask for
	Required   int
	Remembered bool
	Character  int64
	Continue   string // the link to EVE
	Full       string // the same with every extra permission ticked
	Switch     string // the other layer
	Error      string
	Hidden     []linkHidden // choices to carry across a form
}

type linkActivityCard struct {
	linkActivity
	Selected bool
}

type linkHidden struct{ Name, Value string }

type linkFeatureView struct {
	Accent   int // the Trade Ideas accent of the kind of play it serves
	ID       string
	Title    string
	Ticked   bool
	Locked   bool   // another ticked feature requires it
	Why      string // "Needed by Doctrines"
	Needs    string // "Needs Fitting"
	Required []linkScopeView
	Enhance  []linkEnhancementView
}

type linkScopeView struct{ Scope, Unlocks string }

type linkEnhancementView struct {
	Scope   string
	Unlocks string
	Feature string
	Ticked  bool
}

type linkGroupView struct {
	Title    string
	Accent   int
	Features []linkFeatureView
}

// handleLinkChoose renders the chooser.
func (app *Application) handleLinkChoose(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.publicPage(ctx)
	q := r.URL.Query()
	view := &linkView{}
	data.Link = view

	if cid, err := strconv.ParseInt(q.Get("character"), 10, 64); err == nil && cid > 0 && app.ownsCharacter(r, cid) {
		view.Character = cid
	}
	submitted := q.Get("submitted") == "1"
	choice := linkChoice{Activities: q["activity"], Features: q["feature"], Enhance: q["enhance"]}
	if submitted {
		choice.Activities = nil // the ticked features are the answer now
	}
	if choice.empty() && !submitted {
		if remembered, ok := app.rememberedLink(ctx); ok {
			choice = remembered
			view.Remembered = true
		}
	}

	switch {
	case q.Get("advanced") == "1":
		view.Step = "advanced"
	case choice.empty() && !submitted:
		view.Step = "cards"
	default:
		view.Step = "reveal"
	}

	if view.Step == "cards" {
		for _, a := range linkActivities {
			view.Activities = append(view.Activities, linkActivityCard{linkActivity: a, Selected: contains(choice.Activities, a.ID)})
		}
		view.Switch = linkChooseURL(url.Values{"advanced": {"1"}}, view.Character)
		app.render(ctx, w, http.StatusOK, "link.html", data)
		return
	}

	res, err := resolveLinkLenient(choice)
	if err != nil {
		view.Error = "That selection could not be read: " + err.Error() + "."
		view.Step = "cards"
		for _, a := range linkActivities {
			view.Activities = append(view.Activities, linkActivityCard{linkActivity: a})
		}
		app.render(ctx, w, http.StatusBadRequest, "link.html", data)
		return
	}
	fillLinkView(view, res)
	if view.Step == "advanced" {
		fillAdvanced(view, res)
	}
	app.render(ctx, w, http.StatusOK, "link.html", data)
}

// fillLinkView puts a resolved choice on the page: the features that will
// be turned on, the permission count, the opt-ins and the links.
func fillLinkView(view *linkView, res linkResolved) {
	view.Required = len(res.Required)
	view.Count = len(res.Scopes)
	for _, row := range res.Features {
		fv := linkFeatureView{ID: row.ID, Title: row.Title, Ticked: true, Accent: linkAccent(row.Activities)}
		if len(row.NeededBy) > 0 && !row.Pinned {
			fv.Locked = true
			fv.Why = "Needed by " + titleList(row.NeededBy)
		} else if len(row.NeededBy) > 0 {
			fv.Why = "Also needed by " + titleList(row.NeededBy)
		}
		if len(row.Requires) > 0 {
			var needs []string
			for _, id := range row.Requires {
				for _, other := range res.Features {
					if other.ID == id {
						needs = append(needs, other.Title)
					}
				}
			}
			fv.Needs = titleList(needs)
		}
		for _, s := range row.Required {
			fv.Required = append(fv.Required, linkScopeView{Scope: s.Scope, Unlocks: s.Unlocks})
		}
		view.Features = append(view.Features, fv)
	}
	for _, e := range res.Enhancements {
		view.Popular = append(view.Popular, linkEnhancementView{Scope: e.Scope, Unlocks: e.Unlocks, Feature: e.Feature, Ticked: e.Chosen})
	}
	var pinned []string
	for _, row := range res.Features {
		if row.Pinned {
			pinned = append(pinned, row.ID)
		}
	}
	var chosen []string
	for _, e := range res.Enhancements {
		if e.Chosen {
			chosen = append(chosen, e.Scope)
		}
	}
	view.Continue = linkEVEURL(pinned, chosen, view.Character)
	all := append([]string(nil), chosen...)
	for _, e := range res.Enhancements {
		if !e.Chosen {
			all = append(all, e.Scope)
		}
	}
	view.Full = linkChooseURL(url.Values{"feature": pinned, "enhance": all, "submitted": {"1"}}, view.Character)
	view.Switch = linkChooseURL(url.Values{"advanced": {"1"}, "feature": pinned, "enhance": chosen, "submitted": {"1"}}, view.Character)
}

// fillAdvanced lists every feature, grouped by the kind of play it serves,
// with what is ticked now marked.
func fillAdvanced(view *linkView, res linkResolved) {
	in := map[string]linkFeatureRow{}
	for _, row := range res.Features {
		in[row.ID] = row
	}
	chosen := map[string]bool{}
	for _, e := range res.Enhancements {
		if e.Chosen {
			chosen[e.Scope] = true
		}
	}
	titles := map[string]string{}
	all := linkFeatures()
	for _, f := range all {
		titles[f.ID] = f.Title
	}
	groups := map[string]*linkGroupView{}
	var order []string
	for _, f := range all {
		g := "other"
		if len(f.Activities) > 0 {
			g = f.Activities[0]
		}
		if groups[g] == nil {
			gv := &linkGroupView{Title: "Other"}
			if a, ok := linkActivityByID(g); ok {
				gv.Title, gv.Accent = a.Title, a.Accent
			} else {
				gv.Accent = 6
			}
			groups[g] = gv
			order = append(order, g)
		}
		fv := linkFeatureView{ID: f.ID, Title: f.Title}
		if row, ok := in[f.ID]; ok {
			fv.Ticked = true
			if len(row.NeededBy) > 0 && !row.Pinned {
				fv.Locked = true
				fv.Why = "Needed by " + titleList(row.NeededBy)
			}
		}
		var needs []string
		for _, id := range f.Requires {
			if t, ok := titles[id]; ok {
				needs = append(needs, t)
			}
		}
		fv.Needs = titleList(needs)
		for _, s := range f.Required {
			fv.Required = append(fv.Required, linkScopeView{Scope: s.Scope, Unlocks: s.Unlocks})
		}
		for _, s := range f.Enhance {
			fv.Enhance = append(fv.Enhance, linkEnhancementView{Scope: s.Scope, Unlocks: s.Unlocks, Feature: f.Title, Ticked: chosen[s.Scope]})
		}
		groups[g].Features = append(groups[g].Features, fv)
	}
	// The five kinds of play in card order, then the rest.
	for _, a := range linkActivities {
		if gv, ok := groups[a.ID]; ok {
			view.Groups = append(view.Groups, *gv)
		}
	}
	if gv, ok := groups["other"]; ok {
		view.Groups = append(view.Groups, *gv)
	}
	_ = order
}

// linkEVEURL is the link to EVE for a choice: ids only.
func linkEVEURL(features, enhance []string, character int64) string {
	v := url.Values{"feature": features}
	if len(enhance) > 0 {
		v["enhance"] = enhance
	}
	if character > 0 {
		v.Set("character", strconv.FormatInt(character, 10))
	}
	return "/auth/eve?" + v.Encode()
}

func linkChooseURL(v url.Values, character int64) string {
	if character > 0 {
		v.Set("character", strconv.FormatInt(character, 10))
	}
	return "/auth/link?" + v.Encode()
}

// ownsCharacter reports whether a character is linked to the session's
// account (so one cannot probe other accounts' characters).
func (app *Application) ownsCharacter(r *http.Request, characterID int64) bool {
	for _, ch := range app.sessionCharacters(r.Context()) {
		if ch.CharacterID == characterID {
			return true
		}
	}
	return false
}

// rememberedLink is the choice the signed-in account made last time.
func (app *Application) rememberedLink(ctx context.Context) (linkChoice, bool) {
	userID := app.userID(ctx)
	if userID == 0 {
		return linkChoice{}, false
	}
	raw, err := app.queries.GetLinkChoice(ctx, userID)
	if err != nil || strings.TrimSpace(raw) == "" {
		return linkChoice{}, false
	}
	var c linkChoice
	if json.Unmarshal([]byte(raw), &c) != nil || c.empty() {
		return linkChoice{}, false
	}
	// A feature or permission a later build dropped must not break the
	// page: keep what still resolves.
	if _, err := resolveLink(c); err != nil {
		c = linkChoice{Activities: c.Activities}
		if _, err := resolveLink(c); err != nil {
			return linkChoice{}, false
		}
	}
	return c, true
}

// rememberLink stores the account's choice.
func (app *Application) rememberLink(ctx context.Context, userID int64, c linkChoice) {
	if userID == 0 || c.empty() {
		return
	}
	blob, err := json.Marshal(c)
	if err != nil {
		return
	}
	if err := app.queries.SetLinkChoice(ctx, db.SetLinkChoiceParams{UserID: userID, Choice: string(blob), UpdatedAt: time.Now().UTC()}); err != nil {
		logging.Errorf("link: remember choice of user %d: %v", userID, err)
	}
}

// linkChoiceFromQuery reads a choice from /auth/eve: repeated feature,
// enhance and activity parameters, or full=1 for every feature and every
// extra permission (the link as it was before the chooser).
func linkChoiceFromQuery(q url.Values) (linkChoice, bool) {
	if q.Get("full") == "1" {
		return fullLinkChoice(), true
	}
	c := linkChoice{Activities: q["activity"], Features: q["feature"], Enhance: q["enhance"]}
	return c, !c.empty()
}

// resolveLinkLenient is resolveLink for the chooser page: an extra
// permission that belongs to no chosen feature (its feature was just
// unticked) is dropped instead of refused, so the list can be updated.
// The link to EVE (resolveLink) stays strict.
func resolveLinkLenient(c linkChoice) (linkResolved, error) {
	base, err := resolveLink(linkChoice{Activities: c.Activities, Features: c.Features})
	if err != nil {
		return linkResolved{}, err
	}
	known := map[string]bool{}
	for _, e := range base.Enhancements {
		known[e.Scope] = true
	}
	var keep []string
	for _, s := range c.Enhance {
		if known[s] {
			keep = append(keep, s)
		}
	}
	c.Enhance = keep
	return resolveLink(c)
}

// linkAccent is the accent of the first kind of play a feature serves.
func linkAccent(activities []string) int {
	for _, id := range activities {
		if a, ok := linkActivityByID(id); ok {
			return a.Accent
		}
	}
	return 6
}
