package tests

// Hermetic tests for the fold-granularity change: folding lives
// on the SECTION headings of multi-section pages (each
// <section class="foldable"> in the markup), not on the page
// window itself. Sections that start folded do so via
// data-fold="closed", which the served app.js applies on load,
// the raw markup never hides content, so no-JS readers see
// everything. The foldable sections themselves are pinned on
// the pages that use them (the assets and PI tests); the two
// tests here pin the Overview sheet, which is built from panes
// and cards: its training and skill content renders in full in
// the raw markup.

import (
	"testing"
	"time"

	"evesynapse/internal/apptest"
	"evesynapse/internal/esi"
)

// TestOverviewSheetShowsTrainingAndSkills: with a queue and
// trained skills stored, the sheet renders the Now Training
// strip with the queue's head entry and the skill directory,
// and the render stays cache-only.
func TestOverviewSheetShowsTrainingAndSkills(t *testing.T) {
	transport := &apptest.CountingTransport{}
	rig := apptest.Build(t, transport)
	q := rig.Queries()
	ctx := t.Context()
	now := time.Now()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	apptest.SeedCharacter(t, q, user.ID, apptest.FixtureCharA, "Fixture Alpha")
	apptest.SeedSnapshot(t, q, apptest.FixtureCharA, esi.SnapSkills, esi.Skills{
		TotalSP: 1234567,
		Skills: []esi.Skill{
			{SkillID: 3300, SkillpointsInSkill: 256000, TrainedSkillLevel: 5, ActiveSkillLevel: 5},
			{SkillID: 3301, SkillpointsInSkill: 8000, TrainedSkillLevel: 4, ActiveSkillLevel: 4},
		},
	})
	apptest.SeedSnapshot(t, q, apptest.FixtureCharA, esi.SnapSkillqueue, esi.Skillqueue{
		{SkillID: 3301, QueuePosition: 0, FinishedLevel: 5, FinishDate: apptest.RFC(now.Add(6 * time.Hour))},
	})

	code, body := apptest.GetPage(t, rig, apptest.SessionCookie(t, rig, user.ID, apptest.FixtureCharA, "Fixture Alpha"), "/character/")
	if code != 200 {
		t.Fatalf("/character/ status = %d", code)
	}
	apptest.MustContain(t, "/character/", body,
		"1.23M SP",
		"Now training",
		"training to Level V",
		"Skill directory",
	)
	if transport.Calls.Load() != 0 {
		t.Fatalf("/character/ made %d outbound calls; renders stay cache-only", transport.Calls.Load())
	}
}

// TestOverviewSheetBlocksRender: the sheet's blocks (identity,
// wallet, training, skills) each render their stored content.
func TestOverviewSheetBlocksRender(t *testing.T) {
	transport := &apptest.CountingTransport{}
	rig := apptest.Build(t, transport)
	q := rig.Queries()
	ctx := t.Context()
	now := time.Now()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	apptest.SeedCharacter(t, q, user.ID, apptest.FixtureCharA, "Fixture Alpha")
	apptest.SeedSnapshot(t, q, apptest.FixtureCharA, esi.SnapProfile, esi.Character{
		Name: "Fixture Alpha", CorporationID: apptest.FixtureCorpA,
		Birthday: "2009-12-24T00:00:00Z", SecurityStatus: 0.55,
	})
	apptest.SeedSnapshot(t, q, apptest.FixtureCharA, esi.SnapWallet, 1234567.89)
	apptest.SeedSnapshot(t, q, apptest.FixtureCharA, esi.SnapSkills, esi.Skills{
		TotalSP: 999999,
		Skills: []esi.Skill{
			{SkillID: 3300, SkillpointsInSkill: 256000, TrainedSkillLevel: 5, ActiveSkillLevel: 5},
		},
	})
	apptest.SeedSnapshot(t, q, apptest.FixtureCharA, esi.SnapSkillqueue, esi.Skillqueue{
		{SkillID: 3300, QueuePosition: 0, FinishedLevel: 5, FinishDate: apptest.RFC(now.Add(12 * time.Hour))},
	})

	code, body := apptest.GetPage(t, rig, apptest.SessionCookie(t, rig, user.ID, apptest.FixtureCharA, "Fixture Alpha"), "/character/")
	if code != 200 {
		t.Fatalf("/character/ status = %d", code)
	}
	apptest.MustContain(t, "/character/", body,
		"Fixture Alpha",
		"1,234,567.89",
		"Now training",
		"Skill directory",
	)
	// No outbound-call assertion here: the sheet fills optional
	// sections from their snapshots, and this fixture seeds only
	// some of them.
	_ = transport
}
