package srp

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/humanize"
	"evesynapse/internal/logging"
	"evesynapse/internal/standing"
)

// ---------------------------------------------------------------------------
// One fleet's Ship Replacement tab. It opens for the pilots who flew it,
// the owner's members and its handlers, and never for the public. A
// pilot with no account claims here from the public killboard's record
// of their loss, so claiming needs no EVE access grant.
// ---------------------------------------------------------------------------

// srpFleetLoss is one of the viewer's losses during the fleet.
type srpFleetLoss struct {
	KillmailID int64
	Label      string
	Refusal    string
}

// fleetSRPView is the tab.
type fleetSRPView struct {
	ID         int64
	Title      string
	OwnerName  string
	Path       string
	Viewer     map[int64]bool
	Policy     *srpPolicyRow
	Losses     []srpFleetLoss
	Refused    []srpFleetLoss
	Mine       []srpRow
	SeesLedger bool
	Ledger     []srpRow
	CanClaim   bool
	Handles    bool
	HandleHref string
}

func srpFleetPath(id int64) string { return module.OpURL(id) + "srp" }

// srpCandidates lists characters' losses between two times: their own
// stored killmails, then the public killboard's, newest first.
func (app *mod) srpCandidates(ctx context.Context, characters []db.Character, ids []int64, from, to, now time.Time) []srpLoss {
	var out []srpLoss
	seen := map[int64]bool{}
	for _, loss := range app.srpLosses(ctx, characters, now) {
		if !loss.at.Before(from) && !loss.at.After(to) {
			seen[loss.km.KillmailID] = true
			out = append(out, loss)
		}
	}
	if len(ids) > 0 {
		rows, err := app.queries.ListVictimLossesBetween(ctx, db.ListVictimLossesBetweenParams{CharacterIds: ids, FromTime: from, ToTime: to})
		if err != nil {
			logging.Errorf("srp: public losses: %v", err)
		}
		for _, row := range rows {
			if seen[row.KillmailID] {
				continue
			}
			out = append(out, srpLoss{
				hash: row.Hash, at: row.KillmailTime,
				km: esi.Killmail{
					KillmailID: row.KillmailID, KillmailTime: row.KillmailTime.UTC().Format(time.RFC3339), SolarSystemID: row.SystemID,
					Victim: esi.KillmailVictim{CharacterID: row.VictimCharacterID, CorporationID: row.VictimCorporationID, ShipTypeID: row.ShipTypeID},
				},
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].at.After(out[j].at) })
	return out
}

func (app *mod) srpLossLabel(ctx context.Context, loss srpLoss) string {
	return fmt.Sprintf("%s · %s · %s · %s", loss.at.UTC().Format("Jan 2 15:04"),
		app.deps.TypeNameOrID(ctx, loss.km.Victim.ShipTypeID),
		app.deps.LocationTitle(ctx, loss.km.SolarSystemID, "solar_system"),
		app.srpPilot(ctx, loss.km.Victim.CharacterID))
}

// handleFleetSRP is the tab (GET /fleets/{id}/srp).
func (app *mod) handleFleetSRP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	opID, _ := strconv.ParseInt(chi.URLParam(r, "opID"), 10, 64)
	f, ok := app.deps.FleetSRPFor(ctx, opID)
	if !ok {
		app.deps.RenderFleetHidden(w, r)
		return
	}
	now := time.Now().UTC()
	view := &fleetSRPView{
		ID: f.Op.ID, Title: f.Op.Title, OwnerName: f.OwnerName, Path: srpFleetPath(f.Op.ID),
		Viewer: make(map[int64]bool, len(f.Pilots)), CanClaim: len(f.Pilots) > 0, Handles: f.Handles,
	}
	ids := make([]int64, 0, len(f.Pilots))
	for _, p := range f.Pilots {
		ids = append(ids, p.ID)
		view.Viewer[p.ID] = true
	}
	if f.Handles {
		view.HandleHref = standing.Address(srpPath, f.Owner.Key()) + "#srp-handle"
	}
	policies, err := app.queries.ListSRPPolicies(ctx, db.ListSRPPoliciesParams{OwnerKind: f.Op.OwnerKind, OwnerID: f.Op.OwnerID})
	if err != nil {
		logging.Errorf("srp: policies of fleet %d: %v", f.Op.ID, err)
	}
	policy := srpPolicyFor(f.Op, policies)
	if policy != nil {
		row := srpPolicyRowOf(*policy)
		view.Policy = &row
	}
	ops := map[int64]db.Op{f.Op.ID: f.Op}
	if len(ids) > 0 {
		app.fillFleetLosses(ctx, view, f, policy, ids, now)
		mine, err := app.queries.ListSRPRequestsOnOpByCharacters(ctx, db.ListSRPRequestsOnOpByCharactersParams{OpID: f.Op.ID, CharacterIds: ids})
		if err != nil {
			logging.Errorf("srp: requests on fleet %d: %v", f.Op.ID, err)
		}
		view.Mine = app.srpRows(ctx, mine, ops)
	}
	// Each claim is read under the policy it was filed with, so changing
	// a fleet's policy never opens earlier claims to more people.
	var ledger []db.SrpRequest
	switch {
	case f.Handles:
		ledger, err = app.queries.ListSRPRequestsOnOp(ctx, f.Op.ID)
	case f.Member:
		ledger, err = app.queries.ListSRPMemberLedgerOnOp(ctx, f.Op.ID)
	}
	if err != nil {
		logging.Errorf("srp: ledger of fleet %d: %v", f.Op.ID, err)
	}
	view.SeesLedger = f.Handles || len(ledger) > 0
	view.Ledger = app.srpRows(ctx, ledger, ops)
	w.Header().Set("X-Robots-Tag", "noindex")
	data := &PageData{ShellData: app.deps.PublicShell(ctx), FleetSRP: view}
	data.SectionSubTabs = f.Tabs
	app.deps.Render(ctx, w, http.StatusOK, "fleet_srp.html", data)
}

