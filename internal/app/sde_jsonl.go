package app

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// CCP's SDE API (sde.evetech.net) serves every static data table as
// JSON Lines: one record per line, localized text as a map of
// language to string. The parsers below read those tables into the
// same parsedSDE the CSV parsers fill, so the store step, the
// derived tables and every reader stay the same whichever format a
// dump arrives in.

// sdeAPITable is one SDE API table the importer reads. A required
// table missing from a dump fails the import; an optional one keeps
// the previous rows of what it feeds.
type sdeAPITable struct {
	name     string
	key      string // the field holding a record's id
	required bool
}

// sdeAPITables lists every table the importer reads, required first.
// mapPlanets and mapMoons are required because station names are
// built from the planet or moon a station orbits.
var sdeAPITables = []sdeAPITable{
	{"types", "typeID", true},
	{"groups", "groupID", true},
	{"categories", "categoryID", true},
	{"marketGroups", "marketGroupID", true},
	{"typeMaterials", "typeID", true},
	{"npcStations", "stationID", true},
	{"npcCorporations", "corporationID", true},
	{"stationOperations", "operationID", true},
	{"mapSolarSystems", "solarSystemID", true},
	{"mapRegions", "regionID", true},
	{"mapPlanets", "planetID", true},
	{"mapMoons", "moonID", true},
	{"blueprints", "blueprintTypeID", true},
	{"typeDogma", "typeID", true},
	{"dogmaAttributes", "attributeID", true},
	{"dogmaEffects", "effectID", true},
	{"mapConstellations", "constellationID", false},
	{"mapStargates", "stargateID", false},
	{"mapStars", "starID", false},
	{"mapAsteroidBelts", "asteroidBeltID", false},
	{"dynamicItemAttributes", "typeID", false},
	{"dogmaUnits", "unitID", false},
	{"typeBonus", "typeID", false},
	{"metaGroups", "metaGroupID", false},
	{"characterTitles", "titleID", false},
	{"dbuffCollections", "dbuffCollectionID", false},
}

// sdeAPIFileName is a table's file name in a JSON Lines dump.
func sdeAPIFileName(table string) string { return table + ".jsonl" }

// sdeJSONLState holds what one table contributes to rows another
// table finishes: station and celestial names join the systems,
// planets, moons, corporations and station operations.
type sdeJSONLState struct {
	seen       map[string]bool
	systemName map[int64]string
	corpName   map[int64]string
	opName     map[int64]string
	// A system without a faction of its own takes its
	// constellation's, then its region's.
	constFaction  map[int64]int64
	regionFaction map[int64]int64
	stations      []jlStation
	planets       []jlCelestial
	moons         []jlCelestial
	belts         []jlCelestial
	stars         []jlCelestial
	gates         []jlStargate
	traitSeq      int64
}

func (parsed *parsedSDE) jsonl() *sdeJSONLState {
	if parsed.jl == nil {
		parsed.jl = &sdeJSONLState{
			seen:       make(map[string]bool),
			systemName: make(map[int64]string),
			corpName:   make(map[int64]string),
			opName:     make(map[int64]string),

			constFaction:  make(map[int64]int64),
			regionFaction: make(map[int64]int64),
		}
	}
	return parsed.jl
}

// locText reads a localized text field: the English text of a
// language map, or a plain string.
type locText string

