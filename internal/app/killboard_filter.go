package app

// The killboard's filters (/intel/kills/): security band, ship
// class, fleet size, value, kills or losses, a place or hull that
// narrows another scope, and a jump to a date. A filtered page reads
// a bounded window of the scope's newest kills (killScanCap) through
// the scope's own index and keeps the ones that pass; a page that
// comes up short continues behind the window, so no filter ever
// walks a scope's whole history. The live stream applies the same
// filters in memory, from the SDE index below.

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// killScanCap is how many of a scope's kills one filtered page reads.
const killScanCap = 1000

// killScopeOrder is the order a URL's scope parameters are read in:
// the first one present is the page's scope and rides its index; a
// hull or place after it narrows that scope. A pilot, corporation or
// alliance scope narrows by hull and place, a hull by place.
var killScopeOrder = []string{"fight", "character", "corporation", "alliance", "ship", "system", "region"}

// killOpt is one choice in a filter picker.
type killOpt struct {
	Value    string
	Label    string
	Selected bool
}

type killOptGroup struct {
	Label string
	Opts  []killOpt
}

type killValueStep struct {
	key   string
	label string
	min   float64
}

var (
	killBands = []killOpt{
		{Value: "high", Label: "High-sec"}, {Value: "low", Label: "Low-sec"}, {Value: "null", Label: "Null-sec"},
		{Value: "wh", Label: "Wormhole space"}, {Value: "pochven", Label: "Pochven"}, {Value: "abyssal", Label: "Abyssal space"},
	}
	// The solo choice is the Solo chip, the 1B step the Big kills
	// chip: one meaning each, two ways to pick it.
	killFleets = []killOpt{
		{Value: "solo", Label: "Solo"}, {Value: "gang", Label: "2 to 10 attackers"},
		{Value: "fleet", Label: "11 to 50 attackers"}, {Value: "blob", Label: "Over 50 attackers"},
	}
	killValueSteps = []killValueStep{
		{"10m", "10M ISK and up", 1e7}, {"100m", "100M ISK and up", 1e8}, {"1b", "1B ISK and up", killBigValue},
		{"10b", "10B ISK and up", 1e10}, {"100b", "100B ISK and up", 1e11},
	}
	killSides = []killOpt{{Value: "kills", Label: "Kills only"}, {Value: "losses", Label: "Losses only"}}
)

// killFilter is the narrowing on top of a page's scope. The Big and
// Solo chips stay on the view; Value and Fleet hold the other steps.
type killFilter struct {
	Band       string
	GroupID    int64
	Fleet      string // "gang", "fleet" or "blob"
	Value      string // a killValueSteps key other than "1b"
	Side       string // "kills" or "losses", on a pilot, corporation or alliance scope
	SystemID   int64
	RegionID   int64
	ShipTypeID int64
	Date       time.Time // kills up to the end of this day, EVE time
}

// filtering reports whether the page needs the filtered read: the
// date alone only moves the starting point.
func (f killFilter) filtering() bool {
	return f.Band != "" || f.GroupID > 0 || f.Fleet != "" || f.Value != "" || f.Side != "" ||
		f.SystemID > 0 || f.RegionID > 0 || f.ShipTypeID > 0
}

func (f killFilter) minValue() float64 {
	for _, s := range killValueSteps {
		if s.key == f.Value {
			return s.min
		}
	}
	return 0
}

func (f killFilter) attackers() (lo, hi int32) {
	switch f.Fleet {
	case "gang":
		return 2, 10
	case "fleet":
		return 11, 50
	case "blob":
		return 51, 0
	}
	return 0, 0
}

func (f killFilter) side() int32 {
	switch f.Side {
	case "kills":
		return 1
	case "losses":
		return 2
	}
	return 0
}

