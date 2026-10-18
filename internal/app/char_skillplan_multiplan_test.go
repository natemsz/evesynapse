package app

import (
	"context"
	"strconv"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// TestSkillPlansPageAcrossPlans: one render of the plans page
// computes the selected plan's detail, the picker's summaries
// for the character's other plans, and the editor search — the
// output each plan produces is the output it produces on its
// own.
func TestSkillPlansPageAcrossPlans(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Plan Tester")
	seedSkillGraphRows(t, app)

	seedSnapshot(t, q, fixtureCharA, esi.SnapSkills, esi.Skills{
		TotalSP: 1414,
		Skills:  []esi.Skill{{SkillID: 1001, SkillpointsInSkill: 1414, ActiveSkillLevel: 2, TrainedSkillLevel: 2}},
	})
	seedSnapshot(t, q, fixtureCharA, esi.SnapSkillqueue, esi.Skillqueue{})
	seedSnapshot(t, q, fixtureCharA, esi.SnapAttributes, esi.Attributes{
		Charisma: 17, Intelligence: 30, Memory: 20, Perception: 25, Willpower: 20,
	})
	// The page shell reads these too; seed empty so nothing falls
	// through to live ESI.
	seedSnapshot(t, q, fixtureCharA, esi.SnapClones, esi.Clones{})
	seedSnapshot(t, q, fixtureCharA, esi.SnapImplants, esi.Implants{})
	seedSnapshot(t, q, fixtureCharA, esi.SnapOnline, esi.Online{})
	seedSnapshot(t, q, fixtureCharA, esi.SnapLocation, esi.Location{})
	seedSnapshot(t, q, fixtureCharA, esi.SnapShip, esi.Ship{})
	seedSnapshot(t, q, fixtureCharA, esi.SnapFatigue, esi.Fatigue{})
	seedSnapshot(t, q, fixtureCharA, esi.SnapProfile, esi.Character{})
	seedSnapshot(t, q, fixtureCharA, esi.SnapWallet, 0.0)
	seedSnapshot(t, q, fixtureCharA, esi.SnapCorpInfo, esi.Corporation{})

	mkPlan := func(name string, items map[int64]int64) db.SkillPlan {
		t.Helper()
		plan, err := q.CreateSkillPlan(ctx, db.CreateSkillPlanParams{
			UserID: user.ID, CharacterID: fixtureCharA, Name: name, CreatedAt: time.Now().UTC(),
		})
		if err != nil {
			t.Fatalf("create plan %s: %v", name, err)
		}
		pos := int64(0)
		for skillID, level := range items {
			pos++
			if err := q.UpsertSkillPlanItem(ctx, db.UpsertSkillPlanItemParams{
				PlanID: plan.ID, SkillTypeID: skillID, TargetLevel: level, Position: pos,
			}); err != nil {
				t.Fatalf("add item to %s: %v", name, err)
			}
		}
		return plan
	}
	mkPlan("Plan One", map[int64]int64{1001: 5, 1003: 2})
	two := mkPlan("Plan Two", map[int64]int64{1002: 3})
	mkPlan("Plan Three", nil)

	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Plan Tester")

	_, body := getPage(t, app, cookie, "/skills/plans/?character=90000001")
	mustContain(t, "plans page", body, "Plan One", "Plan Two", "Plan Three")
	// The selected plan is the first one: its detail lists both
	// of its skills.
	mustContain(t, "selected plan detail", body, "Alpha Skill", "Gamma Skill")

	// Selecting the second plan computes its detail in the same
	// render as the other plans' summaries.
	_, body = getPage(t, app, cookie, "/skills/plans/?character=90000001&plan="+strconv.FormatInt(two.ID, 10))
	mustContain(t, "second plan detail", body, "Beta Skill", "Plan One", "Plan Three")

	// The editor search answers on the same page.
	_, body = getPage(t, app, cookie, "/skills/plans/?character=90000001&q=beta")
	mustContain(t, "editor search", body, "Beta Skill")

	if n := transport.calls.Load(); n != 0 {
		t.Fatalf("the plans page made %d outbound call(s)", n)
	}
}
