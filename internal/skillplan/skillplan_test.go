package skillplan

// Engine tests: the SP table, topological ordering with prerequisite
// expansion, trained/queued/partial-SP netting, time math, the remap
// advisor and the fit-requirement closure. All on a tiny hand-built
// skill graph; nothing here needs a database.

import (
	"sort"
	"testing"
	"time"
)

func TestSPTable(t *testing.T) {
	cases := []struct {
		rank  float64
		level int
		want  int64
	}{
		{1, 1, 250}, {1, 2, 1414}, {1, 3, 8000}, {1, 4, 45255}, {1, 5, 256000},
		{5, 3, 40000}, {5, 2, 7071}, {14, 5, 3584000}, {2, 4, 90510},
	}
	for _, c := range cases {
		if got := SPForLevel(c.rank, c.level); got != c.want {
			t.Errorf("SPForLevel(%v, %d) = %d, want %d", c.rank, c.level, got, c.want)
		}
	}
	if got := LevelForSP(1, 5000); got != 2 {
		t.Errorf("LevelForSP(1, 5000) = %d, want 2 (partial level III progress)", got)
	}
	if got := LevelForSP(1, 256000); got != 5 {
		t.Errorf("LevelForSP(1, 256000) = %d, want 5", got)
	}
}

// fixtureGraph is a tiny hand-built skill graph:
//
//	A (rank 1, Int/Mem) — no prereqs
//	B (rank 2, Int/Mem) — requires A III
//	C (rank 1, Per/Wil) — no prereqs
//	M (a module type)   — requires B II
type fixtureGraph struct{}

func (fixtureGraph) Meta(id int64) (SkillMeta, bool) {
	switch id {
	case 1001:
		return SkillMeta{Rank: 1, Primary: AttrIntelligence, Secondary: AttrMemory}, true
	case 1002:
		return SkillMeta{Rank: 2, Primary: AttrIntelligence, Secondary: AttrMemory}, true
	case 1003:
		return SkillMeta{Rank: 1, Primary: AttrPerception, Secondary: AttrWillpower}, true
	}
	return SkillMeta{}, false
}

func (fixtureGraph) Requirements(id int64) []Requirement {
	switch id {
	case 1002:
		return []Requirement{{SkillID: 1001, Level: 3}}
	case 2001: // a module: requires B II
		return []Requirement{{SkillID: 1002, Level: 2}}
	}
	return nil
}

var testAttrs = AttrSet{Charisma: 17, Intelligence: 30, Memory: 20, Perception: 25, Willpower: 20}

func TestComputePlanExpandsPrereqsAndOrders(t *testing.T) {
	char := CharTraining{SP: map[int64]int64{1001: 1414}, QueuedTo: map[int64]int{}} // A at II
	out := Compute(fixtureGraph{}, []Target{{SkillID: 1002, Level: 1, Intent: 1}}, char, testAttrs, time.Now())

	if len(out.Steps) != 2 {
		t.Fatalf("steps = %v, want A then B", out.Steps)
	}
	a, b := out.Steps[0], out.Steps[1]
	if a.SkillID != 1001 || !a.Prereq || a.FromLevel != 2 || a.ToLevel != 3 {
		t.Errorf("prereq step = %+v, want A II→III marked prereq", a)
	}
	if a.SPRemaining != 8000-1414 {
		t.Errorf("A remaining SP = %d, want %d", a.SPRemaining, 8000-1414)
	}
	if b.SkillID != 1002 || b.Prereq || b.FromLevel != 0 || b.ToLevel != 1 {
		t.Errorf("target step = %+v, want B 0→I", b)
	}
	if b.SPRemaining != 500 { // rank 2 level I = 500
		t.Errorf("B remaining SP = %d, want 500", b.SPRemaining)
	}
	if out.TotalSP != a.SPRemaining+b.SPRemaining {
		t.Errorf("total SP = %d, want %d", out.TotalSP, a.SPRemaining+b.SPRemaining)
	}
}

