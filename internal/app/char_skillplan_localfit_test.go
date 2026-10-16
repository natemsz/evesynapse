package app

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// Training plans from EveSynapse fits: the fit preview and the
// from-fit creation accept a local fit (?local= / form local) and
// run the same closure as an EVE fitting; another user's local
// fit is not a plan source.
func TestSkillPlanFromLocalFit(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	other, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create other user: %v", err)
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

	// The fixture universe: Test Hull 3001, Test Blaster 2001
	// (needs Beta Skill II, which needs Alpha Skill III; the
	// character has Alpha II).
	doc := `{"name":"Local Test Fit","shipTypeId":3001,"items":[{"typeId":2001,"qty":1}],"charges":{}}`
	local, err := q.CreateLocalFitting(ctx, db.CreateLocalFittingParams{
		UserID: user.ID, Name: "Local Test Fit", ShipTypeID: 3001, ItemsJson: doc,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create local fit: %v", err)
	}
	stranger, err := q.CreateLocalFitting(ctx, db.CreateLocalFittingParams{
		UserID: other.ID, Name: "Stranger Fit", ShipTypeID: 3001, ItemsJson: doc,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create stranger fit: %v", err)
	}

	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Plan Tester")
	get := func(path string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	post := func(path string, form url.Values) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec.Code
	}

	code, body := get(fmt.Sprintf("/skills/plans/fit?character=90000001&local=%d", local.ID))
	if code != 200 {
		t.Fatalf("GET local fit preview = %d", code)
	}
	mustContain(t, "local fit preview", body,
		"Local Test Fit", "Alpha Skill", "Beta Skill",
		fmt.Sprintf(`name="local" value="%d"`, local.ID))

	if code := post("/skills/plans/from-fit", url.Values{
		"character": {"90000001"}, "local": {fmt.Sprint(local.ID)},
	}); code != 200 && code != 303 {
		t.Fatalf("POST from-fit local = %d", code)
	}
	plans, err := q.ListSkillPlans(ctx, db.ListSkillPlansParams{UserID: user.ID, CharacterID: fixtureCharA})
	if err != nil || len(plans) != 1 {
		t.Fatalf("plans after local create = %v, err %v", plans, err)
	}
	if plans[0].Name != "Fit: Local Test Fit" {
		t.Errorf("plan name = %q, want %q", plans[0].Name, "Fit: Local Test Fit")
	}

	// Another user's fit: the preview refuses and no plan appears.
	code, body = get(fmt.Sprintf("/skills/plans/fit?character=90000001&local=%d", stranger.ID))
	if code != 200 {
		t.Fatalf("GET stranger fit preview = %d", code)
	}
	mustContain(t, "stranger fit preview", body, "one of your EveSynapse fits.")
	if code := post("/skills/plans/from-fit", url.Values{
		"character": {"90000001"}, "local": {fmt.Sprint(stranger.ID)},
	}); code != 200 && code != 303 {
		t.Fatalf("POST from-fit stranger = %d", code)
	}
	plans, err = q.ListSkillPlans(ctx, db.ListSkillPlansParams{UserID: user.ID, CharacterID: fixtureCharA})
	if err != nil || len(plans) != 1 {
		t.Fatalf("plans after stranger attempts = %v, err %v", plans, err)
	}
}
