package store

import (
	"database/sql"
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

// rfc3339 prints a time the way the fixtures write one: RFC 3339,
// UTC, to the second.
func rfc3339(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// rfc3339Or is rfc3339 for a nullable column, with fallback standing
// in for NULL.
func rfc3339Or(t sql.NullTime, fallback string) string {
	if !t.Valid {
		return fallback
	}
	return rfc3339(t.Time)
}

// timeSet wraps t for a nullable column.
func timeSet(t time.Time) sql.NullTime {
	return sql.NullTime{Time: t, Valid: true}
}
