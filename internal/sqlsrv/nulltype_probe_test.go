package sqlsrv

import (
	"context"
	"os"
	"testing"
)

// TestNullParamTyping probes which column types reject the driver's untyped
// NULL parameter. Skipped unless DBDUMPER_TEST_DSN is set.
func TestNullParamTyping(t *testing.T) {
	dsn := os.Getenv("DBDUMPER_TEST_DSN")
	if dsn == "" {
		t.Skip("set DBDUMPER_TEST_DSN to run")
	}
	ctx := context.Background()
	db, err := Open(ctx, ConnConfig{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	types := []string{
		"varbinary(max)", "varbinary(50)", "binary(8)", "image",
		"uniqueidentifier", "xml", "decimal(18,4)", "money",
		"datetime", "datetime2(7)", "datetimeoffset(7)", "date", "time(7)",
		"geography", "geometry", "hierarchyid", "sql_variant",
		"nvarchar(50)", "varchar(50)", "int", "bit", "float",
	}
	for _, ty := range types {
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, err = conn.ExecContext(ctx, "CREATE TABLE #probe (c "+ty+" NULL)")
		if err != nil {
			t.Logf("%-20s CREATE failed: %v", ty, err)
			conn.Close()
			continue
		}
		var untyped, typedBytes string
		if _, err := conn.ExecContext(ctx, "INSERT INTO #probe (c) VALUES (@p1)", nil); err != nil {
			untyped = "FAIL: " + err.Error()
		} else {
			untyped = "ok"
		}
		if _, err := conn.ExecContext(ctx, "INSERT INTO #probe (c) VALUES (@p1)", []byte(nil)); err != nil {
			typedBytes = "FAIL: " + err.Error()
		} else {
			typedBytes = "ok"
		}
		t.Logf("%-20s untyped-nil=%-8s  nil-[]byte=%s", ty, untyped, typedBytes)
		conn.ExecContext(ctx, "DROP TABLE #probe")
		conn.Close()
	}
}
