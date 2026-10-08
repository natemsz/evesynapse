package markethistory

// First tests for the pure history math: the change figures, the
// axis formatting, the chart geometry, and the trader snapshot.
// Everything here is a pure function over stored rows, so the
// tests are too — no database, no network.

import (
	"strings"
	"testing"

	db "evesynapse/internal/db/sqlc"
)

func historyRows(days ...string) []db.MarketHistory {
	rows := make([]db.MarketHistory, len(days))
	for i, day := range days {
		rows[i] = db.MarketHistory{
			RegionID: 10000002, TypeID: 34, Date: day,
			Average: 100 + float64(i)*10, Highest: 110 + float64(i)*10,
			Lowest: 90 + float64(i)*10, Volume: 1000 + int64(i)*100,
			OrderCount: 50,
		}
	}
	return rows
}

func TestChangePct(t *testing.T) {
	rows := historyRows(
		"2026-01-01", "2026-01-02", "2026-01-03", "2026-01-04",
		"2026-01-05", "2026-01-06", "2026-01-07", "2026-01-08",
	)
	// Latest is 170; the day nearest 7 back (Jan 1, avg 100) is
	// the base: +70%.
	pct, ok := ChangePct(rows, 7)
	if !ok || pct < 69.99 || pct > 70.01 {
		t.Fatalf("ChangePct 7-day = %v, %v; want ~70, true", pct, ok)
	}
	// Too little history, and nothing near the target, both
	// report false rather than inventing a figure.
	if _, ok := ChangePct(rows[:1], 7); ok {
		t.Fatal("one row reported a change")
	}
	// Jan 1 and Jan 8 only, asking for 3 days: the target is Jan
	// 5, four days from the only candidate — past tolerance.
	sparse := []db.MarketHistory{rows[0], rows[len(rows)-1]}
	if _, ok := ChangePct(sparse, 3); ok {
		t.Fatal("a change bridged a hole past tolerance")
	}
}

func TestFormatChange(t *testing.T) {
	if got := FormatChangePct(8.24); got != "+8.2%" {
		t.Fatalf("FormatChangePct(8.24) = %q", got)
	}
	if got := FormatChangePct(-3.04); got != "-3.0%" {
		t.Fatalf("FormatChangePct(-3.04) = %q", got)
	}
	if got := ChangeDirection(8.2); got != "up 8.2%" {
		t.Fatalf("ChangeDirection(8.2) = %q", got)
	}
	if got := ChangeDirection(-3); got != "down 3.0%" {
		t.Fatalf("ChangeDirection(-3) = %q", got)
	}
}

func TestFormatCompactAxisNumber(t *testing.T) {
	for in, want := range map[float64]string{
		950: "950", 12345: "12.3K", 123456: "123K",
		3400000: "3.4M", 2500000000: "2.5B", 1500000000000: "1.5T",
	} {
		if got := FormatCompactAxisNumber(in); got != want {
			t.Errorf("FormatCompactAxisNumber(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestSummarize(t *testing.T) {
	if _, ok := Summarize(nil); ok {
		t.Fatal("no rows summarized")
	}
	stats, ok := Summarize(historyRows("2026-01-01", "2026-01-02", "2026-01-03"))
	if !ok {
		t.Fatal("three rows did not summarize")
	}
	if stats.Days != 3 || stats.LastDay != "2026-01-03" {
		t.Fatalf("summary = %+v", stats)
	}
	for name, field := range map[string]string{"Min": stats.Min, "Max": stats.Max, "Mean": stats.Mean, "Latest": stats.Latest} {
		if field == "" {
			t.Errorf("%s rendered empty", name)
		}
	}
}

func TestBuildPriceChart(t *testing.T) {
	if _, ok := BuildPriceChart(nil); ok {
		t.Fatal("no rows charted")
	}
	// One row: a lone centered dot, no polyline, one date tick.
	one, ok := BuildPriceChart(historyRows("2026-01-05"))
	if !ok {
		t.Fatal("one row did not chart")
	}
	if one.Points != "" {
		t.Fatalf("one row drew a polyline %q", one.Points)
	}
	if len(one.Dots) != 1 || one.Dots[0].X != ChartWidth/2 {
		t.Fatalf("one row dots = %+v", one.Dots)
	}
	if len(one.Bars) != 1 || len(one.DateTicks) != 1 || one.From != one.To {
		t.Fatalf("one row chart = %+v", one)
	}

	// Several rows: a polyline through every dot, one bar per
	// day with volume, newest-first recents.
	rows := historyRows("2026-01-01", "2026-01-02", "2026-01-03", "2026-01-04", "2026-01-05")
	chart, ok := BuildPriceChart(rows)
	if !ok {
		t.Fatal("five rows did not chart")
	}
	if len(chart.Dots) != 5 || len(strings.Fields(chart.Points)) != 5 {
		t.Fatalf("dots = %d, polyline points = %q", len(chart.Dots), chart.Points)
	}
	if len(chart.Bars) != 5 {
		t.Fatalf("bars = %d, want one per day", len(chart.Bars))
	}
	if chart.From != "2026-01-01" || chart.To != "2026-01-05" {
		t.Fatalf("labels span %q to %q", chart.From, chart.To)
	}
	if len(chart.Recent) != 5 || chart.Recent[0].Date != "2026-01-05" {
		t.Fatalf("recents = %+v, want newest first", chart.Recent)
	}
	if len(chart.PriceTicks) != 3 || len(chart.VolumeTicks) != 3 {
		t.Fatalf("ticks = %d price, %d volume", len(chart.PriceTicks), len(chart.VolumeTicks))
	}
	// A zero-volume day has no bar but still has its dot.
	rows[2].Volume = 0
	chart, _ = BuildPriceChart(rows)
	if len(chart.Bars) != 4 || len(chart.Dots) != 5 {
		t.Fatalf("zero-volume day: %d bars, %d dots", len(chart.Bars), len(chart.Dots))
	}
}

func TestWindow(t *testing.T) {
	if _, _, ok := Window(nil, 7); ok {
		t.Fatal("no rows windowed")
	}
	rows := historyRows("2026-01-01", "2026-01-02", "2026-01-03", "2026-01-04", "2026-01-05")
	price, vol, ok := Window(rows, 3)
	if !ok {
		t.Fatal("three-day window did not compute")
	}
	// Newest three days: averages 120, 130, 140; volumes 1200, 1300, 1400.
	if price != 130 || vol != 1300 {
		t.Fatalf("window = (%v, %v), want (130, 1300)", price, vol)
	}
}

func TestBuildTraderStats(t *testing.T) {
	stats := BuildTraderStats(historyRows("2026-01-01", "2026-01-02"), 110, 100)
	if stats.Avg7 == "" || stats.Avg30 == "" || stats.AvgVol7 == "" {
		t.Fatalf("snapshot = %+v, want computed windows", stats)
	}
	if stats.MarginPct != "9.1%" {
		t.Fatalf("margin = %q, want 9.1%%", stats.MarginPct)
	}
	// No live book: the margin stays blank, never invented.
	if stats := BuildTraderStats(historyRows("2026-01-01"), 0, 0); stats.MarginPct != "" {
		t.Fatalf("margin without a book = %q", stats.MarginPct)
	}
}
