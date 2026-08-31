package model

import (
	"fmt"
	"strings"
)

// Quote wraps an identifier in brackets, escaping embedded closing brackets.
func Quote(name string) string {
	return "[" + strings.ReplaceAll(name, "]", "]]") + "]"
}

// QuoteString wraps a value in single quotes for use as a T-SQL literal.
func QuoteString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// TypeRef renders the type portion of a column definition, e.g. [nvarchar](50).
func (c Column) TypeRef() string {
	if c.TypeSchema != "" {
		// User-defined type: length/precision are baked into the type itself.
		return Quote(c.TypeSchema) + "." + Quote(c.TypeName)
	}
	base := Quote(c.TypeName)
	if c.IsVector() {
		// vector carries its dimension count the way varchar carries a length,
		// and the server rejects it without one: "Cannot find data type vector"
		// (2715), which reads like the type is unsupported rather than
		// incompletely written.
		if n := c.VectorDimensions(); n > 0 {
			return fmt.Sprintf("%s(%d)", base, n)
		}
		return base
	}
	switch strings.ToLower(c.TypeName) {
	case "char", "varchar", "binary", "varbinary":
		if c.MaxLength == -1 {
			return base + "(max)"
		}
		return fmt.Sprintf("%s(%d)", base, c.MaxLength)
	case "nchar", "nvarchar":
		if c.MaxLength == -1 {
			return base + "(max)"
		}
		return fmt.Sprintf("%s(%d)", base, c.MaxLength/2)
	case "decimal", "numeric":
		return fmt.Sprintf("%s(%d,%d)", base, c.Precision, c.Scale)
	case "float":
		return fmt.Sprintf("%s(%d)", base, c.Precision)
	case "datetime2", "time", "datetimeoffset":
		return fmt.Sprintf("%s(%d)", base, c.Scale)
	default:
		return base
	}
}

// ColumnDDL renders one line of a CREATE TABLE column list.
func (c Column) ColumnDDL(includeDefault bool) string {
	if c.IsComputed {
		s := Quote(c.Name) + " AS " + c.ComputedDefinition
		if c.IsPersisted {
			s += " PERSISTED"
			if !c.IsNullable {
				s += " NOT NULL"
			}
		}
		return s
	}

	var b strings.Builder
	b.WriteString(Quote(c.Name))
	b.WriteString(" ")
	b.WriteString(c.TypeRef())

	if c.Collation != "" && c.TypeSchema == "" {
		b.WriteString(" COLLATE " + c.Collation)
	}
	if c.IsSparse {
		b.WriteString(" SPARSE")
	}
	if c.IsIdentity {
		seed, inc := c.IdentitySeed, c.IdentityIncrement
		if seed == "" {
			seed = "1"
		}
		if inc == "" {
			inc = "1"
		}
		fmt.Fprintf(&b, " IDENTITY(%s,%s)", seed, inc)
	}
	if c.IsRowGUIDCol {
		b.WriteString(" ROWGUIDCOL")
	}
	if c.IsNullable {
		b.WriteString(" NULL")
	} else {
		b.WriteString(" NOT NULL")
	}
	if includeDefault && c.DefaultDefinition != "" {
		if c.DefaultName != "" {
			b.WriteString(" CONSTRAINT " + Quote(c.DefaultName))
		}
		b.WriteString(" DEFAULT " + c.DefaultDefinition)
	}
	return b.String()
}

func indexKeyList(cols []IndexColumn, included bool) string {
	parts := make([]string, 0, len(cols))
	for _, ic := range cols {
		if ic.IsIncluded != included {
			continue
		}
		if included {
			parts = append(parts, Quote(ic.Name))
			continue
		}
		dir := " ASC"
		if ic.IsDescending {
			dir = " DESC"
		}
		parts = append(parts, Quote(ic.Name)+dir)
	}
	return strings.Join(parts, ", ")
}

