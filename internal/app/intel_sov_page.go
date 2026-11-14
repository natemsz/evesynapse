package app

// ---------------------------------------------------------------------------
// The sovereignty tracker page (/intel/sov/). It reads what the place
// stats pass stored (intel_sov.go, schema 154): the holder of every
// system with its claim detail, the change log and the live contests.
// Six bounded reads per render, one name lookup for every alliance on
// the page; nothing is computed from raw ESI here.
// ---------------------------------------------------------------------------

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

const (
	// sovRows caps every list on the page.
	sovRows = 100
	// sovShown is how many rows a list shows before its Show all
	// control; the rest render folded.
	sovShown = 10
	// sovAllianceCap bounds the alliance read behind the standings
	// and the alliance picker (every sov-holding alliance; about 70).
	sovAllianceCap = 300
	// sovAllianceRows is how many alliances the standings show.
	sovAllianceRows = 25
	// sovWindowAhead is how far ahead the vulnerability list looks.
	sovWindowAhead = 24 * time.Hour
)

// sovPageView is the tracker page.
type sovPageView struct {
	Warming bool
	Updated string // "4 minutes ago", when the holder set was stored

	// The filters: a region and an alliance, 0 for all.
	RegionID     int64
	AllianceID   int64
	AllianceName string
	Regions      []sovRegionOpt
	ClearHref    string // the page without the alliance filter

	Tiles []sovTile
	// Each list: the rows shown, the rows folded behind Show all,
	// and the total.
	Contests, ContestsMore []sovContestRow
	Vuln, VulnMore         []sovVulnRow
	Claims, ClaimsMore     []sovClaimRow
	Losses, LossesMore     []sovLossRow
	ContestTotal           int
	VulnTotal              int
	ClaimTotal             int
	LossTotal              int
	// HasPrev: some claim names the holder it was taken from.
	HasPrev bool
	// VulnChips switch the windows list between every window, the
	// open ones and the ones opening in the next 24 hours.
	VulnChips []sovChip
	// Alliances: the standings (top sovAllianceRows); AllianceOpts:
	// every holding alliance, for the picker.
	Alliances     []sovAllianceRow
	AllianceOpts  []sovAllianceOpt
	AllianceTotal int
	// LogSince is when the change log's first entry was written:
	// losses are only known from then on.
	LogSince string
	// Fights are the contests that ended in the last 7 days, with
	// what they cost (sov_fights.go).
	Fights, FightsMore []sovEndedFight
	FightTotal         int
}

// sovFightFig is a fight's figures: the kills, what they were worth,
// and the defender's share on each side.
type sovFightFig struct {
	ID        int64 // the contest, which the fight panel opens under
	Kills     int64
	ISK       string
	DefLost   int64
	DefLostV  string
	DefKilled int64
	DefKillV  string
	Href      string // the fight's kills on the killboard
}

// sovFightKillRow is one kill in a contest's fight panel.
type sovFightKillRow struct {
	Kill killRow
	Side int16 // 1 the defender lost it, 2 the defender scored it
}

// sovEndedFight is a contest that ended, with its fight's figures and
// the system's holder now.
type sovEndedFight struct {
	System   sovSystem
	Event    string
	Defender sovOrg
	Holder   sovOrg
	Held     bool // the defender still holds the system
	Ended    time.Time
	Ago      string
	Fight    sovFightFig
	Search   string
}

// sovFightFigOf dresses a fight's stored figures.
func sovFightFigOf(id, kills int64, isk float64, dl int64, dli float64, dk int64, dki float64) sovFightFig {
	return sovFightFig{
		ID: id, Kills: kills, ISK: formatISKShort(isk),
		DefLost: dl, DefLostV: formatISKShort(dli),
		DefKilled: dk, DefKillV: formatISKShort(dki),
		Href: "/intel/kills/?fight=" + strconv.FormatInt(id, 10),
	}
}

type sovChip struct {
	Label, Href string
	On          bool
}

type sovAllianceOpt struct {
	ID       int64
	Name     string
	Selected bool
}

type sovRegionOpt struct {
	ID       int64
	Name     string
	Selected bool
}

