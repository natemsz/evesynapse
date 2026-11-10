// Package itemsheet turns one type's static data into the sheet the
// item page, the public ship page and the fitting workbench's
// tooltips and info drawer show: bonuses, attributes in the game's
// sections with the game's units, and required skills. Every read is
// by the type's key (a type carries at most about a hundred
// attributes), so building a sheet never scans a table.
package itemsheet

import (
	"context"
	"errors"
	"fmt"
	"html"
	"html/template"
	"regexp"
	"strconv"
	"strings"

	db "evesynapse/internal/db/sqlc"

	"github.com/jackc/pgx/v5"
)

// Querier is the part of the generated queries a sheet reads.
type Querier interface {
	GetSDEType(ctx context.Context, typeID int64) (db.SdeType, error)
	GetSDEGroup(ctx context.Context, groupID int64) (db.SdeGroup, error)
	ListItemSheetAttributes(ctx context.Context, typeID int64) ([]db.ListItemSheetAttributesRow, error)
	ListItemSheetTraits(ctx context.Context, typeID int64) ([]db.ListItemSheetTraitsRow, error)
	ListSDERequirementsByTypes(ctx context.Context, typeIds []int64) ([]db.SdeRequirement, error)
	ListSDETypeNamesByIDs(ctx context.Context, typeIds []int64) ([]db.ListSDETypeNamesByIDsRow, error)
	ListSDEGroupNamesByIDs(ctx context.Context, groupIds []int64) ([]db.ListSDEGroupNamesByIDsRow, error)
}

// ErrNotFound means the static data has no such type.
var ErrNotFound = errors.New("itemsheet: unknown type")

// Sheet is one type's stats, ready to render.
type Sheet struct {
	TypeID     int64
	Name       string
	GroupID    int64
	GroupName  string
	CategoryID int64

	Bonuses  []BonusBlock
	Sections []Section
	Skills   []SkillLine

	values map[int64]float64
}

// BonusBlock is one heading of ship bonuses: per level of a skill,
// the role bonus, or other bonuses.
type BonusBlock struct {
	SkillID int64 // -1 role bonus, -2 other bonuses
	Title   string
	Lines   []BonusLine
}

// BonusLine is one bonus: the figure ("10%") and the game's text,
// with its item links pointing at our item pages.
type BonusLine struct {
	Figure string
	Text   template.HTML
}

// Section is one group of attributes under the game's heading.
type Section struct {
	Title string
	Rows  []Row
}

// Row is one attribute as shown. Link is set when the value names a
// type or a group.
type Row struct {
	AttributeID int64
	Label       string
	Value       string
	Link        string
	Raw         float64
	HighIsGood  bool

	names int64 // unitTypeID or unitGroupID: Value is named after the read
}

// SkillLine is one required skill; Depth 0 is the type's own
// requirement, deeper lines are that skill's prerequisites.
type SkillLine struct {
	SkillID int64
	Name    string
	Level   int64
	Depth   int
}

// Value returns a shown attribute's raw value.
func (s *Sheet) Value(attributeID int64) (float64, bool) {
	v, ok := s.values[attributeID]
	return v, ok
}

// Attribute categories from the dump's dgmAttributeCategories, mapped
// to the sections the sheet shows, in display order.
var sectionOrder = []string{"Fitting", "Structure", "Armor", "Shield", "Capacitor", "Targeting", "Navigation", "Drones", "Weapon", "Miscellaneous"}

var sectionOfCategory = map[int64]string{
	1:  "Fitting",
	2:  "Shield",
	3:  "Armor",
	4:  "Structure",
	5:  "Capacitor",
	6:  "Targeting",
	10: "Drones",
	17: "Navigation",
	29: "Weapon",
	30: "Weapon",
	34: "Weapon",
	38: "Drones",
	39: "Weapon",
	40: "Structure",
}

// Categories the sheet never shows: required skills (shown as the
// skill tree instead), the dump's unsorted bucket, and the NPC-only
// AI, loot, graphics, reward and behavior attributes.
var hiddenCategory = map[int64]bool{8: true, 9: true, 12: true, 19: true, 31: true, 32: true, 33: true, 42: true}

// Attributes shown although their category is hidden.
var shownAnyway = map[int64]string{
	1544: "Fitting", // max modules of this group fitted
}

