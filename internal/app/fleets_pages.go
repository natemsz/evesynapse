package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
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
// The Fleets pages around a single fleet: the board (/fleets/), history
// (/fleets/history/), the calendar's view of them and the .ics file.
// All of them read stored rows, a bounded number per render.
// ---------------------------------------------------------------------------

// How many fleets one render reads from each source.
const (
	fleetsFromOwners  = 60
	fleetsOfCharacter = 20
	fleetsPublic      = 20
	fleetsPast        = 30
	fleetsAhead       = 45 * 24 * time.Hour
)

// fleetBoardRow is one line of the board.
type fleetBoardRow struct {
	Href      string
	Title     string
	Sub       string
	Day       string
	Clock     string
	WhenRaw   string
	Count     string
	Ahead     bool
	Live      bool
	Cancelled bool
	Answer    string
}

// fleetsView is the /fleets/ page.
type fleetsView struct {
	Level     string
	PilotName string
	SignIn    string
	Account   string
	CanPlan   bool
	Live      []fleetBoardRow
	Yours     []fleetBoardRow
	Upcoming  []fleetBoardRow
	Public    []fleetBoardRow
	Total     int
}

// visibleFleets gathers the fleets a viewer can see in a span: their
// owners' first, then their own, then public ones, each once.
func (app *Application) visibleFleets(ctx context.Context, v fleetViewer, endsAfter, startsBefore time.Time, withPublic bool) ([]db.Op, map[int64]fleetFacts, map[int64]bool) {
	var ops []db.Op
	seen := map[int64]bool{}
	mine := map[int64]bool{}
	add := func(rows []db.Op, isMine bool) {
		for _, op := range rows {
			if isMine {
				mine[op.ID] = true
			}
			if !seen[op.ID] {
				seen[op.ID] = true
				ops = append(ops, op)
			}
		}
	}
	if owners := v.owners(); len(owners) > 0 {
		kinds, ids := ownerArrays(owners)
		rows, err := app.queries.ListFleetsForOwners(ctx, db.ListFleetsForOwnersParams{
			OwnerKinds: kinds, OwnerIds: ids, StartsBefore: startsBefore, EndsAfter: endsAfter, RowLimit: fleetsFromOwners,
		})
		if err != nil {
			logging.Errorf("fleets: fleets of owners: %v", err)
		}
		add(rows, false)
	}
	if characters := v.characterIDs(); len(characters) > 0 {
		rows, err := app.queries.ListFleetsForCharacters(ctx, db.ListFleetsForCharactersParams{
			CharacterIds: characters, StartsBefore: startsBefore, EndsAfter: endsAfter, RowLimit: fleetsOfCharacter,
		})
		if err != nil {
			logging.Errorf("fleets: fleets of characters: %v", err)
		}
		add(rows, true)
	}
	if withPublic {
		rows, err := app.queries.ListPublicFleets(ctx, db.ListPublicFleetsParams{
			StartsBefore: startsBefore, EndsAfter: endsAfter, RowLimit: fleetsPublic,
		})
		if err != nil {
			logging.Errorf("fleets: public fleets: %v", err)
		}
		add(rows, false)
	}
	facts := app.fleetFactsFor(ctx, v, ops)
	visible := ops[:0]
	for _, op := range ops {
		if v.access(op, facts[op.ID]).View {
			visible = append(visible, op)
		}
	}
	return visible, facts, mine
}

