package contracts

// ---------------------------------------------------------------------------
// Public contract search and detail: pages over the stored
// public corpus core sweeps (schema 055) and appraises
// (schema 056). Everything here is a stored read -- no ESI, no
// pricing math at render: the appraisal columns the worker
// stamped are displayed as they stand, coverage and tier mix
// included, because a number without its basis is how price
// tools lie.
//
// The search is anonymous (the corpus is public data; the
// pages ride the anonymous gate like the other public market
// surfaces) and bounded: filters and sorts run in SQL with a
// limit, and each result page reads its contracts' lines in
// one follow-up query.
//
// Courier contents are private by design -- ESI refuses the
// items route for them -- so courier rows and pages show the
// route and the economics only, never an items placeholder.
// ---------------------------------------------------------------------------

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"evesynapse/internal/humanize"
	"evesynapse/internal/logging"
)

// openWindowScope is the enhancement scope this module
// declares for View in game (see module.go).
const openWindowScope = "esi-ui.open_window.v1"

// searchPageSize is the results page: bounded, and the count
// line tells the reader how much more the filters hold.
const searchPageSize = 50

// searchPageCap bounds paging depth: filters narrow the corpus
// far sooner than anyone pages this deep.
const searchPageCap = 40

// searchFilters is the parsed filter form. Numeric bands use
// -1 for off -- except the two timestamps, where the generated
// query takes a plain time: zero means "no lower bound" and
// the far sentinel "no upper bound" (see searchParams).
type searchFilters struct {
	Types       []string
	RegionIDs   []int64
	TitleQ      string
	ItemQ       string
	ItemDir     string // offered | wanted | either
	IssuerQ     string
	ForCorp     int64 // -1 any, 0 pilots, 1 corporations
	PriceMin    float64
	PriceMax    float64
	BuyoutMin   float64
	PlaceQ      string
	PlaceEndQ   string
	RewardMin   float64
	RewardMax   float64
	CollatMax   float64
	VolumeMin   float64
	VolumeMax   float64
	DaysMax     int64
	BPActive    bool
	BPMode      string // any | bpo | bpc
	MEMin       int64
	TEMin       int64
	RunsMin     int64
	IssuedHrs   int64 // -1 off
	ExpiringHrs int64 // -1 off
	DealsOnly   bool
	// Abyssal filters (schema 057): all off unless AbActive.
	AbActive   bool
	AbFamily   string
	AbScoreMin float64 // -1 off, else 0..1
	AbSourceQ  string
	AbEstMin   float64
	AbEstMax   float64
	AbAttrKey  string // attribute id as text, "" off
	AbAttrMin  float64
	Sort       string
	Page       int
}

var searchSorts = []struct{ Key, Label string }{
	{"newest", "Newest first"},
	{"price_asc", "Price, low to high"},
	{"price_desc", "Price, high to low"},
	{"value_desc", "Appraised value, high to low"},
	{"deal_asc", "Best deal first"},
	{"expiry_asc", "Ending soonest"},
	{"reward_desc", "Courier reward, high to low"},
	{"reward_m3_desc", "Courier reward per m3"},
	{"score_desc", "Abyssal score, high to low"},
}

func validSort(key string) bool {
	for _, s := range searchSorts {
		if s.Key == key {
			return true
		}
	}
	return false
}