// parseKillQuery reads a killboard URL: the scope, the Big and Solo
// chips (or their picker steps), and the filters.
func parseKillQuery(qs url.Values) (kind string, id int64, big, solo bool, f killFilter) {
	ids := map[string]int64{}
	for _, s := range killScopeOrder {
		if n, err := strconv.ParseInt(qs.Get(s), 10, 64); err == nil && n > 0 {
			ids[s] = n
			if kind == "" {
				kind, id = s, n
			}
		}
	}
	switch kind {
	case "character", "corporation", "alliance":
		f.ShipTypeID = ids["ship"]
		f.SystemID = ids["system"]
		if f.SystemID == 0 {
			f.RegionID = ids["region"]
		}
		if s := qs.Get("side"); s == "kills" || s == "losses" {
			f.Side = s
		}
	case "ship":
		f.SystemID = ids["system"]
		if f.SystemID == 0 {
			f.RegionID = ids["region"]
		}
	}
	big = qs.Get("big") == "1"
	solo = qs.Get("solo") == "1"
	// A sov fight's kills are few: the chips narrow them, the
	// filters do not.
	if kind == "fight" {
		return kind, id, big, solo, f
	}
	for _, b := range killBands {
		if qs.Get("sec") == b.Value {
			f.Band = b.Value
		}
	}
	if g, err := strconv.ParseInt(qs.Get("class"), 10, 64); err == nil && g > 0 {
		f.GroupID = g
	}
	switch fl := qs.Get("fleet"); fl {
	case "solo":
		solo = true
	case "gang", "fleet", "blob":
		f.Fleet = fl
	}
	switch v := qs.Get("value"); v {
	case "1b":
		big = true
	case "10m", "100m", "10b", "100b":
		f.Value = v
	}
	if d, err := time.Parse("2006-01-02", qs.Get("date")); err == nil && d.Year() >= 2003 {
		f.Date = d.UTC()
	}
	return kind, id, big, solo, f
}

// killFilterQuery is the URL part that carries a scope and its
// filters, without the chips and the page cursor.
func killFilterQuery(kind string, id int64, f killFilter) string {
	u := url.Values{}
	if kind != "" {
		u.Set(kind, strconv.FormatInt(id, 10))
	}
	set := func(k string, n int64) {
		if n > 0 {
			u.Set(k, strconv.FormatInt(n, 10))
		}
	}
	set("ship", f.ShipTypeID)
	set("system", f.SystemID)
	set("region", f.RegionID)
	set("class", f.GroupID)
	if f.Band != "" {
		u.Set("sec", f.Band)
	}
	if f.Fleet != "" {
		u.Set("fleet", f.Fleet)
	}
	if f.Value != "" {
		u.Set("value", f.Value)
	}
	if f.Side != "" {
		u.Set("side", f.Side)
	}
	if !f.Date.IsZero() {
		u.Set("date", f.Date.Format("2006-01-02"))
	}
	return encodeKillQuery(u, kind)
}

// encodeKillQuery writes the scope first, then the rest in key
// order, so a page's links read the same every time.
func encodeKillQuery(u url.Values, kind string) string {
	if kind == "" {
		return u.Encode()
	}
	first := url.Values{kind: u[kind]}
	rest := url.Values{}
	for k, v := range u {
		if k != kind {
			rest[k] = v
		}
	}
	if len(rest) == 0 {
		return first.Encode()
	}
	return first.Encode() + "&" + rest.Encode()
}

