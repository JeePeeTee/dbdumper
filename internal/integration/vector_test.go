package integration

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JeePeeTee/dbdumper/internal/archive"
	"github.com/JeePeeTee/dbdumper/internal/export"
	"github.com/JeePeeTee/dbdumper/internal/importer"
	"github.com/JeePeeTee/dbdumper/internal/sqlsrv"
)

// TestVectorRoundTrip covers SQL Server 2025's vector type end to end.
//
// It is worth its own test because vector is reported by sys.types as a
// varbinary underneath, so every part of the tool that asks "what type is
// this?" gets the wrong answer unless it asks specifically. All three ways
// that went wrong are exercised here: the DDL needs a dimension count the
// catalogue only expresses as a storage size, the values travel as JSON text
// rather than as bytes, and bulk copy cannot carry the column at all.
//
// Skipped on servers that have no vector type, so the suite still runs against
// SQL Server 2019 and 2022.
func TestVectorRoundTrip(t *testing.T) {
	ctx := context.Background()
	base := baseConfig(t)

	master, err := sqlsrv.Open(ctx, base.WithDatabase("master"))
	if err != nil {
		t.Fatalf("connect to master: %v", err)
	}
	defer master.Close()

	var hasVector int
	if err := master.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sys.types WHERE name = 'vector' AND is_user_defined = 0").Scan(&hasVector); err != nil {
		t.Fatal(err)
	}
	if hasVector == 0 {
		t.Skip("this server has no vector type; SQL Server 2025 or later is needed")
	}

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

	// Shaped after the table this was reported against: an embedding column
	// sitting among ordinary ones, at a dimension count real models produce.
	if _, err := src.ExecContext(ctx, `CREATE TABLE dbo.Activity (
		Oid uniqueidentifier ROWGUIDCOL NOT NULL CONSTRAINT PK_Activity PRIMARY KEY,
		Subject nvarchar(200) NULL,
		Small vector(3) NULL,
		Embedding vector(1536) NOT NULL)`); err != nil {
		t.Fatalf("create the source table: %v", err)
	}

	big := "[" + strings.Repeat("0.25,", 1535) + "0.25]"
	rows := []any{"[1.5,-2,3]", "[0,0,0]", nil} // including a NULL vector
	for i, small := range rows {
		if _, err := src.ExecContext(ctx,
			"INSERT INTO dbo.Activity (Oid, Subject, Small, Embedding) VALUES (NEWID(), @p1, @p2, @p3)",
			fmt.Sprintf("row %d", i), small, big); err != nil {
			t.Fatalf("insert row %d: %v", i, err)
		}
	}

	out := filepath.Join(t.TempDir(), "vector.dbdump")
	if _, err := export.Run(ctx, src, export.Options{
		Out: out, ProgressInterval: -1, Warn: warnf(t),
	}); err != nil {
		t.Fatalf("export: %v", err)
	}

	ar, err := archive.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer ar.Close()

	// The DDL must carry the dimension count, or the restore cannot even
	// create the table.
	ddl := ar.Manifest.Database.Tables[0].CreateDDL()
	for _, want := range []string{"[vector](3)", "[vector](1536)"} {
		if !strings.Contains(ddl, want) {
			t.Errorf("DDL is missing %s:\n%s", want, ddl)
		}
	}

	// Values must be written as text, not as base64 of the text, or a restore
	// puts unreadable strings into the column.
	line := readLines(t, ar, ar.Manifest.Database.Tables[0].DataFile)[0]
	if !strings.Contains(line, "e+00") && !strings.Contains(line, "e-00") {
		t.Errorf("vector values do not look like the server's own text form: %s", truncate(line))
	}

	dst, err := sqlsrv.Open(ctx, base.WithDatabase(dstDB))
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()

	res, err := importer.Run(ctx, dst, ar, importer.Options{Warn: warnf(t), Log: func(string, ...any) {}})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.Failed > 0 {
		t.Fatalf("import reported %d failures: %v", res.Failed, res.FailedList)
	}

	// The restored column has to be a real vector, not a varbinary holding the
	// same bytes: only the former works with the vector functions.
	var typeName string
	var maxLength int
	if err := dst.QueryRowContext(ctx, `SELECT t.name, c.max_length
		FROM sys.columns c JOIN sys.types t ON t.user_type_id = c.user_type_id
		WHERE c.object_id = OBJECT_ID('dbo.Activity') AND c.name = 'Embedding'`).Scan(&typeName, &maxLength); err != nil {
		t.Fatal(err)
	}
	if typeName != "vector" {
		t.Errorf("restored column is %q, not vector", typeName)
	}
	if got := (maxLength - 8) / 4; got != 1536 {
		t.Errorf("restored column has %d dimensions, want 1536", got)
	}

	// Every value must come back exactly, NULLs included.
	before, after := vectorRows(ctx, t, src), vectorRows(ctx, t, dst)
	if len(before) != len(after) {
		t.Fatalf("%d rows exported, %d restored", len(before), len(after))
	}
	for oid, want := range before {
		got, ok := after[oid]
		if !ok {
			t.Errorf("row %s is missing after the restore", oid)
			continue
		}
		if got != want {
			t.Errorf("row %s differs:\n  source:   %v\n  restored: %v", oid, want, got)
		}
	}

	var nulls int
	dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM dbo.Activity WHERE Small IS NULL").Scan(&nulls)
	if nulls != 1 {
		t.Errorf("NULL vectors: got %d, want 1", nulls)
	}

	// And the restored data is usable as vectors, which is the whole point of
	// the type. A vector compared with itself has distance ~0; exact equality
	// is not available, since "=" is invalid on vector (8117).
	var distance float64
	if err := dst.QueryRowContext(ctx, `SELECT TOP 1
		VECTOR_DISTANCE('cosine', Embedding, CAST(Embedding AS vector(1536)))
		FROM dbo.Activity`).Scan(&distance); err != nil {
		t.Errorf("restored vectors are not usable with VECTOR_DISTANCE: %v", err)
	} else if distance > 1e-6 {
		t.Errorf("self-distance of a restored vector is %v, want ~0", distance)
	}
}

// vectorRows reads every row's vector columns as the server's own text.
func vectorRows(ctx context.Context, t *testing.T, db *sql.DB) map[string][2]string {
	t.Helper()
	rows, err := db.QueryContext(ctx, `SELECT CONVERT(varchar(36), Oid),
		ISNULL(CAST(Small AS varchar(max)), '<null>'),
		ISNULL(CAST(Embedding AS varchar(max)), '<null>')
		FROM dbo.Activity`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	out := map[string][2]string{}
	for rows.Next() {
		var oid, small, embedding string
		if err := rows.Scan(&oid, &small, &embedding); err != nil {
			t.Fatal(err)
		}
		out[oid] = [2]string{small, embedding}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
