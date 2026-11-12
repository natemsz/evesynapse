package piplan

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"evesynapse/internal/esi"
)

// Colony is one planet's colony: its structures, the links between
// them and the routes over the links. Positions are the game's own:
// Lat is the polar angle in radians (pi/2 is the equator), Lon the
// longitude in radians.
type Colony struct {
	PlanetID   int64   `json:"planetId,omitempty"`
	PlanetName string  `json:"planetName,omitempty"`
	PlanetType string  `json:"planetType"`
	RadiusKm   float64 `json:"radiusKm"`
	CCLevel    int     `json:"ccLevel"`
	Name       string  `json:"name,omitempty"`
	Pins       []Pin   `json:"pins"`
	Links      []Link  `json:"links"`
	Routes     []Route `json:"routes"`
}

// Pin is one structure. Product is what it makes: the raw an
// extractor pulls, a factory's schematic output, empty for storage.
type Pin struct {
	Kind    PinKind `json:"kind"`
	Lat     float64 `json:"lat"`
	Lon     float64 `json:"lon"`
	Product string  `json:"product,omitempty"`
	// Extractor settings: head count and positions, program length
	// and the measured base (ESI's qty_per_cycle) when known.
	Heads        int       `json:"heads,omitempty"`
	HeadPos      []LatLon  `json:"headPos,omitempty"`
	ProgramHours float64   `json:"programHours,omitempty"`
	QtyPerCycle  float64   `json:"qtyPerCycle,omitempty"`
	Peak         float64   `json:"peak,omitempty"`   // assumed per-head base per hour when nothing is measured
	Keep         PinChange `json:"change,omitempty"` // set by Compare
}

// LatLon is a point on the planet.
type LatLon struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

// PinChange says what a plan does with a structure an existing
// colony already has.
type PinChange string

const (
	ChangeKeep   PinChange = "keep"
	ChangeAdd    PinChange = "add"
	ChangeRemove PinChange = "remove"
	ChangeRetool PinChange = "retool" // same building, new schematic
)

// Link joins two pins by index.
type Link struct {
	A     int `json:"a"`
	B     int `json:"b"`
	Level int `json:"level"`
}

// Route moves Qty of Item along Path (pin indexes, source first).
type Route struct {
	Path []int  `json:"path"`
	Item string `json:"item"`
	Qty  int64  `json:"qty"`
}

// Fit is a colony's CPU and power budget.
type Fit struct {
	CPUUsed, PGUsed int64
	CPUCap, PGCap   int64
}

// Fits reports whether the colony's structures fit its command center.
func (f Fit) Fits() bool { return f.CPUUsed <= f.CPUCap && f.PGUsed <= f.PGCap }

// Angle is the great-circle angle between two points, in radians.
func Angle(a, b LatLon) float64 {
	// Lat is a polar angle: convert to a unit vector.
	ax, ay, az := math.Sin(a.Lat)*math.Cos(a.Lon), math.Sin(a.Lat)*math.Sin(a.Lon), math.Cos(a.Lat)
	bx, by, bz := math.Sin(b.Lat)*math.Cos(b.Lon), math.Sin(b.Lat)*math.Sin(b.Lon), math.Cos(b.Lat)
	d := ax*bx + ay*by + az*bz
	return math.Acos(math.Max(-1, math.Min(1, d)))
}

func (p Pin) pos() LatLon { return LatLon{p.Lat, p.Lon} }

// LinkKm is a link's length on the planet's surface.
func (c *Colony) LinkKm(l Link) float64 {
	if l.A < 0 || l.B < 0 || l.A >= len(c.Pins) || l.B >= len(c.Pins) {
		return 0
	}
	return Angle(c.Pins[l.A].pos(), c.Pins[l.B].pos()) * c.RadiusKm
}

// LinkFit is one link's CPU and power at its level: the base load
// plus the per-km share, the share scaled by (level+1) raised to the
// level modifier. The game holds the per-km figures as 32-bit floats.
func LinkFit(km float64, level int) PinFit {
	cpuKm := float64(float32(LinkCPUPerKm))
	pgKm := float64(float32(LinkPGPerKm))
	return PinFit{
		CPU: int64(LinkCPU) + int64(math.Ceil(cpuKm*km*math.Pow(float64(level+1), LinkCPULevelMod))),
		PG:  int64(LinkPG) + int64(math.Ceil(pgKm*km*math.Pow(float64(level+1), LinkPGLevelMod))),
	}
}

// LinkBandwidthAt is a link's capacity in m3 per hour at a level.
func LinkBandwidthAt(level int) float64 { return LinkBandwidth * math.Pow(2, float64(level)) }