type sovTile struct {
	Label, Value, Sub, Href, Tone string
}

// sovSystem is a system cell: link, security pill and region.
type sovSystem struct {
	Place   placeRef
	SecText string
	SecTier int
	Region  string
}

// sovOrg is an alliance (or NPC faction) cell.
type sovOrg struct {
	ID      int64
	Name    string
	Faction bool // an NPC faction, not an alliance
	Href    string
}

type sovContestRow struct {
	CampaignID int64
	// Fight is the contest's sov fight: its kills so far
	// (sov_fights.go). The live script bumps the count.
	Fight    sovFightFig
	System   sovSystem
	Defender sovOrg
	Event    string // the structure, named only when it is not a hub
	ADMTier  string
	ADMPct   int
	Def, Att int // scores, percent
	DefValue string
	Leading  string // "defender" | "attackers" | "even"
	Starts   time.Time
	StartsIn string // server text, refreshed by the countdown script
	Started  bool
	ADM      string
	Search   string
}

type sovVulnRow struct {
	System   sovSystem
	Holder   sovOrg
	Open     bool
	Edge     time.Time // the window's close when open, else its opening
	EdgeText string
	ADM      string
	ADMTier  string // low | mid | high
	ADMPct   int    // 0 to 100 of the 1 to 6 scale, for the meter
	Capital  bool
	Levels   string // "Military 5 · Industrial 5 · Strategic 5"
	Search   string
}

type sovClaimRow struct {
	ShowPrev bool // the table has a Taken from column
	System   sovSystem
	Holder   sovOrg
	Prev     sovOrg
	Logged   bool
	At       time.Time
	Ago      string
	ADM      string
	Capital  bool
	Search   string
}

type sovLossRow struct {
	System sovSystem
	Lost   sovOrg
	Now    sovOrg
	At     time.Time
	Ago    string
	Search string
}

type sovAllianceRow struct {
	Rank     int
	Org      sovOrg
	Held     int64
	Gained   int64
	Lost     int64
	Share    int // percent of the largest holder, for the meter
	ADM      string
	Capitals int64
	Href     string // filter the page to this alliance
	On       bool
}

// handleSovTracker serves /intel/sov/.
func (app *Application) handleSovTracker(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.publicPage(ctx)
	q := r.URL.Query()
	regionID, _ := strconv.ParseInt(q.Get("region"), 10, 64)
	allianceID, _ := strconv.ParseInt(q.Get("alliance"), 10, 64)
	data.Sov = app.loadSovPage(ctx, max(regionID, 0), max(allianceID, 0), q.Get("window"), time.Now().UTC())
	data.HeadMeta = app.HeadMeta(module.PageMeta{
		Title:         "Sovereignty",
		Description:   "Who holds null-sec in EVE Online: live sovereignty contests, systems that changed hands, vulnerability windows and ADM, refreshed every few minutes.",
		CanonicalPath: "/intel/sov/",
		ImagePath:     sovCardPath(max(regionID, 0), max(allianceID, 0)),
	})
	app.render(ctx, w, http.StatusOK, "intel_sov.html", data)
}

