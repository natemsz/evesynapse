package app

import (
	"context"
	"math"
	"strconv"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// closeFloat asserts two floats are nearly equal.
func closeFloat(t *testing.T, what string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
}

// Foresight (foresight.go): the worker warms data because something
// happened in the game — a docking, a job finishing, an undercut,
// an empty skill queue, a war — scored by learned chance times
// staleness over cost, and settled free when already fresh.

func foresightPendingTargets(t *testing.T, q *db.Queries, now time.Time) []db.ForesightTarget {
	t.Helper()
	targets, err := q.ListForesightPending(context.Background(), db.ListForesightPendingParams{
		Now: now, RowLimit: foresightPendingRead,
	})
	if err != nil {
		t.Fatalf("list pending targets: %v", err)
	}
	return targets
}

// targetKinds renders the target kinds present, for comparison.
func targetKinds(targets []db.ForesightTarget) map[string]bool {
	out := map[string]bool{}
	for _, tg := range targets {
		out[tg.TargetKind] = true
	}
	return out
}

func TestForesightDockWarmsHangarAtHub(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	ch := seedCharacter(t, q, user.ID, 90000100, "Pilot 0")
	now := time.Now().UTC().Truncate(time.Second)

	// Elsewhere first: the first sighting is only a baseline.
	seedSnapshot(t, q, ch.CharacterID, esi.SnapLocation, esi.Location{SolarSystemID: 30000142, StationID: 60000001})
	app.foreseeDock(ctx, user.ID, ch, now)
	if got := foresightPendingTargets(t, q, now); len(got) != 0 {
		t.Fatalf("baseline sighting queued %d targets, want 0", len(got))
	}

	// Into Jita with stock in the hangar: book plus history for
	// what is there, in Jita's region.
	seedSnapshot(t, q, ch.CharacterID, esi.SnapLocation, esi.Location{SolarSystemID: 30000142, StationID: 60003760})
	seedSnapshot(t, q, ch.CharacterID, esi.SnapAssets, []esi.Asset{
		{ItemID: 1, TypeID: 34, LocationID: 60003760, LocationFlag: "Hangar", Quantity: 100},
		{ItemID: 2, TypeID: 35, LocationID: 60003760, LocationFlag: "Hangar", Quantity: 1},
	})
	app.foreseeDock(ctx, user.ID, ch, now)
	got := foresightPendingTargets(t, q, now)
	want := map[string]bool{"book": true, "history": true}
	if found := targetKinds(got); len(found) != len(want) {
		t.Fatalf("target kinds %v, want %v", found, want)
	}
	seen := map[int64]bool{}
	for _, tg := range got {
		if tg.RegionID != 10000002 || tg.UserID != user.ID {
			t.Fatalf("target %+v outside Jita or for the wrong user", tg)
		}
		seen[tg.SubjectID] = true
	}
	if !seen[34] || !seen[35] || len(got) != 4 {
		t.Fatalf("targets cover %v (%d rows), want book+history for 34 and 35", seen, len(got))
	}

	// Still there next pass: no docking, no new targets.
	app.foreseeDock(ctx, user.ID, ch, now)
	if got := foresightPendingTargets(t, q, now); len(got) != 4 {
		t.Fatalf("same station queued %d targets, want the same 4", len(got))
	}
}

func TestForesightJobsWarmsFinishingProduct(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	ch := seedCharacter(t, q, user.ID, 90000100, "Pilot 0")
	now := time.Now().UTC().Truncate(time.Second)
	end := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }

	bundle := &charSnaps{ch: ch, jobsKnown: true, jobs: []esi.IndustryJob{
		{JobID: 7, Status: "active", ActivityID: 1, ProductTypeID: 34, EndDate: end(5 * time.Minute)},
		{JobID: 8, Status: "active", ActivityID: 1, ProductTypeID: 35, EndDate: end(2 * time.Hour)},
		{JobID: 9, Status: "active", ActivityID: 5, ProductTypeID: 36, EndDate: end(5 * time.Minute)},
		{JobID: 10, Status: "delivered", ActivityID: 1, ProductTypeID: 37, EndDate: end(-time.Hour)},
	}}
	app.foreseeJobs(ctx, user.ID, bundle, now)
	got := foresightPendingTargets(t, q, now)
	if len(got) != 2 {
		t.Fatalf("%d targets, want book+history for the one finishing job's product", len(got))
	}
	for _, tg := range got {
		if tg.SubjectID != 34 || tg.RegionID != defaultMarketRegion || tg.EventKind != eventJob {
			t.Fatalf("unexpected target %+v", tg)
		}
	}
	if found := targetKinds(got); !found[targetBook] || !found[targetHistory] {
		t.Fatalf("target kinds %v, want book and history", found)
	}
}

