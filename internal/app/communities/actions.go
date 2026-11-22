package communities

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/humanize"
	"evesynapse/internal/logging"
	"evesynapse/internal/standing"
)

const (
	saveFailed    = "That could not be saved. Try again in a moment."
	staleAction   = "That changed while you were looking. Reload the page and try again."
	notYours      = "Only this community's leader and admins can do that."
	leaderOnly    = "Only this community's leader can do that."
	nameTaken     = "There is already a listed community with that name."
	nameRule      = "A community needs a name of at most 60 characters."
	listedNameKey = "communities_listed_name"
)

var (
	// errStale: the row changed after the page was drawn.
	errStale = errors.New("community changed")
	// errLedMost: the account already leads as many as it may.
	errLedMost = errors.New("account leads the most communities")
	// errNotLeader: someone else leads the community.
	errNotLeader = errors.New("not the leader")
)

// oneRow turns a guarded write's row count into errStale when it
// matched nothing.
func oneRow(n int64, err error) error {
	if err != nil {
		return err
	}
	if n != 1 {
		return errStale
	}
	return nil
}

func ledMostText() string {
	return "One account can lead at most " + strconv.Itoa(ledPerAccount) + " communities."
}

// holdLeadLimit takes the account's leadership lock for the transaction
// and refuses if it cannot lead one more community.
func holdLeadLimit(ctx context.Context, q *db.Queries, user int64) error {
	if err := q.LockAccountLeadership(ctx, user); err != nil {
		return err
	}
	led, err := q.CountCommunitiesLedByUser(ctx, user)
	if err != nil {
		return err
	}
	if led >= ledPerAccount {
		return errLedMost
	}
	return nil
}

// inCommunityTx runs fn in a transaction holding the community's row,
// so changes to who runs it happen one at a time.
func (app *mod) inCommunityTx(ctx context.Context, community int64, fn func(q *db.Queries) error) error {
	tx, err := app.deps.PGPool().Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := app.queries.WithTx(tx)
	if _, err := q.LockCommunity(ctx, community); err != nil {
		return err
	}
	if err := fn(q); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func uniqueViolation(err error) (constraint string, ok bool) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return pgErr.ConstraintName, true
	}
	return "", false
}

func cleanName(raw string) (string, bool) {
	name := strings.Join(strings.Fields(raw), " ")
	return name, name != "" && utf8.RuneCountInString(name) <= nameMax
}

func cleanDescription(raw string) string {
	return humanize.Clip(strings.TrimSpace(raw), descriptionMax)
}

// accountCharacter finds a character of the viewer's account by the id
// a form sent.
func accountCharacter(v viewer, raw string) (db.ListAccountCharactersRow, bool) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return db.ListAccountCharactersRow{}, false
	}
	for _, ch := range v.account {
		if ch.CharacterID == id {
			return ch, true
		}
	}
	return db.ListAccountCharactersRow{}, false
}

// identityOf is the stored identity of an account's character, the
// baseline that later shows it has been sold.
func identityOf(ch db.ListAccountCharactersRow, now time.Time) db.UpsertVerifiedPilotParams {
	return db.UpsertVerifiedPilotParams{CharacterID: ch.CharacterID, Name: ch.Name, OwnerHash: ch.OwnerHash, SeenAt: now}
}

// handleCreate starts a community led by one of the account's
// characters.
func (app *mod) handleCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_ = r.ParseForm()
	back := app.deps.FlashBack(w, r, basePath)
	v := app.viewerOf(ctx)
	ch, ok := accountCharacter(v, r.Form.Get("character"))
	if !ok {
		back("Pick one of your characters to lead the community.")
		return
	}
	name, ok := cleanName(r.Form.Get("name"))
	if !ok {
		back(nameRule)
		return
	}
	policy := r.Form.Get("join_policy")
	if !validPolicy(policy) {
		back("Choose how the community takes new members.")
		return
	}
	listed := r.Form.Get("listed") == "1"
	var invite sql.NullString
	if wantsLink(listed, policy) {
		token, err := newInviteToken()
		if err != nil {
			logging.Errorf("communities: invite token: %v", err)
			back(saveFailed)
			return
		}
		invite = sql.NullString{String: token, Valid: true}
	}
	params := db.CreateCommunityParams{
		Name: name, Description: cleanDescription(r.Form.Get("description")), JoinPolicy: policy,
		Listed: listed, InviteToken: invite, CreatedByCharacter: ch.CharacterID,
		CreatedAt: time.Now().UTC(),
	}
	base := slugFor(name)
	var err error
	for attempt := range 6 {
		params.Slug = base
		if attempt > 0 {
			suffix, serr := newInviteToken()
			if serr != nil {
				back(saveFailed)
				return
			}
			params.Slug = base + "-" + strings.ToLower(strings.NewReplacer("-", "", "_", "").Replace(suffix))[:5]
		}
		err = app.insertCommunity(ctx, v.userID, params, ch)
		constraint, duplicate := uniqueViolation(err)
		if duplicate && constraint == listedNameKey {
			back(nameTaken)
			return
		}
		if !duplicate {
			break
		}
	}
	if errors.Is(err, errLedMost) {
		back(ledMostText())
		return
	}
	if err != nil {
		logging.Errorf("communities: create %q: %v", name, err)
		back(saveFailed)
		return
	}
	logging.Infof("communities: user %d started %q", v.userID, name)
	app.deps.Flash(ctx, name+" is ready. Share its page to bring people in.")
	http.Redirect(w, r, pathOf(params.Slug), http.StatusSeeOther)
}

