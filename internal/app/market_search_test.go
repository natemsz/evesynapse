package app

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
)

// Market search exact hits: read from the stored type_names
// cache, with ESI resolution happening in the background —
// never on the request thread.

// idsTransport answers every request as a /universe/ids/
// resolution of PLEX and counts how often it was asked.
type idsTransport struct {
	calls atomic.Int64
}

func (s *idsTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.calls.Add(1)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"inventory_types":[{"id":44992,"name":"PLEX"}]}`)),
	}, nil
}

func matchIDs(matches []marketMatch) []int64 {
	out := make([]int64, 0, len(matches))
	for _, m := range matches {
		out = append(out, m.ID)
	}
	return out
}

// TestSearchTypesServesStoredExactName: a name the stored
// cache settles is served from it, and no ESI lookup is set
// going at all.
func TestSearchTypesServesStoredExactName(t *testing.T) {
	transport := &countingTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	if err := q.UpsertTypeName(ctx, db.UpsertTypeNameParams{TypeID: 44992, Name: "PLEX"}); err != nil {
		t.Fatalf("seed type name: %v", err)
	}
	matches := app.searchTypes(ctx, "plex", false)
	if len(matches) != 1 || matches[0].ID != 44992 || matches[0].Name != "PLEX" {
		t.Fatalf("stored exact search returned %v, want the stored PLEX", matchIDs(matches))
	}
	time.Sleep(300 * time.Millisecond)
	if n := transport.calls.Load(); n != 0 {
		t.Fatalf("a stored exact hit still reached ESI %d time(s)", n)
	}
}

// TestSearchTypesResolvesMissInBackground: a name the cache
// cannot settle is not resolved on the request thread; the
// background resolution stores it, and the next search reads
// it from the cache.
func TestSearchTypesResolvesMissInBackground(t *testing.T) {
	transport := &idsTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	if matches := app.searchTypes(ctx, "PLEX", false); len(matches) != 0 {
		t.Fatalf("first search returned %v before any resolution", matchIDs(matches))
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		name, err := q.GetTypeName(ctx, 44992)
		if err == nil && name == "PLEX" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("background resolution never stored the name (err %v)", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := transport.calls.Load(); n != 1 {
		t.Fatalf("background resolution asked ESI %d time(s), want 1", n)
	}
	matches := app.searchTypes(ctx, "PLEX", false)
	if len(matches) != 1 || matches[0].ID != 44992 {
		t.Fatalf("second search returned %v, want the resolved PLEX", matchIDs(matches))
	}
	if n := transport.calls.Load(); n != 1 {
		t.Fatalf("the resolved name was re-asked at ESI (%d calls)", n)
	}
}

// TestSearchTypesAnonymousStaysLocal: an anonymous search
// consults neither ESI nor the stored exact cache, and sets
// no background resolution going.
func TestSearchTypesAnonymousStaysLocal(t *testing.T) {
	transport := &idsTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	if err := q.UpsertTypeName(ctx, db.UpsertTypeNameParams{TypeID: 44992, Name: "PLEX"}); err != nil {
		t.Fatalf("seed type name: %v", err)
	}
	if matches := app.searchTypes(ctx, "PLEX", true); len(matches) != 0 {
		t.Fatalf("anonymous search returned %v, want local-only results", matchIDs(matches))
	}
	time.Sleep(300 * time.Millisecond)
	if n := transport.calls.Load(); n != 0 {
		t.Fatalf("an anonymous search reached ESI %d time(s)", n)
	}
}
