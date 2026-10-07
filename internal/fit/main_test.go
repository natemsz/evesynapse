package fit

import (
	"os"
	"testing"

	"evesynapse/internal/pgtest"
)

// TestMain wires the package's tests to the shared embedded
// Postgres (started lazily by the first pgtest.FreshDSN call,
// stopped on the way out). Only the snapshot-loader tests use it;
// the calibration tests run on hand-built data.
func TestMain(m *testing.M) {
	os.Exit(pgtest.TestMain(m))
}
