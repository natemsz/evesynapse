package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// ---------------------------------------------------------------------------
// Phase 1B: the signed-in Home is no longer a single character
// sheet — it is the account overview: a customizable set of
// widgets that read across every linked character. Every widget
// renders cache-only from worker-warmed snapshots (the same rule
// as every other page); the old home's live identity fetch is
// gone — identity now comes from the profile snapshot the worker
// keeps warm (esi.SnapProfile), and the character sheet itself
// lives at /character/.
//
// Layout: which widgets are on, in what order, persisted per
// account as a JSON array of widget ids on the user record
// (schema 010). Unknown ids are ignored on load, so a layout
// saved by a newer build never breaks an older binary.
// ---------------------------------------------------------------------------

// Widget ids. These strings are persisted in users.home_layout;
// never rename one.
const (
	widgetFleet     = "fleet"
	widgetAttention = "attention"
	widgetNetWorth  = "networth"
	widgetIndustry  = "industry"
	widgetMarket    = "market"
	widgetSkills    = "skills"
	widgetServer    = "server"
)

// widgetDef is one entry of the widget catalog: what Customize
// offers and what a saved layout may name.
type widgetDef struct {
	ID          string
	Title       string
	Description string
}

var homeWidgetCatalog = []widgetDef{
	{widgetFleet, "Fleet overview", "Every linked character at a glance: where they are, what they're flying, what they're training."},
	{widgetAttention, "Needs attention", "Characters that need you: re-links, idle queues, finished jobs, expiring orders, waiting contracts."},
	{widgetNetWorth, "Net worth", "Wallets, assets and open-order escrow across all characters, at market prices. An estimate."},
	{widgetIndustry, "Industry", "Active industry jobs across characters, soonest delivery first."},
	{widgetMarket, "Market", "Open orders across characters: counts, sell/buy value, orders expiring soonest."},
	{widgetSkills, "Skills", "The next skill finishes across the fleet, plus who isn't training."},
	{widgetServer, "Tranquility", "Server status: players online."},
}

// defaultHomeLayout is what accounts with no saved layout get:
// the fleet first, what needs the user next, then the money.
var defaultHomeLayout = []string{
	widgetFleet, widgetAttention, widgetNetWorth,
	widgetIndustry, widgetMarket, widgetServer,
}

func widgetDefFor(id string) (widgetDef, bool) {
	for _, def := range homeWidgetCatalog {
		if def.ID == id {
			return def, true
		}
	}
	return widgetDef{}, false
}

// parseHomeLayout normalizes a persisted layout: unknown ids
// dropped, duplicates dropped, order preserved. Empty storage
// (or unparseable JSON) yields the default layout; a saved empty
// array is honored — the user turned everything off.
func parseHomeLayout(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return append([]string(nil), defaultHomeLayout...)
	}
	var ids []string
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		return append([]string(nil), defaultHomeLayout...)
	}
	out := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if _, ok := widgetDefFor(id); !ok || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// encodeHomeLayout serializes a normalized layout for storage.
func encodeHomeLayout(ids []string) string {
	if ids == nil {
		ids = []string{}
	}
	data, err := json.Marshal(ids)
	if err != nil {
		return "[]"
	}
	return string(data)
}

// ---------------------------------------------------------------------------
// Snapshot store: one batched read of the kinds the visible
// widgets need, decoded per character on demand. Rendering never
// calls esi.Get/GetCached — a stale or missing snapshot dims its
// own widget, exactly like every other page.
// ---------------------------------------------------------------------------

var widgetSnapshotKinds = map[string][]string{
	widgetFleet:     {esi.SnapProfile, esi.SnapCorpInfo, esi.SnapLocation, esi.SnapShip, esi.SnapOnline, esi.SnapSkillqueue, esi.SnapWallet},
	widgetAttention: {esi.SnapSkillqueue, esi.SnapIndustryJobs, esi.SnapContracts, esi.SnapOrders},
	widgetNetWorth:  {esi.SnapWallet, esi.SnapAssets, esi.SnapOrders},
	widgetIndustry:  {esi.SnapIndustryJobs},
	widgetMarket:    {esi.SnapOrders},
	widgetSkills:    {esi.SnapSkillqueue},
	widgetServer:    {},
}