// parseISKAmount reads a typed ISK figure: plain digits,
// commas, and the k/m/b suffixes traders type ("1.5b",
// "250m"). ok is false for anything else; the filter treats
// unparseable input as unset rather than guessing.
func parseISKAmount(s string) (float64, bool) {
	s = strings.ReplaceAll(strings.TrimSpace(s), ",", "")
	if s == "" {
		return 0, false
	}
	mult := 1.0
	switch last := s[len(s)-1]; {
	case last == 'b' || last == 'B':
		mult, s = 1e9, s[:len(s)-1]
	case last == 'm' || last == 'M':
		mult, s = 1e6, s[:len(s)-1]
	case last == 'k' || last == 'K':
		mult, s = 1e3, s[:len(s)-1]
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 {
		return 0, false
	}
	return v * mult, true
}

func iskBand(q url.Values, key string) float64 {
	if v, ok := parseISKAmount(q.Get(key)); ok {
		return v
	}
	return -1
}

func intOr(q url.Values, key string, off int64) int64 {
	if v, err := strconv.ParseInt(strings.TrimSpace(q.Get(key)), 10, 64); err == nil {
		return v
	}
	return off
}

func parseSearchFilters(q url.Values) searchFilters {
	f := searchFilters{
		ItemDir:  "either",
		ForCorp:  -1,
		PriceMin: -1, PriceMax: -1, BuyoutMin: -1,
		RewardMin: -1, RewardMax: -1, CollatMax: -1,
		VolumeMin: -1, VolumeMax: -1, DaysMax: -1,
		BPMode: "any", MEMin: -1, TEMin: -1, RunsMin: -1,
		IssuedHrs: -1, ExpiringHrs: -1,
		AbScoreMin: -1, AbEstMin: -1, AbEstMax: -1,
		Sort: "newest", Page: 1,
	}
	for _, t := range q["type"] {
		switch t {
		case "item_exchange", "auction", "courier":
			f.Types = append(f.Types, t)
		}
	}
	for _, rs := range q["region"] {
		if id, err := strconv.ParseInt(rs, 10, 64); err == nil && id > 0 {
			f.RegionIDs = append(f.RegionIDs, id)
		}
	}
	f.TitleQ = strings.TrimSpace(q.Get("q_title"))
	f.ItemQ = strings.TrimSpace(q.Get("q_item"))
	switch q.Get("item_dir") {
	case "offered", "wanted":
		f.ItemDir = q.Get("item_dir")
	}
	f.IssuerQ = strings.TrimSpace(q.Get("q_issuer"))
	switch q.Get("for_corp") {
	case "0":
		f.ForCorp = 0
	case "1":
		f.ForCorp = 1
	}
	f.PriceMin = iskBand(q, "price_min")
	f.PriceMax = iskBand(q, "price_max")
	f.BuyoutMin = iskBand(q, "buyout_min")
	f.PlaceQ = strings.TrimSpace(q.Get("place"))
	f.PlaceEndQ = strings.TrimSpace(q.Get("place_end"))
	f.RewardMin = iskBand(q, "reward_min")
	f.RewardMax = iskBand(q, "reward_max")
	f.CollatMax = iskBand(q, "collateral_max")
	if v, ok := parseISKAmount(q.Get("volume_min")); ok {
		f.VolumeMin = v
	}
	if v, ok := parseISKAmount(q.Get("volume_max")); ok {
		f.VolumeMax = v
	}
	f.DaysMax = intOr(q, "days_max", -1)
	switch q.Get("bp") {
	case "bpo", "bpc":
		f.BPMode = q.Get("bp")
	}
	f.MEMin = intOr(q, "me_min", -1)
	f.TEMin = intOr(q, "te_min", -1)
	f.RunsMin = intOr(q, "runs_min", -1)
	f.BPActive = f.BPMode != "any" || f.MEMin >= 0 || f.TEMin >= 0 || f.RunsMin >= 0
	f.IssuedHrs = intOr(q, "issued_hrs", -1)
	f.ExpiringHrs = intOr(q, "expiring_hrs", -1)
	f.DealsOnly = q.Get("deals") == "1"
	// Abyssal filters: family slug, minimum roll score,
	// source module name, estimate band, and one attribute's
	// minimum percentile.
	f.AbFamily = strings.TrimSpace(q.Get("family"))
	if len(f.AbFamily) > 64 {
		f.AbFamily = f.AbFamily[:64]
	}
	if v := intOr(q, "score_min", -1); v >= 0 && v <= 100 {
		f.AbScoreMin = float64(v) / 100
	}
	f.AbSourceQ = strings.TrimSpace(q.Get("source_q"))
	f.AbEstMin = iskBand(q, "est_min")
	f.AbEstMax = iskBand(q, "est_max")
	if v := intOr(q, "attr", -1); v >= 0 {
		f.AbAttrKey = strconv.FormatInt(v, 10)
		if p := intOr(q, "attr_min", -1); p >= 0 && p <= 100 {
			f.AbAttrMin = float64(p) / 100
		}
	}
	f.AbActive = f.AbFamily != "" || f.AbScoreMin >= 0 || f.AbSourceQ != "" ||
		f.AbEstMin >= 0 || f.AbEstMax >= 0 || f.AbAttrKey != ""
	if s := q.Get("sort"); validSort(s) {
		f.Sort = s
	}
	if p, err := strconv.Atoi(q.Get("page")); err == nil && p > 1 {
		f.Page = p
	}
	if f.Page > searchPageCap {
		f.Page = searchPageCap
	}
	return f
}

// searchParams maps the parsed filters onto the sqlc pair
// (search + count share every filter argument).
func (f searchFilters) params(now time.Time) (sp db.SearchPublicContractsParams, cp db.CountPublicContractsSearchParams) {
	issuedAfter := time.Time{}
	if f.IssuedHrs >= 0 {
		issuedAfter = now.Add(-time.Duration(f.IssuedHrs) * time.Hour)
	}
	expiringBefore := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	if f.ExpiringHrs >= 0 {
		expiringBefore = now.Add(time.Duration(f.ExpiringHrs) * time.Hour)
	}
	sp = db.SearchPublicContractsParams{
		Types: f.Types, RegionIds: f.RegionIDs, TitleQ: f.TitleQ,
		ItemQ: f.ItemQ, ItemDir: f.ItemDir, IssuerQ: f.IssuerQ, ForCorp: f.ForCorp,
		PriceMin: f.PriceMin, PriceMax: f.PriceMax, BuyoutMin: f.BuyoutMin,
		PlaceQ: f.PlaceQ, PlaceEndQ: f.PlaceEndQ,
		RewardMin: f.RewardMin, RewardMax: f.RewardMax, CollateralMax: f.CollatMax,
		VolumeMin: f.VolumeMin, VolumeMax: f.VolumeMax, DaysMax: f.DaysMax,
		BpActive: f.BPActive, BpMode: f.BPMode, MeMin: f.MEMin, TeMin: f.TEMin, RunsMin: f.RunsMin,
		IssuedAfter: issuedAfter, ExpiringBefore: expiringBefore, DealsOnly: f.DealsOnly,
		AbyssalActive: f.AbActive, AbFamily: f.AbFamily, AbScoreMin: f.AbScoreMin,
		AbSourceQ: f.AbSourceQ, AbEstMin: f.AbEstMin, AbEstMax: f.AbEstMax,
		AbAttrKey: f.AbAttrKey, AbAttrMin: f.AbAttrMin,
		Sort: f.Sort, MaxRows: searchPageSize, RowOffset: int64((f.Page - 1) * searchPageSize),
	}
	if sp.Types == nil {
		sp.Types = []string{}
	}
	if sp.RegionIds == nil {
		sp.RegionIds = []int64{}
	}
	cp = db.CountPublicContractsSearchParams{
		Types: sp.Types, RegionIds: sp.RegionIds, TitleQ: sp.TitleQ,
		ItemQ: sp.ItemQ, ItemDir: sp.ItemDir, IssuerQ: sp.IssuerQ, ForCorp: sp.ForCorp,
		PriceMin: sp.PriceMin, PriceMax: sp.PriceMax, BuyoutMin: sp.BuyoutMin,
		PlaceQ: sp.PlaceQ, PlaceEndQ: sp.PlaceEndQ,
		RewardMin: sp.RewardMin, RewardMax: sp.RewardMax, CollateralMax: sp.CollateralMax,
		VolumeMin: sp.VolumeMin, VolumeMax: sp.VolumeMax, DaysMax: sp.DaysMax,
		BpActive: sp.BpActive, BpMode: sp.BpMode, MeMin: sp.MeMin, TeMin: sp.TeMin, RunsMin: sp.RunsMin,
		IssuedAfter: sp.IssuedAfter, ExpiringBefore: sp.ExpiringBefore, DealsOnly: sp.DealsOnly,
		AbyssalActive: sp.AbyssalActive, AbFamily: sp.AbFamily, AbScoreMin: sp.AbScoreMin,
		AbSourceQ: sp.AbSourceQ, AbEstMin: sp.AbEstMin, AbEstMax: sp.AbEstMax,
		AbAttrKey: sp.AbAttrKey, AbAttrMin: sp.AbAttrMin,
	}
	return sp, cp
}

// pageURL rebuilds the search URL for another page, filters
// intact.
func (f searchFilters) pageURL(q url.Values, page int) string {
	next := url.Values{}
	for k, vs := range q {
		if k == "page" {
			continue
		}
		for _, v := range vs {
			next.Add(k, v)
		}
	}
	if page > 1 {
		next.Set("page", strconv.Itoa(page))
	}
	if enc := next.Encode(); enc != "" {
		return "/contracts/search/?" + enc
	}
	return "/contracts/search/"
}

// --- display helpers -------------------------------------------------------

// tierLabel words a stored price tier for readers.
func tierLabel(tier string) string {
	switch tier {
	case module.AppraisalTierTypical:
		return "Our Forge typical"
	case module.AppraisalTierEstimate:
		return "Estimated sell"
	case module.AppraisalTierAbyssal:
		return "Our abyssal estimate"
	case module.AppraisalTierFallback:
		return "Fuzzwork aggregate"
	}
	return "Not priced"
}

// tierMixLabel words the stored tier mix stamp ("typical:4,
// fallback:1") for the appraisal summary.
func tierMixLabel(mix string) string {
	if mix == "" {
		return ""
	}
	var parts []string
	for _, seg := range strings.Split(mix, ",") {
		kv := strings.SplitN(seg, ":", 2)
		if len(kv) != 2 {
			continue
		}
		parts = append(parts, kv[1]+" × "+tierLabel(kv[0]))
	}
	return strings.Join(parts, ", ")
}

// countdownText words the time until a timestamp: "3d 2h",
// "5h 12m", "12m", or "expired".
func countdownText(until time.Time, now time.Time) string {
	d := until.Sub(now)
	if d <= 0 {
		return "expired"
	}
	days := int(d / (24 * time.Hour))
	d -= time.Duration(days) * 24 * time.Hour
	hours := int(d / time.Hour)
	d -= time.Duration(hours) * time.Hour
	mins := int(d / time.Minute)
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, mins)
	default:
		return fmt.Sprintf("%dm", mins)
	}
}

