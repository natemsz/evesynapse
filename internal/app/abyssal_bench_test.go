package app

// The abyssal benchmark harness (dev-time): runs the
// production scorer and estimator over a sampled set of real
// abyssal items (collected from the public contract routes;
// see the benchmark report in the goal files) and writes the
// results as JSON for the report generator. It is gated on
// environment variables and skips in CI:
//
//	ABYSSAL_BENCH_DATA   collector JSON (items, rolls, asks,
//	                     typical proxies, base attributes)
//	ABYSSAL_BENCH_RANGES dynamicItemAttributes.csv (FSD bands)
//	ABYSSAL_BENCH_OUT    results JSON path
//
// Family weights are the real seeded ones, read from the
// test database (schema 055), so the benchmark exercises the
// exact v1 configuration that ships.

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"

	db "evesynapse/internal/db/sqlc"
)

type benchHit struct {
	ItemID         int64              `json:"item_id"`
	TypeID         int64              `json:"type_id"`
	TypeName       string             `json:"type_name"`
	GroupName      string             `json:"group_name"`
	SourceTypeID   int64              `json:"source_type_id"`
	SourceName     string             `json:"source_name"`
	MutatorTypeID  int64              `json:"mutator_type_id"`
	Ask            float64            `json:"ask"`
	SourceTypical  float64            `json:"source_typical"`
	MutatorTypical float64            `json:"mutator_typical"`
	Attributes     []abyssalRollKV    `json:"attributes"`
	BaseAttrs      map[string]float64 `json:"base_attrs"`
}

type benchData struct {
	Hits     []benchHit `json:"hits"`
	AttrMeta map[string]struct {
		HighIsGood bool `json:"high_is_good"`
	} `json:"attr_meta"`
}

type benchResult struct {
	ItemID      int64              `json:"item_id"`
	Family      string             `json:"family"`
	HasScore    bool               `json:"has_score"`
	Score       float64            `json:"score,omitempty"`
	CapKind     string             `json:"cap_kind,omitempty"`
	CapAttr     int64              `json:"cap_attr,omitempty"`
	Percentiles map[string]float64 `json:"percentiles"`
	EstOK       bool               `json:"est_ok"`
	EstValue    float64            `json:"est_value,omitempty"`
	EstLow      float64            `json:"est_low,omitempty"`
	EstHigh     float64            `json:"est_high,omitempty"`
	Basis       string             `json:"basis,omitempty"`
	Scope       string             `json:"scope,omitempty"`
	Comps       int                `json:"comps,omitempty"`
	Premium     float64            `json:"premium,omitempty"`
	Recreation  float64            `json:"recreation_cost,omitempty"`
	Attempts    float64            `json:"attempts,omitempty"`
}

