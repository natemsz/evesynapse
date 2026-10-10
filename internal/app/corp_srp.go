package app

import (
	"context"
	"database/sql"
	"encoding/json"
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
// Ship replacement (SRP).
//
// A pilot who lost a ship on one of the corporation's ops asks for it
// to be replaced. A request rests on two things EveSynapse already
// holds: the killmail (the pilot's own loss, read from EVE) and the
// op it happened on (ops.go). The loss has to fall inside the op's
// time; whether the fleet recorded the pilot on that op
// (ops_attendance.go) is shown to whoever handles the request, read
// fresh each time, because attendance can be ticked by hand later.
//
// Requests are handled by the corporation's directors and CEO, and by
// whoever they hand it to (corp_permissions.go). Handling is marking a
// request paid, with the amount, or denying it with a reason. EVE has
// no call that moves ISK, so the paying is done in game; the pay list
// mail (corp_srp_handle.go) puts what is owed in the handler's inbox
// there.
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
)

// srpLoss is one of an account's recent losses.
type srpLoss struct {
	km   esi.Killmail
	hash string
	at   time.Time
}

// srpLosses lists the losses of an account's characters inside the
// claim window, newest first, from stored killmails only.
func (app *Application) srpLosses(ctx context.Context, characters []db.Character, now time.Time) []srpLoss {
	var ids, mine []int64
	for _, ch := range characters {
		var refs []esi.KillmailRef
		if !app.loadCorpSnapshot(ctx, ch.CharacterID, esi.SnapKillmails, &refs) {
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
		if err != nil || now.Sub(at) > srpClaimWindow {
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
	return !op.CancelledAt.Valid && !at.Before(op.StartsAt.Add(-opCaptureLead)) && !at.After(opEnd(op).Add(opCaptureTail))
}

// srpLossValue estimates what a loss cost the pilot: the hull and
// everything fitted or carried, dropped or destroyed, at CCP's average
// prices. Whatever has no price counts for nothing.
func srpLossValue(km esi.Killmail, prices map[int64]esi.MarketPrice) float64 {
	total := prices[km.Victim.ShipTypeID].AveragePrice
	for _, it := range km.Victim.Items {
		if qty := it.QuantityDestroyed + it.QuantityDropped; qty > 0 {
			total += float64(qty) * prices[it.ItemTypeID].AveragePrice
		}
	}
	return total
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
	if err != nil || v < 0 || v*scale > 1e13 {
		return 0, false
	}
	return v * scale, true
}

// srpPilot names a pilot: by the linked character's name, else however
// the character is known.
func (app *Application) srpPilot(ctx context.Context, characterID int64) string {
	if ch, err := app.queries.GetCharacter(ctx, characterID); err == nil && ch.Name != "" {
		return ch.Name
	}
	return app.displayCharacter(ctx, characterID)
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
}

// srpCorpView is one corporation's part of the page.
type srpCorpView struct {
	ID      int64
	Name    string
	Policy  string
	Handles bool // may answer its requests
	Directs bool // may say who else handles them

	Open      []srpRow
	Handled   []srpRow
	OpenValue string
	PaidCount int64
	PaidValue string
	Mailers   []opChoice // the account's characters that can send the EVE mail
	Who       []permissionChoice
}

type srpView struct {
	Member  bool // the account has a character in a corporation
	Losses  []srpLossChoice
	Outside int // recent losses that were not during an op
	Claimed int // recent losses already asked for
	Mine    []srpRow
	Corps   []srpCorpView
}

// srpRows builds the page's rows for a list of requests. Attendance is
// looked up for the open ones only: it no longer matters for the rest.
func (app *Application) srpRows(ctx context.Context, requests []db.SrpRequest, ops map[int64]db.Op) []srpRow {
	rows := make([]srpRow, 0, len(requests))
	for _, req := range requests {
		op, known := ops[req.OpID]
		if !known {
			op, _ = app.queries.GetOp(ctx, req.OpID)
			ops[req.OpID] = op
		}
		row := srpRow{
			ID: req.ID, Pilot: app.srpPilot(ctx, req.CharacterID), PilotID: req.CharacterID,
			Ship: app.typeNameOrID(ctx, req.ShipTypeID), ShipTypeID: req.ShipTypeID,
			System: app.locationTitle(ctx, req.SolarSystemID, "solar_system"),
			LostAt: req.LostAt.UTC().Format("2006-01-02 15:04"),
			Op:     op.Title, OpURL: opURL(req.OpID), KillmailID: req.KillmailID,
			Value: srpISK(req.LossValue), Note: req.Note,
			Open: req.Status == srpOpen, Paid: req.Status == srpPaid,
			Payout: srpISK(req.Payout), HandlerNote: req.HandlerNote,
		}
		if req.LossValue > 0 {
			row.Suggested = esi.FormatInt(int64(req.LossValue + 0.5))
		}
		if req.HandledAt.Valid {
			row.HandledAt = req.HandledAt.Time.UTC().Format("2006-01-02 15:04")
			row.HandledBy = app.srpPilot(ctx, req.HandledBy)
		}
		if row.Open {
			if op.DoctrineID != 0 {
				if in, err := app.queries.DoctrineHasShip(ctx, db.DoctrineHasShipParams{DoctrineID: op.DoctrineID, ShipTypeID: req.ShipTypeID}); err == nil {
					row.Doctrine, row.DoctrineShip = op.Doctrine, in
				}
			}
			if seen, err := app.queries.GetOpAttendance(ctx, db.GetOpAttendanceParams{OpID: req.OpID, CharacterID: req.CharacterID}); err == nil {
				row.Attended = true
				row.Attendance = "Ticked present by hand."
				if seen.Source == attendanceFleet {
					row.Attendance = "Seen in the fleet."
					if seen.ShipTypeID != 0 {
						row.Attendance = "Seen in the fleet in a " + app.typeNameOrID(ctx, seen.ShipTypeID) + "."
					}
				}
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// srpCorps lists the corporations an account has a character in, in
// the order of its characters.
func (app *Application) srpCorps(ctx context.Context, userID int64) []int64 {
	rows, err := app.queries.ListCharacterCorporationsByUser(ctx, userID)
	if err != nil {
		logging.Errorf("srp: corporations of user %d: %v", userID, err)
		return nil
	}
	seen := map[int64]bool{}
	var out []int64
	for _, row := range rows {
		if row.CorporationID != 0 && !seen[row.CorporationID] {
			seen[row.CorporationID] = true
			out = append(out, row.CorporationID)
		}
	}
	return out
}

func (app *Application) handleSRP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.page(ctx)
	userID := app.userID(ctx)
	now := time.Now().UTC()
	view := &srpView{}
	data.SRP = view

	st, err := app.discordStandingFor(ctx, userID)
	if err != nil {
		logging.Errorf("srp: standing of user %d: %v", userID, err)
		data.Error = "Could not load ship replacement; check the server log."
	}
	corps := app.srpCorps(ctx, userID)
	view.Member = len(corps) > 0
	ops := map[int64]db.Op{}

	characters, _ := app.queries.ListCharactersByUser(ctx, userID)
	app.srpOffer(ctx, view, characters, now)

	if mine, err := app.queries.ListSRPRequestsByUser(ctx, db.ListSRPRequestsByUserParams{UserID: userID, RowLimit: srpMineShown}); err != nil {
		logging.Errorf("srp: requests of user %d: %v", userID, err)
	} else {
		view.Mine = app.srpRows(ctx, mine, ops)
	}

	for _, corp := range corps {
		cv := srpCorpView{ID: corp, Name: app.corpDisplayName(ctx, corp), Directs: st.directs(corp)}
		cv.Handles = app.corpPermits(ctx, st, corp, permSRP)
		if settings, err := app.queries.GetSRPSettings(ctx, corp); err == nil {
			cv.Policy = settings.Policy
		}
		if cv.Handles {
			open, err := app.queries.ListOpenSRPRequests(ctx, []int64{corp})
			if err != nil {
				logging.Errorf("srp: open requests of corporation %d: %v", corp, err)
			}
			var owed float64
			for _, req := range open {
				owed += req.LossValue
			}
			cv.Open, cv.OpenValue = app.srpRows(ctx, open, ops), srpISK(owed)
			handled, _ := app.queries.ListHandledSRPRequests(ctx, db.ListHandledSRPRequestsParams{CorporationID: corp, RowLimit: srpHandledShown})
			cv.Handled = app.srpRows(ctx, handled, ops)
			if paid, err := app.queries.SumSRPPaid(ctx, db.SumSRPPaidParams{CorporationID: corp, Since: timeSet(now.AddDate(0, 0, -srpPaidStatDays))}); err == nil {
				cv.PaidCount, cv.PaidValue = paid.Requests, srpISK(paid.Paid)
			}
			for _, ch := range characters {
				if ch.LinkState == linkStateOK && characterHasScope(ch, mailSendScope) {
					cv.Mailers = append(cv.Mailers, opChoice{ID: ch.CharacterID, Name: ch.Name})
				}
			}
		}
		if cv.Directs {
			cv.Who = app.permissionChoices(ctx, corp, permSRP)
		}
		view.Corps = append(view.Corps, cv)
	}
	app.render(ctx, w, http.StatusOK, "srp.html", data)
}

// srpOffer fills in the losses an account can ask about: those that
// fell inside an op of the corporation the pilot was in, and that
// nobody has asked about yet.
func (app *Application) srpOffer(ctx context.Context, view *srpView, characters []db.Character, now time.Time) {
	losses := app.srpLosses(ctx, characters, now)
	if len(losses) == 0 {
		return
	}
	ids := make([]int64, 0, len(losses))
	seenCorp := map[int64]bool{}
	var corps []int64
	for _, loss := range losses {
		ids = append(ids, loss.km.KillmailID)
		if corp := loss.km.Victim.CorporationID; corp != 0 && !seenCorp[corp] {
			seenCorp[corp] = true
			corps = append(corps, corp)
		}
	}
	claimed := map[int64]bool{}
	if rows, err := app.queries.ListSRPClaimedKillmails(ctx, ids); err == nil {
		for _, id := range rows {
			claimed[id] = true
		}
	}
	ops, err := app.queries.ListOpsForCorporationsBetween(ctx, db.ListOpsForCorporationsBetweenParams{
		CorporationIds: corps,
		FromTime:       now.Add(-srpClaimWindow - opMaxDuration*time.Minute - opCaptureTail),
		ToTime:         now.Add(opCaptureLead),
	})
	if err != nil {
		logging.Errorf("srp: ops for losses: %v", err)
		return
	}
	for _, loss := range losses {
		if claimed[loss.km.KillmailID] {
			view.Claimed++
			continue
		}
		during := false
		for _, op := range ops {
			if op.CorporationID != loss.km.Victim.CorporationID || !srpDuring(op, loss.at) {
				continue
			}
			during = true
			view.Losses = append(view.Losses, srpLossChoice{
				Value: fmt.Sprintf("%d:%d", loss.km.KillmailID, op.ID),
				Label: fmt.Sprintf("%s · %s · %s · %s · op: %s",
					loss.at.UTC().Format("Jan 2 15:04"), app.typeNameOrID(ctx, loss.km.Victim.ShipTypeID),
					app.locationTitle(ctx, loss.km.SolarSystemID, "solar_system"),
					app.srpPilot(ctx, loss.km.Victim.CharacterID), op.Title),
			})
		}
		if !during {
			view.Outside++
		}
	}
}

// handleSRPRequest files a request (POST /srp/request). Everything it
// stores about the loss is read from the stored killmail, not from the
// form: the form only says which killmail and which op.
func (app *Application) handleSRPRequest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	back := app.flashBack(w, r, srpPath)
	_ = r.ParseForm()
	now := time.Now().UTC()
	rawKill, rawOp, _ := strings.Cut(r.Form.Get("loss"), ":")
	killID, _ := strconv.ParseInt(rawKill, 10, 64)
	opID, _ := strconv.ParseInt(rawOp, 10, 64)

	stored, err := app.queries.GetKillmailDetail(ctx, killID)
	var km esi.Killmail
	if err != nil || json.Unmarshal([]byte(stored.Payload), &km) != nil {
		back("Pick the loss to claim.")
		return
	}
	pilot, err := app.queries.GetCharacter(ctx, km.Victim.CharacterID)
	if err != nil || pilot.UserID != userID {
		back("That loss is not one of your characters'.")
		return
	}
	at, err := time.Parse(time.RFC3339, km.KillmailTime)
	if err != nil || now.Sub(at) > srpClaimWindow {
		back("That loss is too old to claim.")
		return
	}
	op, err := app.queries.GetOp(ctx, opID)
	if err != nil || op.CorporationID != km.Victim.CorporationID || !srpDuring(op, at) {
		back("That loss was not during that op.")
		return
	}
	id, err := app.queries.CreateSRPRequest(ctx, db.CreateSRPRequestParams{
		CorporationID: op.CorporationID, OpID: op.ID, KillmailID: km.KillmailID, KillmailHash: stored.Hash,
		CharacterID: pilot.CharacterID, UserID: userID, ShipTypeID: km.Victim.ShipTypeID, SolarSystemID: km.SolarSystemID,
		LostAt: at, LossValue: srpLossValue(km, app.valuationPrices(ctx)),
		Note: clip(strings.TrimSpace(r.Form.Get("note")), srpNoteMax), CreatedAt: now,
	})
	if errors.Is(err, sql.ErrNoRows) {
		back("That loss has already been claimed.")
		return
	}
	if err != nil {
		logging.Errorf("srp: save request for killmail %d: %v", km.KillmailID, err)
		back("The request could not be saved; check the server log.")
		return
	}
	logging.Infof("srp: user %d asked for killmail %d on op %d (request %d)", userID, km.KillmailID, op.ID, id)
	back("Request sent. You will be told here when it is answered.")
}

// handleSRPWithdraw takes back a request nobody has answered
// (POST /srp/{id}/withdraw).
func (app *Application) handleSRPWithdraw(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	back := app.flashBack(w, r, srpPath)
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	n, err := app.queries.WithdrawSRPRequest(ctx, db.WithdrawSRPRequestParams{ID: id, UserID: app.userID(ctx)})
	if err != nil || n == 0 {
		back("That request could not be withdrawn: it is not yours, or it has been answered.")
		return
	}
	back("Request withdrawn.")
}
