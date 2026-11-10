package esi

// The GET collapse and ETag cache behind request. Concurrent identical
// GETs share one ESI call. An answer that came with an ETag is held in
// memory: inside ESI's own cache window it is served without a call
// (ESI would answer the same bytes), and once the window has passed the
// next read offers the ETag, so an unchanged resource costs a 304 and
// no download. The cache is memory only and bounded by entries and by
// bytes; a restart starts it empty. Keys are scoped by the character
// a token speaks for, so answers survive the token's refresh and two
// characters never share one.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	json "github.com/goccy/go-json"
	"golang.org/x/sync/singleflight"
)

const (
	// getCacheMaxEntries and getCacheMaxBytes bound the cache: past
	// either, entries whose window ended longest ago go first.
	getCacheMaxEntries = 4096
	getCacheMaxBytes   = 32 << 20
	// getCacheMaxBody keeps big answers out: whole market books and
	// long lists ride FetchRawLong and the snapshot pipeline, which
	// keep their own ETags.
	getCacheMaxBody = 256 << 10
	// getCacheRevalidateFor is how long past its window an entry is
	// kept for an ETag revalidation before it is dropped.
	getCacheRevalidateFor = 6 * time.Hour
	// sharedGetTimeout bounds one collapsed call. The call runs on
	// its own context, so the first caller leaving does not fail
	// everyone waiting on it; this keeps it from running forever.
	sharedGetTimeout = 60 * time.Second
)

// getCache is the Client's collapse group and ETag store.
type getCache struct {
	flight singleflight.Group

	mu      sync.Mutex
	entries map[string]*getEntry
	bytes   int
}

// getEntry is one held answer: the body, the headers callers read,
// the ETag, and the end of ESI's cache window. Every reader shares
// the body slice; callers decode it and never write to it.
type getEntry struct {
	etag    string
	body    []byte
	header  http.Header
	expires time.Time
}

// keptHeaders are the response headers a held answer keeps, the ones
// callers read from a GET (paging and the cache window).
var keptHeaders = []string{"Etag", "Expires", "Last-Modified", "Cache-Control", "X-Pages"}

// getResult is the shared answer of one collapsed GET.
type getResult struct {
	body   []byte
	header http.Header
}

// cachedGet is request's GET: collapsed with any identical GET in
// flight, then answered from the cache when it can be.
func (c *Client) cachedGet(ctx context.Context, accessToken, path string, want ...int) ([]byte, http.Header, error) {
	key := getCacheKey(accessToken, path)
	ch := c.getCache.flight.DoChan(key+"\x00"+fmt.Sprint(want), func() (any, error) {
		// The shared call keeps the first caller's values but not
		// its cancellation: one reader leaving must not fail the
		// others waiting on the same call.
		callCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sharedGetTimeout)
		defer cancel()
		body, header, err := c.getThroughCache(callCtx, key, accessToken, path, want...)
		if err != nil {
			return nil, err
		}
		return getResult{body: body, header: header}, nil
	})
	select {
	case res := <-ch:
		if res.Err != nil {
			return nil, nil, res.Err
		}
		out := res.Val.(getResult)
		return out.body, out.header, nil
	case <-ctx.Done():
		return nil, nil, fmt.Errorf("ESI GET %s: %w", path, ctx.Err())
	}
}

// getThroughCache serves a held answer inside its window, revalidates
// it with its ETag after, and fetches in full when nothing is held.
func (c *Client) getThroughCache(ctx context.Context, key, accessToken, path string, want ...int) ([]byte, http.Header, error) {
	held, ok := c.getCache.get(key)
	if ok && time.Now().Before(held.expires) {
		return held.body, held.header.Clone(), nil
	}
	if ok {
		want304 := append(append([]int(nil), want...), http.StatusNotModified)
		body, header, status, err := c.send(ctx, c.http, http.MethodGet, accessToken, path, nil, held.etag, want304...)
		if err != nil {
			return nil, nil, err
		}
		if status == http.StatusNotModified {
			// Unchanged: the held body stands for a new window.
			c.getCache.renew(key, header)
			if fresh, ok := c.getCache.get(key); ok {
				return fresh.body, fresh.header.Clone(), nil
			}
			return held.body, held.header.Clone(), nil
		}
		c.getCache.put(key, header, body)
		return body, header, nil
	}
	body, header, _, err := c.send(ctx, c.http, http.MethodGet, accessToken, path, nil, "", want...)
	if err != nil {
		return nil, nil, err
	}
	c.getCache.put(key, header, body)
	return body, header, nil
}

