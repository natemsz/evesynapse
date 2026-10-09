package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Attendance: who was on an op (the PAP).
//
// The default is to take it from the fleet. While an op runs, the
// worker reads the fleet its commander is in and records the members.
// ESI shows a fleet's members only to the fleet's boss, and only
// while the fleet exists, so this works when the op's commander is
// linked here, has granted fleet access, and is the boss.
//
// When any of that is missing the op's page says which, and a manager
// marks attendance by hand from the sign-up list instead. A hand mark
// never replaces what the fleet reported.
// ---------------------------------------------------------------------------

// Attendance sources. Stored in op_attendance.source.
const (
	attendanceFleet  = "fleet"
	attendanceManual = "manual"
)

// How the automatic capture is going. Stored in ops.capture_status;
// the empty string means it has not been tried yet.
const (
	captureRunning = "capturing"
	captureNoFC    = "no_fc"    // the commander is not linked here, or its sign-in is dead
	captureNoScope = "no_scope" // the commander has not granted fleet access
	captureNoFleet = "no_fleet" // the commander is not in a fleet
	captureNotBoss = "not_boss" // the commander is in a fleet but is not its boss
	captureError   = "error"
)

const (
	fleetScope = "esi-fleets.read_fleet.v1"
	// The fleet is looked for from a little before the op's start to
	// a little after its end: fleets form up early and run over.
	opCaptureLead = 15 * time.Minute
	opCaptureTail = 15 * time.Minute
)

// captureOpAttendance records the members of each running op's fleet.
// It makes no call at all unless an op is inside its window, so an
// install with no ops pays nothing for it.
func (app *Application) captureOpAttendance(ctx context.Context, now time.Time) (stored int, limited bool) {
	ops, err := app.queries.ListOpsToCapture(ctx, db.ListOpsToCaptureParams{
		StartsBefore: now.Add(opCaptureLead), EndsAfter: now.Add(-opCaptureTail),
	})
	if err != nil {
		logging.Errorf("worker: ops to capture: %v", err)
		return 0, false
	}
	for _, op := range ops {
		status, seen, hitLimit := app.captureOneOp(ctx, op, now)
		if hitLimit {
			return stored, true
		}
		stored += seen
		if err := app.queries.SetOpCaptureStatus(ctx, db.SetOpCaptureStatusParams{
			CaptureStatus: status, CheckedAt: timeSet(now.UTC()), ID: op.ID,
		}); err != nil {
			logging.Errorf("worker: record capture status for op %d: %v", op.ID, err)
		}
	}
	return stored, false
}

// captureOneOp reads one op's fleet and records who is in it.
func (app *Application) captureOneOp(ctx context.Context, op db.Op, now time.Time) (status string, seen int, limited bool) {
	fc, err := app.queries.GetCharacter(ctx, op.FcCharacterID)
	if err != nil || !characterSyncs(fc) {
		return captureNoFC, 0, false
	}
	if !scopeSet(fc.Scopes)[fleetScope] {
		return captureNoScope, 0, false
	}
	token, err := app.validAccessToken(ctx, fc)
	if err != nil {
		return captureNoFC, 0, false
	}

	var fleet esi.CharacterFleet
	if err := app.esi.Get(ctx, token, fmt.Sprintf("/characters/%d/fleet/", fc.CharacterID), &fleet); err != nil {
		if errors.Is(err, esi.ErrErrorLimit) {
			return "", 0, true
		}
		if code, ok := esi.StatusCode(err); ok && code == http.StatusNotFound {
			return captureNoFleet, 0, false
		}
		logging.Warnf("worker: op %d: fleet of character %d: %v", op.ID, fc.CharacterID, err)
		return captureError, 0, false
	}
	if fleet.FleetBossID != fc.CharacterID {
		return captureNotBoss, 0, false
	}

	var members []esi.FleetMember
	if err := app.esi.Get(ctx, token, fmt.Sprintf("/fleets/%d/members/", fleet.FleetID), &members); err != nil {
		if errors.Is(err, esi.ErrErrorLimit) {
			return "", 0, true
		}
		// ESI answers 403 or 404 here when the character stopped
		// being boss between the two calls.
		if code, ok := esi.StatusCode(err); ok && (code == http.StatusNotFound || code == http.StatusForbidden) {
			return captureNotBoss, 0, false
		}
		logging.Warnf("worker: op %d: members of fleet %d: %v", op.ID, fleet.FleetID, err)
		return captureError, 0, false
	}
	for _, m := range members {
		if m.CharacterID == 0 {
			continue
		}
		if err := app.queries.UpsertFleetAttendance(ctx, db.UpsertFleetAttendanceParams{
			OpID: op.ID, CharacterID: m.CharacterID, ShipTypeID: m.ShipTypeID, SeenAt: now.UTC(),
		}); err != nil {
			logging.Errorf("worker: op %d: record attendance of %d: %v", op.ID, m.CharacterID, err)
			continue
		}
		seen++
	}
	return captureRunning, seen, false
}

