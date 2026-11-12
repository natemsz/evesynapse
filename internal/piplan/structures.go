package piplan

// Planetary structures as the game's static data has them: the type
// ids differ per planet type (a Barren Launchpad is not an Ice
// Launchpad), the fitting does not. Values were read from the SDE's
// dogma attributes (cpuLoad 49, powerLoad 15, the extractor head
// pair 1690/1691, the link's per-km and per-level attributes
// 1633-1636 and logisticalCapacity 1631) on 2026-11-12.

// PinKind is a structure's role on a colony.
type PinKind string

const (
	PinCommandCenter PinKind = "cc"
	PinExtractor     PinKind = "ecu"
	PinBasic         PinKind = "basic"
	PinAdvanced      PinKind = "advanced"
	PinHighTech      PinKind = "hightech"
	PinStorage       PinKind = "storage"
	PinLaunchpad     PinKind = "launchpad"
)

// PinKinds in the order a colony lists them.
var PinKinds = []PinKind{PinCommandCenter, PinExtractor, PinBasic, PinAdvanced, PinHighTech, PinStorage, PinLaunchpad}

// PinLabel names a structure kind in plain words.
func PinLabel(k PinKind) string {
	switch k {
	case PinCommandCenter:
		return "Command Center"
	case PinExtractor:
		return "Extractor Control Unit"
	case PinBasic:
		return "Basic Industry Facility"
	case PinAdvanced:
		return "Advanced Industry Facility"
	case PinHighTech:
		return "High-Tech Production Plant"
	case PinStorage:
		return "Storage Facility"
	case PinLaunchpad:
		return "Launchpad"
	}
	return string(k)
}

// PinFit is one structure's CPU (tf) and power (MW) use.
type PinFit struct{ CPU, PG int64 }

// PinFits by kind. The command center uses nothing: it is the
// supply. Extractor heads add HeadFit each on top of the ECU.
var PinFits = map[PinKind]PinFit{
	PinCommandCenter: {0, 0},
	PinExtractor:     {400, 2600},
	PinBasic:         {200, 800},
	PinAdvanced:      {500, 700},
	PinHighTech:      {1100, 400},
	PinStorage:       {500, 700},
	PinLaunchpad:     {3600, 700},
}

// HeadFit is one extractor head's CPU and power.
var HeadFit = PinFit{110, 550}

// MaxHeads is the most heads one extractor control unit can run.
const MaxHeads = 10

// Storage capacities in m3.
var PinCapacity = map[PinKind]float64{
	PinCommandCenter: 500,
	PinStorage:       12000,
	PinLaunchpad:     10000,
}

// Link fitting: base plus a per-km share, scaled per upgrade level;
// bandwidth doubles with every level.
const (
	LinkCPU          = 15.0
	LinkPG           = 10.0
	LinkCPUPerKm     = 0.2
	LinkPGPerKm      = 0.15
	LinkCPULevelMod  = 1.4
	LinkPGLevelMod   = 1.2
	LinkBandwidth    = 1250.0 // m3 per hour at level 0
	MaxLinkLevel     = 10
	maxRouteStations = 7 // a template route through more structures is dropped on import
)

// PlanetTypeIDs maps a planet type to the SDE type id of its planet
// (the template's "Pln").
var PlanetTypeIDs = map[string]int64{
	"Temperate": 11, "Ice": 12, "Gas": 13, "Oceanic": 2014,
	"Lava": 2015, "Barren": 2016, "Storm": 2017, "Plasma": 2063,
}

// PlanetTypeByID is the reverse of PlanetTypeIDs.
var PlanetTypeByID = func() map[int64]string {
	m := map[int64]string{}
	for k, v := range PlanetTypeIDs {
		m[v] = k
	}
	return m
}()

