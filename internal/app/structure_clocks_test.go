package app

import (
	"testing"

	"evesynapse/internal/esi"
)

// TestStructureClocks: one reading of a structures snapshot serves
// the pulse and the upkeep module alike. Unanchoring wins over a
// state timer, an unnamed structure shows its ID, and a time that
// does not parse is no clock.
func TestStructureClocks(t *testing.T) {
	clocks := structureClocks(esi.CorpStructures{
		{StructureID: 1, Name: "Alpha", UnanchorsAt: "2026-10-20T12:00:00Z", StateTimerEnd: "2026-10-18T12:00:00Z"},
		{StructureID: 2, StateTimerEnd: "2026-10-19T12:00:00Z"},
		{StructureID: 3, Name: "Quiet"},
		{StructureID: 4, Name: "Junk", UnanchorsAt: "soon"},
	})
	if len(clocks) != 2 {
		t.Fatalf("clocks = %+v, want two", clocks)
	}
	if c := clocks[0]; c.Name != "Alpha" || c.Text != "unanchoring" || c.At.Day() != 20 {
		t.Errorf("first clock = %+v, want Alpha unanchoring on the 20th", c)
	}
	if c := clocks[1]; c.Name != "Structure #2" || c.Text != "state change" || c.At.Day() != 19 {
		t.Errorf("second clock = %+v, want structure #2's state change on the 19th", c)
	}
}
