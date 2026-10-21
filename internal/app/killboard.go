package app

// ---------------------------------------------------------------------------
// The killboard pages (Intel): the feed at /intel/kills/ and one kill
// at /intel/kill/{id}/. Everything here reads the stored kills
// (killfeed.go) and the local name caches; nothing fetches at render.
// Only public kills are ever shown; private ones (our characters'
// kills nobody posted) count in the headline totals and nowhere else.
//
// The feed takes one scope -- ?system=, ?region=, ?ship=,
// ?character=, ?corporation= or ?alliance= (kills and losses alike for
// an entity) -- plus two narrowings, ?big=1 (a billion ISK and up) and
// ?solo=1, and pages by keyset (?before=<unix>.<killmail id>). Names
// the caches lack are resolved in bulk by the maintenance pass
// (warmKillNames) and show as a plain placeholder until then.
// ---------------------------------------------------------------------------

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

const (
	killPageSize   = 40
	killBigValue   = 1e9
	killStatsCache = time.Minute
)

// killNames resolves the names one render needs from the caches and
// the SDE, each looked up once.
type killNames struct {
	app     *Application
	ctx     context.Context
	systems map[int64]db.GetSDESystemRow
	regions map[int64]string
	types   map[int64]string
}

func (app *Application) newKillNames(ctx context.Context) *killNames {
	return &killNames{app: app, ctx: ctx, systems: map[int64]db.GetSDESystemRow{}, regions: map[int64]string{}, types: map[int64]string{}}
}

func (n *killNames) character(id int64) string {
	if id <= 0 {
		return ""
	}
	if s, ok := n.app.esi.CachedCharacterName(id); ok && s != "" {
		return s
	}
	return "Pilot #" + strconv.FormatInt(id, 10)
}

func (n *killNames) corp(id int64) string {
	if id <= 0 {
		return ""
	}
	if s, ok := n.app.esi.CachedCorpName(id); ok && s != "" {
		return s
	}
	return "Corporation #" + strconv.FormatInt(id, 10)
}

func (n *killNames) alliance(id int64) string {
	if id <= 0 {
		return ""
	}
	if s, ok := n.app.esi.CachedAllianceName(id); ok && s != "" {
		return s
	}
	return "Alliance #" + strconv.FormatInt(id, 10)
}

func (n *killNames) typeName(id int64) string {
	if id <= 0 {
		return ""
	}
	if s, ok := n.types[id]; ok {
		return s
	}
	s := n.app.esi.CachedTypeName(n.ctx, id)
	n.types[id] = s
	return s
}

func (n *killNames) system(id int64) db.GetSDESystemRow {
	if s, ok := n.systems[id]; ok {
		return s
	}
	s, _ := n.app.queries.GetSDESystem(n.ctx, id)
	n.systems[id] = s
	return s
}

func (n *killNames) region(id int64) string {
	if s, ok := n.regions[id]; ok {
		return s
	}
	r, _ := n.app.queries.GetSDERegion(n.ctx, id)
	n.regions[id] = r.Name
	return r.Name
}

// killRow is one kill in the feed.
type killRow struct {
	ID                int64
	ShipTypeID        int64
	ShipName          string
	VictimID          int64
	VictimName        string
	CorpID            int64
	CorpName          string
	AllianceID        int64
	AllianceName      string
	SystemID          int64
	SystemName        string
	SecText           string
	SecTier           int
	RegionID          int64
	RegionName        string
	Value             string // short ISK, "" while unpriced
	Big               bool
	When              string // "14:02" EVE time
	Ago               string
	Time              time.Time
	Attackers         int
	FinalName         string
	FinalID           int64
	Solo, NPC, Awox   bool
	VictimIsStructure bool
}

