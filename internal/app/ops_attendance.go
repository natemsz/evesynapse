package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
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
// fleet lane (fleet_read.go) tries its FC and co-FCs until one is the
// boss, and records the members. ESI shows a fleet's members only to
// the fleet's boss, and only while the fleet exists, so this works
// when a commander is linked here, is still in the group running the
// fleet, has granted fleet access, and is the boss.
//
// When none is, the op's page says why for each, and a manager
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
	// captureLeftOwner: the commander is no longer in the group that
	// runs the fleet, so what it can see is not recorded for that group.
	captureLeftOwner = "left_owner"
)

const (
	fleetScope = "esi-fleets.read_fleet.v1"
	// The fleet is looked for from a little before the op's start to
	// a little after its end: fleets form up early and run over.
	opCaptureLead = 15 * time.Minute
	opCaptureTail = 15 * time.Minute
)

// captureAttempt is how reading the fleet through one commander went.
// Stored as JSON in ops.capture_detail.
type captureAttempt struct {
	CharacterID int64  `json:"id"`
	Status      string `json:"status"`
}

// captureOutcome is one fleet's result for a pass.
type captureOutcome struct {
	Status   string
	BossID   int64
	Seen     int
	Attempts []captureAttempt
}

// captureOpAttendance reads each running fleet and stores who is in it.
// It makes no ESI call unless a fleet is inside its window. A pass
// reads a bounded number of fleets, longest unread first, so many
// fleets at once share ESI's fleet budget in turn.
func (app *Application) captureOpAttendance(ctx context.Context, now time.Time) (stored int, limited bool) {
	ops, err := app.queries.ListOpsToCapture(ctx, db.ListOpsToCaptureParams{
		StartsBefore: now.Add(opCaptureLead), EndsAfter: now.Add(-opCaptureTail),
	})
	if err != nil {
		logging.Errorf("worker: ops to capture: %v", err)
		return 0, false
	}
	// The demo's fleet is invented: there is nothing in EVE to read.
	ops = slices.DeleteFunc(ops, isDemoFleet)
	if len(ops) == 0 {
		return 0, false
	}
	ids := make([]int64, len(ops))
	for i, op := range ops {
		ids[i] = op.ID
	}
	rows, err := app.queries.ListOpCommandersForOps(ctx, ids)
	if err != nil {
		logging.Errorf("worker: commanders of running fleets: %v", err)
		return 0, false
	}
	coFCs := make(map[int64][]int64, len(ops))
	for _, row := range rows {
		coFCs[row.OpID] = append(coFCs[row.OpID], row.CharacterID)
	}
	accounts := map[int64][]accountCharacter{}
	for _, op := range ops {
		out, hitLimit := app.captureOneOp(ctx, op, commanderOrder(op, coFCs[op.ID]), accounts, now)
		if hitLimit {
			return stored, true
		}
		stored += out.Seen
		detail, err := json.Marshal(out.Attempts)
		if err != nil {
			logging.Errorf("worker: capture detail for op %d: %v", op.ID, err)
			detail = nil
		}
		if err := app.queries.SetOpCaptureStatus(ctx, db.SetOpCaptureStatusParams{
			CaptureStatus: out.Status, CheckedAt: timeSet(now.UTC()),
			BossID: out.BossID, Detail: string(detail), ID: op.ID,
		}); err != nil {
			logging.Errorf("worker: record capture status for op %d: %v", op.ID, err)
		}
	}
	return stored, false
}