// charSnaps is one character's decoded snapshot bundle. Fields
// stay nil/false when the snapshot is absent or undecodable —
// builders treat that as "unknown", never as an error.
type charSnaps struct {
	ch db.Character

	profile  *esi.Character
	corpInfo *esi.Corporation

	location *esi.Location
	ship     *esi.Ship
	online   *esi.Online

	walletKnown bool
	wallet      float64

	queueKnown bool
	queue      esi.Skillqueue

	ordersKnown bool
	orders      []esi.CharOrder

	jobsKnown bool
	jobs      []esi.IndustryJob

	contractsKnown bool
	contracts      []esi.Contract

	assetsKnown bool
	assets      []esi.Asset

	// fetched records each snapshot's fetch timestamp (RFC3339)
	// so widgets can date their data ("as of").
	fetched map[string]string
}

// loadCharSnaps reads the snapshots of every linked character in
// one query (kinds restricted to what the visible widgets need)
// and decodes them into per-character bundles.
func (app *Application) loadCharSnaps(ctx context.Context, userID int64, chars []db.Character, layout []string) []*charSnaps {
	kindSet := map[string]bool{}
	for _, id := range layout {
		for _, kind := range widgetSnapshotKinds[id] {
			kindSet[kind] = true
		}
	}
	kinds := make([]string, 0, len(kindSet))
	for kind := range kindSet {
		kinds = append(kinds, kind)
	}

	bundles := make(map[int64]*charSnaps, len(chars))
	out := make([]*charSnaps, 0, len(chars))
	for _, ch := range chars {
		b := &charSnaps{ch: ch}
		bundles[ch.CharacterID] = b
		out = append(out, b)
	}
	if len(kinds) == 0 {
		return out
	}

	rows, err := app.queries.ListSnapshotsForUser(ctx, db.ListSnapshotsForUserParams{
		UserID: userID,
		Kinds:  kinds,
	})
	if err != nil {
		// A store failure degrades the whole overview to
		// character shells (name/tags) plus warming notes,
		// never to an error page.
		log.Printf("home: list snapshots for user %d: %v", userID, err)
		return out
	}
	for _, row := range rows {
		b := bundles[row.CharacterID]
		if b == nil {
			continue
		}
		if b.fetched == nil {
			b.fetched = map[string]string{}
		}
		b.fetched[row.Kind] = row.FetchedAt
		b.decode(row.Kind, row.Payload)
	}
	return out
}

func (b *charSnaps) decode(kind, payload string) {
	switch kind {
	case esi.SnapProfile:
		var v esi.Character
		if json.Unmarshal([]byte(payload), &v) == nil {
			b.profile = &v
		}
	case esi.SnapCorpInfo:
		var v esi.Corporation
		if json.Unmarshal([]byte(payload), &v) == nil {
			b.corpInfo = &v
		}
	case esi.SnapLocation:
		var v esi.Location
		if json.Unmarshal([]byte(payload), &v) == nil {
			b.location = &v
		}
	case esi.SnapShip:
		var v esi.Ship
		if json.Unmarshal([]byte(payload), &v) == nil {
			b.ship = &v
		}
	case esi.SnapOnline:
		var v esi.Online
		if json.Unmarshal([]byte(payload), &v) == nil {
			b.online = &v
		}
	case esi.SnapWallet:
		var v float64
		if json.Unmarshal([]byte(payload), &v) == nil {
			b.walletKnown, b.wallet = true, v
		}
	case esi.SnapSkillqueue:
		var v esi.Skillqueue
		if json.Unmarshal([]byte(payload), &v) == nil {
			b.queueKnown, b.queue = true, v
		}
	case esi.SnapOrders:
		var v []esi.CharOrder
		if json.Unmarshal([]byte(payload), &v) == nil {
			b.ordersKnown, b.orders = true, v
		}
	case esi.SnapIndustryJobs:
		var v []esi.IndustryJob
		if json.Unmarshal([]byte(payload), &v) == nil {
			b.jobsKnown, b.jobs = true, v
		}
	case esi.SnapContracts:
		var v []esi.Contract
		if json.Unmarshal([]byte(payload), &v) == nil {
			b.contractsKnown, b.contracts = true, v
		}
	case esi.SnapAssets:
		var v []esi.Asset
		if json.Unmarshal([]byte(payload), &v) == nil {
			b.assetsKnown, b.assets = true, v
		}
	}
}

