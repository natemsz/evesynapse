package app

// The killboard's leaderboards (/intel/kills/top/). The worker ranks
// each period's kills into kill_leaders (schema 155): pilots,
// corporations and alliances by kills and by ISK destroyed, hulls
// destroyed and hulls used, systems, regions and the biggest single
// kills, for New Eden, each region and system, each hull (who flies
// it), and each alliance's and corporation's own members. The page
// reads the stored ranks only: an overview of every board's top five,
// or one board in full.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// killLeaderPeriod is one ranked window and how often it is ranked.
type killLeaderPeriod struct {
	Key   string
	Label string
	Span  time.Duration
	Every time.Duration
}

var killLeaderPeriods = []killLeaderPeriod{
	{"day", "24 hours", 24 * time.Hour, 10 * time.Minute},
	{"week", "7 days", 7 * 24 * time.Hour, time.Hour},
	{"month", "30 days", 30 * 24 * time.Hour, 6 * time.Hour},
}

// killPaddingValue is the loss under which a kill is padding for the
// leaderboards: an empty capsule, rookie ship, shuttle or bare hull,
// nothing fitted or plugged in.
const killPaddingValue = 1e6

// killLeadersMu keeps one ranking run at a time: the working set is
// one pair of tables.
var killLeadersMu sync.Mutex

// refreshKillLeaders ranks the period most overdue, at most one per
// call, so a killboard pass never carries more than one run.
func (app *Application) refreshKillLeaders(ctx context.Context) {
	if !killLeadersMu.TryLock() {
		return
	}
	defer killLeadersMu.Unlock()
	states, err := app.queries.ListKillLeadersState(ctx)
	if err != nil {
		logging.Errorf("killboard leaders: state: %v", err)
		return
	}
	done := map[string]time.Time{}
	for _, s := range states {
		done[s.Period] = s.ComputedAt
	}
	now := time.Now().UTC()
	for _, p := range killLeaderPeriods {
		if last, ok := done[p.Key]; ok && now.Sub(last) < p.Every {
			continue
		}
		start := time.Now()
		n, err := app.rankKillLeaders(ctx, p, now)
		if err != nil {
			logging.Errorf("killboard leaders: rank %s: %v", p.Key, err)
			return
		}
		logging.Infof("killboard leaders: ranked the last %s (%d kills) in %s", p.Label, n, time.Since(start).Round(time.Millisecond))
		return
	}
}

// rankKillLeaders fills the working set with one period's kills and
// their attackers and replaces the period's ranks, in one
// transaction: a page reads the old ranks until the new ones land.
func (app *Application) rankKillLeaders(ctx context.Context, p killLeaderPeriod, now time.Time) (int64, error) {
	since := now.Add(-p.Span)
	tx, err := app.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SET LOCAL work_mem = '128MB'"); err != nil {
		return 0, err
	}
	q := db.New(tx)
	if err := q.ClearKillLeadersStage(ctx); err != nil {
		return 0, err
	}
	n, err := q.FillKillLeadersWin(ctx, db.FillKillLeadersWinParams{Since: since, PaddingValue: killPaddingValue})
	if err != nil {
		return 0, err
	}
	if err := q.FillKillLeadersAtt(ctx); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, "ANALYZE kill_leaders_win, kill_leaders_att"); err != nil {
		return 0, err
	}
	if err := q.DeleteKillLeadersPeriod(ctx, p.Key); err != nil {
		return 0, err
	}
	for _, board := range []string{"character", "corporation", "alliance"} {
		if err := q.InsertKillLeadersEntity(ctx, db.InsertKillLeadersEntityParams{Period: p.Key, Board: board}); err != nil {
			return 0, fmt.Errorf("%s: %w", board, err)
		}
	}
	for _, board := range []string{"ship_lost", "system", "region", "kill"} {
		if err := q.InsertKillLeadersPlace(ctx, db.InsertKillLeadersPlaceParams{Period: p.Key, Board: board}); err != nil {
			return 0, fmt.Errorf("%s: %w", board, err)
		}
	}
	if err := q.InsertKillLeadersShipsUsed(ctx, p.Key); err != nil {
		return 0, fmt.Errorf("ship_used: %w", err)
	}
	if err := q.ClearKillLeadersStage(ctx); err != nil {
		return 0, err
	}
	if err := q.UpsertKillLeadersState(ctx, db.UpsertKillLeadersStateParams{Period: p.Key, Since: since, ComputedAt: now, Kills: n}); err != nil {
		return 0, err
	}
	return n, tx.Commit(ctx)
}