// commanderOrder is who to try for a fleet: whoever was boss last time
// (a handoff usually stays put), then the FC, then the co-FCs.
func commanderOrder(op db.Op, coFCs []int64) []int64 {
	sorted := slices.Clone(coFCs)
	slices.Sort(sorted)
	out := make([]int64, 0, len(sorted)+2)
	add := func(id int64) {
		if id != 0 && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	add(op.CaptureBossID)
	if op.CaptureBossID != 0 && op.CaptureBossID != op.FcCharacterID && !slices.Contains(sorted, op.CaptureBossID) {
		out = out[:0] // no longer a commander of this fleet
	}
	add(op.FcCharacterID)
	for _, id := range sorted {
		add(id)
	}
	return out
}

// captureOneOp tries each commander until one is the fleet's boss, and
// stores that fleet. With no boss among them, the status is the most
// useful reason any commander gave.
func (app *Application) captureOneOp(ctx context.Context, op db.Op, commanders []int64, accounts map[int64][]accountCharacter, now time.Time) (captureOutcome, bool) {
	var out captureOutcome
	for _, id := range commanders {
		status, read, token, limited := app.commanderFleet(ctx, op, id, accounts)
		if limited {
			return out, true
		}
		if status == captureRunning {
			seen, err := app.storeFleetRead(ctx, op, read, token, now)
			if err != nil {
				logging.Errorf("worker: op %d: store fleet %d: %v", op.ID, read.FleetID, err)
				status = captureError
			} else {
				out.Attempts = append(out.Attempts, captureAttempt{CharacterID: id, Status: status})
				out.Status, out.BossID, out.Seen = captureRunning, id, seen
				return out, false
			}
		}
		out.Attempts = append(out.Attempts, captureAttempt{CharacterID: id, Status: status})
	}
	out.Status = captureError
	for _, want := range []string{captureError, captureNotBoss, captureNoFleet, captureNoScope, captureLeftOwner, captureNoFC} {
		if slices.ContainsFunc(out.Attempts, func(a captureAttempt) bool { return a.Status == want }) {
			out.Status = want
			break
		}
	}
	return out, false
}

// commanderFleet reads the fleet through one commander, and says why
// not when it cannot.
func (app *Application) commanderFleet(ctx context.Context, op db.Op, id int64, accounts map[int64][]accountCharacter) (status string, read fleetRead, token string, limited bool) {
	ch, err := app.queries.GetCharacter(ctx, id)
	if err != nil || !app.characterMaySync(ctx, ch) {
		return captureNoFC, read, "", false
	}
	characters, known := accounts[ch.UserID]
	if !known {
		if characters, err = app.accountCharacters(ctx, ch.UserID); err != nil {
			logging.Errorf("worker: op %d: standing of commander %d: %v", op.ID, id, err)
			return captureError, read, "", false
		}
		accounts[ch.UserID] = characters
	}
	owner := opOwner(op)
	if !slices.ContainsFunc(characters, func(c accountCharacter) bool {
		return c.CharacterID == id && c.Standing.Belongs(owner)
	}) {
		return captureLeftOwner, read, "", false
	}
	if !scopeSet(ch.Scopes)[fleetScope] {
		return captureNoScope, read, "", false
	}
	if app.refusalSettled(ctx, ch.CharacterID, opFleetKind) {
		return captureNoScope, read, "", false // the fleet read is refused and settled for this login
	}
	token, err = app.validAccessToken(ctx, ch)
	if err != nil {
		return captureNoFC, read, "", false
	}
	read, err = app.readCharacterFleet(ctx, token, ch.CharacterID)
	if err != nil {
		if errors.Is(err, esi.ErrErrorLimit) {
			return "", read, "", true
		}
		if read.FleetID == 0 && app.recordFetchRefusal(ctx, ch.CharacterID, opFleetKind, err) {
			return captureNoScope, read, "", false
		}
		logging.Warnf("worker: op %d: fleet of character %d: %v", op.ID, ch.CharacterID, err)
		return captureError, read, "", false
	}
	switch read.State {
	case fleetReadNone:
		return captureNoFleet, read, "", false
	case fleetReadMember:
		return captureNotBoss, read, "", false
	}
	return captureRunning, read, token, false
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
	// CaptureNotes says how each commander's read went, when more than
	// one was tried and none could read the fleet.
	CaptureNotes []string
	// Capturing: the fleet is being read successfully, so hand-marking
	// is not needed.
	Capturing bool
	// CanMark: the viewer may mark attendance by hand now (a manager,
	// once the op has started).
	CanMark bool
	// MarkLater: marking waits for the fleet to end, because the list
	// would show who is in it to a viewer who cannot see that.
	MarkLater bool
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
	case captureLeftOwner:
		return fmt.Sprintf("%s has left the group running this fleet, so its fleet is not read.", fc)
	case captureError:
		return "The fleet could not be read from EVE when last tried."
	}
	if !started {
		return fmt.Sprintf("Attendance will be recorded from %s's fleet while it runs. %s has to be the fleet's boss.", fc, fc)
	}
	return "Attendance has not been recorded from a fleet."
}

// captureSummary is the sentence on how the fleet's capture is going,
// and one line per commander when several were tried and none could
// read it. After a handoff the fleet is named by whoever holds it now.
func (app *Application) captureSummary(ctx context.Context, op db.Op, now time.Time, recorded int) (string, []string) {
	if notes := app.captureNotes(ctx, op); len(notes) > 0 {
		return "None of this fleet's commanders could read it. EVE only shows a fleet's members to its boss.", notes
	}
	reader := op.FcCharacterID
	if op.CaptureStatus == captureRunning && op.CaptureBossID != 0 {
		reader = op.CaptureBossID
	}
	return captureExplanation(op, app.displayCharacter(ctx, reader), now, recorded), nil
}

// captureNotes words each commander's failed read, when several were
// tried. One commander is covered by captureExplanation.
func (app *Application) captureNotes(ctx context.Context, op db.Op) []string {
	if op.CaptureStatus == captureRunning || op.CaptureDetail == "" {
		return nil
	}
	var attempts []captureAttempt
	if err := json.Unmarshal([]byte(op.CaptureDetail), &attempts); err != nil {
		logging.Errorf("ops: capture detail of %d: %v", op.ID, err)
		return nil
	}
	if len(attempts) < 2 {
		return nil
	}
	reasons := map[string]string{
		captureNoFC:      "not linked to EveSynapse, or its sign-in has stopped working",
		captureNoScope:   "has not granted fleet access",
		captureNoFleet:   "not in a fleet",
		captureNotBoss:   "in a fleet, but not its boss",
		captureLeftOwner: "has left the group running this fleet",
		captureError:     "could not be read from EVE",
	}
	notes := make([]string, 0, len(attempts))
	for _, a := range attempts {
		if reason, ok := reasons[a.Status]; ok {
			notes = append(notes, app.displayCharacter(ctx, a.CharacterID)+": "+reason+".")
		}
	}
	return notes
}