// corpName resolves the character's corporation display name:
// profile's corporation id named by the character's own warmed
// corp_info snapshot, else the honest id fallback.
func (b *charSnaps) corpName() string {
	if b.profile != nil && b.profile.CorporationID > 0 {
		if b.corpInfo != nil && b.corpInfo.Name != "" {
			return b.corpInfo.Name
		}
		return fmt.Sprintf("Corporation #%d", b.profile.CorporationID)
	}
	return ""
}

// currentEntry returns the queue's head entry (position 0) — the
// skill training right now — or nil.
func queueHead(queue esi.Skillqueue) *esi.SkillqueueEntry {
	for i := range queue {
		if queue[i].QueuePosition == 0 {
			return &queue[i]
		}
	}
	return nil
}

// queueState summarizes a character's training: known is false
// until the skillqueue snapshot exists; training is true when a
// future-dated entry remains; finishedAt is the last finish when
// everything in the queue already completed.
func queueState(queue esi.Skillqueue, now time.Time) (training bool, finishedAt time.Time) {
	for _, e := range queue {
		if t, err := time.Parse(time.RFC3339, e.FinishDate); err == nil {
			if t.After(now) {
				training = true
			}
			if t.After(finishedAt) {
				finishedAt = t
			}
		}
	}
	return training, finishedAt
}

// ---------------------------------------------------------------------------
// Widget view models.
// ---------------------------------------------------------------------------

type homeWidget struct {
	ID        string
	Title     string
	Fleet     *fleetWidget
	Attention *attentionWidget
	NetWorth  *netWorthWidget
	Industry  *industryWidget
	Market    *marketWidget
	Skills    *skillsWidget
	Server    *serverStatusView
}

// homeView is the signed-in Home body (pageData.Home).
type homeView struct {
	Widgets    []homeWidget
	HasChars   bool
	Customize  *customizeView // non-nil in customize mode
	LoadFailed bool           // character list unreadable
}

// fleetRow is one line of the fleet overview.
type fleetRow struct {
	ID             int64
	Name           string
	PortraitURL    string
	CorpName       string
	Tags           string
	SystemName     string
	DockedName     string
	ShipTypeName   string
	ShipName       string
	OnlineKnown    bool
	Online         bool
	LastLogin      string
	QueueKnown     bool
	NotTraining    bool
	Training       string // "Skill V", "" when idle
	TrainingFinish string // RFC3339 for the live countdown
	TrainingLeft   string
	WalletKnown    bool
	ISK            string
	Relink         bool
	Search         string // lowercase filter haystack (name/tags/corp/system)
}

type fleetWidget struct {
	Rows []fleetRow
	Tags []string // every tag in use, for the filter chips
}

func splitTags(raw string) []string {
	return strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == ';'
	})
}

// attentionItem is one Needs-attention line. Rank orders the
// severities (fixed rule order, most actionable first); At breaks
// ties chronologically.
type attentionItem struct {
	Char string // character the item is about
	Text string
	Link string
	When string // short human timing, may be ""
	Rank int
	At   time.Time
}

type attentionWidget struct {
	Items []attentionItem
	More  int // items hidden past the cap
}

const attentionCap = 12

type netWorthWidget struct {
	Any         bool
	Total       string
	Wallet      string
	Assets      string
	AssetsKnown bool // false: no prices cache yet or no asset data
	Escrow      string
	AsOf        string
}

type industryRow struct {
	Char     string
	Activity string
	Name     string
	Ends     string
	Left     string
}

type industryWidget struct {
	Rows       []industryRow
	ActiveJobs int
	ReadyJobs  int
}

type marketExpiry struct {
	Char    string
	Item    string
	Expires string
	Left    string
}

type marketWidget struct {
	OrdersKnown int // characters with an orders snapshot
	Open        int
	SellCount   int
	SellValue   string
	BuyCount    int
	BuyValue    string
	Expiring    []marketExpiry
}

type skillFinish struct {
	Char   string
	Skill  string
	Finish string
	Left   string
}

