// Package plan turns a model.Database into an ordered set of DDL phases.
//
// Both sides of the tool go through here: export writes each phase to a .sql
// entry in the archive, and import executes the very same statements. That is
// what keeps the scripts in the archive an honest record of what a restore does.
package plan

import (
	"fmt"
	"strings"

	"github.com/JeePeeTee/dbdumper/internal/model"
)

// Stmt is a single executable statement.
type Stmt struct {
	// Describe is a short label used in progress output and error messages.
	Describe string
	// SQL is what the importer executes.
	SQL string
	// Script overrides what is written to the .sql file when the executable
	// form is less readable (modules, which need sp_executesql wrapping).
	Script string
	// Retryable marks statements that may fail purely because of ordering and
	// should be attempted again after the rest of the phase has run.
	Retryable bool
}

// ScriptText returns the form to write into a .sql file.
func (s Stmt) ScriptText() string {
	if s.Script != "" {
		return s.Script
	}
	return s.SQL
}

// Phase is a named, ordered group of statements with its archive file name.
type Phase struct {
	Name  string
	File  string
	Stmts []Stmt
}

// SchemaPhases returns the DDL that must run before data is loaded.
func SchemaPhases(db *model.Database) []Phase {
	return []Phase{
		{Name: "schemas", File: "schema/010_schemas.sql", Stmts: schemas(db)},
		{Name: "user types", File: "schema/020_types.sql", Stmts: userTypes(db)},
		{Name: "sequences", File: "schema/030_sequences.sql", Stmts: sequences(db)},
		{Name: "tables", File: "schema/040_tables.sql", Stmts: tables(db)},
	}
}

// PostDataPhases returns the DDL that must run after data is loaded: indexes
// and constraints (faster and avoids ordering problems), then programmability.
func PostDataPhases(db *model.Database) []Phase {
	return []Phase{
		{Name: "indexes", File: "schema/050_indexes.sql", Stmts: indexes(db)},
		{Name: "check constraints", File: "schema/060_checks.sql", Stmts: checks(db)},
		{Name: "foreign keys", File: "schema/070_foreignkeys.sql", Stmts: foreignKeys(db)},
		{Name: "views, functions, procedures, triggers", File: "schema/080_modules.sql", Stmts: modules(db)},
		{Name: "sequence positions", File: "schema/090_finalize.sql", Stmts: finalize(db)},
	}
}

// AllPhases is SchemaPhases followed by PostDataPhases.
func AllPhases(db *model.Database) []Phase {
	return append(SchemaPhases(db), PostDataPhases(db)...)
}

func schemas(db *model.Database) []Stmt {
	var out []Stmt
	for _, s := range db.Schemas {
		if strings.EqualFold(s.Name, "dbo") {
			continue // always present
		}
		out = append(out, Stmt{
			Describe: "schema " + s.Name,
			SQL: fmt.Sprintf("IF SCHEMA_ID(%s) IS NULL EXEC(%s)",
				model.QuoteString(s.Name),
				model.QuoteString("CREATE SCHEMA "+model.Quote(s.Name))),
		})
	}
	return out
}

func userTypes(db *model.Database) []Stmt {
	var out []Stmt
	for _, ut := range db.UserTypes {
		out = append(out, Stmt{
			Describe: "type " + ut.Schema + "." + ut.Name,
			SQL:      ut.CreateDDL(),
		})
	}
	return out
}

func sequences(db *model.Database) []Stmt {
	var out []Stmt
	for _, s := range db.Sequences {
		out = append(out, Stmt{
			Describe: "sequence " + s.Schema + "." + s.Name,
			SQL:      s.CreateDDL(),
		})
	}
	return out
}

func tables(db *model.Database) []Stmt {
	var out []Stmt
	for _, t := range db.Tables {
		out = append(out, Stmt{
			Describe: "table " + t.Schema + "." + t.Name,
			// Computed columns may reference scalar functions that do not exist
			// yet, so a table can legitimately need a second attempt.
			SQL:       t.CreateDDL(),
			Retryable: hasComputed(t),
		})
	}
	return out
}

func hasComputed(t model.Table) bool {
	for _, c := range t.Columns {
		if c.IsComputed {
			return true
		}
	}
	return false
}

func indexes(db *model.Database) []Stmt {
	var out []Stmt
	for _, t := range db.Tables {
		for _, ix := range t.Indexes {
			out = append(out, Stmt{
				Describe: fmt.Sprintf("index %s on %s.%s", ix.Name, t.Schema, t.Name),
				SQL:      ix.CreateIndexDDL(t),
			})
		}
	}
	return out
}

func checks(db *model.Database) []Stmt {
	var out []Stmt
	for _, t := range db.Tables {
		for _, cc := range t.CheckConstraints {
			s := Stmt{
				Describe:  fmt.Sprintf("check %s on %s.%s", cc.Name, t.Schema, t.Name),
				SQL:       cc.AddDDL(t),
				Retryable: true, // may reference a not-yet-created function
			}
			if cc.IsDisabled {
				s.SQL += ";\nALTER TABLE " + t.QualifiedName() + " NOCHECK CONSTRAINT " + model.Quote(cc.Name)
			}
			out = append(out, s)
		}
	}
	return out
}