// pinTypeIDs are the structure type ids per planet type. High-tech
// plants exist only on Barren and Temperate; every planet type has
// the rest. The command center id is the
// level-0 one; upgrades keep the pin.
var pinTypeIDs = map[string]map[PinKind]int64{
	"Barren":    {PinCommandCenter: 2524, PinExtractor: 2848, PinBasic: 2473, PinAdvanced: 2474, PinHighTech: 2475, PinStorage: 2541, PinLaunchpad: 2544},
	"Temperate": {PinCommandCenter: 2254, PinExtractor: 3068, PinBasic: 2481, PinAdvanced: 2480, PinHighTech: 2482, PinStorage: 2562, PinLaunchpad: 2256},
	"Gas":       {PinCommandCenter: 2534, PinExtractor: 3060, PinBasic: 2492, PinAdvanced: 2494, PinStorage: 2536, PinLaunchpad: 2543},
	"Ice":       {PinCommandCenter: 2533, PinExtractor: 3061, PinBasic: 2493, PinAdvanced: 2491, PinStorage: 2257, PinLaunchpad: 2552},
	"Lava":      {PinCommandCenter: 2549, PinExtractor: 3062, PinBasic: 2469, PinAdvanced: 2470, PinStorage: 2558, PinLaunchpad: 2555},
	"Oceanic":   {PinCommandCenter: 2525, PinExtractor: 3063, PinBasic: 2490, PinAdvanced: 2485, PinStorage: 2535, PinLaunchpad: 2542},
	"Plasma":    {PinCommandCenter: 2551, PinExtractor: 3064, PinBasic: 2471, PinAdvanced: 2472, PinStorage: 2560, PinLaunchpad: 2556},
	"Storm":     {PinCommandCenter: 2550, PinExtractor: 3067, PinBasic: 2483, PinAdvanced: 2484, PinStorage: 2561, PinLaunchpad: 2557},
}

// PinTypeID is the structure's type id on a planet type; ok is false
// when the planet cannot hold it (a high-tech plant on Gas).
func PinTypeID(planet string, k PinKind) (int64, bool) {
	id, ok := pinTypeIDs[planet][k]
	return id, ok
}

// commandCenterIDs are every command center type id, upgrades
// included: ESI reports a colony's center under its current level's
// id.
var commandCenterIDs = map[int64]bool{
	2129: true, 2130: true, 2131: true, 2132: true, 2133: true, 2134: true, 2135: true, 2136: true,
	2137: true, 2138: true, 2139: true, 2140: true, 2141: true, 2142: true, 2143: true, 2144: true,
	2145: true, 2146: true, 2147: true, 2148: true, 2149: true, 2150: true, 2151: true, 2152: true,
	2153: true, 2154: true, 2155: true, 2156: true, 2157: true, 2158: true, 2159: true, 2160: true,
	2254: true, 2524: true, 2525: true, 2533: true, 2534: true, 2549: true, 2550: true, 2551: true,
	2574: true, 2576: true, 2577: true, 2578: true, 2581: true, 2582: true, 2585: true, 2586: true,
}

// PinKindOf classifies a structure type id from any planet; ok is
// false for ids that are not colony structures.
func PinKindOf(typeID int64) (PinKind, bool) {
	if commandCenterIDs[typeID] {
		return PinCommandCenter, true
	}
	for _, kinds := range pinTypeIDs {
		for k, id := range kinds {
			if id == typeID {
				return k, true
			}
		}
	}
	return "", false
}

// CanHold reports whether a planet type can hold a structure kind.
func CanHold(planet string, k PinKind) bool {
	_, ok := pinTypeIDs[planet][k]
	return ok
}

// facilityPin maps a schematic's facility to the structure that runs it.
func facilityPin(f FacilityKind) PinKind {
	switch f {
	case FacilityBasic:
		return PinBasic
	case FacilityAdvanced:
		return PinAdvanced
	case FacilityHighTech:
		return PinHighTech
	}
	return ""
}

// FacilityPin is facilityPin for callers outside the package.
func FacilityPin(f FacilityKind) PinKind { return facilityPin(f) }
