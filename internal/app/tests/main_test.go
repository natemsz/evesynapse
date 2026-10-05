package tests

import (
	"os"
	"testing"

	"evesynapse/internal/pgtest"
)

// TestMain wires the black-box page tests to the shared embedded
// Postgres (started lazily by the first pgtest.FreshDSN call,
// stopped on the way out).
func TestMain(m *testing.M) {
	os.Exit(pgtest.TestMain(m))
}
