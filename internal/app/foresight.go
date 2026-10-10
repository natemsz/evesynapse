package app

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Foresight: warming data because something happened in the game.
//
// The worker already fetches what a page is waiting on, and what is
// close to an account's own records. This is the third reason to
// fetch: an event in an account's stored data that makes a page
// likely to be opened next.
//
//	a character docks at a trade hub     the hub's prices for what is in
//	                                     its hangar there
//	an industry job is about to finish   the product's sell price
//	a market order is undercut           the order book it competes in
//	a skill queue runs out               the character's skills
//	a war is declared on a corporation   the attacker's public record
//
// Each event is read from data the worker has already stored: nothing
// is fetched to find one. It becomes one or more targets
// (foresight_targets): a thing to warm, and the page that would use it.
//
// Which targets get warmed is a matter of worth, not of order. Every
// pending target is scored
//
//	chance the page is opened  x  how stale the data is
//	---------------------------------------------------
//	          ESI calls it costs to warm
//
// and the best are warmed first, out of what is left of the cycle's
// fetch allowance and never more than FORESIGHT_FETCHES_PER_CYCLE.
//
// The chance is learned. Each kind of event starts from an assumed
// chance; every target that runs its course either was followed by its
// page being opened or was not, and the chance in use is that record
// weighed against the assumption, for everyone and then for the
// account itself. A kind of event nobody follows up on stops being
// worth calls; one an account always follows up on is warmed for that
// account first.
//
// The Sync page shows what it cost and what came of it.
// ---------------------------------------------------------------------------

// Kinds of event (foresight_targets.event_kind) and of target
// (target_kind). Stored: never rename.
const (
	eventDock     = "dock"
	eventJob      = "job"
	eventUndercut = "undercut"
	eventQueue    = "queue"
	eventWar      = "war"

	targetBook        = "book"        // a region's order book for a type
	targetHistory     = "history"     // a region's price history for a type
	targetSnapshot    = "snapshot"    // one of a character's own datasets
	targetCorporation = "corporation" // a corporation's public record
	targetAlliance    = "alliance"    // an alliance's public record
)

// foresightKind is one kind of event: what it is called, the chance
// assumed for it until its own record says otherwise, and how long
// after it the page still counts as having been opened because of it.
type foresightKind struct {
	ID     string
	Title  string
	Chance float64
	Window time.Duration
}

var foresightKinds = []foresightKind{
	{eventDock, "Docked at a trade hub", 0.30, 2 * time.Hour},
	{eventJob, "Industry job finishing", 0.30, 6 * time.Hour},
	{eventUndercut, "Market order undercut", 0.45, 3 * time.Hour},
	{eventQueue, "Skill queue ran out", 0.30, 12 * time.Hour},
	{eventWar, "War declared", 0.40, 24 * time.Hour},
}

func foresightKindByID(id string) foresightKind {
	for _, k := range foresightKinds {
		if k.ID == id {
			return k
		}
	}
	return foresightKind{ID: id, Title: id, Chance: 0.2, Window: 2 * time.Hour}
}

const (
	// foresightAssumedWeight is how many observed targets the assumed
	// chance counts for, and foresightAccountWeight how many the chance
	// learned from everyone counts for against one account's own.
	foresightAssumedWeight = 20.0
	foresightAccountWeight = 10.0
	// foresightLearnWindow is how far back the record learned from goes,
	// and foresightKeep how long a target is kept at all.
	foresightLearnWindow = 30 * 24 * time.Hour
	foresightKeep        = 45 * 24 * time.Hour

	foresightHangarTypes  = 6 // types warmed for one docking
	foresightJobLead      = 20 * time.Minute
	foresightJobLate      = 10 * time.Minute
	foresightQueueWindow  = 24 * time.Hour
	foresightWarWindow    = 48 * time.Hour
	foresightWarScanEvery = 10 * time.Minute
	foresightWarsRead     = 100
	foresightPendingRead  = 200

	defaultForesightFetches = 30
	maxForesightFetches     = 200
)

