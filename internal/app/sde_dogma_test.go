package app

// Importer tests for the dogma tables: attribute types, effects with
// their modifiers, type dogma (attributes and effect links), and the
// store round-trip into the sde_* dogma tables.

import (
	"context"
	"strings"
	"testing"
)

func TestSDEDogmaParsers(t *testing.T) {
	// dogmaAttributes: names, stackable flag, defaults.
	attrs := `{"attributeID":263,"name":"shieldCapacity","iconID":1,"defaultValue":0,"published":true,"displayName":{"en":"Shield HP"},"unitID":113,"stackable":true,"highIsGood":true,"attributeCategoryID":1}
{"attributeID":267,"name":"armorEmDamageResonance","iconID":2,"defaultValue":1,"published":true,"displayName":{"en":"EM damage resistance"},"unitID":112,"stackable":false,"highIsGood":false,"attributeCategoryID":1}
`
	var parsed parsedSDE
	if err := parseSDEJSONLTable("dogmaAttributes", strings.NewReader(attrs), &parsed); err != nil {
		t.Fatalf("parse attribute types: %v", err)
	}
	if len(parsed.attrTypes) != 2 {
		t.Fatalf("parsed %d attribute types, want 2", len(parsed.attrTypes))
	}
	if got := parsed.attrTypes[0]; got.attributeID != 263 || got.name != "shieldCapacity" || got.stackable != 1 || got.unitID != 113 {
		t.Fatalf("attribute type 0 = %+v, want 263 shieldCapacity stackable", got)
	}
	if got := parsed.attrTypes[1]; got.stackable != 0 || got.highIsGood != 0 || got.defaultValue != 1 {
		t.Fatalf("attribute type 1 = %+v, want non-stackable default 1", got)
	}

	// dogmaEffects: every effect lands (the fitting engine reads
	// module states from effect categories, and armorRepair /
	// shieldBoosting carry no modifiers), modifiers decode, and
	// EffectStopper-only entries (no operation) store no modifier.
	effects := `{"effectID":21,"name":"shieldCapacityBonusOnline","effectCategoryID":4,"modifierInfo":[{"domain":"shipID","func":"ItemModifier","modifiedAttributeID":263,"modifyingAttributeID":72,"operation":2}]}
{"effectID":34,"name":"projectileFired","effectCategoryID":2}
{"effectID":5928,"name":"warpScrambleTargetMWDBlockActivationForEntity","effectCategoryID":1,"modifierInfo":[{"domain":"target","func":"EffectStopper"}]}
{"effectID":92,"name":"projectileWeaponDamageMultiply","effectCategoryID":4,"modifierInfo":[{"domain":"shipID","func":"LocationGroupModifier","groupID":55,"modifiedAttributeID":64,"modifyingAttributeID":64,"operation":4}]}
`
	if err := parseSDEJSONLTable("dogmaEffects", strings.NewReader(effects), &parsed); err != nil {
		t.Fatalf("parse effects: %v", err)
	}
	if len(parsed.effects) != 4 {
		t.Fatalf("parsed %d effects, want all 4", len(parsed.effects))
	}
	byID := map[int64]sdeEffectRow{}
	for _, e := range parsed.effects {
		byID[e.effectID] = e
	}
	if got := byID[34]; got.name != "projectileFired" || got.category != 2 {
		t.Fatalf("modifier-less effect 34 = %+v, want projectileFired category 2", got)
	}
	if got := byID[5928]; got.category != 1 {
		t.Fatalf("EffectStopper effect 5928 = %+v, want category 1 kept", got)
	}
	if len(parsed.modifiers) != 2 {
		t.Fatalf("parsed %d modifiers, want 2", len(parsed.modifiers))
	}
	m0 := parsed.modifiers[0]
	if m0.effectID != 21 || m0.domain != "shipID" || m0.fn != "ItemModifier" ||
		m0.modifiedAttr != 263 || m0.modifyingAttr != 72 || m0.operation != 2 {
		t.Fatalf("modifier 0 = %+v, want effect 21 shield add", m0)
	}
	m1 := parsed.modifiers[1]
	if m1.effectID != 92 || m1.fn != "LocationGroupModifier" || m1.groupID != 55 || m1.operation != 4 {
		t.Fatalf("modifier 1 = %+v, want effect 92 group modifier", m1)
	}

	// typeDogma: effect links keep their default flag, and every
	// attribute is kept for fitting, not just the skill graph's
	// dozen: attribute 999 (outside the skill filter) must reach
	// the fitting rows while the skill-graph map holds only the
	// wanted pairs.
	typeDogma := `{"typeID":377,"dogmaEffects":[{"effectID":21,"isDefault":true},{"effectID":16,"isDefault":true}]}
{"typeID":519,"dogmaEffects":[{"effectID":92,"isDefault":false}]}
{"typeID":597,"dogmaAttributes":[{"attributeID":275,"value":1},{"attributeID":999,"value":42},{"attributeID":72,"value":400.5}]}
`
	if err := parseSDEJSONLTable("typeDogma", strings.NewReader(typeDogma), &parsed); err != nil {
		t.Fatalf("parse type dogma: %v", err)
	}
	if len(parsed.typeEffects) != 3 || parsed.typeEffects[0].isDefault != 1 || parsed.typeEffects[2].isDefault != 0 {
		t.Fatalf("type effects = %+v, want 3 rows with defaults 1,1,0", parsed.typeEffects)
	}
	if len(parsed.typeAttrs) != 3 {
		t.Fatalf("kept %d type attribute rows, want all 3", len(parsed.typeAttrs))
	}
	if got := parsed.dogma[597][275]; got != 1.0 {
		t.Fatalf("skill-graph view has 275 = %v, want 1", got)
	}
	if _, ok := parsed.dogma[597][999]; ok {
		t.Fatal("skill-graph view leaked non-skill attribute 999")
	}
	found999 := false
	for _, r := range parsed.typeAttrs {
		if r.attributeID == 999 && r.value == 42 {
			found999 = true
		}
	}
	if !found999 {
		t.Fatal("fitting rows are missing the non-skill attribute 999")
	}
}

