package app

// Mutaplasmid roll ranges (schema 055): the SDE's
// dynamicItemAttributes table is the ground truth the abyssal
// corpus divides by, so its parse is pinned to the live
// dataset's rows for a real mutator, and the store proves an
// import lands them -- and that an import without the table
// leaves stored ranges untouched instead of wiping them.

import (
	"context"
	"sort"
	"strings"
	"testing"
)

// sdeRangesFixture carries the live dataset's ranges for mutator
// 47735 (Gravid Warp Disruptor Mutaplasmid), one entry missing its
// min (skipped), and one other mutator.
const sdeRangesFixture = `{"typeID":47735,"attributeIDs":[{"attributeID":6,"min":1.0,"max":2.0},{"attributeID":50,"min":0.85,"max":1.3},{"attributeID":54,"min":0.9,"max":1.15},{"attributeID":11,"max":2.0}]}
{"typeID":47744,"attributeIDs":[{"attributeID":54,"min":0.85,"max":1.1}]}
`

func parseRangesFixture(t *testing.T) *parsedSDE {
	t.Helper()
	parsed := &parsedSDE{}
	if err := parseSDEJSONLTable("dynamicItemAttributes", strings.NewReader(sdeRangesFixture), parsed); err != nil {
		t.Fatalf("parse ranges: %v", err)
	}
	return parsed
}

func TestParseSDEDynRanges(t *testing.T) {
	parsed := parseRangesFixture(t)
	if len(parsed.dynRanges) != 4 {
		t.Fatalf("parsed ranges = %d, want 4 (the entry without a min skipped)", len(parsed.dynRanges))
	}
	var got []sdeDynRangeRow
	for _, r := range parsed.dynRanges {
		if r.mutatorTypeID == 47735 {
			got = append(got, r)
		}
	}
	sort.Slice(got, func(i, j int) bool { return got[i].attributeID < got[j].attributeID })
	want := []sdeDynRangeRow{
		{mutatorTypeID: 47735, attributeID: 6, minMult: 1.0, maxMult: 2.0},
		{mutatorTypeID: 47735, attributeID: 50, minMult: 0.85, maxMult: 1.3},
		{mutatorTypeID: 47735, attributeID: 54, minMult: 0.9, maxMult: 1.15},
	}
	if len(got) != len(want) {
		t.Fatalf("ranges for 47735 = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("range %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestSDERangesStoreAndKeep(t *testing.T) {
	app, _, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()

	parsed := parseRangesFixture(t)
	parsed.build = "ranges-build-1"
	if _, err := app.storeSDE(ctx, "fixture-base", parsed); err != nil {
		t.Fatalf("store SDE with ranges: %v", err)
	}
	rows, err := q.ListAbyssalRangesForMutator(ctx, 47735)
	if err != nil || len(rows) != 3 {
		t.Fatalf("stored ranges for 47735 = %d, %v; want 3", len(rows), err)
	}
	want := map[int64][2]float64{6: {1.0, 2.0}, 50: {0.85, 1.3}, 54: {0.9, 1.15}}
	for _, r := range rows {
		w := want[r.AttributeID]
		if r.MinMult != w[0] || r.MaxMult != w[1] {
			t.Fatalf("range attr %d = %v..%v, want %v..%v", r.AttributeID, r.MinMult, r.MaxMult, w[0], w[1])
		}
		if r.SdeBuild != "ranges-build-1" {
			t.Fatalf("range sde_build = %q, want the file marker", r.SdeBuild)
		}
	}
	if ver, _ := app.sdeMeta(ctx, "sde_import_version"); ver != sdeImportVersion {
		t.Fatalf("sde_import_version = %q, want 10", ver)
	}

	// An import whose dump lacked the dataset must not wipe
	// the stored ranges: absence is reported, not destructive.
	empty := &parsedSDE{}
	if _, err := app.storeSDE(ctx, "fixture-base", empty); err != nil {
		t.Fatalf("store SDE without ranges: %v", err)
	}
	if n, err := q.CountAbyssalRanges(ctx); err != nil || n != 4 {
		t.Fatalf("ranges after a rangeless import = %d, %v; want 4 kept", n, err)
	}
}