func (n *killNames) row(k db.Kill) killRow {
	sys := n.system(k.SystemID)
	sec, tier := secRound(sys.Security)
	r := killRow{
		ID: k.KillmailID, ShipTypeID: k.ShipTypeID, ShipName: n.typeName(k.ShipTypeID),
		VictimID: k.VictimCharacterID, VictimName: n.character(k.VictimCharacterID),
		CorpID: k.VictimCorporationID, CorpName: n.corp(k.VictimCorporationID),
		AllianceID: k.VictimAllianceID, AllianceName: n.alliance(k.VictimAllianceID),
		SystemID: k.SystemID, SystemName: sys.Name, SecText: sec, SecTier: tier,
		RegionID: k.RegionID, RegionName: n.region(k.RegionID),
		When: k.KillmailTime.UTC().Format("15:04"), Ago: ageText(time.Now(), k.KillmailTime),
		Time: k.KillmailTime, Attackers: int(k.AttackerCount),
		FinalID: k.FbCharacterID, FinalName: n.character(k.FbCharacterID),
		Solo: k.Solo, NPC: k.Npc, Awox: k.Awox,
	}
	if r.VictimID == 0 {
		// A structure, a deployable or an NPC loss: the corp is the victim.
		r.VictimIsStructure = true
		r.VictimName = r.ShipName
	}
	if k.ValueOurs.Valid {
		r.Value = formatISKShort(k.ValueOurs.Float64)
		r.Big = k.ValueOurs.Float64 >= killBigValue
	}
	return r
}

// killboardView is the /intel/kills/ page.
type killboardView struct {
	Kills     []killRow
	ScopeKind string // "", "system", "region", "ship", "character", "corporation", "alliance"
	ScopeID   int64
	ScopeName string
	Big, Solo bool
	Older     string // the next page's URL, "" at the end
	Stats     killboardStats
	Top       killboardTop
	BaseQuery string // the scope part of the URL, for the filter chips
	// The narrowing chips, and the scope pill's label and its
	// clear link (which keeps the narrowing).
	Chips      []killChip
	ScopeLabel string
	ClearHref  string
}

type killChip struct {
	Label  string
	Href   string
	Active bool
}

var killScopeLabels = map[string]string{
	"system": "System", "region": "Region", "ship": "Ship",
	"character": "Pilot", "corporation": "Corporation", "alliance": "Alliance",
}

// killHref builds a feed URL from a scope and the narrowings.
func killHref(scope string, big, solo bool) string {
	var q []string
	if scope != "" {
		q = append(q, scope)
	}
	if big {
		q = append(q, "big=1")
	}
	if solo {
		q = append(q, "solo=1")
	}
	if len(q) == 0 {
		return "/intel/kills/"
	}
	return "/intel/kills/?" + strings.Join(q, "&")
}

type killboardStats struct {
	KillsHour, KillsDay int64
	ISKDay              string
	Biggest             *killRow
}

type killboardTop struct {
	Alliances []killTopRow
	Ships     []killTopRow
	Systems   []killTopRow
}

type killTopRow struct {
	ID    int64
	Name  string
	Count int64
	Note  string
	Tier  int // a system's security color step
}

// killboardStatsMemo holds the headline and leaderboards a minute:
// every feed render would otherwise aggregate the day.
type killboardStatsMemo struct {
	at    time.Time
	stats killboardStats
	top   killboardTop
}

var killScopes = []string{"system", "region", "ship", "character", "corporation", "alliance"}

