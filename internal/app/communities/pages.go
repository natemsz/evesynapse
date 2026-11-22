package communities

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/humanize"
	"evesynapse/internal/logging"
	"evesynapse/internal/standing"
)

// accountHref opens the sign-up chooser with nothing ticked, so coming
// from Fleets never preselects permissions the person did not pick.
const accountHref = "/auth/link?from=cards"

// viewer is who is reading: an account, a verified pilot, or nobody.
type viewer struct {
	userID  int64
	pilot   module.VerifiedPilot
	isPilot bool
	account []db.ListAccountCharactersRow
}

func (app *mod) viewerOf(ctx context.Context) viewer {
	v := viewer{userID: app.deps.UserID(ctx)}
	if v.userID == 0 {
		v.pilot, v.isPilot = app.deps.VerifiedPilot(ctx)
		return v
	}
	rows, err := app.queries.ListAccountCharacters(ctx, v.userID)
	if err != nil {
		logging.Errorf("communities: characters of user %d: %v", v.userID, err)
	}
	v.account = rows
	return v
}

func (v viewer) characterIDs() []int64 {
	if v.isPilot {
		return []int64{v.pilot.CharacterID}
	}
	ids := make([]int64, 0, len(v.account))
	for _, ch := range v.account {
		ids = append(ids, ch.CharacterID)
	}
	return ids
}

func (v viewer) level() string {
	switch {
	case v.userID != 0:
		return "account"
	case v.isPilot:
		return "pilot"
	}
	return "anonymous"
}

// characterOption is one of the viewer's characters in a picker.
type characterOption struct {
	ID       int64
	Name     string
	Selected bool
}

func signInHref(next string) string {
	return "/auth/pilot?next=" + url.QueryEscape(next)
}

func pathOf(slug string) string { return basePath + slug + "/" }

// --- The directory ---

type directoryRow struct {
	Path, Name, Description, Members, Policy string
}

type mineRow struct {
	Path, Name, Place, Members string
	Pending                    string // requests waiting, for a leader or admin
	Closed                     bool
}

type directoryView struct {
	Find       string
	Listed     []directoryRow
	Mine       []mineRow
	Level      string
	PilotName  string
	SignIn     string
	Account    string
	Characters []characterOption
	AtLimit    bool
	LedMost    int
	Policies   []policyOption
}

func (app *mod) handleDirectory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := &PageData{ShellData: app.deps.PublicShell(ctx)}
	v := app.viewerOf(ctx)
	view := &directoryView{
		Find: humanize.Clip(strings.TrimSpace(r.URL.Query().Get("q")), nameMax), Level: v.level(),
		PilotName: v.pilot.Name, SignIn: signInHref(basePath), Account: accountHref,
		LedMost: ledPerAccount, Policies: policyOptions(policyApproval),
	}
	rows, err := app.queries.ListDirectoryCommunities(ctx, likePattern(view.Find))
	if err != nil {
		logging.Errorf("communities: directory: %v", err)
		data.Error = "Could not load the communities. Try again in a moment."
	}
	for _, row := range rows {
		view.Listed = append(view.Listed, directoryRow{
			Path: pathOf(row.Slug), Name: row.Name, Description: row.Description,
			Members: countText(row.MemberCount), Policy: policyLabel(row.JoinPolicy),
		})
	}
	if ids := v.characterIDs(); len(ids) > 0 {
		mine, err := app.queries.ListCommunitiesForCharacters(ctx, ids)
		if err != nil {
			logging.Errorf("communities: communities of user %d: %v", v.userID, err)
		}
		for _, row := range mine {
			m := mineRow{Path: pathOf(row.Slug), Name: row.Name, Place: roleLabel(row.Role), Members: countText(row.MemberCount) + " " + memberWord(row.MemberCount), Closed: row.ArchivedAt.Valid}
			if row.State == statePending {
				m.Place = "Waiting for approval"
			}
			if v.userID != 0 && row.State == stateActive && row.Role != standing.CommunityMember && row.PendingCount > 0 {
				m.Pending = countText(row.PendingCount)
			}
			view.Mine = append(view.Mine, m)
		}
		if v.userID != 0 {
			led, err := app.queries.CountCommunitiesLedByUser(ctx, v.userID)
			if err != nil {
				logging.Errorf("communities: led by user %d: %v", v.userID, err)
			}
			view.AtLimit = led >= ledPerAccount
		}
	}
	acting := app.deps.SessionCharacterID(ctx)
	for _, ch := range v.account {
		view.Characters = append(view.Characters, characterOption{ID: ch.CharacterID, Name: ch.Name, Selected: ch.CharacterID == acting})
	}
	data.Directory = view
	app.deps.Render(ctx, w, http.StatusOK, "communities.html", data)
}

// --- One community ---

type placeView struct {
	CharacterID int64
	Name, Label string
	CanLeave    bool
	Banned      bool
	// LeaveAsk is the question asked before leaving.
	LeaveAsk string
}

