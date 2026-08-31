package sqlsrv

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/JeePeeTee/dbdumper/internal/model"
)

// TestChunksPartitionRealTables is the property the whole feature rests on:
// the planned ranges must cover every row exactly once. A gap loses rows and an
// overlap duplicates them, and either would be silent.
//
// It runs against a real database because the interesting cases - a
// uniqueidentifier clustered key, whose ordering is neither byte order nor the
// order the text form suggests - cannot be reproduced from a fixture.
func TestChunksPartitionRealTables(t *testing.T) {
	dsn := os.Getenv("DBDUMPER_TEST_DSN")
	db := os.Getenv("DBDUMPER_TEST_DB")
	if dsn == "" || db == "" {
		t.Skip("set DBDUMPER_TEST_DSN and DBDUMPER_TEST_DB to run")
	}
	ctx := context.Background()

	conn, err := Open(ctx, ConnConfig{DSN: dsn}.WithDatabase(db))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	in := &Introspector{DB: conn, Warn: func(f string, a ...any) { t.Logf("warn: "+f, a...) }}
	dbm, err := in.Database(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// The largest tables are the ones chunking exists for, and the ones most
	// likely to expose a boundary problem.
	tables := append([]model.Table(nil), dbm.Tables...)
	for i := 0; i < len(tables); i++ {
		for j := i + 1; j < len(tables); j++ {
			if tables[j].EstimatedBytes > tables[i].EstimatedBytes {
				tables[i], tables[j] = tables[j], tables[i]
			}
		}
	}
	if len(tables) > 8 {
		tables = tables[:8]
	}

	checked := 0
	for _, tb := range tables {
		key, ok := in.ChunkKeyFor(ctx, tb)
		if !ok {
			t.Logf("%s.%s: not chunkable", tb.Schema, tb.Name)
			continue
		}
		for _, n := range []int{2, 4, 7} {
			chunks, err := in.PlanChunks(ctx, tb, key, n)
			if err != nil {
				t.Errorf("%s.%s n=%d: %v", tb.Schema, tb.Name, n, err)
				continue
			}
			if len(chunks) == 0 {
				t.Logf("%s.%s n=%d: not split", tb.Schema, tb.Name, n)
				continue
			}

			var total int64
			for _, c := range chunks {
				pred, args := c.Predicate(key.Column.Name, nil)
				q := fmt.Sprintf("SELECT COUNT_BIG(*) FROM %s WHERE %s", tb.QualifiedName(), pred)
				var n64 int64
				if err := conn.QueryRowContext(ctx, q, args...).Scan(&n64); err != nil {
					t.Fatalf("%s.%s chunk %d: %v\n%s", tb.Schema, tb.Name, c.Index, err, q)
				}
				total += n64
			}

			var actual int64
			if err := conn.QueryRowContext(ctx,
				"SELECT COUNT_BIG(*) FROM "+tb.QualifiedName()).Scan(&actual); err != nil {
				t.Fatal(err)
			}
			if total != actual {
				t.Errorf("%s.%s n=%d: chunks cover %d rows, table has %d (key %s %s)",
					tb.Schema, tb.Name, n, total, actual, key.Column.Name, key.Column.SystemType())
				continue
			}
			t.Logf("%s.%s n=%d: %d chunks cover all %d rows (%s %s)",
				tb.Schema, tb.Name, n, len(chunks), actual, key.Column.Name, key.Column.SystemType())
			checked++
		}
	}
	if checked == 0 {
		t.Skip("no chunkable table with rows was found in this database")
	}
}

// TestChunkPredicateShape pins the SQL the ranges produce, which is where an
// off-by-one between chunks would live.
func TestChunkPredicateShape(t *testing.T) {
	chunks := []Chunk{
		{Index: 0, Hi: 10, HasHi: true},
		{Index: 1, Lo: 10, HasLo: true, Hi: 20, HasHi: true},
		{Index: 2, Lo: 20, HasLo: true},
		{Index: 3, NullKeys: true},
	}
	want := []string{
		"[Id] < @p1",
		"[Id] >= @p1 AND [Id] < @p2",
		"[Id] >= @p1",
		"[Id] IS NULL",
	}
	for i, c := range chunks {
		got, args := c.Predicate("Id", nil)
		if got != want[i] {
			t.Errorf("chunk %d: got %q, want %q", i, got, want[i])
		}
		// Boundaries travel as parameters; nothing is spliced into the text.
		if strings.Contains(got, "10") || strings.Contains(got, "20") {
			t.Errorf("chunk %d: a boundary value leaked into the SQL: %q", i, got)
		}
		wantArgs := 0
		if c.HasLo {
			wantArgs++
		}
		if c.HasHi {
			wantArgs++
		}
		if len(args) != wantArgs {
			t.Errorf("chunk %d: %d args, want %d", i, len(args), wantArgs)
		}
	}
}
