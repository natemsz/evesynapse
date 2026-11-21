// Screen layout: picks the largest art that fits the terminal, and
// centers it and the content below it.
package main

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

const (
	defaultWidth  = 100
	defaultHeight = 32
	maxBodyWidth  = 96
)

// headerLayout is one way to draw the header.
type headerLayout struct {
	boltRows int  // 0 for no bolt
	word     int  // index into wordmarkSizes, -1 for plain text
	side     bool // bolt beside the wordmark instead of above it
	credits  bool // tagline and credits under the wordmark
}

// headerLayouts run from the most to the least generous. The first
// one that fits the terminal is used.
var headerLayouts = []headerLayout{
	{boltRows: 16, word: 2, credits: true},
	{boltRows: 14, word: 1, credits: true},
	{boltRows: 12, word: 1, credits: true},
	{boltRows: 10, word: 0, credits: true},
	{boltRows: 8, word: 1, side: true, credits: true},
	{boltRows: 8, word: 0, side: true, credits: true},
	{boltRows: 8, word: 0, credits: true},
	{boltRows: 8, word: 0},
	{word: 1, credits: true},
	{word: 0, credits: true},
	{word: 0},
	{boltRows: 8, word: -1},
	{word: -1, credits: true},
	{word: -1},
}

func (l headerLayout) wordSize() (cols, rows int) {
	if l.word < 0 {
		return len("EVESYNAPSE"), 1
	}
	s := wordmarkSizes[l.word]
	return s.cols, s.rows
}

// size is the layout's footprint in cells.
func (l headerLayout) size() (cols, rows int) {
	wc, wr := l.wordSize()
	textCols, textRows := wc, wr
	if l.credits {
		textCols = max(textCols, lipgloss.Width(tagline), lipgloss.Width(creditsLine))
		textRows += 3 // a blank line, the tagline, the credits
	}
	if l.boltRows == 0 {
		return textCols, textRows
	}
	bc := boltCols(l.boltRows)
	if l.side {
		return bc + 3 + textCols, max(l.boltRows, textRows)
	}
	return max(bc, textCols), l.boltRows + 1 + textRows
}

// pickHeader returns the most generous layout that fits.
func pickHeader(width, maxRows int) (headerLayout, bool) {
	for _, l := range headerLayouts {
		c, r := l.size()
		if c <= width && r <= maxRows {
			return l, true
		}
	}
	return headerLayout{}, false
}

// renderHeader draws a layout, every line centered on its block.
func renderHeader(l headerLayout, frame int) string {
	var word []string
	if l.word >= 0 {
		word = cachedWordmark(wordmarkSizes[l.word])
	} else {
		word = []string{titleStyle.Render("EVESYNAPSE")}
	}
	text := strings.Join(word, "\n")
	if l.credits {
		text = lipgloss.JoinVertical(lipgloss.Center, text, "",
			mutedStyle.Render(tagline), mutedStyle.Render(creditsLine))
	}
	if l.boltRows == 0 {
		return text
	}
	bolt := strings.Join(cachedBolt(l.boltRows, frame), "\n")
	if l.side {
		return lipgloss.JoinHorizontal(lipgloss.Center, bolt, "   ", text)
	}
	return lipgloss.JoinVertical(lipgloss.Center, bolt, "", text)
}

// center places a block in the middle of a width.
func center(width int, s string) string {
	return lipgloss.PlaceHorizontal(width, lipgloss.Center, s)
}

// clipLines keeps at most n lines.
func clipLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if n >= 0 && len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// truncateCells shortens plain text to fit a width in cells.
func truncateCells(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= width {
		return s
	}
	r := []rune(s)
	for len(r) > 0 && lipgloss.Width(string(r))+1 > width {
		r = r[:len(r)-1]
	}
	return string(r) + "…"
}

// compose stacks the header over the body and centers the whole
// screen. The body is drawn first: the art takes what room is left.
func compose(width, height, frame int, body string) string {
	bodyRows := lipgloss.Height(body)
	room := height - bodyRows - 1
	out := body
	if l, ok := pickHeader(width, room); ok {
		out = center(width, renderHeader(l, frame)) + "\n\n" + body
	}
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, clipLines(out, height))
}
