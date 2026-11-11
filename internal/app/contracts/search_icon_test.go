package contracts

import "testing"

// TestTypeCategoryIconURL pins the category rule for the
// surfaces that hold a type's SDE category but no contract
// line: blueprint-category types serve /bp, SKIN-category
// types serve nothing on the image server (their /icon,
// /render, /bp and /bpc all 404), so the app placeholder
// stands in, and every other type serves /icon.
func TestTypeCategoryIconURL(t *testing.T) {
	cases := []struct {
		name     string
		typeID   int64
		category int64
		size     int
		want     string
	}{
		{"plain type", 34, 4, 64, "https://images.evetech.net/types/34/icon?size=64"},
		{"module", 2048, 7, 64, "https://images.evetech.net/types/2048/icon?size=64"},
		{"blueprint", 12000, BlueprintCategoryID, 64, "https://images.evetech.net/types/12000/bp?size=64"},
		{"permanent skin", 87513, SkinCategoryID, 64, ItemPlaceholderPath},
		{"unknown category falls back to icon", 34, 0, 32, "https://images.evetech.net/types/34/icon?size=32"},
	}
	for _, c := range cases {
		if got := TypeCategoryIconURL(c.typeID, c.category, c.size); got != c.want {
			t.Errorf("%s: TypeCategoryIconURL(%d, %d, %d) = %q, want %q",
				c.name, c.typeID, c.category, c.size, got, c.want)
		}
	}
}
