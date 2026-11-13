package card

import (
	"bytes"
	"testing"
)

// sparkCard is a market-shaped card: a title, a figure row and,
// when values are given, a price history line.
func sparkCard(values []float64) Card {
	c := Card{
		Kind:     "MARKET",
		Title:    "Tritanium",
		Subtitle: "Domain",
		Stats: []Stat{
			{Label: "SELL", Value: Figure("4.60")},
			{Label: "BUY", Value: Figure("4.10")},
		},
		Mark: true,
	}
	if values != nil {
		c.Spark = &Spark{Label: "PRICE HISTORY", Values: values}
	}
	return c
}

func TestRenderSparkDrawsTheLine(t *testing.T) {
	plain, err := PNG(sparkCard(nil))
	if err != nil {
		t.Fatal(err)
	}
	vals := []float64{4.0, 4.1, 4.05, 4.3, 4.25, 4.5, 4.6}
	img := Render(sparkCard(vals))
	drawn, err := PNG(sparkCard(vals))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(plain, drawn) {
		t.Fatal("a card with a sparkline rendered identically to one without")
	}
	// The line itself is drawn in the figure colour somewhere in
	// the band under the figures.
	found := false
	for y := int(sparkTop); y < int(sparkBot) && !found; y++ {
		for x := int(colX); x < W-int(margin); x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			if uint8(r>>8) == hiC.R && uint8(g>>8) == hiC.G && uint8(b>>8) == hiC.B {
				found = true
				break
			}
		}
	}
	if !found {
		t.Fatal("no figure-colour line pixel in the sparkline band")
	}
	// A flat series still renders (it sits mid-band).
	_ = Render(sparkCard([]float64{5, 5, 5, 5}))
	// A single point draws no line: the card matches the plain one.
	one, err := PNG(sparkCard([]float64{5}))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(plain, one) {
		t.Fatal("a one-point sparkline changed the card")
	}
}