// tradeHubs are the stations whose market an arriving character is
// most likely there for, with the region each trades in.
var tradeHubs = map[int64]int64{
	60003760: 10000002, // Jita IV - Moon 4 - Caldari Navy Assembly Plant
	60008494: 10000043, // Amarr VIII (Oris) - Emperor Family Academy
	60011866: 10000032, // Dodixie IX - Moon 20 - Federation Navy Assembly Plant
	60004588: 10000030, // Rens VI - Moon 8 - Brutor Tribe Treasury
	60005686: 10000042, // Hek VIII - Moon 12 - Boundless Creation Factory
}

// Industry activities whose product is something to sell.
var foresightSellableActivity = map[int64]bool{1: true, 9: true, 11: true}

// foresightState is what foresight keeps between cycles.
type foresightState struct {
	mu sync.Mutex
	// station is where each character was last seen docked (0: not
	// docked). A docking is a change to it; the first sighting after a
	// start is only a baseline.
	station map[int64]int64
	// noted is the targets already written, so that a condition which
	// stays true is not written again every pass.
	noted       map[string]bool
	lastWarScan time.Time
	lastPrune   time.Time
	// live is how many targets a page view could still count for. A
	// page view asks the database nothing while it is zero.
	live atomic.Int64
}

func parseForesightFetches(raw string) int {
	return intSetting("FORESIGHT_FETCHES_PER_CYCLE", raw, 0, maxForesightFetches, defaultForesightFetches)
}

func foresightMarketView(typeID int64) string { return "market:" + strconv.FormatInt(typeID, 10) }
func foresightSkillsView(characterID int64) string {
	return "skills:" + strconv.FormatInt(characterID, 10)
}
func foresightOrgView(kind string, id int64) string { return kind + ":" + strconv.FormatInt(id, 10) }

// foresightEvent is one thing that happened, for one account.
type foresightEvent struct {
	userID      int64
	characterID int64
	kind        string
	key         string
	at          time.Time
}

// target records one thing to warm because of the event. It is written
// once however often the event is seen.
func (app *Application) foresightTarget(ctx context.Context, ev foresightEvent, targetKind string, subject, region int64, detail, viewKey string) {
	memo := fmt.Sprintf("%d|%s|%s|%d|%d|%s", ev.userID, ev.key, targetKind, subject, region, detail)
	st := &app.foresight
	st.mu.Lock()
	if st.noted == nil {
		st.noted = map[string]bool{}
	}
	seen := st.noted[memo]
	if len(st.noted) > 50000 {
		st.noted = map[string]bool{}
	}
	st.noted[memo] = true
	st.mu.Unlock()
	if seen {
		return
	}
	n, err := app.queries.AddForesightTarget(ctx, db.AddForesightTargetParams{
		UserID: ev.userID, CharacterID: ev.characterID, EventKind: ev.kind, EventKey: ev.key,
		TargetKind: targetKind, SubjectID: subject, RegionID: region, Detail: detail, ViewKey: viewKey,
		CreatedAt: ev.at, ExpiresAt: ev.at.Add(foresightKindByID(ev.kind).Window),
	})
	if err != nil {
		logging.Errorf("foresight: note %s target for user %d: %v", ev.kind, ev.userID, err)
		return
	}
	st.live.Add(n)
}

// marketTargets asks for a type's order book and price history in a
// region: what its market page shows.
func (app *Application) foresightMarketTargets(ctx context.Context, ev foresightEvent, regionID, typeID int64) {
	app.foresightTarget(ctx, ev, targetBook, typeID, regionID, "", foresightMarketView(typeID))
	app.foresightTarget(ctx, ev, targetHistory, typeID, regionID, "", foresightMarketView(typeID))
}

// foresee looks through one account's stored data for events, as the
// notification pass reads it (notify.go). It fetches nothing.
func (app *Application) foresee(ctx context.Context, userID int64, bundles []*charSnaps, now time.Time) {
	if app.cfg.foresightOff {
		return
	}
	for _, b := range bundles {
		if !characterSyncs(b.ch) {
			continue
		}
		app.foreseeDock(ctx, userID, b.ch, now)
		app.foreseeJobs(ctx, userID, b, now)
		app.foreseeQueue(ctx, userID, b, now)
	}
	app.foreseeUndercut(ctx, userID, now)
}

