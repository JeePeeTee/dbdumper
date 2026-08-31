package integration

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JeePeeTee/dbdumper/internal/archive"
	"github.com/JeePeeTee/dbdumper/internal/export"
	"github.com/JeePeeTee/dbdumper/internal/importer"
	"github.com/JeePeeTee/dbdumper/internal/sqlsrv"
)

// TestImportExcludeStillRestores - leaving a table out of a restore must not
// take the restore down with it.
//
// --exclude skips a table's rows but still creates the table, so a foreign key
// pointing at it has nothing to resolve against. Added validated, it fails:
//
//	The ALTER TABLE statement conflicted with the FOREIGN KEY constraint
//	"FK_OrderLine_Customer" ... (547)
//
// and the whole restore stops over a table the caller deliberately left out.
func TestImportExcludeStillRestores(t *testing.T) {
	ctx := context.Background()
	dst, warnings := restoreExcluding(t, "sales.Customer")

	// The excluded table is present and empty; the table that references it
	// still holds its rows.
	if got := count(ctx, t, dst, "sales.Customer"); got != 0 {
		t.Errorf("sales.Customer was excluded but holds %d rows", got)
	}
	if got := count(ctx, t, dst, "sales.[Order Line]"); got == 0 {
		t.Error("sales.[Order Line] was not excluded and should have been loaded")
	}

	// The foreign key exists, and is marked untrusted rather than silently
	// dropped: the database itself now records that it was not verified.
	var notTrusted bool
	err := dst.QueryRowContext(ctx,
		"SELECT is_not_trusted FROM sys.foreign_keys WHERE name = 'FK_OrderLine_Customer'").Scan(&notTrusted)
	if err == sql.ErrNoRows {
		t.Fatal("FK_OrderLine_Customer was not created at all")
	}
	if err != nil {
		t.Fatal(err)
	}
	if !notTrusted {
		t.Error("FK_OrderLine_Customer is marked trusted, but it was never validated")
	}

	// And the caller was told, because nothing about the restored database
	// says so afterwards except that flag.
	if !strings.Contains(warnings, "WITH NOCHECK") ||
		!strings.Contains(warnings, "FK_OrderLine_Customer") {
		t.Errorf("the run should have warned about the untrusted key, got:\n%s", warnings)
	}
}

// TestImportWithoutFiltersTrustsItsForeignKeys is the control: the same
// database restored whole must leave every key validated.
func TestImportWithoutFiltersTrustsItsForeignKeys(t *testing.T) {
	ctx := context.Background()
	dst, warnings := restoreExcluding(t)

	rows, err := dst.QueryContext(ctx,
		"SELECT name FROM sys.foreign_keys WHERE is_not_trusted = 1")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var untrusted []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		untrusted = append(untrusted, n)
	}
	if len(untrusted) > 0 {
		t.Errorf("a complete restore should trust every foreign key; these are not: %v", untrusted)
	}
	if strings.Contains(warnings, "WITH NOCHECK") {
		t.Errorf("nothing was excluded, so there was nothing to warn about:\n%s", warnings)
	}
}

// restoreExcluding dumps the test database and restores it while leaving out
// the named tables, returning the restored connection and anything warned.
func restoreExcluding(t *testing.T, exclude ...string) (*sql.DB, string) {
	t.Helper()
	ctx := context.Background()
	base := baseConfig(t)

	master, err := sqlsrv.Open(ctx, base.WithDatabase("master"))
	if err != nil {
		t.Fatalf("connect to master: %v", err)
	}
	t.Cleanup(func() { master.Close() })

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

	out := filepath.Join(t.TempDir(), "filtered.dbdump")
	if _, err := export.Run(ctx, src, export.Options{
		Out: out, ProgressInterval: -1, Warn: warnf(t),
	}); err != nil {
		t.Fatalf("export: %v", err)
	}
	ar, err := archive.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ar.Close() })

	dst, err := sqlsrv.Open(ctx, base.WithDatabase(dstDB))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dst.Close() })

	warnings := &logCapture{}
	res, err := importer.Run(ctx, dst, ar, importer.Options{
		Exclude: exclude,
		Log:     func(string, ...any) {},
		Warn:    warnings.record,
	})
	if err != nil {
		t.Fatalf("restore while excluding %v failed: %v", exclude, err)
	}
	if res.Failed > 0 {
		t.Fatalf("restore reported %d failures: %v", res.Failed, res.FailedList)
	}
	return dst, warnings.text()
}

func count(ctx context.Context, t *testing.T, db *sql.DB, table string) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}
