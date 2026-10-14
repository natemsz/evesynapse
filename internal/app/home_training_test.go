package app

// Training horizon tests: the widget looks deeper than queue
// heads (second queued skills appear) and names idle
// characters, over the real router with seeded snapshots.

import (
	"context"
	"net/http"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

func TestTrainingHorizonRenders(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now().UTC()

	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	seedCharacter(t, q, user.ID, fixtureCharB, "Fixture Beta")
	if err := q.UpsertTypeName(ctx, db.UpsertTypeNameParams{TypeID: 3300, Name: "Gunnery"}); err != nil {
		t.Fatalf("seed type name: %v", err)
	}
	if err := q.UpsertTypeName(ctx, db.UpsertTypeNameParams{TypeID: 3301, Name: "Missiles"}); err != nil {
		t.Fatalf("seed type name: %v", err)
	}
	// Alpha trains two skills: the horizon shows both, where the
	// Skills widget shows the head only.
	seedSnapshot(t, q, fixtureCharA, esi.SnapSkillqueue, esi.Skillqueue{
		{SkillID: 3300, FinishedLevel: 5, QueuePosition: 0, FinishDate: rfc(now.Add(2 * time.Hour))},
		{SkillID: 3301, FinishedLevel: 4, QueuePosition: 1, FinishDate: rfc(now.Add(26 * time.Hour))},
	})
	// Beta queues nothing: idle by name.
	seedSnapshot(t, q, fixtureCharB, esi.SnapSkillqueue, esi.Skillqueue{})
	setHomeLayout(t, q, user.ID, `[{"id":"training"}]`)
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")
	code, body := getPage(t, app, cookie, "/")
	if code != http.StatusOK {
		t.Fatalf("GET /: status %d", code)
	}
	mustContain(t, "/", body, "Training horizon", "Gunnery", "Missiles", "Idle: Fixture Beta.")
}

// TestTrainingHorizonSkipsFinishedSkills: a stored queue keeps
// entries whose finish time has passed since it was fetched.
// They are not upcoming: the horizon lists only what is still
// to finish, soonest first.
func TestTrainingHorizonSkipsFinishedSkills(t *testing.T) {
	transport := &countingTransport{}
	app, _, _ := buildCorpTestApp(t, transport)
	now := time.Now().UTC()
	bundles := []*charSnaps{{
		ch: db.Character{CharacterID: fixtureCharA, Name: "Fixture Alpha"}, queueKnown: true,
		queue: esi.Skillqueue{
			{SkillID: 3300, FinishedLevel: 3, FinishDate: rfc(now.Add(-3 * time.Hour))},
			{SkillID: 3301, FinishedLevel: 4, FinishDate: rfc(now.Add(26 * time.Hour))},
			{SkillID: 3302, FinishedLevel: 5, FinishDate: rfc(now.Add(2 * time.Hour))},
		},
	}}
	w := app.buildTraining(context.Background(), bundles)
	if len(w.Events) != 2 {
		t.Fatalf("horizon lists %d events, want the 2 still to finish: %+v", len(w.Events), w.Events)
	}
	if w.Events[0].SkillID != 3302 || w.Events[1].SkillID != 3301 {
		t.Fatalf("order %d, %d; want soonest first (3302, 3301)", w.Events[0].SkillID, w.Events[1].SkillID)
	}
	if len(w.Idle) != 0 {
		t.Fatalf("a character still training is listed idle: %v", w.Idle)
	}
}