type personView struct {
	Role string
	Link template.HTML
}

// rowAction is one member action as the form that sends it.
type rowAction struct {
	memberAction
	Href, Show string
}

type memberRowView struct {
	Name         template.HTML
	Role, Joined string
	Actions      []rowAction
}

type peopleTab struct {
	Label, Href, Count string
	On                 bool
}

type communityView struct {
	ID                           int64
	Path, Name, Description      string
	Policy, PolicyValue, Members string
	MemberWord                   string
	Listed, Closed               bool
	Level, PilotName, SignIn     string
	Account                      string
	Yours                        []placeView
	Joinable                     []characterOption
	JoinLabel, Invite            string
	NeedsInvite                  bool
	SeesPeople, Manages, Leads   bool
	Managers                     []personView
	Tabs                         []peopleTab
	Show, Find                   string
	Rows                         []memberRowView
	RowsInline                   bool
	NextHref                     string
	Policies                     []policyOption
	InviteURL, GroupsHref        string
	InviteHint                   string
	CanReopen                    bool
}

func (app *mod) communityFrom(r *http.Request) (db.Community, bool) {
	c, err := app.queries.GetCommunityBySlug(r.Context(), chi.URLParam(r, "slug"))
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			logging.Errorf("communities: read %q: %v", chi.URLParam(r, "slug"), err)
		}
		return db.Community{}, false
	}
	return c, true
}

// placesOf is the rows the viewer's characters hold in a community.
func (app *mod) placesOf(ctx context.Context, community int64, v viewer) []db.CommunityMember {
	ids := v.characterIDs()
	if len(ids) == 0 {
		return nil
	}
	rows, err := app.queries.ListCommunityMembershipsFor(ctx, db.ListCommunityMembershipsForParams{CommunityID: community, CharacterIds: ids})
	if err != nil {
		logging.Errorf("communities: places in %d: %v", community, err)
	}
	return rows
}

// managerRole is the leader or admin place an account holds in a
// community, "" for anyone else. A verified pilot never has one,
// whatever a stored row says.
func managerRole(v viewer, places []db.CommunityMember) string {
	if v.userID == 0 {
		return ""
	}
	best := ""
	for _, p := range places {
		if p.State == stateActive && standing.CommunityRank(p.Role) > standing.CommunityRank(best) {
			best = p.Role
		}
	}
	if standing.CommunityRank(best) < standing.CommunityRank(standing.CommunityAdmin) {
		return ""
	}
	return best
}

func (app *mod) handleCommunity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := app.communityFrom(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	if !c.Listed || c.ArchivedAt.Valid || q.Get("invite") != "" {
		w.Header().Set("X-Robots-Tag", "noindex")
	}
	v := app.viewerOf(ctx)
	places := app.placesOf(ctx, c.ID, v)
	if !visibleTo(c, places, q.Get("invite")) {
		http.NotFound(w, r)
		return
	}
	data := &PageData{ShellData: app.deps.PublicShell(ctx)}
	role := managerRole(v, places)
	path := pathOf(c.Slug)
	view := &communityView{
		ID: c.ID, Path: path, Name: c.Name, Description: c.Description,
		Policy: policyLabel(c.JoinPolicy), PolicyValue: c.JoinPolicy, Members: countText(c.MemberCount), MemberWord: memberWord(c.MemberCount),
		Listed: c.Listed, Closed: c.ArchivedAt.Valid, Level: v.level(), PilotName: v.pilot.Name,
		Account: accountHref, JoinLabel: "Join", Manages: role != "", Leads: role == standing.CommunityLeader,
	}
	if c.JoinPolicy == policyApproval {
		view.JoinLabel = "Ask to join"
	}
	next := path
	if invite := q.Get("invite"); validInvite(c, invite) {
		view.Invite = invite
		next += "?invite=" + url.QueryEscape(invite)
	}
	view.SignIn = signInHref(next)
	view.NeedsInvite = c.JoinPolicy == policyInvite && view.Invite == ""

	taken := make(map[int64]bool, len(places))
	for _, p := range places {
		taken[p.CharacterID] = true
		place := placeView{CharacterID: p.CharacterID, Name: p.Name, Label: roleLabel(p.Role), Banned: p.State == stateBanned}
		switch p.State {
		case statePending:
			place.Label = "Waiting for approval"
		case stateBanned:
			place.Label = "Banned"
		}
		place.CanLeave = p.State != stateBanned && (p.Role != standing.CommunityLeader || view.Closed)
		place.LeaveAsk = "Take " + p.Name + " out of " + c.Name + "?"
		if p.Role == standing.CommunityLeader {
			place.LeaveAsk = "Leave " + c.Name + "? An admin can reopen it and become its leader. With no admin it stays closed."
		}
		view.Yours = append(view.Yours, place)
		if p.State == stateActive {
			view.SeesPeople = true
		}
	}
	if !view.Closed && !view.NeedsInvite {
		switch {
		case v.isPilot && !taken[v.pilot.CharacterID]:
			view.Joinable = []characterOption{{ID: v.pilot.CharacterID, Name: v.pilot.Name}}
		case v.userID != 0:
			acting := app.deps.SessionCharacterID(ctx)
			for _, ch := range v.account {
				if !taken[ch.CharacterID] {
					view.Joinable = append(view.Joinable, characterOption{ID: ch.CharacterID, Name: ch.Name, Selected: ch.CharacterID == acting})
				}
			}
		}
	}
	if view.SeesPeople {
		app.fillPeople(ctx, view, c, role, q)
	}
	if view.Manages {
		view.Policies = policyOptions(c.JoinPolicy)
		if view.Closed {
			_, err := app.queries.GetCommunityLeader(ctx, c.ID)
			view.CanReopen = view.Leads || errors.Is(err, pgx.ErrNoRows)
		}
		if c.InviteToken.Valid {
			view.InviteHint = "Anyone with this link can see this community and join it."
			if c.JoinPolicy == policyApproval {
				view.InviteHint = "Anyone with this link can see this community and ask to join."
			}
			invitePath := path + "?invite=" + url.QueryEscape(c.InviteToken.String)
			view.InviteURL = app.deps.PublicPageURL(invitePath)
			if view.InviteURL == "" {
				view.InviteURL = invitePath
			}
		}
		if app.deps.ModuleEnabled("groups") {
			view.GroupsHref = standing.Address("/groups/", standing.Owner{Kind: standing.OwnerCommunity, ID: c.ID}.Key())
		}
	}
	data.Community = view
	app.deps.Render(ctx, w, http.StatusOK, "community.html", data)
}