// loadSovPage reads the tracker's blocks for one filter.
func (app *Application) loadSovPage(ctx context.Context, regionID, allianceID int64, window string, now time.Time) *sovPageView {
	v := &sovPageView{RegionID: regionID, AllianceID: allianceID}
	mode := int32(0)
	switch window {
	case "open":
		mode = 1
	case "soon":
		mode = 2
	default:
		window = ""
	}
	meta, err := app.queries.GetPlaceStatsMeta(ctx, placeStatsKindSovereignty)
	if err != nil {
		v.Warming = true
		return v
	}
	v.Updated = agoText(now.Sub(meta.FetchedAt))
	dayAgo, weekAgo := now.Add(-24*time.Hour), now.Add(-7*24*time.Hour)

	sum, sumErr := app.queries.SovSummary(ctx, db.SovSummaryParams{
		SinceDay: dayAgo, SinceWeek: weekAgo, Now: now, Until: now.Add(sovWindowAhead), RegionID: regionID, AllianceID: allianceID,
	})
	if sumErr != nil {
		logging.Errorf("sov: summary: %v", sumErr)
	}
	contests, err := app.queries.ListSovContests(ctx, db.ListSovContestsParams{RegionID: regionID, AllianceID: allianceID, MaxRows: sovRows})
	if err != nil {
		logging.Errorf("sov: contests: %v", err)
	}
	vuln, err := app.queries.ListSovVulnerable(ctx, db.ListSovVulnerableParams{
		Now: now, Until: now.Add(sovWindowAhead), Mode: mode, RegionID: regionID, AllianceID: allianceID, MaxRows: sovRows,
	})
	if err != nil {
		logging.Errorf("sov: vulnerable: %v", err)
	}
	claims, err := app.queries.ListSovRecentClaims(ctx, db.ListSovRecentClaimsParams{Since: weekAgo, RegionID: regionID, AllianceID: allianceID, MaxRows: sovRows})
	if err != nil {
		logging.Errorf("sov: claims: %v", err)
	}
	losses, err := app.queries.ListSovLosses(ctx, db.ListSovLossesParams{Since: weekAgo, RegionID: regionID, AllianceID: allianceID, MaxRows: sovRows})
	if err != nil {
		logging.Errorf("sov: losses: %v", err)
	}
	standings, err := app.queries.ListSovAlliances(ctx, db.ListSovAlliancesParams{Since: weekAgo, RegionID: regionID, MaxRows: sovAllianceCap})
	if err != nil {
		logging.Errorf("sov: alliances: %v", err)
	}
	regions, err := app.queries.ListSovRegions(ctx)
	if err != nil {
		logging.Errorf("sov: regions: %v", err)
	}
	fightStats := map[int64]db.ListSovFightStatsRow{}
	if len(contests) > 0 {
		cids := make([]int64, len(contests))
		for i, c := range contests {
			cids[i] = c.CampaignID
		}
		if rows, err := app.queries.ListSovFightStats(ctx, cids); err == nil {
			for _, r := range rows {
				fightStats[r.CampaignID] = r
			}
		} else {
			logging.Errorf("sov: fight figures: %v", err)
		}
	}
	ended, err := app.queries.ListSovFightsEnded(ctx, db.ListSovFightsEndedParams{Since: weekAgo, RegionID: regionID, DefenderID: allianceID, MaxRows: sovRows})
	if err != nil {
		logging.Errorf("sov: ended fights: %v", err)
	}
	if first, err := app.queries.SovChangeLogStart(ctx); err == nil && !first.IsZero() {
		v.LogSince = first.UTC().Format("2006-01-02")
	}

	// Every alliance the page names, in one lookup.
	ids := map[int64]bool{}
	add := func(id int64) {
		if id > 0 {
			ids[id] = true
		}
	}
	add(allianceID)
	for _, c := range contests {
		add(c.DefenderID)
	}
	for _, x := range vuln {
		add(x.AllianceID)
	}
	for _, c := range claims {
		add(c.AllianceID)
		add(c.PrevAllianceID)
	}
	for _, l := range losses {
		add(l.OldAllianceID)
		add(l.NewAllianceID)
	}
	for _, a := range standings {
		add(a.AllianceID)
	}
	for _, f := range ended {
		add(f.DefenderID)
		add(f.HolderID)
	}
	list := make([]int64, 0, len(ids))
	for id := range ids {
		list = append(list, id)
	}
	names := app.namesFor(ctx, esi.NameAlliance, list)
	alliance := func(id int64) sovOrg {
		if id <= 0 {
			return sovOrg{}
		}
		name := names[id]
		if name == "" {
			name = "Alliance #" + strconv.FormatInt(id, 10)
		}
		return sovOrg{ID: id, Name: name, Href: fmt.Sprintf("/alliance/?alliance=%d", id)}
	}
	if allianceID > 0 {
		v.AllianceName = alliance(allianceID).Name
	}

	href := func(region, alli int64) string {
		u := url.Values{}
		if region > 0 {
			u.Set("region", strconv.FormatInt(region, 10))
		}
		if alli > 0 {
			u.Set("alliance", strconv.FormatInt(alli, 10))
		}
		if window != "" {
			u.Set("window", window)
		}
		if len(u) == 0 {
			return "/intel/sov/"
		}
		return "/intel/sov/?" + u.Encode()
	}
	v.ClearHref = href(regionID, 0)
	chip := func(label, w string) sovChip {
		saved := window
		window = w
		h := href(regionID, allianceID) + "#sov-vuln"
		window = saved
		return sovChip{Label: label, Href: h, On: window == w}
	}
	v.VulnChips = []sovChip{chip("All", ""), chip("Open now", "open"), chip("Opening in 24h", "soon")}
	for _, r := range regions {
		v.Regions = append(v.Regions, sovRegionOpt{ID: r.RegionID, Name: r.Name, Selected: r.RegionID == regionID})
	}

	// The tiles.
	if sumErr == nil {
		heldSub := fmt.Sprintf("by %s alliances", thousands(sum.Alliances))
		if allianceID > 0 {
			heldSub = "by this alliance"
		}
		v.Tiles = []sovTile{
			{Label: "Systems held", Value: thousands(sum.Held), Sub: heldSub, Href: "#sov-standings"},
			{Label: "Changed hands, 24h", Value: thousands(sum.ClaimedDay), Sub: thousands(sum.ClaimedWeek) + " in 7 days", Href: "#sov-changes", Tone: toneIf(sum.ClaimedDay > 0, "warn")},
			{Label: "Live contests", Value: thousands(sum.Contests), Sub: contestSub(contests, sum.Contests), Href: "#sov-contests", Tone: toneIf(sum.Contests > 0, "hot")},
			{Label: "Vulnerable now", Value: thousands(sum.VulnerableNow), Sub: thousands(sum.VulnerableNext) + " more open in 24h", Href: "#sov-vuln"},
		}
	}

	for _, c := range contests {
		row := sovContestRow{
			System:   sovSystemOf(c.SystemID, c.SystemName, c.Security, c.RegionName),
			Defender: alliance(c.DefenderID),
			ADMTier:  admTier(c.Adm),
			ADMPct:   admPct(c.Adm),
			Def:      sovPct(c.DefenderScore),
			Att:      sovPct(c.AttackersScore),
			DefValue: strconv.FormatFloat(c.DefenderScore, 'f', 2, 64),
			Starts:   c.StartTime.UTC(),
			Started:  !c.StartTime.After(now),
			ADM:      admText(c.Adm),
		}
		if c.EventType != "ihub_defense" {
			row.Event = sovEventLabel(c.EventType)
		}
		row.CampaignID = c.CampaignID
		fs := fightStats[c.CampaignID]
		row.Fight = sovFightFigOf(c.CampaignID, fs.Kills, fs.Isk, fs.DefenderLost, fs.DefenderLostIsk, fs.DefenderKilled, fs.DefenderKilledIsk)
		switch {
		case row.Def > row.Att:
			row.Leading = "defender"
		case row.Att > row.Def:
			row.Leading = "attackers"
		default:
			row.Leading = "even"
		}
		if row.Started {
			row.StartsIn = durText(now.Sub(row.Starts))
		} else {
			row.StartsIn = "in " + durText(row.Starts.Sub(now))
		}
		row.Search = strings.ToLower(row.System.Place.Name + " " + row.System.Region + " " + row.Defender.Name + " " + sovEventLabel(c.EventType))
		v.Contests = append(v.Contests, row)
	}

	for _, x := range vuln {
		open := x.VulnStart.Valid && !x.VulnStart.Time.After(now)
		row := sovVulnRow{
			System:  sovSystemOf(x.SystemID, x.SystemName, x.Security, x.RegionName),
			Holder:  alliance(x.AllianceID),
			Open:    open,
			ADM:     admText(x.Adm),
			ADMTier: admTier(x.Adm),
			ADMPct:  admPct(x.Adm),
			Capital: x.IsCapital,
			Levels:  fmt.Sprintf("Military %d · Industrial %d · Strategic %d", x.MilitaryLevel, x.IndustrialLevel, x.StrategicLevel),
		}
		if open && x.VulnEnd.Valid {
			row.Edge = x.VulnEnd.Time.UTC()
		} else if x.VulnStart.Valid {
			row.Edge = x.VulnStart.Time.UTC()
		}
		row.EdgeText = "in " + durText(row.Edge.Sub(now))
		row.Search = strings.ToLower(row.System.Place.Name + " " + row.System.Region + " " + row.Holder.Name)
		v.Vuln = append(v.Vuln, row)
	}

	for _, c := range claims {
		row := sovClaimRow{
			System:  sovSystemOf(c.SystemID, c.SystemName, c.Security, c.RegionName),
			Holder:  alliance(c.AllianceID),
			Logged:  c.Logged,
			ADM:     admText(c.Adm),
			Capital: c.IsCapital,
		}
		if c.ClaimedSince.Valid {
			row.At = c.ClaimedSince.Time.UTC()
			row.Ago = agoText(now.Sub(row.At))
		}
		switch {
		case c.PrevAllianceID > 0:
			row.Prev = alliance(c.PrevAllianceID)
		case c.PrevFactionID > 0:
			row.Prev = sovOrg{Name: "NPC space", Faction: true}
		}
		row.Search = strings.ToLower(row.System.Place.Name + " " + row.System.Region + " " + row.Holder.Name + " " + row.Prev.Name)
		if row.Prev.Name != "" {
			v.HasPrev = true
		}
		v.Claims = append(v.Claims, row)
	}

	for _, l := range losses {
		row := sovLossRow{
			System: sovSystemOf(l.SystemID, l.SystemName, l.Security, l.RegionName),
			Lost:   alliance(l.OldAllianceID),
			At:     l.ChangedAt.UTC(),
			Ago:    agoText(now.Sub(l.ChangedAt)),
		}
		if l.NewAllianceID > 0 {
			row.Now = alliance(l.NewAllianceID)
		} else {
			row.Now = sovOrg{Name: "Unclaimed", Faction: true}
		}
		row.Search = strings.ToLower(row.System.Place.Name + " " + row.System.Region + " " + row.Lost.Name + " " + row.Now.Name)
		v.Losses = append(v.Losses, row)
	}

	var top int64
	for _, a := range standings {
		top = max(top, a.Held)
	}
	v.AllianceTotal = len(standings)
	for _, a := range standings {
		v.AllianceOpts = append(v.AllianceOpts, sovAllianceOpt{ID: a.AllianceID, Name: alliance(a.AllianceID).Name, Selected: a.AllianceID == allianceID})
	}
	sort.SliceStable(v.AllianceOpts, func(i, j int) bool {
		return strings.ToLower(v.AllianceOpts[i].Name) < strings.ToLower(v.AllianceOpts[j].Name)
	})
	for i, a := range standings {
		if i >= sovAllianceRows && a.AllianceID != allianceID {
			continue
		}
		row := sovAllianceRow{
			Rank:     i + 1,
			Org:      alliance(a.AllianceID),
			Held:     a.Held,
			Gained:   a.Gained,
			Lost:     a.Lost,
			ADM:      admText(a.AvgAdm),
			Capitals: a.Capitals,
			Href:     href(regionID, a.AllianceID),
			On:       a.AllianceID == allianceID,
		}
		if top > 0 {
			row.Share = int(math.Round(float64(a.Held) / float64(top) * 100))
		}
		v.Alliances = append(v.Alliances, row)
	}

	for _, f := range ended {
		row := sovEndedFight{
			System:   sovSystemOf(f.SystemID, f.SystemName, f.Security, f.RegionName),
			Event:    sovEventLabel(f.EventType),
			Defender: alliance(f.DefenderID),
			Holder:   alliance(f.HolderID),
			Held:     f.HolderID > 0 && f.HolderID == f.DefenderID,
			Fight:    sovFightFigOf(f.CampaignID, f.Kills, f.Isk, f.DefenderLost, f.DefenderLostIsk, f.DefenderKilled, f.DefenderKilledIsk),
		}
		if f.HolderID == 0 {
			row.Holder = sovOrg{Name: "Unclaimed", Faction: true}
		}
		if f.EndedAt.Valid {
			row.Ended = f.EndedAt.Time.UTC()
			row.Ago = agoText(now.Sub(row.Ended))
		}
		row.Search = strings.ToLower(row.System.Place.Name + " " + row.System.Region + " " + row.Defender.Name + " " + row.Holder.Name)
		v.Fights = append(v.Fights, row)
	}

	// Each list shows its first rows; the rest fold behind Show all.
	v.ContestTotal, v.VulnTotal, v.ClaimTotal, v.LossTotal = len(v.Contests), len(v.Vuln), len(v.Claims), len(v.Losses)
	v.Contests, v.ContestsMore = foldRows(v.Contests)
	v.Vuln, v.VulnMore = foldRows(v.Vuln)
	for i := range v.Claims {
		v.Claims[i].ShowPrev = v.HasPrev
	}
	v.Claims, v.ClaimsMore = foldRows(v.Claims)
	v.Losses, v.LossesMore = foldRows(v.Losses)
	v.FightTotal = len(v.Fights)
	v.Fights, v.FightsMore = foldRows(v.Fights)
	return v
}

