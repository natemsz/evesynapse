package app

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/intelanalyze"
)

// fakeResidueEngine counts its calls and answers a canned
// reply: the residue step's engine seat (residue_model.go),
// filled by a fixture so the tests never touch a real runtime.
type fakeResidueEngine struct {
	calls int
	reply string
}

func (f *fakeResidueEngine) Complete(context.Context, string) (string, error) {
	f.calls++
	return f.reply, nil
}

// seedResidueReport stores one settled report carrying the
// given analysis and returns its ID.
func seedResidueReport(t *testing.T, q *db.Queries, channelID, userID int64, a intelanalyze.Analysis) int64 {
	t.Helper()
	raw, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	id, err := q.InsertIntelReport(context.Background(), db.InsertIntelReportParams{
		ChannelID: channelID, SystemID: 30004759, AuthorUserID: userID, AuthorCharacterID: 90000301,
		Reporter: "Scout", Text: "a report", Pilots: "a report", SeenAt: now, CreatedAt: now,
		Kind: a.Kind, Analysis: string(raw), GroupID: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	// A founding report names its own group, as the ingest
	// path leaves it; the group is not what is under test.
	if err := q.SetIntelReportGroup(context.Background(), db.SetIntelReportGroupParams{ID: id, GroupID: id}); err != nil {
		t.Fatal(err)
	}
	return id
}

func residueTestChannel(t *testing.T, q *db.Queries) (int64, int64) {
	t.Helper()
	u, err := q.CreateUser(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cid, err := q.CreateIntelChannel(context.Background(), db.CreateIntelChannelParams{OwnerUserID: u.ID, Name: "Home", CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	return cid, u.ID
}

// TestResidueSkippedWithoutResidue: a reading that resolved
// everything never reaches the reader. The offer is the step's
// whole cost for most reports, and a pass run on no residue
// would spend the box's CPU inventing work; with residue, one
// pass runs and is logged.
func TestResidueSkippedWithoutResidue(t *testing.T) {
	app, conn, q := buildCorpTestApp(t, nil)
	ctx := context.Background()
	cid, userID := residueTestChannel(t, q)
	eng := &fakeResidueEngine{reply: `{"claims":[]}`}

	resolved := intelanalyze.Analysis{Kind: intelanalyze.KindSighting,
		Pilots: []intelanalyze.Pilot{{ID: 91000301, Name: "Hostile One"}}}
	id := seedResidueReport(t, q, cid, userID, resolved)
	app.processResidue(ctx, eng, id)
	if eng.calls != 0 {
		t.Fatalf("a reading with no residue reached the reader %d times", eng.calls)
	}

	withResidue := resolved
	withResidue.Residue = []string{"something the reading could not place"}
	raw, _ := json.Marshal(withResidue)
	if err := q.SetIntelReportAnalysis(ctx, db.SetIntelReportAnalysisParams{ID: id, Kind: withResidue.Kind, Analysis: string(raw), AnalysisPending: false}); err != nil {
		t.Fatal(err)
	}
	app.processResidue(ctx, eng, id)
	if eng.calls != 1 {
		t.Fatalf("a reading with residue: %d passes, want 1", eng.calls)
	}
	var calls int
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM intel_residue_calls WHERE report_id = $1`, id).Scan(&calls); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("logged passes: %d, want 1", calls)
	}
}

// TestResidueUnconfirmedStaysUnresolved: a name the reading did
// not resolve, offered back by the residue pass, is stored
// flagged unconfirmed and never stands among the report's
// resolved entities, on the stored analysis or on the view
// built from it. A claim pointing at an id the reading never
// resolved is dropped outright.
func TestResidueUnconfirmedStaysUnresolved(t *testing.T) {
	app, _, q := buildCorpTestApp(t, nil)
	ctx := context.Background()
	cid, userID := residueTestChannel(t, q)

	a := intelanalyze.Analysis{Kind: intelanalyze.KindSighting,
		Pilots:  []intelanalyze.Pilot{{ID: 91000301, Name: "Hostile One"}},
		Ships:   []intelanalyze.Ship{{TypeID: 22456, Name: "Sabre", Class: intelanalyze.ClassInterdict}},
		Residue: []string{"Kif Kroker watching the gate", "three more sabres", "ghost pilot on scan"}}
	id := seedResidueReport(t, q, cid, userID, a)
	before, err := q.GetIntelReportFull(ctx, id)
	if err != nil {
		t.Fatal(err)
	}

	eng := &fakeResidueEngine{reply: `{"claims":[
		{"span":"Kif Kroker watching the gate","kind":"pilot","id":0,"name":"Kif Kroker"},
		{"span":"three more sabres","kind":"ship","id":22456,"name":"Sabre","n":3},
		{"span":"ghost pilot on scan","kind":"pilot","id":424242,"name":"Ghost Pilot"}
	]}`}
	app.processResidue(ctx, eng, id)
	if eng.calls != 1 {
		t.Fatalf("passes: %d, want 1", eng.calls)
	}

	notes, err := q.ListIntelResidueNotes(ctx, db.ListIntelResidueNotesParams{ReportID: id, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 2 {
		t.Fatalf("notes: %d, want 2 (the invented id's claim is dropped): %+v", len(notes), notes)
	}
	var suggestion, attribution *db.IntelResidueNote
	for i := range notes {
		switch notes[i].EntityName {
		case "Kif Kroker":
			suggestion = &notes[i]
		case "Sabre":
			attribution = &notes[i]
		}
	}
	if suggestion == nil || !suggestion.Unconfirmed || suggestion.EntityID != 0 {
		t.Fatalf("the new name is not stored as an unconfirmed suggestion: %+v", suggestion)
	}
	if attribution == nil || attribution.Unconfirmed || attribution.EntityID != 22456 || attribution.N != 3 {
		t.Fatalf("the resolved ship is not stored as a settled attribution: %+v", attribution)
	}

	// The deterministic facts are untouched, and the view built
	// from the stored report names only what the reading itself
	// resolved.
	after, err := q.GetIntelReportFull(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if after.Analysis != before.Analysis {
		t.Fatalf("the stored analysis changed under the step:\n%s\n%s", before.Analysis, after.Analysis)
	}
	var rv reportView
	applyAnalysis(&rv, after.Kind, after.Analysis, app.newKillNames(ctx))
	if len(rv.Pilots) != 1 || rv.Pilots[0].Name != "Hostile One" {
		t.Fatalf("the view's pilots: %+v", rv.Pilots)
	}
	for _, s := range rv.Ships {
		if s.Name == "Kif Kroker" {
			t.Fatal("the unconfirmed name stands among the view's ships")
		}
	}
}

// TestResidueBacklogSweep: the sweep selects settled reports
// whose readings left residue and which no completed pass has
// covered, newest first, and offers them through the queue. A
// report a pass has covered is never selected again (the call
// row is the durable record), a reading with no residue is not
// selected, and a report still pending its analysis is not
// settled and not selected. A second sweep while the first
// offers stand queued offers nothing twice.
func TestResidueBacklogSweep(t *testing.T) {
	app, _, q := buildCorpTestApp(t, nil)
	ctx := context.Background()
	cid, userID := residueTestChannel(t, q)
	app.cfg.residueModelPath = "fixture.gguf" // the step stands enabled

	withResidue := intelanalyze.Analysis{Kind: intelanalyze.KindSighting,
		Pilots:  []intelanalyze.Pilot{{ID: 91000301, Name: "Hostile One"}},
		Residue: []string{"leftover words the reading could not place"}}

	older := seedResidueReport(t, q, cid, userID, withResidue)
	covered := seedResidueReport(t, q, cid, userID, withResidue)
	if _, err := q.InsertIntelResidueCall(ctx, db.InsertIntelResidueCallParams{
		ReportID: covered, Spans: "[]", Output: `{"claims":[]}`, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	noResidue := intelanalyze.Analysis{Kind: intelanalyze.KindSighting,
		Pilots: []intelanalyze.Pilot{{ID: 91000301, Name: "Hostile One"}}}
	seedResidueReport(t, q, cid, userID, noResidue)
	pending := seedResidueReport(t, q, cid, userID, withResidue)
	row, err := q.GetIntelReportFull(ctx, pending)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.SetIntelReportAnalysis(ctx, db.SetIntelReportAnalysisParams{
		ID: pending, Kind: row.Kind, Analysis: row.Analysis, AnalysisPending: true,
	}); err != nil {
		t.Fatal(err)
	}
	newer := seedResidueReport(t, q, cid, userID, withResidue)

	ids, err := q.ListResidueBacklogReports(ctx, db.ListResidueBacklogReportsParams{
		Limit: residueSweepBatch, NextAttemptAt: timeSet(time.Now().UTC()),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != newer || ids[1] != older {
		t.Fatalf("sweep selection: %v, want [%d %d] (covered %d excluded)", ids, newer, older, covered)
	}

	app.sweepResidueBacklog(ctx)
	ch := app.residueQueueChan()
	if len(ch) != 2 {
		t.Fatalf("queued offers: %d, want 2", len(ch))
	}
	if got := <-ch; got != newer {
		t.Fatalf("first offer: %d, want the newest %d", got, newer)
	}
	if got := <-ch; got != older {
		t.Fatalf("second offer: %d, want %d", got, older)
	}

	app.sweepResidueBacklog(ctx)
	if len(ch) != 0 {
		t.Fatalf("a second sweep re-offered %d queued reports", len(ch))
	}
}

// TestResidueSweepInertWhenDisabled: with no local reader
// configured the step stays exactly as inert as the settle path
// leaves it: offers queue nothing and the worker idles out at
// once instead of sweeping.
func TestResidueSweepInertWhenDisabled(t *testing.T) {
	app, _, q := buildCorpTestApp(t, nil)
	ctx := context.Background()
	cid, userID := residueTestChannel(t, q)
	if app.cfg.ResidueModelEnabled() {
		t.Fatal("the fixture app stands enabled; the disabled case is not under test")
	}
	id := seedResidueReport(t, q, cid, userID, intelanalyze.Analysis{Kind: intelanalyze.KindSighting,
		Residue: []string{"leftover words the reading could not place"}})

	app.noteResidueReport(id)
	if len(app.residueQueueChan()) != 0 {
		t.Fatal("a disabled step queued an offer")
	}
	done := make(chan struct{})
	go func() {
		app.runResidueWorker(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the disabled worker did not idle out")
	}
}

// The stub reader the lifetime test runs: a python script
// answering the health check and one canned completion, close
// enough to the real runtime's shape for the engine to hold
// it to the same contract.
const stubReaderPy = `
import sys, json
from http.server import BaseHTTPRequestHandler, HTTPServer

port = 18099
args = sys.argv[1:]
if "--port" in args:
    port = int(args[args.index("--port") + 1])

class H(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/health":
            body, code = b'{"status":"ok"}', 200
        else:
            body, code = b'{}', 404
        self.send_response(code)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        n = int(self.headers.get("Content-Length") or 0)
        if n:
            self.rfile.read(n)
        body = json.dumps({"choices": [{"message": {"content": "{\"claims\":[]}"}}]}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *a):
        pass

HTTPServer(("127.0.0.1", port), H).serve_forever()
`

// The reader's server is bound to the worker's life, never to
// a pass: a pass's context ending leaves it up and a later
// pass still completes against it, a server killed out from
// under the engine is replaced by the next pass, and the
// worker's context ending takes the server down with it.
func TestResidueServerOutlivesPassContext(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available for the stub reader")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "stub_reader.py"), []byte(stubReaderPy), 0o644); err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(dir, "stub_reader.sh")
	script := "#!/bin/sh\nexec python3 \"$(dirname \"$0\")/stub_reader.py\" \"$@\"\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	model := filepath.Join(dir, "model.gguf")
	if err := os.WriteFile(model, []byte("stub"), 0o644); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	serveCtx, stopServe := context.WithCancel(context.Background())
	defer stopServe()
	eng := newLlamaEngine(Config{
		residueModelBin: stub, residueModelPath: model, residueModelAddr: addr,
	}, serveCtx)

	// The first pass runs under its own deadline context, and
	// that context ends with the pass.
	pass1, cancel1 := context.WithTimeout(context.Background(), 30*time.Second)
	if _, err := eng.Complete(pass1, "prompt"); err != nil {
		cancel1()
		t.Fatalf("first pass: %v", err)
	}
	cancel1()

	// The server is still up: a later pass completes.
	pass2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel2()
	if _, err := eng.Complete(pass2, "prompt"); err != nil {
		t.Fatalf("pass after the first pass's context ended: %v", err)
	}

	// Kill the server out from under the engine: the next
	// pass finds it dead and completes against a fresh one.
	eng.mu.Lock()
	proc := eng.cmd.Process
	eng.mu.Unlock()
	if err := proc.Kill(); err != nil {
		t.Fatalf("kill the stub server: %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, err := eng.Complete(ctx, "prompt")
		cancel()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the engine did not recover after its server was killed: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// The worker's context ending takes the server down:
	// nothing is left listening on its port.
	stopServe()
	deadline = time.Now().Add(10 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err != nil {
			break
		}
		conn.Close()
		if time.Now().After(deadline) {
			t.Fatal("the server is still up after the worker's context ended")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// failResidueEngine always fails its pass.
type failResidueEngine struct{ calls int }

func (f *failResidueEngine) Complete(context.Context, string) (string, error) {
	f.calls++
	return "", errors.New("reader timed out")
}

func residueAttemptRow(t *testing.T, q *db.Queries, id int64) db.IntelResidueAttempt {
	t.Helper()
	row, err := q.GetResidueAttempt(context.Background(), id)
	if err != nil {
		t.Fatalf("attempt row for %d: %v", id, err)
	}
	return row
}

// TestResidueAttemptsDeadLetter: failed passes are counted
// durably, back off, and at the cap the report leaves the
// backlog with the reason recorded.
func TestResidueAttemptsDeadLetter(t *testing.T) {
	app, conn, q := buildCorpTestApp(t, nil)
	ctx := context.Background()
	cid, userID := residueTestChannel(t, q)
	withResidue := intelanalyze.Analysis{Kind: intelanalyze.KindSighting,
		Pilots:  []intelanalyze.Pilot{{ID: 91000301, Name: "Hostile One"}},
		Residue: []string{"leftover words"}}
	id := seedResidueReport(t, q, cid, userID, withResidue)
	eng := &failResidueEngine{}
	for i := 1; i <= residueMaxAttempts; i++ {
		app.processResidue(ctx, eng, id)
		if eng.calls != i {
			t.Fatalf("pass %d: engine calls %d, want %d", i, eng.calls, i)
		}
		if _, err := conn.ExecContext(ctx, `UPDATE intel_residue_attempts SET next_attempt_at = now() - interval '1 hour' WHERE report_id = $1`, id); err != nil {
			t.Fatal(err)
		}
	}
	row := residueAttemptRow(t, q, id)
	if row.Attempts != residueMaxAttempts || row.DeadReason == "" {
		t.Fatalf("attempt row after cap: %+v, want %d attempts with a dead reason", row, residueMaxAttempts)
	}
	ids, err := q.ListResidueBacklogReports(ctx, db.ListResidueBacklogReportsParams{
		Limit: residueSweepBatch, NextAttemptAt: timeSet(time.Now().UTC()),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range ids {
		if got == id {
			t.Fatalf("dead-lettered report %d is still in the backlog", id)
		}
	}
}

// TestResidueFailureBacksOff: one failed pass is counted and
// the report is not retried until its backoff runs out.
func TestResidueFailureBacksOff(t *testing.T) {
	app, _, q := buildCorpTestApp(t, nil)
	ctx := context.Background()
	cid, userID := residueTestChannel(t, q)
	withResidue := intelanalyze.Analysis{Kind: intelanalyze.KindSighting,
		Pilots:  []intelanalyze.Pilot{{ID: 91000301, Name: "Hostile One"}},
		Residue: []string{"leftover words"}}
	id := seedResidueReport(t, q, cid, userID, withResidue)
	eng := &failResidueEngine{}
	app.processResidue(ctx, eng, id)
	app.processResidue(ctx, eng, id)
	if eng.calls != 1 {
		t.Fatalf("engine calls %d, want 1 (the second offer backs off)", eng.calls)
	}
	if row := residueAttemptRow(t, q, id); row.Attempts != 1 || !row.NextAttemptAt.Valid {
		t.Fatalf("attempt row: %+v, want 1 attempt with a backoff gate", row)
	}
}

// TestResidueIdenticalTextNotReRead: a second report carrying
// byte-identical text is not read again; it leaves the backlog
// as a duplicate with the reason recorded.
func TestResidueIdenticalTextNotReRead(t *testing.T) {
	app, _, q := buildCorpTestApp(t, nil)
	ctx := context.Background()
	cid, userID := residueTestChannel(t, q)
	withResidue := intelanalyze.Analysis{Kind: intelanalyze.KindSighting,
		Pilots:  []intelanalyze.Pilot{{ID: 91000301, Name: "Hostile One"}},
		Residue: []string{"leftover words"}}
	first := seedResidueReport(t, q, cid, userID, withResidue)
	second := seedResidueReport(t, q, cid, userID, withResidue)
	eng := &fakeResidueEngine{reply: `{"claims":[]}`}
	app.processResidue(ctx, eng, first)
	app.processResidue(ctx, eng, second)
	if eng.calls != 1 {
		t.Fatalf("engine calls %d, want 1 (identical text is read once)", eng.calls)
	}
	row := residueAttemptRow(t, q, second)
	if !strings.Contains(row.DeadReason, "identical report text") {
		t.Fatalf("duplicate dead reason: %q, want the identical-text reason", row.DeadReason)
	}
}

// TestBoundedStderrKeepsTail: the child's stderr capture is
// bounded and keeps the most recent bytes.
func TestBoundedStderrKeepsTail(t *testing.T) {
	b := &boundedStderr{}
	for i := 0; i < residueStderrCap; i += 1024 {
		if _, err := b.Write(make([]byte, 1024)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.Write([]byte("final words")); err != nil {
		t.Fatal(err)
	}
	if got := len([]byte(b.String())); got > residueStderrCap {
		t.Fatalf("captured stderr is %d bytes, cap is %d", got, residueStderrCap)
	}
	if !strings.HasSuffix(b.String(), "final words") {
		t.Fatalf("captured stderr lost its tail")
	}
}
