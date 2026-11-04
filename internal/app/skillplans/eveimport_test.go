package skillplans

import (
	"testing"

	"evesynapse/internal/skillplan"
)

// The game's import reads one line a level, in training order, from
// the next untrained level up to the target.
func TestEVEImportText(t *testing.T) {
	steps := []skillplan.Step{{SkillID: 1, FromLevel: 3, ToLevel: 5}, {SkillID: 2, FromLevel: 0, ToLevel: 1}}
	got := eveImportText(steps, map[int64]string{1: "Amarr Battleship", 2: "Hull Upgrades"})
	want := "Amarr Battleship 4\nAmarr Battleship 5\nHull Upgrades 1\n"
	if got != want {
		t.Errorf("eveImportText = %q, want %q", got, want)
	}
}
