// Command residue-corpus builds labeled training examples for the
// residue reader (internal/app/residue_model.go) from two production
// exports: the residue calls (spans in, raw reply out) and the
// reports with their final stored analyses.
//
// Labels come only from verified outcomes. A claim the pipeline
// confirmed (it names an entity the final reading resolved, or a
// count the final reading asserts) is a positive label in the
// target. A claim the model made that verification rejected (an
// invented id, a name the reading never resolved, a reading the
// live validation always flags unconfirmed) is recorded in the
// example's rejected list, so the corpus teaches the distinction.
//
// The exports do not carry everything the reader was shown at
// inference time, and the reconstruction is stated rather than
// papered over: the entity lists are rebuilt from each report's
// final analysis (the pass-time lists are not exported), the
// neighbour systems are rebuilt from the gate pairs the reports'
// final readings prove, and system names are recovered from the
// exports themselves (the fused "In <system>:" report sentences
// and the id-and-name strings in call output). Augmentation swaps
// a gate example's destination among the report system's
// reconstructed neighbours and recomputes the labels by running
// the same classification on the swapped material; when the
// exports prove only one neighbour, there is nothing honest to
// swap in and no variant is emitted.
package main

import (
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"evesynapse/internal/intelanalyze"
)

// claim mirrors residueClaim in internal/app/residue_model.go,
// the reply shape the reader is prompted with and must answer in.
type claim struct {
	Span string `json:"span"`
	Kind string `json:"kind"`
	ID   int64  `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
	N    int    `json:"n,omitempty"`
}

type reply struct {
	Claims []claim `json:"claims"`
}

type entity struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// rejectedClaim is one model claim verification rejected, with
// the reason the label logic gives.
type rejectedClaim struct {
	claim
	Reason string `json:"reason"`
}

type exampleInput struct {
	ReportID   int64    `json:"report_id"`
	SystemID   int64    `json:"system_id"`
	Pilots     []entity `json:"pilots"`
	Ships      []entity `json:"ships"`
	Systems    []entity `json:"systems"`
	Neighbours []entity `json:"neighbours"`
	Spans      []string `json:"spans"`
}

type swapInfo struct {
	FromSystem int64 `json:"from_system"`
	ToSystem   int64 `json:"to_system"`
}

type example struct {
	Input          exampleInput    `json:"input"`
	Target         reply           `json:"target"`
	Rejected       []rejectedClaim `json:"rejected"`
	Provenance     string          `json:"provenance"` // real or augmented (synthetic is a later step)
	SourceCallID   int64           `json:"source_call_id"`
	SourceReportID int64           `json:"source_report_id"`
	DerivedGate    bool            `json:"derived_gate,omitempty"`
	GateSpanAdded  bool            `json:"gate_span_added,omitempty"`
	Swap           *swapInfo       `json:"swap,omitempty"`
}

type residueCall struct {
	ID       int64
	ReportID int64
	SpansRaw string
	Output   string
}

type reportRow struct {
	ID          int64
	SystemID    int64
	Text        string
	Hostiles    int
	AnalysisRaw string
}

// reportView is the verified reading of one report, as the label
// logic sees it.
type reportView struct {
	ReportID   int64
	SystemID   int64
	Text       string
	Hostiles   int
	Analysis   intelanalyze.Analysis
	Gate       int64 // the verified gate: the gold override, else the final analysis
	DenyPilots map[int64]bool
}

var residueKinds = map[string]bool{
	"pilot": true, "ship": true, "system": true,
	"count": true, "direction": true, "action": true, "structure": true,
}

// goldGate and goldDenyPilots are the owner's hand-verified
// readings from 2026-10-24, where a report's stored final
// analysis predates the fix it verifies: report 42's analysis
// reads the Bosena gate's name as a friendly pilot and names no
// gate, and the owner verified the line was a gate clear naming
// Bosena. The label logic trusts the hand verification over the
// stale stored reading for this one report.
var goldGate = map[int64]int64{42: 30002387}
var goldDenyPilots = map[int64]map[int64]bool{42: {2113644992: true}}

// Reconstructed from the exports by buildIndex.
var (
	systemNames = map[int64]string{}
	adjacency   = map[int64]map[int64]bool{}
)

func main() {
	callsPath := flag.String("calls", "", "path to the residue calls CSV export")
	reportsPath := flag.String("reports", "", "path to the reports CSV export")
	outPath := flag.String("out", "", "path to write the corpus JSONL to")
	summaryPath := flag.String("summary", "", "path to write the summary markdown to (default: beside -out)")
	flag.Parse()
	if *callsPath == "" || *reportsPath == "" || *outPath == "" {
		fmt.Fprintln(os.Stderr, "residue-corpus: -calls, -reports and -out are required")
		os.Exit(2)
	}
	if *summaryPath == "" {
		*summaryPath = strings.TrimSuffix(*outPath, ".jsonl") + "-summary.md"
	}
	if err := run(*callsPath, *reportsPath, *outPath, *summaryPath); err != nil {
		fmt.Fprintln(os.Stderr, "residue-corpus:", err)
		os.Exit(1)
	}
}

func run(callsPath, reportsPath, outPath, summaryPath string) error {
	calls, err := readCalls(callsPath)
	if err != nil {
		return err
	}
	reports, err := readReports(reportsPath)
	if err != nil {
		return err
	}
	buildIndex(calls, reports)

	stats := newSummary()
	var examples []example
	for _, c := range calls {
		ex, reason := buildExample(c, reports)
		if reason != "" {
			stats.noExample = append(stats.noExample, noExample{CallID: c.ID, ReportID: c.ReportID, Reason: reason})
			continue
		}
		examples = append(examples, ex)
		stats.tally(ex)
	}
	// Augmentation runs over the real examples only, in call
	// order, and only emits a variant whose recomputed labels
	// still carry the swapped gate claim.
	real := examples
	for _, ex := range real {
		examples = append(examples, augment(ex, reports)...)
	}
	for _, ex := range examples[len(real):] {
		stats.tally(ex)
	}

	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	for _, ex := range examples {
		if err := enc.Encode(ex); err != nil {
			f.Close()
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	md := stats.render(len(calls))
	fmt.Print(md)
	return os.WriteFile(summaryPath, []byte(md), 0o644)
}

func readCalls(path string) ([]residueCall, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	var out []residueCall
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(rec) < 5 || rec[0] == "id" {
			continue
		}
		id, err1 := strconv.ParseInt(rec[0], 10, 64)
		rid, err2 := strconv.ParseInt(rec[1], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		out = append(out, residueCall{ID: id, ReportID: rid, SpansRaw: rec[2], Output: rec[3]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func readReports(path string) (map[int64]reportRow, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	out := map[int64]reportRow{}
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(rec) < 12 || rec[0] == "id" {
			continue
		}
		id, err := strconv.ParseInt(rec[0], 10, 64)
		if err != nil {
			continue
		}
		sys, _ := strconv.ParseInt(rec[2], 10, 64)
		hostiles, _ := strconv.Atoi(rec[5])
		out[id] = reportRow{ID: id, SystemID: sys, Text: rec[4], Hostiles: hostiles, AnalysisRaw: rec[11]}
	}
	return out, nil
}

var (
	inSystemRe  = regexp.MustCompile(`^In ([A-Za-z0-9'’\- ]{1,40}):\s`)
	idAndNameRe = regexp.MustCompile(`^(\d{5,})\s+(.+)$`)
)

// buildIndex recovers system names and gate adjacency from the
// exports themselves. Adjacency comes from the reports' final
// readings: a stored gate is a proven neighbour pair, both ways.
func buildIndex(calls []residueCall, reports map[int64]reportRow) {
	systemNames = map[int64]string{}
	adjacency = map[int64]map[int64]bool{}
	universe := map[int64]bool{}
	parsed := map[int64]intelanalyze.Analysis{}
	for id, r := range reports {
		universe[r.SystemID] = true
		var a intelanalyze.Analysis
		if r.AnalysisRaw != "" && json.Unmarshal([]byte(r.AnalysisRaw), &a) == nil {
			parsed[id] = a
			if a.Gate > 0 {
				universe[a.Gate] = true
			}
		}
		if m := inSystemRe.FindStringSubmatch(r.Text); m != nil {
			if _, ok := systemNames[r.SystemID]; !ok {
				systemNames[r.SystemID] = strings.TrimSpace(m[1])
			}
		}
	}
	link := func(a, b int64) {
		if adjacency[a] == nil {
			adjacency[a] = map[int64]bool{}
		}
		adjacency[a][b] = true
	}
	for id, r := range reports {
		a, ok := parsed[id]
		if !ok || a.Gate <= 0 || a.Gate == r.SystemID {
			continue
		}
		link(r.SystemID, a.Gate)
		link(a.Gate, r.SystemID)
	}
	// A claim whose name is "<id> <name>" names a system outright.
	for _, c := range calls {
		rep, ok := parseReply(c.Output)
		if !ok {
			continue
		}
		for _, cl := range rep.Claims {
			if m := idAndNameRe.FindStringSubmatch(strings.TrimSpace(cl.Name)); m != nil {
				id, err := strconv.ParseInt(m[1], 10, 64)
				if err == nil && universe[id] {
					if _, ok := systemNames[id]; !ok {
						systemNames[id] = strings.TrimSpace(m[2])
					}
				}
			}
		}
	}
}

// parseReply reads a reply in the strict live shape, mirroring
// parseResidueReply in internal/app/residue_model.go: unknown
// fields, trailing text, or a claim count past the cap make the
// whole reply unparseable.
func parseReply(raw string) (reply, bool) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	var rep reply
	if err := dec.Decode(&rep); err != nil {
		return reply{}, false
	}
	if dec.More() || len(rep.Claims) > 40 {
		return reply{}, false
	}
	return rep, true
}

// viewOf builds the verified reading of a report.
func viewOf(r reportRow) (reportView, string) {
	v := reportView{ReportID: r.ID, SystemID: r.SystemID, Text: r.Text, Hostiles: r.Hostiles}
	if r.AnalysisRaw != "" {
		if err := json.Unmarshal([]byte(r.AnalysisRaw), &v.Analysis); err != nil {
			return v, "report_analysis_unparseable"
		}
	}
	v.Gate = v.Analysis.Gate
	if g, ok := goldGate[r.ID]; ok {
		v.Gate = g
	}
	if d, ok := goldDenyPilots[r.ID]; ok {
		v.DenyPilots = d
	}
	return v, ""
}

// buildExample turns one residue call into one labeled example,
// or says why it cannot.
func buildExample(c residueCall, reports map[int64]reportRow) (example, string) {
	r, ok := reports[c.ReportID]
	if !ok {
		return example{}, "report_missing"
	}
	v, reason := viewOf(r)
	if reason != "" {
		return example{}, reason
	}
	var spans []string
	if err := json.Unmarshal([]byte(c.SpansRaw), &spans); err != nil {
		return example{}, "spans_unparseable"
	}
	rep, ok := parseReply(c.Output)
	if !ok {
		return example{}, "output_unparseable"
	}
	if len(rep.Claims) == 0 && v.Gate == 0 {
		return example{}, "no_claims_no_verified_gate"
	}
	target, rejected, outSpans, derived := classify(spans, rep.Claims, v)
	ex := example{
		Input:          buildInput(v, outSpans),
		Target:         reply{Claims: target},
		Rejected:       rejected,
		Provenance:     "real",
		SourceCallID:   c.ID,
		SourceReportID: c.ReportID,
		DerivedGate:    derived,
		GateSpanAdded:  len(outSpans) > len(spans),
	}
	if ex.Target.Claims == nil {
		ex.Target.Claims = []claim{}
	}
	if ex.Rejected == nil {
		ex.Rejected = []rejectedClaim{}
	}
	return ex, ""
}

// entitySets is the verified reading as lookups, from the final
// analysis and the reconstructed topology.
func (v reportView) entitySets() (pilots, ships, systems []entity) {
	for _, p := range v.Analysis.Pilots {
		if p.ID > 0 && !v.DenyPilots[p.ID] {
			pilots = append(pilots, entity{ID: p.ID, Name: p.Name})
		}
	}
	for _, s := range v.Analysis.Ships {
		if s.TypeID > 0 {
			ships = append(ships, entity{ID: s.TypeID, Name: s.Name})
		}
	}
	seen := map[int64]bool{}
	add := func(id int64) {
		if id > 0 && !seen[id] {
			seen[id] = true
			systems = append(systems, entity{ID: id, Name: systemNames[id]})
		}
	}
	add(v.SystemID)
	add(v.Gate)
	for id := range adjacency[v.SystemID] {
		add(id)
	}
	sort.Slice(systems, func(i, j int) bool { return systems[i].ID < systems[j].ID })
	return pilots, ships, systems
}

// classify sorts one reply's claims into the verified target and
// the rejected list, and derives the gate claim the verified
// reading carries when the model never claimed it.
func classify(spans []string, claims []claim, v reportView) (target []claim, rejected []rejectedClaim, outSpans []string, derived bool) {
	pilots, ships, systems := v.entitySets()
	spanSet := map[string]bool{}
	for _, s := range spans {
		spanSet[s] = true
	}
	verifiedCounts := map[int]bool{}
	if v.Analysis.Count > 0 {
		verifiedCounts[v.Analysis.Count] = true
	}
	if v.Hostiles > 0 {
		verifiedCounts[v.Hostiles] = true
	}
	flags := map[string]bool{}
	for _, f := range v.Analysis.Flags {
		flags[strings.ToLower(f)] = true
	}
	seenTarget := map[string]bool{}
	seenRejected := map[string]bool{}
	perSpan := map[string]int{}
	gateClaimed := false
	reject := func(c claim, reason string) {
		key := c.Span + "\x00" + c.Kind + "\x00" + strconv.FormatInt(c.ID, 10) + "\x00" + c.Name + "\x00" + strconv.Itoa(c.N) + "\x00" + reason
		if seenRejected[key] {
			return
		}
		seenRejected[key] = true
		rejected = append(rejected, rejectedClaim{claim: c, Reason: reason})
	}
	confirm := func(c claim) {
		key := c.Span + "\x00" + c.Kind + "\x00" + strconv.FormatInt(c.ID, 10) + "\x00" + c.Name + "\x00" + strconv.Itoa(c.N)
		if seenTarget[key] {
			return
		}
		seenTarget[key] = true
		target = append(target, c)
		if c.Kind == "system" && c.ID == v.Gate && v.Gate > 0 {
			gateClaimed = true
		}
	}
	for _, c := range claims {
		if !residueKinds[c.Kind] {
			reject(c, "unknown_kind")
			continue
		}
		if !spanSet[c.Span] {
			reject(c, "span_not_in_input")
			continue
		}
		perSpan[c.Span]++
		if perSpan[c.Span] > 8 {
			reject(c, "over_per_span_cap")
			continue
		}
		switch c.Kind {
		case "pilot", "ship", "system":
			var set []entity
			switch c.Kind {
			case "pilot":
				set = pilots
			case "ship":
				set = ships
			default:
				set = systems
			}
			if c.ID > 0 {
				if e, ok := byID(set, c.ID); ok {
					confirm(claim{Span: c.Span, Kind: c.Kind, ID: e.ID, Name: e.Name, N: c.N})
				} else {
					reject(c, "invented_or_unresolved_id")
				}
				continue
			}
			name := strings.TrimSpace(c.Name)
			if name == "" {
				reject(c, "empty_name")
				continue
			}
			var e entity
			var ok bool
			if c.Kind == "system" {
				e, ok = matchSystem(name, set)
			} else {
				e, ok = byName(set, name)
			}
			if ok {
				confirm(claim{Span: c.Span, Kind: c.Kind, ID: e.ID, Name: e.Name, N: c.N})
			} else {
				reject(c, "unconfirmed_name")
			}
		case "count":
			if c.N < 1 || c.N > 999 {
				reject(c, "count_out_of_range")
			} else if verifiedCounts[c.N] {
				confirm(c)
			} else {
				reject(c, "count_not_in_final_reading")
			}
		default: // direction, action, structure
			name := strings.TrimSpace(c.Name)
			if name == "" {
				reject(c, "empty_name")
			} else if flags[strings.ToLower(name)] {
				confirm(c)
			} else {
				reject(c, "unconfirmed_reading")
			}
		}
	}
	outSpans = spans
	// The derived gate claim: the verified reading names a gate
	// the model never claimed. Its span is the report's line
	// itself, the span the live step adds for a gate phrase no
	// residue span carries (processResidue's gate trigger).
	if v.Gate > 0 && !gateClaimed && mentionsGate(v.Text) {
		if !spanSet[v.Text] {
			outSpans = append(append([]string{}, spans...), v.Text)
		}
		confirm(claim{Span: v.Text, Kind: "system", ID: v.Gate, Name: systemNames[v.Gate]})
		derived = gateClaimed
	}
	return target, rejected, outSpans, derived
}

func byID(set []entity, id int64) (entity, bool) {
	for _, e := range set {
		if e.ID == id {
			return e, true
		}
	}
	return entity{}, false
}

func byName(set []entity, name string) (entity, bool) {
	l := strings.ToLower(strings.TrimSpace(name))
	for _, e := range set {
		if strings.ToLower(e.Name) == l {
			return e, true
		}
	}
	return entity{}, false
}

// matchSystem is byName widened for how systems actually get
// named in replies: the model's "<id> <name>" strings, and a
// shortened form of at least four letters.
func matchSystem(name string, set []entity) (entity, bool) {
	if e, ok := byName(set, name); ok {
		return e, true
	}
	l := strings.ToLower(strings.TrimSpace(name))
	for _, e := range set {
		el := strings.ToLower(e.Name)
		if el == "" {
			continue
		}
		if strings.Contains(l, el) || strings.Contains(l, strconv.FormatInt(e.ID, 10)) {
			return e, true
		}
		if len([]rune(l)) >= 4 && strings.HasPrefix(el, l) {
			return e, true
		}
	}
	return entity{}, false
}

func mentionsGate(text string) bool {
	for _, f := range strings.Fields(strings.ToLower(text)) {
		if strings.Trim(f, ",.;:*!()[]?\"'") == "gate" {
			return true
		}
	}
	return false
}

// buildInput rebuilds the reader's prompt material from the
// verified reading: the resolved entity lists and the spans.
// Neighbours are the reconstructed adjacency, minus the systems
// already listed (the live step dedupes them the same way).
func buildInput(v reportView, spans []string) exampleInput {
	pilots, ships, _ := v.entitySets()
	in := exampleInput{ReportID: v.ReportID, SystemID: v.SystemID, Spans: spans}
	in.Pilots = append([]entity{}, pilots...)
	in.Ships = append([]entity{}, ships...)
	in.Systems = []entity{{ID: v.SystemID, Name: systemNames[v.SystemID]}}
	listed := map[int64]bool{v.SystemID: true}
	if v.Gate > 0 && v.Gate != v.SystemID {
		in.Systems = append(in.Systems, entity{ID: v.Gate, Name: systemNames[v.Gate]})
		listed[v.Gate] = true
	}
	var nb []int64
	for id := range adjacency[v.SystemID] {
		if !listed[id] {
			nb = append(nb, id)
		}
	}
	sort.Slice(nb, func(i, j int) bool { return nb[i] < nb[j] })
	for _, id := range nb {
		in.Neighbours = append(in.Neighbours, entity{ID: id, Name: systemNames[id]})
	}
	if in.Pilots == nil {
		in.Pilots = []entity{}
	}
	if in.Ships == nil {
		in.Ships = []entity{}
	}
	if in.Neighbours == nil {
		in.Neighbours = []entity{}
	}
	return in
}

// augment emits gate-swap variants of one real example: the
// destination is swapped for another reconstructed neighbour of
// the report system, and the labels are recomputed by running
// classify on the swapped material.
func augment(ex example, reports map[int64]reportRow) []example {
	r, ok := reports[ex.SourceReportID]
	if !ok {
		return nil
	}
	v, reason := viewOf(r)
	if reason != "" || v.Gate == 0 {
		return nil
	}
	gateName := systemNames[v.Gate]
	if gateName == "" {
		return nil
	}
	// The claims as the model made them, swapped wherever they
	// reference the old gate; the target/rejected split is then
	// recomputed, never copied.
	rawClaims := append(append([]claim{}, ex.Target.Claims...), claimsOf(ex.Rejected)...)
	var alts []int64
	for id := range adjacency[v.SystemID] {
		if id != v.Gate && id != v.SystemID && systemNames[id] != "" {
			alts = append(alts, id)
		}
	}
	sort.Slice(alts, func(i, j int) bool { return alts[i] < alts[j] })
	var out []example
	for _, alt := range alts {
		altName := systemNames[alt]
		vv := v
		vv.Gate = alt
		vv.Text = replaceName(v.Text, gateName, altName)
		spans := make([]string, len(ex.Input.Spans))
		for i, s := range ex.Input.Spans {
			spans[i] = replaceName(s, gateName, altName)
		}
		swapped := make([]claim, len(rawClaims))
		for i, c := range rawClaims {
			swapped[i] = swapClaim(c, v.Gate, gateName, alt, altName)
		}
		target, rejected, outSpans, derived := classify(spans, swapped, vv)
		carried := false
		for _, t := range target {
			if t.Kind == "system" && t.ID == alt {
				carried = true
			}
		}
		if !carried {
			continue
		}
		variant := example{
			Input:          buildInput(vv, outSpans),
			Target:         reply{Claims: target},
			Rejected:       rejected,
			Provenance:     "augmented",
			SourceCallID:   ex.SourceCallID,
			SourceReportID: ex.SourceReportID,
			DerivedGate:    derived,
			GateSpanAdded:  len(outSpans) > len(spans),
			Swap:           &swapInfo{FromSystem: v.Gate, ToSystem: alt},
		}
		if variant.Target.Claims == nil {
			variant.Target.Claims = []claim{}
		}
		if variant.Rejected == nil {
			variant.Rejected = []rejectedClaim{}
		}
		out = append(out, variant)
	}
	return out
}

func claimsOf(rejected []rejectedClaim) []claim {
	out := make([]claim, len(rejected))
	for i, r := range rejected {
		out[i] = r.claim
	}
	return out
}

func swapClaim(c claim, fromID int64, fromName string, toID int64, toName string) claim {
	if c.ID == fromID {
		c.ID = toID
	}
	if c.Name != "" {
		c.Name = replaceName(c.Name, fromName, toName)
	}
	c.Span = replaceName(c.Span, fromName, toName)
	return c
}

var wordBoundaryCache = map[string]*regexp.Regexp{}

// replaceName swaps whole-word occurrences of a system name,
// case-insensitively, preserving nothing but the new name: the
// swapped material is training input, not a quotation.
func replaceName(s, from, to string) string {
	if from == "" || !strings.Contains(strings.ToLower(s), strings.ToLower(from)) {
		return s
	}
	re, ok := wordBoundaryCache[from]
	if !ok {
		re = regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(from) + `\b`)
		wordBoundaryCache[from] = re
	}
	return re.ReplaceAllString(s, to)
}

