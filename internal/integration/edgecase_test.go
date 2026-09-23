package integration

import (
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
	"github.com/JeePeeTee/dbdumper/internal/importer"
	"github.com/JeePeeTee/dbdumper/internal/model"
	"github.com/JeePeeTee/dbdumper/internal/sqlsrv"
)

const (
	edgeSrcDB = "dbdumper_edge_src_test"
	edgeDstDB = "dbdumper_edge_dst_test"
)

// edgeDB creates an empty database, optionally with its own collation, runs
// setup in it, and drops it again when the test ends.
func edgeDB(t *testing.T, name, collation string, setup ...string) *sql.DB {
	t.Helper()
	ctx := context.Background()
	base := baseConfig(t)

	master, err := sqlsrv.Open(ctx, base.WithDatabase("master"))
	if err != nil {
		t.Fatalf("connect to master: %v", err)
	}
	t.Cleanup(func() { master.Close() })

	drop(ctx, master, name)
	create := "CREATE DATABASE " + model.Quote(name)
	if collation != "" {
		create += " COLLATE " + collation
	}
	if _, err := master.ExecContext(ctx, create); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	t.Cleanup(func() {
		if os.Getenv("DBDUMPER_TEST_KEEP") == "" {
			drop(ctx, master, name)
		}
	})

	db, err := sqlsrv.Open(ctx, base.WithDatabase(name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, stmt := range setup {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("setup failed:\n%s\n%v", firstLine(stmt), err)
		}
	}
	return db
}

// exportTo runs a plain export of db and opens the result.
func exportTo(t *testing.T, db *sql.DB, opts export.Options) *archive.Reader {
	t.Helper()
	if opts.Out == "" {
		opts.Out = filepath.Join(t.TempDir(), "edge.dbdump")
	}
	if opts.ProgressInterval == 0 {
		opts.ProgressInterval = -1
	}
	if opts.Warn == nil {
		opts.Warn = warnf(t)
	}
	if _, err := export.Run(context.Background(), db, opts); err != nil {
		t.Fatalf("export: %v", err)
	}
	ar, err := archive.Open(opts.Out)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ar.Close() })
	return ar
}

// TestBinaryChunkKeySurvivesAResume splits a table on a binary(16) key,
// interrupts the export after the first range lands, and resumes it.
//
// The boundaries used to be passed as their base64 text on the first run and
// as bytes after a resume, so the two runs divided the table on different
// orderings - text collation, then byte order - and rows fell between the
// ranges or into two of them.
func TestBinaryChunkKeySurvivesAResume(t *testing.T) {
	ctx := context.Background()
	const rows = 20000
	src := edgeDB(t, edgeSrcDB, "",
		`CREATE TABLE dbo.Blob (
			Hash binary(16) NOT NULL CONSTRAINT PK_Blob PRIMARY KEY CLUSTERED,
			N int NOT NULL,
			Pad char(200) NOT NULL DEFAULT 'x')`,
		fmt.Sprintf(`WITH n AS (
			SELECT TOP (%d) ROW_NUMBER() OVER (ORDER BY (SELECT NULL)) AS i
			FROM sys.all_objects a CROSS JOIN sys.all_objects b)
		INSERT dbo.Blob (Hash, N) SELECT HASHBYTES('MD5', CONVERT(varbinary(8), i)), i FROM n`, rows),
	)

	out := filepath.Join(t.TempDir(), "blob.dbdump")
	opts := export.Options{
		Out:              out,
		Parallel:         2,
		ChunkMinBytes:    1, // split whatever its size
		ProgressInterval: -1,
		Warn:             warnf(t),
	}

	// First run: stop as soon as one range has been committed.
	runCtx, cancel := context.WithCancel(ctx)
	var once sync.Once
	first := opts
	first.Log = func(format string, args ...any) {
		if strings.Contains(fmt.Sprintf(format, args...), " rows ") {
			once.Do(cancel)
		}
	}
	if _, err := export.Run(runCtx, src, first); err == nil {
		t.Fatal("the export was cancelled part way and should have failed")
	}
	cancel()

	log := &logCapture{}
	second := opts
	second.Resume = true
	second.Log = log.record
	if _, err := export.Run(ctx, src, second); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if !strings.Contains(log.text(), "resuming:") {
		t.Fatalf("the second run did not resume anything, so the test proves nothing:\n%s", log.text())
	}

	ar, err := archive.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer ar.Close()
	tb := ar.Manifest.Database.Tables[0]
	if len(tb.DataFiles) < 2 {
		t.Fatalf("the table was not split (%v); the test proves nothing", tb.DataEntries())
	}

	seen := map[string]int{}
	largest := 0
	for _, entry := range tb.DataEntries() {
		lines := readLines(t, ar, entry)
		if len(lines) > largest {
			largest = len(lines)
		}
		for _, l := range lines {
			seen[l]++
		}
	}
	dupes := 0
	for _, n := range seen {
		if n > 1 {
			dupes++
		}
	}
	if len(seen) != rows || dupes != 0 {
		t.Errorf("archive holds %d distinct rows (%d repeated), want exactly %d", len(seen), dupes, rows)
	}
	// Boundaries compared in byte order divide hashes evenly. Compared as text
	// they fell almost anywhere, and one range took nearly the whole table.
	if largest > rows*3/4 {
		t.Errorf("one range holds %d of %d rows; the boundaries are not dividing on byte order", largest, rows)
	}
}

