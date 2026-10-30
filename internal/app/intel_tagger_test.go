package app

import (
	"reflect"
	"testing"
)

// The embedded classifier loads, INTEL_TAGGER=off keeps it out, and
// a model file that is not there leaves reading as it was.
func TestIntelTaggerLoads(t *testing.T) {
	if (&Application{}).intelTagger() == nil {
		t.Fatal("the embedded model did not load")
	}
	if (&Application{cfg: Config{intelTaggerOff: true}}).intelTagger() != nil {
		t.Error("INTEL_TAGGER=off still loaded the model")
	}
	if (&Application{cfg: Config{intelTaggerPath: "no/such/model.gob.gz"}}).intelTagger() != nil {
		t.Error("a missing model file loaded something")
	}
}

func TestSiteLexicon(t *testing.T) {
	lex := siteLexicon{
		systems: func(l string) (int64, string, bool) { return 30004759, "1DQ1-A", l == "1dq1-a" },
		ships:   map[string]intelShip{"sacrilege": {id: 12019, name: "Sacrilege"}},
		areas:   map[string]int64{"delve": 10000060},
		pilots:  map[string]bool{"kara venn": true},
		nicks:   intelShipNicks,
	}
	for p, want := range map[string][]string{
		"1dq1-a": {"system"}, "sacs": {"ship"}, "sacrilege": {"ship"},
		"delve": {"area"}, "kara venn": {"pilot"}, "help": nil,
	} {
		if got := lex.Kinds(p); !reflect.DeepEqual(got, want) {
			t.Errorf("%q: %v, want %v", p, got, want)
		}
	}
}
