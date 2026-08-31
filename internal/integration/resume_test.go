package integration

import (
	"archive/zip"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/JeePeeTee/dbdumper/internal/archive"
	"github.com/JeePeeTee/dbdumper/internal/export"
	"github.com/JeePeeTee/dbdumper/internal/spool"
	"github.com/JeePeeTee/dbdumper/internal/sqlsrv"
)

// TestResumeObeysTheSecondRunsExclusions pins down which run decides what an
// archive holds.
//
// Packaging reads from the work directory, and the work directory is whatever
// an earlier run left behind - an earlier run that may have been given entirely
// different options. If packaging walked the spool rather than the model, a
// resume asked to hold data back would ship it anyway, and the operator who
// passed --exclude-data to keep a table out of an archive would not find out.
// That is a disclosure bug, not a cosmetic one, so it is worth a live test.
func TestResumeObeysTheSecondRunsExclusions(t *testing.T) {
	// Both cases start from a work directory that really does hold rows, and
	// both assert on the finished archive rather than on any internal state.
	t.Run("exclude-data", func(t *testing.T) {
		src, out := interruptedExport(t)
		log := &logCapture{}
		if _, err := export.Run(context.Background(), src, export.Options{
			Out:              out,
			Resume:           true,
			ExcludeData:      []string{"*"},
			ProgressInterval: -1,
			Log:              log.record,
			Warn:             warnf(t),
		}); err != nil {
			t.Fatalf("resume: %v", err)
		}
		log.mustHaveResumed(t)
		assertNoDataEntries(t, out)

		ar, err := archive.Open(out)
		if err != nil {
			t.Fatal(err)
		}
		defer ar.Close()
		if len(ar.Manifest.Database.Tables) == 0 {
			t.Fatal("no tables in the manifest; the archive is not what this test assumes")
		}
		for _, tb := range ar.Manifest.Database.Tables {
			name := tb.Schema + "." + tb.Name
			if !tb.DataSkipped {
				t.Errorf("%s: DataSkipped = false, so a restore would not know its rows are missing", name)
			}
			if tb.RowCount != 0 {
				t.Errorf("%s: RowCount = %d, but no rows were packaged", name, tb.RowCount)
			}
			if got := tb.DataEntries(); len(got) != 0 {
				t.Errorf("%s: manifest still points at %v", name, got)
			}
		}
	})

	// --schema-only is the sharper case: it names no table, so nothing in the
	// options connects it to what the spool holds.
	t.Run("schema-only", func(t *testing.T) {
		src, out := interruptedExport(t)
		log := &logCapture{}
		if _, err := export.Run(context.Background(), src, export.Options{
			Out:              out,
			Resume:           true,
			SchemaOnly:       true,
			ProgressInterval: -1,
			Log:              log.record,
			Warn:             warnf(t),
		}); err != nil {
			t.Fatalf("resume: %v", err)
		}
		log.mustHaveResumed(t)
		assertNoDataEntries(t, out)
	})
}

// assertNoDataEntries fails if the archive carries any table data at all. It
// reads the zip directly rather than going through the manifest, because the
// point is what is in the file, not what the manifest admits to.
func assertNoDataEntries(t *testing.T, out string) {
	t.Helper()
	zr, err := zip.OpenReader(out)
	if err != nil {
		t.Fatalf("open %s: %v", out, err)
	}
	defer zr.Close()

	var leaked []string
	var bytes uint64
	for _, f := range zr.File {
		if strings.HasPrefix(f.Name, "data/") {
			leaked = append(leaked, f.Name)
			bytes += f.UncompressedSize64
		}
	}
	if len(leaked) > 0 {
		t.Errorf("archive holds %d data entr(ies) totalling %d bytes that this run excluded: %v",
			len(leaked), bytes, leaked)
	}
}

// logCapture keeps an export's permanent output so a test can assert on what
// the run reported about itself.
type logCapture struct {
	mu    sync.Mutex
	lines []string
}

func (l *logCapture) record(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

// mustHaveResumed guards against the test passing for the wrong reason. An
// archive with no data in it proves nothing if the run never saw the spooled
// data in the first place.
func (l *logCapture) mustHaveResumed(t *testing.T) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range l.lines {
		if strings.Contains(line, "resuming:") {
			return
		}
	}
	t.Fatalf("the run did not resume anything, so it was never at risk of packaging it:\n%s",
		strings.Join(l.lines, "\n"))
}

// interruptedExport builds the source database and leaves a work directory
// holding at least one fully spooled table, as an export stopped part way
// through would. It returns the source connection and the intended output path.
func interruptedExport(t *testing.T) (*sql.DB, string) {
	t.Helper()
	ctx := context.Background()
	base := baseConfig(t)

	master, err := sqlsrv.Open(ctx, base.WithDatabase("master"))
	if err != nil {
		t.Fatalf("connect to master: %v", err)
	}
	t.Cleanup(func() { master.Close() })

	recreate(ctx, t, master, srcDB)
	t.Cleanup(func() {
		if os.Getenv("DBDUMPER_TEST_KEEP") == "" {
			drop(ctx, master, srcDB)
		}
	})

	src, err := sqlsrv.Open(ctx, base.WithDatabase(srcDB))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { src.Close() })

	for _, stmt := range srcSchema {
		if _, err := src.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("schema setup failed:\n%s\n%v", firstLine(stmt), err)
		}
	}
	for _, stmt := range srcData {
		if _, err := src.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("data setup failed:\n%s\n%v", firstLine(stmt), err)
		}
	}

	out := filepath.Join(t.TempDir(), "partial.dbdump")

	// Cancel as soon as one table has been committed to the spool. One worker,
	// so the cancel lands between tables rather than in the middle of the only
	// one that finished.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu       sync.Mutex
		finished int
	)
	_, err = export.Run(runCtx, src, export.Options{
		Out:              out,
		Parallel:         1,
		ProgressInterval: -1,
		Warn:             warnf(t),
		Log: func(format string, args ...any) {
			// dumpTable logs one line per committed piece and nothing else
			// counts rows, so this fires exactly when a table lands.
			if !strings.Contains(fmt.Sprintf(format, args...), " rows ") {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if finished++; finished == 1 {
				cancel()
			}
		},
	})
	if err == nil {
		t.Fatal("the export was cancelled part way and should have failed")
	}
	mu.Lock()
	got := finished
	mu.Unlock()
	if got == 0 {
		t.Fatal("no table was spooled before the cancel; there is nothing for a resume to package")
	}
	if dir := spool.DirFor(out); !spool.Exists(dir) {
		t.Fatalf("the interrupted export left no work directory at %s", dir)
	}
	return src, out
}
