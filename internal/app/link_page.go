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
// Two steps, one computation:
//
//  1. The question: "What do you want to do with EveSynapse?" as activity
//     cards. The registry's own declarations turn the answers into
//     features (link_features.go); there is no table of answers.
//  2. The reveal, before any redirect to EVE: the features that will be
//     turned on, in plain language, with the number of permissions EVE
//     will ask for. Features can be unticked (a feature another one needs
//     stays, and says why); every permission of the chosen features
//     starts ticked, the optional ones included, listed like the rest.
//
// The server works everything out; the page only carries choices. The link
// to EVE (/auth/eve) is built from ids, never from scope strings the
// browser sends. An account remembers its last choice, so the next
// character link opens on the reveal, preselected.
// ---------------------------------------------------------------------------

const sessionLinkChoice = "link_choice"

// linkView is the chooser page.
type linkView struct {
	Step       string // "cards" or "reveal"
	Activities []linkActivityCard
	Features   []linkFeatureView
	Groups     []linkGroupView // the chosen features by kind of play, with what each permission is for
	Popular    []linkEnhancementView
	Roles      []linkRoleView        // corporation permissions, framed by the in-game role they need
	Server     []linkEnhancementView // what this server offers on top
	Count      int                   // permissions EVE will ask for
	Required   int
	Remembered bool
	Character  int64
	Continue   string // the link to EVE
	Full       string // the same with every extra permission ticked
	Back       string // the question again
	AllPicked  bool   // every card picked (pick=all)
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
	Role     []linkRoleView // corporation permissions of the feature, as opt-ins
	Enhance  []linkEnhancementView
}

type linkScopeView struct{ Scope, Unlocks string }

type linkRoleView struct {
	Scope   string
	Unlocks string
	Feature string
	Roles   string
	Ticked  bool
}

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
	forceCards := q.Get("cards") == "1"
	if choice.empty() && !submitted && !forceCards && q.Get("from") != "cards" {
		if remembered, ok := app.rememberedLink(ctx); ok {
			choice = remembered
			view.Remembered = true
		}
	}

	// Two steps only: the cards, then the summary. A legacy
	// advanced=1 link lands wherever its choice lands (the cards
	// when it carries none, the summary when it does).
	if forceCards || (choice.empty() && !submitted) {
		view.Step = "cards"
	} else {
		view.Step = "reveal"
	}

	if view.Step == "cards" {
		view.AllPicked = q.Get("pick") == "all"
		// A fresh visit (no cards submission in the query) opens with
		// the Essentials set ticked. A cards submission (from=cards)
		// says what is ticked, even when that is nothing.
		picked := choice.Activities
		if q.Get("from") != "cards" && len(picked) == 0 && !view.AllPicked {
			picked = linkEssentials
		}
		for _, a := range linkActivities {
			view.Activities = append(view.Activities, linkActivityCard{linkActivity: a, Selected: view.AllPicked || contains(picked, a.ID)})
		}
		app.render(ctx, w, http.StatusOK, "link.html", data)
		return
	}

	res, err := app.resolveLinkLenient(choice)
	if err != nil {
		view.Error = "That selection could not be read: " + err.Error() + "."
		view.Step = "cards"
		for _, a := range linkActivities {
			view.Activities = append(view.Activities, linkActivityCard{linkActivity: a})
		}
		app.render(ctx, w, http.StatusBadRequest, "link.html", data)
		return
	}
	if view.Step == "reveal" && !submitted && !view.Remembered && len(choice.Activities) > 0 {
		// Arriving from the playstyle cards: every permission of the
		// chosen features starts ticked, the ones that used to be
		// offered as extras included. They are part of how the
		// features are expected to work, so the summary lists them
		// like any other permission; unticking stays the player's
		// move. A submitted or remembered choice is taken as sent.
		choice.Enhance = allEnhancements(choice.Enhance, res)
		if widened, werr := app.resolveLinkLenient(choice); werr == nil {
			res = widened
		}
	}
	fillLinkView(view, res)
	fillAdvanced(view, res)
	view.Back = linkChooseURL(url.Values{"cards": {"1"}, "activity": q["activity"]}, view.Character)
	app.render(ctx, w, http.StatusOK, "link.html", data)
}

