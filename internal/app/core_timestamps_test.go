package app

import (
	"database/sql"
	"testing"
	"time"
)

// mustTime reads an RFC 3339 fixture time. A fixture that does not
// parse is a mistake in the test itself, so it panics.
func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// mustNullTime is mustTime for a nullable column.
func mustNullTime(s string) sql.NullTime {
	return sql.NullTime{Time: mustTime(s), Valid: true}
}

// TestRFC3339IsUTCToTheSecond: whatever zone or precision a time
// arrives in, the app writes it out one way.
func TestRFC3339IsUTCToTheSecond(t *testing.T) {
	east := time.FixedZone("east", 5*60*60)
	at := time.Date(2026, 10, 6, 17, 30, 15, 987654321, east)
	if got, want := rfc3339(at), "2026-10-06T12:30:15Z"; got != want {
		t.Errorf("rfc3339 = %q, want %q", got, want)
	}
	if got := rfc3339Or(sql.NullTime{}, "—"); got != "—" {
		t.Errorf("an unset time printed as %q, want the fallback", got)
	}
	if got, want := rfc3339Or(timeSet(at), "—"), "2026-10-06T12:30:15Z"; got != want {
		t.Errorf("rfc3339Or = %q, want %q", got, want)
	}
}