type skillsWidget struct {
	Finishing   []skillFinish
	NotTraining int
}

// orderExpiry computes an order's expiry from ESI's issued +
// duration-days pair.
func orderExpiry(o esi.CharOrder) (time.Time, bool) {
	issued, err := time.Parse(time.RFC3339, o.Issued)
	if err != nil || o.Duration <= 0 {
		return time.Time{}, false
	}
	return issued.Add(time.Duration(o.Duration) * 24 * time.Hour), true
}

// parseRFC3339 parses a snapshot timestamp; ok=false for ""/junk.
func parseRFC3339(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, raw)
	return t, err == nil
}

// ---------------------------------------------------------------------------
// Widget builders. All pure reads over the decoded bundles; the
// only app state they touch is the type-name cache and the
// market-prices cache (both local, both cache-only).
// ---------------------------------------------------------------------------

func (app *Application) buildFleet(ctx context.Context, bundles []*charSnaps) *fleetWidget {
	tagSeen := map[string]bool{}
	var rows []fleetRow
	for _, b := range bundles {
		row := fleetRow{
			ID:          b.ch.CharacterID,
			Name:        b.ch.Name,
			PortraitURL: portraitURL(b.ch.CharacterID, 64),
			CorpName:    b.corpName(),
			Tags:        b.ch.Tags,
			Relink:      b.ch.LinkState != "" && b.ch.LinkState != linkStateOK,
			WalletKnown: b.walletKnown,
		}
		for _, tag := range splitTags(b.ch.Tags) {
			tagSeen[tag] = true
		}
		if b.location != nil {
			row.SystemName = app.locationTitle(ctx, b.location.SolarSystemID, "solar_system")
			if b.location.StationID > 0 {
				row.DockedName = app.locationTitle(ctx, b.location.StationID, "station")
			} else if b.location.StructureID > 0 {
				row.DockedName = app.locationTitle(ctx, b.location.StructureID, "structure")
			}
		}
		if b.ship != nil {
			row.ShipTypeName = app.typeNameOrID(ctx, b.ship.ShipTypeID)
			row.ShipName = b.ship.ShipName
		}
		if b.online != nil {
			row.OnlineKnown = true
			row.Online = b.online.Online
			row.LastLogin = formatFinish(b.online.LastLogin)
		}
		if b.queueKnown {
			row.QueueKnown = true
			head := queueHead(b.queue)
			if head == nil {
				row.NotTraining = true
			} else {
				name := app.esi.CachedTypeName(ctx, head.SkillID)
				if name == "" {
					name = fmt.Sprintf("Type #%d", head.SkillID)
				}
				row.Training = fmt.Sprintf("%s %s", name, esi.RomanLevel(head.FinishedLevel))
				if t, ok := parseRFC3339(head.FinishDate); ok {
					row.TrainingFinish = t.UTC().Format(time.RFC3339)
					if t.After(time.Now()) {
						row.TrainingLeft = "in " + humanDuration(time.Until(t))
					} else {
						row.NotTraining = true
					}
				}
			}
		}
		if b.walletKnown {
			row.ISK = esi.FormatISK(b.wallet)
		}
		row.Search = strings.ToLower(strings.Join([]string{
			row.Name, row.Tags, row.CorpName, row.SystemName,
		}, " "))
		rows = append(rows, row)
	}
	tags := make([]string, 0, len(tagSeen))
	for tag := range tagSeen {
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	return &fleetWidget{Rows: rows, Tags: tags}
}

// Attention rule ranks (lower = surfaces first). Fixed by rule,
// then chronological within a rank.
const (
	attentionRelink = iota
	attentionNotTraining
	attentionJobReady
	attentionOrderExpiring
	attentionContract
)

func (app *Application) buildAttention(ctx context.Context, bundles []*charSnaps) *attentionWidget {
	now := time.Now()
	var items []attentionItem

	// Job/product names resolve in one batched name-cache pass.
	addJobItem := func(b *charSnaps, j esi.IndustryJob) {
		nameID := j.ProductTypeID
		if nameID == 0 {
			nameID = j.BlueprintTypeID
		}
		name := app.typeNameOrID(ctx, nameID)
		at, _ := parseRFC3339(j.CompletedDate)
		if at.IsZero() {
			at, _ = parseRFC3339(j.EndDate)
		}
		items = append(items, attentionItem{
			Char: b.ch.Name,
			Text: fmt.Sprintf("%s — job ready for delivery: %s (%s)", b.ch.Name, name, industryActivityLabel(j.ActivityID)),
			Link: fmt.Sprintf("/industry/?character=%d", b.ch.CharacterID),
			Rank: attentionJobReady,
			At:   at,
		})
	}

	for _, b := range bundles {
		// Re-link needed: the token is dead or the character
		// changed hands; everything else about them is frozen
		// until they sign in again, so this outranks all else.
		if b.ch.LinkState != "" && b.ch.LinkState != linkStateOK {
			reason := "sign-in expired"
			if b.ch.LinkState == linkStateOwnerChanged {
				reason = "character changed accounts"
			}
			items = append(items, attentionItem{
				Char: b.ch.Name,
				Text: fmt.Sprintf("%s needs re-linking — %s.", b.ch.Name, reason),
				Link: "/characters/",
				Rank: attentionRelink,
			})
		}

		// Not training: the queue snapshot exists and nothing in
		// it finishes in the future.
		if b.queueKnown {
			training, finishedAt := queueState(b.queue, now)
			if !training {
				item := attentionItem{
					Char: b.ch.Name,
					Text: fmt.Sprintf("%s isn't training — the queue is empty.", b.ch.Name),
					Link: fmt.Sprintf("/skills/?character=%d", b.ch.CharacterID),
					Rank: attentionNotTraining,
					At:   finishedAt,
				}
				if !finishedAt.IsZero() {
					item.Text = fmt.Sprintf("%s isn't training — the queue finished %s.", b.ch.Name, formatFinish(b.queueLastFinish()))
					item.When = "finished " + finishedAt.UTC().Format("2006-01-02")
				}
				items = append(items, item)
			}
		}

		// Industry jobs ready for delivery.
		if b.jobsKnown {
			for _, j := range b.jobs {
				if j.Status == "ready" {
					addJobItem(b, j)
				}
			}
		}

		// Orders expiring within 24h.
		if b.ordersKnown {
			for _, o := range b.orders {
				expiry, ok := orderExpiry(o)
				if !ok || expiry.Before(now) || expiry.After(now.Add(24*time.Hour)) {
					continue
				}
				items = append(items, attentionItem{
					Char: b.ch.Name,
					Text: fmt.Sprintf("%s — %s order expires in %s.", b.ch.Name,
						app.typeNameOrID(ctx, o.TypeID), humanDuration(time.Until(expiry))),
					Link: fmt.Sprintf("/orders/?character=%d", b.ch.CharacterID),
					When: "expires " + expiry.UTC().Format("2006-01-02 15:04 UTC"),
					Rank: attentionOrderExpiring,
					At:   expiry,
				})
			}
		}

		// Contracts waiting on this character to accept/act.
		if b.contractsKnown {
			for _, c := range b.contracts {
				if c.Status != "outstanding" || c.AssigneeID != b.ch.CharacterID || c.AcceptorID != 0 {
					continue
				}
				label := c.Title
				if label == "" {
					label = c.Type + " contract"
				}
				at, _ := parseRFC3339(c.DateExpired)
				items = append(items, attentionItem{
					Char: b.ch.Name,
					Text: fmt.Sprintf("%s — contract waiting for you: %s.", b.ch.Name, label),
					Link: fmt.Sprintf("/contracts/?character=%d", b.ch.CharacterID),
					Rank: attentionContract,
					At:   at,
				})
			}
		}
	}

	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Rank != items[j].Rank {
			return items[i].Rank < items[j].Rank
		}
		return items[i].At.Before(items[j].At)
	})
	w := &attentionWidget{}
	if len(items) > attentionCap {
		w.More = len(items) - attentionCap
		items = items[:attentionCap]
	}
	w.Items = items
	return w
}

