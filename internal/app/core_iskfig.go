package app

import (
	"html/template"
	"strconv"
	"strings"
)

// The house money figure (the owner's format, 2026-10-30): a
// formatted ISK amount renders as its grouped integer at full
// size with the cents as a superscript, and no unit — the card
// label or column header on the surface carries the unit. The
// full figure with its unit stays available to assistive tech in
// a visually-hidden span, and data-v carries the plain numeric
// value for the phone compaction pass in app.js. Input is
// esi.FormatISK output; anything that is not a plain formatted
// figure renders unchanged.

// splitISKFigure splits "10,059,808,708,475.29" into its whole
// and cents parts.
func splitISKFigure(s string) (whole, cents string, ok bool) {
	i := strings.LastIndexByte(s, '.')
	if i <= 0 || len(s)-i != 3 {
		return "", "", false
	}
	whole, cents = s[:i], s[i+1:]
	for j := 0; j < len(cents); j++ {
		if cents[j] < '0' || cents[j] > '9' {
			return "", "", false
		}
	}
	for j := 0; j < len(whole); j++ {
		c := whole[j]
		if (c < '0' || c > '9') && c != ',' && !(j == 0 && c == '-') {
			return "", "", false
		}
	}
	return whole, cents, true
}

// iskLengthBucket grades a hero figure's size by the length of
// its integer part: 0 keeps the surface's plain size (the
// baseline for ordinary figures), and each higher bucket steps
// the size down. The thresholds leave room for the superscript
// cents tail, so the whole figure fits its card with slack.
func iskLengthBucket(intLen int) int {
	switch {
	case intLen <= 15:
		return 0
	case intLen <= 16:
		return 1
	case intLen <= 19:
		return 2
	case intLen <= 22:
		return 3
	default:
		return 4
	}
}

func iskFigure(formatted string, hero bool) template.HTML {
	whole, cents, ok := splitISKFigure(formatted)
	if !ok {
		return template.HTML(template.HTMLEscapeString(formatted))
	}
	cls := "iskfig"
	if hero {
		if b := iskLengthBucket(len(whole)); b > 0 {
			cls += " iskhero l" + strconv.Itoa(b)
		}
	}
	raw := strings.ReplaceAll(whole, ",", "") + "." + cents
	var b strings.Builder
	b.WriteString(`<span class="` + cls + `" data-v="` + raw + `"><span class="visually-hidden">`)
	b.WriteString(template.HTMLEscapeString(formatted + " ISK"))
	b.WriteString(`</span><span aria-hidden="true">`)
	b.WriteString(template.HTMLEscapeString(whole))
	b.WriteString(`<span class="dec">.`)
	b.WriteString(cents)
	b.WriteString(`</span></span></span>`)
	return template.HTML(b.String())
}

// iskFig renders a formatted ISK amount in the house money
// format at the surface's plain size.
func iskFig(formatted string) template.HTML { return iskFigure(formatted, false) }

// iskHero renders a formatted ISK amount in the house money
// format with the hero size steps applied.
func iskHero(formatted string) template.HTML { return iskFigure(formatted, true) }