// blueprintNote words a line's blueprint facts, "" for a plain
// item line.
func blueprintNote(it db.PubContractItem) string {
	if !it.MaterialEfficiency.Valid {
		return ""
	}
	kind := "BPO"
	if it.IsBlueprintCopy {
		kind = "BPC"
	}
	note := fmt.Sprintf("%s · ME %d · TE %d", kind, it.MaterialEfficiency.Int64, it.TimeEfficiency.Int64)
	if it.Runs.Valid && it.Runs.Int64 > 0 {
		note += fmt.Sprintf(" · %d runs", it.Runs.Int64)
	}
	return note
}

// --- search page -----------------------------------------------------------

// searchLine is one lead line on a result row.
type searchLine struct {
	Name string
	Qty  string
	Note string // blueprint facts, when any
}

// searchRow is one result, display-ready.
type searchRow struct {
	ContractID int64
	TypeLabel  string
	Title      string
	Courier    bool
	Offered    []searchLine
	Wanted     []searchLine
	MoreOffer  int
	MoreWant   int
	// Price block: exchanges show the ask, auctions the
	// standing bid (or starting price), couriers the reward.
	PriceLabel string
	Price      string
	PriceNote  string
	Value      string // appraised offered value, "" until appraised
	ValueNote  string // coverage / deal note
	IsDeal     bool
	Location   module.PlaceRef
	Dropoff    module.PlaceRef
	HasRoute   bool
	Abyssal    *rowAbyssal
	Reward     string
	Collateral string
	Volume     string
	IssuerName string
	IssuerID   int64
	IssuerCorp bool
	RegionName string
	ExpiresIn  string
	IssuedAgo  string
}

type regionOption struct {
	ID       int64
	Name     string
	Selected bool
}

type familyOption struct {
	Slug     string
	Label    string
	Selected bool
}

type attrOption struct {
	Key      string // attribute id as text
	Name     string
	Selected bool
}

// rowAbyssalBar is one primary roll's percentile bar on a
// search row.
type rowAbyssalBar struct {
	Name string
	Pct  int
}

// rowAbyssal is a result row's mutated-line summary: where
// the module came from, how its main rolls landed, and the
// stored estimate with its spread and basis -- never a bare
// point.
type rowAbyssal struct {
	SourceName   string
	SourceTypeID int64
	MutatorName  string
	ScoreText    string
	Bars         []rowAbyssalBar
	Estimate     string
	SpreadNote   string // "likely L to H · from N listings of this module"
	CapNote      string // "Held back by: CPU fitting cost"
}

type searchView struct {
	Filters   searchFilters
	Raw       url.Values
	Echo      map[string]string // first raw value per form field
	TypeSet   map[string]bool   // checked contract types
	Regions   []regionOption
	Families  []familyOption
	AttrOpts  []attrOption
	Sorts     []struct{ Key, Label string }
	Rows      []searchRow
	Total     int64
	TotalText string
	Page      int
	Pages     int
	PrevURL   string
	NextURL   string
	HasRows   bool
}

func (app *mod) handleContractSearch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := &PageData{ShellData: app.deps.PublicShell(ctx)}
	data.ContractSearch = app.buildSearchView(ctx, r.URL.Query())
	app.deps.Render(ctx, w, http.StatusOK, "contract_search.html", data)
}

func (app *mod) buildSearchView(ctx context.Context, q url.Values) *searchView {
	f := parseSearchFilters(q)
	now := time.Now().UTC()
	view := &searchView{Filters: f, Raw: q, Sorts: searchSorts, Page: f.Page,
		Echo: map[string]string{}, TypeSet: map[string]bool{}}
	for k, vs := range q {
		if len(vs) > 0 {
			view.Echo[k] = vs[0]
		}
	}
	for _, t := range f.Types {
		view.TypeSet[t] = true
	}

	regionRows, err := app.queries.ListPublicContractRegions(ctx)
	if err != nil {
		logging.Errorf("contract search: regions: %v", err)
	}
	selected := map[int64]bool{}
	for _, id := range f.RegionIDs {
		selected[id] = true
	}
	regionNames := map[int64]string{}
	for _, rr := range regionRows {
		name := rr.RegionName
		if name == "" {
			name = fmt.Sprintf("Region #%d", rr.RegionID)
		}
		regionNames[rr.RegionID] = name
		view.Regions = append(view.Regions, regionOption{ID: rr.RegionID, Name: name, Selected: selected[rr.RegionID]})
	}

	// Abyssal filter options: the families the corpus holds,
	// and the attributes any family scores.
	if fams, err := app.queries.ListAbyssalFamilies(ctx); err == nil {
		for _, fam := range fams {
			view.Families = append(view.Families, familyOption{
				Slug: fam, Label: abyssalFamilyLabel(fam), Selected: fam == f.AbFamily,
			})
		}
	}
	if attrIDs, err := app.queries.ListAbyssalWeightAttributeIDs(ctx); err == nil && len(attrIDs) > 0 {
		names := map[int64]string{}
		if rows, err := app.queries.ListSDEAttributeTypesByIDs(ctx, attrIDs); err == nil {
			for _, row := range rows {
				names[row.AttributeID] = row.Name
			}
		}
		for _, id := range attrIDs {
			name := names[id]
			if name == "" {
				name = fmt.Sprintf("Attribute #%d", id)
			}
			key := strconv.FormatInt(id, 10)
			view.AttrOpts = append(view.AttrOpts, attrOption{Key: key, Name: name, Selected: key == f.AbAttrKey})
		}
	}

	sp, cp := f.params(now)
	total, err := app.queries.CountPublicContractsSearch(ctx, cp)
	if err != nil {
		logging.Errorf("contract search: count: %v", err)
		return view
	}
	view.Total = total
	view.TotalText = esi.FormatInt(total)
	view.Pages = int((total + searchPageSize - 1) / searchPageSize)
	if view.Pages < 1 {
		view.Pages = 1
	}
	if f.Page > 1 {
		view.PrevURL = f.pageURL(q, f.Page-1)
	}
	if f.Page < view.Pages && f.Page < searchPageCap {
		view.NextURL = f.pageURL(q, f.Page+1)
	}

	rows, err := app.queries.SearchPublicContracts(ctx, sp)
	if err != nil {
		logging.Errorf("contract search: rows: %v", err)
		return view
	}
	if len(rows) == 0 {
		return view
	}
	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ContractID)
	}
	items, err := app.queries.ListPublicContractItemsForContracts(ctx, ids)
	if err != nil {
		logging.Errorf("contract search: items: %v", err)
		items = nil
	}
	byContract := map[int64][]db.PubContractItem{}
	typeIDs := map[int64]bool{}
	for _, it := range items {
		byContract[it.ContractID] = append(byContract[it.ContractID], it)
		typeIDs[it.TypeID] = true
	}
	names := map[int64]string{}
	for id := range typeIDs {
		names[id] = app.deps.TypeNameOrID(ctx, id)
	}
	abData := app.pageAbyssalData(ctx, items)
	memo := map[int64]module.PlaceRef{}
	for _, row := range rows {
		view.Rows = append(view.Rows, app.searchRow(ctx, memo, row, byContract[row.ContractID], names, regionNames, abData, now))
	}
	view.HasRows = len(view.Rows) > 0
	return view
}

