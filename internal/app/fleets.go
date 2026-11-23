package app

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
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
	"github.com/jackc/pgx/v5"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
	"evesynapse/internal/standing"
)

// ---------------------------------------------------------------------------
// Fleets: planned fleets owned by a corporation, alliance or community.
// The planner chooses who can see one and who can sign up, and a share
// link carries those rules. What a viewer may do is decided in
// fleets_access.go. All times are EVE time (UTC).
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

// Limits on what a fleet's form accepts.
const (
	opTitleMax       = 120
	opTextMax        = 2000
	opShortMax       = 120
	opMinDuration    = 15
	opMaxDuration    = 24 * 60
	opMaxAhead       = 366 * 24 * time.Hour
	opMaxBehind      = 30 * 24 * time.Hour
	opDateTimeLayout = "2006-01-02T15:04" // what <input type="datetime-local"> sends

	// opRosterMax is the most sign-ups a fleet's page lists.
	opRosterMax = 300
	fleetsPath  = "/fleets/"
	planPath    = "/fleets/plan"
)

// opEnd is when a fleet is over.
func opEnd(op db.Op) time.Time {
	return op.StartsAt.Add(time.Duration(op.DurationMinutes) * time.Minute)
}

func opURL(id int64) string { return module.OpURL(id) }

func fleetSharePath(token string) string { return "/f/" + url.PathEscape(token) + "/" }

// validFleetShare reports whether a token is the fleet's current link.
func validFleetShare(op db.Op, token string) bool {
	return token != "" && op.ShareToken.Valid &&
		subtle.ConstantTimeCompare([]byte(token), []byte(op.ShareToken.String)) == 1
}

// fleetOption is one answer in the planner's choices.
type fleetOption struct {
	Value, Label, Hint string
	Selected           bool
}

var fleetTypeOptions = []fleetOption{
	{Value: fleetTypeNPSI, Label: "NPSI", Hint: "Open to anyone who wants to fly"},
	{Value: fleetTypeCorporation, Label: "Corporation", Hint: "A corporation's own fleet"},
	{Value: fleetTypeAlliance, Label: "Alliance", Hint: "An alliance's own fleet"},
	{Value: fleetTypeCommunity, Label: "Community", Hint: "A community's own fleet"},
	{Value: fleetTypeCustom, Label: "Other", Hint: "Anything else"},
}

var fleetVisibilityOptions = []fleetOption{
	{Value: fleetPublic, Label: "Public", Hint: "Listed on the Fleets page. Anyone can open it."},
	{Value: fleetUnlisted, Label: "Link only", Hint: "Anyone with the share link can open it."},
	{Value: fleetRestricted, Label: "Private", Hint: "Only the people you choose can open it, even with the link."},
}

var fleetJoinOptions = []fleetOption{
	{Value: joinAnyone, Label: "Anyone", Hint: "Everyone who can open the fleet sees where to form up. Going on the roster takes an EVE sign-in."},
	{Value: joinVerified, Label: "Any EVE pilot", Hint: "Pilots sign in with EVE to see where to form up and sign up. No account needed."},
	{Value: joinAccount, Label: "EveSynapse accounts", Hint: "Only pilots with an account here can sign up."},
	{Value: joinByRule, Label: "Only who you choose", Hint: "Only the people you pick below can sign up."},
}

// What a fleet flies (ops.doctrine_mode). Stored: never rename.
const (
	doctrineNone      = "none"
	doctrineSuggested = "suggested"
	doctrineRequired  = "required"
)

var fleetDoctrineModes = []fleetOption{
	{Value: doctrineNone, Label: "Bring what you like", Hint: "No doctrine for this fleet."},
	{Value: doctrineSuggested, Label: "Suggested", Hint: "Pilots see the doctrine and can bring something else."},
	{Value: doctrineRequired, Label: "Required", Hint: "Pilots are told to bring one of its fits."},
}

func fleetOptions(list []fleetOption, selected string) []fleetOption {
	out := make([]fleetOption, len(list))
	copy(out, list)
	for i := range out {
		out[i].Selected = out[i].Value == selected
	}
	return out
}

func fleetOptionLabel(list []fleetOption, value string) string {
	for _, o := range list {
		if o.Value == value {
			return o.Label
		}
	}
	return ""
}

func validFleetOption(list []fleetOption, value string) bool {
	return fleetOptionLabel(list, value) != ""
}

// fleetKindText names a fleet's kind in a sentence.
func fleetKindText(fleetType string) string {
	if fleetType == fleetTypeCustom {
		return "Fleet"
	}
	return fleetOptionLabel(fleetTypeOptions, fleetType) + " fleet"
}

// fleetJoinSummary says who can sign up, as a viewer reads it.
func fleetJoinSummary(op db.Op, rules []db.OpAccessRule) string {
	if op.JoinGate != joinByRule {
		return fleetOptionLabel(fleetJoinOptions, op.JoinGate)
	}
	for _, rule := range rules {
		if rule.Action == accessJoin {
			return "Chosen pilots"
		}
	}
	return "Members of the " + op.OwnerKind
}

// defaultFleetType is the type a new fleet of an owner starts on.
func defaultFleetType(owner orgOwner) string {
	switch owner.Kind {
	case ownerAlliance:
		return fleetTypeAlliance
	case ownerCommunity:
		return fleetTypeCommunity
	}
	return fleetTypeCorporation
}

// ---------------------------------------------------------------------------
// Planning and changing a fleet.
// ---------------------------------------------------------------------------

type opChoice = module.NamedChoice

// fleetOwnerChoice is an owner the planner can pick.
type fleetOwnerChoice struct {
	Key, Name string
	Selected  bool
}

