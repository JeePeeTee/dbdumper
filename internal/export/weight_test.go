package export

import (
	"testing"

	"github.com/JeePeeTee/dbdumper/internal/model"
)

func TestProgressBytesDiscountsFilteredTables(t *testing.T) {
	plain := &model.Table{Schema: "dbo", Name: "Orders", EstimatedBytes: 1000}
	filtered := &model.Table{Schema: "dbo", Name: "LogEvents",
		EstimatedBytes: 9000, RowFilter: "CreatedAt >= '2026-01-01'"}

	if got := progressBytes(plain); got != 1000 {
		t.Errorf("an unfiltered table is worth its size: got %d, want 1000", got)
	}
	if got := progressBytes(filtered); got != 0 {
		t.Errorf("a filtered table's size says nothing about its work: got %d, want 0", got)
	}
}

// TestFilteredTablesDoNotMoveTheBar is the behaviour the discount exists for.
//
// --where can keep one row in a million. Counting such a table at its full
// source size made the bar crawl for the whole run and then jump by the
// table's entire share the moment it finished - here, from 0% straight to 90%
// without a row of the remaining work having been read.
func TestFilteredTablesDoNotMoveTheBar(t *testing.T) {
	plain := &model.Table{Schema: "dbo", Name: "Orders", EstimatedBytes: 1000}
	filtered := &model.Table{Schema: "dbo", Name: "LogEvents",
		EstimatedBytes: 9000, RowFilter: "CreatedAt >= '2026-01-01'"}

	tr := newTracker(progressBytes(plain) + progressBytes(filtered))
	if tr.totalBytes != 1000 {
		t.Fatalf("totalBytes = %d, want the unfiltered work only", tr.totalBytes)
	}

	// The plain table starts first, so it is the one snapshots name.
	runPlain := &tableRun{table: "dbo.Orders", index: 1, estRows: 100, estBytes: progressBytes(plain)}
	tr.begin(runPlain)
	// A filtered table has no usable row estimate either, which is why it
	// contributes no partial credit while in flight.
	runFiltered := &tableRun{table: "dbo.LogEvents", index: 2, estRows: 0, estBytes: progressBytes(filtered)}
	tr.begin(runFiltered)

	before := fractionOf(t, tr)
	tr.finish(runFiltered)
	after := fractionOf(t, tr)

	if before != after {
		t.Errorf("finishing a filtered table moved the bar from %.2f to %.2f", before, after)
	}
	if after != 0 {
		t.Errorf("no rows of the real work have been read, so the bar should be at 0, got %.2f", after)
	}

	// And the real work still takes the bar all the way to the end.
	runPlain.rows.Store(100)
	if got := fractionOf(t, tr); got != 1 {
		t.Errorf("with the unfiltered table fully read the bar should be at 1, got %.2f", got)
	}
}

func fractionOf(t *testing.T, tr *tracker) float64 {
	t.Helper()
	p, ok := tr.snapshot(2)
	if !ok {
		t.Fatal("expected a snapshot while a table is in flight")
	}
	f, ok := p.OverallFraction()
	if !ok {
		t.Fatal("expected a meaningful overall fraction")
	}
	return f
}
