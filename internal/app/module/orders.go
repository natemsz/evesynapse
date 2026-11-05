package module

// Market orders, shared by the pilot's My Orders and the corporation's
// Orders pages: the words and tones a listed order carries.

import (
	"context"
	"fmt"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/humanize"
	"evesynapse/internal/logging"
)

// OrderHealthTone is the tone of a stored order-health verdict:
// "bad" when beaten at the order's own station, "warn" when beaten
// elsewhere in the region, "good" when the order holds the station's
// best price, "" for any other value.
func OrderHealthTone(status string) string {
	switch status {
	case "undercut_station":
		return "bad"
	case "undercut_region":
		return "warn"
	case "best", "best_region_cheaper":
		return "good"
	}
	return ""
}

// HeatKey keys a stored station heat count: one station, one item,
// one side.
type HeatKey struct {
	LocationID, TypeID int64
	Buy                bool
}

// StationHeat reads how often each listed station's best price was
// beaten in the trailing 12 hours (schema 120), in one stored read. A
// pair with no stored row had no beats and reads as zero.
func StationHeat(ctx context.Context, q *db.Queries, locations []int64) map[HeatKey]int64 {
	beats := make(map[HeatKey]int64)
	if len(locations) == 0 {
		return beats
	}
	rows, err := q.ListMarketStationHeatByLocations(ctx, locations)
	if err != nil {
		logging.Errorf("orders: station heat: %v", err)
		return beats
	}
	for _, h := range rows {
		beats[HeatKey{h.LocationID, h.TypeID, h.IsBuyOrder == 1}] = h.Beats12h
	}
	return beats
}

// HeatWord names how competitive an item is at a station, from how
// often the station's best price on that side was beaten in the
// trailing 12 hours.
func HeatWord(beats int64) string {
	switch {
	case beats <= 0:
		return "Quiet"
	case beats <= 2:
		return "Warm"
	case beats <= 5:
		return "Hot"
	default:
		return "Very hot"
	}
}

// HeatTone is HeatWord as a class name: quiet, warm, hot, very-hot.
func HeatTone(word string) string {
	return strings.ReplaceAll(strings.ToLower(word), " ", "-")
}

// HeatNote is the count behind the heat word, in plain words.
func HeatNote(beats int64) string {
	switch beats {
	case 0:
		return "Best price not beaten in the last 12 hours"
	case 1:
		return "Best price beaten once in the last 12 hours"
	default:
		return fmt.Sprintf("Best price beaten %d times in the last 12 hours", beats)
	}
}

// LeftPct is the share of an order still listed, 0 to 100, for its
// meter.
func LeftPct(remain, total int64) int {
	if total <= 0 || remain <= 0 {
		return 0
	}
	if remain >= total {
		return 100
	}
	return int(remain * 100 / total)
}

// TimeLeft phrases the time until t: "in 83d 4h", "in 5h 12m", or
// "Expired" once it has passed.
func TimeLeft(t, now time.Time) string {
	if !t.After(now) {
		return "Expired"
	}
	return "in " + humanize.Duration(t.Sub(now))
}

// ShortDate writes a time as pages show a date beside a countdown:
// "2026-11-18".
func ShortDate(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}