// opFormView is the planner.
type opFormView struct {
	ID          int64 // 0 when creating
	Title       string
	Description string
	StartsAt    string // opDateTimeLayout
	Date        string // the start's day, kept when switching owner
	Duration    int64
	Doctrine    string
	DoctrineID  int64 // one of the owner's doctrines, or 0
	Doctrines   []opDoctrineGroup
	// DoctrineMode is none, suggested or required. DoctrineLocked: the
	// fleet is over, so what it flew no longer changes.
	DoctrineMode   string
	DoctrineModes  []fleetOption
	DoctrineLocked bool
	DoctrinesHref  string
	FormUp         string
	Owners         []fleetOwnerChoice
	OwnerKey       string
	OwnerName      string
	FCID           int64
	FCName         string
	FCs            []opChoice
	Errors         []string

	// CanSetAccess: the viewer may change who can see and join the
	// fleet. A commander without that sees the details form only.
	CanSetAccess bool
	Type         string
	Visibility   string
	JoinGate     string
	ViewRule     string
	JoinRule     string
	RosterPublic bool
	Types        []fleetOption
	Visibilities []fleetOption
	Gates        []fleetOption
	RuleChoices  []whoChoice

	// SRPPolicyID is the replacement policy the fleet names; 0 is the
	// owner's default. SRPPolicySet: the form sent a choice.
	SRPPolicyID  int64
	SRPPolicySet bool
	SRPPolicies  []opChoice
	SRPHref      string
}

// opDoctrineGroup is one owner's doctrines, as the form offers them.
type opDoctrineGroup struct {
	Owner   string
	Choices []opChoice
}

// opFormFor fills the form's choices for one owner.
func (app *Application) opFormFor(ctx context.Context, v fleetViewer, form *opFormView, owner orgOwner, fcID int64) {
	form.OwnerKey, form.OwnerName = owner.Key(), app.ownerName(ctx, owner)
	if len(form.StartsAt) >= len("2006-01-02") {
		form.Date = form.StartsAt[:len("2006-01-02")]
	}
	form.Owners, form.FCs, form.Doctrines = nil, nil, nil
	for _, o := range v.planOwners() {
		form.Owners = append(form.Owners, fleetOwnerChoice{Key: o.Key(), Name: fleetOwnerLabel(o, app.ownerName(ctx, o)), Selected: o == owner})
	}
	listed := false
	for _, c := range v.Characters {
		if c.Standing.Belongs(owner) {
			listed = listed || c.CharacterID == fcID
			form.FCs = append(form.FCs, opChoice{ID: c.CharacterID, Name: c.Name, Selected: c.CharacterID == fcID})
		}
	}
	// Another manager's commander stays selectable, so saving a change
	// never swaps the FC by accident.
	if form.ID != 0 && fcID != 0 && !listed {
		form.FCs = append(form.FCs, opChoice{ID: fcID, Name: app.displayCharacter(ctx, fcID), Selected: true})
	}
	form.FCID = fcID
	if fcID != 0 {
		form.FCName = app.displayCharacter(ctx, fcID)
	}
	doctrines, err := app.queries.ListDoctrinesForOwner(ctx, db.ListDoctrinesForOwnerParams{OwnerKind: owner.Kind, OwnerID: owner.ID})
	if err != nil {
		logging.Errorf("fleets: doctrines of %s: %v", owner.Key(), err)
	}
	group := opDoctrineGroup{Owner: form.OwnerName}
	for _, d := range doctrines {
		group.Choices = append(group.Choices, opChoice{ID: d.ID, Name: d.Name, Selected: d.ID == form.DoctrineID})
	}
	if len(group.Choices) > 0 {
		form.Doctrines = []opDoctrineGroup{group}
	}
	if moduleCompiledIn("doctrines") {
		form.DoctrinesHref = standing.Address("/doctrines/", owner.Key())
	}
	form.DoctrineModes = fleetOptions(fleetDoctrineModes, form.DoctrineMode)
	form.Types = fleetOptions(fleetTypeOptions, form.Type)
	form.Visibilities = fleetOptions(fleetVisibilityOptions, form.Visibility)
	form.Gates = fleetOptions(fleetJoinOptions, form.JoinGate)
	if form.CanSetAccess {
		form.RuleChoices = app.fleetRuleChoices(ctx, v, owner, form.ViewRule, form.JoinRule)
	}
	form.SRPPolicies, form.SRPHref = nil, ""
	if form.CanSetAccess && moduleCompiledIn("srp") {
		form.SRPHref = standing.Address("/srp/", owner.Key()) + "#srp-policy"
		policies, err := app.queries.ListSRPPolicies(ctx, db.ListSRPPoliciesParams{OwnerKind: owner.Kind, OwnerID: owner.ID})
		if err != nil {
			logging.Errorf("fleets: replacement policies of %s: %v", owner.Key(), err)
		}
		for _, p := range policies {
			form.SRPPolicies = append(form.SRPPolicies, opChoice{ID: p.ID, Name: p.Name, Selected: p.ID == form.SRPPolicyID})
		}
	}
}

// fleetOwnerLabel names an owner with its kind, so a corporation and a
// community with one name can be told apart in a list.
func fleetOwnerLabel(owner orgOwner, name string) string {
	switch owner.Kind {
	case ownerAlliance:
		return name + " (alliance)"
	case ownerCommunity:
		return name + " (community)"
	}
	return name + " (corporation)"
}

func (app *Application) handleOpNew(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	v := app.fleetViewerFor(ctx)
	if v.UserID == 0 {
		app.flash(ctx, "Planning a fleet needs an EveSynapse account.")
		http.Redirect(w, r, fleetsPath, http.StatusSeeOther)
		return
	}
	owners := v.planOwners()
	if len(owners) == 0 {
		app.flash(ctx, "Planning a fleet needs a character who runs fleets for a corporation, alliance or community. In a corporation that takes the "+app.cfg.opsManagerRolesText()+" role.")
		http.Redirect(w, r, fleetsPath, http.StatusSeeOther)
		return
	}
	owner := owners[0]
	if want, ok := parseAnyOwner(r.URL.Query().Get("owner")); ok && standing.Includes(owners, want) {
		owner = want
	}
	// A day picked on the calendar starts the form there, at 19:00.
	start := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Hour)
	if day, err := time.Parse("2006-01-02", r.URL.Query().Get("date")); err == nil {
		start = day.Add(19 * time.Hour)
	}
	form := &opFormView{
		StartsAt: start.Format(opDateTimeLayout), Duration: 60, CanSetAccess: true,
		Type: defaultFleetType(owner), Visibility: fleetRestricted, JoinGate: joinByRule,
		ViewRule: ruleMember, JoinRule: ruleMember, DoctrineMode: doctrineNone,
	}
	app.opFormFor(ctx, v, form, owner, sessionCharID(app.sessions, ctx))
	app.renderOpForm(w, r, form)
}