// handleFleets is the Fleets landing page: what is live, what the
// viewer is on, what is coming up, and public fleets anyone can join.
func (app *Application) handleFleets(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	v := app.fleetViewerFor(ctx)
	now := time.Now().UTC()
	view := &fleetsView{
		Level: v.level(), SignIn: "/auth/pilot?next=" + fleetsPath, Account: "/auth/link?from=cards",
		CanPlan: len(v.planOwners()) > 0,
	}
	if v.Pilot != nil {
		view.PilotName = v.Pilot.Name
	}
	ops, _, mine := app.visibleFleets(ctx, v, now, now.Add(fleetsAhead), true)
	answers := app.fleetAnswers(ctx, v, ops)
	names := map[string]string{}
	belongs := map[string]bool{}
	for _, owner := range v.owners() {
		belongs[owner.Key()] = true
	}
	for _, op := range ops {
		owner := opOwner(op)
		name, known := names[owner.Key()]
		if !known {
			name = app.ownerName(ctx, owner)
			names[owner.Key()] = name
		}
		start := op.StartsAt.UTC()
		row := fleetBoardRow{
			Href: opURL(op.ID), Title: op.Title, Sub: fleetRowSub(op, name),
			Day: start.Format("Mon Jan 2"), Clock: start.Format("15:04"), WhenRaw: start.Format(time.RFC3339),
			Count: fleetCountText(op), Ahead: op.StartsAt.After(now),
			Live:      !op.CancelledAt.Valid && !op.StartsAt.After(now),
			Cancelled: op.CancelledAt.Valid,
			Answer:    answerLabel(answers[op.ID]),
		}
		switch {
		case row.Live:
			// Who is on a running fleet stays with the pilots in it, so
			// the board says how long it has run instead of a count.
			row.Count = "Running for " + fleetRunningText(now.Sub(op.StartsAt))
			view.Live = append(view.Live, row)
		case mine[op.ID]:
			view.Yours = append(view.Yours, row)
		case belongs[owner.Key()]:
			view.Upcoming = append(view.Upcoming, row)
		default:
			view.Public = append(view.Public, row)
		}
		view.Total++
	}
	data := pageData{ShellData: app.PublicShell(ctx), Fleets: view}
	data.HeadMeta = module.RenderPageMeta(module.PageMeta{
		Title: "Fleets", CanonicalPath: fleetsPath,
		Description: "Plan EVE Online fleets, share them with a link, and see public fleets anyone can join.",
	})
	app.render(ctx, w, http.StatusOK, "fleets.html", data)
}

// fleetAnswers is the viewer's best answer on each fleet: "yes"
// outranks "maybe", which outranks "no".
func (app *Application) fleetAnswers(ctx context.Context, v fleetViewer, ops []db.Op) map[int64]string {
	out := map[int64]string{}
	characters := v.characterIDs()
	if len(characters) == 0 || len(ops) == 0 {
		return out
	}
	ids := make([]int64, len(ops))
	for i, op := range ops {
		ids[i] = op.ID
	}
	rows, err := app.queries.ListSignupsOfCharacters(ctx, db.ListSignupsOfCharactersParams{OpIds: ids, CharacterIds: characters})
	if err != nil {
		logging.Errorf("fleets: answers of viewer: %v", err)
		return out
	}
	rank := map[string]int{opNo: 1, opMaybe: 2, opYes: 3}
	for _, row := range rows {
		if rank[row.Response] > rank[out[row.OpID]] {
			out[row.OpID] = row.Response
		}
	}
	return out
}

// fleetRowSub is a board row's second line: whose fleet it is, its kind
// where the owner does not already say it, and whether it is private.
func fleetRowSub(op db.Op, owner string) string {
	var parts []string
	if owner != "" {
		parts = append(parts, owner)
	}
	if op.FleetType != defaultFleetType(opOwner(op)) {
		parts = append(parts, fleetOptionLabel(fleetTypeOptions, op.FleetType))
	}
	if op.Visibility == fleetRestricted {
		parts = append(parts, "private")
	}
	return strings.Join(parts, ", ")
}

// fleetCountText says how many are coming, from the stored counts.
func fleetCountText(op db.Op) string {
	if op.MaybeCount > 0 {
		return fmt.Sprintf("%d coming, %d maybe", op.ComingCount, op.MaybeCount)
	}
	return fmt.Sprintf("%d coming", op.ComingCount)
}

// pastFleetCount says how a past fleet went, for its history row: who
// flew and what its report counted, else its sign-ups.
func pastFleetCount(op db.Op, out db.ListFleetOutcomesRow) string {
	var parts []string
	if out.Flew > 0 {
		parts = append(parts, fmt.Sprintf("%d flew", out.Flew))
	} else if n := op.ComingCount + op.MaybeCount; n > 0 {
		parts = append(parts, fmt.Sprintf("%d signed up", n))
	}
	if out.Kills > 0 || out.Losses > 0 {
		losses := "losses"
		if out.Losses == 1 {
			losses = "loss"
		}
		kills := "kills"
		if out.Kills == 1 {
			kills = "kill"
		}
		parts = append(parts, fmt.Sprintf("%d %s, %d %s", out.Kills, kills, out.Losses, losses))
	}
	if len(parts) == 0 {
		return "No sign-ups"
	}
	return strings.Join(parts, ", ")
}

