package export

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/JeePeeTee/dbdumper/internal/model"
)

func sampleDB() *model.Database {
	return &model.Database{
		Schemas: []model.Schema{{Name: "dbo"}, {Name: "sales", Owner: "dbo"}},
		Tables: []model.Table{
			{
				Schema: "sales", Name: "Customer",
				Columns: []model.Column{
					{Name: "Id", TypeName: "int", Ordinal: 1},
					{Name: "Name", TypeName: "nvarchar", MaxLength: 200, Ordinal: 2, IsNullable: true},
				},
				PrimaryKey: &model.Index{
					Name: "PK_Customer", IsPrimaryKey: true, TypeDes: "CLUSTERED",
					Columns: []model.IndexColumn{{Name: "Id"}},
				},
			},
			{
				Schema: "sales", Name: "Order",
				Columns: []model.Column{
					{Name: "Id", TypeName: "int", Ordinal: 1},
					{Name: "CustomerId", TypeName: "int", Ordinal: 2},
				},
				ForeignKeys: []model.ForeignKey{{
					Name: "FK_Order_Customer", Columns: []string{"CustomerId"},
					ReferencedSchema: "sales", ReferencedTable: "Customer",
					ReferencedColumns: []string{"Id"},
				}},
			},
		},
		Modules: []model.Module{
			{Kind: model.ModuleView, Schema: "dbo", Name: "vAll",
				Definition: "CREATE VIEW dbo.vAll AS SELECT 1 AS x", AnsiNulls: true, QuotedIdentifier: true},
			{Kind: model.ModuleProcedure, Schema: "dbo", Name: "pDo",
				Definition: "CREATE PROCEDURE dbo.pDo AS SELECT 1", AnsiNulls: true, QuotedIdentifier: true},
		},
	}
}

func filesUnder(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func TestSchemaDirLaysOutOneFilePerObject(t *testing.T) {
	dir := t.TempDir()
	res, err := writeSchemaDir(dir, sampleDB(), Options{})
	if err != nil {
		t.Fatal(err)
	}

	want := []string{
		"foreignkeys/sales.Order.sql",
		"procedures/dbo.pDo.sql",
		"schemas/sales.sql",
		"tables/sales.Customer.sql",
		"tables/sales.Order.sql",
		"views/dbo.vAll.sql",
	}
	got := filesUnder(t, dir)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("layout differs:\n got %v\nwant %v", got, want)
	}
	if res.Written != len(want) {
		t.Errorf("Written = %d, want %d", res.Written, len(want))
	}

	// dbo is never scripted: it exists in every database.
	if _, err := os.Stat(filepath.Join(dir, "schemas", "dbo.sql")); !os.IsNotExist(err) {
		t.Error("dbo should not have been written")
	}

	// A table's own indexes and constraints belong with it; a foreign key names
	// a second table and is kept apart so every table can be created first.
	table, err := os.ReadFile(filepath.Join(dir, "tables", "sales.Order.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(table), "FOREIGN KEY") {
		t.Error("the table file should not carry its foreign keys")
	}
	fk, err := os.ReadFile(filepath.Join(dir, "foreignkeys", "sales.Order.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(fk), "FK_Order_Customer") {
		t.Errorf("the foreign key file is missing its constraint:\n%s", fk)
	}
}

// TestSchemaDirRemovesDroppedObjects is the behaviour the layout exists for.
// Without it a directory records every object that ever existed rather than the
// ones that do, and a git history built on it never shows a drop.
func TestSchemaDirRemovesDroppedObjects(t *testing.T) {
	dir := t.TempDir()
	db := sampleDB()
	if _, err := writeSchemaDir(dir, db, Options{}); err != nil {
		t.Fatal(err)
	}

	// Drop a table and a view from the model, as dropping them from the
	// database would.
	db.Tables = db.Tables[:1]
	db.Modules = db.Modules[:1]

	res, err := writeSchemaDir(dir, db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed != 3 { // the table, its foreign keys, and the procedure
		t.Errorf("Removed = %d, want 3", res.Removed)
	}

	want := []string{
		"schemas/sales.sql",
		"tables/sales.Customer.sql",
		"views/dbo.vAll.sql",
	}
	if got := filesUnder(t, dir); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("after dropping:\n got %v\nwant %v", got, want)
	}
}

// TestSchemaDirLeavesForeignFilesAlone - the directory is meant to live in a
// repository, where it will accumulate things this tool did not write.
func TestSchemaDirLeavesForeignFilesAlone(t *testing.T) {
	dir := t.TempDir()
	if _, err := writeSchemaDir(dir, sampleDB(), Options{}); err != nil {
		t.Fatal(err)
	}

	keep := map[string]string{
		"README.md":           "why this directory exists",
		"tables/notes.txt":    "a note beside the tables",
		".gitattributes":      "*.sql text eol=lf",
		"tables/subdir/x.sql": "someone else's file, in a directory we do not own",
		"unmanaged/other.sql": "a directory outside the layout",
	}
	for rel, body := range keep {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := writeSchemaDir(dir, sampleDB(), Options{}); err != nil {
		t.Fatal(err)
	}
	for rel := range keep {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
			t.Errorf("%s was removed, but nothing this tool writes could have created it", rel)
		}
	}
}

// TestSchemaDirDoesNotRewriteUnchangedFiles keeps a nightly job from touching
// every file's timestamp when the database has not moved.
func TestSchemaDirDoesNotRewriteUnchangedFiles(t *testing.T) {
	dir := t.TempDir()
	if _, err := writeSchemaDir(dir, sampleDB(), Options{}); err != nil {
		t.Fatal(err)
	}
	res, err := writeSchemaDir(dir, sampleDB(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Written != 0 || res.Removed != 0 {
		t.Errorf("a second run of an unchanged database wrote %d and removed %d, want 0 and 0",
			res.Written, res.Removed)
	}
}

// TestSchemaDirEscapesAwkwardNames - object names are far freer than filenames,
// and two that differ must not land on one file.
func TestSchemaDirEscapesAwkwardNames(t *testing.T) {
	dir := t.TempDir()
	db := &model.Database{
		Tables: []model.Table{
			{Schema: "odd name", Name: `Weird "Table"`,
				Columns: []model.Column{{Name: "Id", TypeName: "int", Ordinal: 1}}},
			{Schema: "dbo", Name: "a.b",
				Columns: []model.Column{{Name: "Id", TypeName: "int", Ordinal: 1}}},
			{Schema: "dbo.a", Name: "b",
				Columns: []model.Column{{Name: "Id", TypeName: "int", Ordinal: 1}}},
		},
	}
	if _, err := writeSchemaDir(dir, db, Options{}); err != nil {
		t.Fatal(err)
	}
	got := filesUnder(t, dir)
	if len(got) != 3 {
		t.Fatalf("three tables should give three files, got %d: %v", len(got), got)
	}
	for _, name := range got {
		if strings.ContainsAny(filepath.Base(name), `"<>|:*?`) {
			t.Errorf("%s is not a legal filename on Windows", name)
		}
	}
}