// Fit works out the colony's CPU and power use against its command
// center level, once the planned changes are made: structures marked
// for removal no longer draw on it.
func (c *Colony) Fit() Fit {
	lvl := c.CCLevel
	if lvl < 0 || lvl >= len(CommandCenters) {
		lvl = 0
	}
	f := Fit{CPUCap: CommandCenters[lvl].CPU, PGCap: CommandCenters[lvl].PG}
	for _, p := range c.Pins {
		if p.Keep == ChangeRemove {
			continue
		}
		pf := PinFits[p.Kind]
		f.CPUUsed += pf.CPU
		f.PGUsed += pf.PG
		if p.Kind == PinExtractor {
			f.CPUUsed += int64(p.Heads) * HeadFit.CPU
			f.PGUsed += int64(p.Heads) * HeadFit.PG
		}
	}
	for _, l := range c.Links {
		lf := LinkFit(c.LinkKm(l), l.Level)
		f.CPUUsed += lf.CPU
		f.PGUsed += lf.PG
	}
	return f
}

// MinPinSeparation is the closest two structures may sit, as an
// angle: the game refuses (or moves apart) closer ones on import.
const MinPinSeparation = 0.012

// Crowded lists pin indexes closer to another pin than the game allows.
func (c *Colony) Crowded() []int {
	var out []int
	for i := range c.Pins {
		for j := range c.Pins {
			if i != j && Angle(c.Pins[i].pos(), c.Pins[j].pos()) < MinPinSeparation*0.98 {
				out = append(out, i)
				break
			}
		}
	}
	return out
}

// Count tallies pins by kind.
func (c *Colony) Count() map[PinKind]int {
	m := map[PinKind]int{}
	for _, p := range c.Pins {
		m[p.Kind]++
	}
	return m
}

// --- the game's template format -------------------------------------------

// templateJSON is the colony template the game imports from the
// clipboard (Planetary Industry view, Templates, Import). Pin and link
// numbers are 1-based.
type templateJSON struct {
	CmdCtrLv int             `json:"CmdCtrLv"`
	Cmt      string          `json:"Cmt"`
	Diam     float64         `json:"Diam"`
	L        []templateLink  `json:"L"`
	P        []templatePin   `json:"P"`
	Pln      int64           `json:"Pln"`
	R        []templateRoute `json:"R"`
}

type templateLink struct {
	D  int `json:"D"`
	Lv int `json:"Lv"`
	S  int `json:"S"`
}

type templatePin struct {
	H  int     `json:"H"`
	La float64 `json:"La"`
	Lo float64 `json:"Lo"`
	S  *int64  `json:"S"`
	T  int64   `json:"T"`
}

type templateRoute struct {
	P []int `json:"P"`
	Q int64 `json:"Q"`
	T int64 `json:"T"`
}

func round5(v float64) float64 { return math.Round(v*1e5) / 1e5 }

// Template renders the colony as the game's template JSON. Removed
// pins (a plan's tear-downs) are left out and the numbering closes up.
func (c *Colony) Template() (string, error) {
	t := templateJSON{CmdCtrLv: c.CCLevel, Cmt: c.Name, Diam: math.Round(c.RadiusKm * 2), Pln: PlanetTypeIDs[c.PlanetType]}
	if t.Pln == 0 {
		return "", fmt.Errorf("unknown planet type %q", c.PlanetType)
	}
	if t.Cmt == "" {
		t.Cmt = c.PlanetType + " colony"
	}
	num := map[int]int{}
	for i, p := range c.Pins {
		if p.Keep == ChangeRemove {
			continue
		}
		id, ok := PinTypeID(c.PlanetType, p.Kind)
		if !ok {
			return "", fmt.Errorf("a %s cannot stand on a %s planet", PinLabel(p.Kind), c.PlanetType)
		}
		tp := templatePin{La: round5(p.Lat), Lo: round5(p.Lon), T: id}
		if p.Kind == PinExtractor {
			tp.H = p.Heads
		}
		if p.Product != "" && p.Kind != PinCommandCenter && p.Kind != PinStorage && p.Kind != PinLaunchpad {
			if pid, ok := CommodityTypeIDs[p.Product]; ok {
				tp.S = &pid
			}
		}
		t.P = append(t.P, tp)
		num[i] = len(t.P)
	}
	for _, l := range c.Links {
		a, okA := num[l.A]
		b, okB := num[l.B]
		if okA && okB {
			t.L = append(t.L, templateLink{S: a, D: b, Lv: l.Level})
		}
	}
	for _, r := range c.Routes {
		tr := templateRoute{Q: r.Qty, T: CommodityTypeIDs[r.Item]}
		ok := tr.T != 0
		for _, i := range r.Path {
			n, has := num[i]
			if !has {
				ok = false
				break
			}
			tr.P = append(tr.P, n)
		}
		if ok && len(tr.P) >= 2 {
			t.R = append(t.R, tr)
		}
	}
	if t.L == nil {
		t.L = []templateLink{}
	}
	if t.R == nil {
		t.R = []templateRoute{}
	}
	b, err := json.Marshal(t)
	return string(b), err
}

