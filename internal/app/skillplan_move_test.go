package app

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
)

var planRowSkill = regexp.MustCompile(`<tr[^>]*data-skill="(\d+)"`)

// planRowOrder is the skill IDs of the plan table's rows, top to bottom.
func planRowOrder(body string) []string {
	var out []string
	for _, m := range planRowSkill.FindAllStringSubmatch(body, -1) {
		out = append(out, m[1])
	}
	return out
}

func (f *planFixture) newPlan(name string) string {
	f.t.Helper()
	f.follow("/skills/plans/create", url.Values{"character": {"90000001"}, "name": {name}})
	plans := f.planIDs(fixtureCharA)
	if len(plans) == 0 {
		f.t.Fatalf("no plan after creating %q", name)
	}
	return fmt.Sprint(plans[len(plans)-1].ID)
}

var inPlace = map[string]string{"X-Requested-With": "XMLHttpRequest"}

// TestPlanMovesFollowPrerequisites: a skill moves one place at a time in
// the order the plan trains, cannot pass a skill it needs (or that needs
// it) and says why, and the table that comes back for an in-place edit is
// the new order. Alpha IV is both a plan skill and Beta's prerequisite.
func TestPlanMovesFollowPrerequisites(t *testing.T) {
	f := newPlanFixture(t)
	const char = "90000001"
	plan := f.newPlan("Road to Beta")
	for _, add := range [][2]string{{"1002", "1"}, {"1003", "2"}, {"1001", "4"}} {
		f.follow("/skills/plans/item-add", url.Values{"character": {char}, "plan": {plan}, "skill": {add[0]}, "level": {add[1]}})
	}
	pageURL := "/skills/plans?character=" + char + "&plan=" + plan
	move := func(skill, dir string, header map[string]string) (int, string) {
		rec := f.do(http.MethodPost, "/skills/plans/item-move",
			url.Values{"character": {char}, "plan": {plan}, "skill": {skill}, "dir": {dir}}, header)
		return rec.Code, rec.Body.String()
	}
	orderOnPage := func() []string {
		t.Helper()
		return planRowOrder(f.do(http.MethodGet, pageURL, nil, nil).Body.String())
	}
	want := func(what string, got, want []string) {
		t.Helper()
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: order = %v, want %v", what, got, want)
		}
	}

	// Alpha leads (Beta needs it), then Beta (position 1) and Gamma.
	want("start", orderOnPage(), []string{"1001", "1002", "1003"})

	// Refused moves: 409, the reason, and the same table.
	for _, c := range []struct{ skill, dir, why string }{
		{"1002", "up", "Alpha Skill has to be trained before it"},
		{"1001", "down", "Beta Skill needs it first"},
		{"1001", "up", "it is already first"},
		{"1003", "down", "it is already last"},
	} {
		code, body := move(c.skill, c.dir, inPlace)
		if code != http.StatusConflict {
			t.Errorf("move %s %s = %d, want 409", c.skill, c.dir, code)
		}
		mustContain(t, "refused move", body, c.why, "data-plan-body")
		if strings.Contains(body, "<html") {
			t.Errorf("move %s %s answered with a whole page, want just the table", c.skill, c.dir)
		}
		want("after refused "+c.skill+" "+c.dir, planRowOrder(body), []string{"1001", "1002", "1003"})
	}
	want("after refusals", orderOnPage(), []string{"1001", "1002", "1003"})

	// Beta later: past Gamma, which it has nothing to do with.
	code, body := move("1002", "down", inPlace)
	if code != http.StatusOK {
		t.Fatalf("move Beta down = %d, want 200", code)
	}
	want("Beta down (table returned)", planRowOrder(body), []string{"1001", "1003", "1002"})
	want("Beta down (page)", orderOnPage(), []string{"1001", "1003", "1002"})

	// Gamma earlier: ahead of Alpha, which does not matter to it.
	code, body = move("1003", "up", inPlace)
	if code != http.StatusOK {
		t.Fatalf("move Gamma up = %d, want 200", code)
	}
	want("Gamma up (table returned)", planRowOrder(body), []string{"1003", "1001", "1002"})
	want("Gamma up (page)", orderOnPage(), []string{"1003", "1001", "1002"})

	// Positions are one per skill, so a later move never ties.
	plans := f.planIDs(fixtureCharA)
	items, _ := f.q.ListSkillPlanItems(context.Background(), plans[0].ID)
	seen := map[int64]bool{}
	for _, it := range items {
		if seen[it.Position] {
			t.Errorf("two plan skills share position %d: %+v", it.Position, items)
		}
		seen[it.Position] = true
	}

	// Without JavaScript the refusal comes back as a flash on the page.
	code, page, landed := f.follow("/skills/plans/item-move",
		url.Values{"character": {char}, "plan": {plan}, "skill": {"1002"}, "dir": {"up"}})
	if code != http.StatusOK || !strings.HasPrefix(landed, "/skills/plans") {
		t.Fatalf("plain refused move landed on %s with %d", landed, code)
	}
	mustContain(t, landed, page, "Beta Skill can&#39;t move earlier: Alpha Skill has to be trained before it.")
	if again := f.do(http.MethodGet, landed, nil, nil).Body.String(); strings.Contains(again, "has to be trained before it.") {
		t.Error("the refusal was shown a second time")
	}

	// The buttons that cannot do anything say why instead of vanishing.
	mustContain(t, pageURL, f.do(http.MethodGet, pageURL, nil, nil).Body.String(),
		`aria-disabled="true"`, "Cannot move earlier: Alpha Skill has to be trained before it")

	// Removing in place returns the table without the skill.
	rec := f.do(http.MethodPost, "/skills/plans/item-remove",
		url.Values{"character": {char}, "plan": {plan}, "skill": {"1003"}}, inPlace)
	if rec.Code != http.StatusOK {
		t.Fatalf("remove Gamma = %d, want 200", rec.Code)
	}
	want("after remove", planRowOrder(rec.Body.String()), []string{"1001", "1002"})
}

