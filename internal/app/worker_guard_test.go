package app

// Tests for the worker's panic containment (worker.go, sde.go): a
// panic in background work is logged and absorbed, the pass that
// hit it reports what happened, and the rest of the worker carries
// on. (The recovered panics print their stacks to the test log;
// that is the guard working, not a failure.)

import (
	"context"
	"strings"
	"testing"
)

func TestRunGuardedContainsPanic(t *testing.T) {
	ran := false
	runGuarded("test pass", func() {
		ran = true
		panic("boom")
	})
	// Reaching this line at all is the point: the panic stopped at
	// the guard.
	if !ran {
		t.Fatal("the guarded pass never ran")
	}
}

func TestGuardedCycleReportsPanicAndRecovers(t *testing.T) {
	app := &Application{}
	ctx := context.Background()

	app.guardedCycle(ctx, func(context.Context) {
		app.updateWorkerStatus(func(s *workerStatus) { s.Warming = true })
		panic("boom")
	})
	status := app.snapshotWorkerStatus()
	if status.Warming {
		t.Fatal("a panicked cycle left the worker marked as still warming")
	}
	if !strings.Contains(status.Summary, "internal error") {
		t.Fatalf("summary %q does not report the failed cycle", status.Summary)
	}

	// The next cycle runs normally.
	ran := false
	app.guardedCycle(ctx, func(context.Context) { ran = true })
	if !ran {
		t.Fatal("the cycle after a panic did not run")
	}
}

func TestWarmPoolSurvivesPanickingItem(t *testing.T) {
	app := &Application{}
	ids := []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	budget := &warmBudget{left: len(ids)}
	resolved := app.runWarmPool(context.Background(), ids, budget, func(_ context.Context, id int64) bool {
		if id == 3 {
			panic("boom")
		}
		return true
	})
	// Every other item still resolves: one bad item costs only
	// itself, and the pool keeps draining so the feeder never
	// blocks.
	if want := len(ids) - 1; resolved != want {
		t.Fatalf("resolved %d of %d items, want %d", resolved, len(ids), want)
	}
}

func TestSDEPanicIsRecordedAsTheFailure(t *testing.T) {
	app := &Application{}
	func() {
		defer app.recoverSDEPanic("import")
		panic("boom")
	}()
	if got := app.snapshotSDEStatus().LastError; !strings.Contains(got, "internal error during import") {
		t.Fatalf("LastError = %q, want the import failure recorded", got)
	}
}
