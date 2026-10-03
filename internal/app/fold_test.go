package app

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

	"evesynapse/internal/esi"
)

// TestSkillsPageFoldDefaults: the skill-queue section renders as
// a plain .foldable (starts open); each skill-group section
// carries data-fold="closed" (starts folded once JS loads).
func TestSkillsPageFoldDefaults(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := t.Context()
	now := time.Now()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	seedSnapshot(t, q, fixtureCharA, esi.SnapSkills, esi.Skills{
		TotalSP: 1234567,
		Skills: []esi.Skill{
			{SkillID: 3300, SkillpointsInSkill: 256000, TrainedSkillLevel: 5, ActiveSkillLevel: 5},
			{SkillID: 3301, SkillpointsInSkill: 8000, TrainedSkillLevel: 4, ActiveSkillLevel: 4},
		},
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapSkillqueue, esi.Skillqueue{
		{SkillID: 3301, QueuePosition: 0, FinishedLevel: 5, FinishDate: rfc(now.Add(6 * time.Hour))},
	})

	code, body := getPage(t, app, sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha"), "/skills/")
	if code != 200 {
		t.Fatalf("/skills/ status = %d", code)
	}
	mustContain(t, "/skills/", body,
		`<section class="foldable">`,
		"<h3>Skill queue</h3>",
		`<section class="foldable" data-fold="closed">`,
		"Currently training:",
	)
	if n := strings.Count(body, `data-fold="closed"`); n != 1 {
		t.Fatalf("/skills/: %d default-folded sections, want 1 (the single resolved group)", n)
	}
	// The queue section must be the open one: the closed marker
	// belongs to a group heading, never to the queue block.
	closedAt := strings.Index(body, `data-fold="closed"`)
	if closedAt < 0 || closedAt < strings.Index(body, "<h3>Skill queue</h3>") {
		t.Fatalf("/skills/: default-folded marker does not follow the queue section")
	}
	if transport.calls.Load() != 0 {
		t.Fatalf("/skills/ made %d outbound calls; renders stay cache-only", transport.calls.Load())
	}
}

// TestCharacterSheetFoldSections: the character sheet's blocks
// (identity, wallet, training, skills…) each render as their own
// .foldable section; nothing on the sheet defaults folded.
func TestCharacterSheetFoldSections(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := t.Context()
	now := time.Now()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	seedSnapshot(t, q, fixtureCharA, esi.SnapProfile, esi.Character{
		Name: "Fixture Alpha", CorporationID: fixtureCorpA,
		Birthday: "2009-12-24T00:00:00Z", SecurityStatus: 0.55,
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapWallet, 1234567.89)
	seedSnapshot(t, q, fixtureCharA, esi.SnapSkills, esi.Skills{
		TotalSP: 999999,
		Skills: []esi.Skill{
			{SkillID: 3300, SkillpointsInSkill: 256000, TrainedSkillLevel: 5, ActiveSkillLevel: 5},
		},
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapSkillqueue, esi.Skillqueue{
		{SkillID: 3300, QueuePosition: 0, FinishedLevel: 5, FinishDate: rfc(now.Add(12 * time.Hour))},
	})

	code, body := getPage(t, app, sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha"), "/character/")
	if code != 200 {
		t.Fatalf("/character/ status = %d", code)
	}
	mustContain(t, "/character/", body,
		`<section class="foldable">`,
		"<h3>Wallet</h3>",
		"<h3>Training</h3>",
		"<h3>Skills</h3>",
	)
	if n := strings.Count(body, `<section class="foldable">`); n < 4 {
		t.Fatalf("/character/: %d foldable sections, want at least 4 (identity, wallet, training, skills)", n)
	}
	if strings.Contains(body, `data-fold="closed"`) {
		t.Fatalf("/character/: nothing on the character sheet should default to folded")
	}
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
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)

	user, err := q.CreateUser(t.Context())
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")

	code, js := getPage(t, app, cookie, "/static/app.js")
	if code != 200 {
		t.Fatalf("/static/app.js status = %d", code)
	}
	mustContain(t, "/static/app.js", js,
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

	code, css := getPage(t, app, cookie, "/static/style.css")
	if code != 200 {
		t.Fatalf("/static/style.css status = %d", code)
	}
	mustContain(t, "/static/style.css", css,
		".foldable.folded > *:not(:first-child)",
		".foldable { position: relative; }",
	)
}
