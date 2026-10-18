package app

// Audit 1.4, import path: a hostile EFT paste must not turn
// into unbounded database work. ParseEFT bounds the paste
// (64 KiB / 500 lines) and memoizes name lookups, so a 20,000
// line paste resolves against a bounded prefix and the same
// bounded name set.

import (
	"context"
	"strings"
	"testing"
)

func TestFitEFTParseHostileBounds(t *testing.T) {
	app, _ := fitTestApp(t)
	ctx := context.Background()

	text := "[Fixture Frigate, Hostile]\n" +
		strings.Repeat("Fixture Blaster, Fixture Charge S\n", 20000)
	doc, _ := app.fitBuilder().ParseEFT(ctx, text)
	if doc.ShipTypeID != fitShipID {
		t.Fatalf("ship = %d, want %d", doc.ShipTypeID, fitShipID)
	}
	if len(doc.Items) != 1 {
		t.Fatalf("items = %v, want the single merged blaster line", doc.Items)
	}
	qty := doc.Items[0].Qty
	if qty <= 0 || qty > 500 {
		t.Fatalf("blaster qty = %d, want within the 500-line parse bound", qty)
	}
	if doc.Charges[fitBlasterID] != fitChargeID {
		t.Fatalf("charges[blaster] = %d, want %d", doc.Charges[fitBlasterID], fitChargeID)
	}
}

func TestFitEFTParseRepeatedUnknown(t *testing.T) {
	app, _ := fitTestApp(t)
	ctx := context.Background()

	text := "[Fixture Frigate, Unknowns]\n" +
		strings.Repeat("Not A Real Module\n", 600)
	doc, notes := app.fitBuilder().ParseEFT(ctx, text)
	if len(doc.Items) != 0 {
		t.Fatalf("items = %v, want none placed", doc.Items)
	}
	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, "Not A Real Module") {
		t.Fatalf("notes = %v, want the unknown line reported", notes)
	}
}