// foldRows splits a list into the rows shown and the rows behind its
// Show all control.
func foldRows[T any](rows []T) (shown, more []T) {
	if len(rows) <= sovShown {
		return rows, nil
	}
	return rows[:sovShown], rows[sovShown:]
}

func sovSystemOf(id int64, name string, sec float64, region string) sovSystem {
	text, tier := secRound(sec)
	return sovSystem{Place: placeRef{Name: name, SystemID: id}, SecText: text, SecTier: tier, Region: region}
}

// sovEventLabel names a contest's structure in plain words.
func sovEventLabel(t string) string {
	switch t {
	case "ihub_defense":
		return "Sovereignty hub"
	case "tcu_defense":
		return "Claim unit"
	case "station_defense":
		return "Station"
	case "station_freeport":
		return "Freeport station"
	}
	return strings.ReplaceAll(t, "_", " ")
}

func contestSub(rows []db.ListSovContestsRow, total int64) string {
	if total == 0 {
		return "None under way"
	}
	ahead := 0
	for _, c := range rows {
		if c.AttackersScore > c.DefenderScore {
			ahead++
		}
	}
	if ahead == 0 {
		return "Defenders lead in all"
	}
	return fmt.Sprintf("Attackers lead in %d", ahead)
}

func toneIf(ok bool, tone string) string {
	if ok {
		return tone
	}
	return ""
}

