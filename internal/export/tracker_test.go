package export

import (
	"sync"
	"testing"
	"time"
)

// TestTrackerUnderConcurrency exercises the tracker the way the export does:
// several workers updating their own counters while a reporter reads across all
// of them. Its value is under -race, which CI runs on Linux.
func TestTrackerUnderConcurrency(t *testing.T) {
	const (
		workers  = 8
		perTable = 500
		estBytes = 1000
	)
	tr := newTracker(workers * estBytes)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// A reporter reading snapshots as fast as it can, to overlap with writers.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				if p, ok := tr.snapshot(workers); ok {
					// Touch the derived values too: they read the same fields.
					_, _ = p.OverallFraction()
					_, _ = p.OverallETA()
				}
			}
		}
	}()

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			run := &tableRun{
				table: string(rune('A' + w)), index: w + 1,
				estRows: perTable, estBytes: estBytes,
			}
			tr.begin(run)
			for i := int64(1); i <= perTable; i++ {
				run.rows.Store(i)
				run.bytes.Store(i * 10)
			}
			tr.finish(run)
		}(w)
	}

	// Wait for the writers, then stop the reader.
	done := make(chan struct{})
	go func() { time.Sleep(50 * time.Millisecond); close(done) }()
	<-done
	close(stop)
	wg.Wait()

	if got := tr.doneBytes.Load(); got != workers*estBytes {
		t.Errorf("doneBytes = %d, want %d", got, workers*estBytes)
	}
	if _, ok := tr.snapshot(workers); ok {
		t.Error("no table is in flight, so there should be nothing to report")
	}
}

// TestSnapshotReportsTheLongestRunningTable - with several tables in flight the
// one worth naming is the one the run is waiting on, not whichever finished
// most recently.
func TestSnapshotReportsTheLongestRunningTable(t *testing.T) {
	tr := newTracker(3000)

	oldest := &tableRun{table: "dbo.Big", index: 1, estRows: 100, estBytes: 1000}
	tr.begin(oldest)
	time.Sleep(5 * time.Millisecond)
	for _, name := range []string{"dbo.Small1", "dbo.Small2"} {
		r := &tableRun{table: name, index: 2, estRows: 100, estBytes: 1000}
		r.rows.Store(50) // half done, so they contribute partial credit
		tr.begin(r)
	}

	p, ok := tr.snapshot(3)
	if !ok {
		t.Fatal("expected a snapshot")
	}
	if p.Table != "dbo.Big" {
		t.Errorf("named %q, want the longest-running dbo.Big", p.Table)
	}
	if p.InFlight != 3 {
		t.Errorf("InFlight = %d, want 3", p.InFlight)
	}
	// The two half-done tables contribute 500 bytes each; the reported table's
	// own credit is added by OverallFraction, so it must not be counted here.
	if p.DoneBytes != 1000 {
		t.Errorf("DoneBytes = %d, want 1000 from the two other tables", p.DoneBytes)
	}
}