func (app *mod) insertCommunity(ctx context.Context, user int64, params db.CreateCommunityParams, ch db.ListAccountCharactersRow) error {
	tx, err := app.deps.PGPool().Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := app.queries.WithTx(tx)
	if err := holdLeadLimit(ctx, q, user); err != nil {
		return err
	}
	c, err := q.CreateCommunity(ctx, params)
	if err != nil {
		return err
	}
	if err := q.UpsertVerifiedPilot(ctx, identityOf(ch, params.CreatedAt)); err != nil {
		return err
	}
	if _, err := q.AddCommunityMember(ctx, db.AddCommunityMemberParams{
		CommunityID: c.ID, CharacterID: ch.CharacterID, Name: ch.Name, Role: standing.CommunityLeader,
		State: stateActive, JoinedAt: params.CreatedAt, AddedBy: ch.CharacterID,
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// handleJoin adds the viewer's character, or asks to. Someone not
// signed in goes through the sign-in that asks for no permissions.
func (app *mod) handleJoin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := app.communityFrom(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	_ = r.ParseForm()
	path := pathOf(c.Slug)
	invite := r.Form.Get("invite")
	inviteOK := validInvite(c, invite)
	if inviteOK {
		path += "?invite=" + url.QueryEscape(invite)
	}
	v := app.viewerOf(ctx)
	if !visibleTo(c, app.placesOf(ctx, c.ID, v), invite) {
		http.NotFound(w, r)
		return
	}
	if v.userID == 0 && !v.isPilot {
		http.Redirect(w, r, signInHref(path), http.StatusSeeOther)
		return
	}
	back := app.deps.FlashBack(w, r, path)
	if c.ArchivedAt.Valid {
		back("This community is closed.")
		return
	}
	if c.JoinPolicy == policyInvite && !inviteOK {
		back("This community only takes new members through its invite link.")
		return
	}
	now := time.Now().UTC()
	var id int64
	var name string
	var identity *db.UpsertVerifiedPilotParams
	if v.isPilot {
		id, name = v.pilot.CharacterID, v.pilot.Name
	} else {
		ch, ok := accountCharacter(v, r.Form.Get("character"))
		if !ok {
			back("Pick one of your characters to join with.")
			return
		}
		id, name = ch.CharacterID, ch.Name
		row := identityOf(ch, now)
		identity = &row
	}
	state := stateActive
	if c.JoinPolicy == policyApproval {
		state = statePending
	}
	added, err := app.addMember(ctx, identity, db.AddCommunityMemberParams{
		CommunityID: c.ID, CharacterID: id, Name: name, Role: standing.CommunityMember,
		State: state, JoinedAt: now, AddedBy: id,
	})
	if err != nil {
		logging.Errorf("communities: %d joining %d: %v", id, c.ID, err)
		back(saveFailed)
		return
	}
	if added == 0 {
		existing, err := app.queries.GetCommunityMember(ctx, db.GetCommunityMemberParams{CommunityID: c.ID, CharacterID: id})
		if err == nil && existing.State == stateBanned {
			back(name + " is banned from this community.")
			return
		}
		back(name + " is already here.")
		return
	}
	if state == statePending {
		back("Request sent. A leader or admin of " + c.Name + " will answer it.")
		return
	}
	back(name + " joined " + c.Name + ".")
}

func (app *mod) addMember(ctx context.Context, identity *db.UpsertVerifiedPilotParams, member db.AddCommunityMemberParams) (int64, error) {
	tx, err := app.deps.PGPool().Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	q := app.queries.WithTx(tx)
	if identity != nil {
		if err := q.UpsertVerifiedPilot(ctx, *identity); err != nil {
			return 0, err
		}
	}
	added, err := q.AddCommunityMember(ctx, member)
	if err != nil {
		return 0, err
	}
	return added, tx.Commit(ctx)
}

// handleLeave takes the viewer's character out, or withdraws its
// request. A ban stays, and a leader hands over first.
func (app *mod) handleLeave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, ok := app.communityFrom(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	_ = r.ParseForm()
	v := app.viewerOf(ctx)
	if !visibleTo(c, app.placesOf(ctx, c.ID, v), r.Form.Get("invite")) {
		http.NotFound(w, r)
		return
	}
	back := app.deps.FlashBack(w, r, pathOf(c.Slug))
	id, _ := strconv.ParseInt(r.Form.Get("character"), 10, 64)
	if !slices.Contains(v.characterIDs(), id) {
		back("That is not one of your characters.")
		return
	}
	row, err := app.queries.GetCommunityMember(ctx, db.GetCommunityMemberParams{CommunityID: c.ID, CharacterID: id})
	if err != nil {
		back("That character is not in this community.")
		return
	}
	if row.State == stateBanned {
		back(row.Name + " is banned from this community.")
		return
	}
	if row.Role == standing.CommunityLeader {
		if !c.ArchivedAt.Valid {
			back("Make someone else the leader first, or close the community.")
			return
		}
		err = oneRow(app.queries.DeleteClosedCommunityLeader(ctx, db.DeleteClosedCommunityLeaderParams{CommunityID: c.ID, CharacterID: id}))
	} else {
		err = oneRow(app.queries.DeleteCommunityMember(ctx, db.DeleteCommunityMemberParams{CommunityID: c.ID, CharacterID: id, State: row.State, Role: row.Role}))
	}
	if errors.Is(err, errStale) {
		back(staleAction)
		return
	}
	if err != nil {
		logging.Errorf("communities: %d leaving %d: %v", id, c.ID, err)
		back(saveFailed)
		return
	}
	back(row.Name + " left " + c.Name + ".")
}

// managedRequest is a form post from someone who runs the community.
type managedRequest struct {
	community db.Community
	viewer    viewer
	role      string
	back      func(string)
}

func (app *mod) managedFrom(w http.ResponseWriter, r *http.Request) (managedRequest, bool) {
	c, ok := app.communityFrom(r)
	if !ok {
		http.NotFound(w, r)
		return managedRequest{}, false
	}
	_ = r.ParseForm()
	path := pathOf(c.Slug)
	if show := r.Form.Get("show"); show == "requests" || show == "banned" {
		path += "?show=" + show
	}
	back := app.deps.FlashBack(w, r, path)
	v := app.viewerOf(r.Context())
	role := managerRole(v, app.placesOf(r.Context(), c.ID, v))
	if role == "" {
		back(notYours)
		return managedRequest{}, false
	}
	return managedRequest{community: c, viewer: v, role: role, back: back}, true
}

func (app *mod) handleSettings(w http.ResponseWriter, r *http.Request) {
	m, ok := app.managedFrom(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	if m.community.ArchivedAt.Valid {
		m.back("This community is closed. Reopen it to change its settings.")
		return
	}
	name, ok := cleanName(r.Form.Get("name"))
	if !ok {
		m.back(nameRule)
		return
	}
	policy := r.Form.Get("join_policy")
	if !validPolicy(policy) {
		m.back("Choose how the community takes new members.")
		return
	}
	listed := r.Form.Get("listed") == "1"
	invite := sql.NullString{}
	if wantsLink(listed, policy) {
		invite = m.community.InviteToken
		if !invite.Valid {
			token, err := newInviteToken()
			if err != nil {
				logging.Errorf("communities: invite token: %v", err)
				m.back(saveFailed)
				return
			}
			invite = sql.NullString{String: token, Valid: true}
		}
	}
	err := app.queries.UpdateCommunitySettings(ctx, db.UpdateCommunitySettingsParams{
		ID: m.community.ID, Name: name, Description: cleanDescription(r.Form.Get("description")),
		JoinPolicy: policy, Listed: listed, InviteToken: invite,
	})
	if constraint, duplicate := uniqueViolation(err); duplicate && constraint == listedNameKey {
		m.back(nameTaken)
		return
	}
	if err != nil {
		logging.Errorf("communities: settings of %d: %v", m.community.ID, err)
		m.back(saveFailed)
		return
	}
	m.back("Settings saved.")
}

// handleNewInvite replaces the invite link; the old one stops working.
func (app *mod) handleNewInvite(w http.ResponseWriter, r *http.Request) {
	m, ok := app.managedFrom(w, r)
	if !ok {
		return
	}
	if !wantsLink(m.community.Listed, m.community.JoinPolicy) || m.community.ArchivedAt.Valid {
		m.back("This community does not use a private link.")
		return
	}
	token, err := newInviteToken()
	if err == nil {
		err = app.queries.SetCommunityInvite(r.Context(), db.SetCommunityInviteParams{
			ID: m.community.ID, InviteToken: sql.NullString{String: token, Valid: true},
		})
	}
	if err != nil {
		logging.Errorf("communities: new invite for %d: %v", m.community.ID, err)
		m.back(saveFailed)
		return
	}
	m.back("New link made. The old link no longer works.")
}

func (app *mod) handleClose(w http.ResponseWriter, r *http.Request) {
	m, ok := app.managedFrom(w, r)
	if !ok {
		return
	}
	if m.role != standing.CommunityLeader {
		m.back(leaderOnly)
		return
	}
	if _, err := app.queries.ArchiveCommunity(r.Context(), db.ArchiveCommunityParams{
		ID: m.community.ID, ArchivedAt: sql.NullTime{Time: time.Now().UTC(), Valid: true},
	}); err != nil {
		logging.Errorf("communities: close %d: %v", m.community.ID, err)
		m.back(saveFailed)
		return
	}
	m.back(m.community.Name + " is closed. You can reopen it from this page.")
}

func (app *mod) handleReopen(w http.ResponseWriter, r *http.Request) {
	m, ok := app.managedFrom(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	ids := m.viewer.characterIDs()
	err := app.inCommunityTx(ctx, m.community.ID, func(q *db.Queries) error {
		if err := holdLeadLimit(ctx, q, m.viewer.userID); err != nil {
			return err
		}
		leader, err := q.GetCommunityLeader(ctx, m.community.ID)
		switch {
		case err == nil:
			if !slices.Contains(ids, leader.CharacterID) {
				return errNotLeader
			}
		case errors.Is(err, pgx.ErrNoRows):
			// A community left with no leader passes to the admin
			// who reopens it.
			places, err := q.ListCommunityMembershipsFor(ctx, db.ListCommunityMembershipsForParams{CommunityID: m.community.ID, CharacterIds: ids})
			if err != nil {
				return err
			}
			heir := slices.IndexFunc(places, func(p db.CommunityMember) bool {
				return p.State == stateActive && p.Role == standing.CommunityAdmin
			})
			if heir < 0 {
				return errStale
			}
			if err := oneRow(q.SetCommunityMemberRole(ctx, db.SetCommunityMemberRoleParams{
				CommunityID: m.community.ID, CharacterID: places[heir].CharacterID,
				Role: standing.CommunityLeader, FromRole: standing.CommunityAdmin,
			})); err != nil {
				return err
			}
		default:
			return err
		}
		return oneRow(q.RestoreCommunity(ctx, m.community.ID))
	})
	if constraint, duplicate := uniqueViolation(err); duplicate && constraint == listedNameKey {
		m.back("Another listed community now has this name. Ask its leader, or start a new one.")
		return
	}
	switch {
	case errors.Is(err, errNotLeader):
		m.back(leaderOnly)
	case errors.Is(err, errLedMost):
		m.back(ledMostText())
	case errors.Is(err, errStale):
		m.back(staleAction)
	case err != nil:
		logging.Errorf("communities: reopen %d: %v", m.community.ID, err)
		m.back(saveFailed)
	default:
		m.back(m.community.Name + " is open again.")
	}
}

// handleMemberAction applies one action to a member. What is allowed
// comes from actionsFor, the same list the page offers.
func (app *mod) handleMemberAction(w http.ResponseWriter, r *http.Request) {
	m, ok := app.managedFrom(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	if m.community.ArchivedAt.Valid {
		m.back("This community is closed.")
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "characterID"), 10, 64)
	if err != nil {
		m.back("That character is not in this community.")
		return
	}
	target, err := app.queries.GetCommunityMember(ctx, db.GetCommunityMemberParams{CommunityID: m.community.ID, CharacterID: id})
	if err != nil {
		m.back("That character is not in this community.")
		return
	}
	working, err := app.queries.ListCharactersWithWorkingAccount(ctx, []int64{id})
	if err != nil {
		logging.Errorf("communities: account of %d: %v", id, err)
		m.back(saveFailed)
		return
	}
	do := r.Form.Get("do")
	allowed := false
	for _, action := range actionsFor(m.role, target.Role, target.State, target.Name, len(working) == 1) {
		allowed = allowed || action.Do == do
	}
	if !allowed {
		m.back("You cannot do that to " + target.Name + ".")
		return
	}
	message, err := app.applyAction(ctx, m.community.ID, m.viewer.characterIDs(), target, do)
	if errors.Is(err, errStale) {
		m.back(staleAction)
		return
	}
	if errors.Is(err, errLedMost) {
		m.back(target.Name + " already leads as many communities as one account can.")
		return
	}
	if err != nil {
		logging.Errorf("communities: %s on %d in %d: %v", do, id, m.community.ID, err)
		m.back(saveFailed)
		return
	}
	logging.Infof("communities: user %d: %s on character %d in community %d", m.viewer.userID, do, id, m.community.ID)
	m.back(message)
}

func (app *mod) applyAction(ctx context.Context, community int64, actors []int64, target db.CommunityMember, do string) (string, error) {
	remove := func(state string) error {
		return oneRow(app.queries.DeleteCommunityMember(ctx, db.DeleteCommunityMemberParams{
			CommunityID: community, CharacterID: target.CharacterID, State: state, Role: target.Role,
		}))
	}
	setState := func(state string) error {
		return oneRow(app.queries.SetCommunityMemberState(ctx, db.SetCommunityMemberStateParams{
			CommunityID: community, CharacterID: target.CharacterID, State: state,
			FromState: target.State, FromRole: target.Role,
		}))
	}
	setRole := func(role string) error {
		return oneRow(app.queries.SetCommunityMemberRole(ctx, db.SetCommunityMemberRoleParams{
			CommunityID: community, CharacterID: target.CharacterID, Role: role, FromRole: target.Role,
		}))
	}
	switch do {
	case doApprove:
		return target.Name + " is now a member.", setState(stateActive)
	case doDecline:
		return "Request from " + target.Name + " declined.", remove(statePending)
	case doBan:
		return target.Name + " is banned.", setState(stateBanned)
	case doUnban:
		return "Ban on " + target.Name + " lifted.", remove(stateBanned)
	case doRemove:
		return target.Name + " removed.", remove(stateActive)
	case doAdmin:
		return target.Name + " is now an admin.", setRole(standing.CommunityAdmin)
	case doMember:
		return target.Name + " is no longer an admin.", setRole(standing.CommunityMember)
	case doLead:
		return target.Name + " now leads this community.", app.handOver(ctx, community, actors, target.CharacterID)
	}
	return "", errors.New("unknown action " + do)
}

// handOver makes an active member with a working account the leader
// and the current leader an admin. It commits only if both change, so a
// community is never left without a leader.
func (app *mod) handOver(ctx context.Context, community int64, actors []int64, to int64) error {
	return app.inCommunityTx(ctx, community, func(q *db.Queries) error {
		leader, err := q.GetCommunityLeader(ctx, community)
		if errors.Is(err, pgx.ErrNoRows) {
			return errStale
		}
		if err != nil {
			return err
		}
		if !slices.Contains(actors, leader.CharacterID) {
			return errStale
		}
		target, err := q.GetCommunityMember(ctx, db.GetCommunityMemberParams{CommunityID: community, CharacterID: to})
		if errors.Is(err, pgx.ErrNoRows) {
			return errStale
		}
		if err != nil {
			return err
		}
		if target.State != stateActive || target.Role == standing.CommunityLeader {
			return errStale
		}
		working, err := q.ListCharactersWithWorkingAccount(ctx, []int64{to})
		if err != nil {
			return err
		}
		if len(working) != 1 {
			return errStale
		}
		ch, err := q.GetCharacter(ctx, to)
		if err != nil {
			return err
		}
		if err := holdLeadLimit(ctx, q, ch.UserID); err != nil {
			return err
		}
		if err := oneRow(q.SetCommunityMemberRole(ctx, db.SetCommunityMemberRoleParams{
			CommunityID: community, CharacterID: leader.CharacterID,
			Role: standing.CommunityAdmin, FromRole: standing.CommunityLeader,
		})); err != nil {
			return err
		}
		return oneRow(q.SetCommunityMemberRole(ctx, db.SetCommunityMemberRoleParams{
			CommunityID: community, CharacterID: to, Role: standing.CommunityLeader, FromRole: target.Role,
		}))
	})
}
