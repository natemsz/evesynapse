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
	body, _, err := c.fetchAllPages(context.Background(), "tok", "/x/?type_id=34")
	if err != nil {
		t.Fatalf("fetchAllPages: %v", err)
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
	_, _, err := c.fetchAllPages(context.Background(), "tok", "/x/")
	if err == nil || !strings.Contains(err.Error(), "exceed") {
		t.Fatalf("70MB merge = %v, want a size error", err)
	}
}