// handleKillboard renders the feed.
func (app *Application) handleKillboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.publicPage(ctx)
	v := &killboardView{}
	qs := r.URL.Query()
	for _, s := range killScopes {
		if id, err := strconv.ParseInt(qs.Get(s), 10, 64); err == nil && id > 0 {
			v.ScopeKind, v.ScopeID = s, id
			break
		}
	}
	v.Big = qs.Get("big") == "1"
	v.Solo = qs.Get("solo") == "1"
	beforeTime, beforeID := time.Now().Add(time.Hour).UTC(), int64(1<<62)
	if b := qs.Get("before"); b != "" {
		if dot := strings.IndexByte(b, '.'); dot > 0 {
			if sec, err := strconv.ParseInt(b[:dot], 10, 64); err == nil {
				if id, err := strconv.ParseInt(b[dot+1:], 10, 64); err == nil {
					beforeTime, beforeID = time.Unix(sec, 0).UTC(), id
				}
			}
		}
	}
	minValue := 0.0
	if v.Big {
		minValue = killBigValue
	}
	kills, err := app.listKills(ctx, v.ScopeKind, v.ScopeID, beforeTime, beforeID, minValue, v.Solo)
	if err != nil {
		logging.Errorf("killboard: list: %v", err)
	}
	names := app.newKillNames(ctx)
	for _, k := range kills {
		v.Kills = append(v.Kills, names.row(k))
	}
	if v.ScopeKind != "" {
		v.ScopeName = names.scopeName(v.ScopeKind, v.ScopeID)
		v.ScopeLabel = killScopeLabels[v.ScopeKind]
		v.BaseQuery = v.ScopeKind + "=" + strconv.FormatInt(v.ScopeID, 10)
		v.ClearHref = killHref("", v.Big, v.Solo)
	}
	v.Chips = []killChip{
		{Label: "All kills", Href: killHref(v.BaseQuery, false, false), Active: !v.Big && !v.Solo},
		{Label: "Big kills", Href: killHref(v.BaseQuery, !v.Big, v.Solo), Active: v.Big},
		{Label: "Solo", Href: killHref(v.BaseQuery, v.Big, !v.Solo), Active: v.Solo},
	}
	if len(kills) == killPageSize {
		last := kills[len(kills)-1]
		q := []string{}
		if v.BaseQuery != "" {
			q = append(q, v.BaseQuery)
		}
		if v.Big {
			q = append(q, "big=1")
		}
		if v.Solo {
			q = append(q, "solo=1")
		}
		q = append(q, fmt.Sprintf("before=%d.%d", last.KillmailTime.Unix(), last.KillmailID))
		v.Older = "/intel/kills/?" + strings.Join(q, "&")
	}
	v.Stats, v.Top = app.killboardHeadline(ctx, names)
	data.Killboard = v
	title := "Killboard"
	if v.ScopeName != "" {
		title = v.ScopeName + " · Killboard"
	}
	data.HeadMeta = app.HeadMeta(module.PageMeta{Title: title, Description: "Live kills across New Eden: every ship lost, priced by EveSynapse's own appraisal.", CanonicalPath: "/intel/kills/"})
	app.render(ctx, w, http.StatusOK, "killboard.html", data)
}

func (app *Application) listKills(ctx context.Context, kind string, id int64, bt time.Time, bid int64, minValue float64, solo bool) ([]db.Kill, error) {
	q := app.queries
	switch kind {
	case "system":
		return q.ListKillsBySystem(ctx, db.ListKillsBySystemParams{SystemID: id, BeforeTime: bt, BeforeID: bid, MinValue: minValue, SoloOnly: solo, MaxRows: killPageSize})
	case "region":
		return q.ListKillsByRegion(ctx, db.ListKillsByRegionParams{RegionID: id, BeforeTime: bt, BeforeID: bid, MinValue: minValue, SoloOnly: solo, MaxRows: killPageSize})
	case "ship":
		return q.ListKillsByShip(ctx, db.ListKillsByShipParams{ShipTypeID: id, BeforeTime: bt, BeforeID: bid, MinValue: minValue, SoloOnly: solo, MaxRows: killPageSize})
	case "character":
		return q.ListKillsByCharacter(ctx, db.ListKillsByCharacterParams{EntityID: id, BeforeTime: bt, BeforeID: bid, MinValue: minValue, SoloOnly: solo, MaxRows: killPageSize})
	case "corporation":
		return q.ListKillsByCorporation(ctx, db.ListKillsByCorporationParams{EntityID: id, BeforeTime: bt, BeforeID: bid, MinValue: minValue, SoloOnly: solo, MaxRows: killPageSize})
	case "alliance":
		return q.ListKillsByAlliance(ctx, db.ListKillsByAllianceParams{EntityID: id, BeforeTime: bt, BeforeID: bid, MinValue: minValue, SoloOnly: solo, MaxRows: killPageSize})
	}
	return q.ListKillsRecent(ctx, db.ListKillsRecentParams{BeforeTime: bt, BeforeID: bid, MinValue: minValue, SoloOnly: solo, MaxRows: killPageSize})
}

func (n *killNames) scopeName(kind string, id int64) string {
	switch kind {
	case "system":
		return n.system(id).Name
	case "region":
		return n.region(id)
	case "ship":
		return n.typeName(id)
	case "character":
		return n.character(id)
	case "corporation":
		return n.corp(id)
	case "alliance":
		return n.alliance(id)
	}
	return ""
}

