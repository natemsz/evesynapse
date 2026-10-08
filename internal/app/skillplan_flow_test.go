package app

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// planFixture is a signed-in character with the small skill universe of
// seedSkillGraphRows: Alpha (rank 1, trained to II), Beta (rank 2,
// needs Alpha III), Gamma (rank 1, untrained).
type planFixture struct {
	t      *testing.T
	app    *Application
	q      *db.Queries
	cookie *http.Cookie
	userID int64
	calls  *countingTransport
}

func newPlanFixture(t *testing.T) *planFixture {
	t.Helper()
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
	return &planFixture{
		t: t, app: app, q: q, userID: user.ID, calls: transport,
		cookie: sessionCookie(t, app, user.ID, fixtureCharA, "Plan Tester"),
	}
}

// do sends one request as the signed-in user.
func (f *planFixture) do(method, path string, form url.Values, header map[string]string) *httptest.ResponseRecorder {
	f.t.Helper()
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	req.AddCookie(f.cookie)
	rec := httptest.NewRecorder()
	f.app.Handler().ServeHTTP(rec, req)
	return rec
}

// follow posts a form the way a browser does and goes where it is
// redirected: it returns the status and body of the page it lands on,
// and the path it landed on.
func (f *planFixture) follow(path string, form url.Values) (int, string, string) {
	f.t.Helper()
	rec := f.do(http.MethodPost, path, form, nil)
	if rec.Code != http.StatusSeeOther {
		return rec.Code, rec.Body.String(), path
	}
	loc := rec.Header().Get("Location")
	landed := f.do(http.MethodGet, loc, nil, nil)
	return landed.Code, landed.Body.String(), loc
}

func (f *planFixture) planIDs(characterID int64) []db.SkillPlan {
	f.t.Helper()
	plans, err := f.q.ListSkillPlans(context.Background(), db.ListSkillPlansParams{UserID: f.userID, CharacterID: characterID})
	if err != nil {
		f.t.Fatalf("list plans: %v", err)
	}
	return plans
}

// TestSkillPlanActionsLandOnARealPage: every plan action does its work
// and then redirects; the redirect must go to a page that exists. They
// used to go to /skills/plans/ (with a slash) while the route is
// /skills/plans, so creating a plan, moving a skill, deleting a plan or
// adding the Magic 14 template all ended on a 404 after succeeding.
func TestSkillPlanActionsLandOnARealPage(t *testing.T) {
	f := newPlanFixture(t)
	const char = "90000001"

	expect := func(what string, code int, landed string) {
		t.Helper()
		if code != http.StatusOK {
			t.Fatalf("%s: landed on %s with status %d, want 200", what, landed, code)
		}
		if !strings.HasPrefix(landed, "/skills/plans") {
			t.Fatalf("%s: landed on %q, want the planner", what, landed)
		}
	}

	code, _, landed := f.follow("/skills/plans/create", url.Values{"character": {char}, "name": {"Road to Beta"}})
	expect("create", code, landed)
	plans := f.planIDs(fixtureCharA)
	if len(plans) != 1 {
		t.Fatalf("%d plans after create, want 1", len(plans))
	}
	plan := fmt.Sprint(plans[0].ID)

	code, _, landed = f.follow("/skills/plans/item-add", url.Values{"character": {char}, "plan": {plan}, "skill": {"1002"}, "level": {"1"}})
	expect("item-add Beta", code, landed)
	code, _, landed = f.follow("/skills/plans/item-add", url.Values{"character": {char}, "plan": {plan}, "skill": {"1003"}, "level": {"2"}})
	expect("item-add Gamma", code, landed)

	code, _, landed = f.follow("/skills/plans/item-move", url.Values{"character": {char}, "plan": {plan}, "skill": {"1003"}, "dir": {"up"}})
	expect("item-move", code, landed)
	code, _, landed = f.follow("/skills/plans/item-remove", url.Values{"character": {char}, "plan": {plan}, "skill": {"1003"}})
	expect("item-remove", code, landed)

	// Magic 14: this fixture has none of its skills, so it creates nothing,
	// but it must still land on the planner.
	code, _, landed = f.follow("/skills/plans/from-template", url.Values{"character": {char}, "template": {"magic14"}})
	expect("from-template", code, landed)

	code, _, landed = f.follow("/skills/plans/delete", url.Values{"character": {char}, "plan": {plan}})
	expect("delete", code, landed)
	if n := len(f.planIDs(fixtureCharA)); n != 0 {
		t.Errorf("%d plans after delete, want 0", n)
	}

	// Old links and bookmarks with the trailing slash work too.
	if rec := f.do(http.MethodGet, "/skills/plans/?character="+char, nil, nil); rec.Code != http.StatusOK {
		t.Errorf("GET /skills/plans/ = %d, want 200", rec.Code)
	}
	if got := f.calls.calls.Load(); got != 0 {
		t.Errorf("outbound calls = %d, want 0", got)
	}
}

