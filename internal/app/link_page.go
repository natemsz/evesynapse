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
// The sign-up and character-link readout (/auth/link).
//
// One page, one computation: the readout lists every feature, grouped
// by kind of play, with every permission it uses beneath it. There is
// no separate extras step: the optional scopes are part of what the
// features need to work as expected, so a fresh visit starts from the
// full selection (every feature, every permission) and the player
// unticks anything they do not want. A feature another one needs
// stays, and says why.
//
// The server works everything out; the page only carries choices. The
// form submits ids to EVE (/auth/eve), never scope strings the browser
// sends. An account remembers its last choice, so the next character
// link opens on the readout with that choice ticked instead.
// ---------------------------------------------------------------------------

const sessionLinkChoice = "link_choice"

// linkView is the readout page.
type linkView struct {
	Features   []linkFeatureView
	Groups     []linkGroupView       // every feature by kind of play, with what each permission is for
	Roles      []linkRoleView        // corporation permissions, framed by the in-game role they need
	Server     []linkEnhancementView // what this server offers on top
	Count      int                   // permissions EVE will ask for
	Required   int
	Remembered bool
	Character  int64
	Error      string
	Hidden     []linkHidden // choices to carry across a form
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

// handleLinkChoose renders the readout. A visit with no choice in it
// starts from the account's remembered choice, or from the full
// selection when there is none: every feature and every permission
// ticked. A submitted choice (submitted=1, or any choice parameter)
// is taken exactly as sent, so unticking everything stays zero and
// never widens back to the full set.
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
		} else {
			choice = fullLinkChoiceWith(app.linkServerExtras())
		}
	}

	status := http.StatusOK
	res, err := app.resolveLinkLenient(choice)
	if err != nil {
		view.Error = "That selection could not be read: " + err.Error() + "."
		status = http.StatusBadRequest
		res, err = app.resolveLinkLenient(fullLinkChoiceWith(app.linkServerExtras()))
		if err != nil {
			http.Error(w, "That selection could not be read.", http.StatusBadRequest)
			return
		}
	}
	fillLinkView(view, res)
	fillAdvanced(view, res, false)
	app.render(ctx, w, status, "link.html", data)
}

// fillLinkView puts a resolved choice on the page: the features that will
// be turned on, the permission count, and the server extras.
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
	for _, r := range res.RoleOptIns {
		view.Roles = append(view.Roles, linkRoleView{Scope: r.Scope, Unlocks: r.Unlocks, Feature: r.Feature, Roles: r.Roles, Ticked: r.Chosen})
	}
	for _, e := range res.ServerExtras {
		view.Server = append(view.Server, linkEnhancementView{Scope: e.Scope, Unlocks: e.Unlocks, Feature: e.Feature, Ticked: e.Chosen})
	}
}

// fillAdvanced lists every feature grouped by the kind of play it
// serves, each with the permissions it needs, what is ticked now marked.
func fillAdvanced(view *linkView, res linkResolved, onlyChosen bool) {
	in := map[string]linkFeatureRow{}
	for _, row := range res.Features {
		in[row.ID] = row
	}
	// A permission shows ticked wherever it appears when it will be
	// requested at all: as a chosen enhancement, or as a scope some
	// ticked feature cannot work without (the same scope is often one
	// feature's requirement and another's enhancement).
	chosen := map[string]bool{}
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
		if _, isIn := in[f.ID]; onlyChosen && !isIn {
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
