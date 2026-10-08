package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Ops: EveSynapse's own calendar entries. An op is a planned fleet for
// one corporation. Members with a character in that corporation see
// it on the calendar beside their in-game events and sign up to it,
// saying what they will bring.
//
// Who may create one is decided by ESI, not by a setting inside the
// app: a character holding one of the manager roles (Director unless
// OPS_MANAGER_ROLES says otherwise) in the corporation, as its stored
// roles snapshot reports. The app keeps no role list of its own to
// fall out of step with the game.
//
// All times are EVE time (UTC), as everywhere else in the app.
// ---------------------------------------------------------------------------

// Sign-up answers. Stored in op_signups.response; never rename one.
const (
	opYes   = "yes"
	opMaybe = "maybe"
	opNo    = "no"
)

// opFleetRoles are the parts a pilot can say they will play, in the
// order the form offers them. Stored as written.
var opFleetRoles = []string{"DPS", "Logistics", "Tackle", "Scout", "EWAR", "Command", "Other"}

// Limits on what an op's form accepts.
const (
	opTitleMax       = 120
	opTextMax        = 2000
	opShortMax       = 120
	opMinDuration    = 15
	opMaxDuration    = 24 * 60
	opMaxAhead       = 366 * 24 * time.Hour
	opMaxBehind      = 30 * 24 * time.Hour
	opDateTimeLayout = "2006-01-02T15:04" // what <input type="datetime-local"> sends
)

// opMember is one of the account's characters as ops see it: which
// corporation it is in, and whether it may manage that corporation's
// ops.
type opMember struct {
	CharacterID   int64
	Name          string
	CorporationID int64
	Manager       bool
}

// opMembers lists the account's characters with their corporations,
// from stored data only.
func (app *Application) opMembers(ctx context.Context, userID int64) []opMember {
	rows, err := app.queries.ListCharacterCorporationsByUser(ctx, userID)
	if err != nil {
		logging.Errorf("ops: characters of user %d: %v", userID, err)
		return nil
	}
	out := make([]opMember, 0, len(rows))
	for _, row := range rows {
		m := opMember{CharacterID: row.CharacterID, Name: row.Name, CorporationID: row.CorporationID}
		var roles esi.CharacterRoles
		if app.loadCorpSnapshot(ctx, row.CharacterID, esi.SnapCorpRoles, &roles) {
			m.Manager = app.cfg.holdsOpsManagerRole(roles.Roles)
		}
		out = append(out, m)
	}
	return out
}

// opCorps lists the corporations the account has a character in, and
// opManaged those where one of them may manage ops.
func opCorps(members []opMember, managedOnly bool) []int64 {
	seen := map[int64]bool{}
	var out []int64
	for _, m := range members {
		if (managedOnly && !m.Manager) || seen[m.CorporationID] {
			continue
		}
		seen[m.CorporationID] = true
		out = append(out, m.CorporationID)
	}
	return out
}

func opInCorp(members []opMember, corpID int64) []opMember {
	var out []opMember
	for _, m := range members {
		if m.CorporationID == corpID {
			out = append(out, m)
		}
	}
	return out
}

func opManages(members []opMember, corpID int64) bool {
	for _, m := range members {
		if m.CorporationID == corpID && m.Manager {
			return true
		}
	}
	return false
}

// opEnd is when an op is over.
func opEnd(op db.Op) time.Time {
	return op.StartsAt.Add(time.Duration(op.DurationMinutes) * time.Minute)
}

// ---------------------------------------------------------------------------
// Creating and changing an op.
// ---------------------------------------------------------------------------

type opChoice struct {
	ID       int64
	Name     string
	Selected bool
}

// opFormView is the create/edit form.
type opFormView struct {
	ID          int64 // 0 when creating
	Title       string
	Description string
	StartsAt    string // opDateTimeLayout
	Duration    int64
	Doctrine    string
	FormUp      string
	Corps       []opChoice
	FCs         []opChoice
	Errors      []string
}