// queueLastFinish returns the latest finish_date in the queue, or
// "" when no entry carries one.
func (b *charSnaps) queueLastFinish() string {
	last := ""
	for _, e := range b.queue {
		if e.FinishDate > last {
			last = e.FinishDate
		}
	}
	return last
}

func (app *Application) buildNetWorth(ctx context.Context, bundles []*charSnaps) *netWorthWidget {
	var walletSum, assetSum, escrowSum float64
	var walletOK, assetsOK, escrowOK bool
	var asOf time.Time
	prices := app.cachedPrices()
	w := &netWorthWidget{}

	// The estimate is only as fresh as its stalest input.
	noteAsOf := func(b *charSnaps, kind string) {
		if t, ok := parseRFC3339(b.fetched[kind]); ok {
			if asOf.IsZero() || t.Before(asOf) {
				asOf = t
			}
		}
	}

	for _, b := range bundles {
		if b.walletKnown {
			walletSum += b.wallet
			walletOK = true
			noteAsOf(b, esi.SnapWallet)
		}
		// Assets price off the warmed adjusted/average market
		// prices; with no price cache yet the widget says so
		// instead of guessing. Unpriced types are skipped, the
		// same rule killmail valuation follows.
		if b.assetsKnown && prices != nil {
			valued := false
			for _, a := range b.assets {
				p, ok := prices[a.TypeID]
				if !ok {
					continue
				}
				price := p.AveragePrice
				if price <= 0 {
					price = p.AdjustedPrice
				}
				if price <= 0 {
					continue
				}
				assetSum += float64(a.Quantity) * price
				valued = true
			}
			if valued {
				assetsOK = true
				noteAsOf(b, esi.SnapAssets)
			}
		}
		if b.ordersKnown {
			for _, o := range b.orders {
				if o.IsBuyOrder {
					escrowSum += o.Escrow
					escrowOK = true
				}
			}
			if escrowOK {
				noteAsOf(b, esi.SnapOrders)
			}
		}
	}

	total := walletSum + assetSum + escrowSum
	if walletOK || assetsOK || escrowOK {
		w.Any = true
		w.Total = esi.FormatISK(total)
	}
	if walletOK {
		w.Wallet = esi.FormatISK(walletSum)
	}
	if assetsOK {
		w.Assets = esi.FormatISK(assetSum)
		w.AssetsKnown = true
	}
	if escrowOK {
		w.Escrow = esi.FormatISK(escrowSum)
	}
	if !asOf.IsZero() {
		w.AsOf = formatFinish(asOf.UTC().Format(time.RFC3339))
	}
	return w
}