func (app *Application) handleOpEdit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	op, v, _, a, ok := app.fleetFromURL(w, r)
	if !ok {
		return
	}
	if !a.Manage {
		http.Redirect(w, r, opURL(op.ID), http.StatusSeeOther)
		return
	}
	form := &opFormView{
		ID: op.ID, Title: op.Title, Description: op.Description,
		StartsAt: op.StartsAt.UTC().Format(opDateTimeLayout), Duration: op.DurationMinutes,
		Doctrine: op.Doctrine, DoctrineID: op.DoctrineID, DoctrineMode: op.DoctrineMode,
		DoctrineLocked: opEnd(op).Before(time.Now()), FormUp: op.FormUp,
		CanSetAccess: a.Plan, Type: op.FleetType, Visibility: op.Visibility, JoinGate: op.JoinGate,
		RosterPublic: op.RosterPublic, ViewRule: ruleMember, JoinRule: ruleMember,
		SRPPolicyID: op.SrpPolicyID.Int64,
	}
	rules, err := app.queries.ListOpAccessRules(ctx, op.ID)
	if err != nil {
		logging.Errorf("fleets: access rules of %d: %v", op.ID, err)
	}
	for _, rule := range rules {
		switch rule.Action {
		case accessView:
			form.ViewRule = ruleValue(rule.RuleKind, rule.RuleRef)
		case accessJoin:
			form.JoinRule = ruleValue(rule.RuleKind, rule.RuleRef)
		}
	}
	app.opFormFor(ctx, v, form, opOwner(op), op.FcCharacterID)
	app.renderOpForm(w, r, form)
}

func (app *Application) renderOpForm(w http.ResponseWriter, r *http.Request, form *opFormView) {
	ctx := r.Context()
	app.render(ctx, w, http.StatusOK, "fleet_form.html", pageData{ShellData: app.PublicShell(ctx), OpForm: form})
}

// handleOpSave creates a fleet, or changes one when the form names it.
// Everything is checked again here: the form's choices are a
// convenience, not the rule.
func (app *Application) handleOpSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	v := app.fleetViewerFor(ctx)
	if err := r.ParseForm(); err != nil || v.UserID == 0 {
		http.Redirect(w, r, fleetsPath, http.StatusSeeOther)
		return
	}
	id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	fcID, _ := strconv.ParseInt(r.FormValue("fc"), 10, 64)
	duration, _ := strconv.ParseInt(r.FormValue("duration"), 10, 64)
	form := &opFormView{
		ID:           id,
		Title:        strings.TrimSpace(r.FormValue("title")),
		Description:  strings.TrimSpace(r.FormValue("description")),
		StartsAt:     strings.TrimSpace(r.FormValue("starts_at")),
		Duration:     duration,
		Doctrine:     strings.TrimSpace(r.FormValue("doctrine")),
		FormUp:       strings.TrimSpace(r.FormValue("form_up")),
		Type:         r.FormValue("fleet_type"),
		Visibility:   r.FormValue("visibility"),
		JoinGate:     r.FormValue("join_gate"),
		ViewRule:     r.FormValue("view_rule"),
		JoinRule:     r.FormValue("join_rule"),
		RosterPublic: r.FormValue("roster_public") == "1",
		CanSetAccess: true,
	}
	form.DoctrineID, _ = strconv.ParseInt(r.FormValue("doctrine_id"), 10, 64)
	form.DoctrineMode = r.FormValue("doctrine_mode")
	form.SRPPolicySet = r.PostForm.Has("srp_policy")
	form.SRPPolicyID, _ = strconv.ParseInt(r.PostForm.Get("srp_policy"), 10, 64)

	var owner orgOwner
	var existing db.Op
	if id != 0 {
		op, err := app.queries.GetOp(ctx, id)
		if err != nil {
			http.Redirect(w, r, fleetsPath, http.StatusSeeOther)
			return
		}
		a := v.access(op, app.fleetFactsFor(ctx, v, []db.Op{op})[op.ID])
		if !a.Manage {
			http.Redirect(w, r, fleetsPath, http.StatusSeeOther)
			return
		}
		existing, owner = op, opOwner(op) // a fleet stays with its owner
		form.CanSetAccess = a.Plan
		if !a.Plan {
			// A commander changes the details only.
			fcID = op.FcCharacterID
			form.Type, form.Visibility, form.JoinGate, form.RosterPublic = op.FleetType, op.Visibility, op.JoinGate, op.RosterPublic
			form.SRPPolicyID, form.SRPPolicySet = op.SrpPolicyID.Int64, false
		}
	} else {
		owners := v.planOwners()
		if len(owners) == 0 {
			http.Redirect(w, r, fleetsPath, http.StatusSeeOther)
			return
		}
		want, ok := parseAnyOwner(r.FormValue("owner"))
		if !ok || !standing.Includes(owners, want) {
			form.Errors = append(form.Errors, "Choose an owner you can plan fleets for.")
			want = owners[0]
		}
		owner = want
	}

	fcOK := id != 0 && fcID == existing.FcCharacterID
	for _, c := range v.Characters {
		fcOK = fcOK || (c.CharacterID == fcID && c.Standing.Belongs(owner))
	}
	if !fcOK {
		form.Errors = append(form.Errors, "Choose the character that will run the fleet: one of yours in "+app.ownerName(ctx, owner)+".")
	}
	if id != 0 && opEnd(existing).Before(time.Now()) {
		// What a finished fleet flew stays as it was.
		form.Doctrine, form.DoctrineID, form.DoctrineMode, form.DoctrineLocked = existing.Doctrine, existing.DoctrineID, existing.DoctrineMode, true
	} else {
		app.checkFleetDoctrine(ctx, form, owner)
	}
	if form.Title == "" || len(form.Title) > opTitleMax {
		form.Errors = append(form.Errors, fmt.Sprintf("Give the fleet a title of up to %d characters.", opTitleMax))
	}
	if len(form.Description) > opTextMax || len(form.Doctrine) > opShortMax || len(form.FormUp) > opShortMax {
		form.Errors = append(form.Errors, fmt.Sprintf("Keep the details under %d characters, and the doctrine and form-up under %d each.", opTextMax, opShortMax))
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

	var rules []db.InsertOpAccessRuleParams
	if form.CanSetAccess {
		if !validFleetOption(fleetTypeOptions, form.Type) {
			form.Type = defaultFleetType(owner)
		}
		// A choice left out is the narrowest one.
		if form.Visibility == "" {
			form.Visibility = fleetRestricted
		}
		if form.JoinGate == "" {
			form.JoinGate = joinByRule
		}
		if !validFleetOption(fleetVisibilityOptions, form.Visibility) {
			form.Visibility = fleetRestricted
			form.Errors = append(form.Errors, "Choose who can see the fleet.")
		}
		if !validFleetOption(fleetJoinOptions, form.JoinGate) {
			form.JoinGate = joinByRule
			form.Errors = append(form.Errors, "Choose who can sign up.")
		}
		var keep []string
		if id != 0 {
			stored, err := app.queries.ListOpAccessRules(ctx, id)
			if err != nil {
				logging.Errorf("fleets: access rules of %d: %v", id, err)
			}
			for _, rule := range stored {
				keep = append(keep, ruleValue(rule.RuleKind, rule.RuleRef))
			}
		}
		choices := app.fleetRuleChoices(ctx, v, owner, keep...)
		addRule := func(action, value string) {
			if value == "" {
				value = ruleMember
			}
			if !hasWhoChoice(choices, value) {
				form.Errors = append(form.Errors, "Choose who the fleet is for from the list.")
				return
			}
			if value == ruleMember {
				return // no rule means the owner's members
			}
			kind, ref := splitRuleValue(value)
			rules = append(rules, db.InsertOpAccessRuleParams{Action: action, RuleKind: kind, RuleRef: ref})
		}
		if form.Visibility == fleetRestricted {
			addRule(accessView, form.ViewRule)
		}
		if form.JoinGate == joinByRule {
			addRule(accessJoin, form.JoinRule)
		}
		if form.SRPPolicySet && form.SRPPolicyID != 0 {
			p, err := app.queries.GetSRPPolicy(ctx, form.SRPPolicyID)
			if err != nil || p.OwnerKind != owner.Kind || p.OwnerID != owner.ID {
				form.SRPPolicyID = 0
				form.Errors = append(form.Errors, "Choose one of "+app.ownerName(ctx, owner)+"'s replacement policies.")
			}
		}
	}
	if len(form.Errors) > 0 {
		app.opFormFor(ctx, v, form, owner, fcID)
		app.renderOpForm(w, r, form)
		return
	}
	saved, err := app.saveFleet(ctx, form, owner, start, fcID, rules, now)
	if err != nil {
		logging.Errorf("fleets: save fleet %d for %s: %v", id, owner.Key(), err)
		form.Errors = []string{"The fleet could not be saved. Try again in a moment."}
		app.opFormFor(ctx, v, form, owner, fcID)
		app.renderOpForm(w, r, form)
		return
	}
	http.Redirect(w, r, opURL(saved), http.StatusSeeOther)
}