// killLeaderBoardDef is one board: what it ranks and how it reads.
type killLeaderBoardDef struct {
	Key   string
	Title string
	Count string // the count column's word
	ISK   string // the ISK column's word
}

var killLeaderBoardDefs = map[string]killLeaderBoardDef{
	"character":   {"character", "Pilots", "kills", "ISK destroyed"},
	"corporation": {"corporation", "Corporations", "kills", "ISK destroyed"},
	"alliance":    {"alliance", "Alliances", "kills", "ISK destroyed"},
	"ship_lost":   {"ship_lost", "Ships destroyed", "lost", "ISK lost"},
	"ship_used":   {"ship_used", "Ships used", "kills", "ISK destroyed"},
	"system":      {"system", "Systems", "kills", "ISK destroyed"},
	"region":      {"region", "Regions", "kills", "ISK destroyed"},
	"kill":        {"kill", "Biggest kills", "", "value"},
}

// killLeaderBoards lists a scope's boards in page order.
func killLeaderBoards(kind string) []string {
	switch kind {
	case "":
		return []string{"alliance", "corporation", "character", "ship_lost", "ship_used", "system", "region", "kill"}
	case "region":
		return []string{"alliance", "corporation", "character", "ship_lost", "ship_used", "system", "kill"}
	case "system":
		return []string{"alliance", "corporation", "character", "ship_lost", "kill"}
	case "ship":
		return []string{"alliance", "corporation", "character"}
	case "alliance":
		return []string{"corporation", "character"}
	case "corporation":
		return []string{"character"}
	}
	return nil
}

// killLeaderCut is how deep a scope's boards go, as the worker cut
// them (queries_killleaders.sql).
func killLeaderCut(kind string) int32 {
	switch kind {
	case "":
		return 50
	case "system":
		return 5
	}
	return 10
}

const killLeaderOverviewRows = 5

type killLeaderRow struct {
	Rank    int
	ID      int64
	Name    string
	Img     string // the icon or logo URL, "" for places
	Href    string // the killboard feed this row opens
	TopHref string // this row's own leaderboard, "" when it has none
	TopName string
	Sub     string // a second line: a kill's victim, a system's region
	SecText string
	SecTier int
	HasSec  bool
	Count   int64
	Solo    int64
	ISK     string
}

type killLeaderBoard struct {
	Key   string
	Title string
	Note  string
	Count string
	ISK   string
	Rows  []killLeaderRow
	Href  string // the board in full, "" when the overview shows it all
}

type killLeadersView struct {
	Period     string
	PeriodText string
	ByISK      bool
	Periods    []killChip
	Ranks      []killChip
	ScopeKind  string
	ScopeID    int64
	ScopeName  string
	ScopeLabel string
	ScopeHref  string // the scope's killboard feed
	ClearHref  string
	Regions    []killOpt
	ShowRegion bool
	Hidden     []killOpt
	Board      string // one board in full, "" for the overview
	OverHref   string // back to the overview
	Boards     []killLeaderBoard
	Updated    string
	Warming    bool
}

