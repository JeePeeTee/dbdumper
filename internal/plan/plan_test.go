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

// TestForeignKeysToFilteredDataAreNotChecked - a --where predicate leaves a
// table holding a subset of its rows, so keys pointing at it are in exactly the
// same position as keys pointing at a table whose data was skipped entirely.
func TestForeignKeysToFilteredDataAreNotChecked(t *testing.T) {
	db := &model.Database{Tables: []model.Table{
		{Schema: "dbo", Name: "LogEvents", RowFilter: "CreatedOn > dateadd(day,-90,getutcdate())"},
		{Schema: "dbo", Name: "Whole"},
		{Schema: "dbo", Name: "Ref", ForeignKeys: []model.ForeignKey{
			{Name: "FK_Ref_LogEvents", Columns: []string{"LogId"},
				ReferencedSchema: "dbo", ReferencedTable: "LogEvents", ReferencedColumns: []string{"Oid"}},
			{Name: "FK_Ref_Whole", Columns: []string{"WholeId"},
				ReferencedSchema: "dbo", ReferencedTable: "Whole", ReferencedColumns: []string{"Oid"}},
		}},
	}}

	got := map[string]string{}
	for _, s := range foreignKeys(db) {
		switch {
		case strings.Contains(s.SQL, "[FK_Ref_LogEvents]"):
			got["filtered"] = s.SQL
		case strings.Contains(s.SQL, "[FK_Ref_Whole]"):
			got["whole"] = s.SQL
		}
	}
	if !strings.Contains(got["filtered"], "WITH NOCHECK") {
		t.Errorf("key to a filtered table should be WITH NOCHECK:\n%s", got["filtered"])
	}
	if strings.Contains(got["whole"], "NOCHECK") {
		t.Errorf("key to a fully dumped table should stay WITH CHECK:\n%s", got["whole"])
	}
}

func TestPartialData(t *testing.T) {
	cases := []struct {
		t    model.Table
		want bool
	}{
		{model.Table{}, false},
		{model.Table{DataSkipped: true}, true},
		{model.Table{RowFilter: "1=1"}, true},
		{model.Table{DataSkipped: true, RowFilter: "1=1"}, true},
	}
	for _, c := range cases {
		if got := c.t.PartialData(); got != c.want {
			t.Errorf("%+v: PartialData() = %v, want %v", c.t, got, c.want)
		}
	}
}

// TestDisabledIndexIsCreatedBeforeTheDataAndDisabled - a disabled index built
// after the load, as enabled ones are, came back enabled; and a disabled unique
// index over rows that no longer satisfy it failed to build at all, aborting
// the restore. It has to be made on the empty table and switched off there.
func TestDisabledIndexIsCreatedBeforeTheDataAndDisabled(t *testing.T) {
	tbl := model.Table{Schema: "dbo", Name: "Orders", Indexes: []model.Index{
		{Name: "IX_Live", TypeDes: "NONCLUSTERED", Columns: []model.IndexColumn{{Name: "A"}}},
		{Name: "UX_Off", TypeDes: "NONCLUSTERED", IsUnique: true, IsDisabled: true,
			Columns: []model.IndexColumn{{Name: "B"}}},
	}}
	db := &model.Database{Tables: []model.Table{tbl}}

	var pre, post string
	for _, s := range tables(db) {
		pre += s.SQL + "\n"
	}
	for _, s := range indexes(db) {
		post += s.SQL + "\n"
	}

	if !strings.Contains(pre, "CREATE UNIQUE NONCLUSTERED INDEX [UX_Off]") ||
		!strings.Contains(pre, "ALTER INDEX [UX_Off] ON [dbo].[Orders] DISABLE") {
		t.Errorf("the disabled index should be created and disabled with its table:\n%s", pre)
	}
	if strings.Index(pre, "CREATE TABLE") > strings.Index(pre, "[UX_Off]") {
		t.Errorf("the disabled index must follow its table:\n%s", pre)
	}
	if strings.Contains(post, "UX_Off") {
		t.Errorf("the disabled index must not be built again after the data:\n%s", post)
	}
	if !strings.Contains(post, "[IX_Live]") || strings.Contains(pre, "IX_Live") {
		t.Errorf("an enabled index still belongs after the data:\npre:\n%s\npost:\n%s", pre, post)
	}

	// The per-object script says the same.
	for _, s := range ObjectScripts(db) {
		if s.Kind == "tables" && !strings.Contains(s.SQL, "ALTER INDEX [UX_Off] ON [dbo].[Orders] DISABLE;\n") {
			t.Errorf("the table script should record the index as disabled:\n%s", s.SQL)
		}
	}
}