// checkFleetDoctrine settles what the form says the fleet flies. A
// doctrine picked from the list has to be the fleet owner's and gives
// the fleet its name. Only a doctrine from the list can be required,
// since only that has fits to bring.
func (app *Application) checkFleetDoctrine(ctx context.Context, form *opFormView, owner orgOwner) {
	if form.DoctrineMode == "" {
		form.DoctrineMode = doctrineNone
		if form.DoctrineID != 0 || form.Doctrine != "" {
			form.DoctrineMode = doctrineSuggested
		}
	}
	if !validFleetOption(fleetDoctrineModes, form.DoctrineMode) {
		form.DoctrineMode = doctrineNone
		form.Errors = append(form.Errors, "Choose what the fleet flies.")
	}
	if form.DoctrineMode == doctrineNone {
		form.Doctrine, form.DoctrineID = "", 0
		return
	}
	if form.DoctrineID != 0 {
		d, err := app.queries.GetDoctrine(ctx, form.DoctrineID)
		if err == nil && doctrineOwner(d) == owner {
			form.Doctrine = d.Name
			return
		}
		form.DoctrineID = 0
		form.Errors = append(form.Errors, "Choose one of "+app.ownerName(ctx, owner)+"'s doctrines, or write the doctrine in.")
		return
	}
	switch {
	case form.DoctrineMode == doctrineRequired:
		form.Errors = append(form.Errors, "A required doctrine has to be one from the list.")
	case form.Doctrine == "":
		form.Errors = append(form.Errors, "Name the doctrine, or choose to let pilots bring what they like.")
	}
}

// saveFleet writes a fleet and its access rules together.
func (app *Application) saveFleet(ctx context.Context, form *opFormView, owner orgOwner, start time.Time, fcID int64, rules []db.InsertOpAccessRuleParams, now time.Time) (int64, error) {
	tx, err := app.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	q := app.queries.WithTx(tx)
	id := form.ID
	if id == 0 {
		token, err := newShareToken()
		if err != nil {
			return 0, err
		}
		corp := int64(0)
		if owner.Kind == ownerCorporation {
			corp = owner.ID
		}
		id, err = q.CreateFleet(ctx, db.CreateFleetParams{
			CorporationID: corp, OwnerKind: owner.Kind, OwnerID: owner.ID, FleetType: form.Type,
			Visibility: form.Visibility, JoinGate: form.JoinGate, RosterPublic: form.RosterPublic,
			ShareToken: sql.NullString{String: token, Valid: true}, Title: form.Title, Description: form.Description,
			StartsAt: start, DurationMinutes: form.Duration, Doctrine: form.Doctrine, DoctrineID: form.DoctrineID, DoctrineMode: form.DoctrineMode,
			FormUp: form.FormUp, FcCharacterID: fcID, CreatedByCharacter: sessionCharID(app.sessions, ctx), CreatedAt: now,
		})
		if err != nil {
			return 0, err
		}
	} else {
		if err := q.UpdateOp(ctx, db.UpdateOpParams{
			ID: id, Title: form.Title, Description: form.Description, StartsAt: start, DurationMinutes: form.Duration,
			Doctrine: form.Doctrine, DoctrineID: form.DoctrineID, DoctrineMode: form.DoctrineMode, FormUp: form.FormUp, FcCharacterID: fcID,
		}); err != nil {
			return 0, err
		}
		if form.CanSetAccess {
			if err := q.UpdateFleetAccess(ctx, db.UpdateFleetAccessParams{
				ID: id, FleetType: form.Type, Visibility: form.Visibility, JoinGate: form.JoinGate, RosterPublic: form.RosterPublic,
			}); err != nil {
				return 0, err
			}
		}
		// A new time moves the window its logs and mails are read from.
		if err := markFleetReportChanged(ctx, q, id); err != nil {
			return 0, err
		}
	}
	if form.CanSetAccess {
		if err := q.DeleteOpAccessRules(ctx, id); err != nil {
			return 0, err
		}
		for _, rule := range rules {
			rule.OpID = id
			if err := q.InsertOpAccessRule(ctx, rule); err != nil {
				return 0, err
			}
		}
		if form.SRPPolicySet {
			if err := q.SetOpSRPPolicy(ctx, db.SetOpSRPPolicyParams{
				ID: id, SrpPolicyID: sql.NullInt64{Int64: form.SRPPolicyID, Valid: form.SRPPolicyID != 0},
			}); err != nil {
				return 0, err
			}
		}
	}
	return id, tx.Commit(ctx)
}