func (app *Application) buildIndustry(ctx context.Context, bundles []*charSnaps) *industryWidget {
	now := time.Now()
	// Product/blueprint names resolve in one batched pass.
	var ids []int64
	for _, b := range bundles {
		for _, j := range b.jobs {
			if j.ProductTypeID > 0 {
				ids = append(ids, j.ProductTypeID)
			}
			ids = append(ids, j.BlueprintTypeID)
		}
	}
	names := app.esi.CachedTypeNames(ctx, ids)
	nameFor := func(id int64) string {
		if name, ok := names[id]; ok && name != "" {
			return name
		}
		return fmt.Sprintf("Type #%d", id)
	}

	type pending struct {
		row industryRow
		end time.Time
	}
	var pendingRows []pending
	w := &industryWidget{}
	for _, b := range bundles {
		if !b.jobsKnown {
			continue
		}
		for _, j := range b.jobs {
			switch j.Status {
			case "ready":
				w.ReadyJobs++
			case "active", "paused":
				w.ActiveJobs++
				end, ok := parseRFC3339(j.EndDate)
				if !ok {
					continue
				}
				nameID := j.ProductTypeID
				if nameID == 0 {
					nameID = j.BlueprintTypeID
				}
				pendingRows = append(pendingRows, pending{
					row: industryRow{
						Char:     b.ch.Name,
						Activity: industryActivityLabel(j.ActivityID),
						Name:     nameFor(nameID),
						Ends:     formatFinish(j.EndDate),
						Left:     humanDuration(time.Until(end)),
					},
					end: end,
				})
			}
		}
	}
	sort.SliceStable(pendingRows, func(i, j int) bool {
		return pendingRows[i].end.Before(pendingRows[j].end)
	})
	for i, p := range pendingRows {
		if i >= 8 {
			break
		}
		_ = now
		w.Rows = append(w.Rows, p.row)
	}
	return w
}

