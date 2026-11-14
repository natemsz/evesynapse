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
// Days is how many EVE days of totals it adds up, today included; 0
// ranks the rolling last 24 hours.
type killLeaderPeriod struct {
	Key   string
	Label string
	Days  int
	Every time.Duration
}

var killLeaderPeriods = []killLeaderPeriod{
	{"day", "24 hours", 0, 10 * time.Minute},
	{"week", "7 days", 7, time.Hour},
	{"month", "30 days", 30, 6 * time.Hour},
}

// killPaddingValue is the loss under which a kill is padding for the
// leaderboards: an empty capsule, rookie ship, shuttle or bare hull,
// nothing fitted or plugged in.
const killPaddingValue = 1e6

const (
	// killLeadersLate is how long after a day ends it is totalled
	// once more, for the killmails that arrive late.
	killLeadersLate = 6 * time.Hour
	// killLeadersTodayFresh is how old today's totals may be when a
	// 7 or 30 day rank adds them up.
	killLeadersTodayFresh = 10 * time.Minute
)

// killLeadersMu keeps one ranking run at a time: the working set is
// one pair of tables.
var killLeadersMu sync.Mutex

// killLeadersRolling is where the rolling last 24 hours' totals sit:
// a day no kill is from.
var killLeadersRolling = time.Unix(0, 0).UTC()

// killLeadersBoardCodes are the boards' codes in kill_leaders_totals.
var killLeadersBoardCodes = map[string]int16{
	"character": 1, "corporation": 2, "alliance": 3, "ship_lost": 4, "ship_used": 5, "system": 6, "region": 7, "kill": 8,
}

func killLeadersDayKey(day time.Time) string { return day.Format("2006-01-02") }

// refreshKillLeaders does one unit of the ranking per killboard pass,
// the most urgent first: the 24 hours when due, then the 7 days and
// the 30 days when due, then a finished day that has no totals
// (newest first) or is due its late totalling. A 7 or 30 day rank
// waits until every day it adds up has totals, so it never ranks a
// partial window.
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
	ranked := map[string]time.Time{}
	for _, s := range states {
		ranked[s.Period] = s.ComputedAt
	}
	rows, err := app.queries.ListKillLeadersDays(ctx)
	if err != nil {
		logging.Errorf("killboard leaders: days: %v", err)
		return
	}
	totalled := map[string]time.Time{}
	for _, d := range rows {
		totalled[killLeadersDayKey(d.Day)] = d.ComputedAt
	}
	now := time.Now().UTC()
	today := now.Truncate(24 * time.Hour)
	due := func(p killLeaderPeriod) bool {
		last, ok := ranked[p.Key]
		return !ok || now.Sub(last) >= p.Every
	}
	logRun := func(what string, start time.Time, n int64) {
		logging.Infof("killboard leaders: %s (%d kills) in %s", what, n, time.Since(start).Round(time.Millisecond))
	}

	// The 24 hours: totalled straight from the kills, then ranked.
	if p := killLeaderPeriods[0]; due(p) {
		start := time.Now()
		n, err := app.totalKillLeaders(ctx, killLeadersRolling, now.Add(-24*time.Hour), now)
		if err == nil {
			err = app.rankKillLeaders(ctx, p, []time.Time{killLeadersRolling}, now.Add(-24*time.Hour), now, n)
		}
		if err != nil {
			logging.Errorf("killboard leaders: rank %s: %v", p.Key, err)
			return
		}
		logRun("ranked the last "+p.Label, start, n)
		return
	}

	longest := killLeaderPeriods[len(killLeaderPeriods)-1].Days

	// The 7 and 30 days: today's totals refreshed, then the days
	// added up.
	for _, p := range killLeaderPeriods[1:] {
		if !due(p) {
			continue
		}
		days := make([]time.Time, 0, p.Days)
		complete := true
		for d := 0; d < p.Days; d++ {
			day := today.AddDate(0, 0, -d)
			if _, ok := totalled[killLeadersDayKey(day)]; !ok && d > 0 {
				complete = false
			}
			days = append(days, day)
		}
		if !complete {
			continue
		}
		start := time.Now()
		if last, ok := totalled[killLeadersDayKey(today)]; !ok || now.Sub(last) >= killLeadersTodayFresh {
			if _, err := app.totalKillLeaders(ctx, today, today, now); err != nil {
				logging.Errorf("killboard leaders: total %s: %v", killLeadersDayKey(today), err)
				return
			}
		}
		if err := app.rankKillLeaders(ctx, p, days, today.AddDate(0, 0, 1-p.Days), now, 0); err != nil {
			logging.Errorf("killboard leaders: rank %s: %v", p.Key, err)
			return
		}
		logRun("ranked the last "+p.Label, start, 0)
		if err := app.queries.PruneKillLeadersDays(ctx, today.AddDate(0, 0, 1-longest)); err != nil {
			logging.Warnf("killboard leaders: prune: %v", err)
		}
		return
	}

	// A finished day without totals, or due its late totalling.
	for d := 1; d < longest; d++ {
		day := today.AddDate(0, 0, -d)
		end := day.Add(24 * time.Hour)
		b := killLeadersDayKey(day)
		last, ok := totalled[b]
		if ok && (!last.Before(end.Add(killLeadersLate)) || now.Before(end.Add(killLeadersLate))) {
			continue
		}
		start := time.Now()
		n, err := app.totalKillLeaders(ctx, day, day, end)
		if err != nil {
			logging.Errorf("killboard leaders: total %s: %v", b, err)
			return
		}
		logRun("totalled "+b, start, n)
		return
	}
}