// partialTables is the set of tables that will not hold all of their rows,
// keyed by lower-cased qualified name.
func partialTables(db *model.Database) map[string]bool {
	out := map[string]bool{}
	for _, t := range db.Tables {
		if t.PartialData() {
			out[strings.ToLower(t.Schema+"."+t.Name)] = true
		}
	}
	return out
}

// UntrustedForeignKeys describes the foreign keys worth warning about: those
// created unvalidated by foreignKeys whose own table *is* fully loaded, and
// which will therefore hold rows referring to rows that are not there.
//
// It is deliberately narrower than the rule foreignKeys marks by. A key on a
// table that is itself only partly held is also created unvalidated, but
// reporting it adds nothing: the caller already knows that table is partial,
// and saying so for every key it owns buries the cases that matter.
func UntrustedForeignKeys(db *model.Database) []string {
	partial := partialTables(db)
	if len(partial) == 0 {
		return nil
	}
	var out []string
	for _, t := range db.Tables {
		// A partially held table satisfies its own outgoing keys: every row it
		// does hold still points at a row that exists.
		if t.PartialData() {
			continue
		}
		for _, fk := range t.ForeignKeys {
			if partial[strings.ToLower(fk.ReferencedSchema+"."+fk.ReferencedTable)] {
				out = append(out, fmt.Sprintf("%s on %s.%s -> %s.%s",
					fk.Name, t.Schema, t.Name, fk.ReferencedSchema, fk.ReferencedTable))
			}
		}
	}
	return out
}

func foreignKeys(db *model.Database) []Stmt {
	// A table holding only some of its rows - skipped outright, or filtered by
	// --where - cannot satisfy the foreign keys pointing at it, so those have
	// to be created unvalidated.
	skipped := partialTables(db)

	var out []Stmt
	for _, t := range db.Tables {
		for _, fk := range t.ForeignKeys {
			if skipped[strings.ToLower(fk.ReferencedSchema+"."+fk.ReferencedTable)] {
				fk.IsNotTrusted = true
			}
			s := Stmt{
				Describe: fmt.Sprintf("foreign key %s on %s.%s", fk.Name, t.Schema, t.Name),
				SQL:      fk.AddDDL(t),
			}
			if fk.IsDisabled {
				s.SQL += ";\nALTER TABLE " + t.QualifiedName() + " NOCHECK CONSTRAINT " + model.Quote(fk.Name)
			}
			out = append(out, s)
		}
	}
	return out
}

func modules(db *model.Database) []Stmt {
	var out []Stmt
	for _, m := range db.Modules {
		settings := fmt.Sprintf("SET ANSI_NULLS %s;\nSET QUOTED_IDENTIFIER %s;\n",
			onOff(m.AnsiNulls), onOff(m.QuotedIdentifier))

		// CREATE must be the first statement in its batch, so the definition is
		// executed through sp_executesql, which inherits the SET options above.
		// The trailing reset matters: the connection goes back to the pool
		// carrying whatever these SETs left behind.
		exec := settings + "EXEC sp_executesql N" + model.QuoteString(m.Definition) + ";\n" +
			"SET ANSI_NULLS ON;\nSET QUOTED_IDENTIFIER ON;"

		script := settings + "GO\n" + m.Definition
		s := Stmt{
			Describe:  fmt.Sprintf("%s %s.%s", m.Kind, m.Schema, m.Name),
			SQL:       exec,
			Script:    script,
			Retryable: true,
		}
		if m.Kind == model.ModuleTrigger && m.IsDisabled {
			disable := "\nDISABLE TRIGGER " + model.Quote(m.Schema) + "." + model.Quote(m.Name) +
				" ON " + model.Quote(m.ParentSchema) + "." + model.Quote(m.ParentName) + ";"
			s.SQL += disable
			s.Script += disable
		}
		out = append(out, s)
	}
	return out
}

func onOff(b bool) string {
	if b {
		return "ON"
	}
	return "OFF"
}

func finalize(db *model.Database) []Stmt {
	var out []Stmt
	for _, s := range db.Sequences {
		if s.CurrentValue == "" {
			continue
		}
		out = append(out, Stmt{
			Describe: "restart sequence " + s.Schema + "." + s.Name,
			SQL: fmt.Sprintf("ALTER SEQUENCE %s.%s RESTART WITH %s",
				model.Quote(s.Schema), model.Quote(s.Name), s.CurrentValue),
		})
	}
	for _, t := range db.Tables {
		if t.RowCount == 0 || !hasIdentity(t) {
			continue
		}
		out = append(out, Stmt{
			Describe: fmt.Sprintf("reseed identity on %s.%s", t.Schema, t.Name),
			SQL: fmt.Sprintf("DBCC CHECKIDENT (%s, RESEED)",
				model.QuoteString(t.QualifiedName())),
		})
	}
	return out
}

func hasIdentity(t model.Table) bool {
	for _, c := range t.Columns {
		if c.IsIdentity {
			return true
		}
	}
	return false
}

// ScriptFor renders a phase as a sqlcmd/SSMS-runnable script.
func ScriptFor(p Phase) string {
	var b strings.Builder
	fmt.Fprintf(&b, "/* %s */\n", p.Name)
	if len(p.Stmts) == 0 {
		b.WriteString("-- none\n")
		return b.String()
	}
	for _, s := range p.Stmts {
		fmt.Fprintf(&b, "\n-- %s\n%s\nGO\n", s.Describe, strings.TrimRight(s.ScriptText(), "\n"))
	}
	return b.String()
}