// plainColumnList renders key columns without a sort direction.
func plainColumnList(cols []IndexColumn) string {
	parts := make([]string, 0, len(cols))
	for _, ic := range cols {
		if !ic.IsIncluded {
			parts = append(parts, Quote(ic.Name))
		}
	}
	return strings.Join(parts, ", ")
}

func (ix Index) withOptions(base string) string {
	var opts []string
	if ix.IgnoreDupKey {
		opts = append(opts, "IGNORE_DUP_KEY = ON")
	}
	if ix.IsPadded {
		opts = append(opts, "PAD_INDEX = ON")
	}
	if ix.FillFactor > 0 {
		opts = append(opts, fmt.Sprintf("FILLFACTOR = %d", ix.FillFactor))
	}
	if len(opts) == 0 {
		return base
	}
	return base + " WITH (" + strings.Join(opts, ", ") + ")"
}

// ConstraintDDL renders a PRIMARY KEY or UNIQUE constraint clause (no leading
// CONSTRAINT keyword handling beyond the name), suitable for inlining in
// CREATE TABLE.
func (ix Index) ConstraintDDL() string {
	kind := "UNIQUE"
	if ix.IsPrimaryKey {
		kind = "PRIMARY KEY"
	}
	clustering := "NONCLUSTERED"
	if strings.HasPrefix(ix.TypeDes, "CLUSTERED") {
		clustering = "CLUSTERED"
	}
	s := fmt.Sprintf("CONSTRAINT %s %s %s (%s)", Quote(ix.Name), kind, clustering, indexKeyList(ix.Columns, false))
	return ix.withOptions(s)
}

// CreateIndexDDL renders a standalone CREATE INDEX statement.
func (ix Index) CreateIndexDDL(table Table) string {
	if strings.Contains(ix.TypeDes, "COLUMNSTORE") {
		clustering := "NONCLUSTERED"
		if strings.HasPrefix(ix.TypeDes, "CLUSTERED") {
			clustering = "CLUSTERED"
		}
		s := fmt.Sprintf("CREATE %s COLUMNSTORE INDEX %s ON %s", clustering, Quote(ix.Name), table.QualifiedName())
		if clustering == "NONCLUSTERED" {
			// Bare column names: a columnstore index stores no sort order, and
			// SQL Server rejects the statement outright if ASC or DESC appears.
			s += " (" + plainColumnList(ix.Columns) + ")"
		}
		return s
	}

	var b strings.Builder
	b.WriteString("CREATE ")
	if ix.IsUnique {
		b.WriteString("UNIQUE ")
	}
	if strings.HasPrefix(ix.TypeDes, "CLUSTERED") {
		b.WriteString("CLUSTERED ")
	} else {
		b.WriteString("NONCLUSTERED ")
	}
	fmt.Fprintf(&b, "INDEX %s ON %s (%s)", Quote(ix.Name), table.QualifiedName(), indexKeyList(ix.Columns, false))
	if inc := indexKeyList(ix.Columns, true); inc != "" {
		b.WriteString(" INCLUDE (" + inc + ")")
	}
	if ix.FilterDefinition != "" {
		b.WriteString(" WHERE " + ix.FilterDefinition)
	}
	return ix.withOptions(b.String())
}

// AddDDL renders the ALTER TABLE statement that creates this foreign key.
func (fk ForeignKey) AddDDL(table Table) string {
	cols := make([]string, len(fk.Columns))
	for i, c := range fk.Columns {
		cols[i] = Quote(c)
	}
	refCols := make([]string, len(fk.ReferencedColumns))
	for i, c := range fk.ReferencedColumns {
		refCols[i] = Quote(c)
	}
	check := "WITH CHECK"
	if fk.IsNotTrusted {
		check = "WITH NOCHECK"
	}
	s := fmt.Sprintf("ALTER TABLE %s %s ADD CONSTRAINT %s FOREIGN KEY (%s) REFERENCES %s.%s (%s)",
		table.QualifiedName(), check, Quote(fk.Name), strings.Join(cols, ", "),
		Quote(fk.ReferencedSchema), Quote(fk.ReferencedTable), strings.Join(refCols, ", "))
	if fk.OnDelete != "" && fk.OnDelete != "NO_ACTION" {
		s += " ON DELETE " + strings.ReplaceAll(fk.OnDelete, "_", " ")
	}
	if fk.OnUpdate != "" && fk.OnUpdate != "NO_ACTION" {
		s += " ON UPDATE " + strings.ReplaceAll(fk.OnUpdate, "_", " ")
	}
	return s
}