func (app *Application) buildMarket(ctx context.Context, bundles []*charSnaps) *marketWidget {
	now := time.Now()
	w := &marketWidget{}
	var sellSum, buySum float64
	type expiryRow struct {
		row marketExpiry
		at  time.Time
	}
	var expiring []expiryRow
	for _, b := range bundles {
		if !b.ordersKnown {
			continue
		}
		w.OrdersKnown++
		for _, o := range b.orders {
			w.Open++
			if o.IsBuyOrder {
				w.BuyCount++
				buySum += o.Escrow
			} else {
				w.SellCount++
				sellSum += o.Price * float64(o.VolumeRemain)
			}
			if expiry, ok := orderExpiry(o); ok && expiry.After(now) {
				expiring = append(expiring, expiryRow{
					row: marketExpiry{
						Char:    b.ch.Name,
						Item:    app.typeNameOrID(ctx, o.TypeID),
						Expires: formatFinish(expiry.UTC().Format(time.RFC3339)),
						Left:    humanDuration(time.Until(expiry)),
					},
					at: expiry,
				})
			}
		}
	}
	sort.SliceStable(expiring, func(i, j int) bool {
		return expiring[i].at.Before(expiring[j].at)
	})
	for i, e := range expiring {
		if i >= 5 {
			break
		}
		w.Expiring = append(w.Expiring, e.row)
	}
	if w.Open > 0 {
		w.SellValue = esi.FormatISK(sellSum)
		w.BuyValue = esi.FormatISK(buySum)
	}
	return w
}

func (app *Application) buildSkills(ctx context.Context, bundles []*charSnaps) *skillsWidget {
	now := time.Now()
	type pending struct {
		row    skillFinish
		finish time.Time
	}
	var pendingRows []pending
	w := &skillsWidget{}
	for _, b := range bundles {
		if !b.queueKnown {
			continue
		}
		training, _ := queueState(b.queue, now)
		if !training {
			w.NotTraining++
			continue
		}
		head := queueHead(b.queue)
		if head == nil {
			continue
		}
		finish, ok := parseRFC3339(head.FinishDate)
		if !ok {
			continue
		}
		name := app.esi.CachedTypeName(ctx, head.SkillID)
		if name == "" {
			name = fmt.Sprintf("Type #%d", head.SkillID)
		}
		pendingRows = append(pendingRows, pending{
			row: skillFinish{
				Char:   b.ch.Name,
				Skill:  fmt.Sprintf("%s %s", name, esi.RomanLevel(head.FinishedLevel)),
				Finish: formatFinish(head.FinishDate),
				Left:   humanDuration(time.Until(finish)),
			},
			finish: finish,
		})
	}
	sort.SliceStable(pendingRows, func(i, j int) bool {
		return pendingRows[i].finish.Before(pendingRows[j].finish)
	})
	for i, p := range pendingRows {
		if i >= 8 {
			break
		}
		w.Finishing = append(w.Finishing, p.row)
	}
	return w
}

// ---------------------------------------------------------------------------
// Home assembly + layout handlers.
// ---------------------------------------------------------------------------

// buildHome assembles the signed-in Home body: the user's layout
// applied to freshly-decoded snapshot bundles. Customize mode
// returns the catalog state instead — the widgets themselves are
// not built (nothing to show until Done).
func (app *Application) buildHome(ctx context.Context, customize bool) *homeView {
	view := &homeView{}
	userID := int64(app.sessions.GetInt(ctx, sessionUserID))
	if userID == 0 {
		// Dev-login sessions carry no user: clean empty state.
		return view
	}

	chars, err := app.queries.ListCharactersByUser(ctx, userID)
	if err != nil {
		log.Printf("home: list characters for user %d: %v", userID, err)
		view.LoadFailed = true
		return view
	}
	view.HasChars = len(chars) > 0

	raw, err := app.queries.GetUserHomeLayout(ctx, userID)
	if err != nil {
		raw = ""
	}
	layout := parseHomeLayout(raw)

	if customize {
		view.Customize = buildCustomizeView(layout)
		return view
	}
	if !view.HasChars || len(layout) == 0 {
		return view
	}

	bundles := app.loadCharSnaps(ctx, userID, chars, layout)
	for _, id := range layout {
		def, _ := widgetDefFor(id)
		w := homeWidget{ID: id, Title: def.Title}
		switch id {
		case widgetFleet:
			w.Fleet = app.buildFleet(ctx, bundles)
		case widgetAttention:
			w.Attention = app.buildAttention(ctx, bundles)
		case widgetNetWorth:
			w.NetWorth = app.buildNetWorth(ctx, bundles)
		case widgetIndustry:
			w.Industry = app.buildIndustry(ctx, bundles)
		case widgetMarket:
			w.Market = app.buildMarket(ctx, bundles)
		case widgetSkills:
			w.Skills = app.buildSkills(ctx, bundles)
		case widgetServer:
			if status, ok := app.loadServerStatus(ctx); ok {
				w.Server = status
			}
		}
		view.Widgets = append(view.Widgets, w)
	}
	return view
}

