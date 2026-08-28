// Package integration round-trips a purpose-built database through export and
// import and checks that what comes out the other side matches.
//
// Set DBDUMPER_TEST_DSN to a server you may create databases on:
//
//	DBDUMPER_TEST_DSN='sqlserver://host/instance?trusted_connection=true&encrypt=disable&protocol=lpc'
package integration

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/JeePeeTee/dbdumper/internal/archive"
	"github.com/JeePeeTee/dbdumper/internal/export"
	"github.com/JeePeeTee/dbdumper/internal/importer"
	"github.com/JeePeeTee/dbdumper/internal/model"
	"github.com/JeePeeTee/dbdumper/internal/plan"
	"github.com/JeePeeTee/dbdumper/internal/sqlsrv"
)

const (
	srcDB = "dbdumper_src_test"
	dstDB = "dbdumper_dst_test"
)

func baseConfig(t *testing.T) sqlsrv.ConnConfig {
	t.Helper()
	dsn := os.Getenv("DBDUMPER_TEST_DSN")
	if dsn == "" {
		t.Skip("set DBDUMPER_TEST_DSN to run integration tests")
	}
	return sqlsrv.ConnConfig{DSN: dsn}
}

// TestBulkSafeClassification pins down which of the test tables take the bulk
// path, so a change in either direction is deliberate rather than accidental.
func TestBulkSafeClassification(t *testing.T) {
	cases := []struct {
		name string
		cols []model.Column
		want bool
	}{
		{"plain", []model.Column{{TypeName: "int"}, {TypeName: "nvarchar"}}, true},
		{"guid and money", []model.Column{{TypeName: "uniqueidentifier"}, {TypeName: "money"}}, true},
		{"all date kinds", []model.Column{
			{TypeName: "date"}, {TypeName: "time"}, {TypeName: "datetime"},
			{TypeName: "smalldatetime"}, {TypeName: "datetime2"}, {TypeName: "datetimeoffset"},
		}, true},
		{"xml", []model.Column{{TypeName: "int"}, {TypeName: "xml"}}, false},
		{"geography", []model.Column{{TypeName: "geography"}}, false},
		{"hierarchyid", []model.Column{{TypeName: "hierarchyid"}}, false},
		{"sql_variant", []model.Column{{TypeName: "sql_variant"}}, false},
		{"alias over a safe type", []model.Column{
			{TypeSchema: "dbo", TypeName: "PhoneNumber", BaseTypeName: "nvarchar"}}, true},
		{"alias over xml", []model.Column{
			{TypeSchema: "dbo", TypeName: "Doc", BaseTypeName: "xml"}}, false},
	}
	for _, c := range cases {
		if got := sqlsrv.BulkSafe(c.cols); got != c.want {
			t.Errorf("%s: BulkSafe = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	t.Run("bulk", func(t *testing.T) { runRoundTrip(t, false) })
	t.Run("insert-only", func(t *testing.T) { runRoundTrip(t, true) })
}

func runRoundTrip(t *testing.T, noBulk bool) {
	ctx := context.Background()
	base := baseConfig(t)

	master, err := sqlsrv.Open(ctx, base.WithDatabase("master"))
	if err != nil {
		t.Fatalf("connect to master: %v", err)
	}
	defer master.Close()

	recreate(ctx, t, master, srcDB)
	recreate(ctx, t, master, dstDB)
	t.Cleanup(func() {
		if os.Getenv("DBDUMPER_TEST_KEEP") == "" {
			drop(ctx, master, srcDB)
			drop(ctx, master, dstDB)
		}
	})

	src, err := sqlsrv.Open(ctx, base.WithDatabase(srcDB))
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()

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

	dir := t.TempDir()
	archiveA := filepath.Join(dir, "a.dbdump")

	resA, err := export.Run(ctx, src, export.Options{Out: archiveA, Warn: warnf(t)})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	t.Logf("exported %d tables, %d rows", resA.Tables, resA.Rows)

	arA, err := archive.Open(archiveA)
	if err != nil {
		t.Fatal(err)
	}
	defer arA.Close()

	// Guard against the round trip passing vacuously because every table fell
	// back to the INSERT path.
	assertBulkSafe(t, arA.Manifest.Database.Tables, map[string]bool{
		"dbo.BulkTypes":                true,
		"dbo.AllTypes":                 false, // xml, geography, geometry, hierarchyid
		"sales.Customer":               true,
		"sales.Order Line":             true,
		"odd name.Weird \"\"Table\"\"": true,
	})

	dst, err := sqlsrv.Open(ctx, base.WithDatabase(dstDB))
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()

	resI, err := importer.Run(ctx, dst, arA, importer.Options{NoBulk: noBulk, Warn: warnf(t)})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if resI.Failed > 0 {
		t.Fatalf("import reported %d failures: %v", resI.Failed, resI.FailedList)
	}
	if resI.Rows != resA.Rows {
		t.Errorf("row count: exported %d, imported %d", resA.Rows, resI.Rows)
	}

	// Dump the restored database and compare the two archives. Anything the
	// round trip changed - a type, a default, a value, an index option - shows
	// up as a difference here.
	archiveB := filepath.Join(dir, "b.dbdump")
	if _, err := export.Run(ctx, dst, export.Options{Out: archiveB, Warn: warnf(t)}); err != nil {
		t.Fatalf("re-export: %v", err)
	}
	arB, err := archive.Open(archiveB)
	if err != nil {
		t.Fatal(err)
	}
	defer arB.Close()

	compareSchema(t, &arA.Manifest.Database, &arB.Manifest.Database)
	compareData(t, arA, arB)
}

// assertBulkSafe checks how the importer will classify each table, using the
// column definitions as actually read back from the server.
func assertBulkSafe(t *testing.T, tables []model.Table, want map[string]bool) {
	t.Helper()
	seen := map[string]bool{}
	for _, tb := range tables {
		name := tb.Schema + "." + tb.Name
		expected, ok := want[name]
		if !ok {
			t.Errorf("unexpected table %q in archive; update the expectations", name)
			continue
		}
		seen[name] = true

		cols := make([]model.Column, 0, len(tb.DataColumns))
		for _, c := range tb.Columns {
			if c.Insertable() {
				cols = append(cols, c)
			}
		}
		if got := sqlsrv.BulkSafe(cols); got != expected {
			t.Errorf("%s: BulkSafe = %v, want %v", name, got, expected)
		}
	}
	for name := range want {
		if !seen[name] {
			t.Errorf("expected table %q was not in the archive", name)
		}
	}
}

func compareSchema(t *testing.T, a, b *model.Database) {
	t.Helper()
	phasesA, phasesB := plan.AllPhases(a), plan.AllPhases(b)
	if len(phasesA) != len(phasesB) {
		t.Fatalf("phase count differs: %d vs %d", len(phasesA), len(phasesB))
	}
	for i := range phasesA {
		gotA := plan.ScriptFor(phasesA[i])
		gotB := plan.ScriptFor(phasesB[i])
		if gotA == gotB {
			continue
		}
		t.Errorf("phase %q differs after round trip:\n%s", phasesA[i].Name, diffLines(gotA, gotB))
	}
}

func compareData(t *testing.T, a, b *archive.Reader) {
	t.Helper()
	byName := map[string]model.Table{}
	for _, tb := range b.Manifest.Database.Tables {
		byName[tb.Schema+"."+tb.Name] = tb
	}

	for _, ta := range a.Manifest.Database.Tables {
		name := ta.Schema + "." + ta.Name
		tb, ok := byName[name]
		if !ok {
			t.Errorf("%s missing from restored database", name)
			continue
		}
		if ta.RowCount != tb.RowCount {
			t.Errorf("%s: %d rows exported, %d rows restored", name, ta.RowCount, tb.RowCount)
		}
		if ta.DataFile == "" {
			continue
		}
		linesA := readLines(t, a, ta.DataFile)
		linesB := readLines(t, b, tb.DataFile)
		// Row order is not guaranteed by an unordered SELECT, so compare as
		// multisets.
		sort.Strings(linesA)
		sort.Strings(linesB)
		if len(linesA) != len(linesB) {
			t.Errorf("%s: %d data lines vs %d", name, len(linesA), len(linesB))
			continue
		}
		for i := range linesA {
			if linesA[i] != linesB[i] {
				t.Errorf("%s: row differs after round trip\n  source:   %s\n  restored: %s",
					name, truncate(linesA[i]), truncate(linesB[i]))
				break
			}
		}
	}
}

func readLines(t *testing.T, ar *archive.Reader, entry string) []string {
	t.Helper()
	rc, err := ar.OpenEntry(entry)
	if err != nil {
		t.Fatalf("open %s: %v", entry, err)
	}
	defer rc.Close()
	var sb strings.Builder
	buf := make([]byte, 32*1024)
	for {
		n, err := rc.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	lines := strings.Split(strings.TrimRight(sb.String(), "\n"), "\n")
	if len(lines) > 0 {
		lines = lines[1:] // drop the header
	}
	return lines
}

func recreate(ctx context.Context, t *testing.T, master *sql.DB, name string) {
	t.Helper()
	drop(ctx, master, name)
	if _, err := master.ExecContext(ctx, "CREATE DATABASE "+model.Quote(name)); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
}

func drop(ctx context.Context, master *sql.DB, name string) {
	master.ExecContext(ctx, fmt.Sprintf(
		"IF DB_ID(%s) IS NOT NULL BEGIN ALTER DATABASE %s SET SINGLE_USER WITH ROLLBACK IMMEDIATE; DROP DATABASE %s; END",
		model.QuoteString(name), model.Quote(name), model.Quote(name)))
}

func warnf(t *testing.T) func(string, ...any) {
	return func(format string, args ...any) { t.Logf("warn: "+format, args...) }
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " ..."
	}
	return s
}

func truncate(s string) string {
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}

func diffLines(a, b string) string {
	la, lb := strings.Split(a, "\n"), strings.Split(b, "\n")
	var out strings.Builder
	n := len(la)
	if len(lb) > n {
		n = len(lb)
	}
	shown := 0
	for i := 0; i < n && shown < 20; i++ {
		var x, y string
		if i < len(la) {
			x = la[i]
		}
		if i < len(lb) {
			y = lb[i]
		}
		if x != y {
			fmt.Fprintf(&out, "  - %s\n  + %s\n", x, y)
			shown++
		}
	}
	return out.String()
}
