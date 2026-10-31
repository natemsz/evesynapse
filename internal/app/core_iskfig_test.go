package app

import (
	"strings"
	"testing"
)

func TestISKFigureFormat(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"1,234.56", `<span class="iskfig" data-v="1234.56"><span class="visually-hidden">1,234.56 ISK</span><span aria-hidden="true">1,234<span class="dec">.56</span></span></span>`},
		{"59,596,208,503.30", `<span class="iskfig" data-v="59596208503.30"><span class="visually-hidden">59,596,208,503.30 ISK</span><span aria-hidden="true">59,596,208,503<span class="dec">.30</span></span></span>`},
		{"-440,006,331.00", `<span class="iskfig" data-v="-440006331.00"><span class="visually-hidden">-440,006,331.00 ISK</span><span aria-hidden="true">-440,006,331<span class="dec">.00</span></span></span>`},
		{"0.00", `<span class="iskfig" data-v="0.00"><span class="visually-hidden">0.00 ISK</span><span aria-hidden="true">0<span class="dec">.00</span></span></span>`},
	}
	for _, c := range cases {
		if got := string(iskFig(c.in)); got != c.want {
			t.Errorf("iskFig(%q) =\n%s\nwant:\n%s", c.in, got, c.want)
		}
	}
}

func TestISKFigurePassthrough(t *testing.T) {
	for _, in := range []string{"", "—", "1.5", "12.345", "1,234", "abc", "1,234.5x"} {
		if got := string(iskFig(in)); got != in {
			t.Errorf("iskFig(%q) = %q, want the input unchanged", in, got)
		}
	}
}

func TestISKHeroLengthSteps(t *testing.T) {
	// 15 integer characters is still the baseline: no step class.
	if got := string(iskHero("172,068,853,681.84")); strings.Contains(got, "iskhero") {
		t.Errorf("15-char figure got a size step: %s", got)
	}
	// 18 characters steps to l2; 21 to l3.
	if got := string(iskHero("10,059,808,708,475.29")); !strings.Contains(got, `class="iskfig iskhero l2"`) {
		t.Errorf("10T figure missing the l2 step: %s", got)
	}
	if got := string(iskHero("9,999,999,999,999,999.99")); !strings.Contains(got, `class="iskfig iskhero l3"`) {
		t.Errorf("21-char figure missing the l3 step: %s", got)
	}
}