func (g *getCache) get(key string) (getEntry, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	e, ok := g.entries[key]
	if !ok {
		return getEntry{}, false
	}
	if time.Since(e.expires) > getCacheRevalidateFor {
		g.removeLocked(key)
		return getEntry{}, false
	}
	return *e, true
}

// renew moves a held answer's window after a 304, Expires header
// included, so callers reading it see the new window.
func (g *getCache) renew(key string, header http.Header) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if e, ok := g.entries[key]; ok {
		e.expires = cacheWindowEnd(header)
		e.header = e.header.Clone()
		e.header.Set("Expires", e.expires.UTC().Format(http.TimeFormat))
	}
}

// put holds a 200 answer that came with an ETag and fits the cache.
func (g *getCache) put(key string, header http.Header, body []byte) {
	etag := header.Get("ETag")
	if etag == "" || len(body) == 0 || len(body) > getCacheMaxBody {
		return
	}
	kept := make(http.Header, len(keptHeaders))
	for _, h := range keptHeaders {
		if v := header.Values(h); len(v) > 0 {
			kept[h] = append([]string(nil), v...)
		}
	}
	e := &getEntry{etag: etag, body: body, header: kept, expires: cacheWindowEnd(header)}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.entries == nil {
		g.entries = make(map[string]*getEntry)
	}
	g.removeLocked(key)
	for len(g.entries) >= getCacheMaxEntries || g.bytes+len(body) > getCacheMaxBytes {
		if !g.evictOldestLocked() {
			break
		}
	}
	g.entries[key] = e
	g.bytes += len(body)
}

func (g *getCache) removeLocked(key string) {
	if e, ok := g.entries[key]; ok {
		g.bytes -= len(e.body)
		delete(g.entries, key)
	}
}

// evictOldestLocked drops the entry whose window ended first.
func (g *getCache) evictOldestLocked() bool {
	var oldestKey string
	var oldest time.Time
	for k, e := range g.entries {
		if oldestKey == "" || e.expires.Before(oldest) {
			oldestKey, oldest = k, e.expires
		}
	}
	if oldestKey == "" {
		return false
	}
	g.removeLocked(oldestKey)
	return true
}

// getCacheKey scopes a GET's full path (query included) by who asks:
// the character an EVE SSO token speaks for, read from its subject, so
// the key outlives the token's refresh. A token whose subject cannot
// be read is scoped by a hash of itself instead; a raw token is never
// held. Public reads share one scope.
func getCacheKey(accessToken, path string) string {
	if accessToken == "" {
		return "public\x00" + path
	}
	if id := tokenCharacter(accessToken); id != "" {
		return "char:" + id + "\x00" + path
	}
	sum := sha256.Sum256([]byte(accessToken))
	return "token:" + hex.EncodeToString(sum[:8]) + "\x00" + path
}

// tokenCharacter reads the character id from an EVE SSO access
// token's subject ("CHARACTER:EVE:<id>"). The token is the app's own,
// already verified at sign-in; this reads it only to key the cache.
func tokenCharacter(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if json.Unmarshal(raw, &claims) != nil {
		return ""
	}
	id, ok := strings.CutPrefix(claims.Sub, "CHARACTER:EVE:")
	if !ok || id == "" || strings.ContainsFunc(id, func(r rune) bool { return r < '0' || r > '9' }) {
		return ""
	}
	return id
}