func sovPct(f float64) int { return int(math.Round(math.Max(0, math.Min(1, f)) * 100)) }

// admText is an activity defense multiplier as the game shows it.
func admText(f float64) string {
	if f <= 0 {
		return "—"
	}
	return strconv.FormatFloat(f, 'f', 1, 64)
}

// admTier bands the 1 to 6 multiplier: a low ADM is a short timer, an
// easy target.
func admTier(f float64) string {
	switch {
	case f < 2:
		return "low"
	case f < 4:
		return "mid"
	}
	return "high"
}

func admPct(f float64) int {
	return int(math.Round(math.Max(0, math.Min(1, (f-1)/5)) * 100))
}

// durText is a short duration: "2h 10m", "35m", "3d 4h".
func durText(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	m := int(d.Round(time.Minute) / time.Minute)
	switch {
	case m >= 24*60:
		return fmt.Sprintf("%dd %dh", m/(24*60), (m%(24*60))/60)
	case m >= 60:
		return fmt.Sprintf("%dh %dm", m/60, m%60)
	}
	return fmt.Sprintf("%dm", m)
}

// agoText is a past moment: "just now", "12 minutes ago", "3 hours
// ago", "2 days ago".
func agoText(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return sovPlural(int(d/time.Minute), "minute") + " ago"
	case d < 48*time.Hour:
		return sovPlural(int(d/time.Hour), "hour") + " ago"
	}
	return sovPlural(int(d/(24*time.Hour)), "day") + " ago"
}

