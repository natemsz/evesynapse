package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/gamelog"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// A fleet's Report tab. Pilot-by-pilot figures are for the people with a
// stake in the fleet: its owner's members, those who flew it, and those
// who run it. The public sees totals only, on the fleet's Overview and
// the leaderboards.
// ---------------------------------------------------------------------------

func fleetReportPath(id int64) string { return opURL(id) + "report" }

// fleetStake: the viewer belongs to the fleet's owner and may see the
// fleet, or one of their characters flew it. A share link and a pilot's
// public corporation do not count.
func (app *Application) fleetStake(ctx context.Context, op db.Op, v fleetViewer) (member, attended bool) {
	facts := app.fleetFactsFor(ctx, v, []db.Op{op})[op.ID]
	facts.ViaToken = false
	member = v.access(op, facts).View && v.readsDoctrine(opOwner(op), module.DoctrineMembers)
	if ids := v.characterIDs(); len(ids) > 0 {
		var err error
		attended, err = app.queries.AnyOnOpAttendance(ctx, db.AnyOnOpAttendanceParams{OpID: op.ID, CharacterIds: ids})
		if err != nil {
			logging.Errorf("fleets: attendance of viewer on %d: %v", op.ID, err)
		}
	}
	return member, attended
}

// fleetReportTab: the viewer can open this fleet's Report tab. It opens
// once the fleet has started, so pilots can add their logs while it runs.
func (app *Application) fleetReportTab(ctx context.Context, op db.Op, v fleetViewer, now time.Time) bool {
	if op.CancelledAt.Valid || op.StartsAt.After(now) {
		return false
	}
	member, attended := app.fleetStake(ctx, op, v)
	if member || attended {
		return true
	}
	facts := app.fleetFactsFor(ctx, v, []db.Op{op})[op.ID]
	facts.ViaToken = false
	return v.access(op, facts).Manage
}

type fleetReportChip struct {
	Label, Href string
	On          bool
}

type fleetReportPilot struct {
	CharacterID, ShipTypeID int64
	Name, Ship              string
	Kills                   int32
	Out, In, Reps           string
	Died, HasLog            bool
}

type fleetReportMail struct {
	KillmailID, VictimID, ShipTypeID int64
	When, Victim, Ship, Value        string
	Loss                             bool
	// Linked: the mail has a page on the killboard. The demo's
	// invented mails stay off it.
	Linked bool
}

type fleetConsentChoice struct {
	CharacterID int64
	Name        string
	On          bool
}

// fleetBattleView is the battle on show.
type fleetBattleView struct {
	SystemID                         int64
	System, When, Length             string
	Coverage, LeftOut, Unpriced      string
	CoverWidth                       float64
	Kills, Losses                    int32
	Destroyed, Lost                  string
	Out, In, Reps, Cap, FriendlyFire string
	Curve                            *fleetCurve
	TopDPS                           string
	// Marks are the battle's kills and losses on the curve's x axis,
	// and Ticks five evenly spaced times along it.
	Marks []fleetCurveMark
	Ticks []string
}

// fleetCurveMark is one killmail drawn on the battle's curve.
type fleetCurveMark struct {
	X     float64
	Loss  bool
	Label string
}

type fleetReportView struct {
	ID         int64
	Title      string
	Owner      string
	Path       string
	Closed     bool
	Building   bool // closed, with its first build still to come
	Demo       bool // the demo's invented fleet
	Chips      []fleetReportChip
	Battle     *fleetBattleView
	Pilots     []fleetReportPilot
	PilotsCut  bool
	Mails      []fleetReportMail
	MailsCut   bool
	Consent    []fleetConsentChoice
	UploadHref string
	// UploadAction is the upload form's target, which counts the log in
	// this fleet's report; LogMB is the largest log it takes.
	UploadAction string
	LogMB        int
	ViewerChars  map[int64]bool
	// Linked: the shown battle's mails that have a killboard page.
	Linked map[int64]bool
	// X is the shown battle's explorer, nil for a battle built before
	// explorers were (the page then shows its plain report).
	X *fleetExplorerView
}

