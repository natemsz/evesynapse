package app

import (
	"database/sql"
	"time"
)

// Times live in the database as timestamptz and in the code as
// time.Time, read back in UTC (see readTimesInUTC in db.go). The
// helpers here cover the two places a time still becomes text or
// has to say "never".

// rfc3339 writes a time the way the app shows and exports one: RFC
// 3339, in UTC, to the second.
func rfc3339(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// rfc3339Or is rfc3339 for a nullable column, with fallback standing
// in for a time that was never set.
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
