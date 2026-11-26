package appraise

import (
	"testing"
)

func TestSnapshotHasItemLines(t *testing.T) {
	if snapshotHasItemLines(nil) || snapshotHasItemLines(&snapshot{}) {
		t.Error("an empty snapshot must not be storable")
	}
	if !snapshotHasItemLines(&snapshot{Lines: []snapLine{{TypeID: 34}}}) {
		t.Error("a snapshot with a resolved line must be storable")
	}
}