// ---------------------------------------------------------------------------
// One fleet: its details, who has signed up, and the viewer's answer.
// ---------------------------------------------------------------------------

// fleetFromURL loads the fleet the URL names, for a viewer who may see
// it. Anyone else gets the same page as for a fleet that does not
// exist: a private fleet's existence is not shown to outsiders.
func (app *Application) fleetFromURL(w http.ResponseWriter, r *http.Request) (db.Op, fleetViewer, fleetFacts, fleetAccess, bool) {
	ctx := r.Context()
	v := app.fleetViewerFor(ctx)
	id, _ := strconv.ParseInt(chi.URLParam(r, "opID"), 10, 64)
	op, err := app.queries.GetOp(ctx, id)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			logging.Errorf("fleets: load fleet %d: %v", id, err)
		}
		app.renderFleetHidden(w, r, v)
		return db.Op{}, v, fleetFacts{}, fleetAccess{}, false
	}
	facts := app.fleetFactsFor(ctx, v, []db.Op{op})[op.ID]
	a := v.access(op, facts)
	if !a.View {
		if !app.redirectIntoLiveFleet(w, r, op, v) {
			app.renderFleetHidden(w, r, v)
		}
		return db.Op{}, v, fleetFacts{}, fleetAccess{}, false
	}
	return op, v, facts, a, true
}

// fleetHiddenView is the answer for a fleet the viewer cannot see. It
// is the same for one that does not exist.
type fleetHiddenView struct {
	Level  string
	SignIn string
}

func (app *Application) renderFleetHidden(w http.ResponseWriter, r *http.Request, v fleetViewer) {
	ctx := r.Context()
	w.Header().Set("X-Robots-Tag", "noindex")
	view := &fleetHiddenView{Level: v.level(), SignIn: "/auth/pilot?next=" + url.QueryEscape(r.URL.Path)}
	app.render(ctx, w, http.StatusNotFound, "fleet_hidden.html", pageData{ShellData: app.PublicShell(ctx), FleetHidden: view})
}

type opSignupRow struct {
	CharacterID int64
	Name        string
	Ship        string
	Role        string
	Note        string
	// CanCommand: a planner can make this pilot a co-FC.
	CanCommand bool
}

type opRoleCount struct {
	Role  string
	Count int
}

// opRosterGroup is one answer's pilots on the fleet's page.
type opRosterGroup struct {
	Title string
	Rows  []opSignupRow
}

// opCommanderRow is a co-FC on the fleet's page.
type opCommanderRow struct {
	CharacterID int64
	Name        string
}

// opView is a fleet's page, at what this viewer may see.
type opView struct {
	ID          int64
	Title       string
	Description string
	Owner       string
	Kind        string // "NPSI fleet", or plain "Fleet" for any other kind
	Visibility  string
	JoinGate    string
	When        string // "Mon Oct 12, 19:00 EVE"
	Day         string
	Clock       string
	WhenRaw     string // RFC3339, drives the live countdown
	Length      string
	Doctrine    string
	DoctrineURL string     // the doctrine's page, for those who can read it
	DoctrineFit []fleetFit // its fits: what to bring
	// DoctrineRequired: pilots are told to bring one of its fits.
	DoctrineRequired bool
	FormUp           string
	FCID             int64
	FC               string
	Cancelled        bool
	Over             bool
	Live             bool
	CanManage        bool
	CanPlan          bool
	SeeNames         bool
	SeeFormUp        bool
	CanJoin          bool
	HasSRP           bool

	// Level is "anonymous", "pilot" or "account"; the sign-up panel
	// offers that viewer's next step.
	Level     string
	PilotName string
	SignIn    string
	Account   string
	// Share is the token a sign-up from a share link carries back.
	Share string

	ComingCount    int32
	MaybeCount     int32
	Yes, Maybe, No []opSignupRow
	Roster         []opRosterGroup
	RosterCut      bool
	RoleCounts     []opRoleCount // among those coming
	Commanders     []opCommanderRow

	// The sign-up form: the viewer's characters that may sign up, and
	// the answer already given for the one selected.
	Characters []opChoice
	Mine       string
	MyShip     string
	MyRole     string
	MyNote     string
	Roles      []string

	// Share link and calendar file.
	ShareURL    string
	ShareNote   string
	CalendarURL string

	// Attendance: who was there, and how that is known.
	Attendance *opAttendance
	// Totals: the fleet's stored kills, losses and ISK, nil until built.
	Totals *fleetTotalsView
}

func (app *Application) handleOp(w http.ResponseWriter, r *http.Request) {
	op, v, facts, a, ok := app.fleetFromURL(w, r)
	if !ok {
		return
	}
	app.renderFleet(w, r, op, v, facts, a)
}

// handleFleetShare is the landing page of a share link (/f/{token}/).
// It answers exactly as for a fleet that does not exist unless the
// fleet is one this link may reveal to this viewer.
func (app *Application) handleFleetShare(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	w.Header().Set("X-Robots-Tag", "noindex")
	v := app.fleetViewerFor(ctx)
	token := chi.URLParam(r, "token")
	op, err := app.queries.GetOpByShareToken(ctx, sql.NullString{String: token, Valid: token != ""})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			logging.Errorf("fleets: share link: %v", err)
		}
		app.renderFleetHidden(w, r, v)
		return
	}
	facts := app.fleetFactsFor(ctx, v, []db.Op{op})[op.ID]
	facts.ViaToken = true
	a := v.access(op, facts)
	if !a.View {
		if !app.redirectIntoLiveFleet(w, r, op, v) {
			app.renderFleetHidden(w, r, v)
		}
		return
	}
	app.renderFleet(w, r, op, v, facts, a)
}