// opFormFor fills the form's choices: the corporations the account
// may manage ops for, and its characters in them as possible FCs.
func (app *Application) opFormFor(ctx context.Context, members []opMember, form *opFormView, corpID, fcID int64) {
	managed := opCorps(members, true)
	if corpID == 0 && len(managed) > 0 {
		corpID = managed[0]
	}
	for _, id := range managed {
		form.Corps = append(form.Corps, opChoice{ID: id, Name: app.corpDisplayName(ctx, id), Selected: id == corpID})
	}
	for _, m := range members {
		if opManages(members, m.CorporationID) {
			form.FCs = append(form.FCs, opChoice{ID: m.CharacterID, Name: m.Name, Selected: m.CharacterID == fcID})
		}
	}
}

func (app *Application) handleOpNew(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := int64(app.sessions.GetInt(ctx, sessionUserID))
	members := app.opMembers(ctx, userID)
	if len(opCorps(members, true)) == 0 {
		app.flash(ctx, "Creating an op needs a character with the "+app.cfg.opsManagerRolesText()+" role in its corporation.")
		http.Redirect(w, r, "/calendar/", http.StatusSeeOther)
		return
	}
	// A day picked on the calendar starts the form there, at 19:00.
	start := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Hour)
	if day, err := time.Parse("2006-01-02", r.URL.Query().Get("date")); err == nil {
		start = day.Add(19 * time.Hour)
	}
	form := &opFormView{StartsAt: start.Format(opDateTimeLayout), Duration: 60}
	app.opFormFor(ctx, members, form, 0, sessionCharID(app.sessions, ctx))
	app.renderOpForm(w, r, form)
}

func (app *Application) handleOpEdit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := int64(app.sessions.GetInt(ctx, sessionUserID))
	members := app.opMembers(ctx, userID)
	op, ok := app.opFromURL(w, r, members)
	if !ok {
		return
	}
	if !opManages(members, op.CorporationID) {
		http.Redirect(w, r, opURL(op.ID), http.StatusSeeOther)
		return
	}
	form := &opFormView{
		ID: op.ID, Title: op.Title, Description: op.Description,
		StartsAt: op.StartsAt.UTC().Format(opDateTimeLayout), Duration: op.DurationMinutes,
		Doctrine: op.Doctrine, FormUp: op.FormUp,
	}
	app.opFormFor(ctx, members, form, op.CorporationID, op.FcCharacterID)
	app.renderOpForm(w, r, form)
}

func (app *Application) renderOpForm(w http.ResponseWriter, r *http.Request, form *opFormView) {
	ctx := r.Context()
	app.render(ctx, w, http.StatusOK, "op_form.html", pageData{
		LoggedIn:      true,
		CharacterName: app.sessions.GetString(ctx, sessionCharacterName),
		SSOConfigured: app.cfg.SSOConfigured(),
		OpForm:        form,
	})
}

