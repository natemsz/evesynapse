package appraise

import "testing"

func TestReprocessBatches(t *testing.T) {
	cases := []struct {
		name       string
		qty        int64
		portion    int64
		prune      bool
		wantBatch  float64
		wantPruned bool
	}{
		{"no portion reprocesses whole", 50, 0, true, 50, false},
		{"portion of one reprocesses whole", 50, 1, true, 50, false},
		{"full batches with prune", 250, 100, true, 2, false},
		{"below one batch pruned", 50, 100, true, 0, true},
		{"below one batch valued in part without prune", 50, 100, false, 0.5, false},
		{"partial batch valued in part without prune", 250, 100, false, 2.5, false},
	}
	for _, c := range cases {
		batches, pruned := reprocessBatches(c.qty, c.portion, c.prune)
		if batches != c.wantBatch || pruned != c.wantPruned {
			t.Errorf("%s: reprocessBatches(%d, %d, %v) = (%v, %v), want (%v, %v)",
				c.name, c.qty, c.portion, c.prune, batches, pruned, c.wantBatch, c.wantPruned)
		}
	}
}

func TestFormatVolume(t *testing.T) {
	for in, want := range map[float64]string{
		0: "0", 0.5: "0.5", 12.30: "12.3", 1200: "1200", 0.25: "0.25",
	} {
		if got := formatVolume(in); got != want {
			t.Errorf("formatVolume(%v) = %q, want %q", in, got, want)
		}
	}
}
