package plan

import (
	"strings"
	"testing"

	"github.com/JeePeeTee/dbdumper/internal/model"
)

// TestForeignKeysToSkippedDataAreNotChecked covers the --exclude-data case: a
// table kept but emptied cannot satisfy the keys pointing at it, so those have
// to be created WITH NOCHECK or the restore aborts partway through.
func TestForeignKeysToSkippedDataAreNotChecked(t *testing.T) {
	db := &model.Database{Tables: []model.Table{
		{Schema: "dbo", Name: "Document", DataSkipped: true},
		{Schema: "dbo", Name: "Kept"},
		{Schema: "dbo", Name: "Invoice", ForeignKeys: []model.ForeignKey{
			{Name: "FK_Invoice_Document", Columns: []string{"FileId"},
				ReferencedSchema: "dbo", ReferencedTable: "Document", ReferencedColumns: []string{"Oid"}},
			{Name: "FK_Invoice_Kept", Columns: []string{"KeptId"},
				ReferencedSchema: "dbo", ReferencedTable: "Kept", ReferencedColumns: []string{"Oid"}},
		}},
	}}

	got := map[string]string{}
	for _, s := range foreignKeys(db) {
		switch {
		case strings.Contains(s.SQL, "[FK_Invoice_Document]"):
			got["skipped"] = s.SQL
		case strings.Contains(s.SQL, "[FK_Invoice_Kept]"):
			got["kept"] = s.SQL
		}
	}

	if !strings.Contains(got["skipped"], "WITH NOCHECK") {
		t.Errorf("key to a data-skipped table should be WITH NOCHECK:\n%s", got["skipped"])
	}
	if !strings.Contains(got["kept"], "WITH CHECK") || strings.Contains(got["kept"], "NOCHECK") {
		t.Errorf("key to a fully dumped table should stay WITH CHECK:\n%s", got["kept"])
	}
}

// TestModulesResetSessionOptions guards the SET options from leaking onto the
// pooled connection after a module with non-default settings is created.
func TestModulesResetSessionOptions(t *testing.T) {
	db := &model.Database{Modules: []model.Module{
		{Schema: "dbo", Name: "V", Kind: model.ModuleView,
			Definition: "CREATE VIEW dbo.V AS SELECT 1 AS x", AnsiNulls: false, QuotedIdentifier: false},
	}}
	stmts := modules(db)
	if len(stmts) != 1 {
		t.Fatalf("got %d statements", len(stmts))
	}
	sql := stmts[0].SQL
	if !strings.HasPrefix(sql, "SET ANSI_NULLS OFF;\nSET QUOTED_IDENTIFIER OFF;\n") {
		t.Errorf("module should be created under its own SET options:\n%s", sql)
	}
	if !strings.HasSuffix(sql, "SET ANSI_NULLS ON;\nSET QUOTED_IDENTIFIER ON;") {
		t.Errorf("module should restore the default SET options:\n%s", sql)
	}
	if !strings.Contains(sql, "EXEC sp_executesql N'CREATE VIEW dbo.V AS SELECT 1 AS x'") {
		t.Errorf("definition should be executed via sp_executesql:\n%s", sql)
	}
	if !stmts[0].Retryable {
		t.Error("modules should be retryable to absorb dependency ordering")
	}
}

// TestSchemaCreationIsIdempotent - a restore into an existing database must not
// trip over schemas that are already there.
func TestSchemaCreationIsIdempotent(t *testing.T) {
	db := &model.Database{Schemas: []model.Schema{{Name: "dbo"}, {Name: "sales"}, {Name: "odd name"}}}
	stmts := schemas(db)
	if len(stmts) != 2 {
		t.Fatalf("dbo should be skipped; got %d statements", len(stmts))
	}
	for _, s := range stmts {
		if !strings.HasPrefix(s.SQL, "IF SCHEMA_ID(") {
			t.Errorf("schema creation should be guarded:\n%s", s.SQL)
		}
	}
	if !strings.Contains(stmts[1].SQL, "[odd name]") {
		t.Errorf("schema name should be quoted inside the dynamic SQL:\n%s", stmts[1].SQL)
	}
}

// TestFinalizeSkipsEmptyTables - DBCC CHECKIDENT on a table that received no
// rows would be noise at best.
func TestFinalizeSkipsEmptyTables(t *testing.T) {
	ident := []model.Column{{Name: "Id", IsIdentity: true}}
	db := &model.Database{Tables: []model.Table{
		{Schema: "dbo", Name: "Empty", Columns: ident, RowCount: 0},
		{Schema: "dbo", Name: "Filled", Columns: ident, RowCount: 5},
		{Schema: "dbo", Name: "NoIdentity", Columns: []model.Column{{Name: "Id"}}, RowCount: 5},
	}}
	stmts := finalize(db)
	if len(stmts) != 1 {
		t.Fatalf("expected one reseed, got %d: %v", len(stmts), stmts)
	}
	if !strings.Contains(stmts[0].SQL, "[dbo].[Filled]") {
		t.Errorf("wrong table reseeded:\n%s", stmts[0].SQL)
	}
}