func sovPlural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return strconv.Itoa(n) + " " + unit + "s"
}

func thousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// allianceSovView is the alliance page's sovereignty block: the
// alliance's holdings at a glance, its capital and the systems it
// holds, best defended first. Nil when it holds none.
type allianceSovView struct {
	AllianceID  int64
	Held        string
	Gained      int64
	Lost        int64
	ADM         string
	Systems     []sovVulnRow
	More        int64 // systems held past the list
	TrackerHref string
}

// allianceSovShown caps the alliance page's system list.
const allianceSovShown = 12

// loadAllianceSov reads one alliance's sovereignty: two bounded reads.
func (app *Application) loadAllianceSov(ctx context.Context, allianceID int64, now time.Time) *allianceSovView {
	sum, err := app.queries.AllianceSovSummary(ctx, db.AllianceSovSummaryParams{AllianceID: allianceID, Since: now.Add(-7 * 24 * time.Hour)})
	if err != nil || sum.Held == 0 {
		return nil
	}
	rows, err := app.queries.ListAllianceSovSystems(ctx, db.ListAllianceSovSystemsParams{AllianceID: allianceID, MaxRows: allianceSovShown})
	if err != nil {
		return nil
	}
	v := &allianceSovView{
		AllianceID: allianceID, Held: thousands(sum.Held), Gained: sum.Gained, Lost: sum.Lost,
		ADM: admText(sum.AvgAdm), TrackerHref: fmt.Sprintf("/intel/sov/?alliance=%d", allianceID),
	}
	if extra := sum.Held - int64(len(rows)); extra > 0 {
		v.More = extra
	}
	for _, x := range rows {
		row := sovVulnRow{
			System: sovSystemOf(x.SystemID, x.SystemName, x.Security, x.RegionName),
			ADM:    admText(x.Adm), ADMTier: admTier(x.Adm), ADMPct: admPct(x.Adm),
			Capital: x.IsCapital,
		}
		if x.VulnStart.Valid && x.VulnEnd.Valid {
			row.Open = !x.VulnStart.Time.After(now) && x.VulnEnd.Time.After(now)
			if row.Open {
				row.Edge = x.VulnEnd.Time.UTC()
			} else {
				row.Edge = x.VulnStart.Time.UTC()
			}
			row.EdgeText = "in " + durText(row.Edge.Sub(now))
		}
		v.Systems = append(v.Systems, row)
	}
	return v
}