func (app *mod) searchRow(ctx context.Context, memo map[int64]module.PlaceRef, row db.SearchPublicContractsRow, items []db.PubContractItem, names map[int64]string, regionNames map[int64]string, abData *pageAbyssalData, now time.Time) searchRow {
	out := searchRow{
		ContractID: row.ContractID,
		Title:      row.Title,
		Location:   app.deps.LinkPlaceMemo(ctx, memo, row.StartLocationID, app.deps.EconLocationTitle(ctx, row.StartLocationID)),
		ExpiresIn:  countdownText(row.DateExpired, now),
		IssuedAgo:  app.deps.AgeText(row.DateIssued),
	}
	switch row.Type {
	case "auction":
		out.TypeLabel = "Auction"
	case "courier":
		out.TypeLabel = "Courier"
		out.Courier = true
	default:
		out.TypeLabel = "Item exchange"
	}
	if name, ok := regionNames[row.RegionID]; ok {
		out.RegionName = name
	} else {
		out.RegionName = fmt.Sprintf("Region #%d", row.RegionID)
	}
	// Issuer: a for-corporation contract is issued by the
	// corporation; otherwise by the character.
	if row.ForCorporation {
		if name := app.deps.CorpDisplayName(ctx, row.IssuerCorporationID); name != "" {
			out.IssuerName, out.IssuerID, out.IssuerCorp = name, row.IssuerCorporationID, true
		}
	}
	if out.IssuerName == "" {
		if name := app.deps.DisplayCharacter(ctx, row.IssuerID); name != "" {
			out.IssuerName, out.IssuerID = name, row.IssuerID
		} else {
			out.IssuerName, out.IssuerID = fmt.Sprintf("Character #%d", row.IssuerID), row.IssuerID
		}
	}

	offered, wanted := 0, 0
	for _, it := range items {
		line := searchLine{Name: names[it.TypeID], Qty: esi.FormatInt(it.Quantity), Note: blueprintNote(it)}
		if it.IsIncluded {
			offered++
			if len(out.Offered) < 3 {
				out.Offered = append(out.Offered, line)
			}
		} else {
			wanted++
			if len(out.Wanted) < 3 {
				out.Wanted = append(out.Wanted, line)
			}
		}
	}
	out.MoreOffer = offered - len(out.Offered)
	out.MoreWant = wanted - len(out.Wanted)

	// The row's mutated-line summary: the first offered
	// singleton the corpus knows, if any.
	if abData != nil {
		for _, it := range items {
			if !it.IsIncluded || !it.ItemID.Valid {
				continue
			}
			if ab, ok := abData.items[it.ItemID.Int64]; ok && ab.Family != "" {
				out.Abyssal = app.rowAbyssalFor(ctx, ab, abData)
				break
			}
		}
	}

	switch row.Type {
	case "courier":
		out.PriceLabel = "Reward"
		out.Price = esi.FormatISK(row.Reward)
		out.Reward = esi.FormatISK(row.Reward)
		out.Collateral = esi.FormatISK(row.Collateral)
		out.Volume = fmt.Sprintf("%s m3", esi.FormatInt(int64(row.Volume)))
		out.Dropoff = app.deps.LinkPlaceMemo(ctx, memo, row.EndLocationID, app.deps.EconLocationTitle(ctx, row.EndLocationID))
		out.HasRoute = true
	case "auction":
		out.PriceLabel = "Current bid"
		if row.BidCount > 0 {
			out.Price = esi.FormatISK(row.TopBid)
		} else {
			out.Price = esi.FormatISK(row.Price)
			out.PriceNote = "starting price"
		}
		notes := []string{}
		if row.BidCount > 0 {
			notes = append(notes, humanize.Plural(int(row.BidCount), "bid"))
		}
		if row.Buyout > 0 {
			notes = append(notes, "buyout "+esi.FormatISK(row.Buyout))
		}
		out.PriceNote = strings.Join(notes, " · ")
	default:
		out.PriceLabel = "Price"
		out.Price = esi.FormatISK(row.Price)
	}

	if row.PricedAt.Valid && row.TotalLines > 0 {
		out.Value = esi.FormatISK(row.OfferedValue)
		switch {
		case row.PricedLines < row.TotalLines:
			out.ValueNote = fmt.Sprintf("priced %d of %d lines", row.PricedLines, row.TotalLines)
		case row.DealRatio.Valid:
			pct := int(row.DealRatio.Float64*100 + 0.5)
			out.ValueNote = fmt.Sprintf("asking %d%% of value", pct)
			out.IsDeal = row.DealRatio.Float64 <= 0.85
		}
	}
	return out
}

// --- abyssal display -------------------------------------------------------

// pageAbyssalData is the search page's stored abyssal facts:
// the page's mutated items, their families' weights (for the
// primary bars), and attribute names.
type pageAbyssalData struct {
	items     map[int64]db.AbyssalItem
	weights   map[string][]db.AbyssalFamilyWeight
	attrNames map[int64]string
}

