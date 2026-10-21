package app

// Mutaplasmid applicability mappings (schema 063): the
// dump's dynamicItemAttributeMappings.csv says which base
// types each mutaplasmid can be applied to and the type each
// becomes, so its parse is pinned to the live dataset's rows
// for a real mutator, and the store proves an import lands
// them -- and that an import without the file leaves stored
// mappings untouched instead of wiping them.

import (
	"bytes"
	"context"
	"encoding/csv"
	"sort"
	"strings"
	"testing"
)

// sdeMappingsFixtureCSV carries the live dataset's rows for
// mutator 47733 (Decayed Warp Disruptor Mutaplasmid, whose
// applications become Abyssal Warp Disruptor, 47736), one
// malformed row (skipped whole), and one other mutator.
const sdeMappingsFixtureCSV = "typeID,applicableTypeID,resultingTypeID\n" +
	"47733,3242,47736\n" +
	"47733,3244,47736\n" +
	"47733,5399,47736\n" +
	"47733,notanumber,47736\n" +
	"47297,5975,47408\n"

func parseMappingsFixture(t *testing.T) *parsedSDE {
	t.Helper()
	cr := csv.NewReader(bytes.NewReader([]byte(sdeMappingsFixtureCSV)))
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
	if err := parseSDEDynMappings(cr, idx, parsed); err != nil {
		t.Fatalf("parse mappings: %v", err)
	}
	return parsed
}

func TestParseSDEDynMappings(t *testing.T) {
	parsed := parseMappingsFixture(t)
	if len(parsed.dynMappings) != 4 {
		t.Fatalf("parsed mappings = %d, want 4 (the malformed row skipped)", len(parsed.dynMappings))
	}
	var got []sdeDynMappingRow
	for _, r := range parsed.dynMappings {
		if r.mutatorTypeID == 47733 {
			got = append(got, r)
		}
	}
	sort.Slice(got, func(i, j int) bool { return got[i].applicableTypeID < got[j].applicableTypeID })
	want := []sdeDynMappingRow{
		{mutatorTypeID: 47733, applicableTypeID: 3242, resultingTypeID: 47736},
		{mutatorTypeID: 47733, applicableTypeID: 3244, resultingTypeID: 47736},
		{mutatorTypeID: 47733, applicableTypeID: 5399, resultingTypeID: 47736},
	}
	if len(got) != len(want) {
		t.Fatalf("mappings for 47733 = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("mapping %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestSDEMappingsStoreAndKeep(t *testing.T) {
	app, _, _ := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()

	parsed := parseMappingsFixture(t)
	parsed.markers = map[string]fileMarker{
		sdeMappingsFileName: {etag: "mappings-etag", lastModified: "mappings-build-1"},
	}
	if _, err := app.storeSDE(ctx, "fixture-base", parsed); err != nil {
		t.Fatalf("store SDE with mappings: %v", err)
	}
	var n int
	var build string
	if err := app.db.QueryRowContext(ctx,
		`SELECT count(*), max(sde_build) FROM abyssal_mappings WHERE resulting_type_id = 47736`).Scan(&n, &build); err != nil {
		t.Fatalf("stored mappings for 47736: %v", err)
	}
	if n != 3 {
		t.Fatalf("stored mappings resulting in 47736 = %d, want 3", n)
	}
	if build != "mappings-build-1" {
		t.Fatalf("mapping sde_build = %q, want the file marker", build)
	}
	if ver, _ := app.sdeMeta(ctx, "sde_import_version"); ver != sdeImportVersion {
		t.Fatalf("sde_import_version = %q, want 10", ver)
	}

	// An import whose dump lacked the dataset must not wipe
	// the stored mappings: absence is reported, not
	// destructive.
	empty := &parsedSDE{markers: map[string]fileMarker{}}
	if _, err := app.storeSDE(ctx, "fixture-base", empty); err != nil {
		t.Fatalf("store SDE without mappings: %v", err)
	}
	if err := app.db.QueryRowContext(ctx,
		`SELECT count(*) FROM abyssal_mappings`).Scan(&n); err != nil {
		t.Fatalf("mappings after a mappingless import: %v", err)
	}
	if n != 4 {
		t.Fatalf("mappings after a mappingless import = %d, want 4 kept", n)
	}
}