// killLeadersHref builds a leaderboard URL.
func killLeadersHref(period string, byISK bool, kind string, id int64, board string) string {
	u := url.Values{}
	if kind != "" {
		u.Set(kind, strconv.FormatInt(id, 10))
	}
	if period != "" && period != "week" {
		u.Set("period", period)
	}
	if byISK {
		u.Set("rank", "isk")
	}
	if board != "" {
		u.Set("board", board)
	}
	if len(u) == 0 {
		return "/intel/kills/top/"
	}
	return "/intel/kills/top/?" + encodeKillQuery(u, kind)
}

func (app *Application) handleKillLeaders(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.publicPage(ctx)
	qs := r.URL.Query()
	v := &killLeadersView{Period: "week", ByISK: qs.Get("rank") == "isk"}
	for _, p := range killLeaderPeriods {
		if qs.Get("period") == p.Key {
			v.Period = p.Key
		}
	}
	// The form's empty region means New Eden: answer with the short
	// URL, as the killboard does.
	if qs.Has("region") && (qs.Get("region") == "" || qs.Get("region") == "0") {
		http.Redirect(w, r, killLeadersHref(v.Period, v.ByISK, "", 0, qs.Get("board")), http.StatusFound)
		return
	}
	for _, s := range []string{"region", "system", "ship", "alliance", "corporation"} {
		if n, err := strconv.ParseInt(qs.Get(s), 10, 64); err == nil && n > 0 {
			v.ScopeKind, v.ScopeID = s, n
			break
		}
	}
	boards := killLeaderBoards(v.ScopeKind)
	for _, b := range boards {
		if qs.Get("board") == b {
			v.Board = b
		}
	}
	names := app.newKillNames(ctx)
	for _, p := range killLeaderPeriods {
		if p.Key == v.Period {
			v.PeriodText = p.Label
		}
		v.Periods = append(v.Periods, killChip{Label: "Last " + p.Label, Href: killLeadersHref(p.Key, v.ByISK, v.ScopeKind, v.ScopeID, v.Board), Active: p.Key == v.Period})
	}
	v.Ranks = []killChip{
		{Label: "By kills", Href: killLeadersHref(v.Period, false, v.ScopeKind, v.ScopeID, v.Board), Active: !v.ByISK},
		{Label: "By ISK", Href: killLeadersHref(v.Period, true, v.ScopeKind, v.ScopeID, v.Board), Active: v.ByISK},
	}
	if v.ScopeKind != "" {
		v.ScopeName = names.scopeName(v.ScopeKind, v.ScopeID)
		v.ScopeLabel = map[string]string{"region": "Region", "system": "System", "ship": "Flying", "alliance": "Members of", "corporation": "Members of"}[v.ScopeKind]
		v.ScopeHref = "/intel/kills/?" + v.ScopeKind + "=" + strconv.FormatInt(v.ScopeID, 10)
		v.ClearHref = killLeadersHref(v.Period, v.ByISK, "", 0, "")
	}
	v.ShowRegion = v.ScopeKind == "" || v.ScopeKind == "region"
	if v.ShowRegion {
		sel := ""
		if v.ScopeKind == "region" {
			sel = strconv.FormatInt(v.ScopeID, 10)
		}
		for _, o := range app.killSDE(ctx).regions {
			o.Selected = o.Value == sel
			v.Regions = append(v.Regions, o)
		}
		if v.Period != "week" {
			v.Hidden = append(v.Hidden, killOpt{Value: "period", Label: v.Period})
		}
		if v.ByISK {
			v.Hidden = append(v.Hidden, killOpt{Value: "rank", Label: "isk"})
		}
		if v.Board != "" && v.Board != "region" {
			v.Hidden = append(v.Hidden, killOpt{Value: "board", Label: v.Board})
		}
	}
	if v.Board != "" {
		v.OverHref = killLeadersHref(v.Period, v.ByISK, v.ScopeKind, v.ScopeID, "")
	}

	states, err := app.queries.ListKillLeadersState(ctx)
	if err != nil {
		logging.Errorf("killboard leaders: state: %v", err)
	}
	for _, s := range states {
		if s.Period == v.Period {
			v.Updated = agoText(time.Since(s.ComputedAt))
		}
	}
	v.Warming = v.Updated == ""

	cut := killLeaderCut(v.ScopeKind)
	show := boards
	rows := int32(killLeaderOverviewRows)
	if v.Board != "" {
		show, rows = []string{v.Board}, cut
	}
	if !v.Warming {
		for _, key := range show {
			v.Boards = append(v.Boards, app.killLeaderBoard(ctx, names, v, key, rows, cut))
		}
	}

	data.KillLeaders = v
	title := "Killboard leaderboards"
	if v.ScopeName != "" {
		title = v.ScopeName + " · " + title
	}
	data.HeadMeta = app.HeadMeta(module.PageMeta{Title: title,
		Description:   "Top pilots, corporations, alliances, ships, systems and kills across New Eden over the last day, week and month, ranked by EveSynapse from every public killmail.",
		CanonicalPath: "/intel/kills/top/"})
	app.kickNameResolve()
	app.render(ctx, w, http.StatusOK, "killboard_top.html", data)
}