// handleOpSave creates an op, or changes one when the form names it.
// Everything is checked again here: the form's choices are a
// convenience, not the rule.
func (app *Application) handleOpSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := int64(app.sessions.GetInt(ctx, sessionUserID))
	if err := r.ParseForm(); err != nil || userID == 0 {
		http.Redirect(w, r, "/calendar/", http.StatusSeeOther)
		return
	}
	members := app.opMembers(ctx, userID)
	id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	corpID, _ := strconv.ParseInt(r.FormValue("corporation"), 10, 64)
	fcID, _ := strconv.ParseInt(r.FormValue("fc"), 10, 64)
	duration, _ := strconv.ParseInt(r.FormValue("duration"), 10, 64)
	form := &opFormView{
		ID:          id,
		Title:       strings.TrimSpace(r.FormValue("title")),
		Description: strings.TrimSpace(r.FormValue("description")),
		StartsAt:    strings.TrimSpace(r.FormValue("starts_at")),
		Duration:    duration,
		Doctrine:    strings.TrimSpace(r.FormValue("doctrine")),
		FormUp:      strings.TrimSpace(r.FormValue("form_up")),
	}

	if id != 0 {
		op, err := app.queries.GetOp(ctx, id)
		if err != nil || !opManages(members, op.CorporationID) {
			http.Redirect(w, r, "/calendar/", http.StatusSeeOther)
			return
		}
		corpID = op.CorporationID // an op stays with its corporation
	}
	if !opManages(members, corpID) {
		form.Errors = append(form.Errors, "Choose a corporation in which one of your characters holds the "+app.cfg.opsManagerRolesText()+" role.")
	}
	fcOK := false
	for _, m := range opInCorp(members, corpID) {
		fcOK = fcOK || m.CharacterID == fcID
	}
	if !fcOK {
		form.Errors = append(form.Errors, "Choose the character that will run the fleet: one of yours in that corporation.")
	}
	if form.Title == "" || len(form.Title) > opTitleMax {
		form.Errors = append(form.Errors, fmt.Sprintf("Give the op a title of up to %d characters.", opTitleMax))
	}
	if len(form.Description) > opTextMax || len(form.Doctrine) > opShortMax || len(form.FormUp) > opShortMax {
		form.Errors = append(form.Errors, fmt.Sprintf("Keep the description under %d characters, and the doctrine and form-up under %d each.", opTextMax, opShortMax))
	}
	if duration < opMinDuration || duration > opMaxDuration {
		form.Errors = append(form.Errors, fmt.Sprintf("The length has to be between %d minutes and %d hours.", opMinDuration, opMaxDuration/60))
	}
	start, err := time.ParseInLocation(opDateTimeLayout, form.StartsAt, time.UTC)
	now := time.Now().UTC()
	switch {
	case err != nil:
		form.Errors = append(form.Errors, "Give the start as a date and a time, in EVE time.")
	case start.After(now.Add(opMaxAhead)) || start.Before(now.Add(-opMaxBehind)):
		form.Errors = append(form.Errors, "The start has to be within the last month or the coming year.")
	}
	if len(form.Errors) > 0 {
		app.opFormFor(ctx, members, form, corpID, fcID)
		app.renderOpForm(w, r, form)
		return
	}

	if id != 0 {
		err = app.queries.UpdateOp(ctx, db.UpdateOpParams{
			ID: id, Title: form.Title, Description: form.Description, StartsAt: start, DurationMinutes: duration,
			Doctrine: form.Doctrine, FormUp: form.FormUp, FcCharacterID: fcID,
		})
	} else {
		id, err = app.queries.CreateOp(ctx, db.CreateOpParams{
			CorporationID: corpID, Title: form.Title, Description: form.Description, StartsAt: start,
			DurationMinutes: duration, Doctrine: form.Doctrine, FormUp: form.FormUp, FcCharacterID: fcID,
			CreatedByCharacter: sessionCharID(app.sessions, ctx), CreatedAt: now,
		})
	}
	if err != nil {
		logging.Errorf("ops: save op %d for corporation %d: %v", id, corpID, err)
		form.Errors = []string{"The op could not be saved; check the server log."}
		app.opFormFor(ctx, members, form, corpID, fcID)
		app.renderOpForm(w, r, form)
		return
	}
	http.Redirect(w, r, opURL(id), http.StatusSeeOther)
}

// ---------------------------------------------------------------------------
// One op: its details, who has signed up, and the account's own answer.
// ---------------------------------------------------------------------------

func opURL(id int64) string { return fmt.Sprintf("/ops/%d", id) }