func answerLabel(response string) string {
	switch response {
	case opYes:
		return "Coming"
	case opMaybe:
		return "Maybe"
	case opNo:
		return "Not coming"
	}
	return ""
}

// CalendarFleets lists the fleets an account can see in a span, for the
// calendar, with the account's own answer on each.
func (app *Application) CalendarFleets(ctx context.Context, userID int64, from, to time.Time) []module.CalendarFleet {
	v := app.accountFleetViewer(ctx, userID)
	ops, _, _ := app.visibleFleets(ctx, v, from, to, true)
	answers := app.fleetAnswers(ctx, v, ops)
	out := make([]module.CalendarFleet, 0, len(ops))
	for _, op := range ops {
		out = append(out, module.CalendarFleet{
			ID: op.ID, Title: op.Title, StartsAt: op.StartsAt, Cancelled: op.CancelledAt.Valid, Answer: answers[op.ID],
		})
	}
	return out
}

// CanPlanFleets reports whether an account can plan a fleet for any
// owner.
func (app *Application) CanPlanFleets(ctx context.Context, userID int64) bool {
	return len(app.accountFleetViewer(ctx, userID).planOwners()) > 0
}

// ---------------------------------------------------------------------------
// History: past fleets and how often each pilot attended.
// ---------------------------------------------------------------------------

const (
	papWindow = 90 * 24 * time.Hour
	papRecent = 30 * 24 * time.Hour
)

type papRow struct {
	CharacterID int64
	Name        string
	Recent      int64 // fleets attended in the last 30 days
	Total       int64 // in the last 90
	LastOp      string
}

// historyOwner is one owner in the history page's picker.
// papsView is the /fleets/history/ page, for one owner at a time.
type papsView struct {
	Owners []module.OwnerOption
	Owner  string
	Past   []fleetBoardRow
	Rows   []papRow
}

func (app *Application) handleFleetHistory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	v := app.fleetViewerFor(ctx)
	if v.UserID == 0 {
		app.flash(ctx, "Fleet history needs an EveSynapse account.")
		http.Redirect(w, r, fleetsPath, http.StatusSeeOther)
		return
	}
	now := time.Now().UTC()
	view := &papsView{}
	owners := v.owners()
	if len(owners) > 0 {
		owner := owners[0]
		if want, ok := parseAnyOwner(r.URL.Query().Get("owner")); ok && standing.Includes(owners, want) {
			owner = want
		}
		for _, o := range owners {
			view.Owners = append(view.Owners, module.OwnerOption{
				ID: o.ID, Name: fleetOwnerLabel(o, app.ownerName(ctx, o)), Active: o == owner, Param: "owner", Value: o.Key(),
			})
		}
		view.Owner = app.ownerName(ctx, owner)
		app.fillFleetHistory(ctx, v, view, owner, now)
	}
	app.render(ctx, w, http.StatusOK, "fleet_history.html", pageData{ShellData: app.PublicShell(ctx), PAPs: view})
}

func (app *Application) fillFleetHistory(ctx context.Context, v fleetViewer, view *papsView, owner orgOwner, now time.Time) {
	past, err := app.queries.ListPastFleetsForOwners(ctx, db.ListPastFleetsForOwnersParams{
		OwnerKinds: []string{owner.Kind}, OwnerIds: []int64{owner.ID}, EndedBefore: now, RowLimit: fleetsPast,
	})
	if err != nil {
		logging.Errorf("fleets: past fleets of %s: %v", owner.Key(), err)
	}
	facts := app.fleetFactsFor(ctx, v, past)
	ids := make([]int64, 0, len(past))
	for _, op := range past {
		ids = append(ids, op.ID)
	}
	outcomes := map[int64]db.ListFleetOutcomesRow{}
	if len(ids) > 0 {
		rows, err := app.queries.ListFleetOutcomes(ctx, ids)
		if err != nil {
			logging.Errorf("fleets: outcomes of %s: %v", owner.Key(), err)
		}
		for _, row := range rows {
			outcomes[row.OpID] = row
		}
	}
	for _, op := range past {
		if !v.access(op, facts[op.ID]).View {
			continue
		}
		start := op.StartsAt.UTC()
		view.Past = append(view.Past, fleetBoardRow{
			Href: opURL(op.ID), Title: op.Title, Sub: fleetRowSub(op, ""),
			Day: start.Format("Mon Jan 2"), Clock: start.Format("15:04"), WhenRaw: start.Format(time.RFC3339),
			Count: pastFleetCount(op, outcomes[op.ID]), Cancelled: op.CancelledAt.Valid,
		})
	}
	rows, err := app.queries.ListOwnerPAPs(ctx, db.ListOwnerPAPsParams{
		OwnerKind: owner.Kind, OwnerID: owner.ID, Since: now.Add(-papWindow), RecentSince: now.Add(-papRecent),
		EndedBefore: now.Add(-opCaptureTail),
	})
	if err != nil {
		logging.Errorf("fleets: attendance of %s: %v", owner.Key(), err)
	}
	for _, row := range rows {
		view.Rows = append(view.Rows, papRow{
			CharacterID: row.CharacterID, Name: app.displayCharacter(ctx, row.CharacterID),
			Recent: row.Recent, Total: row.Total, LastOp: row.LastOp.UTC().Format("Jan 2"),
		})
	}
}

