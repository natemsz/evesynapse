package app

import (
	"strings"
	"testing"
)

// The reprocessing import: typeMaterials records land as (type,
// material, quantity) outputs, a zero quantity left out, and types
// carry each type's portion size into its physics row.
func TestSDEReprocessingParse(t *testing.T) {
	parsed := parsedSDE{}
	if err := parseSDEJSONLTable("typeMaterials", strings.NewReader(
		`{"typeID":34,"materials":[{"materialTypeID":34,"quantity":0}]}`+"\n"+
			`{"typeID":12345,"materials":[{"materialTypeID":34,"quantity":700},{"materialTypeID":35,"quantity":300}]}`+"\n"), &parsed); err != nil {
		t.Fatalf("parse typeMaterials: %v", err)
	}
	if len(parsed.reprocessing) != 2 {
		t.Fatalf("reprocessing rows = %+v, want the two valid outputs", parsed.reprocessing)
	}
	if parsed.reprocessing[0].typeID != 12345 || parsed.reprocessing[0].materialTypeID != 34 || parsed.reprocessing[0].quantity != 700 {
		t.Errorf("first row = %+v, want 12345 -> 34 x700", parsed.reprocessing[0])
	}

	parsed = parsedSDE{}
	if err := parseSDEJSONLTable("types", strings.NewReader(
		`{"typeID":12345,"groupID":25,"marketGroupID":1850,"name":{"en":"Veldspar"},"published":true,"volume":0.1,"portionSize":100}`+"\n"), &parsed); err != nil {
		t.Fatalf("parse types: %v", err)
	}
	if len(parsed.types) != 1 || parsed.types[0].portionSize != 100 || parsed.types[0].volume != 0.1 {
		t.Fatalf("types = %+v, want Veldspar with portion 100 and volume 0.1", parsed.types)
	}
}