func TestForesightQueueWarmsSkills(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	ch := seedCharacter(t, q, user.ID, 90000100, "Pilot 0")
	now := time.Now().UTC().Truncate(time.Second)

	// Ran out an hour ago: the skills pages are next.
	bundle := &charSnaps{ch: ch, queueKnown: true, queue: esi.Skillqueue{
		{SkillID: 1, FinishDate: now.Add(-time.Hour).Format(time.RFC3339)},
	}}
	app.foreseeQueue(ctx, user.ID, bundle, now)
	got := foresightPendingTargets(t, q, now)
	if len(got) != 3 {
		t.Fatalf("%d targets, want the 3 skill snapshots", len(got))
	}
	wantDetails := map[string]bool{esi.SnapSkills: true, esi.SnapSkillqueue: true, esi.SnapAttributes: true}
	for _, tg := range got {
		if tg.TargetKind != targetSnapshot || tg.SubjectID != ch.CharacterID || tg.ViewKey != foresightSkillsView(ch.CharacterID) {
			t.Fatalf("unexpected target %+v", tg)
		}
		delete(wantDetails, tg.Detail)
	}
	if len(wantDetails) != 0 {
		t.Fatalf("missing snapshot targets for %v", wantDetails)
	}

	// Still training: nothing to warm.
	future := &charSnaps{ch: ch, queueKnown: true, queue: esi.Skillqueue{
		{SkillID: 1, FinishDate: now.Add(time.Hour).Format(time.RFC3339)},
	}}
	app.foreseeQueue(ctx, user.ID, future, now)
	if got := foresightPendingTargets(t, q, now); len(got) != 3 {
		t.Fatalf("training queue queued %d targets, want the same 3", len(got))
	}
}

func TestForesightUndercutWarmsBook(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, 90000100, "Pilot 0")
	now := time.Now().UTC().Truncate(time.Second)
	health := func(orderID, typeID int64, status string) {
		t.Helper()
		if err := q.UpsertOrderHealth(ctx, db.UpsertOrderHealthParams{
			CharacterID: 90000100, OrderID: orderID, TypeID: typeID, RegionID: 10000002,
			LocationID: 60003760, MyPrice: 5, StationBest: 4, RegionBest: 4,
			Status: status, ComputedAt: now,
		}); err != nil {
			t.Fatalf("seed order health: %v", err)
		}
	}
	health(1, 34, "undercut_station")
	health(2, 35, "ok")

	app.foreseeUndercut(ctx, user.ID, now)
	got := foresightPendingTargets(t, q, now)
	if len(got) != 2 {
		t.Fatalf("%d targets, want book+history for the undercut order only", len(got))
	}
	for _, tg := range got {
		if tg.SubjectID != 34 || tg.RegionID != 10000002 || tg.EventKind != eventUndercut {
			t.Fatalf("unexpected target %+v", tg)
		}
	}
}