// killboardHeadline is the feed's totals and leaderboards for the
// last day, held a minute.
func (app *Application) killboardHeadline(ctx context.Context, names *killNames) (killboardStats, killboardTop) {
	app.killRegionsMu.Lock()
	memo := app.killStatsMemo
	app.killRegionsMu.Unlock()
	if memo != nil && time.Since(memo.at) < killStatsCache {
		return memo.stats, memo.top
	}
	now := time.Now().UTC()
	day := now.Add(-24 * time.Hour)
	var st killboardStats
	if s, err := app.queries.GetKillStats(ctx, db.GetKillStatsParams{HourAgo: now.Add(-time.Hour), DayAgo: day}); err == nil {
		st.KillsHour, st.KillsDay = s.KillsHour, s.KillsDay
		st.ISKDay = formatISKShort(s.IskDay)
	}
	if k, err := app.queries.GetBiggestKill(ctx, day); err == nil {
		row := names.row(k)
		st.Biggest = &row
	}
	var top killboardTop
	if rows, err := app.queries.TopKillerAlliances(ctx, db.TopKillerAlliancesParams{Since: day, MaxRows: 8}); err == nil {
		for _, r := range rows {
			top.Alliances = append(top.Alliances, killTopRow{ID: r.AllianceID, Name: names.alliance(r.AllianceID), Count: r.Kills, Note: formatISKShort(r.Isk)})
		}
	}
	if rows, err := app.queries.TopShipsLost(ctx, db.TopShipsLostParams{Since: day, MaxRows: 8}); err == nil {
		for _, r := range rows {
			top.Ships = append(top.Ships, killTopRow{ID: r.ShipTypeID, Name: names.typeName(r.ShipTypeID), Count: r.Losses})
		}
	}
	if rows, err := app.queries.TopKillSystems(ctx, db.TopKillSystemsParams{Since: day, MaxRows: 8}); err == nil {
		for _, r := range rows {
			s := names.system(r.SystemID)
			sec, tier := secRound(s.Security)
			top.Systems = append(top.Systems, killTopRow{ID: r.SystemID, Name: s.Name, Count: r.Kills, Note: sec, Tier: tier})
		}
	}
	app.killRegionsMu.Lock()
	app.killStatsMemo = &killboardStatsMemo{at: time.Now(), stats: st, top: top}
	app.killRegionsMu.Unlock()
	return st, top
}

// --- One kill ---

// killItemRow is one line of the victim's fitting or cargo.
type killItemRow struct {
	TypeID  int64
	Name    string
	Qty     int64
	Dropped bool
	Value   string
	Depth   int
	Copy    bool // a blueprint copy
}

type killSlotGroup struct {
	Label string
	Items []killItemRow
}

type killAttackerRow struct {
	CharacterID  int64
	Name         string
	CorpID       int64
	CorpName     string
	AllianceID   int64
	AllianceName string
	ShipTypeID   int64
	ShipName     string
	WeaponTypeID int64
	WeaponName   string
	Damage       string
	Share        int // percent of the damage taken
	FinalBlow    bool
	NPC          bool
}

// killPageView is /intel/kill/{id}/.
type killPageView struct {
	Row           killRow
	ShipGroup     string
	Damage        string
	Points        int32
	ValueFull     string
	DroppedFull   string
	DestroyedFull string
	DroppedPct    int
	ZkillValue    string
	Coverage      int // percent of lines our appraisal priced, 100 when full
	Priced        bool
	Slots         []killSlotGroup
	Attackers     []killAttackerRow
	Detailed      bool // the full killmail is still held (a year)
}

// killSlot names the slot group an inventory flag belongs to, and
// its order on the page.
func killSlot(flag int64) (string, int) {
	switch {
	case flag >= 27 && flag <= 34:
		return "High slots", 0
	case flag >= 19 && flag <= 26:
		return "Mid slots", 1
	case flag >= 11 && flag <= 18:
		return "Low slots", 2
	case flag >= 92 && flag <= 99:
		return "Rigs", 3
	case flag >= 125 && flag <= 132:
		return "Subsystems", 4
	case flag >= 164 && flag <= 171:
		return "Service slots", 5
	case flag == 87:
		return "Drone bay", 6
	case flag == 158 || (flag >= 159 && flag <= 163):
		return "Fighters", 7
	case flag == 5:
		return "Cargo", 8
	case flag == 89:
		return "Implants", 10
	case flag == 90 || flag == 155 || (flag >= 133 && flag <= 154) || flag == 176 || flag == 177:
		return "Holds and hangars", 9
	}
	return "Other", 11
}