// sovFightKillsShown is how many kills a contest's fight panel lists;
// the killboard holds the rest.
const sovFightKillsShown = 20

// sovFightPanel is a contest's fight panel: its figures and newest
// kills.
type sovFightPanel struct {
	Fight sovFightFig
	Name  string
	Ended bool
	Kills []sovFightKillRow
}

// handleSovFight is GET /intel/sov/fight/{id}/: the panel that opens
// under a contest on the tracker, as a fragment. It reads the fight's
// stored figures and its newest kills.
func (app *Application) handleSovFight(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return
	}
	f, err := app.queries.GetSovFightFigures(ctx, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	p := sovFightPanel{
		Fight: sovFightFigOf(id, f.Kills, f.Isk, f.DefenderLost, f.DefenderLostIsk, f.DefenderKilled, f.DefenderKilledIsk),
		Name:  f.SystemName, Ended: f.EndedAt.Valid,
	}
	rows, err := app.queries.ListSovFightKillsOf(ctx, db.ListSovFightKillsOfParams{CampaignID: id, MaxRows: sovFightKillsShown})
	if err != nil {
		logging.Errorf("sov: fight %d kills: %v", id, err)
	}
	names := app.newKillNames(ctx)
	for _, k := range rows {
		p.Kills = append(p.Kills, sovFightKillRow{Kill: names.row(k.Kill), Side: k.Side})
	}
	buf, err := executeFragment("intel_sov.html", "sov-fight-panel", p)
	if err != nil {
		logging.Errorf("sov: fight %d panel: %v", id, err)
		http.Error(w, "The fight could not be shown.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(buf.Bytes())
}
