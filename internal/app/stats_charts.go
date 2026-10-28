package app

import (
	"fmt"
	"html/template"
	"strings"
)

// ---------------------------------------------------------------------------
// SVG chart builders for the Admin Analytics section. Pure string
// building over the stored aggregates the handler read; every
// fill and stroke is a CSS class from admin.css, so the charts
// follow the account's color scheme like the rest of the page.
// No chart ever carries a hex color.
// ---------------------------------------------------------------------------

func svgF(v float64) string { return fmt.Sprintf("%.1f", v) }

// stackedBarsSVG renders one stacked bar per bucket: series is
// [class][bucket] counts, classes the CSS class per class in
// stack order (bottom first).
func stackedBarsSVG(series [][]int64, classes []string, width, height float64) template.HTML {
	if len(series) == 0 || len(series[0]) == 0 {
		return ""
	}
	buckets := len(series[0])
	var maxTotal int64
	totals := make([]int64, buckets)
	for c := range series {
		for b, n := range series[c] {
			totals[b] += n
		}
	}
	for _, t := range totals {
		if t > maxTotal {
			maxTotal = t
		}
	}
	if maxTotal == 0 {
		return ""
	}
	const topPad, bottomPad = 8.0, 10.0
	plotH := height - topPad - bottomPad
	slot := width / float64(buckets)
	barW := slot * 0.82
	var sb strings.Builder
	fmt.Fprintf(&sb, `<svg viewBox="0 0 %s %s" width="100%%" role="img">`, svgF(width), svgF(height))
	for _, frac := range []float64{0.25, 0.5, 0.75, 1} {
		y := topPad + plotH*(1-frac)
		fmt.Fprintf(&sb, `<line x1="0" y1="%s" x2="%s" y2="%s" class="an-grid"/>`, svgF(y), svgF(width), svgF(y))
	}
	for b := 0; b < buckets; b++ {
		x := float64(b)*slot + (slot-barW)/2
		y := topPad + plotH
		for c := range series {
			n := series[c][b]
			if n == 0 {
				continue
			}
			h := float64(n) / float64(maxTotal) * plotH
			y -= h
			fmt.Fprintf(&sb, `<rect x="%s" y="%s" width="%s" height="%s" class="%s"/>`,
				svgF(x), svgF(y), svgF(barW), svgF(h), classes[c])
		}
	}
	sb.WriteString(`</svg>`)
	return template.HTML(sb.String())
}

// areaChartSVG renders one filled line series. max scales the Y
// axis (callers pass a floor so a flat series still reads). When
// ref is non-nil a dashed reference line is drawn at that value
// with refLabel at its right end; topRight and bottomRight are
// corner captions (the axis max, the window note).
func areaChartSVG(values []float64, max float64, lineClass string, width, height float64, ref *float64, refLabel, topRight, bottomRight string) template.HTML {
	if len(values) == 0 || max <= 0 {
		return ""
	}
	const topPad, bottomPad = 14.0, 16.0
	plotH := height - topPad - bottomPad
	x := func(i int) float64 {
		if len(values) == 1 {
			return width / 2
		}
		return float64(i) * width / float64(len(values)-1)
	}
	y := func(v float64) float64 {
		if v > max {
			v = max
		}
		return topPad + plotH*(1-v/max)
	}
	var line, area strings.Builder
	for i, v := range values {
		verb := "L"
		if i == 0 {
			verb = "M"
		}
		fmt.Fprintf(&line, "%s%s,%s ", verb, svgF(x(i)), svgF(y(v)))
	}
	fmt.Fprintf(&area, "%sL%s,%s L0,%s Z", line.String(), svgF(width), svgF(topPad+plotH), svgF(topPad+plotH))
	var sb strings.Builder
	fmt.Fprintf(&sb, `<svg viewBox="0 0 %s %s" width="100%%" role="img">`, svgF(width), svgF(height))
	fmt.Fprintf(&sb, `<path d="%s" class="%s-fill"/>`, area.String(), lineClass)
	fmt.Fprintf(&sb, `<path d="%s" fill="none" class="%s"/>`, line.String(), lineClass)
	if ref != nil && *ref > 0 && *ref <= max {
		ry := y(*ref)
		fmt.Fprintf(&sb, `<line x1="0" y1="%s" x2="%s" y2="%s" class="an-ref"/>`, svgF(ry), svgF(width), svgF(ry))
		if refLabel != "" {
			fmt.Fprintf(&sb, `<text x="%s" y="%s" class="an-svg-note an-ref-t" text-anchor="end">%s</text>`,
				svgF(width-6), svgF(ry-5), template.HTMLEscapeString(refLabel))
		}
	}
	if topRight != "" {
		fmt.Fprintf(&sb, `<text x="%s" y="12" class="an-svg-note" text-anchor="end">%s</text>`,
			svgF(width-6), template.HTMLEscapeString(topRight))
	}
	if bottomRight != "" {
		fmt.Fprintf(&sb, `<text x="%s" y="%s" class="an-svg-note" text-anchor="end">%s</text>`,
			svgF(width-6), svgF(height-3), template.HTMLEscapeString(bottomRight))
	}
	sb.WriteString(`</svg>`)
	return template.HTML(sb.String())
}

// barsSVG renders simple vertical bars (growth).
func barsSVG(values []int64, class string, width, height float64) template.HTML {
	if len(values) == 0 {
		return ""
	}
	var max int64
	for _, v := range values {
		if v > max {
			max = v
		}
	}
	if max == 0 {
		return ""
	}
	const topPad, bottomPad = 6.0, 8.0
	plotH := height - topPad - bottomPad
	slot := width / float64(len(values))
	barW := slot * 0.78
	var sb strings.Builder
	fmt.Fprintf(&sb, `<svg viewBox="0 0 %s %s" width="100%%" role="img">`, svgF(width), svgF(height))
	for i, v := range values {
		if v == 0 {
			continue
		}
		h := float64(v) / float64(max) * plotH
		fmt.Fprintf(&sb, `<rect x="%s" y="%s" width="%s" height="%s" class="%s"/>`,
			svgF(float64(i)*slot+(slot-barW)/2), svgF(topPad+plotH-h), svgF(barW), svgF(h), class)
	}
	sb.WriteString(`</svg>`)
	return template.HTML(sb.String())
}
