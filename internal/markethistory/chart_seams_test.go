package markethistory

import (
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
)

// TestBuildPriceChartShapes: flat, rising, single-row and
// zero-volume histories draw the shapes they should.
func TestBuildPriceChartShapes(t *testing.T) {
	if _, ok := BuildPriceChart(nil); ok {
		t.Fatal("empty rows: got ok, want false")
	}

	flat := []db.MarketHistory{
		shapeRow("2026-09-29", 10), shapeRow("2026-09-30", 10), shapeRow("2026-10-01", 10),
	}
	chart, ok := BuildPriceChart(flat)
	if !ok {
		t.Fatal("flat: chart not built")
	}
	if chart.Points == "" {
		t.Fatal("flat: empty polyline")
	}
	y0 := chart.Dots[0].Y
	for _, d := range chart.Dots {
		if d.Y != y0 {
			t.Fatalf("flat: dot y %d, want all %d", d.Y, y0)
		}
		if d.Y <= chart.TopY || d.Y >= chart.BaseY {
			t.Fatalf("flat: dot y %d outside band (%d..%d)", d.Y, chart.TopY, chart.BaseY)
		}
	}
	if len(chart.Bars) != 3 {
		t.Fatalf("flat: got %d volume bars, want 3", len(chart.Bars))
	}

	rising := []db.MarketHistory{shapeRow("2026-09-30", 5), shapeRow("2026-10-01", 15)}
	chart, _ = BuildPriceChart(rising)
	if chart.Dots[0].Y != chart.BaseY || chart.Dots[1].Y != chart.TopY {
		t.Fatalf("rising: dots at (%d, %d), want (%d, %d)",
			chart.Dots[0].Y, chart.Dots[1].Y, chart.BaseY, chart.TopY)
	}

	single := []db.MarketHistory{shapeRow("2026-10-01", 42)}
	chart, ok = BuildPriceChart(single)
	if !ok || chart.Points != "" || len(chart.Dots) != 1 {
		t.Fatalf("single: ok=%v points=%q dots=%d, want a lone dot", ok, chart.Points, len(chart.Dots))
	}

	noVolume := []db.MarketHistory{
		{RegionID: 10000002, TypeID: 34, Date: "2026-09-30", Average: 5},
		{RegionID: 10000002, TypeID: 34, Date: "2026-10-01", Average: 6},
	}
	chart, _ = BuildPriceChart(noVolume)
	if len(chart.Bars) != 0 {
		t.Fatalf("zero volume: got %d bars, want 0", len(chart.Bars))
	}
	if chart.From != "2026-09-30" || chart.To != "2026-10-01" {
		t.Fatalf("labels: %s → %s", chart.From, chart.To)
	}
}

// ---------------------------------------------------------------------------
// Chart data + trader math (pure)
// ---------------------------------------------------------------------------
func tradeRow(date string, avg, high, low float64, vol int64) db.MarketHistory {
	return db.MarketHistory{RegionID: 10000002, TypeID: 34, Date: date, Average: avg, Highest: high, Lowest: low, Volume: vol, OrderCount: 10}
}

func TestBuildPriceChartCarriesDayData(t *testing.T) {
	rows := []db.MarketHistory{
		tradeRow("2026-09-30", 10.0, 11.0, 9.0, 123456),
		tradeRow("2026-10-01", 10.5, 11.5, 9.5, 100000),
		tradeRow("2026-10-02", 10.2, 11.2, 9.2, 90000),
	}
	chart, ok := BuildPriceChart(rows)
	if !ok {
		t.Fatal("chart: ok=false with rows")
	}
	if len(chart.Dots) != 3 {
		t.Fatalf("dots: got %d, want 3", len(chart.Dots))
	}
	last := chart.Dots[2]
	if last.Date != "2026-10-02" || last.Average != "10.20" || last.Highest != "11.20" || last.Lowest != "9.20" || last.Volume != "90,000" {
		t.Fatalf("dot day data: %+v", last.Day)
	}
	if !strings.Contains(last.Title, "2026-10-02") || !strings.Contains(last.Title, "10.20") {
		t.Fatalf("dot title: %q", last.Title)
	}
	if len(chart.Bars) != 3 || !strings.Contains(chart.Bars[0].Title, "volume 123,456") {
		t.Fatalf("bars: %+v", chart.Bars)
	}
	// Axes: price carries max/mid/min in compact ISK, volume
	// carries max/mid/zero on its own scale, and the x axis is
	// anchored at the first, middle, and last recorded days.
	if len(chart.PriceTicks) != 3 ||
		chart.PriceTicks[0].Label != "10.5 ISK" ||
		chart.PriceTicks[1].Label != "10.25 ISK" ||
		chart.PriceTicks[2].Label != "10 ISK" {
		t.Fatalf("price ticks: %+v", chart.PriceTicks)
	}
	if len(chart.VolumeTicks) != 3 ||
		chart.VolumeTicks[0].Label != "123K" ||
		chart.VolumeTicks[1].Label != "61.7K" ||
		chart.VolumeTicks[2].Label != "0" {
		t.Fatalf("volume ticks: %+v", chart.VolumeTicks)
	}
	if len(chart.DateTicks) != 3 ||
		chart.DateTicks[0].Label != "2026-09-30" || chart.DateTicks[0].Anchor != "start" ||
		chart.DateTicks[1].Label != "2026-10-01" || chart.DateTicks[1].Anchor != "middle" ||
		chart.DateTicks[2].Label != "2026-10-02" || chart.DateTicks[2].Anchor != "end" ||
		chart.DateTicks[1].X <= chart.DateTicks[0].X || chart.DateTicks[2].X <= chart.DateTicks[1].X {
		t.Fatalf("date ticks: %+v", chart.DateTicks)
	}
	// Recent days: newest first, same figures.
	if len(chart.Recent) != 3 || chart.Recent[0].Date != "2026-10-02" || chart.Recent[2].Date != "2026-09-30" {
		t.Fatalf("recent: %+v", chart.Recent)
	}

	// The recent table caps at RecentChartDays.
	var many []db.MarketHistory
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	for i := range 30 {
		many = append(many, tradeRow(base.AddDate(0, 0, i).Format(DateLayout), float64(100+i), 0, 0, 100))
	}
	chart, _ = BuildPriceChart(many)
	if len(chart.Recent) != RecentChartDays {
		t.Fatalf("recent cap: got %d, want %d", len(chart.Recent), RecentChartDays)
	}
	if chart.Recent[0].Date != "2026-08-30" {
		t.Fatalf("recent newest: %q, want 2026-08-30", chart.Recent[0].Date)
	}
}

func shapeRow(date string, avg float64) db.MarketHistory {
	return db.MarketHistory{RegionID: 10000002, TypeID: 34, Date: date, Average: avg, Highest: avg, Lowest: avg, Volume: 1000, OrderCount: 10}
}

// BuildPriceChart is the full-history chart with the volume strip,
// the shape the tests above read.
func BuildPriceChart(rows []db.MarketHistory) (PriceChart, bool) {
	return buildChart(rows, len(rows), ChartOptions{VolumeStrip: true}, true)
}
