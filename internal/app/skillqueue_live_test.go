package app

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"evesynapse/internal/app/skillplans"
	"evesynapse/internal/esi"
)

// TestSkillqueueFinishedEntriesAreTrained: a stored queue still leads
// with a level that finished hours ago (ESI keeps it until the next
// login). Every view reads it as trained, not queued: the character
// page trains the first live entry and lists only live ones, the home
// fleet row agrees, and the skill planner neither counts the finished
// level as queued nor ends the queue on it. An entry with no finish
// date stays, and the stored snapshot is left as fetched.
func TestSkillqueueFinishedEntriesAreTrained(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	now := time.Now().UTC()
	rfc := func(t time.Time) string { return t.Format(time.RFC3339) }
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	seedSnapshot(t, q, fixtureCharA, esi.SnapSkills, esi.Skills{Skills: []esi.Skill{
		{SkillID: 3300, SkillpointsInSkill: 256000, TrainedSkillLevel: 5, ActiveSkillLevel: 5},
	}})
	stored := esi.Skillqueue{
		{SkillID: 3300, QueuePosition: 0, FinishedLevel: 5, FinishDate: rfc(now.Add(-2 * time.Hour))},
		{SkillID: 3301, QueuePosition: 1, FinishedLevel: 2, FinishDate: rfc(now.Add(3 * time.Hour))},
		{SkillID: 3302, QueuePosition: 2, FinishedLevel: 1, FinishDate: rfc(now.Add(30 * time.Hour))},
	}
	seedSnapshot(t, q, fixtureCharA, esi.SnapSkillqueue, stored)
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")

	// The shared filter: the finished head goes, the rest keep their
	// order, and an undated entry stays.
	live := liveSkillqueue(append(stored, esi.SkillqueueEntry{SkillID: 3303, QueuePosition: 3, FinishedLevel: 1}), now)
	if len(live) != 3 || live[0].SkillID != 3301 || live[2].SkillID != 3303 {
		t.Fatalf("live queue = %+v, want 3301, 3302, then the undated 3303", live)
	}
	if head := queueHead(stored, now); head == nil || head.SkillID != 3301 {
		t.Fatalf("queue head = %+v, want 3301", head)
	}
	if queueHead(stored[:1], now) != nil {
		t.Fatal("a queue whose only entry finished still has a head")
	}

	code, body := getPage(t, app, cookie, "/character/")
	if code != http.StatusOK {
		t.Fatalf("GET /character/: status %d", code)
	}
	mustContain(t, "/character/", body, "Currently training: <strong>Type #3301 II")
	for _, stale := range []string{"Type #3300 V", "(done)"} {
		if strings.Contains(body, stale) {
			t.Errorf("character page still shows the finished entry: %q", stale)
		}
	}

	setHomeLayout(t, q, user.ID, `[{"id":"fleet"}]`)
	_, body = getPage(t, app, cookie, "/")
	mustContain(t, "/ fleet", body, "Type #3301 II")
	if strings.Contains(body, "Type #3300 V") {
		t.Error("home fleet row shows the finished entry as training")
	}

	ct, _, ok := skillplans.LoadCharTraining(ctx, app, fixtureCharA)
	if !ok {
		t.Fatal("skill planner: training state not loaded")
	}
	if _, queued := ct.QueuedTo[3300]; queued {
		t.Errorf("skill planner counts the finished level as queued: %v", ct.QueuedTo)
	}
	if ct.QueuedTo[3301] != 2 || !ct.QueueEnd.Equal(now.Add(30*time.Hour).Truncate(time.Second)) {
		t.Errorf("skill planner queue = %v ending %v, want 3301 to II ending at the last live entry", ct.QueuedTo, ct.QueueEnd)
	}

	// The worker's snapshot is untouched.
	var after esi.Skillqueue
	if !app.loadCorpSnapshot(ctx, fixtureCharA, esi.SnapSkillqueue, &after) || len(after) != len(stored) {
		t.Errorf("stored queue changed: %+v", after)
	}
}