// listKillsFiltered reads one filtered page. When the window holds
// fewer matches than a page, edge is the last kill the window read
// (the next page starts behind it); a zero edge means the scope has
// no kills past the window.
func (app *Application) listKillsFiltered(ctx context.Context, kind string, id int64, f killFilter, bt time.Time, bid int64, minValue float64, solo bool) (kills []db.Kill, edge db.KillScanEdgeRecentRow, err error) {
	q := app.queries
	lo, hi := f.attackers()
	minValue = max(minValue, f.minValue())
	switch kind {
	case "system":
		kills, err = q.ListKillsFilteredSystem(ctx, db.ListKillsFilteredSystemParams{ScopeID: id, BeforeTime: bt, BeforeID: bid, ScanCap: killScanCap,
			MinValue: minValue, SoloOnly: solo, MinAttackers: lo, MaxAttackers: hi, SystemID: f.SystemID, RegionID: f.RegionID,
			ShipTypeID: f.ShipTypeID, GroupID: f.GroupID, Band: f.Band, MaxRows: killPageSize})
	case "region":
		kills, err = q.ListKillsFilteredRegion(ctx, db.ListKillsFilteredRegionParams{ScopeID: id, BeforeTime: bt, BeforeID: bid, ScanCap: killScanCap,
			MinValue: minValue, SoloOnly: solo, MinAttackers: lo, MaxAttackers: hi, SystemID: f.SystemID, RegionID: f.RegionID,
			ShipTypeID: f.ShipTypeID, GroupID: f.GroupID, Band: f.Band, MaxRows: killPageSize})
	case "ship":
		kills, err = q.ListKillsFilteredShip(ctx, db.ListKillsFilteredShipParams{ScopeID: id, BeforeTime: bt, BeforeID: bid, ScanCap: killScanCap,
			MinValue: minValue, SoloOnly: solo, MinAttackers: lo, MaxAttackers: hi, SystemID: f.SystemID, RegionID: f.RegionID,
			ShipTypeID: f.ShipTypeID, GroupID: f.GroupID, Band: f.Band, MaxRows: killPageSize})
	case "character", "corporation", "alliance":
		kills, err = q.ListKillsFilteredEntity(ctx, db.ListKillsFilteredEntityParams{Kind: kind, ScopeID: id, BeforeTime: bt, BeforeID: bid, ScanCap: killScanCap,
			MinValue: minValue, SoloOnly: solo, MinAttackers: lo, MaxAttackers: hi, SystemID: f.SystemID, RegionID: f.RegionID,
			ShipTypeID: f.ShipTypeID, GroupID: f.GroupID, Band: f.Band, Side: f.side(), MaxRows: killPageSize})
	default:
		kills, err = q.ListKillsFilteredRecent(ctx, db.ListKillsFilteredRecentParams{BeforeTime: bt, BeforeID: bid, ScanCap: killScanCap,
			MinValue: minValue, SoloOnly: solo, MinAttackers: lo, MaxAttackers: hi, SystemID: f.SystemID, RegionID: f.RegionID,
			ShipTypeID: f.ShipTypeID, GroupID: f.GroupID, Band: f.Band, MaxRows: killPageSize})
	}
	if err != nil || len(kills) >= killPageSize {
		return kills, edge, err
	}
	var e db.KillScanEdgeRecentRow
	var eerr error
	switch kind {
	case "system":
		var r db.KillScanEdgeSystemRow
		r, eerr = q.KillScanEdgeSystem(ctx, db.KillScanEdgeSystemParams{ScopeID: id, BeforeTime: bt, BeforeID: bid, ScanCap: killScanCap})
		e = db.KillScanEdgeRecentRow(r)
	case "region":
		var r db.KillScanEdgeRegionRow
		r, eerr = q.KillScanEdgeRegion(ctx, db.KillScanEdgeRegionParams{ScopeID: id, BeforeTime: bt, BeforeID: bid, ScanCap: killScanCap})
		e = db.KillScanEdgeRecentRow(r)
	case "ship":
		var r db.KillScanEdgeShipRow
		r, eerr = q.KillScanEdgeShip(ctx, db.KillScanEdgeShipParams{ScopeID: id, BeforeTime: bt, BeforeID: bid, ScanCap: killScanCap})
		e = db.KillScanEdgeRecentRow(r)
	case "character", "corporation", "alliance":
		var r db.KillScanEdgeEntityRow
		r, eerr = q.KillScanEdgeEntity(ctx, db.KillScanEdgeEntityParams{Kind: kind, ScopeID: id, BeforeTime: bt, BeforeID: bid, ScanCap: killScanCap})
		e = db.KillScanEdgeRecentRow(r)
	default:
		e, eerr = q.KillScanEdgeRecent(ctx, db.KillScanEdgeRecentParams{BeforeTime: bt, BeforeID: bid, ScanCap: killScanCap})
	}
	if eerr == nil {
		edge = e
	}
	return kills, edge, nil
}

