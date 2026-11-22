package srp

import (
	"context"
	"database/sql"
	"evesynapse/internal/app/module"
	"evesynapse/internal/humanize"
	"fmt"
	json "github.com/goccy/go-json"
	"math"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
	"evesynapse/internal/standing"
)

// ---------------------------------------------------------------------------
// Ship replacement (SRP).
//
// A pilot who lost a ship on a fleet asks for it to be replaced. A
// request rests on two things EveSynapse already holds: the killmail
// and the fleet it happened on. The loss has to fall inside the fleet's
// time, and the fleet's policy (policy.go) can ask for more. Whether the
// fleet recorded the pilot (ops_attendance.go) is shown to whoever
// handles the request, read fresh each time, because attendance can be
// ticked by hand later.
//
// Requests belong to the fleet's owner: a corporation, alliance or
// community. They are handled by the people who run it and whoever they
// hand it to (owner_permissions.go). Handling is marking a request paid,
// with the amount, or denying it with a reason. EVE has no call that
// moves ISK, so the paying is done in game; the pay list mail
// (srp_handle.go) puts what is owed in the handler's inbox there.
//
// Pilots' names on the ledger are never public. Who can read it is the
// policy's choice between the handlers and the owner's members.
// ---------------------------------------------------------------------------

const srpPath = "/srp/"

// Request states (srp_requests.status). Stored: never rename.
const (
	srpOpen   = "open"
	srpPaid   = "paid"
	srpDenied = "denied"
)

const (
	// srpClaimWindow is how long after a loss it can be claimed.
	srpClaimWindow = 30 * 24 * time.Hour
	// srpKillmailsRead is how many of a character's newest killmails
	// are looked through for losses.
	srpKillmailsRead = 200
	srpNoteMax       = 500
	srpPolicyMax     = 2000
	srpMineShown     = 50
	srpHandledShown  = 25
	srpPaidStatDays  = 30
	// srpFleetsRead bounds the fleets a page checks losses against.
	srpFleetsRead = 100
	// srpOpenShown bounds the handler's queue on one page.
	srpOpenShown = 100
	// srpLossReach is how old a loss can be and still be inside a claim
	// window, which runs from the close of a fleet of the longest length.
	srpLossReach = srpClaimWindow + module.OpMaxDuration*time.Minute + module.OpCaptureTail
)

// srpLoss is one of an account's recent losses.
type srpLoss struct {
	km   esi.Killmail
	hash string
	at   time.Time
}