func TestForesightWarWarmsAggressor(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, 90000100, "Pilot 0")
	now := time.Now().UTC().Truncate(time.Second)
	if err := q.UpsertCharacterCorporation(ctx, db.UpsertCharacterCorporationParams{
		CharacterID: 90000100, CorporationID: 98000001, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed mapping: %v", err)
	}
	war := func(id int64, declared, finished string) {
		t.Helper()
		if err := q.UpsertWarDetail(ctx, db.UpsertWarDetailParams{
			WarID: id, FetchedAt: now,
			Payload: `{"id":` + strconv.FormatInt(id, 10) + `,"declared":` + strconv.Quote(declared) +
				`,"aggressor":{"corporation_id":99000001,"alliance_id":99000002},` +
				`"defender":{"corporation_id":98000001},"finished":` + strconv.Quote(finished) + `}`,
		}); err != nil {
			t.Fatalf("seed war %d: %v", id, err)
		}
	}
	war(9, now.Add(-time.Hour).Format(time.RFC3339), "")
	war(10, now.Add(-time.Hour).Format(time.RFC3339), now.Format(time.RFC3339))

	app.foreseeWars(ctx, now)
	got := foresightPendingTargets(t, q, now)
	wantViews := map[string]bool{
		foresightOrgView(targetAlliance, 99000002):    true,
		foresightOrgView(targetCorporation, 99000001): true,
	}
	if len(got) != len(wantViews) {
		t.Fatalf("%d targets, want the live war's aggressor alliance and corp", len(got))
	}
	for _, tg := range got {
		if tg.UserID != user.ID || tg.EventKind != eventWar {
			t.Fatalf("unexpected target %+v", tg)
		}
		delete(wantViews, tg.ViewKey)
	}
	if len(wantViews) != 0 {
		t.Fatalf("missing aggressor targets for %v", wantViews)
	}
}

func TestForesightOpenedMarksHit(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	target := func(key, view string) {
		t.Helper()
		if _, err := q.AddForesightTarget(ctx, db.AddForesightTargetParams{
			UserID: user.ID, CharacterID: 90000100, EventKind: eventDock, EventKey: key,
			TargetKind: targetHistory, SubjectID: 34, RegionID: 10000002,
			ViewKey: view, CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour),
		}); err != nil {
			t.Fatalf("seed target: %v", err)
		}
	}
	target("dock|1", "market:34")
	target("dock|2", "market:35")
	app.foresight.live.Store(2)

	app.foresightOpened(ctx, user.ID, "market:34")
	live, err := q.CountForesightLive(ctx, now)
	if err != nil {
		t.Fatalf("count live: %v", err)
	}
	if live != 1 {
		t.Fatalf("live targets = %d, want 1 (only the opened page settles)", live)
	}
	if got := app.foresight.live.Load(); got != 1 {
		t.Fatalf("in-memory live = %d, want 1", got)
	}
}

func TestForesightOddsLearnsFromOpens(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	seed := func(key, view string, created, expires time.Time, open bool) {
		t.Helper()
		if _, err := q.AddForesightTarget(ctx, db.AddForesightTargetParams{
			UserID: user.ID, CharacterID: 90000100, EventKind: eventDock, EventKey: key,
			TargetKind: targetHistory, SubjectID: 34, RegionID: 10000002,
			ViewKey: view, CreatedAt: created, ExpiresAt: expires,
		}); err != nil {
			t.Fatalf("seed target: %v", err)
		}
		if open {
			if _, err := q.MarkForesightOpened(ctx, db.MarkForesightOpenedParams{
				UserID: user.ID, ViewKey: view, OpenedAt: timeSet(now),
			}); err != nil {
				t.Fatalf("mark opened: %v", err)
			}
		}
	}
	// Three ran their course unopened, one was opened: 1 of 4.
	seed("dock|1", "market:31", now.Add(-3*time.Hour), now.Add(-2*time.Hour), false)
	seed("dock|2", "market:32", now.Add(-3*time.Hour), now.Add(-2*time.Hour), false)
	seed("dock|3", "market:33", now.Add(-3*time.Hour), now.Add(-2*time.Hour), false)
	seed("dock|4", "market:34", now.Add(-3*time.Hour), now.Add(time.Hour), true)

	odds := app.foresightOdds(ctx, []int64{user.ID}, now)
	global := (1 + 0.30*foresightAssumedWeight) / (4 + foresightAssumedWeight)
	closeFloat(t, "chance", odds.chance(eventDock), global)
	closeFloat(t, "account chance", odds.chanceFor(user.ID, eventDock),
		(1+global*foresightAccountWeight)/(4+foresightAccountWeight))
	// No record: the assumed chance stands.
	closeFloat(t, "unseen kind", odds.chance(eventWar), 0.40)
}