// killLeaderBoard reads one stored board and dresses its rows.
func (app *Application) killLeaderBoard(ctx context.Context, names *killNames, v *killLeadersView, key string, rows, cut int32) killLeaderBoard {
	def := killLeaderBoardDefs[key]
	b := killLeaderBoard{Key: key, Title: def.Title, Count: def.Count, ISK: def.ISK}
	if v.ScopeKind == "ship" {
		b.Note = "Kills where they flew a " + v.ScopeName
	}
	if v.Board == "" && cut > rows {
		b.Href = killLeadersHref(v.Period, v.ByISK, v.ScopeKind, v.ScopeID, key)
	}
	list, err := app.queries.ListKillLeaders(ctx, db.ListKillLeadersParams{
		Period: v.Period, ScopeKind: v.ScopeKind, ScopeID: v.ScopeID, Board: key, ByIsk: v.ByISK || key == "kill", MaxRows: rows,
	})
	if err != nil {
		logging.Errorf("killboard leaders: %s: %v", key, err)
		return b
	}
	var kills map[int64]db.Kill
	if key == "kill" && len(list) > 0 {
		ids := make([]int64, len(list))
		for i, r := range list {
			ids[i] = r.EntityID
		}
		kills = map[int64]db.Kill{}
		if ks, err := app.queries.ListPublicKillsByID(ctx, ids); err == nil {
			for _, k := range ks {
				kills[k.KillmailID] = k
			}
		}
	}
	id := func(n int64) string { return strconv.FormatInt(n, 10) }
	for i, r := range list {
		row := killLeaderRow{Rank: i + 1, ID: r.EntityID, Count: r.Kills, Solo: r.Solo, ISK: formatISKShort(r.Isk)}
		switch key {
		case "character":
			row.Name = names.character(r.EntityID)
			row.Img = "https://images.evetech.net/characters/" + id(r.EntityID) + "/portrait?size=64"
			row.Href = "/intel/kills/?character=" + id(r.EntityID)
		case "corporation":
			row.Name = names.corp(r.EntityID)
			row.Img = "https://images.evetech.net/corporations/" + id(r.EntityID) + "/logo?size=64"
			row.Href = "/intel/kills/?corporation=" + id(r.EntityID)
			row.TopHref = killLeadersHref(v.Period, v.ByISK, "corporation", r.EntityID, "")
		case "alliance":
			row.Name = names.alliance(r.EntityID)
			row.Img = "https://images.evetech.net/alliances/" + id(r.EntityID) + "/logo?size=64"
			row.Href = "/intel/kills/?alliance=" + id(r.EntityID)
			row.TopHref = killLeadersHref(v.Period, v.ByISK, "alliance", r.EntityID, "")
		case "ship_lost", "ship_used":
			row.Name = names.typeName(r.EntityID)
			row.Img = "https://images.evetech.net/types/" + id(r.EntityID) + "/icon?size=64"
			row.Href = "/intel/kills/?ship=" + id(r.EntityID)
			row.TopHref = killLeadersHref(v.Period, v.ByISK, "ship", r.EntityID, "")
		case "system":
			s := names.system(r.EntityID)
			row.Name = s.Name
			row.SecText, row.SecTier = secRound(s.Security)
			row.HasSec = true
			row.Sub = names.region(s.RegionID)
			row.Href = "/intel/kills/?system=" + id(r.EntityID)
			row.TopHref = killLeadersHref(v.Period, v.ByISK, "system", r.EntityID, "")
		case "region":
			row.Name = names.region(r.EntityID)
			row.Href = "/intel/kills/?region=" + id(r.EntityID)
			row.TopHref = killLeadersHref(v.Period, v.ByISK, "region", r.EntityID, "")
		case "kill":
			k, ok := kills[r.EntityID]
			if !ok {
				continue
			}
			kr := names.row(k)
			row.Name = kr.ShipName
			row.Img = "https://images.evetech.net/types/" + id(k.ShipTypeID) + "/icon?size=64"
			row.Href = "/intel/kill/" + id(r.EntityID) + "/"
			row.Sub = kr.VictimName
			if kr.VictimIsStructure || row.Sub == "" {
				row.Sub = kr.CorpName
			}
			row.Sub += " · " + kr.SystemName
		}
		if row.TopHref != "" {
			row.TopName = row.Name
		}
		b.Rows = append(b.Rows, row)
	}
	return b
}

