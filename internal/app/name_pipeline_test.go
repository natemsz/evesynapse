package app

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	db "evesynapse/internal/db/sqlc"
)

// creatorCorpTransport is ESI for the name pipeline test: the creator
// corporation answers, and every pilot route is rate limited (429), the
// way one busy character's bucket can be.
type creatorCorpTransport struct {
	corpCalls atomic.Int64
}

func (tr *creatorCorpTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	respond := func(status int, body string) (*http.Response, error) {
		h := http.Header{"Content-Type": {"application/json"}}
		if status == http.StatusTooManyRequests {
			h.Set("Retry-After", "60")
		}
		return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	}
	switch {
	case strings.Contains(req.URL.Path, "/corporations/459299583/"):
		tr.corpCalls.Add(1)
		return respond(http.StatusOK, `{"name":"Effervescent Blue","ticker":"-E B-","member_count":10,"ceo_id":240070320,"creator_id":1128357157,"tax_rate":0.075,"date_founded":"2007-06-30T23:18:00Z"}`)
	case strings.Contains(req.URL.Path, "/characters/"):
		return respond(http.StatusTooManyRequests, `{"error":"rate limited"}`)
	}
	return respond(http.StatusNotFound, `{"error":"not found"}`)
}

// TestCreatorCorpNameResolvesOnView: an alliance page names its creator
// corporation, one the app has never met. Viewing the page queues it,
// and one urgent tick fetches it -- even with the pilot queue stuck on a
// rate limit ahead of it -- after which the label resolves.
func TestCreatorCorpNameResolvesOnView(t *testing.T) {
	transport := &creatorCorpTransport{}
	app, _, q := buildCorpTestApp(t, transport)
	ctx := context.Background()
	user, err := q.CreateUser(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seedCharacter(t, q, user.ID, fixtureCharA, "Fixture Ceo")
	cookie := sessionCookie(t, app, user.ID, fixtureCharA, "Fixture Ceo")

	if err := q.SetAllianceRecord(ctx, db.SetAllianceRecordParams{
		AllianceID: 1354830081,
		Payload:    `{"alliance":{"name":"Goonswarm Federation","ticker":"CONDI","creator_id":1,"creator_corporation_id":459299583,"executor_corporation_id":0,"date_founded":"2010-06-01T00:00:00Z"},"corporations":[]}`,
		State:      orgStateReady, FetchedAt: mustNullTime("2026-10-01T00:00:00Z"),
	}); err != nil {
		t.Fatalf("seed alliance: %v", err)
	}
	// A pilot want ahead of it in the urgent order, whose route is
	// rate limited.
	if err := app.wantPilot(ctx, 1128357157, wantViewed); err != nil {
		t.Fatalf("seed pilot want: %v", err)
	}

	_, body := getPage(t, app, cookie, "/alliance/?alliance=1354830081")
	mustContain(t, "alliance page", body, "Loading name for Corporation #459299583")
	rec, err := q.GetCorporationRecord(ctx, 459299583)
	if err != nil || rec.State != orgStatePending || rec.Priority != wantViewed {
		t.Fatalf("viewing the page queued %+v (%v); want a pending record at viewed priority", rec, err)
	}

	app.urgentDrain(ctx)
	if transport.corpCalls.Load() != 1 {
		t.Fatalf("urgent tick fetched the corporation %d times, want 1", transport.corpCalls.Load())
	}
	_, body = getPage(t, app, cookie, "/labels/corporation-fragment?id=459299583")
	mustContain(t, "corporation label", body, `data-poll-state="ready"`, "Effervescent Blue")

	// The pilot's rate limit held nothing else: no global hold set.
	app.urgentMu.Lock()
	held := !app.urgentHoldUntil.IsZero()
	app.urgentMu.Unlock()
	if held {
		t.Error("a pilot rate limit put the whole urgent drain on hold")
	}
}