// TestPlanKeepsFinishedSkillsRemovable: a plan skill the character has
// already trained to its target has nothing to time, so it sits apart
// from the training order (a move never counts it) and can still be
// removed.
func TestPlanKeepsFinishedSkillsRemovable(t *testing.T) {
	f := newPlanFixture(t)
	const char = "90000001"
	plan := f.newPlan("Done and not done")
	plans := f.planIDs(fixtureCharA)
	ctx := context.Background()
	// Alpha II is trained already; add it behind the handler's back, as an
	// older plan (or a later sync) can leave it.
	for _, it := range []db.UpsertSkillPlanItemParams{
		{PlanID: plans[0].ID, SkillTypeID: 1001, TargetLevel: 2, Position: 1},
		{PlanID: plans[0].ID, SkillTypeID: 1003, TargetLevel: 1, Position: 2},
	} {
		if err := f.q.UpsertSkillPlanItem(ctx, it); err != nil {
			t.Fatal(err)
		}
	}
	pageURL := "/skills/plans?character=" + char + "&plan=" + plan
	body := f.do(http.MethodGet, pageURL, nil, nil).Body.String()
	mustContain(t, "plan with a finished skill", body,
		`class="plan-covered" data-skill="1001"`, "nothing left to train", `data-skill="1003"`)
	if got := planRowOrder(body); !reflect.DeepEqual(got, []string{"1003", "1001"}) {
		t.Errorf("rows = %v, want Gamma to train then Alpha listed as done", got)
	}

	rec := f.do(http.MethodPost, "/skills/plans/item-move",
		url.Values{"character": {char}, "plan": {plan}, "skill": {"1003"}, "dir": {"up"}}, inPlace)
	if rec.Code != http.StatusConflict {
		t.Errorf("moving the only skill left to train = %d, want 409 (it is already first)", rec.Code)
	}
	rec = f.do(http.MethodPost, "/skills/plans/item-move",
		url.Values{"character": {char}, "plan": {plan}, "skill": {"1001"}, "dir": {"down"}}, inPlace)
	if rec.Code != http.StatusConflict {
		t.Errorf("moving a finished skill = %d, want 409", rec.Code)
	}

	rec = f.do(http.MethodPost, "/skills/plans/item-remove",
		url.Values{"character": {char}, "plan": {plan}, "skill": {"1001"}}, inPlace)
	if rec.Code != http.StatusOK {
		t.Fatalf("remove finished skill = %d, want 200", rec.Code)
	}
	if got := planRowOrder(rec.Body.String()); !reflect.DeepEqual(got, []string{"1003"}) {
		t.Errorf("rows after removing the finished skill = %v, want [1003]", got)
	}
}

