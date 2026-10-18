package contracts

import "testing"

// TestTypeIconURL pins the image-server variant rule:
// blueprint-category types (reaction formulas included) have
// no /icon asset -- originals serve /bp, copies serve /bpc --
// and every other type serves /icon.
func TestTypeIconURL(t *testing.T) {
	cases := []struct {
		name      string
		typeID    int64
		blueprint bool
		copy      bool
		size      int
		want      string
	}{
		{"plain type", 34, false, false, 32, "https://images.evetech.net/types/34/icon?size=32"},
		{"plain type, copy flag ignored", 34, false, true, 32, "https://images.evetech.net/types/34/icon?size=32"},
		{"blueprint original", 991, true, false, 32, "https://images.evetech.net/types/991/bp?size=32"},
		{"blueprint copy", 991, true, true, 32, "https://images.evetech.net/types/991/bpc?size=32"},
		{"reaction formula original", 46167, true, false, 32, "https://images.evetech.net/types/46167/bp?size=32"},
		{"size passes through", 2048, false, false, 64, "https://images.evetech.net/types/2048/icon?size=64"},
	}
	for _, c := range cases {
		if got := TypeIconURL(c.typeID, c.blueprint, c.copy, c.size); got != c.want {
			t.Errorf("%s: TypeIconURL(%d, %v, %v, %d) = %q, want %q",
				c.name, c.typeID, c.blueprint, c.copy, c.size, got, c.want)
		}
	}
}