// killLeaderRail is the leaders rail of a killboard scoped to a
// system, region or hull: its alliances, hulls lost and systems from
// the stored week, in place of New Eden's day. A hull's rail ranks
// the alliances and pilots flying it.
func (app *Application) killLeaderRail(ctx context.Context, names *killNames, kind string, id int64) killboardTop {
	top := killboardTop{Window: "last 7 days", LeadersHref: killLeadersHref("week", false, kind, id, "")}
	read := func(board string) []db.ListKillLeadersRow {
		rows, err := app.queries.ListKillLeaders(ctx, db.ListKillLeadersParams{Period: "week", ScopeKind: kind, ScopeID: id, Board: board, MaxRows: 8})
		if err != nil {
			logging.Errorf("killboard rail %s %d: %s: %v", kind, id, board, err)
		}
		return rows
	}
	for _, r := range read("alliance") {
		top.Alliances = append(top.Alliances, killTopRow{ID: r.EntityID, Name: names.alliance(r.EntityID), Count: r.Kills, Note: formatISKShort(r.Isk)})
	}
	if kind == "ship" {
		for _, r := range read("character") {
			top.Pilots = append(top.Pilots, killTopRow{ID: r.EntityID, Name: names.character(r.EntityID), Count: r.Kills})
		}
		return top
	}
	for _, r := range read("ship_lost") {
		top.Ships = append(top.Ships, killTopRow{ID: r.EntityID, Name: names.typeName(r.EntityID), Count: r.Kills})
	}
	if kind == "region" {
		for _, r := range read("system") {
			s := names.system(r.EntityID)
			sec, tier := secRound(s.Security)
			top.Systems = append(top.Systems, killTopRow{ID: r.EntityID, Name: s.Name, Count: r.Kills, Note: sec, Tier: tier})
		}
	}
	return top
}

// killLeadersScopeHref is the leaderboard for a killboard scope: New
// Eden's for a pilot, which has no board of its own.
func killLeadersScopeHref(kind string, id int64) string {
	switch kind {
	case "system", "region", "ship", "alliance", "corporation":
		return killLeadersHref("week", false, kind, id, "")
	}
	return "/intel/kills/top/"
}
