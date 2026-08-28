package sqlsrv

import (
	"context"
	"os"
	"testing"

	mssql "github.com/microsoft/go-mssqldb"
)

// TestBulkCopyKeepsIdentity checks whether go-mssqldb's bulk copy preserves
// explicit identity values when SET IDENTITY_INSERT is on for the session.
// The driver exposes no KEEPIDENTITY option, so this is the only way to know.
func TestBulkCopyKeepsIdentity(t *testing.T) {
	dsn := os.Getenv("DBDUMPER_TEST_DSN")
	if dsn == "" {
		t.Skip("set DBDUMPER_TEST_DSN to run")
	}
	ctx := context.Background()
	db, err := Open(ctx, ConnConfig{DSN: dsn}.WithDatabase("tempdb"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	conn.ExecContext(ctx, "DROP TABLE IF EXISTS dbo.dbdumper_bulk_probe")
	if _, err := conn.ExecContext(ctx,
		"CREATE TABLE dbo.dbdumper_bulk_probe (Id int IDENTITY(1,1) NOT NULL PRIMARY KEY, V nvarchar(20) NULL)"); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(ctx, "DROP TABLE IF EXISTS dbo.dbdumper_bulk_probe")

	if _, err := conn.ExecContext(ctx, "SET IDENTITY_INSERT dbo.dbdumper_bulk_probe ON"); err != nil {
		t.Fatal(err)
	}

	err = func() error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()

		stmt, err := tx.PrepareContext(ctx,
			mssql.CopyIn("dbo.dbdumper_bulk_probe", mssql.BulkOptions{}, "Id", "V"))
		if err != nil {
			return err
		}
		for _, id := range []int64{100, 200, 300} {
			if _, err := stmt.ExecContext(ctx, id, "x"); err != nil {
				return err
			}
		}
		if _, err := stmt.ExecContext(ctx); err != nil {
			return err
		}
		if err := stmt.Close(); err != nil {
			return err
		}
		return tx.Commit()
	}()
	if err != nil {
		t.Logf("bulk copy with IDENTITY_INSERT ON failed: %v", err)
		t.Log("=> bulk copy cannot be used for tables with identity columns")
		return
	}

	rows, err := conn.QueryContext(ctx, "SELECT Id FROM dbo.dbdumper_bulk_probe ORDER BY Id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		got = append(got, id)
	}
	t.Logf("ids after bulk copy: %v (wanted [100 200 300])", got)
}
