package app

// A short-lived cache for anonymous views that are pure functions of
// their query parameters over stored data (the market browser, the
// Trends screen). Their underlying rows change at sweep cadence, so
// a view built seconds ago is still the truth; caching it turns a
// scripted loop over the expensive ranking queries into memory
// reads. Entries are shared pointers: builders and templates only
// ever read a built view.

import (
	"sync"
	"time"
)

type viewCacheEntry[T any] struct {
	at   time.Time
	view *T
}

type viewCache[T any] struct {
	mu      sync.Mutex
	ttl     time.Duration
	max     int
	entries map[string]viewCacheEntry[T]
}

func newViewCache[T any](ttl time.Duration, max int) *viewCache[T] {
	return &viewCache[T]{ttl: ttl, max: max, entries: make(map[string]viewCacheEntry[T])}
}

func (c *viewCache[T]) get(key string) (*T, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || time.Since(e.at) >= c.ttl {
		return nil, false
	}
	return e.view, true
}

func (c *viewCache[T]) put(key string, view *T) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= c.max {
		now := time.Now()
		for k, e := range c.entries {
			if now.Sub(e.at) >= c.ttl {
				delete(c.entries, k)
			}
		}
		if len(c.entries) >= c.max {
			c.entries = make(map[string]viewCacheEntry[T])
		}
	}
	c.entries[key] = viewCacheEntry[T]{at: time.Now(), view: view}
}