func (app *Application) handleFleetReport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	v := app.fleetViewerFor(ctx)
	now := time.Now()
	id, _ := strconv.ParseInt(chi.URLParam(r, "opID"), 10, 64)
	op, err := app.queries.GetOp(ctx, id)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			logging.Errorf("fleets: load fleet %d: %v", id, err)
		}
		app.renderFleetHidden(w, r, v)
		return
	}
	if !app.fleetReportTab(ctx, op, v, now) {
		app.renderFleetHidden(w, r, v)
		return
	}
	w.Header().Set("X-Robots-Tag", "noindex")
	view := &fleetReportView{
		ID: op.ID, Title: op.Title, Owner: app.ownerName(ctx, opOwner(op)), Path: fleetReportPath(op.ID),
		Closed: opEnd(op).Add(opCaptureTail).Before(now), UploadHref: "/intel/battles/", Demo: isDemoFleet(op),
		UploadAction: "/intel/battles/upload?fleet=" + strconv.FormatInt(op.ID, 10), LogMB: gamelog.MaxText >> 20,
		ViewerChars: make(map[int64]bool, len(v.Characters)),
	}
	for _, c := range v.Characters {
		view.ViewerChars[c.CharacterID] = true
	}
	view.Consent = app.fleetConsentChoices(ctx, op.ID, v)
	app.fillFleetBattle(ctx, view, r.URL.Query().Get("battle"))
	view.Building = view.Closed && view.Battle == nil && app.fleetReportPending(ctx, op.ID)
	app.fillFleetExplorer(ctx, view, r.URL.Query().Get("battle"), r.URL.Query().Get("m"))

	data := pageData{ShellData: app.PublicShell(ctx), FleetReport: view}
	facts := app.fleetFactsFor(ctx, v, []db.Op{op})[op.ID]
	facts.ViaToken = false
	// A pilot let in only by having flown it has no Overview to go to.
	access := v.access(op, facts)
	live, err := app.fleetLiveReadFor(ctx, op, v, now)
	if err != nil {
		logging.Errorf("fleets: composition of %d: %v", op.ID, err)
	}
	_, _, _, srp := app.seesFleetSRP(ctx, op, v)
	data.SectionSubTabs = fleetTabs(op.ID, access.View, seesFleetLive(op, access, live.sight, now), srp, true, "report")
	app.render(ctx, w, http.StatusOK, "fleet_report.html", data)
}

// fleetReportPending: a build of this fleet is still to come, so having
// no battle stored yet does not mean it had none.
func (app *Application) fleetReportPending(ctx context.Context, opID int64) bool {
	state, err := app.queries.GetFleetReportState(ctx, opID)
	if errors.Is(err, pgx.ErrNoRows) {
		has, err := app.queries.OpHasAttendance(ctx, opID)
		if err != nil {
			logging.Errorf("fleets: attendance of %d: %v", opID, err)
		}
		return has
	}
	if err != nil {
		logging.Errorf("fleets: report state of %d: %v", opID, err)
		return false
	}
	return state.DirtySeq.Valid || !state.ComputedAt.Valid
}

// fleetConsentChoices is the viewer's account characters that flew this
// fleet, and whether each has added its logs.
func (app *Application) fleetConsentChoices(ctx context.Context, opID int64, v fleetViewer) []fleetConsentChoice {
	if v.UserID == 0 || len(v.Characters) == 0 {
		return nil
	}
	ids := v.characterIDs()
	flew, err := app.queries.ListOpAttendanceOf(ctx, db.ListOpAttendanceOfParams{OpID: opID, CharacterIds: ids})
	if err != nil {
		logging.Errorf("fleets: attendance of viewer on %d: %v", opID, err)
		return nil
	}
	on, err := app.queries.ListFleetBattleConsentFor(ctx, db.ListFleetBattleConsentForParams{OpID: opID, UserID: v.UserID, CharacterIds: ids})
	if err != nil {
		logging.Errorf("fleets: log choices on %d: %v", opID, err)
		return nil
	}
	flown := make(map[int64]bool, len(flew))
	for _, id := range flew {
		flown[id] = true
	}
	added := make(map[int64]bool, len(on))
	for _, id := range on {
		added[id] = true
	}
	var out []fleetConsentChoice
	for _, c := range v.Characters {
		if flown[c.CharacterID] {
			out = append(out, fleetConsentChoice{CharacterID: c.CharacterID, Name: c.Name, On: added[c.CharacterID]})
		}
	}
	return out
}