// handleKillPage renders one public kill.
func (app *Application) handleKillPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.publicPage(ctx)
	id, err := strconv.ParseInt(strings.Trim(strings.TrimPrefix(r.URL.Path, "/intel/kill/"), "/"), 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return
	}
	k, err := app.queries.GetPublicKill(ctx, id)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			logging.Errorf("killboard: kill %d: %v", id, err)
		}
		// Unknown, or a kill nobody posted: the page says so.
		data.KillPage = &killPageView{}
		app.render(ctx, w, http.StatusNotFound, "kill.html", data)
		return
	}
	names := app.newKillNames(ctx)
	v := &killPageView{Row: names.row(k), Points: k.Points}
	v.Damage = esi.FormatInt(k.DamageTaken)
	if k.ValueOurs.Valid {
		v.Priced = true
		v.ValueFull = formatISKShort(k.ValueOurs.Float64)
		v.DroppedFull = formatISKShort(k.ValueDroppedOurs.Float64)
		v.DestroyedFull = formatISKShort(k.ValueOurs.Float64 - k.ValueDroppedOurs.Float64)
		if k.ValueOurs.Float64 > 0 {
			v.DroppedPct = int(k.ValueDroppedOurs.Float64/k.ValueOurs.Float64*100 + 0.5)
		}
		v.Coverage = int(float64(k.ValueCoverage.Float64)*100 + 0.5)
	}
	if k.ValueZkill.Valid {
		v.ZkillValue = formatISKShort(k.ValueZkill.Float64)
	}
	if g, err := app.queries.ListTypeGroupNames(ctx, []int64{k.ShipTypeID}); err == nil && len(g) == 1 {
		v.ShipGroup = g[0].Name
	}
	if payload, err := app.queries.GetKillDetail(ctx, id); err == nil {
		if km, ok := decodeKillDetail(payload); ok {
			v.Detailed = true
			app.fillKillDetail(ctx, names, v, km, k.DamageTaken)
		}
	}
	data.KillPage = v
	title := v.Row.ShipName
	if !v.Row.VictimIsStructure {
		title = v.Row.VictimName + " · " + v.Row.ShipName
	}
	desc := fmt.Sprintf("%s lost in %s, %s.", v.Row.ShipName, v.Row.SystemName, v.Row.RegionName)
	if v.ValueFull != "" {
		desc = fmt.Sprintf("%s lost in %s, %s: %s ISK.", v.Row.ShipName, v.Row.SystemName, v.Row.RegionName, v.ValueFull)
	}
	data.HeadMeta = app.HeadMeta(module.PageMeta{Title: title, Description: desc, CanonicalPath: fmt.Sprintf("/intel/kill/%d/", id)})
	app.render(ctx, w, http.StatusOK, "kill.html", data)
}

func decodeKillDetail(gz []byte) (killmailFull, bool) {
	var km killmailFull
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return km, false
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		return km, false
	}
	return km, json.Unmarshal(raw, &km) == nil
}

