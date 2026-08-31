package export

import (
	"sync"
	"sync/atomic"
	"time"
)

// tracker aggregates what the workers are doing so the run reports progress
// once, on a timer, rather than once per worker.
//
// Letting each worker report for itself would make the status line flick
// between unrelated tables several times a second, which is unreadable and
// tells you less than one honest summary.
type tracker struct {
	mu       sync.Mutex
	inFlight map[string]*tableRun

	// totalBytes is the source size of everything this run will read, and
	// doneBytes the part of it already finished. Both are in the engine's
	// source bytes, the unit the whole-export estimate works in.
	totalBytes int64
	doneBytes  atomic.Int64
	started    time.Time
}

// tableRun is one table being read. Rows and bytes are updated by the worker
// that owns it and read by the reporter, so they are atomic; everything else is
// written once before the run is published.
type tableRun struct {
	table    string
	index    int // 1-based position, for the [i/n] counter
	estRows  int64
	estBytes int64
	started  time.Time
	rows     atomic.Int64
	bytes    atomic.Int64
}

func newTracker(totalBytes int64) *tracker {
	return &tracker{
		inFlight:   map[string]*tableRun{},
		totalBytes: totalBytes,
		started:    time.Now(),
	}
}

func (tr *tracker) begin(r *tableRun) {
	r.started = time.Now()
	tr.mu.Lock()
	tr.inFlight[r.table] = r
	tr.mu.Unlock()
}

// finish retires a table and credits its full size, replacing whatever partial
// credit it had while in flight.
func (tr *tracker) finish(r *tableRun) {
	tr.mu.Lock()
	delete(tr.inFlight, r.table)
	tr.mu.Unlock()
	tr.doneBytes.Add(r.estBytes)
}

// snapshot builds one Progress covering the whole run.
//
// The table it names is the one that has been running longest, because with
// several in flight that is the one the run is waiting on - the others will
// finish before it and are not what you want to watch.
func (tr *tracker) snapshot(tableCount int) (Progress, bool) {
	tr.mu.Lock()
	defer tr.mu.Unlock()

	if len(tr.inFlight) == 0 {
		return Progress{}, false
	}

	var oldest *tableRun
	var othersCredit float64
	for _, r := range tr.inFlight {
		if oldest == nil || r.started.Before(oldest.started) {
			oldest = r
		}
	}
	// Every table except the one being reported contributes its partial credit
	// here; the reported one is credited by Progress.OverallFraction from its
	// own row counts, so counting it twice has to be avoided.
	for _, r := range tr.inFlight {
		if r == oldest || r.estRows <= 0 {
			continue
		}
		f := float64(r.rows.Load()) / float64(r.estRows)
		if f > 1 {
			f = 1
		}
		othersCredit += f * float64(r.estBytes)
	}

	now := time.Now()
	return Progress{
		Table:         oldest.table,
		TableIndex:    oldest.index,
		TableCount:    tableCount,
		Rows:          oldest.rows.Load(),
		EstimatedRows: oldest.estRows,
		TableBytes:    oldest.estBytes,
		Bytes:         oldest.bytes.Load(),
		Elapsed:       now.Sub(oldest.started),
		InFlight:      len(tr.inFlight),
		TotalBytes:    tr.totalBytes,
		DoneBytes:     tr.doneBytes.Load() + int64(othersCredit),
		TotalElapsed:  now.Sub(tr.started),
	}, true
}

// report emits a snapshot on every tick until stop is closed. It runs on its
// own goroutine so that a worker blocked on a slow row does not hold up the
// display, and so the interval is wall-clock rather than row-driven.
func (tr *tracker) report(opts Options, tableCount int, interval time.Duration, stop <-chan struct{}) {
	if interval <= 0 {
		return
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			if p, ok := tr.snapshot(tableCount); ok {
				opts.progress(p)
			}
		}
	}
}