// fillFleetBattle reads the stored battle on show: its figures, its
// first 100 pilots and mails, and its curve.
func (app *Application) fillFleetBattle(ctx context.Context, view *fleetReportView, pick string) {
	battles, err := app.queries.ListFleetBattles(ctx, view.ID)
	if err != nil {
		logging.Errorf("fleets: battles of %d: %v", view.ID, err)
		return
	}
	if len(battles) == 0 {
		return
	}
	names := app.newKillNames(ctx)
	shown := battles[0]
	want, wanted := strconv.Atoi(pick)
	for _, b := range battles {
		if wanted == nil && int(b.Seq) == want {
			shown = b
		}
	}
	systemName := func(id int64) string {
		if name := names.system(id).Name; id != 0 && name != "" {
			return name
		}
		return "Unknown system"
	}
	if len(battles) > 1 {
		for _, b := range battles {
			view.Chips = append(view.Chips, fleetReportChip{
				Label: fmt.Sprintf("%s %s: %d %s, %d %s", systemName(b.SystemID), b.StartedAt.UTC().Format("15:04"),
					b.Kills, map[bool]string{true: "kill", false: "kills"}[b.Kills == 1], b.Losses, map[bool]string{true: "loss", false: "losses"}[b.Losses == 1]),
				Href: view.Path + "?battle=" + strconv.Itoa(int(b.Seq)),
				On:   b.ID == shown.ID,
			})
		}
	}
	bv := &fleetBattleView{
		SystemID: shown.SystemID, System: systemName(shown.SystemID),
		When:   shown.StartedAt.UTC().Format("Mon Jan 2, 15:04") + " EVE",
		Length: humanDuration(max(shown.EndedAt.Sub(shown.StartedAt), time.Minute)),
		Kills:  shown.Kills, Losses: shown.Losses,
		Destroyed: formatISKShort(shown.IskDestroyed), Lost: formatISKShort(shown.IskLost),
		Out: battleNum(shown.DamageOut), In: battleNum(shown.DamageIn), Reps: battleNum(shown.RepsOut),
		Cap: battleNum(shown.CapOut), FriendlyFire: battleNum(shown.FriendlyFire),
	}
	bv.Coverage = fmt.Sprintf("Based on %d of %d pilots' logs.", shown.PilotsWithLogs, shown.PilotsTotal)
	if shown.PilotsTotal > 0 {
		bv.CoverWidth = round1(float64(shown.PilotsWithLogs) / float64(shown.PilotsTotal) * 100)
	}
	if shown.LogsLeftOut > 0 {
		bv.LeftOut = fmt.Sprintf("%d stored before logs could be lined up, so not counted.", shown.LogsLeftOut)
	}
	if shown.UnpricedMails > 0 {
		bv.Unpriced = fmt.Sprintf("%d killmails have no price yet and add no ISK.", shown.UnpricedMails)
	}
	if raw, err := app.queries.GetFleetBattleSeries(ctx, shown.ID); err == nil {
		var curve fleetCurve
		if json.Unmarshal([]byte(raw), &curve) == nil && curve.Seconds > 0 {
			bv.Curve, bv.TopDPS = &curve, battleNum(curve.TopDPS)
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		logging.Errorf("fleets: curve of battle %d: %v", shown.ID, err)
	}
	view.Battle = bv

	pilots, err := app.queries.ListFleetBattlePilots(ctx, shown.ID)
	if err != nil {
		logging.Errorf("fleets: pilots of battle %d: %v", shown.ID, err)
	}
	view.PilotsCut = int32(len(pilots)) < shown.PilotsTotal
	for _, p := range pilots {
		row := fleetReportPilot{
			CharacterID: p.CharacterID, ShipTypeID: p.ShipTypeID, Name: names.character(p.CharacterID),
			Ship: names.typeName(p.ShipTypeID), Kills: p.KillsOn, Died: p.Died, HasLog: p.HasLog,
		}
		if p.HasLog {
			row.Out, row.In, row.Reps = battleNum(p.DamageOut), battleNum(p.DamageIn), battleNum(p.RepsOut)
		}
		view.Pilots = append(view.Pilots, row)
	}
	kills, err := app.queries.ListFleetBattleKills(ctx, shown.ID)
	if err != nil {
		logging.Errorf("fleets: mails of battle %d: %v", shown.ID, err)
	}
	view.MailsCut = int32(len(kills)) < shown.Kills+shown.Losses
	view.Linked = make(map[int64]bool, len(kills))
	for _, k := range kills {
		view.Linked[k.KillmailID] = !view.Demo && k.Public
		row := fleetReportMail{
			KillmailID: k.KillmailID, VictimID: k.VictimCharacterID, ShipTypeID: k.ShipTypeID,
			When: k.KillmailTime.UTC().Format("15:04:05"), Victim: names.character(k.VictimCharacterID),
			Ship: names.typeName(k.ShipTypeID), Value: "No price yet", Loss: k.Loss, Linked: !view.Demo && k.Public,
		}
		if k.ValueIsk.Valid {
			row.Value = formatISKShort(k.ValueIsk.Float64)
		}
		view.Mails = append(view.Mails, row)
	}
	if c := bv.Curve; c != nil && c.Seconds > 0 {
		start := c.Start
		if start == 0 {
			start = shown.StartedAt.Unix()
		}
		for _, k := range kills {
			x := float64(k.KillmailTime.Unix()-start) / float64(c.Seconds) * 600
			if x < 0 || x > 600 {
				continue
			}
			result := "Kill"
			if k.Loss {
				result = "Loss"
			}
			label := fmt.Sprintf("%s %s: %s, %s", k.KillmailTime.UTC().Format("15:04:05"), result, names.typeName(k.ShipTypeID), names.character(k.VictimCharacterID))
			bv.Marks = append(bv.Marks, fleetCurveMark{X: round1(x), Loss: k.Loss, Label: label})
		}
		for i := 0; i <= 4; i++ {
			at := time.Unix(start+c.Seconds*int64(i)/4, 0).UTC()
			bv.Ticks = append(bv.Ticks, at.Format("15:04"))
		}
	}
}

// handleFleetReportLogs saves which of the viewer's characters add their
// logs to this fleet. Only characters on the account that flew it can.
func (app *Application) handleFleetReportLogs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	v := app.fleetViewerFor(ctx)
	id, _ := strconv.ParseInt(chi.URLParam(r, "opID"), 10, 64)
	op, err := app.queries.GetOp(ctx, id)
	if err != nil || v.UserID == 0 || !app.fleetReportTab(ctx, op, v, time.Now()) {
		app.renderFleetHidden(w, r, v)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, fleetReportPath(op.ID), http.StatusSeeOther)
		return
	}
	wanted := make(map[string]bool, len(r.Form["logs"]))
	for _, raw := range r.Form["logs"] {
		wanted[raw] = true
	}
	now := time.Now().UTC()
	failed, changed := false, false
	for _, c := range app.fleetConsentChoices(ctx, op.ID, v) {
		want := wanted[strconv.FormatInt(c.CharacterID, 10)]
		if want == c.On {
			continue
		}
		changed = true
		var err error
		if want {
			err = app.queries.SetFleetBattleConsent(ctx, db.SetFleetBattleConsentParams{OpID: op.ID, CharacterID: c.CharacterID, UserID: v.UserID, ConsentedAt: now})
		} else {
			err = app.queries.DeleteFleetBattleConsent(ctx, db.DeleteFleetBattleConsentParams{OpID: op.ID, CharacterID: c.CharacterID})
		}
		if err != nil {
			failed = true
			logging.Errorf("fleets: log choice of %d on %d: %v", c.CharacterID, op.ID, err)
		}
	}
	if changed {
		if err := app.queries.MarkFleetReportDirty(ctx, op.ID); err != nil {
			failed = true
			logging.Errorf("fleets: mark report of %d: %v", op.ID, err)
		}
	}
	if failed {
		app.flash(ctx, "Your log choices could not all be saved. Try again.")
	} else {
		app.flash(ctx, "Saved. The report updates within a few minutes of the fleet ending.")
	}
	http.Redirect(w, r, fleetReportPath(op.ID), http.StatusSeeOther)
}