// --- summary ---

type noExample struct {
	CallID   int64
	ReportID int64
	Reason   string
}

type summary struct {
	byProvenance map[string]int
	targetByKind map[string]map[string]int
	rejectedBy   map[string]map[string]int
	rejectedKind map[string]map[string]int
	rejectedN    int
	noExample    []noExample
}

func newSummary() *summary {
	return &summary{
		byProvenance: map[string]int{},
		targetByKind: map[string]map[string]int{},
		rejectedBy:   map[string]map[string]int{},
		rejectedKind: map[string]map[string]int{},
	}
}

func (s *summary) tally(ex example) {
	s.byProvenance[ex.Provenance]++
	if s.targetByKind[ex.Provenance] == nil {
		s.targetByKind[ex.Provenance] = map[string]int{}
	}
	for _, c := range ex.Target.Claims {
		s.targetByKind[ex.Provenance][c.Kind]++
	}
	if s.rejectedBy[ex.Provenance] == nil {
		s.rejectedBy[ex.Provenance] = map[string]int{}
		s.rejectedKind[ex.Provenance] = map[string]int{}
	}
	for _, r := range ex.Rejected {
		s.rejectedBy[ex.Provenance][r.Reason]++
		s.rejectedKind[ex.Provenance][r.Kind]++
		s.rejectedN++
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (s *summary) render(totalCalls int) string {
	var b strings.Builder
	b.WriteString("# Residue corpus seed: summary\n\n")
	fmt.Fprintf(&b, "Built from %d residue calls and the reports export.\n\n", totalCalls)
	b.WriteString("## Examples by provenance\n\n")
	for _, p := range []string{"real", "augmented", "synthetic"} {
		fmt.Fprintf(&b, "- %s: %d\n", p, s.byProvenance[p])
	}
	b.WriteString("\n## Target claims by kind\n\n")
	for _, p := range sortedKeys(s.targetByKind) {
		for _, k := range sortedKeys(s.targetByKind[p]) {
			fmt.Fprintf(&b, "- %s/%s: %d\n", p, k, s.targetByKind[p][k])
		}
	}
	fmt.Fprintf(&b, "\n## Rejected claims\n\nTotal rejected: %d\n\n", s.rejectedN)
	for _, p := range sortedKeys(s.rejectedBy) {
		for _, r := range sortedKeys(s.rejectedBy[p]) {
			fmt.Fprintf(&b, "- %s/%s: %d\n", p, r, s.rejectedBy[p][r])
		}
	}
	b.WriteString("\n## Calls that produced no example\n\n")
	if len(s.noExample) == 0 {
		b.WriteString("None.\n")
	}
	for _, n := range s.noExample {
		fmt.Fprintf(&b, "- call %d (report %d): %s\n", n.CallID, n.ReportID, n.Reason)
	}
	b.WriteString(`
## Label rules and reconstruction notes

- A target claim is one the pipeline confirmed: it names an
  entity the report's final analysis resolved (by id, by exact
  name, or, for systems, by the reply's own "<id> <name>" string
  or a shortened form of at least four letters), or it is a count
  the final reading asserts (the analysis count or the report's
  hostile count; a number that only coincides with the standing
  split does not verify a count claim). Every other parsed claim
  is a labelled negative in the example's rejected list, with
  its reason.
- Direction, action and structure claims stand confirmed only
  when the final analysis carries the same reading as a flag;
  the live validation flags them unconfirmed in every other
  case, and they are labelled rejected here.
- When the verified reading names a gate the model never
  claimed, the target gains the gate's system claim on the
  report's own line, the span the live step adds for a gate
  phrase no residue span carries.
- Report 42 carries the owner's hand-verified reading (gate
  Bosena, 30002387): its stored analysis predates the gate fix
  and reads the gate's name as a friendly pilot, so that pilot
  is excluded from its entity lists and the gate is labelled
  from the hand verification.
- The entity lists are rebuilt from the final analyses (the
  pass-time lists are not exported), neighbour systems from
  the gate pairs the final readings prove, and system names
  from the exports themselves. Augmentation swaps a gate
  destination only among those proven neighbours and
  recomputes every label; where the exports prove a single
  neighbour pair, no destination swap exists and no variant
  is emitted.
`)
	return b.String()
}
