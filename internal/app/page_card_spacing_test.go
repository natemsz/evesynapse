package app

import (
	"strings"
	"testing"
)

// TestStackedPageCardsCarryTheSectionGap pins the spacing rhythm
// of stacked page cards: the wrapper's stacked surfaces (.panel,
// .foldable) sit 1.25rem apart, and a page-card that follows
// another page-card keeps the same gap instead of sitting flush
// against it. A lone page-card carries no extra margin, and the
// phone band tightens the gap with the other surfaces.
func TestStackedPageCardsCarryTheSectionGap(t *testing.T) {
	style := readStatic(t, "style.css")
	for _, want := range []string{
		// The standard the stacked surfaces already keep.
		".page-body > .panel { margin-bottom: 1.25rem; }",
		// A page-card keeps its padding and gains no margin of
		// its own; the gap belongs to the following card only.
		".page-card { padding: 1rem 1.25rem; }",
		".page-body:not(:has(.home-grid)) .page-card + .page-card { margin-top: 1.25rem; }",
	} {
		if !strings.Contains(style, want) {
			t.Errorf("style.css missing stacked-card rule %q", want)
		}
	}
	phone := readStatic(t, "phone.css")
	if want := ".page-body:not(:has(.home-grid)) .page-card + .page-card { margin-top: 0.85rem; }"; !strings.Contains(phone, want) {
		t.Errorf("phone.css missing stacked-card rule %q", want)
	}
}

// TestBriefingCardsStackTheSideChipsOnPhones pins the phone
// layout of a briefing card's row: at phone widths the severity
// pill and tally sit on their own line under the text instead
// of sharing its row, where they squeezed the text into a
// column a few letters wide.
func TestBriefingCardsStackTheSideChipsOnPhones(t *testing.T) {
	style := readStatic(t, "style.css")
	for _, want := range []string{
		".brief-cards .brief-row { flex-direction: column; gap: 0.35rem; }",
		".brief-cards .brief-row-side { margin-left: 0; }",
	} {
		if !strings.Contains(style, want) {
			t.Errorf("style.css missing phone briefing-row rule %q", want)
		}
	}
}