func TestForesightScoreWorth(t *testing.T) {
	if got := foresightScore(0.5, 1.0, 1); got != 0.5 {
		t.Fatalf("score = %v, want 0.5", got)
	}
	if got := foresightScore(0.5, 1.0, 0); got != 0.5 {
		t.Fatalf("score with zero cost = %v, want cost floored to 1", got)
	}
	if got := foresightScore(0.5, 0, 4); got != 0 {
		t.Fatalf("score of fresh data = %v, want 0", got)
	}
	if got := staleByDay(time.Hour); got != 0 {
		t.Fatalf("staleByDay(1h) = %v, want 0", got)
	}
	if got := staleByDay(12 * time.Hour); got != 0.5 {
		t.Fatalf("staleByDay(12h) = %v, want 0.5", got)
	}
	if got := staleByDay(48 * time.Hour); got != 1 {
		t.Fatalf("staleByDay(48h) = %v, want 1", got)
	}
}

func TestForesightSpendSettlesFreshAtNoCost(t *testing.T) {
	transport := &countingTransport{}
	app, conn, q := buildCorpTestApp(t, transport)
	app.cfg.foresightFetches = 10
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if err := q.SetCorporationRecord(ctx, db.SetCorporationRecordParams{
		CorporationID: 98000001,
		Payload:       `{"corp":{"name":"Fresh Corp"},"alliance":{}}`,
		State:         orgStateReady, FetchedAt: timeSet(now),
	}); err != nil {
		t.Fatalf("seed corporation record: %v", err)
	}
	if _, err := q.AddForesightTarget(ctx, db.AddForesightTargetParams{
		UserID: user.ID, CharacterID: 90000100, EventKind: eventWar, EventKey: "war|9",
		TargetKind: targetCorporation, SubjectID: 98000001,
		ViewKey:   foresightOrgView(targetCorporation, 98000001),
		CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("seed target: %v", err)
	}

	stored, limited := app.foresightSpend(ctx, &fetchBudget{left: 120}, now)
	if limited || stored != 0 {
		t.Fatalf("spend = (%d, %v), want (0, false): a fresh record settles free", stored, limited)
	}
	if got := transport.calls.Load(); got != 0 {
		t.Fatalf("spend made %d outbound calls, want 0", got)
	}
	var warmed bool
	if err := conn.QueryRowContext(ctx,
		`SELECT warmed_at IS NOT NULL FROM foresight_targets WHERE view_key = $1`,
		foresightOrgView(targetCorporation, 98000001)).Scan(&warmed); err != nil || !warmed {
		t.Fatalf("target not settled as warmed: warmed=%v err=%v", warmed, err)
	}
}

func TestForesightOffNoops(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	app.cfg.foresightOff = true
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	ch := seedCharacter(t, q, user.ID, 90000100, "Pilot 0")
	now := time.Now().UTC().Truncate(time.Second)
	seedSnapshot(t, q, ch.CharacterID, esi.SnapLocation, esi.Location{SolarSystemID: 30000142, StationID: 60003760})
	app.foreseeDock(ctx, user.ID, ch, now)
	app.foreseeWars(ctx, now)
	if got := foresightPendingTargets(t, q, now); len(got) != 0 {
		t.Fatalf("foresight off queued %d targets, want 0", len(got))
	}
	if stored, limited := app.foresightSpend(ctx, &fetchBudget{left: 120}, now); stored != 0 || limited {
		t.Fatalf("foresight off spend = (%d, %v), want (0, false)", stored, limited)
	}
}
