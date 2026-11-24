package app

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// netWorthDay is one sampled day, summed across the user's characters:
// the sampler records, per character and day, the ISK balance and a
// net worth that adds priced assets and buy-order escrow.
type netWorthDay struct {
	At     time.Time
	ISK    float64 // wallet balances
	Assets float64 // total minus ISK: priced assets and buy-order escrow
}

func (d netWorthDay) Total() float64 { return d.ISK + d.Assets }

// The KPI sparkline's box: viewBox units, polyline points inset so
// the stroke is never clipped.
const (
	sparkWidth  = 100.0
	sparkHeight = 28.0
	sparkPad    = 2.0
)

// sparklineSVG shapes daily totals into an inline SVG sparkline: a
// polyline and its soft area fill, in viewBox coordinates. ok=false
// under two days, no line is drawn at all, never a placeholder.
func sparklineSVG(totals []float64) (line, area string, ok bool) {
	if len(totals) < 2 {
		return "", "", false
	}
	lo, hi := totals[0], totals[0]
	for _, v := range totals {
		lo = math.Min(lo, v)
		hi = math.Max(hi, v)
	}
	span := hi - lo
	pts := make([]string, 0, len(totals))
	for i, v := range totals {
		x := sparkPad + float64(i)*(sparkWidth-2*sparkPad)/float64(len(totals)-1)
		y := sparkHeight - sparkPad
		if span > 0 {
			y -= (v - lo) / span * (sparkHeight - 2*sparkPad)
		} else {
			y = sparkHeight / 2
		}
		pts = append(pts, fmt.Sprintf("%.1f,%.1f", x, y))
	}
	line = strings.Join(pts, " ")
	area = line + fmt.Sprintf(" %.1f,%.1f %.1f,%.1f", sparkWidth-sparkPad, sparkHeight, sparkPad, sparkHeight)
	return line, area, true
}
