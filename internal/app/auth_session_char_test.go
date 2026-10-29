package app

import (
	"context"
	"net/http"
	"net/http/httptest"
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

// TestPickCharacterViewDoesNotPin: a view pick (?view=1 — the
// links notifications and the briefing carry) renders the named
// character's page without moving the session's acting character.
// A plain pick still pins: the character switcher relies on it.
func TestPickCharacterViewDoesNotPin(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Alpha")
	seedCharacter(t, q, user.ID, fixtureCharB, "Fixture Bravo")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Alpha")

	run := func(target string) (active, pin int64) {
		req := httptest.NewRequest(http.MethodGet, "https://example.com"+target, nil)
		req.AddCookie(cookie)
		app.sessions.LoadAndSave(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, a, _, err := app.pickCharacter(r.Context(), r, "/character/")
			if err != nil {
				t.Fatalf("pickCharacter: %v", err)
			}
			active, pin = a.CharacterID, sessionCharID(app.sessions, r.Context())
		})).ServeHTTP(httptest.NewRecorder(), req)
		return active, pin
	}

	if active, pin := run("/character/?character=90000002&view=1"); active != fixtureCharB || pin != fixtureCharA {
		t.Errorf("view pick: active %d, pin %d — want Bravo (%d) shown with the pin still on Alpha (%d)",
			active, pin, fixtureCharB, fixtureCharA)
	}
	if active, pin := run("/character/?character=90000002"); active != fixtureCharB || pin != fixtureCharB {
		t.Errorf("plain pick: active %d, pin %d — want both on Bravo (%d)", active, pin, fixtureCharB)
	}
}