// seedMagicSkills adds three of the Magic 14 skills to the fixture's SDE
// (rank 1, group 300) and trains the character to V in the first two:
// CPU Management and Power Grid Management are done, Mechanics is not.
func (f *planFixture) seedMagicSkills() {
	f.t.Helper()
	ctx := context.Background()
	for _, stmt := range []string{
		`INSERT INTO sde_types (type_id, name, group_id, market_group_id, published) VALUES
			(3301, 'CPU Management', 300, 0, 1), (3302, 'Power Grid Management', 300, 0, 1), (3303, 'Mechanics', 300, 0, 1)`,
		`INSERT INTO sde_skill_meta (type_id, rank, primary_attr, secondary_attr) VALUES
			(3301, 1, 165, 166), (3302, 1, 165, 166), (3303, 1, 165, 166)`,
	} {
		if _, err := f.app.db.ExecContext(ctx, stmt); err != nil {
			f.t.Fatalf("seed magic skills: %v", err)
		}
	}
	// Level V of a rank-1 skill is 250 * 2^(2.5*4) = 256,000 SP.
	seedSnapshot(f.t, f.q, fixtureCharA, esi.SnapSkills, esi.Skills{
		TotalSP: 513414,
		Skills: []esi.Skill{
			{SkillID: 1001, SkillpointsInSkill: 1414, ActiveSkillLevel: 2, TrainedSkillLevel: 2},
			{SkillID: 3301, SkillpointsInSkill: 256000, ActiveSkillLevel: 5, TrainedSkillLevel: 5},
			{SkillID: 3302, SkillpointsInSkill: 256000, ActiveSkillLevel: 5, TrainedSkillLevel: 5},
		},
	})
}

// TestPlansLeaveOutSkillsAlreadyTrained: a plan is for what is left to
// learn. The Magic 14 template skips the skills the character already
// has at V (and says which), makes no plan at all when everything is
// trained, and a single skill the character already has at that level is
// refused instead of added.
func TestPlansLeaveOutSkillsAlreadyTrained(t *testing.T) {
	f := newPlanFixture(t)
	f.seedMagicSkills()
	const char = "90000001"

	// Magic 14: Mechanics is the only one of the three still to train.
	code, body, landed := f.follow("/skills/plans/from-template", url.Values{"character": {char}, "template": {"magic14"}})
	if code != http.StatusOK {
		t.Fatalf("from-template landed on %s with %d", landed, code)
	}
	plans := f.planIDs(fixtureCharA)
	if len(plans) != 1 {
		t.Fatalf("%d plans, want 1", len(plans))
	}
	items, _ := f.q.ListSkillPlanItems(context.Background(), plans[0].ID)
	if len(items) != 1 || items[0].SkillTypeID != 3303 {
		t.Fatalf("Magic 14 plan holds %+v, want only Mechanics (3303): the trained skills stay out", items)
	}
	mustContain(t, landed, body,
		"without the 2 skill(s) already trained to V", "CPU Management V", "Power Grid Management V")
	// The notice is shown once.
	_, again := f.do(http.MethodGet, landed, nil, nil).Code, f.do(http.MethodGet, landed, nil, nil).Body.String()
	if strings.Contains(again, "already trained to V") {
		t.Error("the one-time notice was shown a second time")
	}

	// Everything trained: no plan is made, and the page says why.
	seedSnapshot(t, f.q, fixtureCharA, esi.SnapSkills, esi.Skills{
		Skills: []esi.Skill{
			{SkillID: 3301, SkillpointsInSkill: 256000, ActiveSkillLevel: 5, TrainedSkillLevel: 5},
			{SkillID: 3302, SkillpointsInSkill: 256000, ActiveSkillLevel: 5, TrainedSkillLevel: 5},
			{SkillID: 3303, SkillpointsInSkill: 256000, ActiveSkillLevel: 5, TrainedSkillLevel: 5},
		},
	})
	code, body, landed = f.follow("/skills/plans/from-template", url.Values{"character": {char}, "template": {"magic14"}})
	if code != http.StatusOK {
		t.Fatalf("from-template (all trained) landed on %s with %d", landed, code)
	}
	if n := len(f.planIDs(fixtureCharA)); n != 1 {
		t.Errorf("%d plans after an all-trained template, want still 1", n)
	}
	mustContain(t, landed, body, "has already trained all 3 Magic 14 skills to V")

	// A single skill: Alpha is trained to II, so Alpha II is refused and
	// Alpha III is accepted (and needs no row for the levels below it).
	seedSnapshot(t, f.q, fixtureCharA, esi.SnapSkills, esi.Skills{
		Skills: []esi.Skill{{SkillID: 1001, SkillpointsInSkill: 1414, ActiveSkillLevel: 2, TrainedSkillLevel: 2}},
	})
	plan := fmt.Sprint(plans[0].ID)
	code, body, landed = f.follow("/skills/plans/item-add", url.Values{"character": {char}, "plan": {plan}, "skill": {"1001"}, "level": {"2"}})
	if code != http.StatusOK {
		t.Fatalf("item-add (trained) landed on %s with %d", landed, code)
	}
	mustContain(t, landed, body, "Alpha Skill is already trained to II on this character")
	items, _ = f.q.ListSkillPlanItems(context.Background(), plans[0].ID)
	for _, it := range items {
		if it.SkillTypeID == 1001 {
			t.Fatalf("a skill already trained to II was added at level %d", it.TargetLevel)
		}
	}
	code, _, _ = f.follow("/skills/plans/item-add", url.Values{"character": {char}, "plan": {plan}, "skill": {"1001"}, "level": {"3"}})
	if code != http.StatusOK {
		t.Fatalf("item-add III = %d", code)
	}
	var levels []int64
	items, _ = f.q.ListSkillPlanItems(context.Background(), plans[0].ID)
	for _, it := range items {
		if it.SkillTypeID == 1001 {
			levels = append(levels, it.TargetLevel)
		}
	}
	if len(levels) != 1 || levels[0] != 3 {
		t.Errorf("Alpha rows after adding III = %v, want just [3] (I and II are already trained)", levels)
	}
}