// killPlace is one system's place in the SDE, for the stream's
// filters and the sov fight correlation.
type killPlace struct {
	Region        int64
	Constellation int64
	Security      float64
}

// killSDESnap is an immutable copy of the SDE facts the killboard
// reads in memory: every system's place, every victim type's class,
// and the pickers' choices.
type killSDESnap struct {
	places  map[int64]killPlace
	groups  map[int64]int64 // type id to group id
	regions []killOpt
	classes []killOptGroup
}

// killSDEIndex holds the snapshot, loaded on first use and again
// after killSDETTL, so an SDE import reaches it within hours.
type killSDEIndex struct {
	mu     sync.Mutex
	snap   *killSDESnap
	loaded time.Time
}

const killSDETTL = 6 * time.Hour

// killSDE returns the snapshot, loading it when it is missing or old.
// A failed load keeps the old snapshot; with none it returns an empty
// one, so callers never test for nil.
func (app *Application) killSDE(ctx context.Context) *killSDESnap {
	ix := &app.killSDEIndex
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.snap != nil && time.Since(ix.loaded) < killSDETTL {
		return ix.snap
	}
	snap, err := app.loadKillSDE(ctx)
	if err != nil {
		logging.Warnf("killboard: load SDE index: %v", err)
		if ix.snap == nil {
			return &killSDESnap{places: map[int64]killPlace{}, groups: map[int64]int64{}}
		}
		return ix.snap
	}
	ix.snap, ix.loaded = snap, time.Now()
	return snap
}

func (app *Application) loadKillSDE(ctx context.Context) (*killSDESnap, error) {
	places, err := app.queries.ListKillPlaceIndex(ctx)
	if err != nil {
		return nil, err
	}
	types, err := app.queries.ListKillTypeGroups(ctx)
	if err != nil {
		return nil, err
	}
	groups, err := app.queries.ListKillFilterGroups(ctx)
	if err != nil {
		return nil, err
	}
	regions, err := app.queries.ListKillFilterRegions(ctx)
	if err != nil {
		return nil, err
	}
	s := &killSDESnap{places: make(map[int64]killPlace, len(places)), groups: make(map[int64]int64, len(types))}
	for _, p := range places {
		s.places[p.SystemID] = killPlace{Region: p.RegionID, Constellation: p.ConstellationID, Security: p.Security}
	}
	for _, t := range types {
		s.groups[t.TypeID] = t.GroupID
	}
	for _, r := range regions {
		s.regions = append(s.regions, killOpt{Value: strconv.FormatInt(r.RegionID, 10), Label: r.Name})
	}
	// Ships first, then what else dies on a killmail.
	sets := []struct {
		label string
		cats  []int64
	}{{"Ships", []int64{6}}, {"Structures", []int64{65, 40, 23, 46}}, {"Deployables", []int64{22}}}
	for _, set := range sets {
		g := killOptGroup{Label: set.label}
		for _, row := range groups {
			for _, c := range set.cats {
				if row.CategoryID == c {
					g.Opts = append(g.Opts, killOpt{Value: strconv.FormatInt(row.GroupID, 10), Label: row.Name})
				}
			}
		}
		if len(g.Opts) > 0 {
			s.classes = append(s.classes, g)
		}
	}
	return s, nil
}

// killBand names a kill's security band, as the filtered queries
// do: abyssal and wormhole space by system id, Pochven by region,
// then the system's security.
func killBand(systemID, regionID int64, sec float64) string {
	switch {
	case systemID >= 32000000:
		return "abyssal"
	case systemID >= 31000000:
		return "wh"
	case regionID == 10000070:
		return "pochven"
	case sec >= 0.45:
		return "high"
	case sec > 0:
		return "low"
	}
	return "null"
}

