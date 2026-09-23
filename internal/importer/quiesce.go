package importer

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/JeePeeTee/dbdumper/internal/model"
)

// liveState is how a table's constraints and triggers stood in the target
// before a data-only load switched them off, so that the load can put back
// exactly that rather than a blanket "everything on".
//
// Switching everything back on with WITH CHECK CHECK CONSTRAINT ALL did two
// wrong things. It enabled and trusted constraints the target had deliberately
// left disabled or untrusted, and it is one statement per table: a single key
// that could not be validated - one pointing at a table an archive holds only
// in part, say - failed it, and every other constraint on the table stayed off.
type liveState struct {
	constraints []liveConstraint
	triggers    []string // names of the triggers that were enabled
}

type liveConstraint struct {
	name    string
	kind    string // "foreign key" or "check", for messages
	enabled bool
	trusted bool
}

// readLiveState reads the constraint and trigger state of the given tables,
// keyed by "schema.table" exactly as the catalog spells it.
func readLiveState(ctx context.Context, db *sql.DB, tables []model.Table) (map[string]*liveState, error) {
	want := make(map[string]*liveState, len(tables))
	for _, t := range tables {
		want[t.Schema+"."+t.Name] = &liveState{}
	}

	rows, err := db.QueryContext(ctx, `
SELECT s.name, t.name, c.name, c.kind, c.is_disabled, c.is_not_trusted
FROM (
  SELECT parent_object_id, name, 'foreign key' AS kind, is_disabled, is_not_trusted FROM sys.foreign_keys
  UNION ALL
  SELECT parent_object_id, name, 'check', is_disabled, is_not_trusted FROM sys.check_constraints
) c
JOIN sys.tables t ON t.object_id = c.parent_object_id
JOIN sys.schemas s ON s.schema_id = t.schema_id
ORDER BY s.name, t.name, c.name`)
	if err != nil {
		return nil, fmt.Errorf("read constraint state: %w", err)
	}
	for rows.Next() {
		var schema, table string
		var c liveConstraint
		var disabled, untrusted bool
		if err := rows.Scan(&schema, &table, &c.name, &c.kind, &disabled, &untrusted); err != nil {
			rows.Close()
			return nil, err
		}
		if st := want[schema+"."+table]; st != nil {
			c.enabled, c.trusted = !disabled, !untrusted
			st.constraints = append(st.constraints, c)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = db.QueryContext(ctx, `
SELECT s.name, t.name, tr.name
FROM sys.triggers tr
JOIN sys.tables t ON t.object_id = tr.parent_id
JOIN sys.schemas s ON s.schema_id = t.schema_id
WHERE tr.is_disabled = 0
ORDER BY s.name, t.name, tr.name`)
	if err != nil {
		return nil, fmt.Errorf("read trigger state: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var schema, table, name string
		if err := rows.Scan(&schema, &table, &name); err != nil {
			return nil, err
		}
		if st := want[schema+"."+table]; st != nil {
			st.triggers = append(st.triggers, name)
		}
	}
	return want, rows.Err()
}

// quiesce switches off constraint checking and triggers on the tables about to
// be loaded, and returns the function that restores what was there before.
func quiesce(ctx context.Context, db *sql.DB, tables []model.Table, opts Options) (func(), error) {
	state, err := readLiveState(ctx, db, tables)
	if err != nil {
		return nil, err
	}
	for _, t := range tables {
		exec(ctx, db, "ALTER TABLE "+t.QualifiedName()+" NOCHECK CONSTRAINT ALL", opts)
		exec(ctx, db, "DISABLE TRIGGER ALL ON "+t.QualifiedName(), opts)
	}

	return func() {
		// Deliberately detached from ctx: on Ctrl+C it is already cancelled,
		// and restoring through it would fail for every table, leaving the
		// target database with its constraints unchecked and its triggers off
		// - a state this function created and must undo whether the load
		// finished or not.
		restoreCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()

		var untrusted, failed []string
		for _, t := range tables {
			st := state[t.Schema+"."+t.Name]
			if st == nil {
				continue
			}
			for _, c := range st.constraints {
				if !c.enabled {
					continue // it was off before the load; it stays off
				}
				label := fmt.Sprintf("%s %s on %s.%s", c.kind, c.name, t.Schema, t.Name)
				if c.trusted {
					// Validate it as it was validated before. Rows that break
					// it are not a reason to leave it switched off: it is
					// enabled untrusted instead, and said so.
					q := "ALTER TABLE " + t.QualifiedName() + " WITH CHECK CHECK CONSTRAINT " + model.Quote(c.name)
					if _, err := db.ExecContext(restoreCtx, q); err == nil {
						continue
					}
					untrusted = append(untrusted, label)
				}
				q := "ALTER TABLE " + t.QualifiedName() + " WITH NOCHECK CHECK CONSTRAINT " + model.Quote(c.name)
				if !execOK(restoreCtx, db, q, opts) {
					failed = append(failed, label)
				}
			}
			for _, name := range st.triggers {
				q := "ENABLE TRIGGER " + model.Quote(t.Schema) + "." + model.Quote(name) + " ON " + t.QualifiedName()
				if !execOK(restoreCtx, db, q, opts) {
					failed = append(failed, fmt.Sprintf("trigger %s on %s.%s", name, t.Schema, t.Name))
				}
			}
		}

		if len(untrusted) > 0 {
			opts.warn("%d constraint(s) no longer hold for the loaded rows; they are enabled again but left untrusted:", len(untrusted))
			for i, u := range untrusted {
				if i >= 10 {
					opts.warn("  ... and %d more", len(untrusted)-i)
					break
				}
				opts.warn("  %s", u)
			}
		}
		if len(failed) > 0 {
			// Loud, because the database is left in a state the user did not
			// ask for and cannot see without looking.
			opts.warn("could not re-enable %d constraint(s) or trigger(s): %s",
				len(failed), strings.Join(failed, ", "))
			opts.warn("they are left switched off; re-run the import or fix them by hand")
		}
	}, nil
}
