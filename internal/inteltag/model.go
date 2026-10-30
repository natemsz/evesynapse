package inteltag

import (
	"compress/gzip"
	"encoding/gob"
	"io"
)

// modelFile is a model as it is saved: the labels, the transition
// scores, and each feature's scores over the labels.
type modelFile struct {
	Labels []string
	Trans  [][]float32
	W      map[string][]float32
}

// Save writes the model, gzipped.
func (m *Model) Save(w io.Writer) error {
	zw := gzip.NewWriter(w)
	if err := gob.NewEncoder(zw).Encode(modelFile{Labels: m.Labels, Trans: m.Trans, W: m.W}); err != nil {
		return err
	}
	return zw.Close()
}

// Load reads a model Save wrote. The caller sets its Lexicon.
func Load(r io.Reader) (*Model, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	var f modelFile
	if err := gob.NewDecoder(zr).Decode(&f); err != nil {
		return nil, err
	}
	return &Model{Labels: f.Labels, Trans: f.Trans, W: f.W}, nil
}