// foreseeDock: the character is docked at a trade hub and was not
// when last looked at. What it has in its hangar there is what it is
// most likely there to sell, so those are the prices to have ready:
// the few types worth most.
func (app *Application) foreseeDock(ctx context.Context, userID int64, ch db.Character, now time.Time) {
	var loc esi.Location
	if !app.loadCorpSnapshot(ctx, ch.CharacterID, esi.SnapLocation, &loc) {
		return
	}
	st := &app.foresight
	st.mu.Lock()
	if st.station == nil {
		st.station = map[int64]int64{}
	}
	prev, known := st.station[ch.CharacterID]
	st.station[ch.CharacterID] = loc.StationID
	st.mu.Unlock()
	region, hub := tradeHubs[loc.StationID]
	if !known || !hub || prev == loc.StationID {
		return
	}
	ev := foresightEvent{
		userID: userID, characterID: ch.CharacterID, kind: eventDock, at: now,
		key: fmt.Sprintf("dock|%d|%d|%d", ch.CharacterID, loc.StationID, now.Unix()/3600),
	}
	for _, typeID := range app.hangarTypesWorthMost(ctx, ch.CharacterID, loc.StationID, foresightHangarTypes) {
		app.foresightMarketTargets(ctx, ev, region, typeID)
	}
}

// hangarTypesWorthMost lists the types a character has most value of
// in a station's hangar, by the price guide; by quantity where there
// is no guide.
func (app *Application) hangarTypesWorthMost(ctx context.Context, characterID, stationID int64, most int) []int64 {
	var assets []esi.Asset
	if !app.loadCorpSnapshot(ctx, characterID, esi.SnapAssets, &assets) {
		return nil
	}
	prices := app.valuationPrices(ctx)
	worth := map[int64]float64{}
	for _, a := range assets {
		if a.LocationID != stationID || a.LocationFlag != "Hangar" || a.IsBlueprintCopy {
			continue
		}
		value := float64(a.Quantity)
		if p := prices[a.TypeID].AveragePrice; p > 0 {
			value *= p
		} else if prices != nil {
			continue // nothing the market prices: nothing to sell
		}
		worth[a.TypeID] += value
	}
	ids := make([]int64, 0, len(worth))
	for id := range worth {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if worth[ids[i]] != worth[ids[j]] {
			return worth[ids[i]] > worth[ids[j]]
		}
		return ids[i] < ids[j]
	})
	if len(ids) > most {
		ids = ids[:most]
	}
	return ids
}

// foreseeJobs: a job that makes something to sell is about to finish,
// or just has. The product's price in The Forge is what the builder
// looks up next.
func (app *Application) foreseeJobs(ctx context.Context, userID int64, b *charSnaps, now time.Time) {
	if !b.jobsKnown {
		return
	}
	for _, job := range b.jobs {
		if job.Status != "active" || job.ProductTypeID <= 0 || !foresightSellableActivity[job.ActivityID] {
			continue
		}
		end, err := time.Parse(time.RFC3339, job.EndDate)
		if err != nil || end.After(now.Add(foresightJobLead)) || end.Before(now.Add(-foresightJobLate)) {
			continue
		}
		ev := foresightEvent{userID: userID, characterID: b.ch.CharacterID, kind: eventJob, at: now, key: fmt.Sprintf("job|%d", job.JobID)}
		app.foresightMarketTargets(ctx, ev, defaultMarketRegion, job.ProductTypeID)
	}
}

// foreseeQueue: the last skill in the queue has finished. The stored
// queue says when without being fetched again; what is wanted fresh is
// what the character can now train, which is its skills.
func (app *Application) foreseeQueue(ctx context.Context, userID int64, b *charSnaps, now time.Time) {
	if !b.queueKnown || len(b.queue) == 0 {
		return
	}
	var last time.Time
	for _, entry := range b.queue {
		finish, err := time.Parse(time.RFC3339, entry.FinishDate)
		if err != nil {
			return // a paused queue has no finish times: it has not run out
		}
		if finish.After(last) {
			last = finish
		}
	}
	if last.After(now) || now.Sub(last) > foresightQueueWindow {
		return
	}
	ev := foresightEvent{
		userID: userID, characterID: b.ch.CharacterID, kind: eventQueue, at: now,
		key: fmt.Sprintf("queue|%d|%d", b.ch.CharacterID, last.Unix()),
	}
	for _, kind := range []string{esi.SnapSkills, esi.SnapSkillqueue, esi.SnapAttributes} {
		app.foresightTarget(ctx, ev, targetSnapshot, b.ch.CharacterID, 0, kind, foresightSkillsView(b.ch.CharacterID))
	}
}

