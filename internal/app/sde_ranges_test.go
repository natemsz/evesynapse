package app

// Mutaplasmid roll ranges (schema 055): the dump's
// dynamicItemAttributes.csv is the ground truth the abyssal
// corpus divides by, so its parse is pinned to the live
// dataset's rows for a real mutator, and the store proves an
// import lands them -- and that an import without the file
// leaves stored ranges untouched instead of wiping them.

import (
	"bytes"
	"context"
	"encoding/csv"
	"sort"
	"strings"
	"testing"
)

// sdeRangesFixtureCSV carries the live dataset's rows for
// mutator 47735 (Gravid Warp Disruptor Mutaplasmid), one
// malformed row (skipped whole), and one other mutator.
const sdeRangesFixtureCSV = "typeID,attributeID,min,max,highIsGood\n" +
	"47735,6,1.0,2.0,0\n" +
	"47735,50,0.85,1.3,0\n" +
	"47735,54,0.9,1.15,1\n" +
	"47735,11,notanumber,2.0,0\n" +
	"47744,54,0.85,1.1,1\n"

func parseRangesFixture(t *testing.T) *parsedSDE {
	t.Helper()
	cr := csv.NewReader(bytes.NewReader([]byte(sdeRangesFixtureCSV)))
	cr.FieldsPerRecord = -1
	head, err := cr.Read()
	if err != nil {
		t.Fatalf("fixture header: %v", err)
	}
	idx := make(map[string]int, len(head))
	for i, name := range head {
		idx[strings.TrimSpace(name)] = i
	}
	parsed := &parsedSDE{}
	if err := parseSDEDynRanges(cr, idx, parsed); err != nil {
		t.Fatalf("parse ranges: %v", err)
	}
	return parsed
}

func TestParseSDEDynRanges(t *testing.T) {
	parsed := parseRangesFixture(t)
	if len(parsed.dynRanges) != 4 {
		t.Fatalf("parsed ranges = %d, want 4 (the malformed row skipped)", len(parsed.dynRanges))
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
	parsed.markers = map[string]fileMarker{
		sdeRangesFileName: {etag: "ranges-etag", lastModified: "ranges-build-1"},
	}
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
	if ver, _ := app.sdeMeta(ctx, "sde_import_version"); ver != "9" {
		t.Fatalf("sde_import_version = %q, want 9", ver)
	}

	// An import whose dump lacked the dataset must not wipe
	// the stored ranges: absence is reported, not destructive.
	empty := &parsedSDE{markers: map[string]fileMarker{}}
	if _, err := app.storeSDE(ctx, "fixture-base", empty); err != nil {
		t.Fatalf("store SDE without ranges: %v", err)
	}
	if n, err := q.CountAbyssalRanges(ctx); err != nil || n != 4 {
		t.Fatalf("ranges after a rangeless import = %d, %v; want 4 kept", n, err)
	}
}