// TestEncryptedModulesAreSkipped - sys.sql_modules hides the text of a module
// created WITH ENCRYPTION behind a NULL, which used to fail the whole export.
// The module cannot be carried, so it is left out, and so is what is built on
// it, and the rest restores.
func TestEncryptedModulesAreSkipped(t *testing.T) {
	ctx := context.Background()
	src := edgeDB(t, edgeSrcDB, "",
		`CREATE TABLE dbo.T (Id int NOT NULL PRIMARY KEY)`,
		`INSERT dbo.T VALUES (1), (2)`,
		`CREATE VIEW dbo.vSecret WITH ENCRYPTION AS SELECT Id FROM dbo.T`,
		`CREATE VIEW dbo.vOnSecret AS SELECT Id FROM dbo.vSecret`,
		`CREATE VIEW dbo.vPlain AS SELECT Id FROM dbo.T`,
	)

	warnings := &logCapture{}
	ar := exportTo(t, src, export.Options{Warn: warnings.record})

	if !strings.Contains(warnings.text(), "vSecret") || !strings.Contains(warnings.text(), "WITH ENCRYPTION") {
		t.Errorf("the encrypted view should be reported:\n%s", warnings.text())
	}
	var names []string
	for _, m := range ar.Manifest.Database.Modules {
		names = append(names, m.Name)
	}
	if strings.Join(names, ",") != "vPlain" {
		t.Errorf("modules in the archive = %v, want only vPlain", names)
	}

	dst := edgeDB(t, edgeDstDB, "")
	res, err := importer.Run(ctx, dst, ar, importer.Options{Log: func(string, ...any) {}, Warn: warnf(t)})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if res.Failed > 0 {
		t.Errorf("restore reported failures: %v", res.FailedList)
	}
}