// ---------------------------------------------------------------------------
// The op page's attendance section.
// ---------------------------------------------------------------------------

type opAttendanceRow struct {
	CharacterID int64
	Name        string
	Ship        string
	ShipTypeID  int64
	ByHand      bool
}

// opTick is one line of the hand-marking list.
type opTick struct {
	CharacterID int64
	Name        string
	Present     bool
	FromFleet   bool // recorded from the fleet: shown ticked, not editable
}

// opAttendance is what the op page shows about attendance.
type opAttendance struct {
	Rows []opAttendanceRow
	// Capture explains, in a sentence, how the automatic capture is
	// going or why it is not.
	Capture string
	// Capturing: the fleet is being read successfully, so hand-marking
	// is not needed.
	Capturing bool
	// CanMark: the viewer may mark attendance by hand now (a manager,
	// once the op has started).
	CanMark bool
	// Ticks is the hand-marking list, for a manager once the op has
	// started: everyone who signed up, plus anyone already recorded.
	Ticks []opTick
}

// captureExplanation words the capture state for the op's page.
func captureExplanation(op db.Op, fc string, now time.Time, recorded int) string {
	started := !now.Before(op.StartsAt.Add(-opCaptureLead))
	over := now.After(opEnd(op).Add(opCaptureTail))
	switch op.CaptureStatus {
	case captureRunning:
		if over {
			return fmt.Sprintf("Attendance was recorded from %s's fleet.", fc)
		}
		return fmt.Sprintf("Attendance is being recorded from %s's fleet: %d so far.", fc, recorded)
	case captureNoFC:
		return fmt.Sprintf("%s is not linked to EveSynapse, or its sign-in has stopped working, so its fleet could not be read.", fc)
	case captureNoScope:
		return fmt.Sprintf("%s has not granted fleet access, so its fleet could not be read. Signing %s in again grants it.", fc, fc)
	case captureNoFleet:
		return fmt.Sprintf("%s was not in a fleet when last checked, so there was nothing to record.", fc)
	case captureNotBoss:
		return fmt.Sprintf("%s was in a fleet but was not its boss, and EVE only shows a fleet's members to its boss.", fc)
	case captureError:
		return "The fleet could not be read from EVE when last tried."
	}
	if !started {
		return fmt.Sprintf("Attendance will be recorded from %s's fleet while the op runs. %s has to be the fleet's boss.", fc, fc)
	}
	return "Attendance has not been recorded from a fleet."
}

// opAttendanceFor builds the attendance section for one op.
func (app *Application) opAttendanceFor(ctx context.Context, op db.Op, signups []db.OpSignup, canManage bool, now time.Time) *opAttendance {
	rows, err := app.queries.ListOpAttendance(ctx, op.ID)
	if err != nil {
		logging.Errorf("ops: attendance for op %d: %v", op.ID, err)
	}
	fc := app.displayCharacter(ctx, op.FcCharacterID)
	att := &opAttendance{
		Capture:   captureExplanation(op, fc, now, len(rows)),
		Capturing: op.CaptureStatus == captureRunning,
	}
	present := map[int64]db.OpAttendance{}
	for _, row := range rows {
		present[row.CharacterID] = row
		ship := ""
		if row.ShipTypeID != 0 {
			ship = app.typeNameOrID(ctx, row.ShipTypeID)
		}
		att.Rows = append(att.Rows, opAttendanceRow{
			CharacterID: row.CharacterID, Name: app.displayCharacter(ctx, row.CharacterID),
			Ship: ship, ShipTypeID: row.ShipTypeID, ByHand: row.Source == attendanceManual,
		})
	}
	sort.Slice(att.Rows, func(i, j int) bool { return att.Rows[i].Name < att.Rows[j].Name })

	if canManage && !op.CancelledAt.Valid && !now.Before(op.StartsAt) {
		att.CanMark = true
		listed := map[int64]bool{}
		add := func(id int64) {
			if listed[id] {
				return
			}
			listed[id] = true
			row, here := present[id]
			att.Ticks = append(att.Ticks, opTick{
				CharacterID: id, Name: app.displayCharacter(ctx, id),
				Present: here, FromFleet: here && row.Source == attendanceFleet,
			})
		}
		for _, s := range signups {
			add(s.CharacterID)
		}
		for _, row := range rows {
			add(row.CharacterID)
		}
		sort.Slice(att.Ticks, func(i, j int) bool { return att.Ticks[i].Name < att.Ticks[j].Name })
	}
	return att
}

