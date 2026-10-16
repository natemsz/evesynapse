package standing

import "testing"

func sample() Standing {
	st := New()
	st.Working = true
	st.Corps[100] = true
	st.Corps[200] = true
	st.Alliance[100] = 9000 // corporation 100 is in alliance 9000
	st.Alliances[9000] = true
	st.CEOOf[100] = true
	st.EVERoles[200] = map[string]bool{"accountant": true}
	st.Groups[7] = true
	return st
}

func TestParseOwnerAndKey(t *testing.T) {
	o, ok := ParseOwner("corporation:98000001")
	if !ok || o.Kind != OwnerCorporation || o.ID != 98000001 || o.Key() != "corporation:98000001" {
		t.Fatalf("round trip: %+v %v", o, ok)
	}
	for _, bad := range []string{"", "corporation", "corporation:0", "faction:5", "alliance:x", "corporation:-1"} {
		if _, ok := ParseOwner(bad); ok {
			t.Errorf("ParseOwner(%q) accepted", bad)
		}
	}
	if !Includes([]Owner{{OwnerAlliance, 1}, o}, o) || Includes(nil, o) {
		t.Error("Includes")
	}
}

func TestMeets(t *testing.T) {
	st := sample()
	corp100 := Owner{Kind: OwnerCorporation, ID: 100}
	alliance := Owner{Kind: OwnerAlliance, ID: 9000}
	cases := []struct {
		name  string
		owner Owner
		rule  Rule
		want  bool
	}{
		{"linked is everyone working", corp100, Rule{Kind: RuleLinked}, true},
		{"member of its corporation", corp100, Rule{Kind: RuleMember}, true},
		{"member of an unrelated corporation", Owner{Kind: OwnerCorporation, ID: 300}, Rule{Kind: RuleMember}, false},
		{"member of the alliance through a corporation", alliance, Rule{Kind: RuleMember}, true},
		{"ceo of its corporation", corp100, Rule{Kind: RuleCEO}, true},
		{"ceo of a corporation of the alliance", alliance, Rule{Kind: RuleCEO}, true},
		{"corp rule inside the owner", alliance, Rule{Kind: RuleCorp, Ref: "100"}, true},
		{"corp rule for a corporation outside the alliance", alliance, Rule{Kind: RuleCorp, Ref: "200"}, false},
		{"eve role held in the owner corporation", Owner{Kind: OwnerCorporation, ID: 200}, Rule{Kind: RuleEVERole, Ref: "Accountant"}, true},
		{"eve role held elsewhere", corp100, Rule{Kind: RuleEVERole, Ref: "Accountant"}, false},
		{"group the account is in", corp100, Rule{Kind: RuleGroup, Ref: "7"}, true},
		{"group it is not in", corp100, Rule{Kind: RuleGroup, Ref: "8"}, false},
		{"unknown kind", corp100, Rule{Kind: "bogus"}, false},
	}
	for _, c := range cases {
		if got := st.Meets(c.owner, c.rule); got != c.want {
			t.Errorf("%s: Meets = %v, want %v", c.name, got, c.want)
		}
	}
	if (Standing{}).Meets(corp100, Rule{Kind: RuleLinked}) {
		t.Error("an account with no working character met a rule")
	}
}

func TestDirectsBelongsInCorporation(t *testing.T) {
	st := sample()
	if !st.Directs(100) || st.Directs(200) {
		t.Errorf("Directs: ceo of 100 %v, accountant of 200 %v", st.Directs(100), st.Directs(200))
	}
	st.EVERoles[200]["director"] = true
	if !st.Directs(200) {
		t.Error("a director directs")
	}
	if !st.Belongs(Owner{Kind: OwnerAlliance, ID: 9000}) || st.Belongs(Owner{Kind: OwnerAlliance, ID: 1}) {
		t.Error("Belongs by alliance")
	}
	if !st.InCorporation(200) || st.InCorporation(300) {
		t.Error("InCorporation")
	}
	if !HoldsDirector([]string{"Accountant", "director"}) || HoldsDirector([]string{"Accountant"}) {
		t.Error("HoldsDirector")
	}
	if !ValidEVECorpRole("Station_Manager") || ValidEVECorpRole("Janitor") {
		t.Error("ValidEVECorpRole")
	}
}