// opFromURL loads the op the URL names, provided the account has a
// character in its corporation. Anyone else is sent to the calendar,
// the same as for an op that does not exist: an op's existence is not
// shown to outsiders.
func (app *Application) opFromURL(w http.ResponseWriter, r *http.Request, members []opMember) (db.Op, bool) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "opID"), 10, 64)
	op, err := app.queries.GetOp(r.Context(), id)
	if err != nil || len(opInCorp(members, op.CorporationID)) == 0 {
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			logging.Errorf("ops: load op %d: %v", id, err)
		}
		http.Redirect(w, r, "/calendar/", http.StatusSeeOther)
		return db.Op{}, false
	}
	return op, true
}

type opSignupRow struct {
	CharacterID int64
	Name        string
	Ship        string
	Role        string
	Note        string
}

type opRoleCount struct {
	Role  string
	Count int
}

// opView is the /ops/{id} page.
type opView struct {
	ID          int64
	Title       string
	Description string
	Corporation string
	When        string // "Mon Oct 12, 19:00 EVE"
	WhenRaw     string // RFC3339, drives the live countdown
	Length      string
	Doctrine    string
	FormUp      string
	FCID        int64
	FC          string
	Cancelled   bool
	Over        bool
	CanManage   bool

	Yes, Maybe, No []opSignupRow
	RoleCounts     []opRoleCount // among those coming

	// The sign-up form: the account's characters in the corporation,
	// and the answer already given for the one selected.
	Characters []opChoice
	Mine       string
	MyShip     string
	MyRole     string
	MyNote     string
	Roles      []string

	// Attendance: who was there, and how that is known
	// (ops_attendance.go).
	Attendance *opAttendance
}

func (app *Application) handleOp(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := int64(app.sessions.GetInt(ctx, sessionUserID))
	members := app.opMembers(ctx, userID)
	op, ok := app.opFromURL(w, r, members)
	if !ok {
		return
	}
	view := &opView{
		ID: op.ID, Title: op.Title, Description: op.Description,
		Corporation: app.corpDisplayName(ctx, op.CorporationID),
		When:        op.StartsAt.UTC().Format("Mon Jan 2, 15:04") + " EVE",
		WhenRaw:     op.StartsAt.UTC().Format(time.RFC3339),
		Length:      humanDuration(time.Duration(op.DurationMinutes) * time.Minute),
		Doctrine:    op.Doctrine, FormUp: op.FormUp,
		FCID: op.FcCharacterID, FC: app.displayCharacter(ctx, op.FcCharacterID),
		Cancelled: op.CancelledAt.Valid,
		Over:      opEnd(op).Before(time.Now()),
		CanManage: opManages(members, op.CorporationID),
		Roles:     opFleetRoles,
	}

	signups, err := app.queries.ListOpSignups(ctx, op.ID)
	if err != nil {
		logging.Errorf("ops: sign-ups for op %d: %v", op.ID, err)
	}
	mine := opInCorp(members, op.CorporationID)
	// The form starts on the acting character when it is in this
	// corporation, else on the first that is.
	selected := mine[0].CharacterID
	for _, m := range mine {
		if m.CharacterID == sessionCharID(app.sessions, ctx) {
			selected = m.CharacterID
		}
	}
	if want, _ := strconv.ParseInt(r.URL.Query().Get("as"), 10, 64); want != 0 {
		for _, m := range mine {
			if m.CharacterID == want {
				selected = want
			}
		}
	}
	for _, m := range mine {
		view.Characters = append(view.Characters, opChoice{ID: m.CharacterID, Name: m.Name, Selected: m.CharacterID == selected})
	}
	counts := map[string]int{}
	for _, s := range signups {
		row := opSignupRow{CharacterID: s.CharacterID, Name: app.displayCharacter(ctx, s.CharacterID), Ship: s.Ship, Role: s.FleetRole, Note: s.Note}
		switch s.Response {
		case opYes:
			view.Yes = append(view.Yes, row)
			if s.FleetRole != "" {
				counts[s.FleetRole]++
			}
		case opMaybe:
			view.Maybe = append(view.Maybe, row)
		default:
			view.No = append(view.No, row)
		}
		if s.CharacterID == selected {
			view.Mine, view.MyShip, view.MyRole, view.MyNote = s.Response, s.Ship, s.FleetRole, s.Note
		}
	}
	for _, list := range [][]opSignupRow{view.Yes, view.Maybe, view.No} {
		sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	}
	for _, role := range opFleetRoles {
		if counts[role] > 0 {
			view.RoleCounts = append(view.RoleCounts, opRoleCount{Role: role, Count: counts[role]})
		}
	}

	view.Attendance = app.opAttendanceFor(ctx, op, signups, view.CanManage, time.Now())

	app.render(ctx, w, http.StatusOK, "op.html", pageData{
		LoggedIn:      true,
		CharacterName: app.sessions.GetString(ctx, sessionCharacterName),
		SSOConfigured: app.cfg.SSOConfigured(),
		Op:            view,
	})
}