// TestPlanEditsAreOwnersOnly: another user's plan answers an in-place
// edit with a 404 and is left as it was.
func TestPlanEditsAreOwnersOnly(t *testing.T) {
	f := newPlanFixture(t)
	ctx := context.Background()
	other, err := f.q.CreateUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seedCharacter(t, f.q, other.ID, fixtureCharB, "Someone Else")
	theirs, err := f.q.CreateSkillPlan(ctx, db.CreateSkillPlanParams{
		UserID: other.ID, CharacterID: fixtureCharB, Name: "Theirs", CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.q.UpsertSkillPlanItem(ctx, db.UpsertSkillPlanItemParams{PlanID: theirs.ID, SkillTypeID: 1003, TargetLevel: 1, Position: 1}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/skills/plans/item-move", "/skills/plans/item-remove"} {
		for _, char := range []string{"90000001", fmt.Sprint(fixtureCharB)} {
			rec := f.do(http.MethodPost, path,
				url.Values{"character": {char}, "plan": {fmt.Sprint(theirs.ID)}, "skill": {"1003"}, "dir": {"down"}}, inPlace)
			if rec.Code != http.StatusNotFound {
				t.Errorf("POST %s on someone else's plan (character %s) = %d, want 404", path, char, rec.Code)
			}
		}
	}
	items, _ := f.q.ListSkillPlanItems(ctx, theirs.ID)
	if len(items) != 1 {
		t.Errorf("their plan holds %d skills after the attempts, want 1", len(items))
	}
}

// TestAddingAPlannedSkillNeverLowersIt: a plan holds one target per
// skill. Adding a skill that is already planned to that level or higher
// says so and leaves the target alone (it used to be overwritten with the
// lower one).
func TestAddingAPlannedSkillNeverLowersIt(t *testing.T) {
	f := newPlanFixture(t)
	const char = "90000001"
	plan := f.newPlan("Gamma")
	f.follow("/skills/plans/item-add", url.Values{"character": {char}, "plan": {plan}, "skill": {"1003"}, "level": {"4"}})
	code, body, landed := f.follow("/skills/plans/item-add", url.Values{"character": {char}, "plan": {plan}, "skill": {"1003"}, "level": {"2"}})
	if code != http.StatusOK {
		t.Fatalf("add landed on %s with %d", landed, code)
	}
	mustContain(t, landed, body, "Gamma Skill is already in this plan to IV.")
	items, _ := f.q.ListSkillPlanItems(context.Background(), f.planIDs(fixtureCharA)[0].ID)
	if len(items) != 1 || items[0].TargetLevel != 4 {
		t.Fatalf("plan holds %+v, want Gamma at 4", items)
	}
	// Raising it is still just an add.
	f.follow("/skills/plans/item-add", url.Values{"character": {char}, "plan": {plan}, "skill": {"1003"}, "level": {"5"}})
	items, _ = f.q.ListSkillPlanItems(context.Background(), f.planIDs(fixtureCharA)[0].ID)
	if len(items) != 1 || items[0].TargetLevel != 5 {
		t.Fatalf("plan holds %+v, want Gamma raised to 5", items)
	}
}