func (app *Application) renderFleet(w http.ResponseWriter, r *http.Request, op db.Op, v fleetViewer, facts fleetFacts, a fleetAccess) {
	ctx := r.Context()
	now := time.Now()
	owner := opOwner(op)
	path := opURL(op.ID)
	if facts.ViaToken {
		path = fleetSharePath(op.ShareToken.String)
	}
	if op.Visibility != fleetPublic {
		w.Header().Set("X-Robots-Tag", "noindex")
	}
	start := op.StartsAt.UTC()
	view := &opView{
		ID: op.ID, Title: op.Title, Description: op.Description, Owner: app.ownerName(ctx, owner),
		Kind:       fleetKindText(op.FleetType),
		Visibility: fleetOptionLabel(fleetVisibilityOptions, op.Visibility),
		JoinGate:   fleetJoinSummary(op, facts.Rules),
		When:       start.Format("Mon Jan 2, 15:04") + " EVE",
		Day:        start.Format("Mon Jan 2"), Clock: start.Format("15:04"),
		WhenRaw:   start.Format(time.RFC3339),
		Length:    humanDuration(time.Duration(op.DurationMinutes) * time.Minute),
		Doctrine:  op.Doctrine,
		Cancelled: op.CancelledAt.Valid,
		Over:      opEnd(op).Before(now),
		Live:      !op.CancelledAt.Valid && !op.StartsAt.After(now) && !opEnd(op).Before(now),
		CanManage: a.Manage, CanPlan: a.Plan, SeeNames: a.SeeNames, SeeFormUp: a.SeeFormUp, CanJoin: a.Join,
		Level: v.level(), SignIn: "/auth/pilot?next=" + url.QueryEscape(path), Account: "/auth/link?from=cards",
		ComingCount: op.ComingCount, MaybeCount: op.MaybeCount, Roles: opFleetRoles,
	}
	if v.Pilot != nil {
		view.PilotName = v.Pilot.Name
	}
	if facts.ViaToken {
		view.Share = op.ShareToken.String
	}
	if a.SeeFormUp {
		view.FormUp = op.FormUp
	}
	if a.SeeNames && op.FcCharacterID != 0 {
		view.FCID, view.FC = op.FcCharacterID, app.displayCharacter(ctx, op.FcCharacterID)
	}
	_, _, _, view.HasSRP = app.seesFleetSRP(ctx, op, v)
	view.DoctrineRequired = op.DoctrineMode == doctrineRequired
	fits, doctrine := app.fleetFits(ctx, op)
	readable := doctrine != nil && v.readsDoctrine(owner, doctrine.Visibility)
	if readable {
		view.DoctrineURL = doctrineURL(doctrine.ID)
	}
	if a.Join || a.Manage || (doctrine != nil && doctrine.Visibility == module.DoctrinePublic) {
		if readable {
			// A fit taken out of the doctrine since the start has
			// nothing left to open.
			open := map[int64]bool{}
			for _, fit := range app.doctrineFitViews(ctx, []int64{doctrine.ID})[doctrine.ID] {
				open[fit.ID] = true
			}
			for i := range fits {
				if open[fits[i].id] {
					fits[i].Href = "/fittings/?doctrine=" + strconv.FormatInt(fits[i].id, 10) + "#fit-editor"
				}
			}
		}
		view.DoctrineFit = fits
	}

	var signups []db.OpSignup
	if a.SeeNames {
		var err error
		if signups, err = app.queries.ListOpSignups(ctx, op.ID); err != nil {
			logging.Errorf("fleets: sign-ups for %d: %v", op.ID, err)
		}
		view.RosterCut = len(signups) >= opRosterMax
	}
	app.fillFleetSignup(ctx, r, view, op, a)
	app.fillFleetRoster(ctx, view, op, signups, a)

	if op.ShareToken.Valid && op.Visibility != fleetRestricted {
		view.CalendarURL = fleetSharePath(op.ShareToken.String) + "fleet.ics"
	}
	if a.Plan && op.ShareToken.Valid {
		view.ShareURL = app.SiteURL(fleetSharePath(op.ShareToken.String))
		view.ShareNote = "Anyone with this link can open the fleet."
		if op.Visibility == fleetRestricted {
			view.ShareNote = "This fleet is private: the link only works for people who can already see it."
		}
	}
	live, err := app.fleetLiveReadFor(ctx, op, v, now)
	if err != nil {
		logging.Errorf("fleets: composition of %d: %v", op.ID, err)
	}
	if ids := v.characterIDs(); len(ids) > 0 {
		// How the capture is going is for those running the fleet; a
		// pilot who flew in it sees attendance once there is some.
		if att := app.opAttendanceFor(ctx, op, signups, a.Manage, ids, live.sight, now); a.Manage || len(att.Rows) > 0 {
			view.Attendance = att
		}
	}

	view.Totals = app.fleetTotalsFor(ctx, op.ID)
	data := pageData{ShellData: app.PublicShell(ctx), Op: view}
	// The tab opens by the fleet's address, where a share link does not
	// count, so it is offered only to those it would let in.
	direct := facts
	direct.ViaToken = false
	data.SectionSubTabs = fleetTabs(op.ID, true, seesFleetLive(op, v.access(op, direct), live.sight, now), view.HasSRP, app.fleetReportTab(ctx, op, v, now), "overview")
	if op.Visibility == fleetPublic || facts.ViaToken {
		data.HeadMeta = module.RenderPageMeta(module.PageMeta{
			Title:       op.Title,
			Description: clip(fmt.Sprintf("%s by %s, %s.", view.Kind, view.Owner, view.When), 160),
		})
	}
	app.render(ctx, w, http.StatusOK, "fleet.html", data)
}

// fleetFit is one fit a fleet's pilots are asked to bring. Href is empty
// for a viewer who cannot open it.
type fleetFit struct {
	id               int64
	Name, Ship, Role string
	Href             string
}

