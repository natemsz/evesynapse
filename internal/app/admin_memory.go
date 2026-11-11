package app

// ---------------------------------------------------------------------------
// Memory: the Go memory limit the process runs under, and what the
// in-memory surfaces hold, for the admin analytics page. Counts are
// read from the resident snapshots under their own locks; nothing
// here touches the database.
// ---------------------------------------------------------------------------

import (
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"

	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
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

// memoryFacts is the admin page's memory lines: the Go heap against
// the limit, then each in-memory surface's entry count.
func (app *Application) memoryFacts() []string {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	n := func(v int) string { return esi.FormatInt(int64(v)) }
	facts := []string{
		fmt.Sprintf("Go heap in use %s, from the OS %s, limit %s, %s collections",
			humanBytes(int64(ms.HeapInuse)), humanBytes(int64(ms.Sys)), humanBytes(debug.SetMemoryLimit(-1)), esi.FormatInt(int64(ms.NumGC))),
	}

	app.hubBooksMu.RLock()
	orders, regions := 0, len(app.hubBooks)
	for _, b := range app.hubBooks {
		if b != nil {
			orders += b.orders
		}
	}
	app.hubBooksMu.RUnlock()
	facts = append(facts, fmt.Sprintf("Hub order books %s orders in %s regions", n(orders), n(regions)))

	app.statPackIdx.mu.RLock()
	if s := app.statPackIdx.snap; s != nil {
		facts = append(facts, fmt.Sprintf("Market stat pack %s region rows, %s station rows, %s fallback rows, %s best beats, %s estimates",
			n(len(s.regionStats)), n(len(s.stationStats)), n(len(s.fallbacks)), n(len(s.beats)), n(len(s.estimates))))
	}
	app.statPackIdx.mu.RUnlock()

	chars, corps, alliances := app.esi.NameCacheCounts()
	facts = append(facts, fmt.Sprintf("Name cache %s characters, %s corporations, %s alliances", n(chars), n(corps), n(alliances)))

	app.searchIdx.mu.RLock()
	if s := app.searchIdx.snap; s != nil {
		facts = append(facts, fmt.Sprintf("Search index %s items, %s systems, %s pilots, %s corporations, %s alliances",
			n(len(s.types)), n(len(s.systems)), n(len(s.pilots)), n(len(s.corps)), n(len(s.alliances))))
	}
	app.searchIdx.mu.RUnlock()

	app.entityDir.mu.RLock()
	if s := app.entityDir.snap; s != nil {
		facts = append(facts, fmt.Sprintf("Entity directory %s entries", n(len(s.entities))))
	}
	app.entityDir.mu.RUnlock()

	app.sdeTopology.mu.RLock()
	if s := app.sdeTopology.snap; s != nil {
		facts = append(facts, fmt.Sprintf("Universe topology %s systems, %s types", n(len(s.systems)), n(len(s.types))))
	}
	app.sdeTopology.mu.RUnlock()

	app.fleetOverview.mu.RLock()
	rollups := len(app.fleetOverview.rollups)
	app.fleetOverview.mu.RUnlock()
	app.recentKills.mu.RLock()
	recent := len(app.recentKills.kills)
	app.recentKills.mu.RUnlock()
	facts = append(facts, fmt.Sprintf("Fleet overview rollups %s, recent kills %s", n(rollups), n(recent)))
	return facts
}
