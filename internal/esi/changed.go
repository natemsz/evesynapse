package esi

import "sync"

// changeLog remembers which characters have had new data stored since
// it was last read. New data means a payload was written: an answer of
// "not modified", which only renews the stored copy's dates, is not a
// change.
type changeLog struct {
	mu      sync.Mutex
	changed map[int64]bool
}

func (l *changeLog) note(characterID int64) {
	l.mu.Lock()
	if l.changed == nil {
		l.changed = map[int64]bool{}
	}
	l.changed[characterID] = true
	l.mu.Unlock()
}

// TakeChangedCharacters returns the characters whose stored data has
// changed since the last call, and forgets them. It knows only of what
// this client stored, and nothing from before the process started.
func (c *Client) TakeChangedCharacters() map[int64]bool {
	c.changes.mu.Lock()
	defer c.changes.mu.Unlock()
	out := c.changes.changed
	c.changes.changed = nil
	return out
}

// NoteCharacterChanged records that a character's stored data was
// changed by something other than this client.
func (c *Client) NoteCharacterChanged(characterID int64) { c.changes.note(characterID) }
