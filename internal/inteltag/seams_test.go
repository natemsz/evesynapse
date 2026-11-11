package inteltag

// NewModel makes an empty model over Kinds.
func NewModel() *Model {
	m := &Model{Labels: []string{"O"}, W: map[string][]float32{}}
	for _, k := range Kinds {
		m.Labels = append(m.Labels, "B-"+k, "I-"+k)
	}
	L := len(m.Labels)
	m.Trans = make([][]float32, L+1)
	for i := range m.Trans {
		m.Trans[i] = make([]float32, L)
	}
	return m
}