// ParseTemplate reads a template the game exported. radius is taken
// from the template's diameter.
func ParseTemplate(raw string) (*Colony, error) {
	var t templateJSON
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &t); err != nil {
		return nil, fmt.Errorf("not a colony template: %w", err)
	}
	pt, ok := PlanetTypeByID[t.Pln]
	if !ok {
		return nil, fmt.Errorf("the template names an unknown planet type (%d)", t.Pln)
	}
	c := &Colony{PlanetType: pt, RadiusKm: t.Diam / 2, CCLevel: t.CmdCtrLv, Name: t.Cmt}
	if c.CCLevel < 0 || c.CCLevel > 5 {
		c.CCLevel = 5
	}
	for _, tp := range t.P {
		k, ok := PinKindOf(tp.T)
		if !ok {
			return nil, fmt.Errorf("the template holds an unknown structure (%d)", tp.T)
		}
		p := Pin{Kind: k, Lat: tp.La, Lon: tp.Lo}
		if tp.S != nil {
			p.Product = CommodityByTypeID[*tp.S]
		}
		if k == PinExtractor {
			p.Heads = tp.H
		}
		c.Pins = append(c.Pins, p)
	}
	n := len(c.Pins)
	for _, tl := range t.L {
		if tl.S >= 1 && tl.S <= n && tl.D >= 1 && tl.D <= n {
			c.Links = append(c.Links, Link{A: tl.S - 1, B: tl.D - 1, Level: tl.Lv})
		}
	}
	for _, tr := range t.R {
		r := Route{Item: CommodityByTypeID[tr.T], Qty: tr.Q}
		for _, i := range tr.P {
			if i >= 1 && i <= n {
				r.Path = append(r.Path, i-1)
			}
		}
		if len(r.Path) >= 2 && r.Item != "" {
			c.Routes = append(c.Routes, r)
		}
	}
	return c, nil
}

// FromESI turns a stored ESI colony layout into a Colony. The
// planet's type and radius come from the caller (static data).
func FromESI(layout esi.PlanetLayout, planetType string, radiusKm float64, ccLevel int) *Colony {
	c := &Colony{PlanetType: planetType, RadiusKm: radiusKm, CCLevel: ccLevel}
	index := map[int64]int{}
	for _, ep := range layout.Pins {
		k, ok := PinKindOf(ep.TypeID)
		if !ok {
			if ep.ExtractorDetails == nil {
				continue
			}
			k = PinExtractor
		}
		p := Pin{Kind: k, Lat: ep.Latitude, Lon: ep.Longitude, Keep: ChangeKeep}
		if ep.SchematicID != 0 {
			p.Product = SchematicOutputs[ep.SchematicID]
		} else if ep.FactoryDetails != nil {
			p.Product = SchematicOutputs[ep.FactoryDetails.SchematicID]
		}
		if x := ep.ExtractorDetails; x != nil {
			p.Product = CommodityByTypeID[x.ProductTypeID]
			p.Heads = len(x.Heads)
			for _, h := range x.Heads {
				p.HeadPos = append(p.HeadPos, LatLon{h.Latitude, h.Longitude})
			}
			p.QtyPerCycle = float64(x.QtyPerCycle)
			if hrs := esiProgramHours(ep.InstallTime, ep.ExpiryTime); hrs > 0 {
				p.ProgramHours = hrs
			}
		}
		index[ep.PinID] = len(c.Pins)
		c.Pins = append(c.Pins, p)
	}
	for _, l := range layout.Links {
		a, okA := index[l.SourcePinID]
		b, okB := index[l.DestinationPinID]
		if okA && okB {
			c.Links = append(c.Links, Link{A: a, B: b, Level: int(l.LinkLevel)})
		}
	}
	for _, r := range layout.Routes {
		path := []int{}
		ok := true
		for _, id := range append(append([]int64{r.SourcePinID}, r.Waypoints...), r.DestinationPinID) {
			i, has := index[id]
			if !has {
				ok = false
				break
			}
			if len(path) > 0 && path[len(path)-1] == i {
				continue
			}
			path = append(path, i)
		}
		if ok && len(path) >= 2 {
			c.Routes = append(c.Routes, Route{Path: path, Item: CommodityByTypeID[r.ContentTypeID], Qty: int64(r.Quantity)})
		}
	}
	return c
}

// esiProgramHours is an extractor program's length from its install
// and expiry times; 0 when either is missing.
func esiProgramHours(install, expiry string) float64 {
	a, err1 := time.Parse(time.RFC3339, install)
	b, err2 := time.Parse(time.RFC3339, expiry)
	if err1 != nil || err2 != nil || !b.After(a) {
		return 0
	}
	return math.Round(b.Sub(a).Hours())
}