// fillFleetLosses sorts the viewer's losses during the fleet into those
// that can be claimed and those that cannot, with the reason.
func (app *mod) fillFleetLosses(ctx context.Context, view *fleetSRPView, f module.FleetSRP, policy *db.SrpPolicy, ids []int64, now time.Time) {
	var characters []db.Character
	if f.UserID != 0 {
		var err error
		if characters, err = app.queries.ListCharactersByUser(ctx, f.UserID); err != nil {
			logging.Errorf("srp: characters of user %d: %v", f.UserID, err)
		}
	}
	from := f.Op.StartsAt.Add(-module.OpCaptureLead)
	to := module.OpEnd(f.Op).Add(module.OpCaptureTail)
	losses := app.srpCandidates(ctx, characters, ids, from, to, now)
	if len(losses) == 0 {
		return
	}
	killIDs := make([]int64, 0, len(losses))
	for _, loss := range losses {
		killIDs = append(killIDs, loss.km.KillmailID)
	}
	claimed, err := app.queries.ListSRPClaimedKillmails(ctx, killIDs)
	if err != nil {
		logging.Errorf("srp: claimed losses: %v", err)
		return
	}
	seen, err := app.queries.ListOpAttendanceForPilots(ctx, db.ListOpAttendanceForPilotsParams{OpIds: []int64{f.Op.ID}, CharacterIds: ids})
	if err != nil {
		logging.Errorf("srp: attendance on fleet %d: %v", f.Op.ID, err)
		return
	}
	sightings := make(map[int64]db.ListOpAttendanceForPilotsRow, len(seen))
	for _, row := range seen {
		sightings[row.CharacterID] = row
	}
	visible := app.deps.FleetsVisible(ctx, []db.Op{f.Op})[f.Op.ID]
	doctrine := map[int64]bool{}
	for _, loss := range losses {
		if slices.Contains(claimed, loss.km.KillmailID) {
			continue
		}
		ship := loss.km.Victim.ShipTypeID
		in, known := doctrine[ship]
		if !known {
			in = app.srpInDoctrine(ctx, f.Op, ship)
			doctrine[ship] = in
		}
		sighting, on := sightings[loss.km.Victim.CharacterID]
		attended := on && srpAttendedAt(sighting.Source, sighting.FirstSeenAt, sighting.LastSeenAt, loss.at)
		row := srpFleetLoss{KillmailID: loss.km.KillmailID, Label: app.srpLossLabel(ctx, loss)}
		if row.Refusal = srpRefusal(f.Op, policy, loss, visible, attended, in, now); row.Refusal != "" {
			view.Refused = append(view.Refused, row)
		} else {
			view.Losses = append(view.Losses, row)
		}
	}
}

// handleFleetSRPClaim files a claim from the tab
// (POST /fleets/{id}/srp/claim).
func (app *mod) handleFleetSRPClaim(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	opID, _ := strconv.ParseInt(chi.URLParam(r, "opID"), 10, 64)
	f, ok := app.deps.FleetSRPFor(ctx, opID)
	if !ok {
		app.deps.RenderFleetHidden(w, r)
		return
	}
	_ = r.ParseForm()
	back := app.deps.FlashBack(w, r, srpFleetPath(f.Op.ID))
	killID, _ := strconv.ParseInt(r.Form.Get("killmail"), 10, 64)
	mine := func(id int64) bool {
		return slices.ContainsFunc(f.Pilots, func(p module.NamedChoice) bool { return p.ID == id })
	}
	note := humanize.Clip(strings.TrimSpace(r.Form.Get("note")), srpNoteMax)
	message, _ := app.fileClaim(ctx, f.Op, killID, f.UserID, mine, note, time.Now().UTC())
	back(message)
}

// handleFleetSRPWithdraw takes back the viewer's own unanswered request
// (POST /fleets/{id}/srp/{id}/withdraw).
func (app *mod) handleFleetSRPWithdraw(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	opID, _ := strconv.ParseInt(chi.URLParam(r, "opID"), 10, 64)
	f, ok := app.deps.FleetSRPFor(ctx, opID)
	if !ok {
		app.deps.RenderFleetHidden(w, r)
		return
	}
	back := app.deps.FlashBack(w, r, srpFleetPath(f.Op.ID))
	id, _ := strconv.ParseInt(chi.URLParam(r, "requestID"), 10, 64)
	ids := make([]int64, 0, len(f.Pilots))
	for _, p := range f.Pilots {
		ids = append(ids, p.ID)
	}
	n, err := app.queries.WithdrawFleetSRPRequest(ctx, db.WithdrawFleetSRPRequestParams{ID: id, OpID: f.Op.ID, CharacterIds: ids})
	if err != nil {
		logging.Errorf("srp: withdraw request %d: %v", id, err)
	}
	if err != nil || n == 0 {
		back("That request could not be withdrawn: it is not yours, or it has been answered.")
		return
	}
	back("Request withdrawn.")
}
