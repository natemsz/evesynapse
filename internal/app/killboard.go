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
	"context"
	"errors"
	"fmt"
	json "github.com/goccy/go-json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
	"github.com/jackc/pgx/v5"
)

const (
	killPageSize = 40
	killBigValue = 1e9
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
	if s, ok := n.app.pilotName(id); ok && s != "" {
		return s
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
	if s, ok := n.app.corpName(id); ok && s != "" {
		return s
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
	if s, ok := n.app.allianceName(id); ok && s != "" {
		return s
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
	if sys, ok := n.app.sdeSystem(id); ok {
		row := db.GetSDESystemRow{
			SystemID: sys.ID,
			Name:     sys.Name,
			RegionID: sys.RegionID,
			Security: sys.Security,
		}
		n.systems[id] = row
		return row
	}
	s, _ := n.app.queries.GetSDESystem(n.ctx, id)
	n.systems[id] = s
	return s
}

func (n *killNames) region(id int64) string {
	if s, ok := n.regions[id]; ok {
		return s
	}
	if rName, ok := n.app.sdeRegion(id); ok && rName != "" {
		n.regions[id] = rName
		return rName
	}
	r, _ := n.app.queries.GetSDERegion(n.ctx, id)
	n.regions[id] = r.Name
	return r.Name
}

// killRow is one kill in the feed. The JSON tags carry it over the
// live feed (livefeed.go): the browser builds a new row from the
// same fields the server renders.
type killRow struct {
	ID                int64     `json:"id"`
	ShipTypeID        int64     `json:"shipType"`
	ShipName          string    `json:"ship"`
	VictimID          int64     `json:"victim"`
	VictimName        string    `json:"victimName"`
	CorpID            int64     `json:"corp"`
	CorpName          string    `json:"corpName"`
	AllianceID        int64     `json:"alliance"`
	AllianceName      string    `json:"allianceName"`
	SystemID          int64     `json:"system"`
	SystemName        string    `json:"systemName"`
	SecText           string    `json:"sec"`
	SecTier           int       `json:"secTier"`
	RegionID          int64     `json:"region"`
	RegionName        string    `json:"regionName"`
	Value             string    `json:"value"` // short ISK, "" while unpriced
	Big               bool      `json:"big"`
	When              string    `json:"when"` // "14:02" EVE time
	Ago               string    `json:"ago"`
	Time              time.Time `json:"time"`
	Attackers         int       `json:"attackers"`
	FinalName         string    `json:"finalName"`
	FinalID           int64     `json:"final"`
	FinalCorpID       int64     `json:"finalCorp"`
	FinalCorpName     string    `json:"finalCorpName"`
	FinalAllianceID   int64     `json:"finalAlliance"`
	FinalAllianceName string    `json:"finalAllianceName"`
	Solo              bool      `json:"solo"`
	NPC               bool      `json:"npc"`
	Awox              bool      `json:"awox"`
	VictimIsStructure bool      `json:"structure"`
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
		FinalCorpID: k.FbCorporationID, FinalCorpName: n.corp(k.FbCorporationID),
		FinalAllianceID: k.FbAllianceID, FinalAllianceName: n.alliance(k.FbAllianceID),
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
	// ScopeProfileHref links the scope banner's name to the
	// scoped pilot's profile; only a character scope has one,
	// the other kinds keep the plain strong name.
	ScopeProfileHref string
	// SearchQ is a search-box query that resolved to nothing; the
	// page names it so a miss does not read as an empty board.
	SearchQ string
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

	// A scoped entity headline. The global fields above stay
	// zero on one of these, and these stay zero on the global
	// headline.
	Scoped          bool
	AriaLabel       string
	ScopeNote       string
	ScopeUpdatedAgo string
	Kills           int64
	Solo            int64
	HasSolo         bool
	Losses          int64
	ISKDestroyed    string
}

type killboardTop struct {
	Alliances []killTopRow
	TopShips  []killTopRow // a scoped entity's ships flown, on its kills
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
	// The search form's path (no script): a name in q resolves
	// through the same merged lookup the suggestions use, and a
	// hit goes straight to its scoped feed. No hit renders the
	// board with the query named under the bar.
	if v.ScopeKind == "" {
		if q := strings.TrimSpace(qs.Get("q")); q != "" {
			if hits, _ := app.killboardSearch(ctx, q, app.userID(ctx)); len(hits) > 0 {
				http.Redirect(w, r, "/intel/kills/?"+hits[0].Kind+"="+strconv.FormatInt(hits[0].ID, 10), http.StatusFound)
				return
			}
			v.SearchQ = q
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
		if v.ScopeKind == "character" {
			v.ScopeProfileHref = "/pilot/?character=" + strconv.FormatInt(v.ScopeID, 10)
		}
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
	v.Stats, v.Top = app.killboardBoardStats(ctx, names, v.ScopeKind, v.ScopeID, v.ScopeName)
	data.Killboard = v
	title := "Killboard"
	if v.ScopeName != "" {
		title = v.ScopeName + " · Killboard"
	}
	data.HeadMeta = app.HeadMeta(module.PageMeta{Title: title, Description: "Live kills across New Eden: every ship lost, priced by EveSynapse's own appraisal.", CanonicalPath: "/intel/kills/"})
	app.kickNameResolve()
	app.render(ctx, w, http.StatusOK, "killboard.html", data)
}

func (app *Application) listKills(ctx context.Context, kind string, id int64, bt time.Time, bid int64, minValue float64, solo bool) ([]db.Kill, error) {
	if kind == "" {
		if kills, ok := app.getRecentKills(ctx, bt, bid, minValue, solo, killPageSize); ok {
			return kills, nil
		}
	}
	q := app.queries
	switch kind {
	case "system":
		return q.ListKillsBySystem(ctx, db.ListKillsBySystemParams{SystemID: id, BeforeTime: bt, BeforeID: bid, MinValue: minValue, SoloOnly: solo, MaxRows: killPageSize})
	case "region":
		return q.ListKillsByRegion(ctx, db.ListKillsByRegionParams{RegionID: id, BeforeTime: bt, BeforeID: bid, MinValue: minValue, SoloOnly: solo, MaxRows: killPageSize})
	case "ship":
		return q.ListKillsByShip(ctx, db.ListKillsByShipParams{ShipTypeID: id, BeforeTime: bt, BeforeID: bid, MinValue: minValue, SoloOnly: solo, MaxRows: killPageSize})
	case "character", "corporation", "alliance":
		return q.ListKillsByEntityFeed(ctx, db.ListKillsByEntityFeedParams{Kind: kind, EntityID: id, BeforeTime: bt, BeforeID: bid, MinValue: minValue, SoloOnly: solo, MaxRows: killPageSize})
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
	st := killboardStats{AriaLabel: "The last day across New Eden"}
	h, err := app.queries.GetKillboardHeadline(ctx)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			logging.Errorf("killboard: headline: %v", err)
		}
		return st, killboardTop{}
	}
	st.KillsHour, st.KillsDay = h.KillsHour, h.KillsDay
	st.ISKDay = formatISKShort(h.IskDay)
	if h.BiggestKillmailID > 0 {
		if k, err := app.queries.GetPublicKill(ctx, h.BiggestKillmailID); err == nil {
			row := names.row(k)
			st.Biggest = &row
		}
	}
	var top killboardTop
	if rows, err := app.queries.ListKillboardTop(ctx, db.ListKillboardTopParams{Kind: "alliance", MaxRows: 8}); err == nil {
		for _, r := range rows {
			top.Alliances = append(top.Alliances, killTopRow{ID: r.EntityID, Name: names.alliance(r.EntityID), Count: r.Kills, Note: formatISKShort(r.Isk)})
		}
	}
	if rows, err := app.queries.ListKillboardTop(ctx, db.ListKillboardTopParams{Kind: "ship", MaxRows: 8}); err == nil {
		for _, r := range rows {
			top.Ships = append(top.Ships, killTopRow{ID: r.EntityID, Name: names.typeName(r.EntityID), Count: r.Kills})
		}
	}
	if rows, err := app.queries.ListKillboardTop(ctx, db.ListKillboardTopParams{Kind: "system", MaxRows: 8}); err == nil {
		for _, r := range rows {
			s := names.system(r.EntityID)
			sec, tier := secRound(s.Security)
			top.Systems = append(top.Systems, killTopRow{ID: r.EntityID, Name: s.Name, Count: r.Kills, Note: sec, Tier: tier})
		}
	}
	return st, top
}

// killStatsEntity reports whether a scope kind has a headline of its
// own; every other scope (none, a system, a region, a ship) shows New
// Eden's.
func killStatsEntity(kind string) bool {
	return kind == "character" || kind == "corporation" || kind == "alliance"
}

// killboardBoardStats is a board's headline and leaders rail: the
// scoped entity's own for a character, corporation or alliance,
// New Eden's for every other scope. The page render and the live
// stats event (publishKillboardStats) both read it, from stored
// rows only.
func (app *Application) killboardBoardStats(ctx context.Context, names *killNames, kind string, id int64, scopeName string) (killboardStats, killboardTop) {
	if !killStatsEntity(kind) {
		return app.killboardHeadline(ctx, names)
	}
	st, top := app.killboardScopedHeadline(ctx, names, kind, id)
	st.AriaLabel = scopeName + ": " + st.ScopeNote
	if st.ScopeUpdatedAgo != "" {
		st.AriaLabel += " · updated " + st.ScopeUpdatedAgo
	}
	return st, top
}

// scopedBiggestRow renders an entity's biggest kill. The
// scoped queries rank on the PilotKillStats value basis (our
// price, else zKillboard's figure), so a kill ranked on the
// fallback figure shows that figure rather than nothing.
func scopedBiggestRow(names *killNames, k db.Kill) *killRow {
	if !k.ValueOurs.Valid && k.ValueZkill.Valid {
		k.ValueOurs = k.ValueZkill
	}
	row := names.row(k)
	return &row
}

// killboardScopedHeadline is a character, corporation or
// alliance board's headline: the scoped entity's own figures
// instead of New Eden's. A character with a stored activity
// profile reads kills and losses from the profile and the
// biggest kill from the all-time board read; solo kills and
// ISK destroyed come from the all-time stats read over the
// permanent involvement record, since the profile carries
// neither. A character without a profile, and corporations
// and alliances, read the 90-day board window. The rail
// reads the 90-day window for all three kinds. Held a
// minute per entity, like the global headline.
func (app *Application) killboardScopedHeadline(ctx context.Context, names *killNames, kind string, id int64) (killboardStats, killboardTop) {
	now := time.Now().UTC()
	since := now.Add(-pilotIntelWindow)
	st := killboardStats{Scoped: true, ScopeNote: "last 90 days"}
	// A profiled pilot's stored profile backs the headline: its
	// kills and losses cover every recorded kill, kept current at
	// ingest. Solo and ISK have no profile column; they come from
	// the stored daily aggregates, over all time like the profile.
	if kind == "character" {
		if act := app.loadPilotActivity(ctx, id, names, now); act != nil && act.Any {
			st.Kills, st.Losses = act.Kills, act.Losses
			st.ScopeNote = "every recorded kill and loss"
			st.ScopeUpdatedAgo = act.UpdatedAgo
			if s, err := app.queries.GetKillEntityStats(ctx, db.GetKillEntityStatsParams{Kind: kind, EntityID: id, Since: time.Unix(0, 0).UTC()}); err == nil {
				st.Solo, st.HasSolo = s.Solo, true
				if s.IskKilled > 0 {
					st.ISKDestroyed = formatISKShort(s.IskKilled)
				}
			} else {
				logging.Warnf("killboard scoped %s %d: stats: %v", kind, id, err)
			}
			st.Biggest = app.killEntityBiggest(ctx, names, kind, id, time.Unix(0, 0).UTC())
			return st, app.killEntityTop(ctx, names, kind, id, time.Unix(0, 0).UTC())
		}
	}
	if s, err := app.queries.GetKillEntityStats(ctx, db.GetKillEntityStatsParams{Kind: kind, EntityID: id, Since: since}); err == nil {
		st.Kills, st.Solo, st.Losses = s.Kills, s.Solo, s.Losses
		st.HasSolo = true
		if s.IskKilled > 0 {
			st.ISKDestroyed = formatISKShort(s.IskKilled)
		}
	} else {
		logging.Warnf("killboard scoped %s %d: stats: %v", kind, id, err)
	}
	st.Biggest = app.killEntityBiggest(ctx, names, kind, id, since)
	return st, app.killEntityTop(ctx, names, kind, id, since)
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
	Coverage      int // percent of lines our appraisal priced, 100 when full
	Priced        bool
	// BuildPriced: at least one retained line price came from
	// the build-cost tier, so the page asterisks the value and
	// labels it (my ruling, 2026-11-12).
	BuildPriced bool
	Slots       []killSlotGroup
	Attackers   []killAttackerRow
	Detailed    bool   // the full killmail is still held (a year)
	Fit         string // the lost fitting as EFT text, "" without the full killmail
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
		if !errors.Is(err, pgx.ErrNoRows) {
			logging.Errorf("killboard: kill %d: %v", id, err)
		}
		// Unknown, or a kill nobody posted: the page says so.
		data.KillPage = &killPageView{}
		app.render(ctx, w, http.StatusNotFound, "kill.html", data)
		return
	}
	names := app.newKillNames(ctx)
	v := app.killPageFor(ctx, names, k)
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
	app.kickNameResolve()
	app.render(ctx, w, http.StatusOK, "kill.html", data)
}

// killPageFor builds one kill's view from its board row: the value
// split, the ship group, and the fitting and attackers while the full
// killmail is held. The kill page and the Killmails inspector share it.
func (app *Application) killPageFor(ctx context.Context, names *killNames, k db.Kill) *killPageView {
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
	if g, err := app.queries.ListTypeGroupNames(ctx, []int64{k.ShipTypeID}); err == nil && len(g) == 1 {
		v.ShipGroup = g[0].Name
	}
	if payload, err := app.queries.GetKillDetail(ctx, k.KillmailID); err == nil {
		if km, ok := decodeKillDetail(payload); ok {
			v.Detailed = true
			app.fillKillDetail(ctx, names, v, km, k.DamageTaken)
			v.Fit = killFitEFT(names, v.Row.ShipName, km.Victim.Items)
		}
	}
	return v
}

// killFitEFT writes the lost fitting as EFT text, the format the
// fitting tool and the game's own import read: low, mid and high
// slots, rigs and subsystems, then drones and cargo with counts. A
// slot's module comes first and its loaded charge after a comma.
func killFitEFT(names *killNames, ship string, items []killItem) string {
	type line struct {
		id  int64
		qty int64
	}
	byFlag := map[int64][]line{}
	for _, it := range items {
		byFlag[it.Flag] = append(byFlag[it.Flag], line{it.ItemTypeID, it.QuantityDestroyed + it.QuantityDropped})
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[%s, Lost %s]\n", ship, ship)
	racks := [][2]int64{{11, 18}, {19, 26}, {27, 34}, {92, 99}, {125, 132}}
	for i, rack := range racks {
		if i > 0 {
			b.WriteString("\n")
		}
		for f := rack[0]; f <= rack[1]; f++ {
			ls := byFlag[f]
			if len(ls) == 0 {
				continue
			}
			sort.SliceStable(ls, func(a, c int) bool { return ls[a].qty < ls[c].qty })
			b.WriteString(names.typeName(ls[0].id))
			if len(ls) > 1 {
				b.WriteString(", " + names.typeName(ls[1].id))
			}
			b.WriteString("\n")
		}
	}
	// Drones, then fighters (the bay and the five launch tubes as one
	// block), then cargo, each type once with its total count.
	for _, flags := range [][]int64{{87}, {158, 159, 160, 161, 162, 163}, {5}} {
		var order []int64
		total := map[int64]int64{}
		for _, f := range flags {
			for _, l := range byFlag[f] {
				if _, ok := total[l.id]; !ok {
					order = append(order, l.id)
				}
				total[l.id] += l.qty
			}
		}
		if len(order) == 0 {
			continue
		}
		b.WriteString("\n")
		for _, id := range order {
			fmt.Fprintf(&b, "%s x%d\n", names.typeName(id), total[id])
		}
	}
	return b.String()
}

// killFitEFTFor is a public kill's lost fitting as EFT text, for the
// workbench's ?kill= link; false when the kill is not public or its
// full killmail is no longer kept.
func (app *Application) killFitEFTFor(ctx context.Context, killID int64) (string, bool) {
	k, err := app.queries.GetPublicKill(ctx, killID)
	if err != nil {
		return "", false
	}
	payload, err := app.queries.GetKillDetail(ctx, k.KillmailID)
	if err != nil {
		return "", false
	}
	km, ok := decodeKillDetail(payload)
	if !ok {
		return "", false
	}
	names := app.newKillNames(ctx)
	return killFitEFT(names, names.typeName(k.ShipTypeID), km.Victim.Items), true
}

func decodeKillDetail(payload []byte) (killmailFull, bool) {
	var km killmailFull
	raw, err := killDetailRaw(payload)
	if err != nil {
		return km, false
	}
	return km, json.Unmarshal(raw, &km) == nil
}

// fillKillDetail lays out the fitting by slot and the attackers by
// damage, items priced at the same unit prices the kill's value
// used.
func (app *Application) fillKillDetail(ctx context.Context, names *killNames, v *killPageView, km killmailFull, damageTaken int64) {
	// Stored at ingest, read here: a render never reprices.
	price := map[int64]float64{}
	if rows, err := app.queries.GetKillLinePrices(ctx, km.KillmailID); err == nil {
		for _, r := range rows {
			price[r.TypeID] = r.UnitPrice
			if r.Tier == module.AppraisalTierBuildCost {
				v.BuildPriced = true
			}
		}
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

// killUnitPrices prices a kill's item types through the house
// order, and reports the tier each price came from: a line
// priced at the build-cost tier is stored with its tier so the
// kill page can asterisk the value it feeds.
func (app *Application) killUnitPrices(ctx context.Context, types map[int64]bool) (map[int64]float64, map[int64]string, error) {
	out := map[int64]float64{}
	tiers := map[int64]string{}
	lines := make([]module.AppraisalLine, 0, len(types))
	for id := range types {
		lines = append(lines, module.AppraisalLine{TypeID: id, Quantity: 1, Included: true})
	}
	res, err := app.AppraiseLines(ctx, lines)
	if err != nil {
		return out, tiers, err
	}
	for _, l := range res.Lines {
		if l.Priced {
			out[l.TypeID] = l.UnitPrice
			tiers[l.TypeID] = l.Tier
		}
	}
	return out, tiers, nil
}

// warmKillNames resolves, in bulk, the names recent public kills
// will show and the caches lack: up to three /universe/names calls a
// pass. Unresolved IDs stay as placeholders until a later pass.
func (app *Application) warmKillNames(ctx context.Context) {
	ids, err := app.queries.ListRecentKillEntities(ctx, time.Now().UTC().Add(-2*time.Hour))
	if err != nil {
		return
	}
	app.resolveEntityNames(ctx, ids)
}

// killNameBackedOff reports whether the ID was asked about
// within the last hour and ESI had no answer for it.
func (app *Application) killNameBackedOff(id int64) bool {
	app.killNameMissesMu.Lock()
	defer app.killNameMissesMu.Unlock()
	t, bad := app.killNameMisses[id]
	return bad && time.Since(t) < time.Hour
}

// markKillNameMiss records that the ID was asked about and ESI
// had no answer, so it is not re-asked within the hour.
func (app *Application) markKillNameMiss(id int64) {
	app.killNameMissesMu.Lock()
	defer app.killNameMissesMu.Unlock()
	if app.killNameMisses == nil {
		app.killNameMisses = map[int64]time.Time{}
	}
	app.killNameMisses[id] = time.Now()
}

// resolveEntityNames settles a set of entity IDs into the shared
// name caches in bulk: IDs already cached, or in the one-hour
// no-answer backoff, are skipped; the rest go through the bulk
// names path, bounded per pass, and the ones ESI does not answer
// enter the backoff. The recent-kills warming pass and the
// render-noted drain share it, so both keep the same bookkeeping.
func (app *Application) resolveEntityNames(ctx context.Context, ids []int64) {
	seen := make(map[int64]bool, len(ids))
	var missing []int64
	for _, id := range ids {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		if _, ok := app.esi.CachedCharacterName(id); ok {
			continue
		}
		if _, ok := app.esi.CachedCorpName(id); ok {
			continue
		}
		if _, ok := app.esi.CachedAllianceName(id); ok {
			continue
		}
		if app.killNameBackedOff(id) {
			continue // asked within the hour; EVE had no answer
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
	for _, id := range missing {
		if _, ok := got[id]; !ok {
			app.markKillNameMiss(id)
		}
	}
	if len(got) > 0 {
		logging.Debugf("killboard: resolved %d of %d names", len(got), len(missing))
	}
}

// resolveNotedNames drains the name caches' miss registry: the
// IDs pages asked for and the caches did not hold. Resolving
// them here is what makes a name a page showed as a placeholder
// settle because it was viewed, on any page that reads the
// caches. The killboard maintenance cycle calls it as a
// backstop; renders kick it directly (kickNameResolve).
func (app *Application) resolveNotedNames(ctx context.Context) {
	ids := app.esi.DrainNameMisses()
	if len(ids) == 0 {
		return
	}
	app.resolveEntityNames(ctx, ids)
}

// kickNameResolve starts the noted-names pass in the background
// when a render left misses in the registry. Single-flight: a
// kick while a pass is running is a no-op (that pass drained
// already, or the misses wait for the next kick or the worker
// cycle). The render never waits on ESI. Without a worker
// context (a bare test literal) there is no background lifecycle
// to run the pass on, so nothing is kicked; tests drive
// resolveNotedNames directly.
func (app *Application) kickNameResolve() {
	if app.workerCtx == nil || !app.esi.NameMissesPending() {
		return
	}
	app.nameResolveMu.Lock()
	if app.nameResolveRunning {
		app.nameResolveMu.Unlock()
		return
	}
	app.nameResolveRunning = true
	app.nameResolveMu.Unlock()
	go func() {
		defer func() {
			app.nameResolveMu.Lock()
			app.nameResolveRunning = false
			app.nameResolveMu.Unlock()
		}()
		app.resolveNotedNames(app.workerCtx)
	}()
}