// The skill tree stops at five levels of prerequisites and sixty
// lines, more than any type in the game needs.
const (
	maxSkillDepth = 5
	maxSkillLines = 60
)

// Load builds the sheet for one type.
func Load(ctx context.Context, q Querier, typeID int64) (*Sheet, error) {
	t, err := q.GetSDEType(ctx, typeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("itemsheet: type %d: %w", typeID, err)
	}
	sh := &Sheet{TypeID: t.TypeID, Name: t.Name, GroupID: t.GroupID, values: map[int64]float64{}}
	if g, err := q.GetSDEGroup(ctx, t.GroupID); err == nil {
		sh.GroupName, sh.CategoryID = g.Name, g.CategoryID
	}

	attrs, err := q.ListItemSheetAttributes(ctx, typeID)
	if err != nil {
		return nil, fmt.Errorf("itemsheet: attributes %d: %w", typeID, err)
	}
	if err := sh.buildSections(ctx, q, attrs); err != nil {
		return nil, err
	}

	traits, err := q.ListItemSheetTraits(ctx, typeID)
	if err != nil {
		return nil, fmt.Errorf("itemsheet: traits %d: %w", typeID, err)
	}
	sh.Bonuses = buildBonuses(traits)

	if err := sh.buildSkills(ctx, q); err != nil {
		return nil, err
	}
	return sh, nil
}

func (sh *Sheet) buildSections(ctx context.Context, q Querier, attrs []db.ListItemSheetAttributesRow) error {
	bySection := map[string][]Row{}
	var typeIDs, groupIDs []int64
	for _, a := range attrs {
		section, ok := shownAnyway[a.AttributeID]
		if !ok {
			if hiddenCategory[a.CategoryID] {
				continue
			}
			section = sectionOfCategory[a.CategoryID]
			if section == "" {
				section = "Miscellaneous"
			}
		}
		if a.DisplayName == "" {
			continue
		}
		if a.Value == 0 && a.DisplayWhenZero == 0 {
			continue
		}
		sh.values[a.AttributeID] = a.Value
		row := Row{AttributeID: a.AttributeID, Label: a.DisplayName, Raw: a.Value, HighIsGood: a.HighIsGood != 0}
		switch a.UnitID {
		case unitTypeID:
			typeIDs = append(typeIDs, int64(a.Value))
			row.names = unitTypeID
		case unitGroupID:
			groupIDs = append(groupIDs, int64(a.Value))
			row.names = unitGroupID
		default:
			row.Value = FormatValue(a.UnitID, a.UnitName, a.Value)
		}
		bySection[section] = append(bySection[section], row)
	}

	typeNames, groupNames := map[int64]string{}, map[int64]string{}
	if len(typeIDs) > 0 {
		rows, err := q.ListSDETypeNamesByIDs(ctx, typeIDs)
		if err != nil {
			return fmt.Errorf("itemsheet: type names: %w", err)
		}
		for _, r := range rows {
			typeNames[r.TypeID] = r.Name
		}
	}
	if len(groupIDs) > 0 {
		rows, err := q.ListSDEGroupNamesByIDs(ctx, groupIDs)
		if err != nil {
			return fmt.Errorf("itemsheet: group names: %w", err)
		}
		for _, r := range rows {
			groupNames[r.GroupID] = r.Name
		}
	}

	for _, title := range sectionOrder {
		rows := bySection[title]
		kept := rows[:0]
		for _, r := range rows {
			switch r.names {
			case unitTypeID:
				id := int64(r.Raw)
				name, ok := typeNames[id]
				if !ok {
					continue
				}
				r.Value, r.Link = name, "/items/type/"+strconv.FormatInt(id, 10)+"/"
			case unitGroupID:
				name, ok := groupNames[int64(r.Raw)]
				if !ok {
					continue
				}
				r.Value = name
			}
			kept = append(kept, r)
		}
		if len(kept) > 0 {
			sh.Sections = append(sh.Sections, Section{Title: title, Rows: kept})
		}
	}
	return nil
}

