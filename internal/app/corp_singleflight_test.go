package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// blockingCorpTransport holds every corporation request at a gate
// until the test opens it, so concurrent readers pile onto one
// expired entry; then it answers from the script.
type blockingCorpTransport struct {
	corpCalls atomic.Int64
	gate      chan struct{}
}

var errTestTimeout = errors.New("test timed out waiting for corporation()")

func (s *blockingCorpTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	path := req.URL.Path
	if strings.HasPrefix(path, "/corporations/") {
		s.corpCalls.Add(1)
		<-s.gate // wait for the test to release the pile-up
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"name":"Shared Corp","ticker":"SHR","member_count":7,"tax_rate":0.05,"description":""}`)),
		}, nil
	}
	return &http.Response{
		StatusCode: http.StatusNotFound,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{}`)),
	}, nil
}

// TestCorporationReadersShareOneRefresh: ten readers arriving on an
// expired entry fire one ESI refresh between them, and every one of
// them gets the view. Before the in-flight sharing, each reader
// fired its own.
func TestCorporationReadersShareOneRefresh(t *testing.T) {
	transport := &blockingCorpTransport{gate: make(chan struct{})}
	app, _, _ := buildCorpTestApp(t, transport)
	ctx := context.Background()

	const readers = 10
	var wg sync.WaitGroup
	errs := make(chan error, readers)
	names := make(chan string, readers)
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Guard the wait: a deadlock must fail the test, not
			// the suite.
			done := make(chan struct{})
			var view corpView
			var err error
			go func() {
				view, err = app.corporation(ctx, 2000001)
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				errs <- errTestTimeout
				return
			}
			if err != nil {
				errs <- err
				return
			}
			names <- view.Name
		}()
	}
	// Wait for the first refresh to reach ESI, give the other
	// readers a moment to pile onto it, then release them at once.
	// (The assertion below — exactly one refresh — holds for the
	// sharing code under every interleaving; the pile-up just
	// makes sure the old stampede would have shown itself.)
	deadline := time.Now().Add(30 * time.Second)
	for transport.corpCalls.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	close(transport.gate)
	wg.Wait()
	close(errs)
	close(names)
	for err := range errs {
		t.Fatalf("reader: %v", err)
	}
	count := 0
	for name := range names {
		count++
		if name != "Shared Corp" {
			t.Fatalf("reader got corporation %q, want Shared Corp", name)
		}
	}
	if count != readers {
		t.Fatalf("%d readers answered, want %d", count, readers)
	}
	if got := transport.corpCalls.Load(); got != 1 {
		t.Fatalf("corporation endpoint called %d times, want 1", got)
	}
}