// handleOpSignup records one character's answer. The character has
// to be the account's own and in the op's corporation.
func (app *Application) handleOpSignup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := int64(app.sessions.GetInt(ctx, sessionUserID))
	members := app.opMembers(ctx, userID)
	op, ok := app.opFromURL(w, r, members)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, opURL(op.ID), http.StatusSeeOther)
		return
	}
	characterID, _ := strconv.ParseInt(r.FormValue("character"), 10, 64)
	allowed := false
	for _, m := range opInCorp(members, op.CorporationID) {
		allowed = allowed || m.CharacterID == characterID
	}
	response := r.FormValue("response")
	if response != opYes && response != opMaybe && response != opNo {
		allowed = false
	}
	switch {
	case !allowed:
		app.flash(ctx, "That sign-up could not be recorded.")
	case op.CancelledAt.Valid:
		app.flash(ctx, "This op was cancelled.")
	case opEnd(op).Before(time.Now()):
		app.flash(ctx, "This op is over.")
	default:
		role := r.FormValue("fleet_role")
		known := false
		for _, r := range opFleetRoles {
			known = known || r == role
		}
		if !known {
			role = ""
		}
		ship := clip(strings.TrimSpace(r.FormValue("ship")), opShortMax)
		note := clip(strings.TrimSpace(r.FormValue("note")), opShortMax)
		if response == opNo {
			ship, role = "", "" // not coming: nothing to bring
		}
		if err := app.queries.UpsertOpSignup(ctx, db.UpsertOpSignupParams{
			OpID: op.ID, CharacterID: characterID, UserID: userID, Response: response,
			Ship: ship, FleetRole: role, Note: note, UpdatedAt: time.Now().UTC(),
		}); err != nil {
			logging.Errorf("ops: sign-up of character %d to op %d: %v", characterID, op.ID, err)
			app.flash(ctx, "That sign-up could not be saved; check the server log.")
		}
	}
	http.Redirect(w, r, fmt.Sprintf("%s?as=%d", opURL(op.ID), characterID), http.StatusSeeOther)
}

// handleOpCancel cancels an op, or brings a cancelled one back. A
// cancelled op stays on the calendar, struck through, so that those
// who signed up see what happened to it.
func (app *Application) handleOpCancel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := int64(app.sessions.GetInt(ctx, sessionUserID))
	members := app.opMembers(ctx, userID)
	op, ok := app.opFromURL(w, r, members)
	if !ok {
		return
	}
	if opManages(members, op.CorporationID) {
		cancelled := sql.NullTime{}
		if !op.CancelledAt.Valid {
			cancelled = timeSet(time.Now().UTC())
		}
		if err := app.queries.SetOpCancelled(ctx, db.SetOpCancelledParams{CancelledAt: cancelled, ID: op.ID}); err != nil {
			logging.Errorf("ops: cancel op %d: %v", op.ID, err)
		}
	}
	http.Redirect(w, r, opURL(op.ID), http.StatusSeeOther)
}

// clip shortens s to at most max bytes without splitting a character.
func clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return strings.ToValidUTF8(s[:max], "")
}