// fleetTotalsView is a fleet's public totals, for its Overview.
type fleetTotalsView struct {
	Battles, Kills, Losses int64
	Destroyed, Lost        string
}

// fleetTotalsFor is nil until the fleet has a stored battle.
func (app *Application) fleetTotalsFor(ctx context.Context, opID int64) *fleetTotalsView {
	t, err := app.queries.GetFleetBattleTotals(ctx, opID)
	if err != nil {
		logging.Errorf("fleets: totals of %d: %v", opID, err)
		return nil
	}
	if t.Battles == 0 {
		return nil
	}
	return &fleetTotalsView{Battles: t.Battles, Kills: t.Kills, Losses: t.Losses, Destroyed: formatISKShort(t.IskDestroyed), Lost: formatISKShort(t.IskLost)}
}

// fleetBattleLink is one fight of a fleet's report, as a row on the
// fleet's Overview that opens it.
type fleetBattleLink struct {
	Href, System, When string
	Kills, Losses      int32
	Destroyed, Lost    string
}

// fleetBattleLinks lists a fleet's stored fights, at most a handful.
func (app *Application) fleetBattleLinks(ctx context.Context, opID int64) []fleetBattleLink {
	battles, err := app.queries.ListFleetBattles(ctx, opID)
	if err != nil {
		logging.Errorf("fleets: battles of %d: %v", opID, err)
		return nil
	}
	names := app.newKillNames(ctx)
	out := make([]fleetBattleLink, 0, len(battles))
	for _, b := range battles {
		system := names.system(b.SystemID).Name
		if system == "" {
			system = "Unknown system"
		}
		out = append(out, fleetBattleLink{
			Href:   fleetReportPath(opID) + "?battle=" + strconv.Itoa(int(b.Seq)),
			System: system, When: b.StartedAt.UTC().Format("15:04") + " EVE",
			Kills: b.Kills, Losses: b.Losses,
			Destroyed: formatISKShort(b.IskDestroyed) + " destroyed", Lost: formatISKShort(b.IskLost) + " lost",
		})
		if b.IskDestroyed == 0 {
			out[len(out)-1].Destroyed = "nothing destroyed"
		}
		if b.IskLost == 0 {
			out[len(out)-1].Lost = "nothing lost"
		}
	}
	return out
}
