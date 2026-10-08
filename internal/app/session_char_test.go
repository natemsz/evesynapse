package app

import (
	"context"
	"testing"

	"github.com/alexedwards/scs/v2"
)

// TestSessionCharIDRoundTrip: the acting character survives the
// session as a full int64. EVE IDs reach past 32 bits, so the old
// int() conversion corrupted them on 32-bit builds.
func TestSessionCharIDRoundTrip(t *testing.T) {
	sm := scs.New()
	ctx, err := sm.Load(context.Background(), "")
	if err != nil {
		t.Fatalf("session load: %v", err)
	}
	const big = int64(9_876_543_210)
	putSessionCharID(sm, ctx, big)
	if got := sessionCharID(sm, ctx); got != big {
		t.Fatalf("sessionCharID = %d, want %d", got, big)
	}
}

// TestSessionCharIDLegacyInt: sessions written before the int64
// switch (a plain int) still read.
func TestSessionCharIDLegacyInt(t *testing.T) {
	sm := scs.New()
	ctx, err := sm.Load(context.Background(), "")
	if err != nil {
		t.Fatalf("session load: %v", err)
	}
	sm.Put(ctx, sessionCharacterID, int(95465499))
	if got := sessionCharID(sm, ctx); got != 95465499 {
		t.Fatalf("sessionCharID = %d, want 95465499", got)
	}
}

// TestSessionCharIDMissing: no acting character reads as zero,
// like the old GetInt did.
func TestSessionCharIDMissing(t *testing.T) {
	sm := scs.New()
	ctx, err := sm.Load(context.Background(), "")
	if err != nil {
		t.Fatalf("session load: %v", err)
	}
	if got := sessionCharID(sm, ctx); got != 0 {
		t.Fatalf("sessionCharID = %d, want 0", got)
	}
}
