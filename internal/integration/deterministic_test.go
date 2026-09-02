package integration

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JeePeeTee/dbdumper/internal/archive"
	"github.com/JeePeeTee/dbdumper/internal/export"
	"github.com/JeePeeTee/dbdumper/internal/model"
	"github.com/JeePeeTee/dbdumper/internal/sqlsrv"
)

// TestDeterministicExportIsByteIdentical is the whole point of the flag: two
// exports of a database nobody has touched must produce the same bytes.
//
// It is worth testing at the file level rather than entry by entry, because
// the ways this breaks are mostly outside the data - a timestamp in the
// manifest, a modification time in a zip header, entries in a different order.
// Comparing whole files catches all of those without having to guess which one
// went wrong first.
func TestDeterministicExportIsByteIdentical(t *testing.T) {
	ctx := context.Background()
	src := populatedSource(t)
	dir := t.TempDir()

	run := func(name string, opts export.Options) string {
		t.Helper()
		opts.Out = filepath.Join(dir, name)
		opts.ProgressInterval = -1
		opts.Warn = warnf(t)
		if _, err := export.Run(ctx, src, opts); err != nil {
			t.Fatalf("export %s: %v", name, err)
		}
		return opts.Out
	}

	// Parallelism is left on deliberately: it must not affect the bytes, and a
	// test that turned it off would not show that.
	a := run("a.dbdump", export.Options{Deterministic: true, Parallel: 4})

	// A zip header stores modification time as DOS time, which has two-second
	// resolution. Two exports run back to back land in the same bucket, so a
	// timestamp being written would go unnoticed. Waiting past the boundary is
	// what makes this assertion mean anything.
	time.Sleep(3 * time.Second)

	b := run("b.dbdump", export.Options{Deterministic: true, Parallel: 4})

	ba, err := os.ReadFile(a)
	if err != nil {
		t.Fatal(err)
	}
	bb, err := os.ReadFile(b)
	if err != nil {
		t.Fatal(err)
	}

	if len(ba) != len(bb) {
		t.Fatalf("archives differ in size: %d and %d bytes", len(ba), len(bb))
	}
	if !bytes.Equal(ba, bb) {
		at := firstDifference(ba, bb)
		t.Errorf("archives are the same size but differ, first at byte %d", at)
	}
}

// TestDeterministicExportOrdersRowsByPrimaryKey checks the ordering directly,
// on a table built so that key order and natural order disagree.
//
// Every table in the standard fixture has a clustered primary key, and a scan
// of one of those comes back in key order whether an ORDER BY was asked for or
// not - so asserting "ascending" against them passes even with the ordering
// removed, and proves nothing. A heap with a nonclustered key does not have
// that property: rows come back in the order they were inserted, which here is
// deliberately descending.
func TestDeterministicExportOrdersRowsByPrimaryKey(t *testing.T) {
	ctx := context.Background()
	src := populatedSource(t)

	if _, err := src.ExecContext(ctx, `CREATE TABLE dbo.Unsorted (
		Id int NOT NULL CONSTRAINT PK_Unsorted PRIMARY KEY NONCLUSTERED,
		Note nvarchar(50) NOT NULL)`); err != nil {
		t.Fatalf("create the heap: %v", err)
	}
	// Inserted high to low, so a heap scan returns them that way round.
	for _, id := range []int{9, 7, 5, 3, 1} {
		if _, err := src.ExecContext(ctx,
			"INSERT INTO dbo.Unsorted (Id, Note) VALUES (@p1, @p2)", id, "row"); err != nil {
			t.Fatalf("insert %d: %v", id, err)
		}
	}

	out := filepath.Join(t.TempDir(), "ordered.dbdump")
	if _, err := export.Run(ctx, src, export.Options{
		Out: out, Deterministic: true, Parallel: 4, ProgressInterval: -1, Warn: warnf(t),
	}); err != nil {
		t.Fatalf("export: %v", err)
	}

	ar, err := archive.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer ar.Close()

	var table *model.Table
	for i := range ar.Manifest.Database.Tables {
		tb := &ar.Manifest.Database.Tables[i]
		if tb.Schema == "dbo" && tb.Name == "Unsorted" {
			table = tb
		}
	}
	if table == nil {
		t.Fatal("dbo.Unsorted is missing from the archive")
	}

	header, rows := dataEntry(t, ar, table.DataFile)
	key := -1
	for i, name := range header.Columns {
		if strings.EqualFold(name, "Id") {
			key = i
		}
	}
	if key < 0 {
		t.Fatalf("Id is not among the exported columns: %v", header.Columns)
	}
	if len(rows) != 5 {
		t.Fatalf("expected the 5 rows that were inserted, got %d", len(rows))
	}

	got := make([]int, len(rows))
	for i, row := range rows {
		n, ok := row[key].(float64)
		if !ok {
			t.Fatalf("row %d: Id came back as %T, not a number", i, row[key])
		}
		got[i] = int(n)
	}
	want := []int{1, 3, 5, 7, 9}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("rows are not in primary key order: got %v, want %v", got, want)
			break
		}
	}
}

// dataEntry reads a JSONL entry as its header plus decoded rows.
func dataEntry(t *testing.T, ar *archive.Reader, entry string) (struct {
	Table   string   `json:"table"`
	Columns []string `json:"columns"`
}, [][]any) {
	t.Helper()
	rc, err := ar.OpenEntry(entry)
	if err != nil {
		t.Fatalf("open %s: %v", entry, err)
	}
	defer rc.Close()

	dec := json.NewDecoder(rc)
	var header struct {
		Table   string   `json:"table"`
		Columns []string `json:"columns"`
	}
	if err := dec.Decode(&header); err != nil {
		t.Fatalf("read the header of %s: %v", entry, err)
	}
	var rows [][]any
	for dec.More() {
		var row []any
		if err := dec.Decode(&row); err != nil {
			t.Fatalf("read a row of %s: %v", entry, err)
		}
		rows = append(rows, row)
	}
	return header, rows
}

// TestExportWithoutDeterministicStillWorks guards the default path, since the
// flag changes how every table is read.
func TestExportWithoutDeterministicStillWorks(t *testing.T) {
	ctx := context.Background()
	src := populatedSource(t)
	out := filepath.Join(t.TempDir(), "plain.dbdump")

	res, err := export.Run(ctx, src, export.Options{
		Out: out, Parallel: 4, ProgressInterval: -1, Warn: warnf(t),
	})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if res.Rows == 0 {
		t.Error("the source has rows, so the export should have found some")
	}
}

func firstDifference(a, b []byte) int {
	for i := range a {
		if a[i] != b[i] {
			return i
		}
	}
	return len(a)
}

// populatedSource builds the standard test database and hands back a
// connection to it.
func populatedSource(t *testing.T) *sql.DB {
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

	db, err := sqlsrv.Open(ctx, base.WithDatabase(srcDB))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	for _, stmt := range srcSchema {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("schema setup failed:\n%s\n%v", firstLine(stmt), err)
		}
	}
	for _, stmt := range srcData {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("data setup failed:\n%s\n%v", firstLine(stmt), err)
		}
	}
	return db
}