func TestSDEDogmaStoreRoundTrip(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()

	parsed := &parsedSDE{}
	parsed.types = []sdeTypeRow{
		{typeID: 597, name: "Fixture Punisher", groupID: 25, published: 1, mass: 1190000, volume: 28600, capacity: 275},
	}
	parsed.attrTypes = []sdeAttributeTypeRow{
		{attributeID: 263, name: "shieldCapacity", stackable: 1, highIsGood: 1, unitID: 113, defaultValue: 0},
		{attributeID: 267, name: "armorEmDamageResonance", stackable: 0, highIsGood: 0, unitID: 112, defaultValue: 1},
	}
	parsed.typeAttrs = []sdeTypeAttributeRow{
		{typeID: 597, attributeID: 263, value: 350},
		{typeID: 597, attributeID: 267, value: 0.5},
		{typeID: 377, attributeID: 72, value: 400},
	}
	parsed.effects = []sdeEffectRow{{effectID: 21, name: "shieldCapacityBonusOnline", category: 4}}
	parsed.modifiers = []sdeEffectModifierRow{
		{effectID: 21, domain: "shipID", fn: "ItemModifier", modifiedAttr: 263, modifyingAttr: 72, operation: 2},
	}
	parsed.typeEffects = []sdeTypeEffectRow{
		{typeID: 377, effectID: 21, isDefault: 1},
	}
	parsed.titles = []sdeCharacterTitleRow{
		{titleID: "08dfebb0-b4ff-593d-b78e-b42a963f7847", name: "Arch-Luminary"},
	}

	if _, err := app.storeSDE(ctx, "fixture", parsed); err != nil {
		t.Fatalf("store SDE: %v", err)
	}

	if n, err := q.CountSDETypeAttributes(ctx); err != nil || n != 3 {
		t.Fatalf("CountSDETypeAttributes = %d, %v; want 3", n, err)
	}
	if n, err := q.CountSDEAttributeTypes(ctx); err != nil || n != 2 {
		t.Fatalf("CountSDEAttributeTypes = %d, %v; want 2", n, err)
	}
	if n, err := q.CountSDEEffects(ctx); err != nil || n != 1 {
		t.Fatalf("CountSDEEffects = %d, %v; want 1", n, err)
	}
	if n, err := q.CountSDEEffectModifiers(ctx); err != nil || n != 1 {
		t.Fatalf("CountSDEEffectModifiers = %d, %v; want 1", n, err)
	}
	if n, err := q.CountSDETypeEffects(ctx); err != nil || n != 1 {
		t.Fatalf("CountSDETypeEffects = %d, %v; want 1", n, err)
	}

	attrs, err := q.ListSDETypeAttributes(ctx, 597)
	if err != nil || len(attrs) != 2 || attrs[0].AttributeID != 263 || attrs[0].Value != 350 {
		t.Fatalf("ListSDETypeAttributes(597) = %+v, %v", attrs, err)
	}
	if title, err := q.GetSDECharacterTitle(ctx, "08dfebb0-b4ff-593d-b78e-b42a963f7847"); err != nil || title != "Arch-Luminary" {
		t.Fatalf("GetSDECharacterTitle = %q, %v; want Arch-Luminary", title, err)
	}
	meta, err := q.GetSDEAttributeType(ctx, 267)
	if err != nil || meta.Stackable != 0 || meta.DefaultValue != 1 {
		t.Fatalf("GetSDEAttributeType(267) = %+v, %v", meta, err)
	}
	mods, err := q.ListSDEEffectModifiers(ctx, 21)
	if err != nil || len(mods) != 1 || mods[0].ModifiedAttr != 263 || mods[0].Operation != 2 {
		t.Fatalf("ListSDEEffectModifiers(21) = %+v, %v", mods, err)
	}
	links, err := q.ListSDETypeEffects(ctx, 377)
	if err != nil || len(links) != 1 || links[0].EffectID != 21 {
		t.Fatalf("ListSDETypeEffects(377) = %+v, %v", links, err)
	}
	phys, err := q.ListSDETypePhysicsByIDs(ctx, []int64{597})
	if err != nil || len(phys) != 1 || phys[0].Mass != 1190000 || phys[0].Capacity != 275 {
		t.Fatalf("ListSDETypePhysicsByIDs(597) = %+v, %v", phys, err)
	}
	if ver, _ := app.sdeMeta(ctx, "sde_import_version"); ver != sdeImportVersion {
		t.Fatalf("sde_import_version = %q, want 10", ver)
	}
}