func (t *locText) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*t = locText(s)
		return nil
	}
	if string(b) == "null" {
		*t = ""
		return nil
	}
	var m struct {
		En string `json:"en"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	*t = locText(m.En)
	return nil
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

type jlPos struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

type jlStation struct {
	StationID        int64 `json:"stationID"`
	OrbitID          int64 `json:"orbitID"`
	OwnerID          int64 `json:"ownerID"`
	OperationID      int64 `json:"operationID"`
	SolarSystemID    int64 `json:"solarSystemID"`
	UseOperationName bool  `json:"useOperationName"`
}

// jlCelestial is a star, planet, moon or asteroid belt; the id field
// depends on the table and is filled by its parser.
type jlCelestial struct {
	id             int64
	SolarSystemID  int64   `json:"solarSystemID"`
	TypeID         int64   `json:"typeID"`
	Radius         float64 `json:"radius"`
	OrbitID        int64   `json:"orbitID"`
	CelestialIndex int64   `json:"celestialIndex"`
	OrbitIndex     int64   `json:"orbitIndex"`
	UniqueName     locText `json:"uniqueName"`
}

type jlStargate struct {
	StargateID    int64 `json:"stargateID"`
	SolarSystemID int64 `json:"solarSystemID"`
	TypeID        int64 `json:"typeID"`
	Destination   struct {
		SolarSystemID int64 `json:"solarSystemID"`
	} `json:"destination"`
}

type jlBonus struct {
	Bonus      *float64 `json:"bonus"`
	BonusText  locText  `json:"bonusText"`
	Importance int64    `json:"importance"`
	UnitID     int64    `json:"unitID"`
}

type jlActivity struct {
	Time      int64 `json:"time"`
	Materials []struct {
		TypeID   int64 `json:"typeID"`
		Quantity int64 `json:"quantity"`
	} `json:"materials"`
	Products []struct {
		TypeID   int64 `json:"typeID"`
		Quantity int64 `json:"quantity"`
	} `json:"products"`
	Skills []struct {
		TypeID int64 `json:"typeID"`
		Level  int64 `json:"level"`
	} `json:"skills"`
}

// openSDEJSONL returns a reader over a table's JSON Lines, unpacking
// gzip when the body is gzipped (the SDE API serves it that way).
func openSDEJSONL(body io.Reader) (io.Reader, error) {
	br := bufio.NewReaderSize(body, 64<<10)
	if magic, err := br.Peek(2); err == nil && magic[0] == 0x1f && magic[1] == 0x8b {
		zr, err := gzip.NewReader(br)
		if err != nil {
			return nil, err
		}
		return zr, nil
	}
	return br, nil
}

// sdeZipShape reads the JSON Lines of CCP's static data zip, which
// writes records in the older shape: the id under "_key" rather than
// the table's own id field, and nested maps as lists of {"_key",
// "_value"} pairs. Each such record is rewritten to the SDE API's
// shape; records already in it pass through unchanged.
type sdeZipShape struct {
	table string
	key   string
	src   *bufio.Reader
	buf   []byte
	err   error
}

func newSDEZipShape(table string, body io.Reader) io.Reader {
	key := ""
	for _, t := range sdeAPITables {
		if t.name == table {
			key = t.key
		}
	}
	return &sdeZipShape{table: table, key: key, src: bufio.NewReaderSize(body, 1<<20)}
}

func (z *sdeZipShape) Read(p []byte) (int, error) {
	for len(z.buf) == 0 {
		if z.err != nil {
			return 0, z.err
		}
		line, err := z.src.ReadBytes('\n')
		z.err = err
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if z.key != "" && bytes.Contains(line, []byte(`"_key"`)) {
			if line, err = z.reshape(line); err != nil {
				return 0, err
			}
		}
		z.buf = line
	}
	n := copy(p, z.buf)
	z.buf = z.buf[n:]
	return n, nil
}

func (z *sdeZipShape) reshape(line []byte) ([]byte, error) {
	var rec map[string]json.RawMessage
	if err := json.Unmarshal(line, &rec); err != nil {
		return nil, fmt.Errorf("%s: %w", z.table, err)
	}
	if id, ok := rec["_key"]; ok {
		rec[z.key] = id
		delete(rec, "_key")
	}
	switch z.table {
	case "typeBonus":
		// types: [{"_key": skill, "_value": [bonus...]}]
		if raw, ok := rec["types"]; ok {
			var pairs []struct {
				Key   json.RawMessage `json:"_key"`
				Value json.RawMessage `json:"_value"`
			}
			if err := json.Unmarshal(raw, &pairs); err == nil && len(pairs) > 0 && pairs[0].Key != nil {
				out := make([]map[string]json.RawMessage, len(pairs))
				for i, kv := range pairs {
					out[i] = map[string]json.RawMessage{"typeID": kv.Key, "bonuses": kv.Value}
				}
				if rec["types"], err = json.Marshal(out); err != nil {
					return nil, err
				}
			}
		}
	case "dynamicItemAttributes":
		// attributeIDs: [{"_key": attribute, "min": ..., "max": ...}]
		if raw, ok := rec["attributeIDs"]; ok {
			var attrs []map[string]json.RawMessage
			if err := json.Unmarshal(raw, &attrs); err == nil {
				for _, a := range attrs {
					if id, ok := a["_key"]; ok {
						a["attributeID"] = id
						delete(a, "_key")
					}
				}
				if rec["attributeIDs"], err = json.Marshal(attrs); err != nil {
					return nil, err
				}
			}
		}
	}
	out, err := json.Marshal(rec)
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// eachJSONL decodes every record of a JSON Lines body into a fresh T
// and hands it to fn.
func eachJSONL[T any](body io.Reader, fn func(*T) error) error {
	dec := json.NewDecoder(body)
	for n := 1; ; n++ {
		var rec T
		if err := dec.Decode(&rec); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("record %d: %w", n, err)
		}
		if err := fn(&rec); err != nil {
			return fmt.Errorf("record %d: %w", n, err)
		}
	}
}

// parseSDEJSONLTable streams one SDE API table (plain or gzipped JSON
// Lines) into parsed. Rows that join other tables wait for
// finishJSONL.
func parseSDEJSONLTable(table string, body io.Reader, parsed *parsedSDE) error {
	r, err := openSDEJSONL(io.LimitReader(body, 1<<30))
	if err != nil {
		return fmt.Errorf("parse %s: %w", table, err)
	}
	r = io.LimitReader(r, 2<<30)
	st := parsed.jsonl()
	st.seen[table] = true
	switch table {
	case "types":
		err = eachJSONL(r, func(t *struct {
			TypeID                int64   `json:"typeID"`
			GroupID               int64   `json:"groupID"`
			MarketGroupID         int64   `json:"marketGroupID"`
			Name                  locText `json:"name"`
			Description           locText `json:"description"`
			Published             bool    `json:"published"`
			Mass                  float64 `json:"mass"`
			Volume                float64 `json:"volume"`
			Capacity              float64 `json:"capacity"`
			PortionSize           int64   `json:"portionSize"`
			MetaGroupID           int64   `json:"metaGroupID"`
			VariationParentTypeID int64   `json:"variationParentTypeID"`
		}) error {
			parsed.types = append(parsed.types, sdeTypeRow{
				typeID: t.TypeID, name: string(t.Name), description: string(t.Description),
				groupID: t.GroupID, marketGroupID: t.MarketGroupID, published: boolInt(t.Published),
				mass: t.Mass, volume: t.Volume, capacity: t.Capacity, portionSize: t.PortionSize,
			})
			if t.MetaGroupID != 0 || t.VariationParentTypeID != 0 {
				parsed.metaTypes = append(parsed.metaTypes, sdeMetaTypeRow{
					typeID: t.TypeID, parentTypeID: t.VariationParentTypeID, metaGroupID: t.MetaGroupID,
				})
			}
			return nil
		})
	case "groups":
		err = eachJSONL(r, func(g *struct {
			GroupID    int64   `json:"groupID"`
			CategoryID int64   `json:"categoryID"`
			Name       locText `json:"name"`
		}) error {
			parsed.groups = append(parsed.groups, sdeGroupRow{groupID: g.GroupID, name: string(g.Name), categoryID: g.CategoryID})
			return nil
		})
	case "categories":
		err = eachJSONL(r, func(c *struct {
			CategoryID int64   `json:"categoryID"`
			Name       locText `json:"name"`
		}) error {
			parsed.categories = append(parsed.categories, sdeCategoryRow{categoryID: c.CategoryID, name: string(c.Name)})
			return nil
		})
	case "marketGroups":
		err = eachJSONL(r, func(m *struct {
			MarketGroupID int64   `json:"marketGroupID"`
			ParentGroupID int64   `json:"parentGroupID"`
			Name          locText `json:"name"`
			IconID        int64   `json:"iconID"`
			HasTypes      bool    `json:"hasTypes"`
		}) error {
			parsed.marketGroups = append(parsed.marketGroups, sdeMarketGroupRow{
				marketGroupID: m.MarketGroupID, parentGroupID: m.ParentGroupID, name: string(m.Name),
				iconID: m.IconID, hasTypes: boolInt(m.HasTypes),
			})
			return nil
		})
	case "typeMaterials":
		err = eachJSONL(r, func(t *struct {
			TypeID    int64 `json:"typeID"`
			Materials []struct {
				MaterialTypeID int64 `json:"materialTypeID"`
				Quantity       int64 `json:"quantity"`
			} `json:"materials"`
		}) error {
			for _, m := range t.Materials {
				if m.Quantity > 0 && m.MaterialTypeID != 0 {
					parsed.reprocessing = append(parsed.reprocessing, sdeReprocessRow{typeID: t.TypeID, materialTypeID: m.MaterialTypeID, quantity: m.Quantity})
				}
			}
			return nil
		})
	case "npcStations":
		err = eachJSONL(r, func(s *jlStation) error {
			st.stations = append(st.stations, *s)
			return nil
		})
	case "npcCorporations":
		err = eachJSONL(r, func(c *struct {
			CorporationID int64   `json:"corporationID"`
			FactionID     int64   `json:"factionID"`
			Name          locText `json:"name"`
		}) error {
			parsed.npcCorps = append(parsed.npcCorps, sdeNPCCorpRow{corporationID: c.CorporationID, factionID: c.FactionID})
			st.corpName[c.CorporationID] = string(c.Name)
			return nil
		})
	case "stationOperations":
		err = eachJSONL(r, func(o *struct {
			OperationID   int64   `json:"operationID"`
			OperationName locText `json:"operationName"`
		}) error {
			st.opName[o.OperationID] = string(o.OperationName)
			return nil
		})
	case "mapSolarSystems":
		err = eachJSONL(r, func(s *struct {
			SolarSystemID   int64   `json:"solarSystemID"`
			Name            locText `json:"name"`
			RegionID        int64   `json:"regionID"`
			ConstellationID int64   `json:"constellationID"`
			SecurityStatus  float64 `json:"securityStatus"`
			SecurityClass   string  `json:"securityClass"`
			FactionID       int64   `json:"factionID"`
			Position        jlPos   `json:"position"`
			Position2D      jlPos   `json:"position2D"`
		}) error {
			parsed.systems = append(parsed.systems, sdeSystemRow{
				systemID: s.SolarSystemID, name: string(s.Name), regionID: s.RegionID,
				security: s.SecurityStatus, constellationID: s.ConstellationID,
				x: s.Position.X, y: s.Position.Y, z: s.Position.Z,
				pos2dx: s.Position2D.X, pos2dy: s.Position2D.Y,
				factionID: s.FactionID, securityClass: strings.TrimSpace(s.SecurityClass),
			})
			st.systemName[s.SolarSystemID] = string(s.Name)
			return nil
		})
	case "mapRegions":
		err = eachJSONL(r, func(g *struct {
			RegionID  int64   `json:"regionID"`
			FactionID int64   `json:"factionID"`
			Name      locText `json:"name"`
		}) error {
			st.regionFaction[g.RegionID] = g.FactionID
			parsed.regions = append(parsed.regions, sdeRegionRow{regionID: g.RegionID, name: string(g.Name)})
			return nil
		})
	case "mapConstellations":
		err = eachJSONL(r, func(c *struct {
			ConstellationID int64   `json:"constellationID"`
			RegionID        int64   `json:"regionID"`
			FactionID       int64   `json:"factionID"`
			Name            locText `json:"name"`
		}) error {
			st.constFaction[c.ConstellationID] = c.FactionID
			parsed.constellations = append(parsed.constellations, sdeConstellationRow{constellationID: c.ConstellationID, name: string(c.Name), regionID: c.RegionID})
			return nil
		})
	case "mapStargates":
		err = eachJSONL(r, func(g *jlStargate) error {
			st.gates = append(st.gates, *g)
			if g.SolarSystemID != 0 && g.Destination.SolarSystemID != 0 {
				parsed.jumps = append(parsed.jumps, sdeJumpRow{from: g.SolarSystemID, to: g.Destination.SolarSystemID})
			}
			return nil
		})
	case "mapStars":
		err = eachCelestial(r, &st.stars)
	case "mapPlanets":
		err = eachCelestial(r, &st.planets)
	case "mapMoons":
		err = eachCelestial(r, &st.moons)
	case "mapAsteroidBelts":
		err = eachCelestial(r, &st.belts)
	case "blueprints":
		err = parseJSONLBlueprints(r, parsed)
	case "typeDogma":
		if parsed.dogma == nil {
			parsed.dogma = make(map[int64]map[int64]float64)
		}
		err = eachJSONL(r, func(t *struct {
			TypeID          int64 `json:"typeID"`
			DogmaAttributes []struct {
				AttributeID int64   `json:"attributeID"`
				Value       float64 `json:"value"`
			} `json:"dogmaAttributes"`
			DogmaEffects []struct {
				EffectID  int64 `json:"effectID"`
				IsDefault bool  `json:"isDefault"`
			} `json:"dogmaEffects"`
		}) error {
			for _, a := range t.DogmaAttributes {
				parsed.typeAttrs = append(parsed.typeAttrs, sdeTypeAttributeRow{typeID: t.TypeID, attributeID: a.AttributeID, value: a.Value})
				if !dogmaWantedAttrs[a.AttributeID] {
					continue
				}
				attrs := parsed.dogma[t.TypeID]
				if attrs == nil {
					attrs = make(map[int64]float64, 4)
					parsed.dogma[t.TypeID] = attrs
				}
				attrs[a.AttributeID] = a.Value
			}
			for _, e := range t.DogmaEffects {
				parsed.typeEffects = append(parsed.typeEffects, sdeTypeEffectRow{typeID: t.TypeID, effectID: e.EffectID, isDefault: boolInt(e.IsDefault)})
			}
			return nil
		})
	case "dogmaAttributes":
		err = eachJSONL(r, func(a *struct {
			AttributeID         int64   `json:"attributeID"`
			Name                string  `json:"name"`
			Stackable           bool    `json:"stackable"`
			HighIsGood          bool    `json:"highIsGood"`
			UnitID              int64   `json:"unitID"`
			DefaultValue        float64 `json:"defaultValue"`
			DisplayName         locText `json:"displayName"`
			Published           bool    `json:"published"`
			DisplayWhenZero     bool    `json:"displayWhenZero"`
			AttributeCategoryID int64   `json:"attributeCategoryID"`
			IconID              int64   `json:"iconID"`
		}) error {
			parsed.attrTypes = append(parsed.attrTypes, sdeAttributeTypeRow{
				attributeID: a.AttributeID, name: a.Name,
				stackable: boolInt(a.Stackable), highIsGood: boolInt(a.HighIsGood),
				unitID: a.UnitID, defaultValue: a.DefaultValue,
				displayName: string(a.DisplayName), published: boolInt(a.Published),
				displayWhenZero: boolInt(a.DisplayWhenZero), categoryID: a.AttributeCategoryID, iconID: a.IconID,
			})
			return nil
		})
	case "dogmaEffects":
		err = eachJSONL(r, func(e *struct {
			EffectID         int64             `json:"effectID"`
			Name             string            `json:"name"`
			EffectCategoryID int64             `json:"effectCategoryID"`
			ModifierInfo     []sdeModifierJSON `json:"modifierInfo"`
		}) error {
			parsed.effects = append(parsed.effects, sdeEffectRow{effectID: e.EffectID, name: e.Name, category: e.EffectCategoryID})
			parsed.appendModifiers(e.EffectID, e.ModifierInfo)
			return nil
		})
	case "dynamicItemAttributes":
		err = eachJSONL(r, func(d *struct {
			TypeID       int64 `json:"typeID"`
			AttributeIDs []struct {
				AttributeID int64    `json:"attributeID"`
				Min         *float64 `json:"min"`
				Max         *float64 `json:"max"`
			} `json:"attributeIDs"`
			InputOutputMapping []struct {
				ApplicableTypes []int64 `json:"applicableTypes"`
				ResultingType   int64   `json:"resultingType"`
			} `json:"inputOutputMapping"`
		}) error {
			for _, a := range d.AttributeIDs {
				if a.Min == nil || a.Max == nil {
					continue
				}
				parsed.dynRanges = append(parsed.dynRanges, sdeDynRangeRow{mutatorTypeID: d.TypeID, attributeID: a.AttributeID, minMult: *a.Min, maxMult: *a.Max})
			}
			for _, m := range d.InputOutputMapping {
				for _, applicable := range m.ApplicableTypes {
					parsed.dynMappings = append(parsed.dynMappings, sdeDynMappingRow{mutatorTypeID: d.TypeID, applicableTypeID: applicable, resultingTypeID: m.ResultingType})
				}
			}
			return nil
		})
	case "dogmaUnits":
		err = eachJSONL(r, func(u *struct {
			UnitID      int64   `json:"unitID"`
			DisplayName locText `json:"displayName"`
		}) error {
			parsed.units = append(parsed.units, sdeUnitRow{unitID: u.UnitID, displayName: string(u.DisplayName)})
			return nil
		})
	case "typeBonus":
		err = eachJSONL(r, func(b *struct {
			TypeID      int64     `json:"typeID"`
			RoleBonuses []jlBonus `json:"roleBonuses"`
			Types       []struct {
				TypeID  int64     `json:"typeID"`
				Bonuses []jlBonus `json:"bonuses"`
			} `json:"types"`
			MiscBonuses []jlBonus `json:"miscBonuses"`
		}) error {
			// Skill -1 marks a role bonus and -2 a bonus that needs no
			// skill, as the item sheet's trait query expects.
			for _, x := range b.RoleBonuses {
				parsed.appendTrait(b.TypeID, -1, x)
			}
			for _, s := range b.Types {
				for _, x := range s.Bonuses {
					parsed.appendTrait(b.TypeID, s.TypeID, x)
				}
			}
			for _, x := range b.MiscBonuses {
				parsed.appendTrait(b.TypeID, -2, x)
			}
			return nil
		})
	case "metaGroups":
		err = eachJSONL(r, func(m *struct {
			MetaGroupID int64   `json:"metaGroupID"`
			Name        locText `json:"name"`
		}) error {
			parsed.metaGroups = append(parsed.metaGroups, sdeMetaGroupRow{metaGroupID: m.MetaGroupID, name: string(m.Name)})
			return nil
		})
	case "characterTitles":
		err = eachJSONL(r, func(c *struct {
			TitleID string  `json:"titleID"`
			Name    locText `json:"name"`
		}) error {
			id := strings.ToLower(strings.TrimSpace(c.TitleID))
			name := strings.TrimSpace(string(c.Name))
			if id != "" && name != "" {
				parsed.titles = append(parsed.titles, sdeCharacterTitleRow{titleID: id, name: name})
			}
			return nil
		})
	case "dbuffCollections":
		type attrRef struct {
			DogmaAttributeID int64 `json:"dogmaAttributeID"`
			GroupID          int64 `json:"groupID"`
			SkillID          int64 `json:"skillID"`
		}
		err = eachJSONL(r, func(d *struct {
			DbuffCollectionID              int64     `json:"dbuffCollectionID"`
			DisplayName                    locText   `json:"displayName"`
			AggregateMode                  string    `json:"aggregateMode"`
			OperationName                  string    `json:"operationName"`
			ItemModifiers                  []attrRef `json:"itemModifiers"`
			LocationModifiers              []attrRef `json:"locationModifiers"`
			LocationGroupModifiers         []attrRef `json:"locationGroupModifiers"`
			LocationRequiredSkillModifiers []attrRef `json:"locationRequiredSkillModifiers"`
		}) error {
			parsed.dbuffs = append(parsed.dbuffs, sdeDbuffRow{dbuffID: d.DbuffCollectionID, displayName: string(d.DisplayName), aggregate: d.AggregateMode, operation: d.OperationName})
			for _, set := range []struct {
				kind string
				refs []attrRef
			}{{"item", d.ItemModifiers}, {"location", d.LocationModifiers}, {"group", d.LocationGroupModifiers}, {"skill", d.LocationRequiredSkillModifiers}} {
				for _, m := range set.refs {
					parsed.dbuffMods = append(parsed.dbuffMods, sdeDbuffModRow{dbuffID: d.DbuffCollectionID, kind: set.kind, attributeID: m.DogmaAttributeID, groupID: m.GroupID, skillID: m.SkillID})
				}
			}
			return nil
		})
	default:
		err = fmt.Errorf("unknown SDE table %q", table)
	}
	if err != nil {
		return fmt.Errorf("parse %s: %w", table, err)
	}
	return nil
}

// dropJSONLTable forgets everything an optional table fed, so one that
// failed partway leaves no half set behind and the store keeps the
// rows stored before.
func (parsed *parsedSDE) dropJSONLTable(table string) {
	st := parsed.jsonl()
	delete(st.seen, table)
	switch table {
	case "mapConstellations":
		parsed.constellations = nil
		clear(st.constFaction)
	case "mapStargates":
		parsed.jumps, st.gates = nil, nil
	case "mapStars":
		st.stars = nil
	case "mapAsteroidBelts":
		st.belts = nil
	case "dynamicItemAttributes":
		parsed.dynRanges, parsed.dynMappings = nil, nil
	case "dogmaUnits":
		parsed.units = nil
	case "typeBonus":
		parsed.traits, st.traitSeq = nil, 0
	case "metaGroups":
		parsed.metaGroups = nil
	case "characterTitles":
		parsed.titles = nil
	case "dbuffCollections":
		parsed.dbuffs, parsed.dbuffMods = nil, nil
	}
}

// eachCelestial reads a star, planet, moon or belt table; each names
// its row id differently.
func eachCelestial(r io.Reader, out *[]jlCelestial) error {
	return eachJSONL(r, func(c *struct {
		jlCelestial
		StarID         int64 `json:"starID"`
		PlanetID       int64 `json:"planetID"`
		MoonID         int64 `json:"moonID"`
		AsteroidBeltID int64 `json:"asteroidBeltID"`
	}) error {
		c.id = max(c.StarID, c.PlanetID, c.MoonID, c.AsteroidBeltID)
		if c.id == 0 {
			return nil
		}
		*out = append(*out, c.jlCelestial)
		return nil
	})
}

func parseJSONLBlueprints(r io.Reader, parsed *parsedSDE) error {
	if parsed.indMaxLimit == nil {
		parsed.indMaxLimit = make(map[int64]int64)
		parsed.indTime = make(map[int64]int64)
		parsed.indProduct = make(map[int64]sdeIndustryProduct)
		parsed.indMats = make(map[[2]int64]int64)
		parsed.indSkills = make(map[[2]int64]int64)
	}
	if parsed.invProduct == nil {
		parsed.invProduct = make(map[[2]int64]int64)
		parsed.invMats = make(map[[2]int64]int64)
	}
	return eachJSONL(r, func(b *struct {
		BlueprintTypeID    int64 `json:"blueprintTypeID"`
		MaxProductionLimit int64 `json:"maxProductionLimit"`
		Activities         struct {
			Manufacturing *jlActivity `json:"manufacturing"`
			Invention     *jlActivity `json:"invention"`
		} `json:"activities"`
	}) error {
		id := b.BlueprintTypeID
		parsed.indMaxLimit[id] = b.MaxProductionLimit
		if m := b.Activities.Manufacturing; m != nil {
			parsed.indTime[id] = m.Time
			if len(m.Products) > 0 {
				qty := m.Products[0].Quantity
				if qty < 1 {
					qty = 1
				}
				parsed.indProduct[id] = sdeIndustryProduct{productTypeID: m.Products[0].TypeID, quantity: qty}
			}
			for _, mat := range m.Materials {
				if mat.TypeID != 0 {
					parsed.indMats[[2]int64{id, mat.TypeID}] += mat.Quantity
				}
			}
			for _, s := range m.Skills {
				key := [2]int64{id, s.TypeID}
				if s.TypeID != 0 && s.Level > parsed.indSkills[key] {
					parsed.indSkills[key] = s.Level
				}
			}
		}
		if inv := b.Activities.Invention; inv != nil {
			for _, p := range inv.Products {
				if p.TypeID != 0 {
					parsed.invProduct[[2]int64{id, p.TypeID}] = max(p.Quantity, 1)
				}
			}
			for _, mat := range inv.Materials {
				if mat.TypeID != 0 {
					parsed.invMats[[2]int64{id, mat.TypeID}] += mat.Quantity
				}
			}
		}
		return nil
	})
}

// appendModifiers adds an effect's decoded modifiers, skipping the
// ones that change no attribute (effect stoppers).
func (parsed *parsedSDE) appendModifiers(effectID int64, mods []sdeModifierJSON) {
	for _, m := range mods {
		if m.ModifiedAttributeID == nil || m.ModifyingAttributeID == nil || m.Operation == nil {
			continue
		}
		row := sdeEffectModifierRow{
			effectID:      effectID,
			modifiedAttr:  *m.ModifiedAttributeID,
			modifyingAttr: *m.ModifyingAttributeID,
			operation:     *m.Operation,
		}
		if m.Domain != nil {
			row.domain = *m.Domain
		}
		if m.Func != nil {
			row.fn = *m.Func
		}
		if m.GroupID != nil {
			row.groupID = *m.GroupID
		}
		if m.SkillTypeID != nil {
			row.skillTypeID = *m.SkillTypeID
		}
		parsed.modifiers = append(parsed.modifiers, row)
	}
}

// appendTrait adds one bonus line, numbered in the order the table
// lists them.
func (parsed *parsedSDE) appendTrait(typeID, skillID int64, b jlBonus) {
	st := parsed.jsonl()
	st.traitSeq++
	row := sdeTraitRow{
		traitID: st.traitSeq, typeID: typeID, skillID: skillID,
		bonusText: string(b.BonusText), unitID: b.UnitID, importance: b.Importance,
	}
	if b.Bonus != nil {
		row.bonus, row.hasBonus = *b.Bonus, true
	}
	parsed.traits = append(parsed.traits, row)
}

// finishJSONL builds the rows that join several tables: station
// names and the named celestials. It runs once every table of the
// dump is parsed; without the systems it leaves both alone.
func (parsed *parsedSDE) finishJSONL() {
	st := parsed.jl
	if st == nil {
		return
	}
	defer func() { parsed.jl = nil }()

	for i := range parsed.systems {
		s := &parsed.systems[i]
		if s.factionID == 0 {
			s.factionID = st.constFaction[s.constellationID]
		}
		if s.factionID == 0 {
			s.factionID = st.regionFaction[s.regionID]
		}
	}

	// Celestial names as the game writes them: planets by Roman
	// numeral unless the planet has its own name, moons and belts
	// after the planet they orbit.
	names := make(map[int64]string, len(st.planets)+len(st.moons))
	for _, s := range st.stars {
		names[s.id] = st.systemName[s.SolarSystemID] + " - Star"
	}
	for _, p := range st.planets {
		if p.UniqueName != "" {
			names[p.id] = string(p.UniqueName)
		} else {
			names[p.id] = st.systemName[p.SolarSystemID] + " " + romanNumeral(p.CelestialIndex)
		}
	}
	for _, m := range st.moons {
		if m.UniqueName != "" {
			names[m.id] = string(m.UniqueName)
		} else {
			names[m.id] = names[m.OrbitID] + " - Moon " + strconv.FormatInt(m.OrbitIndex, 10)
		}
	}
	for _, b := range st.belts {
		names[b.id] = names[b.OrbitID] + " - Asteroid Belt " + strconv.FormatInt(b.OrbitIndex, 10)
	}

	if st.seen["npcStations"] {
		for _, s := range st.stations {
			name := st.corpName[s.OwnerID]
			if s.UseOperationName && st.opName[s.OperationID] != "" {
				name += " " + st.opName[s.OperationID]
			}
			if orbit := names[s.OrbitID]; orbit != "" {
				name = orbit + " - " + name
			}
			parsed.stations = append(parsed.stations, sdeStationRow{stationID: s.StationID, name: name, systemID: s.SolarSystemID, corporationID: s.OwnerID})
		}
	}

	// The celestials table goes in whole or not at all: a kill's
	// location must not lose its name because one table was missing.
	if !st.seen["mapStars"] || !st.seen["mapPlanets"] || !st.seen["mapMoons"] || !st.seen["mapAsteroidBelts"] || !st.seen["mapStargates"] {
		return
	}
	add := func(list []jlCelestial) {
		for _, c := range list {
			parsed.celestials = append(parsed.celestials, sdeCelestialRow{itemID: c.id, systemID: c.SolarSystemID, typeID: c.TypeID, radius: c.Radius, name: names[c.id]})
		}
	}
	add(st.stars)
	add(st.planets)
	add(st.moons)
	add(st.belts)
	for _, g := range st.gates {
		parsed.celestials = append(parsed.celestials, sdeCelestialRow{
			itemID: g.StargateID, systemID: g.SolarSystemID, typeID: g.TypeID,
			name: "Stargate (" + st.systemName[g.Destination.SolarSystemID] + ")",
		})
	}
}

// romanNumeral writes n (1 to 3999) in Roman numerals, the way the
// game numbers planets.
func romanNumeral(n int64) string {
	if n <= 0 || n >= 4000 {
		return strconv.FormatInt(n, 10)
	}
	vals := []int64{1000, 900, 500, 400, 100, 90, 50, 40, 10, 9, 5, 4, 1}
	syms := []string{"M", "CM", "D", "CD", "C", "XC", "L", "XL", "X", "IX", "V", "IV", "I"}
	var b strings.Builder
	for i, v := range vals {
		for n >= v {
			b.WriteString(syms[i])
			n -= v
		}
	}
	return b.String()
}
