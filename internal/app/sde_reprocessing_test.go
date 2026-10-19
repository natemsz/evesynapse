package app

import (
	"strings"
	"testing"
)

// The reprocessing import: invTypeMaterials rows land as
// (type, material, quantity) outputs, and invTypes carries
// each type's portion size into its physics row.
func TestSDEReprocessingParse(t *testing.T) {
	parsed := parsedSDE{markers: map[string]fileMarker{}}
	if err := parseSDEFile("invTypeMaterials.csv", strings.NewReader(
		"typeID,materialTypeID,quantity\n34,34,0\n12345,34,700\n12345,35,300\nbad,1,1\n"), &parsed); err != nil {
		t.Fatalf("parse invTypeMaterials: %v", err)
	}
	if len(parsed.reprocessing) != 2 {
		t.Fatalf("reprocessing rows = %+v, want the two valid outputs", parsed.reprocessing)
	}
	if parsed.reprocessing[0].typeID != 12345 || parsed.reprocessing[0].materialTypeID != 34 || parsed.reprocessing[0].quantity != 700 {
		t.Errorf("first row = %+v, want 12345 -> 34 x700", parsed.reprocessing[0])
	}

	parsed = parsedSDE{markers: map[string]fileMarker{}}
	if err := parseSDEFile("invTypes.csv", strings.NewReader(
		"typeID,typeName,groupID,marketGroupID,published,mass,volume,capacity,portionSize,description\n12345,Veldspar,25,1850,1,0,0.1,0,100,\n"), &parsed); err != nil {
		t.Fatalf("parse invTypes: %v", err)
	}
	if len(parsed.types) != 1 || parsed.types[0].portionSize != 100 || parsed.types[0].volume != 0.1 {
		t.Fatalf("types = %+v, want Veldspar with portion 100 and volume 0.1", parsed.types)
	}
}
