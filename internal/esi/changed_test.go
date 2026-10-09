package esi

import "testing"

// TestChangedCharacters: characters noted as changed are handed over
// once and then forgotten.
func TestChangedCharacters(t *testing.T) {
	c := New(nil, nil, nil)
	if got := c.TakeChangedCharacters(); len(got) != 0 {
		t.Fatalf("changes before anything was stored: %v", got)
	}
	c.changes.note(1)
	c.changes.note(1)
	c.NoteCharacterChanged(2)
	got := c.TakeChangedCharacters()
	if len(got) != 2 || !got[1] || !got[2] || got[3] {
		t.Fatalf("changed: %v, want characters 1 and 2", got)
	}
	if got := c.TakeChangedCharacters(); len(got) != 0 {
		t.Fatalf("changes were not forgotten once read: %v", got)
	}
}