func (app *mod) pageAbyssalData(ctx context.Context, items []db.PubContractItem) *pageAbyssalData {
	var ids []int64
	for _, it := range items {
		if it.ItemID.Valid {
			ids = append(ids, it.ItemID.Int64)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := app.queries.ListAbyssalItemsByIDs(ctx, ids)
	if err != nil || len(rows) == 0 {
		return nil
	}
	data := &pageAbyssalData{
		items:     map[int64]db.AbyssalItem{},
		weights:   map[string][]db.AbyssalFamilyWeight{},
		attrNames: map[int64]string{},
	}
	families := map[string]bool{}
	for _, row := range rows {
		data.items[row.ItemID] = row
		if row.Family != "" {
			families[row.Family] = true
		}
	}
	if len(families) > 0 {
		fams := make([]string, 0, len(families))
		for fam := range families {
			fams = append(fams, fam)
		}
		if wrows, err := app.queries.ListAbyssalFamilyWeightsByFamilies(ctx, fams); err == nil {
			attrIDs := map[int64]bool{}
			for _, w := range wrows {
				data.weights[w.Family] = append(data.weights[w.Family], w)
				attrIDs[w.AttributeID] = true
			}
			for _, row := range rows {
				if row.CapAttributeID > 0 {
					attrIDs[row.CapAttributeID] = true
				}
			}
			ids := make([]int64, 0, len(attrIDs))
			for id := range attrIDs {
				ids = append(ids, id)
			}
			if arows, err := app.queries.ListSDEAttributeTypesByIDs(ctx, ids); err == nil {
				for _, r := range arows {
					data.attrNames[r.AttributeID] = r.Name
				}
			}
		}
	}
	return data
}

// rowAbyssalFor words one stored abyssal row for a search
// result: provenance, score, the primary bars, the estimate
// with its basis, and the cap when one binds.
func (app *mod) rowAbyssalFor(ctx context.Context, ab db.AbyssalItem, data *pageAbyssalData) *rowAbyssal {
	out := &rowAbyssal{
		SourceName:   app.deps.TypeNameOrID(ctx, ab.SourceTypeID),
		SourceTypeID: ab.SourceTypeID,
		MutatorName:  app.deps.TypeNameOrID(ctx, ab.MutatorTypeID),
	}
	if ab.Score.Valid {
		out.ScoreText = fmt.Sprintf("Roll score %d / 100", int(ab.Score.Float64*100+0.5))
	}
	// Primary bars: the family's heaviest non-penalty rolls
	// this item has percentiles for, best three.
	pcts := parseAbyssalPercentiles(ab.Percentiles)
	weights := append([]db.AbyssalFamilyWeight(nil), data.weights[ab.Family]...)
	sort.Slice(weights, func(i, j int) bool {
		if weights[i].Weight != weights[j].Weight {
			return weights[i].Weight > weights[j].Weight
		}
		return weights[i].AttributeID < weights[j].AttributeID
	})
	for _, w := range weights {
		if w.PenaltyOnly || len(out.Bars) >= 3 {
			continue
		}
		p, ok := pcts[w.AttributeID]
		if !ok {
			continue
		}
		name := data.attrNames[w.AttributeID]
		if name == "" {
			name = fmt.Sprintf("Attribute #%d", w.AttributeID)
		}
		out.Bars = append(out.Bars, rowAbyssalBar{Name: name, Pct: int(p*100 + 0.5)})
	}
	if ab.EstValue.Valid {
		out.Estimate = esi.FormatISK(ab.EstValue.Float64)
		note := abyssalBasisShort(ab)
		if ab.EstLow.Valid && ab.EstHigh.Valid {
			note = fmt.Sprintf("likely %s to %s · %s",
				esi.FormatISK(ab.EstLow.Float64), esi.FormatISK(ab.EstHigh.Float64), note)
		}
		out.SpreadNote = note
	}
	if ab.CapAttributeID > 0 {
		out.CapNote = "Held back by: " + capAttrDisplayName(ab.CapAttributeID, data.attrNames[ab.CapAttributeID])
	}
	return out
}

// parseAbyssalPercentiles reads the stored percentile map
// (attribute id as text -> 0..1).
func parseAbyssalPercentiles(raw []byte) map[int64]float64 {
	out := map[int64]float64{}
	var m map[string]float64
	if err := json.Unmarshal(raw, &m); err != nil {
		return out
	}
	for k, v := range m {
		if id, err := strconv.ParseInt(k, 10, 64); err == nil {
			out[id] = v
		}
	}
	return out
}

// abyssalFamilyLabel prettifies a family slug for the filter
// list ("warp_scrambler" -> "Warp Scrambler").
func abyssalFamilyLabel(slug string) string {
	parts := strings.Split(slug, "_")
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, " ")
}

// abyssalBasisShort is the estimate's basis in a breath: what
// the number rests on, never just the number.
func abyssalBasisShort(ab db.AbyssalItem) string {
	switch ab.EstBasis {
	case "corpus":
		if ab.EstScope == "module" {
			return fmt.Sprintf("from %d listings of this module", ab.EstComps)
		}
		return fmt.Sprintf("from %d listings of similar modules", ab.EstComps)
	case "curve":
		return "score curve (few listings)"
	case "prior":
		return "base module price and rolls only"
	}
	return ""
}

// capAttrDisplayName: fitting costs get their plain-words
// names; everything else keeps the SDE's.
func capAttrDisplayName(attrID int64, sdeName string) string {
	switch attrID {
	case 50:
		return "CPU fitting cost"
	case 30:
		return "Powergrid fitting cost"
	}
	if sdeName != "" {
		return sdeName
	}
	return fmt.Sprintf("Attribute #%d", attrID)
}

// --- detail page -----------------------------------------------------------

type detailLine struct {
	Name      string
	TypeID    int64
	Qty       string
	Note      string // blueprint facts
	UnitPrice string
	LineValue string
	TierLabel string
	TierNote  string // provenance as-of, fallback only spoken aloud
	Abyssal   *abyssalPanel
}

type abyssalRoll struct {
	Name    string
	Value   string
	Base    string
	Delta   string
	Good    bool
	HasBase bool
	Pct     int // percentile in the mutaplasmid's band, 0..100
	HasPct  bool
}

type abyssalDerivedRow struct {
	Label string
	Value string
	Pct   int
}

type abyssalPanel struct {
	SourceName    string
	SourceTypeID  int64
	MutatorName   string
	MutatorTypeID int64
	Rolls         []abyssalRoll
	Derived       []abyssalDerivedRow
	HasScore      bool
	ScoreText     string // "87 / 100"
	HasEstimate   bool
	Estimate      string
	EstLow        string
	EstHigh       string
	BasisLabel    string // what the estimate rests on
	ScoredNote    string // "Scored 3h ago"
	CapNote       string // the capping attribute, in plain words
	Recreation    string // rolling another this good, priced
	NoEstimate    string // why there is no figure, when there is none
	ContextPrice  string // the source module's own price, context only
	ContextTier   string
}

type bidRow struct {
	Amount string
	When   string
}

type viewInGameState struct {
	Visible bool // signed in
	Enabled bool // some linked character holds the scope
	Note    string
	Relink  string // sign-in-again URL when not enabled
}

type detailView struct {
	Found      bool
	ContractID int64
	TypeLabel  string
	Title      string
	StateNote  string // items private / still gathering
	Offered    []detailLine
	Wanted     []detailLine
	HasWanted  bool

	OfferedValue  string
	WantValue     string
	CoverageNote  string
	TierMix       string
	AppraisedNote string

	PriceLabel string
	Price      string
	Buyout     string
	DealNote   string
	IsDeal     bool

	TopBid   string
	BidCount int64
	Bids     []bidRow

	Reward     string
	Collateral string
	Volume     string
	Days       int64

	IssuerName string
	IssuerID   int64
	IssuerCorp bool
	CorpName   string
	CorpID     int64
	HasCorp    bool

	Location     module.PlaceRef
	Dropoff      module.PlaceRef
	HasRoute     bool
	RegionName   string
	IssuedNote   string
	ExpiresNote  string
	LastSeenNote string

	ViewInGame viewInGameState
}

func (app *mod) handleContractDetail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := strconv.ParseInt(chi.URLParam(r, "contractID"), 10, 64)
	data := &PageData{ShellData: app.deps.PublicShell(ctx)}
	if err != nil || id <= 0 {
		data.ContractDetail = &detailView{}
		app.deps.Render(ctx, w, http.StatusNotFound, "contract_detail.html", data)
		return
	}
	view, ok := app.buildDetailView(ctx, id)
	if !ok {
		data.ContractDetail = &detailView{}
		app.deps.Render(ctx, w, http.StatusNotFound, "contract_detail.html", data)
		return
	}
	data.ContractDetail = view
	app.deps.Render(ctx, w, http.StatusOK, "contract_detail.html", data)
}