// customizeEntry is one catalog row in Customize mode.
type customizeEntry struct {
	ID          string
	Title       string
	Description string
	Enabled     bool
	Position    int // 1-based among enabled; 0 when disabled
	CanUp       bool
	CanDown     bool
}

type customizeView struct {
	Entries []customizeEntry
}

func buildCustomizeView(layout []string) *customizeView {
	position := make(map[string]int, len(layout))
	for i, id := range layout {
		position[id] = i + 1
	}
	view := &customizeView{}
	for _, def := range homeWidgetCatalog {
		entry := customizeEntry{
			ID:          def.ID,
			Title:       def.Title,
			Description: def.Description,
			Enabled:     position[def.ID] > 0,
			Position:    position[def.ID],
		}
		if entry.Enabled {
			entry.CanUp = entry.Position > 1
			entry.CanDown = entry.Position < len(layout)
		}
		view.Entries = append(view.Entries, entry)
	}
	return view
}

// saveHomeLayout persists a normalized layout for the user.
func (app *Application) saveHomeLayout(ctx context.Context, userID int64, layout []string) {
	err := app.queries.SetUserHomeLayout(ctx, db.SetUserHomeLayoutParams{
		HomeLayout: encodeHomeLayout(layout),
		ID:         userID,
	})
	if err != nil {
		log.Printf("home: save layout for user %d: %v", userID, err)
	}
}

// handleHomeLayout applies one layout change (POST /home/layout):
// toggle a widget on/off, move one up/down, or reset to the
// default. Every control is a plain form, so reordering works
// with no JavaScript; the change saves immediately and bounces
// back to Customize.
func (app *Application) handleHomeLayout(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := int64(app.sessions.GetInt(ctx, sessionUserID))
	if userID == 0 {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	raw, err := app.queries.GetUserHomeLayout(ctx, userID)
	if err != nil {
		raw = ""
	}
	layout := parseHomeLayout(raw)

	if err := r.ParseForm(); err == nil {
		action := r.FormValue("action")
		widget := r.FormValue("widget")
		_, known := widgetDefFor(widget)
		switch action {
		case "reset":
			layout = append([]string(nil), defaultHomeLayout...)
			// Storing "" would mean "no saved layout" — which
			// is the default anyway, so store the canonical
			// default JSON instead to keep intent explicit.
			app.saveHomeLayout(ctx, userID, layout)
			http.Redirect(w, r, "/?customize=1", http.StatusSeeOther)
			return
		case "toggle":
			if known {
				found := -1
				for i, id := range layout {
					if id == widget {
						found = i
					}
				}
				if found >= 0 {
					layout = append(layout[:found], layout[found+1:]...)
				} else {
					layout = append(layout, widget)
				}
				app.saveHomeLayout(ctx, userID, layout)
			}
		case "up", "down":
			if known {
				for i, id := range layout {
					if id != widget {
						continue
					}
					j := i - 1
					if action == "down" {
						j = i + 1
					}
					if j >= 0 && j < len(layout) {
						layout[i], layout[j] = layout[j], layout[i]
					}
					break
				}
				app.saveHomeLayout(ctx, userID, layout)
			}
		}
	}
	http.Redirect(w, r, "/?customize=1", http.StatusSeeOther)
}