// killStreamFilter is a stream's filter with the SDE snapshot it
// reads, fixed at subscribe time.
type killStreamFilter struct {
	f    killFilter
	kind string
	id   int64
	sde  *killSDESnap
}

// matches applies the filters a stream can test on a kill: the
// value step waits for the price like the Big chip does, and a kills
// or losses side reads the victim (the stream knows the victim and
// the final blow, as its scope test does).
func (s *killStreamFilter) matches(k db.Kill) bool {
	if s == nil {
		return true
	}
	f := s.f
	if f.SystemID > 0 && k.SystemID != f.SystemID {
		return false
	}
	if f.RegionID > 0 && k.RegionID != f.RegionID {
		return false
	}
	if f.ShipTypeID > 0 && k.ShipTypeID != f.ShipTypeID {
		return false
	}
	if f.GroupID > 0 && s.sde.groups[k.ShipTypeID] != f.GroupID {
		return false
	}
	if f.Band != "" && killBand(k.SystemID, k.RegionID, s.sde.places[k.SystemID].Security) != f.Band {
		return false
	}
	if lo, hi := f.attackers(); k.AttackerCount < lo || (hi > 0 && k.AttackerCount > hi) {
		return false
	}
	if f.Side != "" {
		var victim int64
		switch s.kind {
		case "character":
			victim = k.VictimCharacterID
		case "corporation":
			victim = k.VictimCorporationID
		case "alliance":
			victim = k.VictimAllianceID
		}
		if (victim == s.id) != (f.Side == "losses") {
			return false
		}
	}
	return true
}

// killFilterView is the filter form above the list.
type killFilterView struct {
	Hidden     []killOpt // name (Value) and value (Label) pairs the form carries
	Bands      []killOpt
	Classes    []killOptGroup
	Fleets     []killOpt
	Values     []killOpt
	Sides      []killOpt
	Regions    []killOpt
	ShowRegion bool
	ShowSide   bool
	Date       string
	Today      string
	Active     bool
	Open       bool // a picker's filter is on: the disclosure starts open
	Count      int
	ClearHref  string
	// Narrowing pills for the hull and place a scope is narrowed by,
	// each with the link that drops it.
	Pills []killChip
}