func (app *mod) buildDetailView(ctx context.Context, contractID int64) (*detailView, bool) {
	c, err := app.queries.GetPublicContract(ctx, contractID)
	if err != nil {
		return nil, false
	}
	now := time.Now().UTC()
	v := &detailView{Found: true, ContractID: contractID, Title: c.Title, Days: c.DaysToComplete}
	switch c.Type {
	case "auction":
		v.TypeLabel = "Auction"
	case "courier":
		v.TypeLabel = "Courier"
	default:
		v.TypeLabel = "Item exchange"
	}
	if reg, err := app.queries.GetSDERegion(ctx, c.RegionID); err == nil && reg.Name != "" {
		v.RegionName = reg.Name
	} else {
		v.RegionName = fmt.Sprintf("Region #%d", c.RegionID)
	}
	memo := map[int64]module.PlaceRef{}
	v.Location = app.deps.LinkPlaceMemo(ctx, memo, c.StartLocationID, app.deps.EconLocationTitle(ctx, c.StartLocationID))
	if c.Type == "courier" {
		v.Dropoff = app.deps.LinkPlaceMemo(ctx, memo, c.EndLocationID, app.deps.EconLocationTitle(ctx, c.EndLocationID))
		v.HasRoute = true
		v.Reward = esi.FormatISK(c.Reward)
		v.Collateral = esi.FormatISK(c.Collateral)
		v.Volume = fmt.Sprintf("%s m3", esi.FormatInt(int64(c.Volume)))
	}
	v.IssuedNote = c.DateIssued.Format("Jan 2, 2006, 3:04 PM") + " (" + app.deps.AgeText(c.DateIssued) + ")"
	v.ExpiresNote = c.DateExpired.Format("Jan 2, 2006, 3:04 PM") + " (" + countdownText(c.DateExpired, now) + " left)"
	v.LastSeenNote = "Last seen by our sweep " + app.deps.AgeText(c.LastSeen)

	// Issuer block: the issuing character, and the
	// corporation when the contract is for one.
	if name := app.deps.DisplayCharacter(ctx, c.IssuerID); name != "" {
		v.IssuerName = name
	} else {
		v.IssuerName = fmt.Sprintf("Character #%d", c.IssuerID)
	}
	v.IssuerID = c.IssuerID
	if c.ForCorporation || c.IssuerCorporationID > 0 {
		v.HasCorp = true
		v.CorpID = c.IssuerCorporationID
		v.IssuerCorp = c.ForCorporation
		if name := app.deps.CorpDisplayName(ctx, c.IssuerCorporationID); name != "" {
			v.CorpName = name
		} else {
			v.CorpName = fmt.Sprintf("Corporation #%d", c.IssuerCorporationID)
		}
	}

	switch c.Type {
	case "auction":
		v.PriceLabel = "Starting price"
		v.Price = esi.FormatISK(c.Price)
		if c.Buyout > 0 {
			v.Buyout = esi.FormatISK(c.Buyout)
		}
	case "courier":
		v.PriceLabel = "Reward"
		v.Price = esi.FormatISK(c.Reward)
	default:
		v.PriceLabel = "Price"
		v.Price = esi.FormatISK(c.Price)
	}

	// Lines and appraisal.
	switch c.ItemsState {
	case "private":
		v.StateNote = "Courier cargo is private — the game does not publish what a courier contract carries, so there is no contents list to show."
	case "pending":
		v.StateNote = "Our sweep has the contract but has not gathered its contents yet."
	case "gone":
		v.StateNote = "The contents list is no longer available."
	}
	items, err := app.queries.ListPublicContractItems(ctx, contractID)
	if err != nil {
		logging.Errorf("contract detail: items %d: %v", contractID, err)
		items = nil
	}
	// Abyssal membership for the singleton lines, in one read.
	var abyssal map[int64]db.AbyssalItem
	{
		var itemIDs []int64
		for _, it := range items {
			if it.ItemID.Valid {
				itemIDs = append(itemIDs, it.ItemID.Int64)
			}
		}
		if len(itemIDs) > 0 {
			rows, err := app.queries.ListAbyssalItemsByIDs(ctx, itemIDs)
			if err != nil {
				logging.Errorf("contract detail: abyssal %d: %v", contractID, err)
			}
			abyssal = map[int64]db.AbyssalItem{}
			for _, row := range rows {
				abyssal[row.ItemID] = row
			}
		}
	}
	pricedWanted := 0
	for _, it := range items {
		line := app.detailLine(ctx, it, abyssal)
		if it.IsIncluded {
			v.Offered = append(v.Offered, line)
		} else {
			v.Wanted = append(v.Wanted, line)
			if it.PriceTier != "" && it.PriceTier != module.AppraisalTierUnpriced {
				pricedWanted++
			}
		}
	}
	v.HasWanted = len(v.Wanted) > 0
	if c.PricedAt.Valid {
		v.AppraisedNote = "Appraised " + c.PricedAt.Time.Format("Jan 2, 3:04 PM")
		v.TierMix = tierMixLabel(c.TierMix)
		if c.TotalLines > 0 {
			v.OfferedValue = esi.FormatISK(c.OfferedValue)
		}
		if c.WantValue > 0 {
			v.WantValue = esi.FormatISK(c.WantValue)
		}
		var cov []string
		if c.TotalLines > 0 {
			cov = append(cov, fmt.Sprintf("offered side priced %d of %d lines", c.PricedLines, c.TotalLines))
		}
		if len(v.Wanted) > 0 {
			cov = append(cov, fmt.Sprintf("wanted side priced %d of %d lines", pricedWanted, len(v.Wanted)))
		}
		v.CoverageNote = strings.Join(cov, " · ")
		if c.DealRatio.Valid {
			pct := int(c.DealRatio.Float64*100 + 0.5)
			v.DealNote = fmt.Sprintf("Asking %d%% of our appraised value", pct)
			v.IsDeal = c.DealRatio.Float64 <= 0.85
		}
	}

	// Auction bids.
	if c.Type == "auction" {
		bids, err := app.queries.ListPublicContractBids(ctx, contractID)
		if err != nil {
			logging.Errorf("contract detail: bids %d: %v", contractID, err)
		}
		sort.Slice(bids, func(i, j int) bool {
			if bids[i].Amount != bids[j].Amount {
				return bids[i].Amount > bids[j].Amount
			}
			return bids[i].DateBid.After(bids[j].DateBid)
		})
		v.BidCount = int64(len(bids))
		for _, b := range bids {
			v.Bids = append(v.Bids, bidRow{
				Amount: esi.FormatISK(b.Amount),
				When:   b.DateBid.Format("Jan 2, 3:04 PM") + " (" + app.deps.AgeText(b.DateBid) + ")",
			})
		}
		if len(bids) > 0 {
			v.TopBid = esi.FormatISK(bids[0].Amount)
		}
	}

	v.ViewInGame = app.viewInGameState(ctx)
	return v, true
}

