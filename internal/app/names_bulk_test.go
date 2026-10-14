package app

// Bulk name resolution: which failures split a batch, and which
// leave it for the next cycle.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// bulkNamesStub plays POST /universe/names/. known IDs resolve;
// a batch holding any other ID gets 404, like ESI. status, when
// set, answers every call instead; fail makes the transport
// itself fail.
type bulkNamesStub struct {
	known  map[int64]string
	status int
	fail   bool
	calls  int
}

func (s *bulkNamesStub) RoundTrip(req *http.Request) (*http.Response, error) {
	s.calls++
	respond := func(status int, body string) (*http.Response, error) {
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(body))}, nil
	}
	if s.fail {
		return nil, errors.New("connection reset")
	}
	if s.status != 0 {
		return respond(s.status, `{"error":"unavailable"}`)
	}
	var ids []int64
	if err := json.NewDecoder(req.Body).Decode(&ids); err != nil {
		return respond(http.StatusBadRequest, `{"error":"bad body"}`)
	}
	var out []universeName
	for _, id := range ids {
		name, ok := s.known[id]
		if !ok {
			return respond(http.StatusNotFound, `{"error":"Ensure all IDs are valid before resolving."}`)
		}
		out = append(out, universeName{ID: id, Name: name, Category: "corporation"})
	}
	raw, _ := json.Marshal(out)
	return respond(http.StatusOK, string(raw))
}

// TestBulkNamesSplitOnlyOnNotFound: a 404 means one ID in the
// batch does not exist, so the batch is halved until the bad ID
// is alone and every good one is named.
func TestBulkNamesSplitOnlyOnNotFound(t *testing.T) {
	stub := &bulkNamesStub{known: map[int64]string{1: "One", 2: "Two", 3: "Three"}}
	app, _, _ := buildCorpTestApp(t, stub)
	allowance := &fetchBudget{left: 20}
	names, limited := app.resolveUniverseNamesLimited(context.Background(), allowance, []int64{1, 2, 3, 999})
	if limited {
		t.Fatal("a 404 was reported as the error limit")
	}
	if len(names) != 3 || names[1] != "One" || names[2] != "Two" || names[3] != "Three" {
		t.Fatalf("names = %v, want the three that exist", names)
	}
	// [1 2 3 999] 404, [1 2] ok, [3 999] 404, [3] ok, [999] 404.
	if stub.calls != 5 || allowance.left != 15 {
		t.Fatalf("%d calls, %d allowance left; want 5 calls and 15 left", stub.calls, allowance.left)
	}
}

// TestBulkNamesBackOffOnOtherFailures: a 5xx or a dead connection
// says nothing about the IDs. The batch is not split, nothing
// more is spent on it or on the batches behind it, and the names
// wait for the next cycle.
func TestBulkNamesBackOffOnOtherFailures(t *testing.T) {
	ids := make([]int64, 0, universeNamesChunk+500)
	for i := 1; i <= universeNamesChunk+500; i++ {
		ids = append(ids, int64(i))
	}
	for name, stub := range map[string]*bulkNamesStub{
		"500":                {status: http.StatusInternalServerError},
		"503":                {status: http.StatusServiceUnavailable},
		"a dead connection":  {fail: true},
		"an unreadable body": {status: http.StatusBadGateway},
	} {
		app, _, _ := buildCorpTestApp(t, stub)
		allowance := &fetchBudget{left: 20}
		names, limited := app.resolveUniverseNamesLimited(context.Background(), allowance, ids)
		if limited {
			t.Errorf("%s: reported as the error limit", name)
		}
		if len(names) != 0 {
			t.Errorf("%s: resolved %d names from a failing endpoint", name, len(names))
		}
		if stub.calls != 1 || allowance.left != 19 {
			t.Errorf("%s: %d calls and %d allowance left; want exactly 1 call and 19 left", name, stub.calls, allowance.left)
		}
	}
}

// TestBulkNamesReportTheErrorLimit: ESI's stop signal reaches
// the caller, after one call.
func TestBulkNamesReportTheErrorLimit(t *testing.T) {
	stub := &bulkNamesStub{status: 420}
	app, _, _ := buildCorpTestApp(t, stub)
	allowance := &fetchBudget{left: 20}
	_, limited := app.resolveUniverseNamesLimited(context.Background(), allowance, []int64{1, 2, 3, 4})
	if !limited {
		t.Fatal("the error limit was not reported")
	}
	if stub.calls != 1 {
		t.Fatalf("%d calls after the error limit, want 1", stub.calls)
	}
	if _, limited := app.resolveCorpNames(context.Background(), allowance, []int64{5, 6}); !limited {
		t.Fatal("resolveCorpNames swallowed the error limit")
	}
}
