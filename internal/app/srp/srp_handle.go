package srp

import (
	"context"
	"evesynapse/internal/app/module"
	"evesynapse/internal/humanize"
	"fmt"
	"html"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
	"evesynapse/internal/standing"
)

// ---------------------------------------------------------------------------
// Ship replacement: answering requests, and what goes to the game.
//
// EVE's API cannot give ISK, so paying stays in game. Two EVE mails
// make that easier, each sent from the handler's own character with
// the mail access it already granted:
//
//   - the pay list, to the handler's own inbox: every open request on
//     one page, each pilot's name a link that opens their information
//     window (from which ISK is given) and each loss a link to its
//     kill report;
//   - a note to the pilot when a request is marked paid, if the
//     handler leaves that ticked.
//
// Neither is allowed to cost the sender anything: EVE's charge for
// mailing a stranger is approved at zero, so a mail that would cost
// ISK is refused by EVE and reported here instead.
// ---------------------------------------------------------------------------

// srpPayListMost bounds the pay list so the mail stays inside EVE's
// size limit for one.
const (
	srpPayListMost  = 30
	srpNotifyWindow = 14 * 24 * time.Hour
	// eveCharacterType is a character type id, which an in-game
	// "show info" link needs beside the character's id.
	eveCharacterType = 1377
)

// srpHandler reports whether the account making a request may answer a
// corporation's requests.
func (app *mod) srpHandler(r *http.Request, corp int64) (standing.Standing, bool) {
	ctx := r.Context()
	st, err := app.deps.StandingFor(ctx, app.deps.UserID(ctx))
	return st, err == nil && corp != 0 && app.deps.CorpPermits(ctx, st, corp, permSRP)
}

// handleSRPDecide answers a request (POST /srp/{id}/decide): paid with
// an amount, denied with a reason, or opened again.
func (app *mod) handleSRPDecide(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.deps.UserID(ctx)
	back := app.srpBack(w, r)
	_ = r.ParseForm()
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	req, err := app.queries.GetSRPRequest(ctx, id)
	if err != nil {
		back("That request is not yours to handle.")
		return
	}
	if _, ok := app.srpHandler(r, req.CorporationID); !ok {
		back("That request is not yours to handle.")
		return
	}
	actor := app.deps.CorpActorFor(ctx, userID, req.CorporationID)
	note := humanize.Clip(strings.TrimSpace(r.Form.Get("note")), srpNoteMax)
	decide := func(status string, payout float64) bool {
		n, err := app.queries.DecideSRPRequest(ctx, db.DecideSRPRequestParams{
			ID: id, Status: status, Payout: payout, HandledBy: actor.CharacterID, HandlerNote: note, HandledAt: timeSet(time.Now().UTC()),
		})
		if err != nil {
			logging.Errorf("srp: answer request %d: %v", id, err)
		}
		if err != nil || n == 0 {
			back("Somebody has already answered that request.")
			return false
		}
		logging.Infof("srp: user %d marked request %d %s (%.0f ISK)", userID, id, status, payout)
		return true
	}

	switch r.Form.Get("action") {
	case "reopen":
		if n, err := app.queries.ReopenSRPRequest(ctx, id); err != nil || n == 0 {
			back("That request is open already.")
			return
		}
		logging.Infof("srp: user %d reopened request %d", userID, id)
		back("Request opened again.")
	case "deny":
		if note == "" {
			back("Say why it is denied, so the pilot knows.")
			return
		}
		if decide(srpDenied, 0) {
			back("Request denied.")
		}
	case "paid":
		payout, ok := parseISK(r.Form.Get("payout"))
		if !ok || payout <= 0 {
			back("Enter the amount paid, in ISK.")
			return
		}
		if !decide(srpPaid, payout) {
			return
		}
		message := "Marked paid: " + srpISK(payout) + " to " + app.srpPilot(ctx, req.CharacterID) + "."
		if r.Form.Get("mail") == "1" && req.UserID != userID {
			req.Payout, req.HandlerNote = payout, note
			message += " " + app.srpMailPaid(ctx, actor, req)
		}
		back(message)
	default:
		back("Nothing was changed.")
	}
}

// srpKillLink is an in-game link to a loss's kill report; plain words
// where the killmail's hash is not known.
func srpKillLink(req db.SrpRequest, text string) string {
	if req.KillmailHash == "" {
		return text
	}
	return fmt.Sprintf(`<a href="killReport:%d:%s">%s</a>`, req.KillmailID, html.EscapeString(req.KillmailHash), text)
}

