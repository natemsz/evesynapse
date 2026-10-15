package ideas

import (
	"fmt"
	"html/template"
	"reflect"
	"strings"
	"testing"
)

func TestTradeIdeasRenderMarkdown(t *testing.T) {
	groups := Parse("Some preamble\n" +
		"* Loose idea\n" +
		"### **High-Volume Scalping**\n" +
		"*Focus on low spreads.*\n" +
		"* **Tritanium**: 83.9B daily profit <script>alert(1)</script>\n" +
		"- Pyerite: steady\n" +
		"1. Mexallon: calm\n" +
		"### Empty Heading\n" +
		"### Margin Trading\n" +
		"* **Nocxium**: wide\n")
	if len(groups) != 3 {
		t.Fatalf("got %d groups, want 3 (preamble, scalping, margin; the empty one dropped): %+v", len(groups), groups)
	}
	if groups[0].Title != "Ideas" || len(groups[0].Ideas) != 1 || len(groups[0].Notes) != 1 {
		t.Errorf("lines before the first header: %+v", groups[0])
	}
	g := groups[1]
	if g.ID != "ti-2" || g.Title != "High-Volume Scalping" || g.Desc != "Focus on low spreads." {
		t.Errorf("strategy heading: %+v", g)
	}
	want := []template.HTML{
		"<strong>Tritanium</strong>: 83.9B daily profit &lt;script&gt;alert(1)&lt;/script&gt;",
		"Pyerite: steady",
		"Mexallon: calm",
	}
	if !reflect.DeepEqual(g.Ideas, want) {
		t.Errorf("ideas = %q, want %q", g.Ideas, want)
	}
	if g.More(1) != 2 || g.More(5) != 0 {
		t.Errorf("More(1) = %d, More(5) = %d", g.More(1), g.More(5))
	}
	if groups[2].ID != "ti-3" || groups[2].Title != "Margin Trading" {
		t.Errorf("last strategy: %+v", groups[2])
	}
	for i, g := range groups {
		if g.Accent != i+1 {
			t.Errorf("group %d accent = %d, want %d (accents rotate by place)", i, g.Accent, i+1)
		}
	}
}

func TestTradeIdeasAccentWrapsAfterSix(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 7; i++ {
		fmt.Fprintf(&b, "### Strategy %d\n* idea\n", i)
	}
	groups := Parse(b.String())
	if len(groups) != 7 {
		t.Fatalf("got %d groups, want 7", len(groups))
	}
	if groups[5].Accent != 6 || groups[6].Accent != 1 {
		t.Errorf("accents at the wrap: sixth = %d, seventh = %d, want 6 then 1", groups[5].Accent, groups[6].Accent)
	}
}
