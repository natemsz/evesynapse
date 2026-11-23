package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/jackc/pgx/v5"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
	"evesynapse/internal/standing"
)

// ---------------------------------------------------------------------------
// The fleet leaderboards. Public boards count public fleets only. Someone
// in a group also sees that group's board with its private fleets counted.
// Everything is read from fleet_leaders, written by the worker.
// ---------------------------------------------------------------------------

const (
	fleetLeaderRows  = 100
	fleetBoardPilots = "pilot"
	fleetBoardGroups = "group"
	fleetRecordSpan  = "all"
)

type fleetLeaderRow struct {
	Place                 int32
	Kind                  string
	ID                    int64
	Name, Href            string
	Fleets, Kills, Deaths int64
	ISK                   string
	Points                int64
	Yours                 bool
}

type fleetLeadersView struct {
	Title, Summary, Scoring string
	Periods, Boards         []fleetReportChip
	// Scopes switch between the public board and the viewer's groups'.
	Scopes             []module.OwnerOption
	GroupBoard, Scoped bool
	AllHref            string
	Rows               []fleetLeaderRow
	ViewerChars        map[int64]bool
}

func fleetLeadersHref(period, board, ownerKey string) string {
	q := url.Values{}
	if period != fleetLeaderPeriods[0].Key {
		q.Set("period", period)
	}
	if board == fleetBoardGroups {
		q.Set("board", "groups")
	}
	if ownerKey != "" {
		q.Set("owner", ownerKey)
	}
	if len(q) == 0 {
		return module.FleetLeadersPath
	}
	return module.FleetLeadersPath + "?" + q.Encode()
}

func (app *Application) handleFleetLeaders(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	v := app.fleetViewerFor(ctx)

	period := fleetLeaderPeriods[0].Key
	for _, p := range fleetLeaderPeriods {
		if q.Get("period") == p.Key {
			period = p.Key
		}
	}
	board := fleetBoardPilots
	if q.Get("board") == "groups" {
		board = fleetBoardGroups
	}
	audience, scopeKind, ownerKey := "public", "", ""
	var scopeID int64
	view := &fleetLeadersView{
		Title:   "Fleet leaderboards",
		Summary: "Counts public fleets, from attendance read in EVE.",
		Scoring: fmt.Sprintf("A pilot earns %d points for each fleet, %d for each kill and up to %d more for the kill's value, split by damage. Each death takes away %d. Kills of ships worth under 1 million ISK, of a pilot's own ship, or inside one corporation do not count.",
			fleetPointsAttend, fleetPointsKill, int(fleetPointsValueCap), fleetPointsDeath),
		ViewerChars: make(map[int64]bool, len(v.Characters)),
	}
	for _, c := range v.Characters {
		view.ViewerChars[c.CharacterID] = true
	}
	if raw := q.Get("owner"); raw != "" {
		owner, ok := standing.ParseAnyOwner(raw)
		if !ok || !app.fleetBoardOpen(ctx, owner, v) {
			app.renderFleetHidden(w, r, v)
			return
		}
		board, scopeKind, scopeID, ownerKey = fleetBoardPilots, owner.Kind, owner.ID, owner.Key()
		view.Scoped, view.AllHref = true, fleetLeadersHref(period, fleetBoardPilots, "")
		view.Title = app.ownerName(ctx, owner) + ": fleet leaderboard"
		if v.readsDoctrine(owner, module.DoctrineMembers) {
			audience = "members"
			view.Summary = "You are in this group, so its private fleets count here too."
		}
	}
	view.GroupBoard = board == fleetBoardGroups
	for _, p := range fleetLeaderPeriods {
		view.Periods = append(view.Periods, fleetReportChip{Label: p.Label, Href: fleetLeadersHref(p.Key, board, ownerKey), On: p.Key == period})
	}
	if !view.Scoped {
		view.Boards = []fleetReportChip{
			{Label: "Pilots", Href: fleetLeadersHref(period, fleetBoardPilots, ""), On: board == fleetBoardPilots},
			{Label: "Groups", Href: fleetLeadersHref(period, fleetBoardGroups, ""), On: board == fleetBoardGroups},
		}
	}
	// The boards of the viewer's own groups sit beside the public one.
	if owners := v.owners(); len(owners) > 0 {
		view.Scopes = append(view.Scopes, module.OwnerOption{Name: "Public board", Active: ownerKey == "", Href: fleetLeadersHref(period, board, "")})
		for _, o := range owners {
			view.Scopes = append(view.Scopes, module.OwnerOption{ID: o.ID, Name: fleetOwnerLabel(o, app.ownerName(ctx, o)), Active: o.Key() == ownerKey, Href: fleetLeadersHref(period, fleetBoardPilots, o.Key())})
		}
	}
	rows, err := app.queries.ListFleetLeaders(ctx, db.ListFleetLeadersParams{
		Period: period, Audience: audience, ScopeKind: scopeKind, ScopeID: scopeID, Board: board, RowLimit: fleetLeaderRows,
	})
	if err != nil {
		logging.Errorf("fleets: leaderboard: %v", err)
	}
	view.Rows = app.fleetLeaderRows(ctx, rows, period, view.ViewerChars)
	if audience != "public" {
		w.Header().Set("X-Robots-Tag", "noindex")
	}
	data := pageData{ShellData: app.PublicShell(ctx), FleetLeaders: view}
	data.HeadMeta = app.HeadMeta(module.PageMeta{
		Title: view.Title, Description: "Fleet leaderboards for EVE Online pilots and groups: fleets flown, kills, deaths and ISK destroyed.",
		CanonicalPath: module.FleetLeadersPath,
	})
	app.render(ctx, w, http.StatusOK, "fleet_leaders.html", data)
}

