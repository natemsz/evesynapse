package intelchat

import (
	"testing"
	"time"
)

var systems = map[string]struct {
	id   int64
	name string
}{
	"1dq1-a":       {30004759, "1DQ1-A"},
	"old man star": {30005003, "Old Man Star"},
	"jita":         {30000142, "Jita"},
	"t5zi-s":       {30004755, "T5ZI-S"},
}

func lookup(n string) (int64, string, bool) {
	s, ok := systems[n]
	return s.id, s.name, ok
}

func TestParse(t *testing.T) {
	now := time.Date(2026, 10, 21, 11, 30, 0, 0, time.UTC)
	raw := "[ 2026.10.21 11:20:05 ] Some Scout > 1DQ1-A  Hostile One  Hostile Two +3 nv\r\n" +
		"[ 11:25:00 ] Other Scout > old man star clr\n" +
		"[ 23:59:00 ] Late Scout > Jita 5x\n" +
		"Scout Three > t5zi-s status?\n" +
		"[ 11:26:00 ] EVE System > Channel changed to Local : Jita\n" +
		"Scout Four > nothing to see here\n" +
		"T5ZI-S, Bad Guy\n" +
		"[ 11:27:00 ] Scout Five > Jita 5 reds\n" +
		"Scout Six > 1DQ1-A 12 neuts docked\n"
	o := Parse(raw, lookup, now)
	if len(o.Reports) != 6 || o.Questions != 1 || o.NoSystem != 1 {
		t.Fatalf("reports %d, questions %d, no system %d: %+v", len(o.Reports), o.Questions, o.NoSystem, o.Reports)
	}
	if len(o.NoSystemLines) != 1 || o.NoSystemLines[0] != "Scout Four > nothing to see here" {
		t.Errorf("no-system lines kept as written: %q", o.NoSystemLines)
	}
	r := o.Reports[0]
	if r.Speaker != "Some Scout" || r.SystemID != 30004759 || r.Hostiles != 3 || !r.NoVisual || r.Text != "Hostile One  Hostile Two" { // the gap names were pasted with is kept
		t.Errorf("first: %+v", r)
	}
	if !r.At.Equal(time.Date(2026, 10, 21, 11, 20, 5, 0, time.UTC)) {
		t.Errorf("first time: %v", r.At)
	}
	if c := o.Reports[1]; c.SystemName != "Old Man Star" || !c.Clear || c.Text != "" {
		t.Errorf("clear: %+v", c)
	}
	if l := o.Reports[2]; l.Hostiles != 5 || !l.At.Equal(time.Date(2026, 10, 20, 23, 59, 0, 0, time.UTC)) {
		t.Errorf("a time of day still ahead is yesterday's: %+v", l)
	}
	if b := o.Reports[3]; b.SystemID != 30004755 || b.Speaker != "" || !b.At.IsZero() || b.Text != "Bad Guy" {
		t.Errorf("bare line: %+v", b)
	}
	if n := o.Reports[4]; n.SystemID != 30000142 || n.Hostiles != 5 || n.Text != "" {
		t.Errorf("bare count before who they are: %+v", n)
	}
	if n := o.Reports[5]; n.SystemID != 30004759 || n.Hostiles != 12 || n.Text != "docked" {
		t.Errorf("bare count, rest kept: %+v", n)
	}
}