// foreseeUndercut: one of the account's sell orders is no longer the
// cheapest (the worker's own order health says so). The order book it
// sits in is what its owner opens to reprice it. Once a day for an
// order, however long it stays undercut.
func (app *Application) foreseeUndercut(ctx context.Context, userID int64, now time.Time) {
	rows, err := app.queries.ListOrderHealthByUser(ctx, userID)
	if err != nil {
		return
	}
	for _, row := range rows {
		if !strings.HasPrefix(row.Status, "undercut") {
			continue
		}
		ev := foresightEvent{
			userID: userID, characterID: row.CharacterID, kind: eventUndercut, at: now,
			key: fmt.Sprintf("undercut|%d|%s", row.OrderID, now.UTC().Format("20060102")),
		}
		app.foresightMarketTargets(ctx, ev, row.RegionID, row.TypeID)
	}
}

// foreseeWars: a war was declared on a corporation, or on the alliance
// of one, that an account has a character in. Whoever declared it is
// who its members look up. Read from the stored wars, a few times an
// hour.
func (app *Application) foreseeWars(ctx context.Context, now time.Time) {
	if app.cfg.foresightOff {
		return
	}
	st := &app.foresight
	st.mu.Lock()
	due := now.Sub(st.lastWarScan) >= foresightWarScanEvery
	if due {
		st.lastWarScan = now
	}
	st.mu.Unlock()
	if !due {
		return
	}
	rows, err := app.queries.ListWarsDeclaredSince(ctx, db.ListWarsDeclaredSinceParams{Since: now.Add(-foresightWarWindow), RowLimit: foresightWarsRead})
	if err != nil {
		logging.Errorf("foresight: read wars: %v", err)
		return
	}
	if len(rows) == 0 {
		return
	}
	known, err := app.queries.ListKnownCorporations(ctx)
	if err != nil || len(known) == 0 {
		return
	}
	inAlliance := map[int64][]int64{}
	isKnown := map[int64]bool{}
	for _, corp := range known {
		isKnown[corp] = true
		if alliance, _ := app.corpAlliance(ctx, corp); alliance != 0 {
			inAlliance[alliance] = append(inAlliance[alliance], corp)
		}
	}
	for _, row := range rows {
		var war esi.War
		if json.Unmarshal([]byte(row.Payload), &war) != nil || war.Finished != "" {
			continue
		}
		var corps []int64
		if isKnown[war.Defender.CorporationID] {
			corps = append(corps, war.Defender.CorporationID)
		}
		corps = append(corps, inAlliance[war.Defender.AllianceID]...)
		if len(corps) == 0 {
			continue
		}
		users, err := app.queries.ListUsersInCorporations(ctx, corps)
		if err != nil {
			continue
		}
		seen := map[int64]bool{}
		for _, u := range users {
			if seen[u.UserID] {
				continue
			}
			seen[u.UserID] = true
			ev := foresightEvent{userID: u.UserID, kind: eventWar, at: now, key: fmt.Sprintf("war|%d", war.ID)}
			if id := war.Aggressor.AllianceID; id > 0 {
				app.foresightTarget(ctx, ev, targetAlliance, id, 0, "", foresightOrgView(targetAlliance, id))
			}
			if id := war.Aggressor.CorporationID; id > 0 {
				app.foresightTarget(ctx, ev, targetCorporation, id, 0, "", foresightOrgView(targetCorporation, id))
			}
		}
	}
}

// foresightOpened records that an account opened a page: any target
// warmed for that page, and still current, was right.
func (app *Application) foresightOpened(ctx context.Context, userID int64, viewKey string) {
	if app.cfg.foresightOff || userID == 0 || app.foresight.live.Load() <= 0 {
		return
	}
	now := time.Now().UTC()
	n, err := app.queries.MarkForesightOpened(ctx, db.MarkForesightOpenedParams{UserID: userID, ViewKey: viewKey, OpenedAt: timeSet(now)})
	if err != nil {
		logging.Errorf("foresight: note %s opened by user %d: %v", viewKey, userID, err)
		return
	}
	if n > 0 {
		app.foresight.live.Add(-n)
	}
}