func TestComputePlanNetting(t *testing.T) {
	t.Run("partial SP counts", func(t *testing.T) {
		char := CharTraining{SP: map[int64]int64{1001: 5000}, QueuedTo: map[int64]int{}}
		out := Compute(fixtureGraph{}, []Target{{SkillID: 1001, Level: 3, Intent: 1}}, char, testAttrs, time.Now())
		if len(out.Steps) != 1 {
			t.Fatalf("steps = %v, want one", out.Steps)
		}
		if out.Steps[0].SPRemaining != 3000 || out.Steps[0].FromLevel != 2 {
			t.Errorf("step = %+v, want 3000 SP from level II", out.Steps[0])
		}
	})

	t.Run("queued levels net out as in queue", func(t *testing.T) {
		now := time.Now()
		char := CharTraining{
			SP:       map[int64]int64{1001: 1414},
			QueuedTo: map[int64]int{1001: 3},
			QueueEnd: now.Add(2 * time.Hour),
		}
		out := Compute(fixtureGraph{}, []Target{{SkillID: 1001, Level: 3, Intent: 1}}, char, testAttrs, now)
		if len(out.Steps) != 0 {
			t.Fatalf("steps = %v, want none (queued)", out.Steps)
		}
		if len(out.Dropped) != 1 || out.Dropped[0].Reason != "already in queue" {
			t.Fatalf("dropped = %v, want already in queue", out.Dropped)
		}
		if !out.StartsAt.Equal(char.QueueEnd) {
			t.Errorf("StartsAt = %v, want queue end %v", out.StartsAt, char.QueueEnd)
		}
	})

	t.Run("target at trained level drops as trained", func(t *testing.T) {
		char := CharTraining{SP: map[int64]int64{1001: 8000}, QueuedTo: map[int64]int{}}
		out := Compute(fixtureGraph{}, []Target{{SkillID: 1001, Level: 2, Intent: 1}}, char, testAttrs, time.Now())
		if len(out.Steps) != 0 || len(out.Dropped) != 1 || out.Dropped[0].Reason != "already trained" {
			t.Fatalf("out = %+v, want dropped as already trained", out)
		}
	})

	t.Run("step times accumulate from queue end", func(t *testing.T) {
		now := time.Now()
		char := CharTraining{SP: map[int64]int64{}, QueuedTo: map[int64]int{}, QueueEnd: now.Add(time.Hour)}
		out := Compute(fixtureGraph{}, []Target{{SkillID: 1001, Level: 1, Intent: 1}}, char, testAttrs, now)
		if len(out.Steps) != 1 {
			t.Fatalf("steps = %v", out.Steps)
		}
		// 250 SP at Int 30 + Mem 20/2 = 40 SP/min → 6.25 min.
		wantFinish := char.QueueEnd.Add(375 * time.Second)
		if !out.Steps[0].Finish.Equal(wantFinish) {
			t.Errorf("finish = %v, want %v", out.Steps[0].Finish, wantFinish)
		}
	})
}

func TestRemapAdvisorPicksDominantAttributes(t *testing.T) {
	steps := []Step{
		{SkillID: 1003, SPRemaining: 100000}, // Per/Wil skill
	}
	advice := AdviseRemap(fixtureGraph{}, steps, FlatAttrSet)
	if advice.Best.Perception != 27 || advice.Best.Willpower != 21 {
		t.Errorf("best spread = %+v, want Perception 27 / Willpower 21 (primary takes the 10-point cap, secondary the rest)", advice.Best)
	}
	if advice.BestSeconds >= advice.CurSeconds {
		t.Errorf("remap should beat flat 20s: best %v, current %v", advice.BestSeconds, advice.CurSeconds)
	}
}

func TestFitClosureExpandsThroughSkills(t *testing.T) {
	targets := FitSkillClosure(fixtureGraph{}, []int64{2001})
	if len(targets) != 2 {
		t.Fatalf("closure = %v, want A then B", targets)
	}
	if targets[0].SkillID != 1001 || targets[0].Level != 3 {
		t.Errorf("first = %+v, want A III (prereq of B)", targets[0])
	}
	if targets[1].SkillID != 1002 || targets[1].Level != 2 {
		t.Errorf("second = %+v, want B II (the module's requirement)", targets[1])
	}
	if st := sortedTargets(targets); st[0].SkillID != 1001 {
		t.Errorf("sorted sanity: %v", st)
	}
}

// sortedTargets returns the targets ordered by skill, so a test can
// index into them whatever order the engine produced.
func sortedTargets(targets []Target) []Target {
	out := append([]Target(nil), targets...)
	sort.Slice(out, func(i, j int) bool { return out[i].SkillID < out[j].SkillID })
	return out
}
