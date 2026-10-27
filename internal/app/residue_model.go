package app

// ---------------------------------------------------------------------------
// The residue step: a settled reading's leftover text, read once
// more by a small reader that runs locally on this box.
//
// The deterministic analysis (internal/intelanalyze) records the
// spans of a report it did not consume (Analysis.Residue). As a
// report settles, its ID is offered here, and one bounded worker
// walks the queue: for each report it hands the spans, beside
// the entities the reading itself resolved, to the local reader
// (residue_llama.go), and stores what comes back as notes on the
// reading (schema 091). A slow sweep inside the worker offers
// the same pass to settled reports that predate the step (or
// settled while it was off), newest first, so the backlog drains
// through the same queue.
//
// The step adds nothing to the deterministic facts, and nothing
// anywhere renders its notes. A note either links a span to an
// entity the reading already resolved, or records a name the
// reading did not know: those stand flagged unconfirmed, never
// as a resolved entity. One exception, the gate fuse
// (applyResidueGateReading): a gate phrase the deterministic
// reading left unresolved is this step's home ground, and a
// system claim the fuse verifies against the report system's
// real gates is written into the stored analysis, marked as
// model-read, so the bilateral surfaces use it. Every
// completed pass is logged whole (spans in, raw reply out) as
// a residue call, the raw material the label corpus is later
// built from. A reply that does not parse in the strict shape
// is discarded whole, never partially applied.
//
// Bounds, in the family pattern: a fixed-size queue with
// drop-and-log, one worker, one report at a time, a per-report
// deadline. The step is skipped entirely for a reading that left
// no residue, and it is a no-op until a model file is configured
// (RESIDUE_MODEL_PATH) and not switched off (RESIDUE_MODEL=off):
// a box without the reader installed runs exactly as before.
// ---------------------------------------------------------------------------

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/intelanalyze"
	"evesynapse/internal/logging"
)

const (
	// The offer queue's depth: settled reports arrive faster
	// than one reader at CPU pace can ever take them, and a
	// dropped offer costs one report's notes, logged as it goes.
	residueQueueSize = 128
	// Failed passes are counted per report (schema 092) and a
	// report leaves the backlog at this many, with the last
	// failure recorded as the reason.
	residueMaxAttempts = 5
	// The backoff between failed passes doubles from this base:
	// 2, 4, 8, 16 minutes, then the cap above dead-letters.
	residueBackoffBase = 2 * time.Minute
	// A reply may carry at most this many claims, and this many
	// on any one span: a reading's leftovers are few, and a
	// reply past the caps is not the shape that was asked for.
	residueMaxClaims        = 40
	residueMaxClaimsPerSpan = 8
	// The backlog sweep's shape: how many old reports one tick
	// offers, and how often it looks. A pass takes tens of
	// seconds at the reader's pace, so a small batch on a slow
	// tick still drains the backlog over hours, and the batch
	// is all a fresh settle can ever wait behind.
	residueSweepBatch    = 4
	residueSweepInterval = 2 * time.Minute
)

// residueKinds are the readings a span can carry: the label
// schema the corpus is built with (plan P4).
var residueKinds = map[string]bool{
	"pilot": true, "ship": true, "system": true,
	"count": true, "direction": true, "action": true, "structure": true,
}

// residueEntity is one entity a reading resolved, as the reader
// is told of it and as its claims are checked against it.
type residueEntity struct {
	ID   int64
	Name string
}

// residueInput is one report's residue pass: the spans, and what
// the reading resolved, for the claims to be checked against.
type residueInput struct {
	ReportID int64
	SystemID int64
	Spans    []string
	Pilots   []residueEntity
	Ships    []residueEntity
	Systems  []residueEntity // the report's system, and its gate's destination when one was named
	// Neighbours are the systems the report system's gates
	// lead to (sde_system_jumps): the only further systems a
	// claim may name, and the fuse's verification set for a
	// gate reading.
	Neighbours []residueEntity
	// Suspect are the spans the reading took for a pilot on a
	// doubtful word (Analysis.Suspect), carried among Spans too.
	Suspect []string
}