// totalKillLeaders fills the working set with the kills of [since,
// until) and replaces one day's totals with theirs, in one
// transaction.
func (app *Application) totalKillLeaders(ctx context.Context, day, since, until time.Time) (int64, error) {
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
	n, err := q.FillKillLeadersWin(ctx, db.FillKillLeadersWinParams{Since: since, Until: until, PaddingValue: killPaddingValue})
	if err != nil {
		return 0, err
	}
	if err := q.FillKillLeadersAtt(ctx); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, "ANALYZE kill_leaders_win, kill_leaders_att"); err != nil {
		return 0, err
	}
	if err := q.DeleteKillLeadersDay(ctx, day); err != nil {
		return 0, err
	}
	for _, board := range []string{"character", "corporation", "alliance"} {
		if err := q.InsertKillLeadersTotalsEntity(ctx, db.InsertKillLeadersTotalsEntityParams{Day: day, Board: killLeadersBoardCodes[board]}); err != nil {
			return 0, fmt.Errorf("%s: %w", board, err)
		}
	}
	for _, board := range []string{"ship_lost", "system", "region"} {
		if err := q.InsertKillLeadersTotalsPlace(ctx, db.InsertKillLeadersTotalsPlaceParams{Day: day, Board: killLeadersBoardCodes[board]}); err != nil {
			return 0, fmt.Errorf("%s: %w", board, err)
		}
	}
	if err := q.InsertKillLeadersTotalsKills(ctx, day); err != nil {
		return 0, fmt.Errorf("kill: %w", err)
	}
	if err := q.InsertKillLeadersTotalsShipsUsed(ctx, day); err != nil {
		return 0, fmt.Errorf("ship_used: %w", err)
	}
	if err := q.ClearKillLeadersStage(ctx); err != nil {
		return 0, err
	}
	if err := q.UpsertKillLeadersDay(ctx, db.UpsertKillLeadersDayParams{Day: day, ComputedAt: time.Now().UTC()}); err != nil {
		return 0, err
	}
	return n, tx.Commit(ctx)
}

// rankKillLeaders replaces a period's ranks with its days added up, in one transaction: a page reads the old ranks until the new
// ones land.
func (app *Application) rankKillLeaders(ctx context.Context, p killLeaderPeriod, days []time.Time, since, now time.Time, kills int64) error {
	tx, err := app.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SET LOCAL work_mem = '128MB'"); err != nil {
		return err
	}
	q := db.New(tx)
	if err := q.DeleteKillLeadersPeriod(ctx, p.Key); err != nil {
		return err
	}
	if err := q.RankKillLeaders(ctx, db.RankKillLeadersParams{Period: p.Key, Days: days}); err != nil {
		return err
	}
	if err := q.UpsertKillLeadersState(ctx, db.UpsertKillLeadersStateParams{Period: p.Key, Since: since, ComputedAt: now, Kills: kills}); err != nil {
		return err
	}
	return tx.Commit(ctx)
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