// srpLosses lists the losses of an account's characters inside the
// claim window, newest first, from stored killmails only.
func (app *mod) srpLosses(ctx context.Context, characters []db.Character, now time.Time) []srpLoss {
	var ids, mine []int64
	for _, ch := range characters {
		var refs []esi.KillmailRef
		if !app.deps.LoadCorpSnapshot(ctx, ch.CharacterID, esi.SnapKillmails, &refs) {
			continue
		}
		mine = append(mine, ch.CharacterID)
		if len(refs) > srpKillmailsRead {
			refs = refs[:srpKillmailsRead]
		}
		for _, ref := range refs {
			ids = append(ids, ref.KillmailID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := app.queries.ListLossDetails(ctx, db.ListLossDetailsParams{KillmailIds: ids, CharacterIds: mine})
	if err != nil {
		logging.Errorf("srp: read losses: %v", err)
		return nil
	}
	var out []srpLoss
	for _, row := range rows {
		var km esi.Killmail
		if json.Unmarshal([]byte(row.Payload), &km) != nil {
			continue
		}
		at, err := time.Parse(time.RFC3339, km.KillmailTime)
		if err != nil || now.Sub(at) > srpLossReach {
			continue
		}
		out = append(out, srpLoss{km: km, hash: row.Hash, at: at})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].at.After(out[j].at) })
	return out
}

// srpDuring reports whether a moment falls inside an op, counting the
// time a fleet forms up before it and runs on after it.
func srpDuring(op db.Op, at time.Time) bool {
	return !op.CancelledAt.Valid && !at.Before(op.StartsAt.Add(-module.OpCaptureLead)) && !at.After(module.OpEnd(op).Add(module.OpCaptureTail))
}

// srpISK writes an amount in whole ISK; nothing where there is none.
func srpISK(v float64) string {
	if v <= 0 {
		return ""
	}
	return esi.FormatInt(int64(v+0.5)) + " ISK"
}

// parseISK reads an amount typed by hand: "120,000,000", "120m",
// "1.2b", with or without "ISK".
func parseISK(raw string) (float64, bool) {
	s := strings.ToLower(strings.NewReplacer(",", "", " ", "", "_", "").Replace(raw))
	s = strings.TrimSuffix(s, "isk")
	scale := 1.0
	switch {
	case strings.HasSuffix(s, "k"):
		scale, s = 1e3, strings.TrimSuffix(s, "k")
	case strings.HasSuffix(s, "m"):
		scale, s = 1e6, strings.TrimSuffix(s, "m")
	case strings.HasSuffix(s, "b"):
		scale, s = 1e9, strings.TrimSuffix(s, "b")
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || v < 0 || v*scale > 1e13 {
		return 0, false
	}
	return v * scale, true
}

// srpPilot names a pilot: by the linked character's name, else however
// the character is known.
func (app *mod) srpPilot(ctx context.Context, characterID int64) string {
	if ch, err := app.queries.GetCharacter(ctx, characterID); err == nil && ch.Name != "" {
		return ch.Name
	}
	return app.deps.DisplayCharacter(ctx, characterID)
}

// srpLossChoice is one loss the form offers, on one op.
type srpLossChoice struct {
	Value string // "killmail:op"
	Label string
}

// srpRow is one request on the page.
type srpRow struct {
	ID          int64
	Pilot       string
	PilotID     int64
	Ship        string
	ShipTypeID  int64
	System      string
	LostAt      string
	Op          string
	OpURL       string
	KillmailID  int64
	Value       string // the estimate when it was asked for
	Suggested   string // the estimate as the payout box starts
	Note        string
	Open        bool
	Paid        bool
	Payout      string
	HandledBy   string
	HandledAt   string
	HandlerNote string
	// Attended: the op's attendance has the pilot on it; Attendance
	// says how it knows.
	Attended   bool
	Attendance string
	// Doctrine is the doctrine the op named, if it named one of the
	// corporation's; DoctrineShip, that the lost hull is in it.
	Doctrine     string
	DoctrineShip bool
	// FitNote says what the loss mail showed against the doctrine.
	FitNote string
	// CanWithdraw: open, and never answered.
	CanWithdraw bool
	// Earlier: it was paid and then opened again.
	Earlier string
}

// srpCorpView is the owner a ship replacement page is about.
type srpCorpView struct {
	ID      int64
	Key     string
	Name    string
	Policy  string
	Handles bool // may answer its requests
	Directs bool // a corporation whose settings the viewer can change
	Keeps   bool // may change its policies

	Open       []srpRow
	Handled    []srpRow
	Ledger     []srpRow // answered requests its members can read
	OpenCount  int64
	OpenValue  string
	PaidCount  int64
	PaidValue  string
	Mailers    []module.NamedChoice // the account's characters that can send the EVE mail
	Policies   []srpPolicyRow
	PolicyForm *srpPolicyRow
	HandOver   string
}

// srpView is the /srp/ page: one owner's ship replacement.
type srpView struct {
	CorpOptions []module.OwnerOption
	Corp        *srpCorpView // nil when the account belongs to no owner
	Losses      []srpLossChoice
	Outside     int // recent losses that were not during one of its fleets
	Claimed     int // recent losses already asked for
	Refused     int // losses during a fleet that its policy does not cover
	Mine        []srpRow
}

// srpRows builds the page's rows for a list of requests. Attendance is
// looked up for the open ones only: it no longer matters for the rest.
func (app *mod) srpRows(ctx context.Context, requests []db.SrpRequest, ops map[int64]db.Op) []srpRow {
	rows := make([]srpRow, 0, len(requests))
	for _, req := range requests {
		op, known := ops[req.OpID]
		if !known {
			op, _ = app.queries.GetOp(ctx, req.OpID)
			ops[req.OpID] = op
		}
		row := srpRow{
			ID: req.ID, Pilot: app.srpPilot(ctx, req.CharacterID), PilotID: req.CharacterID,
			Ship: app.deps.TypeNameOrID(ctx, req.ShipTypeID), ShipTypeID: req.ShipTypeID,
			System: app.deps.LocationTitle(ctx, req.SolarSystemID, "solar_system"),
			LostAt: req.LostAt.UTC().Format("2006-01-02 15:04"),
			Op:     op.Title, OpURL: module.OpURL(req.OpID), KillmailID: req.KillmailID,
			Value: srpISK(req.LossValue), Note: req.Note,
			Open: req.Status == srpOpen, Paid: req.Status == srpPaid,
			Payout: srpISK(req.Payout), HandlerNote: req.HandlerNote,
		}
		if req.SuggestedPayout > 0 {
			row.Suggested = esi.FormatInt(int64(req.SuggestedPayout + 0.5))
		}
		row.CanWithdraw = row.Open && !req.AnsweredAt.Valid
		if req.PreviousPayout > 0 {
			row.Earlier = "Already paid " + srpISK(req.PreviousPayout) + " before it was opened again."
		}
		if req.HandledAt.Valid {
			row.HandledAt = req.HandledAt.Time.UTC().Format("2006-01-02 15:04")
			row.HandledBy = app.srpPilot(ctx, req.HandledBy)
		}
		if row.Open {
			row.FitNote = srpFitSentence(req.FitEvidence)
			if op.DoctrineID != 0 {
				// A fleet that has started is judged by the doctrine it
				// flew, not by later edits.
				if op.DoctrineSnapshotAt.Valid {
					row.Doctrine, row.DoctrineShip = op.Doctrine, slices.Contains(op.DoctrineSnapshotShips, req.ShipTypeID)
				} else if in, err := app.queries.DoctrineHasShip(ctx, db.DoctrineHasShipParams{DoctrineID: op.DoctrineID, ShipTypeID: req.ShipTypeID}); err == nil {
					row.Doctrine, row.DoctrineShip = op.Doctrine, in
				}
			}
			seen, err := app.queries.GetOpAttendance(ctx, db.GetOpAttendanceParams{OpID: req.OpID, CharacterID: req.CharacterID})
			if err == nil && !srpAttendedAt(seen.Source, seen.FirstSeenAt, seen.LastSeenAt, req.LostAt) {
				row.Attendance = "Seen in the fleet, but not when this ship was lost."
			} else if err == nil {
				row.Attended = true
				row.Attendance = "Ticked present by hand."
				if seen.Source == module.AttendanceFleet {
					row.Attendance = "Seen in the fleet."
					if seen.ShipTypeID != 0 {
						row.Attendance = "Seen in the fleet in a " + app.deps.TypeNameOrID(ctx, seen.ShipTypeID) + "."
					}
				}
			}
		}
		rows = append(rows, row)
	}
	return rows
}

func (app *mod) handleSRP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := &PageData{ShellData: app.deps.PageShell(ctx)}
	userID := app.deps.UserID(ctx)
	now := time.Now().UTC()
	view := &srpView{}
	data.SRP = view

	owner, options, found := app.deps.PickOwner(ctx, r, app.deps.DoctrineOwners(ctx, userID))
	if !found {
		app.deps.Render(ctx, w, http.StatusOK, "srp.html", data)
		return
	}
	st, err := app.deps.StandingFor(ctx, userID)
	if err != nil {
		logging.Errorf("srp: standing of user %d: %v", userID, err)
		data.Error = "Could not load ship replacement; check the server log."
	}
	cv := &srpCorpView{ID: owner.ID, Key: owner.Key(), Name: app.deps.OwnerName(ctx, owner)}
	cv.Directs = owner.Kind == standing.OwnerCorporation && st.Directs(owner.ID)
	cv.Handles = app.deps.OwnerPermits(ctx, st, owner, permSRP)
	cv.Keeps = app.deps.HoldsProgramme(ctx, st, owner, permSRP)
	if cv.Keeps && owner.Kind != standing.OwnerCorporation {
		cv.HandOver = standing.Address("/fleets/permissions/", owner.Key())
	}
	view.CorpOptions, view.Corp = options, cv
	policies, err := app.queries.ListSRPPolicies(ctx, db.ListSRPPoliciesParams{OwnerKind: owner.Kind, OwnerID: owner.ID})
	if err != nil {
		logging.Errorf("srp: policies of %s: %v", owner.Key(), err)
	}
	for _, p := range policies {
		if p.IsDefault {
			cv.Policy = p.PolicyText
		}
	}
	if cv.Keeps {
		cv.Policies = srpPolicyRows(owner, policies)
		cv.PolicyForm = app.srpPolicyForm(ctx, r, policies)
	}
	ops := map[int64]db.Op{}

	characters, _ := app.queries.ListCharactersByUser(ctx, userID)
	app.srpOffer(ctx, view, characters, owner, policies, now)

	mine, err := app.queries.ListSRPRequestsByUserForOwner(ctx, db.ListSRPRequestsByUserForOwnerParams{
		UserID: userID, OwnerKind: owner.Kind, OwnerID: owner.ID, RowLimit: srpMineShown,
	})
	if err != nil {
		logging.Errorf("srp: requests of user %d: %v", userID, err)
	}
	view.Mine = app.srpRows(ctx, mine, ops)

	if cv.Handles {
		open, err := app.queries.ListOpenSRPRequests(ctx, db.ListOpenSRPRequestsParams{OwnerKind: owner.Kind, OwnerID: owner.ID, RowLimit: srpOpenShown})
		if err != nil {
			logging.Errorf("srp: open requests of %s: %v", owner.Key(), err)
		}
		cv.Open = app.srpRows(ctx, open, ops)
		if totals, err := app.queries.CountOpenSRPRequests(ctx, db.CountOpenSRPRequestsParams{OwnerKind: owner.Kind, OwnerID: owner.ID}); err == nil {
			cv.OpenCount, cv.OpenValue = totals.Requests, srpISK(totals.Owed)
		} else {
			logging.Errorf("srp: open total of %s: %v", owner.Key(), err)
		}
		handled, _ := app.queries.ListHandledSRPRequests(ctx, db.ListHandledSRPRequestsParams{OwnerKind: owner.Kind, OwnerID: owner.ID, RowLimit: srpHandledShown})
		cv.Handled = app.srpRows(ctx, handled, ops)
		if paid, err := app.queries.SumSRPPaid(ctx, db.SumSRPPaidParams{OwnerKind: owner.Kind, OwnerID: owner.ID, Since: timeSet(now.AddDate(0, 0, -srpPaidStatDays))}); err == nil {
			cv.PaidCount, cv.PaidValue = paid.Requests, srpISK(paid.Paid)
		}
		for _, ch := range characters {
			if ch.LinkState == module.LinkStateOK && module.CharacterHasScope(ch, module.MailSendScope) {
				cv.Mailers = append(cv.Mailers, module.NamedChoice{ID: ch.CharacterID, Name: ch.Name})
			}
		}
	} else if slices.ContainsFunc(policies, func(p db.SrpPolicy) bool { return p.LedgerVisibility == srpLedgerMembers }) {
		cv.Ledger = app.srpMemberLedger(ctx, owner, ops)
	}
	app.deps.Render(ctx, w, http.StatusOK, "srp.html", data)
}

// srpMemberLedger is the answered requests a member can read: filed
// under a policy that opens its ledger to members, on a fleet they can
// see.
func (app *mod) srpMemberLedger(ctx context.Context, owner standing.Owner, ops map[int64]db.Op) []srpRow {
	rows, err := app.queries.ListSRPMemberLedger(ctx, db.ListSRPMemberLedgerParams{OwnerKind: owner.Kind, OwnerID: owner.ID})
	if err != nil {
		logging.Errorf("srp: ledger of %s: %v", owner.Key(), err)
		return nil
	}
	ids := make([]int64, 0, len(rows))
	for _, req := range rows {
		if !slices.Contains(ids, req.OpID) {
			ids = append(ids, req.OpID)
		}
	}
	fleets, err := app.queries.ListOpsByIDs(ctx, ids)
	if err != nil {
		logging.Errorf("srp: ledger fleets of %s: %v", owner.Key(), err)
		return nil
	}
	for _, op := range fleets {
		ops[op.ID] = op
	}
	visible := app.deps.FleetsVisible(ctx, fleets)
	shown := make([]db.SrpRequest, 0, srpHandledShown)
	for _, req := range rows {
		if visible[req.OpID] && len(shown) < srpHandledShown {
			shown = append(shown, req)
		}
	}
	return app.srpRows(ctx, shown, ops)
}

// srpFormOwner is the owner a form names. A form from before the page
// had owners names a corporation.
func srpFormOwner(r *http.Request) (standing.Owner, bool) {
	if owner, ok := standing.ParseAnyOwner(r.Form.Get("owner")); ok {
		return owner, true
	}
	if id, err := strconv.ParseInt(r.Form.Get("corporation"), 10, 64); err == nil && id > 0 {
		return standing.CorporationOwner(id), true
	}
	return standing.Owner{}, false
}

// srpBack answers an SRP form by sending the account back to the
// owner's page it was sent from, with a word.
func (app *mod) srpBack(w http.ResponseWriter, r *http.Request) func(string) {
	_ = r.ParseForm()
	path := srpPath
	if owner, ok := srpFormOwner(r); ok {
		path = standing.Address(srpPath, owner.Key())
	}
	return app.deps.FlashBack(w, r, path)
}

// srpOffer fills in the losses an account can ask an owner about: those
// that fell inside one of its fleets, that its policy covers, and that
// nobody has asked about yet.
func (app *mod) srpOffer(ctx context.Context, view *srpView, characters []db.Character, owner standing.Owner, policies []db.SrpPolicy, now time.Time) {
	ids := make([]int64, 0, len(characters))
	for _, ch := range characters {
		ids = append(ids, ch.CharacterID)
	}
	losses := app.srpCandidates(ctx, characters, ids, now.Add(-srpLossReach), now, now)
	if owner.Kind == standing.OwnerCorporation {
		losses = slices.DeleteFunc(losses, func(l srpLoss) bool { return l.km.Victim.CorporationID != owner.ID })
	}
	if len(losses) == 0 {
		return
	}
	killIDs := make([]int64, 0, len(losses))
	for _, loss := range losses {
		killIDs = append(killIDs, loss.km.KillmailID)
	}
	claimed := map[int64]bool{}
	if rows, err := app.queries.ListSRPClaimedKillmails(ctx, killIDs); err == nil {
		for _, id := range rows {
			claimed[id] = true
		}
	}
	// Losses are newest first, so this is the stretch they cover.
	ops, err := app.queries.ListFleetsForOwnerNewest(ctx, db.ListFleetsForOwnerNewestParams{
		OwnerKind: owner.Kind, OwnerID: owner.ID,
		StartsBefore: losses[0].at.Add(module.OpCaptureLead + time.Second),
		EndsAfter:    losses[len(losses)-1].at.Add(-module.OpCaptureTail),
		RowLimit:     srpFleetsRead,
	})
	if err != nil {
		logging.Errorf("srp: fleets for losses: %v", err)
		return
	}
	opIDs := make([]int64, 0, len(ops))
	for _, op := range ops {
		opIDs = append(opIDs, op.ID)
	}
	seen, err := app.queries.ListOpAttendanceForPilots(ctx, db.ListOpAttendanceForPilotsParams{OpIds: opIDs, CharacterIds: ids})
	if err != nil {
		logging.Errorf("srp: attendance for losses: %v", err)
		return
	}
	sightings := make(map[[2]int64]db.ListOpAttendanceForPilotsRow, len(seen))
	for _, row := range seen {
		sightings[[2]int64{row.OpID, row.CharacterID}] = row
	}
	visible := app.deps.FleetsVisible(ctx, ops)
	for _, loss := range losses {
		if claimed[loss.km.KillmailID] {
			view.Claimed++
			continue
		}
		during, offered := false, false
		for _, op := range ops {
			if !srpDuring(op, loss.at) {
				continue
			}
			row, on := sightings[[2]int64{op.ID, loss.km.Victim.CharacterID}]
			attended := on && srpAttendedAt(row.Source, row.FirstSeenAt, row.LastSeenAt, loss.at)
			if !visible[op.ID] && !attended {
				continue // a fleet kept from this pilot is not mentioned
			}
			during = true
			if srpRefusal(op, srpPolicyFor(op, policies), loss, visible[op.ID], attended, app.srpInDoctrine(ctx, op, loss.km.Victim.ShipTypeID), now) != "" {
				continue
			}
			offered = true
			view.Losses = append(view.Losses, srpLossChoice{
				Value: fmt.Sprintf("%d:%d", loss.km.KillmailID, op.ID),
				Label: app.srpLossLabel(ctx, loss) + " · fleet: " + op.Title,
			})
		}
		switch {
		case during && !offered:
			view.Refused++
		case !during && owner.Kind == standing.OwnerCorporation:
			view.Outside++
		}
	}
}

// handleSRPRequest files a request (POST /srp/request). Everything it
// stores about the loss is read from the stored killmail, not from the
// form: the form only says which killmail and which fleet.
func (app *mod) handleSRPRequest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.deps.UserID(ctx)
	back := app.srpBack(w, r)
	_ = r.ParseForm()
	rawKill, rawOp, _ := strings.Cut(r.Form.Get("loss"), ":")
	killID, _ := strconv.ParseInt(rawKill, 10, 64)
	opID, _ := strconv.ParseInt(rawOp, 10, 64)
	op, err := app.queries.GetOp(ctx, opID)
	if err != nil {
		back("That loss was not during that fleet.")
		return
	}
	mine := func(id int64) bool {
		ch, err := app.queries.GetCharacter(ctx, id)
		return err == nil && ch.UserID == userID
	}
	note := humanize.Clip(strings.TrimSpace(r.Form.Get("note")), srpNoteMax)
	message, _ := app.fileClaim(ctx, op, killID, userID, mine, note, time.Now().UTC())
	back(message)
}

// handleSRPWithdraw takes back a request nobody has answered
// (POST /srp/{id}/withdraw).
func (app *mod) handleSRPWithdraw(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	back := app.srpBack(w, r)
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	n, err := app.queries.WithdrawSRPRequest(ctx, db.WithdrawSRPRequestParams{ID: id, UserID: app.deps.UserID(ctx)})
	if err != nil || n == 0 {
		back("That request could not be withdrawn: it is not yours, or it has been answered.")
		return
	}
	back("Request withdrawn.")
}

// permSRP is the corporation permission that lets an account handle ship
// replacement requests (core's access model names it "srp").
const permSRP = "srp"

// timeSet is a valid timestamp for a nullable column.
func timeSet(t time.Time) sql.NullTime { return sql.NullTime{Time: t, Valid: true} }
