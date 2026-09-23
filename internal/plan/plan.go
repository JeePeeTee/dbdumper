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
			SQL:      schemaSQL(s),
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
		// A disabled index is created here, while the table is still empty,
		// and disabled at once. Building it after the load, as the enabled
		// ones are, would build what the source had switched off - and a
		// unique index is often switched off precisely because the rows no
		// longer satisfy it, so the build would fail and abort the restore.
		// Disabled, it is not maintained while the rows go in.
		for _, ix := range t.Indexes {
			if !ix.IsDisabled {
				continue
			}
			out = append(out, Stmt{
				Describe:  fmt.Sprintf("disabled index %s on %s.%s", ix.Name, t.Schema, t.Name),
				SQL:       indexSQL(t, ix),
				Retryable: hasComputed(t), // follows its table if that is deferred
			})
		}
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
			if ix.IsDisabled {
				continue // created with its table; see tables
			}
			out = append(out, Stmt{
				Describe: fmt.Sprintf("index %s on %s.%s", ix.Name, t.Schema, t.Name),
				SQL:      indexSQL(t, ix),
			})
		}
	}
	return out
}

func checks(db *model.Database) []Stmt {
	var out []Stmt
	for _, t := range db.Tables {
		for _, cc := range t.CheckConstraints {
			out = append(out, Stmt{
				Describe:  fmt.Sprintf("check %s on %s.%s", cc.Name, t.Schema, t.Name),
				SQL:       checkSQL(t, cc),
				Retryable: true, // may reference a not-yet-created function
			})
		}
	}
	return out
}

// partialTables is the set of tables that will not hold all of their rows,
// keyed by "schema.table" exactly as the catalog spells it. A foreign key names
// its target the same way, and folding case would, in a case-sensitive
// database, mark dbo.order partial because dbo.Order is.
func partialTables(db *model.Database) map[string]bool {
	out := map[string]bool{}
	for _, t := range db.Tables {
		if t.PartialData() {
			out[t.Schema+"."+t.Name] = true
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
			if partial[fk.ReferencedSchema+"."+fk.ReferencedTable] {
				out = append(out, fmt.Sprintf("%s on %s.%s -> %s.%s",
					fk.Name, t.Schema, t.Name, fk.ReferencedSchema, fk.ReferencedTable))
			}
		}
	}
	return out
}

func foreignKeys(db *model.Database) []Stmt {
	partial := partialTables(db)

	var out []Stmt
	for _, t := range db.Tables {
		for _, fk := range t.ForeignKeys {
			out = append(out, Stmt{
				Describe: fmt.Sprintf("foreign key %s on %s.%s", fk.Name, t.Schema, t.Name),
				SQL:      foreignKeySQL(t, fk, partial),
			})
		}
	}
	return out
}

func modules(db *model.Database) []Stmt {
	var out []Stmt
	for _, m := range db.Modules {
		settings := moduleSettings(m)

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
			disable := "\n" + disableTriggerSQL(m) + ";"
			s.SQL += disable
			s.Script += disable
		}
		out = append(out, s)
	}
	return out
}

// The functions below render one object each. The restore phases above and
// the per-object scripts in objects.go are both built from them, so what a
// schema directory records cannot drift from what a restore executes.

func schemaSQL(s model.Schema) string {
	return fmt.Sprintf("IF SCHEMA_ID(%s) IS NULL EXEC(%s)",
		model.QuoteString(s.Name), model.QuoteString("CREATE SCHEMA "+model.Quote(s.Name)))
}

// indexSQL creates an index, and leaves it disabled if the source had it so.
func indexSQL(t model.Table, ix model.Index) string {
	s := ix.CreateIndexDDL(t)
	if ix.IsDisabled {
		s += ";\n" + ix.DisableDDL(t)
	}
	return s
}

func checkSQL(t model.Table, cc model.CheckConstraint) string {
	s := cc.AddDDL(t)
	if cc.IsDisabled {
		s += ";\nALTER TABLE " + t.QualifiedName() + " NOCHECK CONSTRAINT " + model.Quote(cc.Name)
	}
	return s
}

// foreignKeySQL creates a foreign key. One pointing at a table that holds
// only some of its rows - skipped outright, or filtered by --where - cannot be
// satisfied, so it is created unvalidated.
func foreignKeySQL(t model.Table, fk model.ForeignKey, partial map[string]bool) string {
	if partial[fk.ReferencedSchema+"."+fk.ReferencedTable] {
		fk.IsNotTrusted = true
	}
	s := fk.AddDDL(t)
	if fk.IsDisabled {
		s += ";\nALTER TABLE " + t.QualifiedName() + " NOCHECK CONSTRAINT " + model.Quote(fk.Name)
	}
	return s
}

func moduleSettings(m model.Module) string {
	return fmt.Sprintf("SET ANSI_NULLS %s;\nSET QUOTED_IDENTIFIER %s;\n",
		onOff(m.AnsiNulls), onOff(m.QuotedIdentifier))
}

func disableTriggerSQL(m model.Module) string {
	return "DISABLE TRIGGER " + model.Quote(m.Schema) + "." + model.Quote(m.Name) +
		" ON " + model.Quote(m.ParentSchema) + "." + model.Quote(m.ParentName)
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
