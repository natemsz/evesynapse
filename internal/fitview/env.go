package fitview

import "fmt"

// A fit's system environment (FitDoc.Env): a wormhole or incursion
// effect beacon, or an abyssal weather. One at a time: a ship is in
// one system. The types are the SDE's own; the labels say what each
// is in plain words, the weather's from the warfare buffs it carries.

// FitEnvGroup is one family of environments for the picker.
type FitEnvGroup struct {
	Name    string
	Options []FitEnvOption
}

// FitEnvOption is one environment: its type and its label.
type FitEnvOption struct {
	TypeID int64
	Name   string
}

// fitWormholeBeacons are the wormhole effect beacons, class 1 to 6
// (SDE group 920, "Class N ... Effects").
var fitWormholeBeacons = []struct {
	name    string
	classes [6]int64
}{
	{"Black hole", [6]int64{30845, 30850, 30851, 30852, 30853, 30854}},
	{"Cataclysmic variable", [6]int64{30846, 30880, 30881, 30884, 30883, 30882}},
	{"Magnetar", [6]int64{30847, 30860, 30861, 30862, 30863, 30864}},
	{"Pulsar", [6]int64{30844, 30865, 30866, 30867, 30868, 30869}},
	{"Red giant", [6]int64{30848, 30870, 30871, 30872, 30873, 30874}},
	{"Wolf-Rayet", [6]int64{30849, 30875, 30876, 30877, 30878, 30879}},
}

// fitAbyssalWeather are the abyssal weathers at their three
// strengths (30%, 50% and 70% penalty), named for what they do.
var fitAbyssalWeather = []struct {
	name     string
	strength [3]int64
}{
	{"Electrical storm: EM resistance down, capacitor recharge faster", [3]int64{47381, 47382, 47383}},
	{"Dark matter: turret range down, speed up", [3]int64{47378, 47379, 47380}},
	{"Exotic particles: kinetic resistance down, scan resolution up", [3]int64{47384, 47385, 47386}},
	{"Gamma rays: explosive resistance down, shield HP up", [3]int64{47387, 47388, 47389}},
	{"Firestorm: thermal resistance down, armor HP up", [3]int64{47390, 47391, 47392}},
}

// fitIncursionBeacons are the Sansha incursion system effects.
var fitIncursionBeacons = []FitEnvOption{
	{3069, "Incursion, vanguard system"},
	{3493, "Incursion, assault system"},
	{3494, "Incursion, headquarters system"},
}

// FitEnvGroups is the picker's environments, by family.
func FitEnvGroups() []FitEnvGroup {
	var groups []FitEnvGroup
	for _, wh := range fitWormholeBeacons {
		g := FitEnvGroup{Name: "Wormhole: " + wh.name}
		for i, id := range wh.classes {
			g.Options = append(g.Options, FitEnvOption{id, fmt.Sprintf("%s, class %d", wh.name, i+1)})
		}
		groups = append(groups, g)
	}
	for _, w := range fitAbyssalWeather {
		g := FitEnvGroup{Name: "Abyssal weather"}
		if len(groups) > 0 && groups[len(groups)-1].Name == g.Name {
			g = groups[len(groups)-1]
			groups = groups[:len(groups)-1]
		}
		for i, id := range w.strength {
			g.Options = append(g.Options, FitEnvOption{id, fmt.Sprintf("%s, %d%%", w.name, 30+20*i)})
		}
		groups = append(groups, g)
	}
	groups = append(groups, FitEnvGroup{Name: "Incursion", Options: fitIncursionBeacons})
	return groups
}

// FitEnvName is an environment's label, "" for an unknown type.
func FitEnvName(typeID int64) string {
	for _, g := range FitEnvGroups() {
		for _, o := range g.Options {
			if o.TypeID == typeID {
				return o.Name
			}
		}
	}
	return ""
}