// fleetBoardOpen: an owner's members can always read its board. Anyone
// else can read it once it has a public board, and a community's only
// while it is listed. An address naming any other owner opens nothing,
// so it cannot set off a name lookup for an arbitrary id.
func (app *Application) fleetBoardOpen(ctx context.Context, owner orgOwner, v fleetViewer) bool {
	if v.readsDoctrine(owner, module.DoctrineMembers) {
		return true
	}
	if owner.Kind == ownerCommunity {
		c, err := app.queries.GetCommunity(ctx, owner.ID)
		if err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				logging.Errorf("fleets: community %d: %v", owner.ID, err)
			}
			return false
		}
		if !c.Listed || c.ArchivedAt.Valid {
			return false
		}
	}
	periods := make([]string, 0, len(fleetLeaderPeriods))
	for _, p := range fleetLeaderPeriods {
		periods = append(periods, p.Key)
	}
	open, err := app.queries.FleetBoardExists(ctx, db.FleetBoardExistsParams{Periods: periods, ScopeKind: owner.Kind, ScopeID: owner.ID})
	if err != nil {
		logging.Errorf("fleets: board of %s: %v", owner.Key(), err)
		return false
	}
	return open
}

// fleetLeaderRows names one board's rows, with one read for all its
// communities.
func (app *Application) fleetLeaderRows(ctx context.Context, rows []db.ListFleetLeadersRow, period string, mine map[int64]bool) []fleetLeaderRow {
	names := app.newKillNames(ctx)
	var communityIDs []int64
	for _, r := range rows {
		if r.EntityKind == ownerCommunity {
			communityIDs = append(communityIDs, r.EntityID)
		}
	}
	communities := map[int64]string{}
	if len(communityIDs) > 0 {
		labels, err := app.queries.ListCommunityLabels(ctx, communityIDs)
		if err != nil {
			logging.Errorf("fleets: leaderboard community names: %v", err)
		}
		for _, l := range labels {
			communities[l.ID] = l.Name
		}
	}
	out := make([]fleetLeaderRow, 0, len(rows))
	for _, r := range rows {
		row := fleetLeaderRow{
			Place: r.Place, Kind: r.EntityKind, ID: r.EntityID, Fleets: r.Fleets, Kills: r.Kills, Deaths: r.Deaths,
			ISK: formatISKShort(r.Isk), Points: r.Points,
		}
		switch r.EntityKind {
		case "character":
			row.Name, row.Yours = names.character(r.EntityID), mine[r.EntityID]
		case ownerCorporation:
			row.Name = names.corp(r.EntityID)
		case ownerAlliance:
			row.Name = names.alliance(r.EntityID)
		case ownerCommunity:
			row.Name = communities[r.EntityID]
		}
		if r.EntityKind != "character" {
			row.Href = fleetLeadersHref(period, fleetBoardPilots, orgOwner{Kind: r.EntityKind, ID: r.EntityID}.Key())
		}
		out = append(out, row)
	}
	return out
}

// fleetRecordView is a pilot's or group's standing, for the killboard
// and the public profile.
type fleetRecordView struct {
	Text, Href string
}

// fleetRecord is nil for anyone not on the all-time public board. kind
// is character, corporation or alliance.
func (app *Application) fleetRecord(ctx context.Context, kind string, id int64) *fleetRecordView {
	board, href := fleetBoardGroups, ""
	switch kind {
	case "character":
		board, href = fleetBoardPilots, fleetLeadersHref(fleetRecordSpan, fleetBoardPilots, "")
	case ownerCorporation, ownerAlliance:
		href = module.FleetLeadersFor(orgOwner{Kind: kind, ID: id}.Key())
	default:
		return nil
	}
	r, err := app.queries.GetFleetLeaderPlace(ctx, db.GetFleetLeaderPlaceParams{Period: fleetRecordSpan, Board: board, EntityKind: kind, EntityID: id})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			logging.Errorf("fleets: record of %s %s: %v", kind, strconv.FormatInt(id, 10), err)
		}
		return nil
	}
	return &fleetRecordView{Text: module.FleetRecordText(r.Place, r.Fleets, r.Kills, r.Deaths, r.Points), Href: href}
}