func TestAbyssalBenchmarkHarness(t *testing.T) {
	dataPath := os.Getenv("ABYSSAL_BENCH_DATA")
	rangesPath := os.Getenv("ABYSSAL_BENCH_RANGES")
	outPath := os.Getenv("ABYSSAL_BENCH_OUT")
	if dataPath == "" || rangesPath == "" || outPath == "" {
		t.Skip("benchmark harness: ABYSSAL_BENCH_DATA/RANGES/OUT unset")
	}
	_, _, q := buildCorpTestApp(t, roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 404, Body: http.NoBody, Header: make(http.Header)}, nil
	}))
	ctx := context.Background()

	weightRows, err := q.ListAllAbyssalFamilyWeights(ctx)
	if err != nil {
		t.Fatalf("weights: %v", err)
	}
	weightsByFamily := map[string][]db.AbyssalFamilyWeight{}
	canonByKey := map[string]string{}
	for _, w := range weightRows {
		weightsByFamily[w.Family] = append(weightsByFamily[w.Family], w)
		canonByKey[abyssalFamilyKey(w.Family)] = w.Family
	}

	raw, err := os.ReadFile(rangesPath)
	if err != nil {
		t.Fatalf("ranges: %v", err)
	}
	r := csv.NewReader(strings.NewReader(strings.TrimPrefix(string(raw), "\uFEFF")))
	recs, err := r.ReadAll()
	if err != nil {
		t.Fatalf("ranges csv: %v", err)
	}
	rangesByMutator := map[int64]map[int64]abyssalRange{}
	for i, rec := range recs {
		if i == 0 || len(rec) < 4 {
			continue
		}
		mut, _ := strconv.ParseInt(strings.TrimSpace(rec[0]), 10, 64)
		attr, _ := strconv.ParseInt(strings.TrimSpace(rec[1]), 10, 64)
		mn, _ := strconv.ParseFloat(strings.TrimSpace(rec[2]), 64)
		mx, _ := strconv.ParseFloat(strings.TrimSpace(rec[3]), 64)
		if rangesByMutator[mut] == nil {
			rangesByMutator[mut] = map[int64]abyssalRange{}
		}
		rangesByMutator[mut][attr] = abyssalRange{MinMult: mn, MaxMult: mx}
	}

	raw, err = os.ReadFile(dataPath)
	if err != nil {
		t.Fatalf("data: %v", err)
	}
	var data benchData
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("data json: %v", err)
	}
	highGood := map[int64]bool{}
	for k, v := range data.AttrMeta {
		if id, err := strconv.ParseInt(k, 10, 64); err == nil {
			highGood[id] = v.HighIsGood
		}
	}

	// Phase one: score everything.
	type entry struct {
		hit    benchHit
		family string
		in     abyssalScoringInputs
		sc     abyssalScoring
		rolls  map[int64]float64
	}
	entries := make([]entry, 0, len(data.Hits))
	type scoredInfo struct {
		score      float64
		primaryAbs float64
	}
	scoreByItem := map[int64]scoredInfo{}
	for _, h := range data.Hits {
		slug := abyssalSlug(h.GroupName)
		family := slug
		if canon, ok := canonByKey[abyssalFamilyKey(slug)]; ok {
			family = canon
		}
		rolls := map[int64]float64{}
		for _, kv := range h.Attributes {
			rolls[kv.AttributeID] = kv.Value
		}
		base := map[int64]float64{}
		for k, v := range h.BaseAttrs {
			if id, err := strconv.ParseInt(k, 10, 64); err == nil {
				base[id] = v
			}
		}
		in := abyssalScoringInputs{
			Family:   family,
			Rolls:    rolls,
			Ranges:   rangesByMutator[h.MutatorTypeID],
			Base:     base,
			HighGood: highGood,
			Weights:  weightsByFamily[family],
		}
		sc := scoreAbyssal(in)
		entries = append(entries, entry{hit: h, family: family, in: in, sc: sc, rolls: rolls})
		if sc.HasScore {
			scoreByItem[h.ItemID] = scoredInfo{score: sc.Score, primaryAbs: sc.PrimaryAbs}
		}
	}

	// Phase two: estimate against the sample as corpus (all
	// listings live, dynamics weight 1).
	results := make([]benchResult, 0, len(entries))
	for _, e := range entries {
		res := benchResult{ItemID: e.hit.ItemID, Family: e.family, Percentiles: map[string]float64{}}
		for id, p := range e.sc.Percentiles {
			res.Percentiles[strconv.FormatInt(id, 10)] = p
		}
		if e.sc.HasScore {
			res.HasScore = true
			res.Score = e.sc.Score
			res.CapKind = e.sc.CapKind
			res.CapAttr = e.sc.CapAttributeID
			var comps []abyssalComp
			for _, o := range entries {
				if o.hit.ItemID == e.hit.ItemID || o.family != e.family {
					continue
				}
				s, ok := scoreByItem[o.hit.ItemID]
				if !ok {
					continue
				}
				comps = append(comps, abyssalComp{
					ItemID: o.hit.ItemID, SourceTypeID: o.hit.SourceTypeID,
					Score: s.score, Ask: o.hit.Ask, SourceTypical: o.hit.SourceTypical,
					DynamicsWeight: 1, PrimaryAbs: s.primaryAbs,
				})
			}
			est := estimateAbyssal(abyssalEstimateInput{
				Score: e.sc.Score, SourceTypical: e.hit.SourceTypical,
				SourceTypeID: e.hit.SourceTypeID, PrimaryAbs: e.sc.PrimaryAbs, Comps: comps,
			})
			if est.OK {
				res.EstOK = true
				res.EstValue, res.EstLow, res.EstHigh = est.Value, est.Low, est.High
				res.Basis, res.Scope, res.Comps = est.Basis, est.Scope, est.Comps
				res.Premium = est.PremiumMult
			}
			// Recreation candidates: every source this
			// mutaplasmid appears on in the sample.
			baseBySource := map[int64]map[int64]float64{}
			typBySource := map[int64]float64{}
			for _, o := range entries {
				if o.hit.MutatorTypeID != e.hit.MutatorTypeID {
					continue
				}
				if _, ok := baseBySource[o.hit.SourceTypeID]; !ok {
					b := map[int64]float64{}
					for k, v := range o.hit.BaseAttrs {
						if id, err := strconv.ParseInt(k, 10, 64); err == nil {
							b[id] = v
						}
					}
					baseBySource[o.hit.SourceTypeID] = b
					typBySource[o.hit.SourceTypeID] = o.hit.SourceTypical
				}
			}
			var paths []abyssalRecreationPath
			for src, b := range baseBySource {
				paths = append(paths, abyssalRecreationPath{SourceTypeID: src, BaseTypical: typBySource[src], Base: b,
					MutatorTypical: e.hit.MutatorTypical, Ranges: e.in.Ranges})
			}
			if cost, attempts, ok := abyssalRecreationCost(e.sc, e.in, e.rolls, paths, nil); ok {
				res.Recreation, res.Attempts = cost, attempts
			}
		}
		results = append(results, res)
	}
	out, err := json.MarshalIndent(results, "", " ")
	if err != nil {
		t.Fatalf("marshal results: %v", err)
	}
	if err := os.WriteFile(outPath, out, 0o644); err != nil {
		t.Fatalf("write results: %v", err)
	}
	t.Logf("benchmark: %d items -> %s", len(results), outPath)
}