func (app *mod) detailLine(ctx context.Context, it db.PubContractItem, abyssal map[int64]db.AbyssalItem) detailLine {
	line := detailLine{
		Name:   app.deps.TypeNameOrID(ctx, it.TypeID),
		TypeID: it.TypeID,
		Qty:    esi.FormatInt(it.Quantity),
		Note:   blueprintNote(it),
	}
	if it.PriceTier != "" {
		line.TierLabel = tierLabel(it.PriceTier)
		if it.UnitPrice.Valid {
			line.UnitPrice = esi.FormatISK(it.UnitPrice.Float64)
		}
		if it.LineValue.Valid {
			line.LineValue = esi.FormatISK(it.LineValue.Float64)
		}
		if it.PriceAsof.Valid && !it.PriceAsof.Time.IsZero() {
			line.TierNote = "as of " + app.deps.AgeText(it.PriceAsof.Time)
		}
	}
	if it.ItemID.Valid {
		if ab, ok := abyssal[it.ItemID.Int64]; ok {
			line.Abyssal = app.abyssalPanel(ctx, ab)
		}
	}
	return line
}

// abyssalPanel builds a mutated item's display from its
// stored row: every rolled attribute with its value, the
// source module's base value, which way the roll went, and
// where the roll sits in the band its mutaplasmid could
// produce -- then the stored score and estimate with their
// spread, basis and cap, all computed by core's scoring pass
// and read here as they stand. The source module's own price
// rides along as context only.
func (app *mod) abyssalPanel(ctx context.Context, ab db.AbyssalItem) *abyssalPanel {
	panel := &abyssalPanel{
		SourceName:    app.deps.TypeNameOrID(ctx, ab.SourceTypeID),
		SourceTypeID:  ab.SourceTypeID,
		MutatorName:   app.deps.TypeNameOrID(ctx, ab.MutatorTypeID),
		MutatorTypeID: ab.MutatorTypeID,
	}
	var rolls []struct {
		AttributeID int64   `json:"attribute_id"`
		Value       float64 `json:"value"`
	}
	if err := json.Unmarshal(ab.Attributes, &rolls); err != nil {
		logging.Errorf("contract detail: abyssal attributes %d: %v", ab.ItemID, err)
		return panel
	}
	attrIDs := make([]int64, 0, len(rolls)+1)
	for _, rl := range rolls {
		attrIDs = append(attrIDs, rl.AttributeID)
	}
	if ab.CapAttributeID > 0 {
		attrIDs = append(attrIDs, ab.CapAttributeID)
	}
	names := map[int64]db.SdeAttributeType{}
	if rows, err := app.queries.ListSDEAttributeTypesByIDs(ctx, attrIDs); err == nil {
		for _, row := range rows {
			names[row.AttributeID] = row
		}
	}
	base := map[int64]float64{}
	if rows, err := app.queries.ListSDETypeAttributes(ctx, ab.SourceTypeID); err == nil {
		for _, row := range rows {
			base[row.AttributeID] = row.Value
		}
	}
	pcts := parseAbyssalPercentiles(ab.Percentiles)
	for _, rl := range rolls {
		row := abyssalRoll{Value: formatAttrValue(rl.Value)}
		if at, ok := names[rl.AttributeID]; ok && at.Name != "" {
			row.Name = at.Name
		} else {
			row.Name = fmt.Sprintf("Attribute #%d", rl.AttributeID)
		}
		if p, ok := pcts[rl.AttributeID]; ok {
			row.Pct = int(p*100 + 0.5)
			row.HasPct = true
		}
		if bv, ok := base[rl.AttributeID]; ok {
			row.HasBase = true
			row.Base = formatAttrValue(bv)
			if bv != 0 {
				pct := (rl.Value - bv) / absf(bv) * 100
				row.Delta = fmt.Sprintf("%+.1f%%", pct)
				good := pct > 0
				if at, ok := names[rl.AttributeID]; ok && at.HighIsGood == 0 {
					good = pct < 0
				}
				row.Good = good
			}
		}
		panel.Rolls = append(panel.Rolls, row)
	}
	var derived []struct {
		Label string  `json:"label"`
		Value float64 `json:"value"`
		Pct   float64 `json:"pct"`
	}
	if err := json.Unmarshal(ab.DerivedStats, &derived); err == nil {
		for _, d := range derived {
			panel.Derived = append(panel.Derived, abyssalDerivedRow{
				Label: d.Label, Value: formatAttrValue(d.Value), Pct: int(d.Pct*100 + 0.5),
			})
		}
	}
	if ab.Score.Valid {
		panel.HasScore = true
		panel.ScoreText = fmt.Sprintf("%d / 100", int(ab.Score.Float64*100+0.5))
	}
	if ab.ScoredAt.Valid {
		panel.ScoredNote = "Scored " + app.deps.AgeText(ab.ScoredAt.Time)
	}
	if ab.EstValue.Valid {
		panel.HasEstimate = true
		panel.Estimate = esi.FormatISK(ab.EstValue.Float64)
		if ab.EstLow.Valid {
			panel.EstLow = esi.FormatISK(ab.EstLow.Float64)
		}
		if ab.EstHigh.Valid {
			panel.EstHigh = esi.FormatISK(ab.EstHigh.Float64)
		}
		switch ab.EstBasis {
		case "corpus":
			if ab.EstScope == "module" {
				panel.BasisLabel = fmt.Sprintf("from %d current and recent listings of this module", ab.EstComps)
			} else {
				panel.BasisLabel = fmt.Sprintf("from %d current and recent listings of similar modules", ab.EstComps)
			}
		case "curve":
			panel.BasisLabel = "score curve (few listings)"
		case "prior":
			panel.BasisLabel = "the base module's price and the rolls alone"
		}
	} else if ab.Score.Valid {
		panel.NoEstimate = "No estimate: the base module has no price of its own and too few listings of its kind exist to price the rolls against, so this item shows its rolls and score without an ISK figure rather than a guess."
	} else if len(pcts) > 0 {
		panel.NoEstimate = "Not scored: this family has no scoring weights yet, so the item shows its rolls and where each one sits in its range, with no score and no ISK figure."
	}
	if ab.CapAttributeID > 0 {
		name := ""
		if at, ok := names[ab.CapAttributeID]; ok {
			name = at.Name
		}
		name = capAttrDisplayName(ab.CapAttributeID, name)
		if ab.CapKind == "fitting" {
			panel.CapNote = fmt.Sprintf("Held back by: %s. A fitting cost that far above the base module's makes the module hard to fit at all, so the score and the estimate are capped no matter how well the rest rolled.", name)
		} else {
			panel.CapNote = fmt.Sprintf("Held back by: %s. That roll is one this module is bought for, and it landed near the bottom of its range, so the score and the estimate are capped no matter how well the rest rolled.", name)
		}
	}
	if ab.RecreationCost.Valid && ab.RecreationAttempts.Valid {
		panel.Recreation = fmt.Sprintf("Rolling another this good takes about %s attempts on average -- around %s ISK at current base module and mutaplasmid prices.",
			formatAttempts(ab.RecreationAttempts.Float64), esi.FormatISK(ab.RecreationCost.Float64))
	}
	// Source context: what the unmutated module prices at
	// through the same house order. Display only.
	if res, err := app.deps.AppraiseLines(ctx, []module.AppraisalLine{
		{TypeID: ab.SourceTypeID, Quantity: 1, Included: true},
	}); err == nil && len(res.Lines) == 1 && res.Lines[0].Priced {
		panel.ContextPrice = esi.FormatISK(res.Lines[0].UnitPrice)
		panel.ContextTier = tierLabel(res.Lines[0].Tier)
	}
	return panel
}