// killFilterViewFor builds the form for a page's scope and filters.
func killFilterViewFor(snap *killSDESnap, names *killNames, kind string, id int64, big, solo bool, f killFilter, now time.Time) killFilterView {
	v := killFilterView{Today: now.Format("2006-01-02")}
	if !f.Date.IsZero() {
		v.Date = f.Date.Format("2006-01-02")
	}
	pick := func(opts []killOpt, cur string) []killOpt {
		out := make([]killOpt, len(opts))
		for i, o := range opts {
			o.Selected = o.Value == cur
			out[i] = o
		}
		return out
	}
	v.Bands = pick(killBands, f.Band)
	fleet := f.Fleet
	if solo {
		fleet = "solo"
	}
	v.Fleets = pick(killFleets, fleet)
	value := f.Value
	if big {
		value = "1b"
	}
	for _, s := range killValueSteps {
		v.Values = append(v.Values, killOpt{Value: s.key, Label: s.label, Selected: s.key == value})
	}
	class := strconv.FormatInt(f.GroupID, 10)
	for _, g := range snap.classes {
		v.Classes = append(v.Classes, killOptGroup{Label: g.Label, Opts: pick(g.Opts, class)})
	}
	v.ShowSide = killStatsEntity(kind)
	if v.ShowSide {
		v.Sides = pick(killSides, f.Side)
	}
	// The region picker sets the scope on New Eden and a region
	// page, and narrows a pilot, corporation, alliance or hull.
	region := f.RegionID
	if kind == "region" {
		region = id
	}
	v.ShowRegion = kind != "system" && f.SystemID == 0
	if v.ShowRegion {
		v.Regions = pick(snap.regions, strconv.FormatInt(region, 10))
	}
	// The form carries the scope and the hull and system narrowing;
	// the pickers carry the rest.
	if kind != "" && kind != "region" {
		v.Hidden = append(v.Hidden, killOpt{Value: kind, Label: strconv.FormatInt(id, 10)})
	}
	if f.ShipTypeID > 0 {
		v.Hidden = append(v.Hidden, killOpt{Value: "ship", Label: strconv.FormatInt(f.ShipTypeID, 10)})
	}
	if f.SystemID > 0 {
		v.Hidden = append(v.Hidden, killOpt{Value: "system", Label: strconv.FormatInt(f.SystemID, 10)})
	}
	// Each narrowing the URL holds beyond the scope.
	drop := func(g killFilter) string { return killHref(killFilterQuery(kind, id, g), big, solo) }
	if f.ShipTypeID > 0 {
		g := f
		g.ShipTypeID = 0
		v.Pills = append(v.Pills, killChip{Label: "Ship: " + names.typeName(f.ShipTypeID), Href: drop(g)})
	}
	if f.SystemID > 0 {
		g := f
		g.SystemID = 0
		v.Pills = append(v.Pills, killChip{Label: "System: " + names.system(f.SystemID).Name, Href: drop(g)})
	}
	for _, n := range []bool{f.Band != "", f.GroupID > 0, f.Fleet != "" || solo, f.Value != "" || big, f.Side != "", f.RegionID > 0 || (kind == "region"), !f.Date.IsZero(), f.ShipTypeID > 0, f.SystemID > 0} {
		if n {
			v.Count++
		}
	}
	v.Active = f.filtering() || !f.Date.IsZero() || big || solo
	v.Open = f.filtering() || !f.Date.IsZero() || kind == "region"
	if v.Active {
		// Clearing keeps the scope; a region scope set by the picker
		// is a filter like the others and goes too.
		ck, cid := kind, id
		if kind == "region" {
			ck, cid = "", 0
		}
		v.ClearHref = killHref(killFilterQuery(ck, cid, killFilter{}), false, false)
	}
	return v
}

// killDateCursor is where a date jump starts reading: just past the
// end of the day, EVE time.
func killDateCursor(d time.Time) (time.Time, int64) {
	return d.Add(24 * time.Hour), int64(1 << 62)
}

// killScanNote is the line under a short filtered page.
func killScanNote(matches int) string {
	n := strconv.Itoa(killScanCap)
	if len(n) > 3 {
		n = n[:len(n)-3] + "," + n[len(n)-3:]
	}
	m := "no kills match"
	switch matches {
	case 0:
	case 1:
		m = "1 matches"
	default:
		m = strconv.Itoa(matches) + " match"
	}
	return strings.TrimSpace("These filters checked the " + n + " kills before this point and " + m + ". Older kills checks the next " + n + ".")
}

// killQueryUntidy reports whether a killboard URL holds an empty
// parameter or a picker step that has a chip of its own (the 1B value
// step is Big kills, the solo fleet size is Solo).
func killQueryUntidy(qs url.Values) bool {
	for k, vals := range qs {
		for _, v := range vals {
			if v == "" {
				return true
			}
			if (k == "value" && v == "1b") || (k == "fleet" && v == "solo") {
				return true
			}
		}
	}
	return false
}

// killTidyHref is a killboard URL in its short form, keeping a page
// cursor when there is one.
func killTidyHref(kind string, id int64, big, solo bool, f killFilter, before string) string {
	h := killHref(killFilterQuery(kind, id, f), big, solo)
	if before == "" {
		return h
	}
	sep := "?"
	if strings.Contains(h, "?") {
		sep = "&"
	}
	return h + sep + "before=" + url.QueryEscape(before)
}
