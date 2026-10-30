package inteltag

import (
	"bytes"
	"reflect"
	"testing"
)

func TestTokenize(t *testing.T) {
	var got []string
	for _, tk := range Tokenize("1DQ1-A  Calebyr* (Ikitursa), status?") {
		got = append(got, tk.Text)
	}
	want := []string{"1DQ1-A", "Calebyr*", "(", "Ikitursa", ")", ",", "status", "?"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tokens %q, want %q", got, want)
	}
}

// An inside label never starts a report or follows another kind,
// and a link's star is left off the span.
func TestDecodeAndSpans(t *testing.T) {
	m := NewModel()
	idx := map[string]int{}
	for i, l := range m.Labels {
		idx[l] = i
	}
	// Scores that would like to start on I-pilot and end on I-ship.
	emit := [][]float64{make([]float64, len(m.Labels)), make([]float64, len(m.Labels))}
	emit[0][idx["I-pilot"]], emit[0][idx["B-pilot"]] = 5, 4
	emit[1][idx["I-ship"]], emit[1][idx["I-pilot"]] = 5, 4
	path := m.DecodeScores(emit)
	if m.Labels[path[0]] != "B-pilot" || m.Labels[path[1]] != "I-pilot" {
		t.Fatalf("path %v %v", m.Labels[path[0]], m.Labels[path[1]])
	}
	toks := Tokenize("Kara Venn*")
	if s := m.Spans(toks, path); len(s) != 1 || s[0] != (Span{0, 9, "pilot"}) {
		t.Errorf("spans %+v, want Kara Venn without the star", s)
	}
}

func TestSaveLoad(t *testing.T) {
	m := NewModel()
	m.W["w=sabre"] = make([]float32, len(m.Labels))
	m.W["w=sabre"][1] = 2.5
	m.Trans[0][1] = -1
	var b bytes.Buffer
	if err := m.Save(&b); err != nil {
		t.Fatal(err)
	}
	got, err := Load(&b)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.W, m.W) || !reflect.DeepEqual(got.Trans, m.Trans) || !reflect.DeepEqual(got.Labels, m.Labels) {
		t.Errorf("round trip changed the model")
	}
}
