package publicfits

import (
	"strings"
	"testing"

	"evesynapse/internal/app/module"
)

// publicFitURL pins the browser addresses the filter bar builds:
// defaults omitted (sort=new, faction=any, any age), empty values
// dropped, page resets unless kept.
func TestPublicFitURL(t *testing.T) {
	blank := publicFitFilters{Sort: publicFitDefaultSort, Faction: "any", Page: 1}
	if got := blank.publicFitURL(map[string]string{}, false); got != module.PublicFitsPath {
		t.Errorf("blank filters = %q, want %q", got, module.PublicFitsPath)
	}

	full := publicFitFilters{
		Q: "rift", Role: "Logistics", Hull: "Frigate", HullQ: "rif",
		IncludeName: "Medium Shield Extender I", ExcludeName: "Warp Disruptor",
		Faction: "none", Tag: "pvp", Sort: "rank", SinceDays: 14,
		MinStars: 4.5, MaxPrice: 1000000, Alpha: true, Page: 3,
	}
	kept := module.PublicFitsPath + "?alpha=1&exclude=Warp+Disruptor&faction=none&hull=Frigate" +
		"&hullq=rif&include=Medium+Shield+Extender+I&maxprice=1000000&page=3" +
		"&q=rift&role=Logistics&sort=rank&stars=4.5&tag=pvp&updated=14"
	if got := full.publicFitURL(map[string]string{}, true); got != kept {
		t.Errorf("full filters keepPage:\n got %q\nwant %q", got, kept)
	}

	// Page resets unless kept or explicitly changed.
	reset := module.PublicFitsPath + "?alpha=1&exclude=Warp+Disruptor&faction=none&hull=Frigate" +
		"&hullq=rif&include=Medium+Shield+Extender+I&maxprice=1000000" +
		"&q=rift&role=Logistics&sort=rank&stars=4.5&tag=pvp&updated=14"
	if got := full.publicFitURL(map[string]string{}, false); got != reset {
		t.Errorf("page reset:\n got %q\nwant %q", got, reset)
	}
	if got := full.publicFitURL(map[string]string{"page": "2"}, true); !strings.Contains(got, "page=2") {
		t.Errorf("page change = %q, want page=2", got)
	}

	// Changes override; empty changes drop the choice and
	// defaults stay omitted even when named.
	changed := full.publicFitURL(map[string]string{
		"sort": publicFitDefaultSort, "faction": "any", "q": "", "tag": "solo", "updated": "",
	}, false)
	for _, dropped := range []string{"sort=", "faction=", "q=rift", "tag=pvp", "updated="} {
		if strings.Contains(changed, dropped) {
			t.Errorf("changed URL %q keeps %q", changed, dropped)
		}
	}
	if !strings.Contains(changed, "tag=solo") {
		t.Errorf("changed URL %q misses tag=solo", changed)
	}
}