// opAttendanceFor builds the attendance section for one op. Who flew is
// for those running the fleet and the pilots who were in it, and a pilot
// sees only their own ship. While the fleet runs, the pilots read from it
// are who is in it now, so they show only to a viewer who sees the whole
// fleet in game.
func (app *Application) opAttendanceFor(ctx context.Context, op db.Op, signups []db.OpSignup, canManage bool, viewer []int64, sight fleetSight, now time.Time) *opAttendance {
	rows, err := app.queries.ListOpAttendance(ctx, op.ID)
	if err != nil {
		logging.Errorf("ops: attendance for op %d: %v", op.ID, err)
	}
	capture, notes := app.fleetCaptureStatus(ctx, op, now, len(rows), sight)
	flew := slices.ContainsFunc(rows, func(row db.OpAttendance) bool { return slices.Contains(viewer, row.CharacterID) })
	if !canManage && !flew {
		rows = nil
	}
	hidesFleet := fleetLiveOpen(op, now) && !sight.All && slices.ContainsFunc(rows, func(row db.OpAttendance) bool { return row.Source == attendanceFleet })
	if hidesFleet {
		rows = slices.DeleteFunc(rows, func(row db.OpAttendance) bool { return row.Source == attendanceFleet })
	}
	att := &opAttendance{
		Capture: capture, CaptureNotes: notes,
		Capturing: op.CaptureStatus == captureRunning,
	}
	present := map[int64]db.OpAttendance{}
	for _, row := range rows {
		present[row.CharacterID] = row
		ship, shipID := "", int64(0)
		if row.ShipTypeID != 0 && (canManage || sight.All || slices.Contains(viewer, row.CharacterID)) {
			ship, shipID = app.typeNameOrID(ctx, row.ShipTypeID), row.ShipTypeID
		}
		att.Rows = append(att.Rows, opAttendanceRow{
			CharacterID: row.CharacterID, Name: app.displayCharacter(ctx, row.CharacterID),
			Ship: ship, ShipTypeID: shipID, ByHand: row.Source == attendanceManual,
		})
	}
	sort.Slice(att.Rows, func(i, j int) bool { return att.Rows[i].Name < att.Rows[j].Name })

	att.MarkLater = canManage && hidesFleet
	if canManage && !hidesFleet && !op.CancelledAt.Valid && !now.Before(op.StartsAt) {
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

// attendanceKeptFrom: the fleet is running and has been read, and this
// viewer does not see all of it. Saving a hand list would show them who
// is in it, because a pilot the fleet reported stays as reported.
func (app *Application) attendanceKeptFrom(ctx context.Context, op db.Op, v fleetViewer, now time.Time) bool {
	if !fleetLiveOpen(op, now) {
		return false
	}
	live, err := app.fleetLiveReadFor(ctx, op, v, now)
	if err != nil {
		logging.Errorf("ops: composition of %d: %v", op.ID, err)
		return true
	}
	if live.sight.All {
		return false
	}
	rows, err := app.queries.ListOpAttendance(ctx, op.ID)
	if err != nil {
		logging.Errorf("ops: attendance for op %d: %v", op.ID, err)
		return true
	}
	return slices.ContainsFunc(rows, func(row db.OpAttendance) bool { return row.Source == attendanceFleet })
}

// handleOpAttendance saves a manager's hand-marking: the characters
// ticked are present, the others it could have ticked are not. Only
// characters on the op's sign-up list can be marked, and nothing the
// fleet reported is changed.
func (app *Application) handleOpAttendance(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	op, v, _, a, ok := app.fleetFromURL(w, r)
	if !ok {
		return
	}
	now := time.Now().UTC()
	switch {
	case !a.Manage:
		// Not theirs to mark; nothing is said, nothing changes.
	case r.ParseForm() != nil:
	case op.CancelledAt.Valid:
		app.flash(ctx, "This fleet was cancelled.")
	case now.Before(op.StartsAt):
		app.flash(ctx, "Attendance can be marked once the fleet has started.")
	case app.attendanceKeptFrom(ctx, op, v, now):
		app.flash(ctx, "Attendance can be marked by hand once the fleet is over.")
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
		if err := markFleetReportChanged(ctx, app.queries, op.ID); err != nil {
			logging.Errorf("ops: mark report of op %d: %v", op.ID, err)
		}
		app.flash(ctx, "Attendance saved.")
	}
	http.Redirect(w, r, opURL(op.ID), http.StatusSeeOther)
}