// AddDDL renders the ALTER TABLE statement that creates this check constraint.
func (cc CheckConstraint) AddDDL(table Table) string {
	check := "WITH CHECK"
	if cc.IsDisabled {
		check = "WITH NOCHECK"
	}
	return fmt.Sprintf("ALTER TABLE %s %s ADD CONSTRAINT %s CHECK %s",
		table.QualifiedName(), check, Quote(cc.Name), cc.Definition)
}

// CreateDDL renders the CREATE TABLE statement, with primary key, unique
// constraints and column defaults inlined so the table is loadable immediately.
func (t Table) CreateDDL() string {
	lines := make([]string, 0, len(t.Columns)+4)
	for _, c := range t.Columns {
		lines = append(lines, "    "+c.ColumnDDL(true))
	}
	if t.PrimaryKey != nil {
		lines = append(lines, "    "+t.PrimaryKey.ConstraintDDL())
	}
	for _, uc := range t.UniqueConstraints {
		lines = append(lines, "    "+uc.ConstraintDDL())
	}
	return fmt.Sprintf("CREATE TABLE %s (\n%s\n)", t.QualifiedName(), strings.Join(lines, ",\n"))
}

// CreateDDL renders CREATE TYPE for a scalar alias or table type.
func (ut UserType) CreateDDL() string {
	if ut.IsTableType {
		lines := make([]string, 0, len(ut.Columns))
		for _, c := range ut.Columns {
			lines = append(lines, "    "+c.ColumnDDL(true))
		}
		return fmt.Sprintf("CREATE TYPE %s.%s AS TABLE (\n%s\n)",
			Quote(ut.Schema), Quote(ut.Name), strings.Join(lines, ",\n"))
	}
	spec := Column{TypeName: ut.BaseType, MaxLength: ut.MaxLength, Precision: ut.Precision, Scale: ut.Scale}.TypeRef()
	null := "NOT NULL"
	if ut.IsNullable {
		null = "NULL"
	}
	return fmt.Sprintf("CREATE TYPE %s.%s FROM %s %s", Quote(ut.Schema), Quote(ut.Name), spec, null)
}

// CreateDDL renders CREATE SEQUENCE.
func (s Sequence) CreateDDL() string {
	var b strings.Builder
	fmt.Fprintf(&b, "CREATE SEQUENCE %s.%s AS %s", Quote(s.Schema), Quote(s.Name), Quote(s.DataType))
	if strings.EqualFold(s.DataType, "decimal") || strings.EqualFold(s.DataType, "numeric") {
		b.Reset()
		fmt.Fprintf(&b, "CREATE SEQUENCE %s.%s AS %s(%d,%d)", Quote(s.Schema), Quote(s.Name), Quote(s.DataType), s.Precision, s.Scale)
	}
	fmt.Fprintf(&b, " START WITH %s INCREMENT BY %s", s.StartValue, s.Increment)
	if s.MinValue != "" {
		b.WriteString(" MINVALUE " + s.MinValue)
	}
	if s.MaxValue != "" {
		b.WriteString(" MAXVALUE " + s.MaxValue)
	}
	if s.IsCycling {
		b.WriteString(" CYCLE")
	} else {
		b.WriteString(" NO CYCLE")
	}
	switch {
	case s.IsCached && s.CacheSize != nil:
		fmt.Fprintf(&b, " CACHE %d", *s.CacheSize)
	case s.IsCached:
		b.WriteString(" CACHE")
	default:
		b.WriteString(" NO CACHE")
	}
	return b.String()
}