// srpMailPaid tells the pilot in game that a request was paid, and
// says in a sentence how that went.
func (app *mod) srpMailPaid(ctx context.Context, actor module.CorpActor, req db.SrpRequest) string {
	if actor.Mailer == nil {
		return "No EVE mail was sent: none of your characters in the corporation has granted mail access."
	}
	ship := app.deps.TypeNameOrID(ctx, req.ShipTypeID)
	lines := []string{
		"Your ship replacement request was paid.",
		"",
		"Ship: " + html.EscapeString(ship) + " (" + srpKillLink(req, "kill report") + ")",
		"Lost: " + req.LostAt.UTC().Format("2006-01-02 15:04") + " EVE time in " + html.EscapeString(app.deps.LocationTitle(ctx, req.SolarSystemID, "solar_system")),
	}
	if op, err := app.queries.GetOp(ctx, req.OpID); err == nil {
		lines = append(lines, "Op: "+html.EscapeString(op.Title))
	}
	lines = append(lines, "Paid: "+srpISK(req.Payout))
	if req.HandlerNote != "" {
		lines = append(lines, "Note: "+html.EscapeString(req.HandlerNote))
	}
	lines = append(lines, "", "Sent through EveSynapse by "+html.EscapeString(actor.Mailer.Name)+".")
	err := app.deps.SendEVEMail(ctx, *actor.Mailer, req.CharacterID, "character", "SRP paid: "+ship, strings.Join(lines, "<br>"), 0)
	if err != nil {
		logging.Errorf("srp: paid mail for request %d: %v", req.ID, err)
		return "The EVE mail to the pilot was not sent" + app.deps.ESIRefusalDetail(err) + "."
	}
	return "The pilot was told by EVE mail."
}

// handleSRPPayList mails a corporation's open requests to one of the
// handler's own characters in game (POST /srp/paylist).
func (app *mod) handleSRPPayList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.deps.UserID(ctx)
	back := app.srpBack(w, r)
	_ = r.ParseForm()
	corp, _ := strconv.ParseInt(r.Form.Get("corporation"), 10, 64)
	if _, ok := app.srpHandler(r, corp); !ok {
		back("Those requests are not yours to handle.")
		return
	}
	mailerID, _ := strconv.ParseInt(r.Form.Get("character"), 10, 64)
	mailer, err := app.queries.GetCharacter(ctx, mailerID)
	if err != nil || mailer.UserID != userID || mailer.LinkState != module.LinkStateOK || !module.CharacterHasScope(mailer, module.MailSendScope) {
		back("Pick one of your characters that has granted mail access.")
		return
	}
	open, err := app.queries.ListOpenSRPRequests(ctx, []int64{corp})
	if err != nil || len(open) == 0 {
		back("There is nothing open to pay.")
		return
	}
	left := 0
	if len(open) > srpPayListMost {
		left, open = len(open)-srpPayListMost, open[:srpPayListMost]
	}
	name := app.deps.CorpDisplayName(ctx, corp)
	lines := []string{
		"Open ship replacement requests for " + html.EscapeString(name) + ".",
		"Open a pilot's name to give ISK, then mark each request paid at " + app.deps.SiteURL(srpPath),
		"",
	}
	ops := map[int64]db.Op{}
	for _, req := range open {
		op, known := ops[req.OpID]
		if !known {
			op, _ = app.queries.GetOp(ctx, req.OpID)
			ops[req.OpID] = op
		}
		amount := srpISK(req.LossValue)
		if amount == "" {
			amount = "no estimate"
		}
		lines = append(lines, fmt.Sprintf(`<a href="showinfo:%d//%d">%s</a>: %s, %s, op %s on %s`,
			eveCharacterType, req.CharacterID, html.EscapeString(app.srpPilot(ctx, req.CharacterID)), amount,
			srpKillLink(req, html.EscapeString(app.deps.TypeNameOrID(ctx, req.ShipTypeID))),
			html.EscapeString(op.Title), req.LostAt.UTC().Format("Jan 2")))
	}
	if left > 0 {
		lines = append(lines, "", "And "+humanize.Plural(left, "more request")+" on the site.")
	}
	subject := "SRP pay list: " + name + " (" + humanize.Plural(len(open), "request") + ")"
	if err := app.deps.SendEVEMail(ctx, mailer, mailer.CharacterID, "character", subject, strings.Join(lines, "<br>"), 0); err != nil {
		logging.Errorf("srp: pay list mail for corporation %d: %v", corp, err)
		back("The pay list was not sent" + app.deps.ESIRefusalDetail(err) + ".")
		return
	}
	logging.Infof("srp: user %d mailed the pay list of corporation %d to character %d", userID, corp, mailer.CharacterID)
	back("The pay list is in " + mailer.Name + "'s EVE mail: " + humanize.Plural(len(open), "request") + ".")
}