// handleOpAttendance saves a manager's hand-marking: the characters
// ticked are present, the others it could have ticked are not. Only
// characters on the op's sign-up list can be marked, and nothing the
// fleet reported is changed.
func (app *Application) handleOpAttendance(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	members := app.opMembers(ctx, userID)
	op, ok := app.opFromURL(w, r, members)
	if !ok {
		return
	}
	now := time.Now().UTC()
	switch {
	case !opManages(members, op.CorporationID):
		// Not theirs to mark; nothing is said, nothing changes.
	case r.ParseForm() != nil:
	case op.CancelledAt.Valid:
		app.flash(ctx, "This op was cancelled.")
	case now.Before(op.StartsAt):
		app.flash(ctx, "Attendance can be marked once the op has started.")
	default:
		signups, err := app.queries.ListOpSignups(ctx, op.ID)
		if err != nil {
			logging.Errorf("ops: sign-ups for op %d: %v", op.ID, err)
			break
		}
		signedUp := map[int64]bool{}
		for _, s := range signups {
			signedUp[s.CharacterID] = true
		}
		keep := []int64{} // never nil: an empty list must still mean "none"
		for _, raw := range r.Form["present"] {
			id, _ := strconv.ParseInt(raw, 10, 64)
			if !signedUp[id] {
				continue
			}
			keep = append(keep, id)
			if err := app.queries.InsertManualAttendance(ctx, db.InsertManualAttendanceParams{OpID: op.ID, CharacterID: id, SeenAt: now}); err != nil {
				logging.Errorf("ops: mark attendance of %d on op %d: %v", id, op.ID, err)
			}
		}
		if err := app.queries.DeleteManualAttendanceExcept(ctx, db.DeleteManualAttendanceExceptParams{OpID: op.ID, KeepIds: keep}); err != nil {
			logging.Errorf("ops: clear attendance on op %d: %v", op.ID, err)
		}
		app.flash(ctx, "Attendance saved.")
	}
	http.Redirect(w, r, opURL(op.ID), http.StatusSeeOther)
}

// ---------------------------------------------------------------------------
// The PAP table: how often each character has attended.
// ---------------------------------------------------------------------------

const (
	papWindow = 90 * 24 * time.Hour
	papRecent = 30 * 24 * time.Hour
)

type papRow struct {
	CharacterID int64
	Name        string
	Recent      int64 // ops attended in the last 30 days
	Total       int64 // in the last 90
	LastOp      string
}

type papCorp struct {
	Name string
	Rows []papRow
}

// papsView is the /ops/paps page.
type papsView struct {
	Corps []papCorp
}

func (app *Application) handleOpPAPs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	now := time.Now().UTC()
	view := &papsView{}
	for _, corpID := range opCorps(app.opMembers(ctx, userID), false) {
		rows, err := app.queries.ListCorporationPAPs(ctx, db.ListCorporationPAPsParams{
			CorporationID: corpID, Since: now.Add(-papWindow), RecentSince: now.Add(-papRecent),
		})
		if err != nil {
			logging.Errorf("ops: PAPs for corporation %d: %v", corpID, err)
			continue
		}
		corp := papCorp{Name: app.corpDisplayName(ctx, corpID)}
		for _, row := range rows {
			corp.Rows = append(corp.Rows, papRow{
				CharacterID: row.CharacterID, Name: app.displayCharacter(ctx, row.CharacterID),
				Recent: row.Recent, Total: row.Total, LastOp: row.LastOp.UTC().Format("Jan 2"),
			})
		}
		view.Corps = append(view.Corps, corp)
	}
	app.render(ctx, w, http.StatusOK, "op_paps.html", pageData{
		LoggedIn:      true,
		CharacterName: app.sessions.GetString(ctx, sessionCharacterName),
		SSOConfigured: app.cfg.SSOConfigured(),
		PAPs:          view,
	})
}