// fleetFits is what a fleet flies: the copy kept from its start, else
// its doctrine as it stands. The doctrine is nil once it is gone, or if
// it is not the fleet owner's.
func (app *Application) fleetFits(ctx context.Context, op db.Op) ([]fleetFit, *db.Doctrine) {
	if op.DoctrineID == 0 {
		return nil, nil
	}
	var live *db.Doctrine
	if d, err := app.queries.GetDoctrine(ctx, op.DoctrineID); err == nil && doctrineOwner(d) == opOwner(op) {
		live = &d
	}
	if op.DoctrineSnapshotAt.Valid && op.DoctrineSnapshot.Valid {
		var snapshot doctrineSnapshot
		if err := json.Unmarshal([]byte(op.DoctrineSnapshot.String), &snapshot); err != nil {
			logging.Errorf("fleets: doctrine snapshot of %d: %v", op.ID, err)
			return nil, live
		}
		out := make([]fleetFit, 0, len(snapshot.Fits))
		for _, fit := range snapshot.Fits {
			out = append(out, fleetFit{id: fit.ID, Name: fit.Name, Ship: app.typeNameOrID(ctx, fit.ShipTypeID), Role: fit.Role})
		}
		return out, live
	}
	if live == nil {
		return nil, nil
	}
	current := app.doctrineFitViews(ctx, []int64{live.ID})[live.ID]
	out := make([]fleetFit, 0, len(current))
	for _, fit := range current {
		out = append(out, fleetFit{id: fit.ID, Name: fit.Name, Ship: fit.Ship, Role: fit.Role})
	}
	return out, live
}

// doctrineRoleFor is the part a ship plays in a fleet's doctrine, when
// every fit of that ship agrees on one the sign-up form offers.
func doctrineRoleFor(fits []fleetFit, ship string) string {
	role, found := "", false
	for _, fit := range fits {
		if !strings.EqualFold(fit.Ship, ship) {
			continue
		}
		if found && fit.Role != role {
			return ""
		}
		role, found = fit.Role, true
	}
	if !slices.Contains(opFleetRoles, role) {
		return ""
	}
	return role
}

// fillFleetSignup sets the sign-up form: the viewer's characters that
// may sign up, starting on the acting one, and its answer so far.
func (app *Application) fillFleetSignup(ctx context.Context, r *http.Request, view *opView, op db.Op, a fleetAccess) {
	if len(a.Signers) == 0 {
		return
	}
	selected := a.Signers[0].CharacterID
	acting := sessionCharID(app.sessions, ctx)
	want, _ := strconv.ParseInt(r.URL.Query().Get("as"), 10, 64)
	for _, c := range a.Signers {
		if c.CharacterID == acting {
			selected = acting
		}
	}
	for _, c := range a.Signers {
		if want != 0 && c.CharacterID == want {
			selected = want
		}
	}
	for _, c := range a.Signers {
		view.Characters = append(view.Characters, opChoice{ID: c.CharacterID, Name: c.Name, Selected: c.CharacterID == selected})
	}
	row, err := app.queries.GetOpSignup(ctx, db.GetOpSignupParams{OpID: op.ID, CharacterID: selected})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			logging.Errorf("fleets: sign-up of %d on %d: %v", selected, op.ID, err)
		}
		return
	}
	view.Mine, view.MyShip, view.MyRole, view.MyNote = row.Response, row.Ship, row.FleetRole, row.Note
}

// fillFleetRoster sets who is coming, for a viewer who may see names.
func (app *Application) fillFleetRoster(ctx context.Context, view *opView, op db.Op, signups []db.OpSignup, a fleetAccess) {
	if !a.SeeNames {
		return
	}
	commanders, err := app.queries.ListOpCommanders(ctx, op.ID)
	if err != nil {
		logging.Errorf("fleets: commanders of %d: %v", op.ID, err)
	}
	isCommander := make(map[int64]bool, len(commanders))
	for _, c := range commanders {
		isCommander[c.CharacterID] = true
		view.Commanders = append(view.Commanders, opCommanderRow{CharacterID: c.CharacterID, Name: app.displayCharacter(ctx, c.CharacterID)})
	}
	linked := map[int64]bool{}
	if a.Plan && len(signups) > 0 {
		ids := make([]int64, len(signups))
		for i, s := range signups {
			ids[i] = s.CharacterID
		}
		rows, err := app.queries.ListWorkingCharacterIDs(ctx, ids)
		if err != nil {
			logging.Errorf("fleets: linked sign-ups of %d: %v", op.ID, err)
		}
		for _, id := range rows {
			linked[id] = true
		}
	}
	counts := map[string]int{}
	for _, s := range signups {
		row := opSignupRow{
			CharacterID: s.CharacterID, Name: app.displayCharacter(ctx, s.CharacterID), Ship: s.Ship, Role: s.FleetRole, Note: s.Note,
			CanCommand: linked[s.CharacterID] && !isCommander[s.CharacterID] && s.CharacterID != op.FcCharacterID && s.Response != opNo,
		}
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
	}
	for _, list := range [][]opSignupRow{view.Yes, view.Maybe, view.No} {
		sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	}
	for _, role := range opFleetRoles {
		if counts[role] > 0 {
			view.RoleCounts = append(view.RoleCounts, opRoleCount{Role: role, Count: counts[role]})
		}
	}
	for _, group := range []opRosterGroup{{"", view.Yes}, {"Maybe", view.Maybe}, {"Not coming", view.No}} {
		if len(group.Rows) > 0 {
			view.Roster = append(view.Roster, group)
		}
	}
}

