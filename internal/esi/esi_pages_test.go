package esi

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestWithPageParam(t *testing.T) {
	if got := withPageParam("/markets/10000002/orders/", 3); got != "/markets/10000002/orders/?page=3" {
		t.Fatalf("plain path: %q", got)
	}
	// A path that already carries a query keeps it: the old code
	// appended a second "?", discarding the first half server-side.
	if got := withPageParam("/x/?type_id=34", 2); got != "/x/?type_id=34&page=2" {
		t.Fatalf("query path: %q", got)
	}
}

// TestFetchAllPagesKeepsExistingQuery: page two of a query-bearing
// kind merges with the kind's own parameters.
func TestFetchAllPagesKeepsExistingQuery(t *testing.T) {
	fastRetries(t)
	var seen []string
	pages := http.Header{"X-Pages": []string{"2"}}
	transport := &scriptedTransport{
		seen: &seen,
		steps: []scriptedStep{
			{status: http.StatusOK, header: pages, body: `[{"id":1}]`},
			{status: http.StatusOK, header: pages, body: `[{"id":2}]`},
		},
	}
	c := New(&http.Client{Transport: transport}, nil, nil)
	body, _, _, _, err := c.fetchAllPagesIfChanged(context.Background(), "tok", "/x/?type_id=34", "")
	if err != nil {
		t.Fatalf("fetchAllPagesIfChanged: %v", err)
	}
	if string(body) != `[{"id":1},{"id":2}]` {
		t.Fatalf("merged = %s", body)
	}
	if len(seen) != 2 || seen[1] != baseURL+"/x/?type_id=34&page=2" {
		t.Fatalf("page two fetched %q", seen)
	}
}

// TestFetchAllPagesCapsMergedSize: pages merging past 64MB fail
// the dataset instead of ballooning memory for it. Each page stays
// under the 8MB per-page cap, so only the merge total trips.
func TestFetchAllPagesCapsMergedSize(t *testing.T) {
	fastRetries(t)
	pages := http.Header{"X-Pages": []string{"10"}}
	big := `["` + strings.Repeat("x", 7<<20) + `"]`
	transport := &scriptedTransport{steps: []scriptedStep{
		{status: http.StatusOK, header: pages, body: big},
	}}
	c := New(&http.Client{Transport: transport}, nil, nil)
	_, _, _, _, err := c.fetchAllPagesIfChanged(context.Background(), "tok", "/x/", "")
	if err == nil || !strings.Contains(err.Error(), "exceed") {
		t.Fatalf("70MB merge = %v, want a size error", err)
	}
}

// TestFetchAllPagesIfChangedReadsAChangedSetWhole: a paginated set
// whose every page answers 304 is unchanged, with no body; when any
// page changed, the pages that answered 304 are read again so the
// stored set is the whole new book, with every page's new ETag.
func TestFetchAllPagesIfChangedReadsAChangedSetWhole(t *testing.T) {
	pages := func(etag string) http.Header {
		return http.Header{"X-Pages": []string{"2"}, "Etag": []string{etag}}
	}
	stored := encodePagedETags([]string{`"a1"`, `"b1"`})

	unchanged := &scriptedTransport{steps: []scriptedStep{
		{status: http.StatusNotModified, header: pages(`"a1"`)},
		{status: http.StatusNotModified, header: pages(`"b1"`)},
	}}
	c := New(&http.Client{Transport: unchanged}, nil, nil)
	body, _, etag, notModified, err := c.fetchAllPagesIfChanged(context.Background(), "tok", "/x/", stored)
	if err != nil || !notModified || body != nil || etag != stored {
		t.Fatalf("unchanged set: body=%q etag=%q notModified=%v err=%v, want no body, the stored ETags, notModified", body, etag, notModified, err)
	}

	var seen []string
	changed := &scriptedTransport{seen: &seen, steps: []scriptedStep{
		{status: http.StatusNotModified, header: pages(`"a1"`)},
		{status: http.StatusOK, header: pages(`"b2"`), body: `[{"id":3}]`},
		{status: http.StatusOK, header: pages(`"a1"`), body: `[{"id":1}]`},
	}}
	c = New(&http.Client{Transport: changed}, nil, nil)
	body, _, etag, notModified, err = c.fetchAllPagesIfChanged(context.Background(), "tok", "/x/", stored)
	if err != nil || notModified {
		t.Fatalf("changed set: notModified=%v err=%v, want a fresh read", notModified, err)
	}
	if string(body) != `[{"id":1},{"id":3}]` {
		t.Fatalf("changed set merged = %s, want page 1 re-read and page 2's new entries", body)
	}
	if etag != encodePagedETags([]string{`"a1"`, `"b2"`}) {
		t.Fatalf("stored ETags = %q, want page 1's and page 2's new one", etag)
	}
	if len(seen) != 3 {
		t.Fatalf("requests = %q, want page 1, page 2, then page 1 again", seen)
	}
}