func buildBonuses(traits []db.ListItemSheetTraitsRow) []BonusBlock {
	var blocks []BonusBlock
	for _, t := range traits {
		if len(blocks) == 0 || blocks[len(blocks)-1].SkillID != t.SkillID {
			title := "Other bonuses"
			switch {
			case t.SkillID == -1:
				title = "Role bonus"
			case t.SkillID > 0 && t.SkillName != "":
				title = t.SkillName + " bonuses, per level"
			case t.SkillID > 0:
				title = "Skill bonuses, per level"
			}
			blocks = append(blocks, BonusBlock{SkillID: t.SkillID, Title: title})
		}
		line := BonusLine{Text: TraitHTML(t.BonusText)}
		if t.Bonus.Valid {
			line.Figure = traitFigure(t.UnitID, t.UnitName, t.Bonus.Float64)
		}
		b := &blocks[len(blocks)-1]
		b.Lines = append(b.Lines, line)
	}
	return blocks
}

// traitFigure renders a bonus figure: traits store the plain number
// with the unit beside it ("10" and %), not the dogma encoding.
func traitFigure(unitID int64, unitName string, v float64) string {
	switch unitID {
	case unitPercent:
		return Number(v) + "%"
	case unitMultiplier:
		return Number(v) + "x"
	case unitBonus:
		return Signed(v)
	case 0:
		return Number(v)
	}
	return FormatValue(unitID, unitName, v)
}

var (
	traitLink = regexp.MustCompile(`(?is)<a\s+href=["']?showinfo:(\d+)[^>]*>(.*?)</a>`)
	anyTag    = regexp.MustCompile(`(?s)<[^>]*>`)
)

// TraitHTML turns the game's bonus text into safe HTML: its showinfo
// links become links to our item pages, every other tag is dropped,
// and all text is escaped.
func TraitHTML(raw string) template.HTML {
	var b strings.Builder
	last := 0
	for _, m := range traitLink.FindAllStringSubmatchIndex(raw, -1) {
		b.WriteString(plainText(raw[last:m[0]]))
		id := raw[m[2]:m[3]]
		label := plainText(raw[m[4]:m[5]])
		b.WriteString(`<a href="/items/type/` + id + `/">` + label + `</a>`)
		last = m[1]
	}
	b.WriteString(plainText(raw[last:]))
	return template.HTML(b.String())
}

// plainText strips tags from game text and escapes the rest. The
// dump's text is already entity-free apart from the odd &amp;, which
// is unescaped first so it is not escaped twice.
func plainText(s string) string {
	return html.EscapeString(html.UnescapeString(anyTag.ReplaceAllString(s, "")))
}

func (sh *Sheet) buildSkills(ctx context.Context, q Querier) error {
	children := map[int64][]db.SdeRequirement{}
	pending := []int64{sh.TypeID}
	seen := map[int64]bool{sh.TypeID: true}
	var skillIDs []int64
	for depth := 0; depth < maxSkillDepth && len(pending) > 0; depth++ {
		reqs, err := q.ListSDERequirementsByTypes(ctx, pending)
		if err != nil {
			return fmt.Errorf("itemsheet: requirements: %w", err)
		}
		pending = nil
		for _, r := range reqs {
			children[r.TypeID] = append(children[r.TypeID], r)
			if !seen[r.SkillTypeID] {
				seen[r.SkillTypeID] = true
				pending = append(pending, r.SkillTypeID)
				skillIDs = append(skillIDs, r.SkillTypeID)
			}
		}
	}
	if len(skillIDs) == 0 {
		return nil
	}
	names := map[int64]string{}
	rows, err := q.ListSDETypeNamesByIDs(ctx, skillIDs)
	if err != nil {
		return fmt.Errorf("itemsheet: skill names: %w", err)
	}
	for _, r := range rows {
		names[r.TypeID] = r.Name
	}
	var walk func(id int64, depth int, path map[int64]bool)
	walk = func(id int64, depth int, path map[int64]bool) {
		for _, r := range children[id] {
			if path[r.SkillTypeID] || len(sh.Skills) >= maxSkillLines {
				continue
			}
			sh.Skills = append(sh.Skills, SkillLine{SkillID: r.SkillTypeID, Name: names[r.SkillTypeID], Level: r.Level, Depth: depth})
			path[r.SkillTypeID] = true
			walk(r.SkillTypeID, depth+1, path)
			delete(path, r.SkillTypeID)
		}
	}
	walk(sh.TypeID, 0, map[int64]bool{sh.TypeID: true})
	return nil
}