// residueClaim is one reading of one span, in the strict shape
// the reply must take (residueReplySchema in residue_llama.go).
type residueClaim struct {
	Span string `json:"span"`
	Kind string `json:"kind"`
	ID   int64  `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
	N    int    `json:"n,omitempty"`
}

// residueReply is a whole reply: claims, and nothing else.
type residueReply struct {
	Claims []residueClaim `json:"claims"`
}

// residueNote is one validated claim, ready to store.
type residueNote struct {
	Span        string
	Kind        string
	EntityID    int64
	EntityName  string
	N           int
	Unconfirmed bool
}

// residueEngine is the step's reader: one bounded pass over a
// prompt, returning the raw reply text. The llama.cpp reader
// (residue_llama.go) is the engine today; the on-box tagger the
// plan ends with takes this same seat.
type residueEngine interface {
	Complete(ctx context.Context, prompt string) (string, error)
}

// residueQueueChan returns the offer queue, making it on first
// use.
func (app *Application) residueQueueChan() chan int64 {
	app.residueMu.Lock()
	defer app.residueMu.Unlock()
	if app.residueCh == nil {
		app.residueCh = make(chan int64, residueQueueSize)
		app.residueQueued = map[int64]bool{}
	}
	return app.residueCh
}

// noteResidueReport offers a settled report to the residue
// worker. It never blocks the settle: a full queue drops the
// offer and logs it. A report whose reading left no residue
// costs the worker one read; the step itself is skipped there.
func (app *Application) noteResidueReport(id int64) {
	if id <= 0 || !app.cfg.ResidueModelEnabled() {
		return
	}
	ch := app.residueQueueChan()
	app.residueMu.Lock()
	if !app.residueQueued[id] {
		select {
		case ch <- id:
			app.residueQueued[id] = true
		default:
			logging.Warnf("residue: queue full; report %d's leftovers are skipped", id)
		}
	}
	app.residueMu.Unlock()
}

// runResidueWorker is the residue worker: one report at a time
// until ctx ends. On a slow tick it also sweeps the backlog of
// settled reports no pass has covered.
func (app *Application) runResidueWorker(ctx context.Context) {
	if !app.cfg.ResidueModelEnabled() {
		logging.Infof("residue: step idle (RESIDUE_MODEL=off or no model file configured)")
		return
	}
	ch := app.residueQueueChan()
	sweep := time.NewTicker(residueSweepInterval)
	defer sweep.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-ch:
			app.processResidue(ctx, app.residueEngine(ctx), id)
			app.residueMu.Lock()
			delete(app.residueQueued, id)
			app.residueMu.Unlock()
		case <-sweep.C:
			app.sweepResidueBacklog(ctx)
		}
	}
}

// sweepResidueBacklog offers the newest settled reports whose
// readings left residue and which no completed pass has covered
// (ListResidueBacklogReports): the backlog from before the step
// was enabled, or from while it was off. Fresh settles keep
// priority: the sweep offers only into an empty queue, one
// small batch per tick, through the same offer path, so the
// dedupe mark covers its offers and a report a pass has reached
// is never offered again, across restarts as much as in-process
// (the completed call is the durable record).
func (app *Application) sweepResidueBacklog(ctx context.Context) {
	if len(app.residueQueueChan()) > 0 {
		return // fresh work is waiting; the backlog keeps
	}
	ids, err := app.queries.ListResidueBacklogReports(ctx, db.ListResidueBacklogReportsParams{
		Limit: residueSweepBatch, NextAttemptAt: timeSet(time.Now().UTC()),
	})
	if err != nil {
		logging.Warnf("residue: backlog sweep: %v", err)
		return
	}
	for _, id := range ids {
		app.noteResidueReport(id)
	}
}

// residueEngine returns the local reader, made on first use
// and held for the process's life. The worker's context goes
// with it: the reader's server lives and dies with the
// worker, never with a single pass.
func (app *Application) residueEngine(ctx context.Context) residueEngine {
	app.residueMu.Lock()
	defer app.residueMu.Unlock()
	if app.residueEng == nil {
		app.residueEng = newLlamaEngine(app.cfg, ctx)
	}
	return app.residueEng
}

// mentionsGate reports whether a text speaks of a gate: a
// field that is the word itself, trimmed of the punctuation
// a paste drags with it.
func mentionsGate(text string) bool {
	for _, f := range strings.Fields(strings.ToLower(text)) {
		if strings.Trim(f, ",.;:*!()[]?\"'") == "gate" {
			return true
		}
	}
	return false
}

// processResidue runs one report's pass: the spans its settled
// analysis left, read against what the analysis resolved, the
// reply validated and stored. A reading with no residue never
// reaches the engine, unless its text carries a gate phrase
// the deterministic reading could not resolve: a loose gate
// phrasing is the step's home ground, residue or none. Scan
// reports are out of that trigger: their text is a reading's
// own sentence, not a scout's line.
func (app *Application) processResidue(ctx context.Context, eng residueEngine, reportID int64) {
	row, err := app.queries.GetIntelReportFull(ctx, reportID)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			logging.Warnf("residue: read report %d: %v", reportID, err)
		}
		return
	}
	var a intelanalyze.Analysis
	if row.Analysis == "" || json.Unmarshal([]byte(row.Analysis), &a) != nil {
		return
	}
	gateLoose := a.Gate == 0 && row.ScanID == "" && mentionsGate(row.Text)
	if len(a.Residue) == 0 && !gateLoose && len(a.Suspect) == 0 {
		return // the reading resolved everything, and surely: the step is skipped
	}
	// Identical report text is read once, ever: a completed
	// pass over the same text already produced this reading's
	// notes, and re-reading it only burns the reader. The
	// report leaves the backlog as a duplicate, with the
	// reason recorded, instead of being re-offered forever.
	hash := residueTextHash(row.Text)
	now := time.Now().UTC()
	attempt, attemptErr := app.queries.GetResidueAttempt(ctx, reportID)
	if attemptErr == nil && attempt.TextHash == hash {
		if attempt.DeadReason != "" {
			return
		}
		if attempt.NextAttemptAt.Valid && now.Before(attempt.NextAttemptAt.Time) {
			return // backing off after a failed pass
		}
	}
	if done, err := app.queries.ResidueCallExistsForHash(ctx, hash); err == nil && done {
		attempts := int32(0)
		if attemptErr == nil && attempt.TextHash == hash {
			attempts = attempt.Attempts
		}
		if err := app.queries.MarkResidueDead(ctx, db.MarkResidueDeadParams{
			ReportID: reportID, TextHash: hash, Attempts: attempts,
			LastAttemptAt: timeSet(now),
			DeadReason:    "identical report text already read (sha256 " + hash[:12] + ")",
		}); err != nil {
			logging.Warnf("residue: mark duplicate report %d: %v", reportID, err)
		}
		return
	}
	in := residueInput{ReportID: reportID, SystemID: row.SystemID, Spans: a.Residue, Suspect: a.Suspect}
	// A suspect pilot's span is a fragment like any other: the
	// claim on it says whether it is the pilot or the word.
	for _, s := range a.Suspect {
		if !slices.Contains(in.Spans, s) {
			in.Spans = append(in.Spans, s)
		}
	}
	if gateLoose {
		// The claims must be able to name the fragment the
		// gate stands in: when no residue span carries it,
		// the line itself is the span.
		carried := false
		for _, s := range in.Spans {
			carried = carried || mentionsGate(s)
		}
		if !carried {
			in.Spans = append(in.Spans, row.Text)
		}
	}
	for _, p := range a.Pilots {
		if p.ID > 0 {
			in.Pilots = append(in.Pilots, residueEntity{ID: p.ID, Name: p.Name})
		}
	}
	for _, s := range a.Ships {
		if s.TypeID > 0 {
			in.Ships = append(in.Ships, residueEntity{ID: s.TypeID, Name: s.Name})
		}
	}
	seenSystems := map[int64]bool{}
	for _, id := range []int64{row.SystemID, a.Gate} {
		if id <= 0 || seenSystems[id] {
			continue
		}
		seenSystems[id] = true
		name := ""
		if sys, err := app.queries.GetSDESystem(ctx, id); err == nil {
			name = sys.Name
		}
		in.Systems = append(in.Systems, residueEntity{ID: id, Name: name})
	}
	// The systems the report's gates lead to, from the stored
	// jumps: the only further systems a claim may name.
	if rows, err := app.queries.ListSDESystemNeighbours(ctx, row.SystemID); err == nil {
		for _, nid := range rows {
			if nid <= 0 || seenSystems[nid] {
				continue
			}
			seenSystems[nid] = true
			if sys, err := app.queries.GetSDESystem(ctx, nid); err == nil && sys.Name != "" {
				in.Neighbours = append(in.Neighbours, residueEntity{ID: nid, Name: sys.Name})
			}
		}
	}
	raw, err := eng.Complete(ctx, residuePrompt(in))
	if err != nil {
		if !errors.Is(err, errResidueAbsent) {
			logging.Warnf("residue: report %d: %v", reportID, err)
			app.recordResidueFailure(ctx, reportID, hash, attempt, attemptErr, err)
		}
		return
	}
	notes := validateResidueReply(in, raw)
	app.storeResiduePass(ctx, in, raw, hash, notes)
	app.applyResidueGateReading(ctx, row, a, in, notes)
	app.applyResiduePilotDoubts(ctx, row.ID, in, notes)
}

// applyResiduePilotDoubts is the fuse step for suspect pilots: a
// pilot the reading resolved on a doubtful word (Analysis.Suspect)
// is taken out of the stored reading when the pass reads its span
// as something else, a count or an action, and never as a pilot.
// A span the pass left out, or read as the pilot, stands as it
// was: only a contrary reading takes a pilot back. The kind is
// decided again without the pilot, and the standing split loses
// it.
func (app *Application) applyResiduePilotDoubts(ctx context.Context, reportID int64, in residueInput, notes []residueNote) {
	if len(in.Suspect) == 0 {
		return
	}
	pilotSpan, otherSpan := map[string]bool{}, map[string]bool{}
	for _, n := range notes {
		if n.Kind == "pilot" {
			pilotSpan[n.Span] = true
		} else {
			otherSpan[n.Span] = true
		}
	}
	drop := map[string]bool{}
	for _, s := range in.Suspect {
		if otherSpan[s] && !pilotSpan[s] {
			drop[strings.ToLower(s)] = true
		}
	}
	if len(drop) == 0 {
		return
	}
	// Re-read before writing, as the gate fuse does.
	fresh, err := app.queries.GetIntelReportFull(ctx, reportID)
	if err != nil {
		return
	}
	var na intelanalyze.Analysis
	if fresh.Analysis == "" || json.Unmarshal([]byte(fresh.Analysis), &na) != nil {
		return
	}
	kept := na.Pilots[:0:0]
	var gone []string
	for _, p := range na.Pilots {
		if drop[strings.ToLower(p.Name)] {
			gone = append(gone, p.Name)
			if na.Counts != nil {
				switch {
				case p.Red:
					na.Counts.Hostile = max(na.Counts.Hostile-1, 0)
				case p.Friendly:
					na.Counts.Friendly = max(na.Counts.Friendly-1, 0)
				default:
					na.Counts.Neutral = max(na.Counts.Neutral-1, 0)
				}
			}
			continue
		}
		kept = append(kept, p)
	}
	if len(gone) == 0 {
		return
	}
	na.Pilots = kept
	if na.Counts != nil && na.Counts.Total() == 0 {
		na.Counts = nil
	}
	na.Suspect = nil
	kind := intelanalyze.KindOf(intelanalyze.Input{
		System: fresh.SystemID, Hostiles: int(fresh.Hostiles), Clear: fresh.Clear,
		NoVisual: slices.Contains(na.Flags, "nv"),
	}, na)
	raw, err := json.Marshal(na)
	if err != nil {
		return
	}
	if err := app.queries.SetIntelReportAnalysis(ctx, db.SetIntelReportAnalysisParams{
		ID: reportID, Kind: kind, Analysis: string(raw), AnalysisPending: false,
	}); err != nil {
		logging.Warnf("residue: drop doubted pilots of report %d: %v", reportID, err)
		return
	}
	logging.Infof("residue: report %d: %s read as words, not pilots, by the model pass", reportID, strings.Join(gone, ", "))
	app.refreshReportGroup(ctx, reportID)
}

// residueTextHash is a report text's content identity: two
// reports carrying the same text are the same reading work.
func residueTextHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// residueBackoff is the wait after the nth failed pass.
func residueBackoff(attempts int32) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	if attempts > 5 {
		attempts = 5
	}
	return residueBackoffBase << (attempts - 1)
}

// recordResidueFailure counts one failed pass durably. At the
// cap the report dead-letters with the failure recorded;
// otherwise the next attempt waits out the backoff. A pass
// over changed text starts the count over.
func (app *Application) recordResidueFailure(ctx context.Context, reportID int64, hash string, prev db.IntelResidueAttempt, prevErr error, passErr error) {
	now := time.Now().UTC()
	attempts := int32(1)
	if prevErr == nil && prev.TextHash == hash {
		attempts = prev.Attempts + 1
	}
	reason := passErr.Error()
	if len(reason) > 300 {
		reason = reason[:300]
	}
	if attempts >= residueMaxAttempts {
		if err := app.queries.MarkResidueDead(ctx, db.MarkResidueDeadParams{
			ReportID: reportID, TextHash: hash, Attempts: attempts,
			LastAttemptAt: timeSet(now), DeadReason: reason,
		}); err != nil {
			logging.Warnf("residue: dead-letter report %d: %v", reportID, err)
			return
		}
		logging.Warnf("residue: report %d leaves the backlog after %d failed passes: %s", reportID, attempts, reason)
		return
	}
	if err := app.queries.UpsertResidueAttempt(ctx, db.UpsertResidueAttemptParams{
		ReportID: reportID, TextHash: hash, Attempts: attempts,
		LastAttemptAt: timeSet(now), NextAttemptAt: timeSet(now.Add(residueBackoff(attempts))),
	}); err != nil {
		logging.Warnf("residue: count failed pass for report %d: %v", reportID, err)
	}
}

// applyResidueGateReading is the fuse step for gate readings.
// A system claim from the pass that names one of the report
// system's neighbours, in a report whose text speaks of a
// gate, is verified by construction: the neighbour list the
// claim was checked against is built from sde_system_jumps,
// so the named system is a real gate of the report's system.
// A verified gate is written into the report's stored
// analysis, marked as model-read, so the map, the place pages
// and the sentences use it like a deterministic one. A clear
// the deterministic reading could not scope becomes the gate
// clear it always said. Anything else the pass said stays a
// note, exactly as before.
func (app *Application) applyResidueGateReading(ctx context.Context, row db.IntelReport, a intelanalyze.Analysis, in residueInput, notes []residueNote) {
	if a.Gate > 0 || !mentionsGate(row.Text) {
		return
	}
	neighbours := map[int64]bool{}
	for _, e := range in.Neighbours {
		neighbours[e.ID] = true
	}
	var gate int64
	for _, n := range notes {
		if n.Kind == "system" && !n.Unconfirmed && n.EntityID > 0 && n.EntityID != row.SystemID && neighbours[n.EntityID] {
			gate = n.EntityID
			break
		}
	}
	if gate == 0 {
		return
	}
	// Re-read before writing: the report may have settled
	// further since the pass began, and its newest reading
	// is the one the gate joins.
	fresh, err := app.queries.GetIntelReportFull(ctx, row.ID)
	if err != nil {
		return
	}
	var na intelanalyze.Analysis
	if fresh.Analysis == "" || json.Unmarshal([]byte(fresh.Analysis), &na) != nil || na.Gate > 0 {
		return
	}
	na.Gate = gate
	na.GateModel = true
	kind := fresh.Kind
	if fresh.Clear && len(na.Pilots) == 0 && len(na.Ships) == 0 && na.Count == 0 {
		kind = intelanalyze.KindClear
	}
	raw, err := json.Marshal(na)
	if err != nil {
		return
	}
	if err := app.queries.SetIntelReportAnalysis(ctx, db.SetIntelReportAnalysisParams{
		ID: row.ID, Kind: kind, Analysis: string(raw), AnalysisPending: false,
	}); err != nil {
		logging.Warnf("residue: write gate reading of report %d: %v", row.ID, err)
		return
	}
	logging.Infof("residue: report %d's gate read as system %d by the model pass", row.ID, gate)
	app.refreshReportGroup(ctx, row.ID)
}

// storeResiduePass logs one completed pass and its validated
// notes in one transaction. The call is logged even when the
// reply parsed to nothing: the pair is the corpus's material
// either way.
func (app *Application) storeResiduePass(ctx context.Context, in residueInput, raw, textHash string, notes []residueNote) {
	spans, _ := json.Marshal(in.Spans)
	tx, err := app.db.BeginTx(ctx, nil)
	if err != nil {
		logging.Warnf("residue: store pass for report %d: %v", in.ReportID, err)
		return
	}
	defer tx.Rollback()
	q := db.New(tx)
	now := time.Now().UTC()
	callID, err := q.InsertIntelResidueCall(ctx, db.InsertIntelResidueCallParams{
		ReportID: in.ReportID, Spans: string(spans), Output: raw, CreatedAt: now, TextHash: textHash,
	})
	if err != nil {
		logging.Warnf("residue: store pass for report %d: %v", in.ReportID, err)
		return
	}
	for _, n := range notes {
		if err := q.InsertIntelResidueNote(ctx, db.InsertIntelResidueNoteParams{
			ReportID: in.ReportID, CallID: callID, Span: n.Span, Kind: n.Kind,
			EntityID: n.EntityID, EntityName: n.EntityName, N: int32(n.N),
			Unconfirmed: n.Unconfirmed, CreatedAt: now,
		}); err != nil {
			logging.Warnf("residue: store note for report %d: %v", in.ReportID, err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		logging.Warnf("residue: store pass for report %d: %v", in.ReportID, err)
	}
}

// residuePrompt is the whole ask, in one message: the entities
// the reading resolved, the spans it left, and the reply shape.
func residuePrompt(in residueInput) string {
	var b strings.Builder
	b.WriteString("You read the leftover fragments of an EVE Online intel report. The report's own reading already resolved the entities listed below; the fragments are the parts it could not place.\n\n")
	writeEntities := func(label string, list []residueEntity) {
		if len(list) == 0 {
			return
		}
		parts := make([]string, len(list))
		for i, e := range list {
			parts[i] = strconv.FormatInt(e.ID, 10) + " " + e.Name
		}
		b.WriteString(label + ": " + strings.Join(parts, "; ") + "\n")
	}
	writeEntities("Pilots", in.Pilots)
	writeEntities("Ships", in.Ships)
	writeEntities("Systems", in.Systems)
	writeEntities("Neighbouring systems", in.Neighbours)
	b.WriteString("\nFragments:\n")
	for _, s := range in.Spans {
		b.WriteString("- " + s + "\n")
	}
	if len(in.Suspect) > 0 {
		b.WriteString("\nThe reading took these fragments for the pilot of the same name, but each is also an everyday word: " + strings.Join(in.Suspect, "; ") + ". For each, claim it as that pilot only when the report plainly names a pilot there; when it is the word (a count, an action, a direction), claim that instead.\n")
	}
	b.WriteString(`
Answer with JSON in exactly this shape and nothing else: {"claims":[{"span":"...","kind":"...","id":0,"name":"...","n":0}]}
A claim reads one fragment. "span" is the fragment exactly as given. "kind" is one of: pilot, ship, system, count, direction, action, structure.
For pilot, ship and system claims: "id" is the id of a listed entity the fragment names or plainly refers to, and "name" is that entity's name as listed. When the fragment names a pilot, ship or system the lists do not hold, set "id" to 0 and "name" to the name the fragment itself gives. Never invent an id; an id must come from the lists.
For a count claim, "n" is the number the fragment states. For direction, action and structure claims, "name" is the reading in a word or two (for example "inbound", "landing", "anchored").
A fragment may name the gate to a neighbouring system, exactly or in a shortened or misspelled form of that system's name: claim it as a system claim naming that neighbouring system.
Leave out any fragment you cannot place.`)
	return b.String()
}

// parseResidueReply reads a reply in the strict shape: unknown
// fields, trailing text, or a claim count past the cap make the
// whole reply unparseable, and an unparseable reply is discarded
// whole.
func parseResidueReply(raw string) (residueReply, bool) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	var rep residueReply
	if err := dec.Decode(&rep); err != nil {
		return residueReply{}, false
	}
	if dec.More() || len(rep.Claims) > residueMaxClaims {
		return residueReply{}, false
	}
	return rep, true
}

// validateResidueReply checks a parsed reply against what the
// reading resolved. A claim that references an entity by an id
// the reading never resolved is dropped: it is not converted
// into a suggestion, because a name standing beside an invented
// id is not trusted either. A claim naming something new, with
// no id, is kept as an unconfirmed note under the name the span
// itself gives. A claim naming a resolved entity stands as a
// note on that entity, under the reading's own name for it.
func validateResidueReply(in residueInput, raw string) []residueNote {
	rep, ok := parseResidueReply(raw)
	if !ok {
		return nil
	}
	spans := map[string]bool{}
	for _, s := range in.Spans {
		spans[s] = true
	}
	byID := func(list []residueEntity) map[int64]residueEntity {
		m := make(map[int64]residueEntity, len(list))
		for _, e := range list {
			m[e.ID] = e
		}
		return m
	}
	byName := func(list []residueEntity) map[string]residueEntity {
		m := make(map[string]residueEntity, len(list))
		for _, e := range list {
			l := strings.ToLower(e.Name)
			if _, dup := m[l]; !dup {
				m[l] = e
			}
		}
		return m
	}
	systemsAll := make([]residueEntity, 0, len(in.Systems)+len(in.Neighbours))
	systemsAll = append(systemsAll, in.Systems...)
	systemsAll = append(systemsAll, in.Neighbours...)
	entitySet := func(kind string) (map[int64]residueEntity, map[string]residueEntity) {
		switch kind {
		case "pilot":
			return byID(in.Pilots), byName(in.Pilots)
		case "ship":
			return byID(in.Ships), byName(in.Ships)
		default:
			// A system claim may name the reading's systems
			// or a neighbouring one: the gate a loose phrase
			// meant is among the neighbours.
			return byID(systemsAll), byName(systemsAll)
		}
	}
	boundN := func(n int) int {
		if n < 0 || n > 999 {
			return 0
		}
		return n
	}
	var notes []residueNote
	seen := map[string]bool{}
	perSpan := map[string]int{}
	for _, c := range rep.Claims {
		if !residueKinds[c.Kind] || !spans[c.Span] || perSpan[c.Span] >= residueMaxClaimsPerSpan {
			continue
		}
		var note residueNote
		switch c.Kind {
		case "pilot", "ship", "system":
			ids, names := entitySet(c.Kind)
			if c.ID > 0 {
				e, ok := ids[c.ID]
				if !ok {
					continue // a reference the reading never made: dropped
				}
				note = residueNote{Span: c.Span, Kind: c.Kind, EntityID: e.ID, EntityName: e.Name, N: boundN(c.N)}
			} else {
				name := strings.TrimSpace(c.Name)
				if name == "" {
					continue
				}
				if e, ok := names[strings.ToLower(name)]; ok {
					note = residueNote{Span: c.Span, Kind: c.Kind, EntityID: e.ID, EntityName: e.Name, N: boundN(c.N)}
				} else {
					note = residueNote{Span: c.Span, Kind: c.Kind, EntityName: capRunes(name, 128), N: boundN(c.N), Unconfirmed: true}
				}
			}
		case "count":
			if c.N < 1 || c.N > 999 {
				continue
			}
			note = residueNote{Span: c.Span, Kind: c.Kind, N: c.N, Unconfirmed: true}
		default: // direction, action, structure: a reading of the span, no entity
			name := strings.TrimSpace(c.Name)
			if name == "" {
				continue
			}
			note = residueNote{Span: c.Span, Kind: c.Kind, EntityName: capRunes(name, 64), Unconfirmed: true}
		}
		key := note.Span + "\x00" + note.Kind + "\x00" + strconv.FormatInt(note.EntityID, 10) + "\x00" + note.EntityName
		if seen[key] {
			continue
		}
		seen[key] = true
		perSpan[c.Span]++
		notes = append(notes, note)
	}
	return notes
}

// capRunes bounds a stored name to n runes.
func capRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
