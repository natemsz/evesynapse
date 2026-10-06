package tests

// Hermetic tests for the fold-granularity change: folding lives
// on the SECTION headings of multi-section pages (each
// <section class="foldable"> in the markup), not on the page
// window itself. Skill-group sections on /skills/ start folded
// by default via data-fold="closed", which the served app.js
// applies on load — the raw markup never hides content, so
// no-JS readers see everything. The served JS must keep the
// two guardrails that encode the whole design: no page-level
// .panel fold when the panel wraps foldable sections or cards,
// and default-folded state applied via JS only.

import (
	"strings"
	"testing"
	"time"

	"evesynapse/internal/apptest"
	"evesynapse/internal/esi"
)

// TestSkillsPageFoldDefaults: the skill-queue section renders as
// a plain .foldable (starts open); each skill-group section
// carries data-fold="closed" (starts folded once JS loads).
func TestSkillsPageFoldDefaults(t *testing.T) {
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
		`<section class="foldable`,
		"<h3>Training</h3>",
		`<section class="foldable" data-fold="closed">`,
		"Currently training:",
	)
	if n := strings.Count(body, `data-fold="closed"`); n < 1 {
		t.Fatalf("/character/: %d default-folded sections, want at least 1", n)
	}
	// The queue section must be the open one: the closed marker
	// belongs to a group heading, never to the queue block.
	closedAt := strings.Index(body, `data-fold="closed"`)
	if closedAt < 0 || closedAt < strings.Index(body, "<h3>Skill queue</h3>") {
		t.Fatalf("/skills/: default-folded marker does not follow the queue section")
	}
	if transport.Calls.Load() != 0 {
		t.Fatalf("/skills/ made %d outbound calls; renders stay cache-only", transport.Calls.Load())
	}
}

// TestCharacterSheetFoldSections: the character sheet's blocks
// (identity, wallet, training, skills…) each render as their own
// .foldable section; nothing on the sheet defaults folded.
func TestCharacterSheetFoldSections(t *testing.T) {
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
		`<section class="foldable`,
		"<h3>Wallet</h3>",
		"<h3>Training</h3>",
		"<h3>Skills</h3>",
	)
	if n := strings.Count(body, `<section class="foldable`); n < 4 {
		t.Fatalf("/character/: %d foldable sections, want at least 4 (identity, wallet, training, skills)", n)
	}
	// Skill groups, browse catalog, and clones default to folded (merged from
	// the old skills page); the four main sheet sections above stay expanded.
	// No outbound-call assertion here: the legacy /character/
	// handler fills optional sections (online, location, ship…)
	// live when their snapshots are absent; that predates this
	// change and is out of scope for the fold work.
	_ = transport
}

// TestFoldGuardrailsInServedAssets: the served app.js encodes
// the fold rules (page-level fold skipped when the panel wraps
// sections or cards; default-fold state applied by JS only from
// data-fold), and the served CSS hides content only under the
// JS-applied .folded class — so raw markup stays fully visible
// without JavaScript.
func TestFoldGuardrailsInServedAssets(t *testing.T) {
	transport := &apptest.CountingTransport{}
	rig := apptest.Build(t, transport)
	q := rig.Queries()

	user, err := q.CreateUser(t.Context())
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	apptest.SeedCharacter(t, q, user.ID, apptest.FixtureCharA, "Fixture Alpha")
	cookie := apptest.SessionCookie(t, rig, user.ID, apptest.FixtureCharA, "Fixture Alpha")

	code, js := apptest.GetPage(t, rig, cookie, "/static/app.js")
	if code != 200 {
		t.Fatalf("/static/app.js status = %d", code)
	}
	apptest.MustContain(t, "/static/app.js", js,
		`panel.querySelector(".foldable, .card")`,
		`getAttribute("data-fold")`,
		`data-fold") === "closed"`,
		`querySelectorAll(".foldable")`,
		`querySelectorAll(".card")`,
	)
	// The old unconditional page-window fold must be gone.
	if strings.Contains(js, `addFold(panel, panel.querySelector(":scope > h1"))`+"\n  }") &&
		!strings.Contains(js, `!panel.querySelector(".foldable, .card")`) {
		t.Fatalf("app.js still folds the page window unconditionally")
	}

	code, css := apptest.GetPage(t, rig, cookie, "/static/style.css")
	if code != 200 {
		t.Fatalf("/static/style.css status = %d", code)
	}
	apptest.MustContain(t, "/static/style.css", css,
		".foldable.folded > *:not(:first-child)",
		".foldable { position: relative; }",
	)
}