// fillPeople reads who runs the community and one page of its members.
// Names are for members only: the public page shows the count.
func (app *mod) fillPeople(ctx context.Context, view *communityView, c db.Community, role string, q url.Values) {
	managers, err := app.queries.ListCommunityManagers(ctx, c.ID)
	if err != nil {
		logging.Errorf("communities: managers of %d: %v", c.ID, err)
	}
	for _, m := range managers {
		view.Managers = append(view.Managers, personView{Role: roleLabel(m.Role), Link: app.deps.CharLinkHTML(ctx, m.CharacterID, m.Name)})
	}

	state, show := stateActive, ""
	if role != "" {
		switch q.Get("show") {
		case "requests":
			state, show = statePending, "requests"
		case "banned":
			state, show = stateBanned, "banned"
		}
		view.Tabs = []peopleTab{
			{Label: "Members", Href: view.Path, Count: countText(c.MemberCount), On: show == ""},
			{Label: "Requests", Href: view.Path + "?show=requests", Count: countText(c.PendingCount), On: show == "requests"},
			{Label: "Banned", Href: view.Path + "?show=banned", On: show == "banned"},
		}
	}
	view.Show = show
	view.RowsInline = state != stateActive
	view.Find = humanize.Clip(strings.TrimSpace(q.Get("find")), nameMax)
	afterName := q.Get("after")
	afterID, _ := strconv.ParseInt(q.Get("id"), 10, 64)
	if afterName == "" {
		afterName, afterID = strings.ToLower(view.Find), 0
	}
	rows, err := app.queries.ListCommunityMembersPage(ctx, db.ListCommunityMembersPageParams{
		CommunityID: c.ID, State: state, AfterName: afterName, AfterID: afterID, MaxRows: membersPerPage + 1,
	})
	if err != nil {
		logging.Errorf("communities: members of %d: %v", c.ID, err)
	}
	if len(rows) > membersPerPage {
		rows = rows[:membersPerPage]
		last := rows[len(rows)-1]
		more := url.Values{"after": {last.SortName}, "id": {strconv.FormatInt(last.CharacterID, 10)}}
		if show != "" {
			more.Set("show", show)
		}
		view.NextHref = view.Path + "?" + more.Encode()
	}
	working := map[int64]bool{}
	if role != "" && state == stateActive && len(rows) > 0 {
		ids := make([]int64, 0, len(rows))
		for _, row := range rows {
			ids = append(ids, row.CharacterID)
		}
		found, err := app.queries.ListCharactersWithWorkingAccount(ctx, ids)
		if err != nil {
			logging.Errorf("communities: accounts of members of %d: %v", c.ID, err)
		}
		for _, id := range found {
			working[id] = true
		}
	}
	for _, row := range rows {
		out := memberRowView{
			Name: app.deps.CharLinkHTML(ctx, row.CharacterID, row.Name),
			Role: roleLabel(row.Role), Joined: row.JoinedAt.UTC().Format("2006-01-02"),
		}
		if !view.Closed {
			href := view.Path + "members/" + strconv.FormatInt(row.CharacterID, 10)
			for _, action := range actionsFor(role, row.Role, row.State, row.Name, working[row.CharacterID]) {
				out.Actions = append(out.Actions, rowAction{memberAction: action, Href: href, Show: show})
			}
		}
		view.Rows = append(view.Rows, out)
	}
}
