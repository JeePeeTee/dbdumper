package plan

import (
	"fmt"
	"strings"

	"github.com/JeePeeTee/dbdumper/internal/model"
)

// ObjectScript is one database object's DDL, on its own.
//
// The phases in plan.go group statements by the order a restore needs them in.
// This groups the same statements by the object they belong to, which is what a
// version-controlled copy of a schema wants: a change to one procedure should
// be a change to one file.
type ObjectScript struct {
	// Kind is the directory an object of this sort belongs in: "tables",
	// "views", "procedures" and so on.
	Kind string
	// Schema is empty for objects that do not live in one.
	Schema string
	Name   string
	SQL    string
}

// ObjectScripts renders every object in the database as its own script.
//
// A table's file carries the table and everything owned by it - its indexes,
// unique constraints and check constraints - because those change together and
// reading them apart helps nobody. Foreign keys are the exception: they name a
// second table, so they live in files of their own that can all be applied
// after every table exists.
func ObjectScripts(db *model.Database) []ObjectScript {
	var out []ObjectScript

	for _, s := range db.Schemas {
		if strings.EqualFold(s.Name, "dbo") {
			continue // always present, never scripted
		}
		out = append(out, ObjectScript{
			Kind: "schemas", Name: s.Name,
			SQL: fmt.Sprintf("IF SCHEMA_ID(%s) IS NULL EXEC(%s);\n",
				model.QuoteString(s.Name), model.QuoteString("CREATE SCHEMA "+model.Quote(s.Name))),
		})
	}

	for _, ut := range db.UserTypes {
		out = append(out, ObjectScript{
			Kind: "types", Schema: ut.Schema, Name: ut.Name,
			SQL: ut.CreateDDL() + ";\n",
		})
	}

	for _, s := range db.Sequences {
		out = append(out, ObjectScript{
			Kind: "sequences", Schema: s.Schema, Name: s.Name,
			SQL: s.CreateDDL() + ";\n",
		})
	}

	// The same rule the foreign key phase applies: a table that will not hold
	// all of its rows cannot satisfy the keys pointing at it.
	partial := partialTables(db)

	for _, t := range db.Tables {
		var b strings.Builder
		b.WriteString(t.CreateDDL())
		b.WriteString(";\n")

		for _, ix := range t.Indexes {
			b.WriteString("\n")
			b.WriteString(ix.CreateIndexDDL(t))
			b.WriteString(";\n")
		}
		for _, cc := range t.CheckConstraints {
			b.WriteString("\n")
			b.WriteString(cc.AddDDL(t))
			b.WriteString(";\n")
			if cc.IsDisabled {
				fmt.Fprintf(&b, "ALTER TABLE %s NOCHECK CONSTRAINT %s;\n",
					t.QualifiedName(), model.Quote(cc.Name))
			}
		}
		out = append(out, ObjectScript{
			Kind: "tables", Schema: t.Schema, Name: t.Name, SQL: b.String(),
		})

		if len(t.ForeignKeys) == 0 {
			continue
		}
		b.Reset()
		for _, fk := range t.ForeignKeys {
			if partial[strings.ToLower(fk.ReferencedSchema+"."+fk.ReferencedTable)] {
				fk.IsNotTrusted = true
			}
			b.WriteString(fk.AddDDL(t))
			b.WriteString(";\n")
			if fk.IsDisabled {
				fmt.Fprintf(&b, "ALTER TABLE %s NOCHECK CONSTRAINT %s;\n",
					t.QualifiedName(), model.Quote(fk.Name))
			}
		}
		out = append(out, ObjectScript{
			Kind: "foreignkeys", Schema: t.Schema, Name: t.Name, SQL: b.String(),
		})
	}

	for _, m := range db.Modules {
		var b strings.Builder
		fmt.Fprintf(&b, "SET ANSI_NULLS %s;\nSET QUOTED_IDENTIFIER %s;\nGO\n",
			onOff(m.AnsiNulls), onOff(m.QuotedIdentifier))
		b.WriteString(m.Definition)
		if !strings.HasSuffix(m.Definition, "\n") {
			b.WriteString("\n")
		}
		if m.Kind == model.ModuleTrigger && m.IsDisabled {
			fmt.Fprintf(&b, "GO\nDISABLE TRIGGER %s.%s ON %s.%s;\n",
				model.Quote(m.Schema), model.Quote(m.Name),
				model.Quote(m.ParentSchema), model.Quote(m.ParentName))
		}
		out = append(out, ObjectScript{
			Kind: moduleDir(m.Kind), Schema: m.Schema, Name: m.Name, SQL: b.String(),
		})
	}

	return out
}

// moduleDir is the directory name for a kind of programmability object.
func moduleDir(k model.ModuleKind) string {
	switch k {
	case model.ModuleView:
		return "views"
	case model.ModuleFunction:
		return "functions"
	case model.ModuleTrigger:
		return "triggers"
	default:
		return "procedures"
	}
}

// ObjectDirs lists every directory ObjectScripts can write into.
//
// A caller pruning objects that have been dropped needs to know which
// directories it owns, so that it deletes only files it could have written.
func ObjectDirs() []string {
	return []string{
		"schemas", "types", "sequences", "tables",
		"foreignkeys", "views", "functions", "procedures", "triggers",
	}
}