// TestDisabledUniqueIndexRestores - a unique index switched off in the source
// over rows that no longer satisfy it. Built after the load, as it used to be,
// it failed on the duplicates and aborted the restore; it has to come back as
// it was, disabled.
func TestDisabledUniqueIndexRestores(t *testing.T) {
	ctx := context.Background()
	src := edgeDB(t, edgeSrcDB, "",
		`CREATE TABLE dbo.T (Id int NOT NULL PRIMARY KEY, Code int NOT NULL)`,
		`CREATE UNIQUE INDEX UX_T_Code ON dbo.T (Code)`,
		`ALTER INDEX UX_T_Code ON dbo.T DISABLE`,
		`INSERT dbo.T VALUES (1, 7), (2, 7)`,
	)
	ar := exportTo(t, src, export.Options{})

	dst := edgeDB(t, edgeDstDB, "")
	if _, err := importer.Run(ctx, dst, ar, importer.Options{Log: func(string, ...any) {}, Warn: warnf(t)}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	var disabled bool
	if err := dst.QueryRowContext(ctx,
		`SELECT is_disabled FROM sys.indexes WHERE name = 'UX_T_Code'`).Scan(&disabled); err != nil {
		t.Fatalf("the index was not restored: %v", err)
	}
	if !disabled {
		t.Error("the index should come back disabled")
	}
	if n := count(ctx, t, dst, "dbo.T"); n != 2 {
		t.Errorf("restored %d rows, want 2", n)
	}
}

// TestDataOnlyRestoresEachConstraintAsItWas loads rows that break one foreign
// key into a schema whose constraints are otherwise in order.
//
// Re-enabling with WITH CHECK CHECK CONSTRAINT ALL failed on that one key and
// left every constraint on the table switched off; it also switched on what
// the target had deliberately switched off. Each is now put back as it stood.
func TestDataOnlyRestoresEachConstraintAsItWas(t *testing.T) {
	ctx := context.Background()
	src := edgeDB(t, edgeSrcDB, "",
		`CREATE TABLE dbo.Parent (Id int NOT NULL PRIMARY KEY)`,
		`CREATE TABLE dbo.Child (
			Id int NOT NULL PRIMARY KEY,
			ParentId int NOT NULL CONSTRAINT FK_Child_Parent REFERENCES dbo.Parent (Id),
			Qty int NOT NULL CONSTRAINT CK_Child_Qty CHECK (Qty > 0),
			Note nvarchar(20) NULL CONSTRAINT CK_Child_Note CHECK (Note <> N''))`,
		`CREATE TRIGGER dbo.trOn ON dbo.Child AFTER INSERT AS SET NOCOUNT ON`,
		`CREATE TRIGGER dbo.trOff ON dbo.Child AFTER INSERT AS SET NOCOUNT ON`,
		`DISABLE TRIGGER dbo.trOff ON dbo.Child`,
		`INSERT dbo.Parent VALUES (1), (2)`,
		`INSERT dbo.Child VALUES (10, 1, 5, NULL), (20, 2, 5, NULL)`,
	)
	// Parent 2 is filtered out, so child 20 arrives dangling.
	ar := exportTo(t, src, export.Options{Where: []string{"dbo.Parent:Id = 1"}})

	dst := edgeDB(t, edgeDstDB, "")
	quiet := importer.Options{Log: func(string, ...any) {}, Warn: warnf(t)}
	schemaOnly := quiet
	schemaOnly.SchemaOnly = true
	if _, err := importer.Run(ctx, dst, ar, schemaOnly); err != nil {
		t.Fatalf("schema-only restore: %v", err)
	}
	// The target as someone might keep it: the key trusted (the tables are
	// empty, so it validates), one check deliberately switched off.
	for _, q := range []string{
		`ALTER TABLE dbo.Child WITH CHECK CHECK CONSTRAINT FK_Child_Parent`,
		`ALTER TABLE dbo.Child NOCHECK CONSTRAINT CK_Child_Note`,
	} {
		if _, err := dst.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	warnings := &logCapture{}
	dataOnly := quiet
	dataOnly.DataOnly = true
	dataOnly.Warn = warnings.record
	if _, err := importer.Run(ctx, dst, ar, dataOnly); err != nil {
		t.Fatalf("data-only restore: %v", err)
	}

	state := func(name string) (disabled, untrusted bool) {
		t.Helper()
		err := dst.QueryRowContext(ctx, `
SELECT is_disabled, is_not_trusted FROM sys.foreign_keys WHERE name = @p1
UNION ALL
SELECT is_disabled, is_not_trusted FROM sys.check_constraints WHERE name = @p1`, name).Scan(&disabled, &untrusted)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return
	}
	if d, u := state("FK_Child_Parent"); d || !u {
		t.Errorf("FK_Child_Parent: disabled=%v untrusted=%v, want enabled but untrusted", d, u)
	}
	if d, u := state("CK_Child_Qty"); d || u {
		t.Errorf("CK_Child_Qty: disabled=%v untrusted=%v, want enabled and trusted", d, u)
	}
	if d, _ := state("CK_Child_Note"); !d {
		t.Error("CK_Child_Note was switched off in the target and should have stayed off")
	}
	if !strings.Contains(warnings.text(), "FK_Child_Parent") {
		t.Errorf("the key left untrusted should be reported:\n%s", warnings.text())
	}

	triggers := map[string]bool{}
	rows, err := dst.QueryContext(ctx, `SELECT name, is_disabled FROM sys.triggers`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var disabled bool
		if err := rows.Scan(&name, &disabled); err != nil {
			t.Fatal(err)
		}
		triggers[name] = disabled
	}
	if triggers["trOn"] || !triggers["trOff"] {
		t.Errorf("trigger state after the load = %v, want trOn enabled and trOff disabled", triggers)
	}
}

// TestCaseSensitiveTablesKeepTheirOwnRows - a case-sensitive database can hold
// dbo.Item and dbo.item. The work directory used to fold them into one spool
// file, which two workers then wrote at once.
func TestCaseSensitiveTablesKeepTheirOwnRows(t *testing.T) {
	ctx := context.Background()
	src := edgeDB(t, edgeSrcDB, "Latin1_General_100_CS_AS",
		`CREATE TABLE dbo.Item (Id int NOT NULL PRIMARY KEY, Who nvarchar(10) NOT NULL)`,
		`CREATE TABLE dbo.item (Id int NOT NULL PRIMARY KEY, Who nvarchar(10) NOT NULL)`,
		`INSERT dbo.Item VALUES (1, N'upper')`,
		`INSERT dbo.item VALUES (1, N'lower'), (2, N'lower')`,
	)
	ar := exportTo(t, src, export.Options{Parallel: 2})

	dst := edgeDB(t, edgeDstDB, "Latin1_General_100_CS_AS")
	if _, err := importer.Run(ctx, dst, ar, importer.Options{Log: func(string, ...any) {}, Warn: warnf(t)}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	for table, want := range map[string]string{"dbo.Item": "upper", "dbo.item": "lower"} {
		var n int
		var who string
		if err := dst.QueryRowContext(ctx,
			"SELECT COUNT(*), MIN(Who) FROM "+table).Scan(&n, &who); err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		wantN := map[string]int{"upper": 1, "lower": 2}[want]
		if n != wantN || who != want {
			t.Errorf("%s holds %d row(s) of %q, want %d of %q", table, n, who, wantN, want)
		}
	}
}
