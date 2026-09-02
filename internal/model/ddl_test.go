package model

import (
	"strings"
	"testing"
)

func TestQuote(t *testing.T) {
	cases := map[string]string{
		"Customer":     "[Customer]",
		"Order Line":   "[Order Line]",
		"weird]name":   "[weird]]name]",
		`quo"ted`:      `[quo"ted]`,
		"]]":           "[]]]]]",
		"dbo.NotSplit": "[dbo.NotSplit]",
	}
	for in, want := range cases {
		if got := Quote(in); got != want {
			t.Errorf("Quote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestQuoteString(t *testing.T) {
	if got, want := QuoteString("O'Brien"), "'O''Brien'"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestTypeRef(t *testing.T) {
	cases := []struct {
		name string
		col  Column
		want string
	}{
		{"varchar", Column{TypeName: "varchar", MaxLength: 100}, "[varchar](100)"},
		{"varchar max", Column{TypeName: "varchar", MaxLength: -1}, "[varchar](max)"},
		// nvarchar max_length is in bytes; the DDL wants characters.
		{"nvarchar", Column{TypeName: "nvarchar", MaxLength: 100}, "[nvarchar](50)"},
		{"nvarchar max", Column{TypeName: "nvarchar", MaxLength: -1}, "[nvarchar](max)"},
		{"nchar", Column{TypeName: "nchar", MaxLength: 20}, "[nchar](10)"},
		{"binary", Column{TypeName: "binary", MaxLength: 8}, "[binary](8)"},
		{"varbinary max", Column{TypeName: "varbinary", MaxLength: -1}, "[varbinary](max)"},
		{"decimal", Column{TypeName: "decimal", Precision: 19, Scale: 4}, "[decimal](19,4)"},
		{"numeric", Column{TypeName: "numeric", Precision: 38, Scale: 10}, "[numeric](38,10)"},
		{"float", Column{TypeName: "float", Precision: 53}, "[float](53)"},
		{"datetime2", Column{TypeName: "datetime2", Scale: 7}, "[datetime2](7)"},
		{"time", Column{TypeName: "time", Scale: 3}, "[time](3)"},
		{"datetimeoffset", Column{TypeName: "datetimeoffset", Scale: 0}, "[datetimeoffset](0)"},
		{"int", Column{TypeName: "int", Precision: 10}, "[int]"},
		// vector needs its dimension count, which the server reports only as a
		// storage size: an 8-byte header plus a float32 per dimension. Without
		// it, CREATE TABLE fails with "Cannot find data type vector" (2715),
		// which reads as though the type were unsupported.
		{"vector", Column{TypeName: "vector", BaseTypeName: "varbinary", MaxLength: 6152},
			"[vector](1536)"},
		{"small vector", Column{TypeName: "vector", BaseTypeName: "varbinary", MaxLength: 20},
			"[vector](3)"},
		// A size that cannot be a vector is left bare rather than guessed at.
		{"vector of unreadable size", Column{TypeName: "vector", BaseTypeName: "varbinary", MaxLength: 7},
			"[vector]"},
		// A user-defined type that happens to be called vector is not one.
		{"user type named vector", Column{TypeSchema: "dbo", TypeName: "vector"}, "[dbo].[vector]"},
		{"uniqueidentifier", Column{TypeName: "uniqueidentifier", MaxLength: 16}, "[uniqueidentifier]"},
		{"geography", Column{TypeName: "geography", MaxLength: -1}, "[geography]"},
		// A user-defined alias carries its own length; do not restate it.
		{"alias", Column{TypeSchema: "dbo", TypeName: "PhoneNumber", MaxLength: 60}, "[dbo].[PhoneNumber]"},
	}
	for _, c := range cases {
		if got := c.col.TypeRef(); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestColumnDDL(t *testing.T) {
	cases := []struct {
		name string
		col  Column
		want string
	}{
		{
			"identity not null",
			Column{Name: "Id", TypeName: "int", IsIdentity: true, IdentitySeed: "10", IdentityIncrement: "5"},
			"[Id] [int] IDENTITY(10,5) NOT NULL",
		},
		{
			"nullable with default",
			Column{Name: "Balance", TypeName: "decimal", Precision: 19, Scale: 4, IsNullable: true,
				DefaultName: "DF_B", DefaultDefinition: "((0))"},
			"[Balance] [decimal](19,4) NULL CONSTRAINT [DF_B] DEFAULT ((0))",
		},
		{
			"collated",
			Column{Name: "Name", TypeName: "nvarchar", MaxLength: 100, IsNullable: true, Collation: "Latin1_General_BIN2"},
			"[Name] [nvarchar](50) COLLATE Latin1_General_BIN2 NULL",
		},
		{
			"rowguidcol",
			Column{Name: "G", TypeName: "uniqueidentifier", IsRowGUIDCol: true},
			"[G] [uniqueidentifier] ROWGUIDCOL NOT NULL",
		},
		{
			"sparse",
			Column{Name: "S", TypeName: "int", IsSparse: true, IsNullable: true},
			"[S] [int] SPARSE NULL",
		},
		{
			"computed",
			Column{Name: "C", IsComputed: true, ComputedDefinition: "([A]*(2))", IsNullable: true},
			"[C] AS ([A]*(2))",
		},
		{
			"computed persisted not null",
			Column{Name: "C", IsComputed: true, ComputedDefinition: "([A]+(1))", IsPersisted: true},
			"[C] AS ([A]+(1)) PERSISTED NOT NULL",
		},
		{
			// A user-defined type carries its own collation; restating it is an error.
			"alias type has no collation clause",
			Column{Name: "P", TypeSchema: "dbo", TypeName: "PhoneNumber", Collation: "Latin1_General_CI_AS"},
			"[P] [dbo].[PhoneNumber] NOT NULL",
		},
	}
	for _, c := range cases {
		if got := c.col.ColumnDDL(true); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

func TestIndexDDL(t *testing.T) {
	tbl := Table{Schema: "sales", Name: "Customer"}

	filtered := Index{
		Name: "IX_Active", TypeDes: "NONCLUSTERED", IsUnique: true,
		Columns: []IndexColumn{
			{Name: "Code"},
			{Name: "Name", IsDescending: true},
			{Name: "Balance", IsIncluded: true},
		},
		FilterDefinition: "([IsActive]=(1))",
		FillFactor:       80,
	}
	want := "CREATE UNIQUE NONCLUSTERED INDEX [IX_Active] ON [sales].[Customer] " +
		"([Code] ASC, [Name] DESC) INCLUDE ([Balance]) WHERE ([IsActive]=(1)) WITH (FILLFACTOR = 80)"
	if got := filtered.CreateIndexDDL(tbl); got != want {
		t.Errorf("filtered index:\n got %s\nwant %s", got, want)
	}

	pk := Index{
		Name: "PK_Customer", TypeDes: "CLUSTERED", IsPrimaryKey: true, IsUnique: true,
		Columns: []IndexColumn{{Name: "Id"}},
	}
	if got, want := pk.ConstraintDDL(), "CONSTRAINT [PK_Customer] PRIMARY KEY CLUSTERED ([Id] ASC)"; got != want {
		t.Errorf("pk:\n got %s\nwant %s", got, want)
	}

	uq := Index{
		Name: "UQ_Code", TypeDes: "NONCLUSTERED", IsUniqueConstraint: true, IsUnique: true,
		Columns: []IndexColumn{{Name: "Code"}},
	}
	if got, want := uq.ConstraintDDL(), "CONSTRAINT [UQ_Code] UNIQUE NONCLUSTERED ([Code] ASC)"; got != want {
		t.Errorf("unique:\n got %s\nwant %s", got, want)
	}
}

func TestForeignKeyDDL(t *testing.T) {
	tbl := Table{Schema: "sales", Name: "Order Line"}
	fk := ForeignKey{
		Name: "FK_OL_C", Columns: []string{"CustomerId"},
		ReferencedSchema: "sales", ReferencedTable: "Customer", ReferencedColumns: []string{"CustomerId"},
		OnDelete: "CASCADE", OnUpdate: "NO_ACTION",
	}
	want := "ALTER TABLE [sales].[Order Line] WITH CHECK ADD CONSTRAINT [FK_OL_C] " +
		"FOREIGN KEY ([CustomerId]) REFERENCES [sales].[Customer] ([CustomerId]) ON DELETE CASCADE"
	if got := fk.AddDDL(tbl); got != want {
		t.Errorf("\n got %s\nwant %s", got, want)
	}

	fk.IsNotTrusted = true
	fk.OnDelete = "SET_NULL"
	if got := fk.AddDDL(tbl); got[:len("ALTER TABLE [sales].[Order Line] WITH NOCHECK")] !=
		"ALTER TABLE [sales].[Order Line] WITH NOCHECK" {
		t.Errorf("untrusted FK should use WITH NOCHECK, got %s", got)
	} else if want := " ON DELETE SET NULL"; got[len(got)-len(want):] != want {
		t.Errorf("SET_NULL should render as SET NULL, got %s", got)
	}
}

func TestInsertable(t *testing.T) {
	cases := []struct {
		col  Column
		want bool
	}{
		{Column{TypeName: "int"}, true},
		{Column{TypeName: "timestamp"}, false},
		{Column{TypeName: "rowversion"}, false},
		{Column{IsComputed: true, TypeName: "int"}, false},
		// An alias type over a normal type is insertable...
		{Column{TypeSchema: "dbo", TypeName: "PhoneNumber", BaseTypeName: "nvarchar"}, true},
		// ...but the base type is what decides it.
		{Column{TypeSchema: "dbo", TypeName: "Stamp", BaseTypeName: "timestamp"}, false},
	}
	for _, c := range cases {
		if got := c.col.Insertable(); got != c.want {
			t.Errorf("Insertable(%+v) = %v, want %v", c.col, got, c.want)
		}
	}
}

func TestSequenceDDL(t *testing.T) {
	cache := 20
	s := Sequence{
		Schema: "sales", Name: "OrderNumber", DataType: "bigint",
		StartValue: "5000", Increment: "7", MinValue: "1", MaxValue: "999999",
		IsCached: true, CacheSize: &cache,
	}
	want := "CREATE SEQUENCE [sales].[OrderNumber] AS [bigint] START WITH 5000 INCREMENT BY 7 " +
		"MINVALUE 1 MAXVALUE 999999 NO CYCLE CACHE 20"
	if got := s.CreateDDL(); got != want {
		t.Errorf("\n got %s\nwant %s", got, want)
	}
}

// TestColumnstoreIndexHasNoSortOrder - SQL Server rejects CREATE COLUMNSTORE
// INDEX outright when a column carries ASC or DESC: "specifying sort order
// (ASC or DESC) is not allowed when creating a columnstore index".
func TestColumnstoreIndexHasNoSortOrder(t *testing.T) {
	tbl := Table{Schema: "dbo", Name: "Fact"}

	nc := Index{
		Name: "IX_Fact_CS", TypeDes: "NONCLUSTERED COLUMNSTORE",
		Columns: []IndexColumn{{Name: "a"}, {Name: "b", IsDescending: true}},
	}
	got := nc.CreateIndexDDL(tbl)
	want := "CREATE NONCLUSTERED COLUMNSTORE INDEX [IX_Fact_CS] ON [dbo].[Fact] ([a], [b])"
	if got != want {
		t.Errorf("\n got %s\nwant %s", got, want)
	}
	for _, bad := range []string{" ASC", " DESC"} {
		if strings.Contains(got, bad) {
			t.Errorf("columnstore DDL must not carry a sort order, got %s", got)
		}
	}

	// A clustered columnstore covers the whole table and names no columns.
	cc := Index{
		Name: "IX_Fact_CCS", TypeDes: "CLUSTERED COLUMNSTORE",
		Columns: []IndexColumn{{Name: "a"}},
	}
	if got, want := cc.CreateIndexDDL(tbl),
		"CREATE CLUSTERED COLUMNSTORE INDEX [IX_Fact_CCS] ON [dbo].[Fact]"; got != want {
		t.Errorf("\n got %s\nwant %s", got, want)
	}

	// A normal index still gets its sort order.
	rb := Index{Name: "IX_Plain", TypeDes: "NONCLUSTERED", Columns: []IndexColumn{{Name: "a", IsDescending: true}}}
	if !strings.Contains(rb.CreateIndexDDL(tbl), "[a] DESC") {
		t.Errorf("rowstore index lost its sort order: %s", rb.CreateIndexDDL(tbl))
	}
}

// TestUntrustedCheckConstraintIsRecreatedUnvalidated - SQL Server lets a check
// constraint be enabled but never verified against the rows already in the
// table. Recreating one of those WITH CHECK asks the server to verify them now,
// which fails on exactly the data the flag records. Foreign keys have always
// handled this; check constraints did not.
func TestUntrustedCheckConstraintIsRecreatedUnvalidated(t *testing.T) {
	table := Table{Schema: "dbo", Name: "T"}
	cases := []struct {
		name string
		cc   CheckConstraint
		want string
	}{
		{"ordinary", CheckConstraint{Name: "CK", Definition: "([Qty]>(0))"}, "WITH CHECK"},
		{"disabled", CheckConstraint{Name: "CK", Definition: "([Qty]>(0))", IsDisabled: true}, "WITH NOCHECK"},
		{"untrusted", CheckConstraint{Name: "CK", Definition: "([Qty]>(0))", IsNotTrusted: true}, "WITH NOCHECK"},
		{"both", CheckConstraint{Name: "CK", Definition: "([Qty]>(0))", IsDisabled: true, IsNotTrusted: true}, "WITH NOCHECK"},
	}
	for _, c := range cases {
		got := c.cc.AddDDL(table)
		if !strings.Contains(got, c.want) {
			t.Errorf("%s: expected %s in:\n  %s", c.name, c.want, got)
		}
	}
}
