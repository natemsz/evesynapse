package app

import (
	"strings"
	"testing"
)

// boxClasses renders skillLevel and returns the class of each of
// the five boxes in order ("" for untrained/dark).
func boxClasses(t *testing.T, trained, next int, state string) []string {
	t.Helper()
	html := string(skillLevel(trained, next, state))
	// Split on "<i" markers; each box is <i> or <i class="...">.
	parts := strings.Split(html, "<i")
	if len(parts) != 6 {
		t.Fatalf("expected 5 boxes, got %d in %q", len(parts)-1, html)
	}
	out := make([]string, 5)
	for i := 0; i < 5; i++ {
		p := parts[i+1]
		if strings.HasPrefix(p, ` class="`) {
			end := strings.Index(p[8:], `"`)
			out[i] = p[8 : 8+end]
		} else {
			out[i] = ""
		}
	}
	return out
}

func eqClasses(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Skill trained to 3, levels 4 and 5 queued.
// The level-5 queue row must show 1-3 trained and 4-5 queued.
func TestSkillLevelQueuedRowShowsOwnLevelRange(t *testing.T) {
	got := boxClasses(t, 3, 5, "queued")
	want := []string{"on", "on", "on", "queued", "queued"}
	if !eqClasses(got, want) {
		t.Fatalf("queued row: got %v want %v", got, want)
	}
}

// Training Power Grid to V from scratch — five
// queue rows. Each row must show queued only up to its own level,
// not all five boxes blue on every row.
func TestSkillLevelQueuedRowOnlyToOwnLevel(t *testing.T) {
	for lvl := 1; lvl <= 5; lvl++ {
		got := boxClasses(t, 0, lvl, "queued")
		want := make([]string, 5)
		for i := 0; i < lvl; i++ {
			want[i] = "queued"
		}
		if !eqClasses(got, want) {
			t.Fatalf("queued row level %d: got %v want %v", lvl, got, want)
		}
	}
}

// The training row pulses only the level actually training —
// never a later queued level.
func TestSkillLevelTrainingPulsesOwnLevel(t *testing.T) {
	got := boxClasses(t, 3, 4, "training")
	want := []string{"on", "on", "on", "training", ""}
	if !eqClasses(got, want) {
		t.Fatalf("training row: got %v want %v", got, want)
	}
}