// handleOpSignup records one character's answer. The character has to
// be the viewer's own and allowed to join; a share link can show the
// fleet but never passes that check for anyone.
func (app *Application) handleOpSignup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	v := app.fleetViewerFor(ctx)
	id, _ := strconv.ParseInt(chi.URLParam(r, "opID"), 10, 64)
	op, err := app.queries.GetOp(ctx, id)
	if err != nil || r.ParseForm() != nil {
		app.renderFleetHidden(w, r, v)
		return
	}
	facts := app.fleetFactsFor(ctx, v, []db.Op{op})[op.ID]
	share := r.FormValue("share")
	facts.ViaToken = validFleetShare(op, share)
	a := v.access(op, facts)
	if !a.View {
		app.renderFleetHidden(w, r, v)
		return
	}
	back := opURL(op.ID)
	if facts.ViaToken {
		back = fleetSharePath(share)
	}
	characterID, _ := strconv.ParseInt(r.FormValue("character"), 10, 64)
	allowed := false
	for _, c := range a.Signers {
		allowed = allowed || c.CharacterID == characterID
	}
	response := r.FormValue("response")
	if response != opYes && response != opMaybe && response != opNo {
		allowed = false
	}
	switch {
	case !allowed:
		app.flash(ctx, "That sign-up could not be recorded.")
	case op.CancelledAt.Valid:
		app.flash(ctx, "This fleet was cancelled.")
	case opEnd(op).Before(time.Now()):
		app.flash(ctx, "This fleet is over.")
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
		if role == "" && ship != "" {
			fits, _ := app.fleetFits(ctx, op)
			role = doctrineRoleFor(fits, ship)
		}
		if err := app.queries.UpsertOpSignup(ctx, db.UpsertOpSignupParams{
			OpID: op.ID, CharacterID: characterID, UserID: sql.NullInt64{Int64: v.UserID, Valid: v.UserID != 0},
			Response: response, Ship: ship, FleetRole: role, Note: note, UpdatedAt: time.Now().UTC(),
		}); err != nil {
			logging.Errorf("fleets: sign-up of character %d to fleet %d: %v", characterID, op.ID, err)
			app.flash(ctx, "That sign-up could not be saved. Try again in a moment.")
		}
	}
	http.Redirect(w, r, fmt.Sprintf("%s?as=%d", back, characterID), http.StatusSeeOther)
}

// handleOpCancel cancels a fleet, or brings a cancelled one back. A
// cancelled fleet stays listed, struck through, so that those who
// signed up see what happened to it.
func (app *Application) handleOpCancel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	op, _, _, a, ok := app.fleetFromURL(w, r)
	if !ok {
		return
	}
	if a.Manage {
		cancelled := sql.NullTime{}
		if !op.CancelledAt.Valid {
			cancelled = timeSet(time.Now().UTC())
		}
		if err := app.queries.SetOpCancelled(ctx, db.SetOpCancelledParams{CancelledAt: cancelled, ID: op.ID}); err != nil {
			logging.Errorf("fleets: cancel fleet %d: %v", op.ID, err)
		} else if err := markFleetReportChanged(ctx, app.queries, op.ID); err != nil {
			logging.Errorf("fleets: mark report of %d: %v", op.ID, err)
		}
	}
	http.Redirect(w, r, opURL(op.ID), http.StatusSeeOther)
}

// handleFleetShareReset makes a new share link; the old one stops
// working.
func (app *Application) handleFleetShareReset(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	op, _, _, a, ok := app.fleetFromURL(w, r)
	if !ok {
		return
	}
	if a.Plan {
		token, err := newShareToken()
		if err == nil {
			err = app.queries.SetOpShareToken(ctx, db.SetOpShareTokenParams{ShareToken: sql.NullString{String: token, Valid: true}, ID: op.ID})
		}
		if err != nil {
			logging.Errorf("fleets: new share link for %d: %v", op.ID, err)
			app.flash(ctx, "A new link could not be made. Try again in a moment.")
		} else {
			app.flash(ctx, "New share link made. The old link no longer works.")
		}
	}
	http.Redirect(w, r, opURL(op.ID), http.StatusSeeOther)
}

// handleFleetCommander adds or removes a co-FC. Only a pilot on the
// roster with a working account can be one, since commanding a fleet
// takes an account.
func (app *Application) handleFleetCommander(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	op, v, _, a, ok := app.fleetFromURL(w, r)
	if !ok {
		return
	}
	characterID, _ := strconv.ParseInt(r.FormValue("character"), 10, 64)
	switch {
	case !a.Plan || characterID == 0:
	case r.FormValue("do") == "remove":
		if err := app.queries.RemoveOpCommander(ctx, db.RemoveOpCommanderParams{OpID: op.ID, CharacterID: characterID}); err != nil {
			logging.Errorf("fleets: remove commander %d from %d: %v", characterID, op.ID, err)
		}
	default:
		_, signed := app.queries.GetOpSignup(ctx, db.GetOpSignupParams{OpID: op.ID, CharacterID: characterID})
		linked, err := app.queries.ListWorkingCharacterIDs(ctx, []int64{characterID})
		if signed != nil || err != nil || len(linked) == 0 || characterID == op.FcCharacterID {
			app.flash(ctx, "A co-FC has to be on the roster and have an EveSynapse account.")
			break
		}
		if err := app.queries.AddOpCommander(ctx, db.AddOpCommanderParams{
			OpID: op.ID, CharacterID: characterID, AddedBy: firstCharacter(v), AddedAt: time.Now().UTC(),
		}); err != nil {
			logging.Errorf("fleets: add commander %d to %d: %v", characterID, op.ID, err)
		}
	}
	http.Redirect(w, r, opURL(op.ID), http.StatusSeeOther)
}

// firstCharacter is the character an action is recorded under.
func firstCharacter(v fleetViewer) int64 {
	if len(v.Characters) > 0 {
		return v.Characters[0].CharacterID
	}
	return 0
}

// mustFleetID is the id in a route whose pattern only matches digits.
func mustFleetID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(chi.URLParam(r, "opID"), 10, 64)
	return id
}

// handleOldOpPath sends an address from before Fleets to its new page.
// A form posted to one keeps its method.
func handleOldOpPath(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(chi.URLParam(r, "*"), "/")
	target := fleetsPath
	switch rest {
	case "new":
		target = planPath
	case "save":
		target = "/fleets/save"
	case "paps":
		target = "/fleets/history/"
	default:
		id, action, _ := strings.Cut(rest, "/")
		if n, err := strconv.ParseInt(id, 10, 64); err == nil && n > 0 {
			switch action {
			case "", "edit", "signup", "cancel", "attendance":
				target = fleetsPath + strconv.FormatInt(n, 10) + "/" + action
			}
		}
	}
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	status := http.StatusMovedPermanently
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		status = http.StatusPermanentRedirect
	}
	http.Redirect(w, r, target, status)
}

// clip shortens s to at most max bytes without splitting a character.
func clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return strings.ToValidUTF8(s[:max], "")
}
