package app

// ---------------------------------------------------------------------------
// Memory: the Go memory limit the process runs under, and what the
// in-memory surfaces hold, for the admin analytics page. Sizes and
// counts are read from the resident snapshots under their own locks
// by the worker's stats flush; nothing here touches the database.
// ---------------------------------------------------------------------------

import (
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
	"evesynapse/internal/memsize"
)

// applyMemoryLimit sets the Go runtime's soft memory limit when
// GOMEMLIMIT is not set: a quarter of the host's RAM, at least
// 1 GiB. The limit never drops data; it only makes the garbage
// collector work harder near it, so the in-memory surfaces keep
// room to grow while idle garbage stops crowding the database's
// page cache.
func applyMemoryLimit() {
	if os.Getenv("GOMEMLIMIT") != "" {
		logging.Infof("memory: GOMEMLIMIT set, limit %s", humanBytes(debug.SetMemoryLimit(-1)))
		return
	}
	total := hostMemTotal()
	if total <= 0 {
		return
	}
	limit := total / 4
	if limit < 1<<30 {
		limit = 1 << 30
	}
	debug.SetMemoryLimit(limit)
	logging.Infof("memory: limit %s (a quarter of %s)", humanBytes(limit), humanBytes(total))
}

// hostMemTotal is the host's RAM from /proc/meminfo, 0 when unread.
func hostMemTotal() int64 {
	mem, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(mem), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "MemTotal:" {
			kb, _ := strconv.ParseInt(f[1], 10, 64)
			return kb * 1024
		}
	}
	return 0
}

// memoryEvery is how often the worker re-measures the memory
// breakdown; the admin page only reads the stored result.
const memoryEvery = 5 * time.Minute

// memoryPart is one segment of the memory bar: an in-memory surface,
// the rest of the Go heap, or the room left under the limit.
type memoryPart struct {
	Key    string  // swatch and segment class: an-mem-<Key>
	Label  string  // "Hub order books"
	Size   string  // "412 MB"
	Detail string  // what it holds: "1,012,345 orders in 4 regions"
	Pct    float64 // share of the bar
	Share  string  // Pct as shown: "7%", "<1%"
	X      float64 // segment start, percent
}

// memoryView is the measured breakdown the admin page shows.
type memoryView struct {
	Parts      []memoryPart
	Heap       string // Go heap in use
	Limit      string // the runtime's soft memory limit
	FromOS     string // what the runtime holds from the OS
	Collection string // garbage collections so far
	At         time.Time
	Age        string // how long ago At was, set per render
}

// memoryBreakdown is the last measured breakdown, nil until the
// first measure.
func (app *Application) memoryBreakdown() *memoryView { return app.memView.Load() }

// refreshMemoryBreakdown measures what each in-memory surface holds
// (memsize's sampled estimate, each under its own read lock) against
// the Go heap and its limit, at most once per memoryEvery. The
// stats flush calls it; no page measures on load.
func (app *Application) refreshMemoryBreakdown(now time.Time) {
	if v := app.memView.Load(); v != nil && now.Sub(v.At) < memoryEvery {
		return
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	limit := debug.SetMemoryLimit(-1)
	n := func(v int) string { return esi.FormatInt(int64(v)) }
	type surface struct {
		key, label, detail string
		bytes              int64
	}
	var parts []surface

	app.hubBooksMu.RLock()
	orders, regions := 0, len(app.hubBooks)
	for _, b := range app.hubBooks {
		if b != nil {
			orders += b.orders
		}
	}
	hub := memsize.Of(app.hubBooks)
	app.hubBooksMu.RUnlock()
	parts = append(parts, surface{"hub", "Hub order books", fmt.Sprintf("%s orders in %s regions", n(orders), n(regions)), hub})

	app.statPackIdx.mu.RLock()
	if s := app.statPackIdx.snap; s != nil {
		parts = append(parts, surface{"stats", "Market stat pack",
			fmt.Sprintf("%s region rows, %s station rows, %s fallback rows, %s best beats, %s estimates",
				n(len(s.regionStats)), n(len(s.stationStats)), n(len(s.fallbacks)), n(len(s.beats)), n(len(s.estimates))),
			memsize.Of(s)})
	}
	app.statPackIdx.mu.RUnlock()

	if app.esi != nil {
		chars, corps, alliances := app.esi.NameCacheCounts()
		parts = append(parts, surface{"names", "Name cache",
			fmt.Sprintf("%s characters, %s corporations, %s alliances", n(chars), n(corps), n(alliances)), app.esi.NameCacheBytes()})
	}

	app.searchIdx.mu.RLock()
	if s := app.searchIdx.snap; s != nil {
		parts = append(parts, surface{"search", "Search index",
			fmt.Sprintf("%s items, %s systems, %s pilots, %s corporations, %s alliances",
				n(len(s.types)), n(len(s.systems)), n(len(s.pilots)), n(len(s.corps)), n(len(s.alliances))),
			memsize.Of(s)})
	}
	app.searchIdx.mu.RUnlock()

	app.entityDir.mu.RLock()
	if s := app.entityDir.snap; s != nil {
		parts = append(parts, surface{"entities", "Entity directory", fmt.Sprintf("%s entries", n(len(s.entities))), memsize.Of(s)})
	}
	app.entityDir.mu.RUnlock()

	app.sdeTopology.mu.RLock()
	if s := app.sdeTopology.snap; s != nil {
		parts = append(parts, surface{"topology", "Universe topology",
			fmt.Sprintf("%s systems, %s types", n(len(s.systems)), n(len(s.types))), memsize.Of(s)})
	}
	app.sdeTopology.mu.RUnlock()

	app.fleetOverview.mu.RLock()
	rollups := len(app.fleetOverview.rollups)
	fleet := memsize.Of(app.fleetOverview.rollups)
	app.fleetOverview.mu.RUnlock()
	app.recentKills.mu.RLock()
	recent := len(app.recentKills.kills)
	fleet += memsize.Of(app.recentKills.kills)
	app.recentKills.mu.RUnlock()
	parts = append(parts, surface{"fleet", "Fleet overview and recent kills",
		fmt.Sprintf("%s fleet rollups, %s recent kills", n(rollups), n(recent)), fleet})

	heap := int64(ms.HeapInuse)
	var held int64
	for _, p := range parts {
		held += p.bytes
	}
	if other := heap - held; other > 0 {
		parts = append(parts, surface{"other", "Rest of the Go heap", "working memory, caches and garbage not yet collected", other})
	}
	total := max(limit, heap, held)
	if room := total - max(heap, held); room > 0 {
		parts = append(parts, surface{"free", "Room under the limit", "the collector works harder as the heap nears the limit", room})
	}

	v := &memoryView{
		Heap: humanBytes(heap), Limit: humanBytes(limit), FromOS: humanBytes(int64(ms.Sys)),
		Collection: esi.FormatInt(int64(ms.NumGC)), At: now,
	}
	x := 0.0
	for _, p := range parts {
		if p.bytes <= 0 {
			continue
		}
		pct := float64(p.bytes) * 100 / float64(total)
		share := fmt.Sprintf("%.0f%%", pct)
		if pct < 1 {
			share = "<1%"
		}
		v.Parts = append(v.Parts, memoryPart{Key: p.key, Label: p.label, Size: humanBytes(p.bytes), Detail: p.detail, Pct: pct, Share: share, X: x})
		x += pct
	}
	app.memView.Store(v)
}