// allEnhancements is every optional permission a resolved choice
// offers: the features' enhancements, the role-framed corporation
// permissions, and what this server offers on top, merged with the
// ones already named.
func allEnhancements(named []string, res linkResolved) []string {
	out := append([]string(nil), named...)
	add := func(scope string) {
		if scope != "" && !contains(out, scope) {
			out = append(out, scope)
		}
	}
	for _, e := range res.Enhancements {
		add(e.Scope)
	}
	for _, r := range res.RoleOptIns {
		add(r.Scope)
	}
	for _, e := range res.ServerExtras {
		add(e.Scope)
	}
	return out
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
	for _, r := range res.RoleOptIns {
		view.Roles = append(view.Roles, linkRoleView{Scope: r.Scope, Unlocks: r.Unlocks, Feature: r.Feature, Roles: r.Roles, Ticked: r.Chosen})
	}
	for _, e := range res.ServerExtras {
		view.Server = append(view.Server, linkEnhancementView{Scope: e.Scope, Unlocks: e.Unlocks, Feature: e.Feature, Ticked: e.Chosen})
	}
	var pinned []string
	for _, row := range res.Features {
		if row.Pinned {
			pinned = append(pinned, row.ID)
		}
	}
	var chosen, unchosen []string
	for _, e := range res.Enhancements {
		if e.Chosen {
			chosen = append(chosen, e.Scope)
		} else {
			unchosen = append(unchosen, e.Scope)
		}
	}
	for _, r := range res.RoleOptIns {
		if r.Chosen {
			chosen = append(chosen, r.Scope)
		} else {
			unchosen = append(unchosen, r.Scope)
		}
	}
	for _, e := range res.ServerExtras {
		if e.Chosen {
			chosen = append(chosen, e.Scope)
		} else {
			unchosen = append(unchosen, e.Scope)
		}
	}
	view.Continue = linkEVEURL(pinned, chosen, view.Character)
	all := append(append([]string(nil), chosen...), unchosen...)
	view.Full = linkChooseURL(url.Values{"feature": pinned, "enhance": all, "submitted": {"1"}}, view.Character)

}

// fillAdvanced lists the chosen features grouped by the kind of play
// they serve, each with the permissions it needs, what is ticked now
// marked, so the summary shows what each permission is for.
func fillAdvanced(view *linkView, res linkResolved) {
	in := map[string]linkFeatureRow{}
	for _, row := range res.Features {
		in[row.ID] = row
	}
	chosen := map[string]bool{}
	// A scope that will be requested shows ticked wherever it appears,
	// even where another feature lists it as an enhancement.
	for _, s := range res.Scopes {
		chosen[s] = true
	}
	for _, e := range res.Enhancements {
		if e.Chosen {
			chosen[e.Scope] = true
		}
	}
	for _, r := range res.RoleOptIns {
		if r.Chosen {
			chosen[r.Scope] = true
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
		if _, isIn := in[f.ID]; !isIn {
			continue
		}
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
		for _, r := range f.Role {
			fv.Role = append(fv.Role, linkRoleView{Scope: r.Scope, Unlocks: r.Unlocks, Feature: f.Title, Roles: r.Roles, Ticked: chosen[r.Scope]})
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
	if _, err := app.resolveLink(c); err != nil {
		c = linkChoice{Activities: c.Activities}
		if _, err := app.resolveLink(c); err != nil {
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
// extra permission (the link as it was before the chooser). ok is true
// when a choice was submitted at all — the chooser's submitted=1 marker
// or any choice parameter — even when everything was unticked. Only a
// request with no choice in it at all is "no choice" (the full link);
// an empty submitted choice is a choice of nothing and never widens.
func (app *Application) linkChoiceFromQuery(q url.Values) (linkChoice, bool) {
	if q.Get("full") == "1" {
		return fullLinkChoiceWith(app.linkServerExtras()), true
	}
	nonEmpty := func(in []string) (out []string) {
		for _, s := range in {
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	c := linkChoice{Activities: nonEmpty(q["activity"]), Features: nonEmpty(q["feature"]), Enhance: nonEmpty(q["enhance"])}
	_, hasActivity := q["activity"]
	_, hasFeature := q["feature"]
	_, hasEnhance := q["enhance"]
	submitted := q.Get("submitted") == "1" || hasActivity || hasFeature || hasEnhance
	return c, submitted
}

// resolveLinkLenient is resolveLink for the chooser page: an extra
// permission that belongs to no chosen feature (its feature was just
// unticked) is dropped instead of refused, so the list can be updated.
// The link to EVE (resolveLink) stays strict.
func (app *Application) resolveLinkLenient(c linkChoice) (linkResolved, error) {
	base, err := app.resolveLink(linkChoice{Activities: c.Activities, Features: c.Features})
	if err != nil {
		return linkResolved{}, err
	}
	known := map[string]bool{}
	for _, e := range base.Enhancements {
		known[e.Scope] = true
	}
	for _, r := range base.RoleOptIns {
		known[r.Scope] = true
	}
	for _, e := range base.ServerExtras {
		known[e.Scope] = true
	}
	// A permission some feature declares but no chosen one does is dropped;
	// one nothing declares stays, and the strict resolve refuses it.
	declared := map[string]bool{}
	for _, f := range linkFeatures() {
		for _, s := range f.Enhance {
			declared[s.Scope] = true
		}
		for _, r := range f.Role {
			declared[r.Scope] = true
		}
	}
	for _, e := range app.linkServerExtras() {
		declared[e.Scope] = true
	}
	var keep []string
	for _, s := range c.Enhance {
		if known[s] || !declared[s] {
			keep = append(keep, s)
		}
	}
	c.Enhance = keep
	return app.resolveLink(c)
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