// formatAttempts words an expected-attempts figure:
// whole numbers once they are big, one decimal while small.
func formatAttempts(v float64) string {
	if v >= 100 {
		return esi.FormatInt(int64(v + 0.5))
	}
	if v >= 10 {
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.1f", v)
}

func absf(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// formatAttrValue renders a dogma attribute value compactly:
// integers whole, fractions to two places.
func formatAttrValue(v float64) string {
	if v == float64(int64(v)) {
		return esi.FormatInt(int64(v))
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", v), "0"), ".")
}

// viewInGameState works out what the detail page's View in
// game control should say for this reader: hidden signed out,
// live when one of their characters holds the scope, and an
// honest sign-in-again note when every grant predates it --
// never a button that silently does nothing.
func (app *mod) viewInGameState(ctx context.Context) viewInGameState {
	userID := app.deps.UserID(ctx)
	if userID == 0 {
		return viewInGameState{}
	}
	state := viewInGameState{Visible: true}
	chars, err := app.queries.ListCharactersByUser(ctx, userID)
	if err != nil || len(chars) == 0 {
		state.Note = "Link a character to open contracts in the game client."
		return state
	}
	for _, ch := range chars {
		if module.CharacterHasScope(ch, openWindowScope) {
			state.Enabled = true
			return state
		}
	}
	state.Note = "Your characters were linked before EveSynapse could open contracts in the game."
	state.Relink = app.deps.RelinkURL("contracts", chars[0].CharacterID)
	return state
}

// handleOpenContractInGame posts ESI's open-window call for
// the contract as one of the reader's characters holding the
// scope (the session character when it qualifies, else the
// first that does). A reader whose grants all predate the
// scope gets the sign-in-again flash, never a silent failure.
func (app *mod) handleOpenContractInGame(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	detailURL := "/contracts/search/"
	id, err := strconv.ParseInt(chi.URLParam(r, "contractID"), 10, 64)
	if err != nil || id <= 0 {
		http.Redirect(w, r, detailURL, http.StatusSeeOther)
		return
	}
	detailURL = fmt.Sprintf("/contracts/search/%d/", id)
	userID := app.deps.UserID(ctx)
	chars, err := app.queries.ListCharactersByUser(ctx, userID)
	if err != nil {
		logging.Errorf("openwindow: characters for user %d: %v", userID, err)
		app.deps.FlashBack(w, r, "That did not go through — please try again.")
		return
	}
	var chosen *db.Character
	sessionID := app.deps.SessionCharacterID(ctx)
	for i := range chars {
		if module.CharacterHasScope(chars[i], openWindowScope) {
			if chars[i].CharacterID == sessionID {
				chosen = &chars[i]
				break
			}
			if chosen == nil {
				chosen = &chars[i]
			}
		}
	}
	if chosen == nil {
		app.deps.FlashBack(w, r, "None of your linked characters can open contracts in the game yet — sign in again to grant EveSynapse permission to open windows in your game client.")
		http.Redirect(w, r, detailURL, http.StatusSeeOther)
		return
	}
	token, err := app.deps.AccessToken(ctx, *chosen)
	if err != nil {
		logging.Errorf("openwindow: token for character %d: %v", chosen.CharacterID, err)
		app.deps.FlashBack(w, r, "That did not go through — please try again.")
		return
	}
	path := fmt.Sprintf("/ui/openwindow/contract/?contract_id=%d", id)
	if err := app.deps.PostESINoContent(ctx, token, path, nil); err != nil {
		var se *esi.StatusError
		if errors.As(err, &se) && se.Code == http.StatusForbidden {
			app.deps.FlashBack(w, r, "The game says this character cannot open windows for EveSynapse — sign in again to grant that permission.")
		} else {
			logging.Errorf("openwindow: contract %d as %d: %v", id, chosen.CharacterID, err)
			app.deps.FlashBack(w, r, "The game did not answer — the contract was not opened.")
		}
		http.Redirect(w, r, detailURL, http.StatusSeeOther)
		return
	}
	app.deps.FlashBack(w, r, "Opening that contract in your game client now.")
	http.Redirect(w, r, detailURL, http.StatusSeeOther)
}
