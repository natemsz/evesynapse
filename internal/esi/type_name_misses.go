package esi

import "sort"

// Type names nobody here could supply: IDs a reader asked for
// that were in neither the in-process cache, the SDE tables nor
// the type_names table. They are the instance's standing demand
// for type names, which the shared cache asks peers for before
// ESI is spent on them. In memory only: a restart forgets the
// list and the next readers rebuild it.

// maxTypeNameMisses bounds the list. Past it new misses are not
// added until old ones resolve; the IDs still read as unnamed
// and are noted again later.
const maxTypeNameMisses = 2000

// NoteTypeNameMiss puts one type ID on the miss list, as a reader
// finding it unnamed would: a dedicated swarm node takes a peer's
// demand for a type name this way.
func (c *Client) NoteTypeNameMiss(id int64) { c.noteTypeNameMiss(id) }

func (c *Client) noteTypeNameMiss(id int64) {
	if id <= 0 {
		return
	}
	c.typeNamesMu.Lock()
	defer c.typeNamesMu.Unlock()
	if c.typeNameMisses == nil {
		c.typeNameMisses = make(map[int64]struct{})
	}
	if _, known := c.typeNameMisses[id]; known || len(c.typeNameMisses) >= maxTypeNameMisses {
		return
	}
	c.typeNameMisses[id] = struct{}{}
}

// MissingTypeNames lists up to limit type IDs readers have asked
// for and found unnamed, lowest ID first so the answer is stable
// from one call to the next.
func (c *Client) MissingTypeNames(limit int) []int64 {
	c.typeNamesMu.RLock()
	ids := make([]int64, 0, len(c.typeNameMisses))
	for id := range c.typeNameMisses {
		ids = append(ids, id)
	}
	c.typeNamesMu.RUnlock()
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if limit >= 0 && len(ids) > limit {
		ids = ids[:limit]
	}
	return ids
}

// LearnTypeName puts a name obtained elsewhere (the shared cache)
// into the in-process cache and settles its miss. The caller has
// already stored it.
func (c *Client) LearnTypeName(id int64, name string) {
	if id <= 0 || name == "" {
		return
	}
	c.typeNamesMu.Lock()
	c.typeNames[id] = name
	delete(c.typeNameMisses, id)
	c.typeNamesMu.Unlock()
}

// ForgetTypeName drops a name from the in-process cache, for a
// caller that has just removed it from the table: without this
// the removed name would keep being served until a restart.
func (c *Client) ForgetTypeName(id int64) {
	c.typeNamesMu.Lock()
	delete(c.typeNames, id)
	c.typeNamesMu.Unlock()
}