// ---------------------------------------------------------------------------
// The calendar file: one fleet as an .ics a calendar app can subscribe
// to, so someone with no account can follow it. It carries only what an
// anonymous visitor to the share link may see.
// ---------------------------------------------------------------------------

func (app *Application) handleFleetICS(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	token := chi.URLParam(r, "token")
	op, err := app.queries.GetOpByShareToken(ctx, sql.NullString{String: token, Valid: token != ""})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			logging.Errorf("fleets: calendar file: %v", err)
		}
		http.NotFound(w, r)
		return
	}
	anonymous := fleetViewer{Standing: standing.New(), plans: map[string]bool{}}
	facts := app.fleetFactsFor(ctx, anonymous, []db.Op{op})[op.ID]
	facts.ViaToken = true
	a := anonymous.access(op, facts)
	if !a.View {
		http.NotFound(w, r)
		return
	}
	body := fleetICS(op, a, app.ownerName(ctx, opOwner(op)), app.SiteURL(fleetSharePath(token)), time.Now().UTC())
	w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="fleet.ics"`)
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Header().Set("X-Robots-Tag", "noindex")
	fmt.Fprint(w, body)
}

// fleetICS writes one fleet as an iCalendar file (RFC 5545).
func fleetICS(op db.Op, a fleetAccess, owner, link string, now time.Time) string {
	const stamp = "20060102T150405Z"
	details := []string{"Fleet by " + owner + "."}
	if a.SeeFormUp && op.FormUp != "" {
		details = append(details, "Form up: "+op.FormUp)
	}
	if op.Doctrine != "" {
		details = append(details, "Doctrine: "+op.Doctrine)
	}
	if op.Description != "" {
		details = append(details, op.Description)
	}
	details = append(details, link)
	status := "CONFIRMED"
	if op.CancelledAt.Valid {
		status = "CANCELLED"
	}
	lines := []string{
		"BEGIN:VCALENDAR",
		"VERSION:2.0",
		"PRODID:-//EveSynapse//Fleets//EN",
		"CALSCALE:GREGORIAN",
		"METHOD:PUBLISH",
		"BEGIN:VEVENT",
		fmt.Sprintf("UID:fleet-%d@evesynapse", op.ID),
		"DTSTAMP:" + now.UTC().Format(stamp),
		"DTSTART:" + op.StartsAt.UTC().Format(stamp),
		"DTEND:" + opEnd(op).UTC().Format(stamp),
		"SUMMARY:" + icsText(op.Title),
		"DESCRIPTION:" + icsText(strings.Join(details, "\n")),
		"URL:" + link,
		"STATUS:" + status,
		"END:VEVENT",
		"END:VCALENDAR",
	}
	var b strings.Builder
	for _, line := range lines {
		b.WriteString(icsFold(line))
		b.WriteString("\r\n")
	}
	return b.String()
}

// icsText escapes a value as RFC 5545 asks.
func icsText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.NewReplacer(`\`, `\\`, ";", `\;`, ",", `\,`, "\n", `\n`, "\r", "").Replace(s)
}

// icsFold breaks a line at 75 bytes without splitting a character.
func icsFold(line string) string {
	const limit = 75
	if len(line) <= limit {
		return line
	}
	var b strings.Builder
	width := 0
	for _, r := range line {
		size := len(string(r))
		if width+size > limit {
			b.WriteString("\r\n ")
			width = 1
		}
		b.WriteRune(r)
		width += size
	}
	return b.String()
}
