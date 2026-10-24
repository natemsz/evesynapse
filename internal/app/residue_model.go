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
// reading (schema 091).
//
// The step adds nothing to the deterministic facts, and nothing
// anywhere renders its notes. A note either links a span to an
// entity the reading already resolved, or records a name the
// reading did not know: those stand flagged unconfirmed, never
// as a resolved entity. Every completed pass is logged whole
// (spans in, raw reply out) as a residue call, the raw material
// the label corpus is later built from. A reply that does not
// parse in the strict shape is discarded whole, never partially
// applied.
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
	"database/sql"
	"encoding/json"
	"errors"
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
	// One pass's deadline: a small reader on CPU takes seconds
	// over a report's spans; the cap keeps a wedged runtime
	// from holding the worker forever.
	residueCallTimeout = 120 * time.Second
	// A reply may carry at most this many claims, and this many
	// on any one span: a reading's leftovers are few, and a
	// reply past the caps is not the shape that was asked for.
	residueMaxClaims        = 40
	residueMaxClaimsPerSpan = 8
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
// until ctx ends.
func (app *Application) runResidueWorker(ctx context.Context) {
	if !app.cfg.ResidueModelEnabled() {
		logging.Infof("residue: step idle (RESIDUE_MODEL=off or no model file configured)")
		return
	}
	ch := app.residueQueueChan()
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-ch:
			app.processResidue(ctx, app.residueEngine(), id)
			app.residueMu.Lock()
			delete(app.residueQueued, id)
			app.residueMu.Unlock()
		}
	}
}

// residueEngine returns the local reader, made on first use and
// held for the process's life.
func (app *Application) residueEngine() residueEngine {
	app.residueMu.Lock()
	defer app.residueMu.Unlock()
	if app.residueEng == nil {
		app.residueEng = newLlamaEngine(app.cfg)
	}
	return app.residueEng
}

// processResidue runs one report's pass: the spans its settled
// analysis left, read against what the analysis resolved, the
// reply validated and stored. A reading with no residue never
// reaches the engine.
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
	if len(a.Residue) == 0 {
		return // the reading resolved everything: the step is skipped
	}
	in := residueInput{ReportID: reportID, SystemID: row.SystemID, Spans: a.Residue}
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
	callCtx, cancel := context.WithTimeout(ctx, residueCallTimeout)
	defer cancel()
	raw, err := eng.Complete(callCtx, residuePrompt(in))
	if err != nil {
		if !errors.Is(err, errResidueAbsent) {
			logging.Warnf("residue: report %d: %v", reportID, err)
		}
		return
	}
	app.storeResiduePass(ctx, in, raw, validateResidueReply(in, raw))
}

// storeResiduePass logs one completed pass and its validated
// notes in one transaction. The call is logged even when the
// reply parsed to nothing: the pair is the corpus's material
// either way.
func (app *Application) storeResiduePass(ctx context.Context, in residueInput, raw string, notes []residueNote) {
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
		ReportID: in.ReportID, Spans: string(spans), Output: raw, CreatedAt: now,
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
	b.WriteString("\nFragments:\n")
	for _, s := range in.Spans {
		b.WriteString("- " + s + "\n")
	}
	b.WriteString(`
Answer with JSON in exactly this shape and nothing else: {"claims":[{"span":"...","kind":"...","id":0,"name":"...","n":0}]}
A claim reads one fragment. "span" is the fragment exactly as given. "kind" is one of: pilot, ship, system, count, direction, action, structure.
For pilot, ship and system claims: "id" is the id of a listed entity the fragment names or plainly refers to, and "name" is that entity's name as listed. When the fragment names a pilot, ship or system the lists do not hold, set "id" to 0 and "name" to the name the fragment itself gives. Never invent an id; an id must come from the lists.
For a count claim, "n" is the number the fragment states. For direction, action and structure claims, "name" is the reading in a word or two (for example "inbound", "landing", "anchored").
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
	entitySet := func(kind string) (map[int64]residueEntity, map[string]residueEntity) {
		switch kind {
		case "pilot":
			return byID(in.Pilots), byName(in.Pilots)
		case "ship":
			return byID(in.Ships), byName(in.Ships)
		default:
			return byID(in.Systems), byName(in.Systems)
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