// fillKillDetail lays out the fitting by slot and the attackers by
// damage, items priced at the same unit prices the kill's value
// used.
func (app *Application) fillKillDetail(ctx context.Context, names *killNames, v *killPageView, km killmailFull, damageTaken int64) {
	price := map[int64]float64{}
	types := map[int64]bool{}
	var walk func(items []killItem)
	walk = func(items []killItem) {
		for _, it := range items {
			types[it.ItemTypeID] = true
			walk(it.Items)
		}
	}
	walk(km.Victim.Items)
	if len(types) > 0 {
		price = app.killUnitPrices(ctx, types)
	}
	groups := map[string]*killSlotGroup{}
	order := map[string]int{}
	add := func(flag int64, row killItemRow) {
		label, o := killSlot(flag)
		g, ok := groups[label]
		if !ok {
			g = &killSlotGroup{Label: label}
			groups[label] = g
			order[label] = o
		}
		g.Items = append(g.Items, row)
	}
	var lines func(items []killItem, flag int64, depth int)
	lines = func(items []killItem, flag int64, depth int) {
		for _, it := range items {
			f := it.Flag
			if depth > 0 {
				f = flag // a container's contents sit with the container
			}
			for _, part := range []struct {
				qty     int64
				dropped bool
			}{{it.QuantityDestroyed, false}, {it.QuantityDropped, true}} {
				if part.qty <= 0 {
					continue
				}
				row := killItemRow{TypeID: it.ItemTypeID, Name: names.typeName(it.ItemTypeID), Qty: part.qty,
					Dropped: part.dropped, Depth: depth, Copy: it.Singleton == 2}
				if p, ok := price[it.ItemTypeID]; ok && !row.Copy {
					row.Value = formatISKShort(p * float64(part.qty))
				}
				add(f, row)
			}
			lines(it.Items, f, depth+1)
		}
	}
	lines(km.Victim.Items, 0, 0)
	for _, g := range groups {
		v.Slots = append(v.Slots, *g)
	}
	sort.Slice(v.Slots, func(i, j int) bool { return order[v.Slots[i].Label] < order[v.Slots[j].Label] })

	atk := km.Attackers
	sort.SliceStable(atk, func(i, j int) bool {
		if atk[i].FinalBlow != atk[j].FinalBlow {
			return atk[i].FinalBlow
		}
		return atk[i].DamageDone > atk[j].DamageDone
	})
	for _, a := range atk {
		row := killAttackerRow{
			CharacterID: a.CharacterID, Name: names.character(a.CharacterID),
			CorpID: a.CorporationID, CorpName: names.corp(a.CorporationID),
			AllianceID: a.AllianceID, AllianceName: names.alliance(a.AllianceID),
			ShipTypeID: a.ShipTypeID, ShipName: names.typeName(a.ShipTypeID),
			WeaponTypeID: a.WeaponTypeID, WeaponName: names.typeName(a.WeaponTypeID),
			Damage: esi.FormatInt(a.DamageDone), FinalBlow: a.FinalBlow, NPC: a.CharacterID == 0,
		}
		if row.NPC {
			row.Name = row.ShipName
		}
		if damageTaken > 0 {
			row.Share = int(float64(a.DamageDone)/float64(damageTaken)*100 + 0.5)
		}
		v.Attackers = append(v.Attackers, row)
	}
}

// killUnitPrices prices a kill's item types through the house order.
func (app *Application) killUnitPrices(ctx context.Context, types map[int64]bool) map[int64]float64 {
	out := map[int64]float64{}
	lines := make([]module.AppraisalLine, 0, len(types))
	for id := range types {
		lines = append(lines, module.AppraisalLine{TypeID: id, Quantity: 1, Included: true})
	}
	res, err := app.AppraiseLines(ctx, lines)
	if err != nil {
		return out
	}
	for _, l := range res.Lines {
		if l.Priced {
			out[l.TypeID] = l.UnitPrice
		}
	}
	return out
}

// warmKillNames resolves, in bulk, the names recent public kills
// will show and the caches lack: up to three /universe/names calls a
// pass. Unresolved IDs stay as placeholders until a later pass.
func (app *Application) warmKillNames(ctx context.Context) {
	ids, err := app.queries.ListRecentKillEntities(ctx, time.Now().UTC().Add(-2*time.Hour))
	if err != nil {
		return
	}
	var missing []int64
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, ok := app.esi.CachedCharacterName(id); ok {
			continue
		}
		if _, ok := app.esi.CachedCorpName(id); ok {
			continue
		}
		if _, ok := app.esi.CachedAllianceName(id); ok {
			continue
		}
		missing = append(missing, id)
		if len(missing) >= 3*universeNamesChunk {
			break
		}
	}
	if len(missing) == 0 {
		return
	}
	got, _ := app.resolveUniverseNamesLimited(ctx, &fetchBudget{left: 6}, missing)
	if len(got) > 0 {
		logging.Debugf("killboard: resolved %d of %d names", len(got), len(missing))
	}
}
