package sqlsrv

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/JeePeeTee/dbdumper/internal/model"
)

// TableFilter decides which tables take part in a dump.
type TableFilter func(schema, table string) bool

// Introspector reads a live database into a model.Database.
type Introspector struct {
	DB     *sql.DB
	Filter TableFilter
	Warn   func(format string, args ...any)
}

func (in *Introspector) warn(format string, args ...any) {
	if in.Warn != nil {
		in.Warn(format, args...)
	}
}

func (in *Introspector) keep(schema, table string) bool {
	return in.Filter == nil || in.Filter(schema, table)
}

// Source gathers identifying information about the connected instance.
func (in *Introspector) Source(ctx context.Context) (model.Source, error) {
	var s model.Source
	row := in.DB.QueryRowContext(ctx, `
SELECT CONVERT(nvarchar(200), SERVERPROPERTY('ServerName')),
       DB_NAME(),
       CONVERT(nvarchar(400), @@VERSION),
       CONVERT(nvarchar(200), SERVERPROPERTY('Edition')),
       CONVERT(nvarchar(200), DATABASEPROPERTYEX(DB_NAME(), 'Collation'))`)
	var server, db, version, edition, collation sql.NullString
	if err := row.Scan(&server, &db, &version, &edition, &collation); err != nil {
		return s, fmt.Errorf("read server properties: %w", err)
	}
	s.Server = server.String
	s.Database = db.String
	s.ServerVersion = strings.TrimSpace(strings.SplitN(version.String, "\n", 2)[0])
	s.Edition = edition.String
	s.Collation = collation.String
	return s, nil
}

// Database reads the full schema of the connected database.
func (in *Introspector) Database(ctx context.Context) (*model.Database, error) {
	db := &model.Database{}

	var collation sql.NullString
	if err := in.DB.QueryRowContext(ctx,
		`SELECT CONVERT(nvarchar(200), DATABASEPROPERTYEX(DB_NAME(), 'Collation'))`).Scan(&collation); err == nil {
		db.Collation = collation.String
	}

	var err error
	if db.Schemas, err = in.schemas(ctx); err != nil {
		return nil, err
	}
	if db.UserTypes, err = in.userTypes(ctx); err != nil {
		return nil, err
	}
	if db.Sequences, err = in.sequences(ctx); err != nil {
		return nil, err
	}
	if db.Tables, err = in.tables(ctx); err != nil {
		return nil, err
	}
	if db.Modules, err = in.modules(ctx, db.Tables); err != nil {
		return nil, err
	}
	return db, nil
}

func (in *Introspector) schemas(ctx context.Context) ([]model.Schema, error) {
	// schema_id >= 16384 is the block reserved for the fixed database roles.
	rows, err := in.DB.QueryContext(ctx, `
SELECT s.name, ISNULL(p.name, N'dbo')
FROM sys.schemas s
LEFT JOIN sys.database_principals p ON p.principal_id = s.principal_id
WHERE s.schema_id < 16384
  AND s.name NOT IN (N'sys', N'INFORMATION_SCHEMA', N'guest')
ORDER BY s.name`)
	if err != nil {
		return nil, fmt.Errorf("read schemas: %w", err)
	}
	defer rows.Close()

	var out []model.Schema
	for rows.Next() {
		var s model.Schema
		if err := rows.Scan(&s.Name, &s.Owner); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (in *Introspector) userTypes(ctx context.Context) ([]model.UserType, error) {
	rows, err := in.DB.QueryContext(ctx, `
SELECT s.name, t.name, t.is_table_type, ISNULL(TYPE_NAME(t.system_type_id), N''),
       t.max_length, t.precision, t.scale, t.is_nullable,
       ISNULL(tt.type_table_object_id, 0)
FROM sys.types t
JOIN sys.schemas s ON s.schema_id = t.schema_id
LEFT JOIN sys.table_types tt ON tt.user_type_id = t.user_type_id
WHERE t.is_user_defined = 1 AND t.is_assembly_type = 0
ORDER BY t.is_table_type, s.name, t.name`)
	if err != nil {
		return nil, fmt.Errorf("read user types: %w", err)
	}
	defer rows.Close()

	var (
		out      []model.UserType
		tableIDs []int64
		byID     = map[int64]int{}
	)
	for rows.Next() {
		var ut model.UserType
		var objID int64
		if err := rows.Scan(&ut.Schema, &ut.Name, &ut.IsTableType, &ut.BaseType,
			&ut.MaxLength, &ut.Precision, &ut.Scale, &ut.IsNullable, &objID); err != nil {
			return nil, err
		}
		if ut.IsTableType {
			byID[objID] = len(out)
			tableIDs = append(tableIDs, objID)
			ut.BaseType = ""
		}
		out = append(out, ut)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(tableIDs) > 0 {
		cols, err := in.columns(ctx, tableIDs)
		if err != nil {
			return nil, err
		}
		for id, cs := range cols {
			out[byID[id]].Columns = cs
		}
	}
	return out, nil
}

func (in *Introspector) sequences(ctx context.Context) ([]model.Sequence, error) {
	rows, err := in.DB.QueryContext(ctx, `
SELECT s.name, q.name, TYPE_NAME(q.user_type_id), q.precision, q.scale,
       CONVERT(varchar(64), q.start_value), CONVERT(varchar(64), q.increment),
       CONVERT(varchar(64), q.minimum_value), CONVERT(varchar(64), q.maximum_value),
       q.is_cycling, q.is_cached, q.cache_size, CONVERT(varchar(64), q.current_value)
FROM sys.sequences q
JOIN sys.schemas s ON s.schema_id = q.schema_id
ORDER BY s.name, q.name`)
	if err != nil {
		return nil, fmt.Errorf("read sequences: %w", err)
	}
	defer rows.Close()

	var out []model.Sequence
	for rows.Next() {
		var s model.Sequence
		var minV, maxV, cur sql.NullString
		var cache sql.NullInt64
		if err := rows.Scan(&s.Schema, &s.Name, &s.DataType, &s.Precision, &s.Scale,
			&s.StartValue, &s.Increment, &minV, &maxV, &s.IsCycling, &s.IsCached, &cache, &cur); err != nil {
			return nil, err
		}
		s.MinValue, s.MaxValue, s.CurrentValue = minV.String, maxV.String, cur.String
		if cache.Valid {
			n := int(cache.Int64)
			s.CacheSize = &n
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (in *Introspector) tables(ctx context.Context) ([]model.Table, error) {
	rows, err := in.DB.QueryContext(ctx, `
SELECT t.object_id, s.name, t.name,
       CASE WHEN CONVERT(int, SERVERPROPERTY('ProductMajorVersion')) >= 12
            THEN CONVERT(bit, ISNULL(OBJECTPROPERTY(t.object_id, 'TableIsMemoryOptimized'), 0))
            ELSE CONVERT(bit, 0) END
FROM sys.tables t
JOIN sys.schemas s ON s.schema_id = t.schema_id
WHERE t.is_ms_shipped = 0 AND t.type = 'U'
ORDER BY s.name, t.name`)
	if err != nil {
		return nil, fmt.Errorf("read tables: %w", err)
	}
	defer rows.Close()

	var (
		tables []model.Table
		ids    []int64
		byID   = map[int64]int{}
	)
	for rows.Next() {
		var id int64
		var t model.Table
		if err := rows.Scan(&id, &t.Schema, &t.Name, &t.IsMemoryOptimized); err != nil {
			return nil, err
		}
		if !in.keep(t.Schema, t.Name) {
			continue
		}
		byID[id] = len(tables)
		ids = append(ids, id)
		tables = append(tables, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(tables) == 0 {
		return nil, nil
	}

	cols, err := in.columns(ctx, ids)
	if err != nil {
		return nil, err
	}
	for id, cs := range cols {
		tables[byID[id]].Columns = cs
	}

	if err := in.loadIndexes(ctx, ids, tables, byID); err != nil {
		return nil, err
	}
	if err := in.loadForeignKeys(ctx, ids, tables, byID); err != nil {
		return nil, err
	}
	if err := in.loadChecks(ctx, ids, tables, byID); err != nil {
		return nil, err
	}

	for i := range tables {
		t := &tables[i]
		for _, c := range t.Columns {
			if c.Insertable() {
				t.DataColumns = append(t.DataColumns, c.Name)
			}
		}
	}

	if err := in.loadRowEstimates(ctx, tables); err != nil {
		// Only progress display depends on this, so a missing VIEW DATABASE
		// STATE permission must not fail the dump.
		in.warn("could not read row estimates (%v); progress will show no percentage", err)
	}
	return tables, nil
}

// loadRowEstimates reads the engine's maintained row counts so that progress
// has a denominator. SELECT COUNT(*) per table would be correct but would also
// scan every table twice.
func (in *Introspector) loadRowEstimates(ctx context.Context, tables []model.Table) error {
	rows, err := in.DB.QueryContext(ctx, `
SELECT s.name, t.name, SUM(ps.row_count)
FROM sys.dm_db_partition_stats ps
JOIN sys.tables t   ON t.object_id = ps.object_id
JOIN sys.schemas s  ON s.schema_id = t.schema_id
WHERE ps.index_id IN (0, 1)
GROUP BY s.name, t.name`)
	if err != nil {
		return err
	}
	defer rows.Close()

	est := map[string]int64{}
	for rows.Next() {
		var schema, name string
		var n int64
		if err := rows.Scan(&schema, &name, &n); err != nil {
			return err
		}
		est[strings.ToLower(schema+"."+name)] = n
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range tables {
		tables[i].EstimatedRows = est[strings.ToLower(tables[i].Schema+"."+tables[i].Name)]
	}
	return nil
}

// idList renders object ids as a literal IN-list. The values come from
// sys.objects, never from user input, so there is nothing to inject.
func idList(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = fmt.Sprintf("%d", id)
	}
	return strings.Join(parts, ",")
}

func (in *Introspector) columns(ctx context.Context, ids []int64) (map[int64][]model.Column, error) {
	q := `
SELECT c.object_id, c.column_id, c.name,
       CASE WHEN ty.is_user_defined = 1 THEN ts.name ELSE N'' END,
       ty.name, ISNULL(TYPE_NAME(ty.system_type_id), ty.name),
       c.max_length, c.precision, c.scale, c.is_nullable, ISNULL(c.collation_name, N''),
       c.is_identity, ISNULL(CONVERT(varchar(64), ic.seed_value), N''),
       ISNULL(CONVERT(varchar(64), ic.increment_value), N''),
       c.is_computed, ISNULL(cc.definition, N''), ISNULL(cc.is_persisted, 0),
       ISNULL(dc.name, N''), ISNULL(dc.definition, N''),
       c.is_rowguidcol, c.is_sparse
FROM sys.columns c
JOIN sys.types ty ON ty.user_type_id = c.user_type_id
JOIN sys.schemas ts ON ts.schema_id = ty.schema_id
LEFT JOIN sys.identity_columns ic ON ic.object_id = c.object_id AND ic.column_id = c.column_id
LEFT JOIN sys.computed_columns cc ON cc.object_id = c.object_id AND cc.column_id = c.column_id
LEFT JOIN sys.default_constraints dc ON dc.object_id = c.default_object_id
WHERE c.object_id IN (` + idList(ids) + `)
ORDER BY c.object_id, c.column_id`

	rows, err := in.DB.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("read columns: %w", err)
	}
	defer rows.Close()

	out := map[int64][]model.Column{}
	for rows.Next() {
		var id int64
		var c model.Column
		if err := rows.Scan(&id, &c.Ordinal, &c.Name, &c.TypeSchema, &c.TypeName, &c.BaseTypeName,
			&c.MaxLength, &c.Precision, &c.Scale, &c.IsNullable, &c.Collation,
			&c.IsIdentity, &c.IdentitySeed, &c.IdentityIncrement,
			&c.IsComputed, &c.ComputedDefinition, &c.IsPersisted,
			&c.DefaultName, &c.DefaultDefinition, &c.IsRowGUIDCol, &c.IsSparse); err != nil {
			return nil, err
		}
		out[id] = append(out[id], c)
	}
	return out, rows.Err()
}

func (in *Introspector) loadIndexes(ctx context.Context, ids []int64, tables []model.Table, byID map[int64]int) error {
	colRows, err := in.DB.QueryContext(ctx, `
SELECT ic.object_id, ic.index_id, c.name, ic.is_descending_key, ic.is_included_column
FROM sys.index_columns ic
JOIN sys.columns c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
WHERE ic.object_id IN (`+idList(ids)+`)
ORDER BY ic.object_id, ic.index_id, ic.is_included_column, ic.key_ordinal, ic.index_column_id`)
	if err != nil {
		return fmt.Errorf("read index columns: %w", err)
	}
	type ikey struct {
		obj int64
		idx int
	}
	idxCols := map[ikey][]model.IndexColumn{}
	for colRows.Next() {
		var k ikey
		var ic model.IndexColumn
		if err := colRows.Scan(&k.obj, &k.idx, &ic.Name, &ic.IsDescending, &ic.IsIncluded); err != nil {
			colRows.Close()
			return err
		}
		idxCols[k] = append(idxCols[k], ic)
	}
	colRows.Close()
	if err := colRows.Err(); err != nil {
		return err
	}

	rows, err := in.DB.QueryContext(ctx, `
SELECT i.object_id, i.index_id, ISNULL(i.name, N''), i.type, i.type_desc,
       i.is_unique, i.is_primary_key, i.is_unique_constraint,
       ISNULL(i.filter_definition, N''), i.fill_factor, i.is_padded,
       i.ignore_dup_key, i.is_disabled
FROM sys.indexes i
WHERE i.object_id IN (`+idList(ids)+`)
  AND i.type <> 0 AND i.is_hypothetical = 0
ORDER BY i.object_id, i.index_id`)
	if err != nil {
		return fmt.Errorf("read indexes: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var objID int64
		var indexID, indexType int
		var ix model.Index
		if err := rows.Scan(&objID, &indexID, &ix.Name, &indexType, &ix.TypeDes,
			&ix.IsUnique, &ix.IsPrimaryKey, &ix.IsUniqueConstraint,
			&ix.FilterDefinition, &ix.FillFactor, &ix.IsPadded,
			&ix.IgnoreDupKey, &ix.IsDisabled); err != nil {
			return err
		}
		t := &tables[byID[objID]]
		switch indexType {
		case 1, 2, 5, 6: // clustered, nonclustered, clustered/nonclustered columnstore
		default:
			in.warn("skipping index %s on %s.%s: unsupported type %s", ix.Name, t.Schema, t.Name, ix.TypeDes)
			continue
		}
		ix.Columns = idxCols[ikey{objID, indexID}]

		switch {
		case ix.IsPrimaryKey:
			t.PrimaryKey = &ix
		case ix.IsUniqueConstraint:
			t.UniqueConstraints = append(t.UniqueConstraints, ix)
		default:
			t.Indexes = append(t.Indexes, ix)
		}
	}
	return rows.Err()
}

func (in *Introspector) loadForeignKeys(ctx context.Context, ids []int64, tables []model.Table, byID map[int64]int) error {
	colRows, err := in.DB.QueryContext(ctx, `
SELECT fkc.constraint_object_id, pc.name, rc.name
FROM sys.foreign_key_columns fkc
JOIN sys.columns pc ON pc.object_id = fkc.parent_object_id AND pc.column_id = fkc.parent_column_id
JOIN sys.columns rc ON rc.object_id = fkc.referenced_object_id AND rc.column_id = fkc.referenced_column_id
WHERE fkc.parent_object_id IN (`+idList(ids)+`)
ORDER BY fkc.constraint_object_id, fkc.constraint_column_id`)
	if err != nil {
		return fmt.Errorf("read foreign key columns: %w", err)
	}
	type fkCols struct{ local, remote []string }
	cols := map[int64]*fkCols{}
	for colRows.Next() {
		var id int64
		var l, r string
		if err := colRows.Scan(&id, &l, &r); err != nil {
			colRows.Close()
			return err
		}
		c := cols[id]
		if c == nil {
			c = &fkCols{}
			cols[id] = c
		}
		c.local = append(c.local, l)
		c.remote = append(c.remote, r)
	}
	colRows.Close()
	if err := colRows.Err(); err != nil {
		return err
	}

	rows, err := in.DB.QueryContext(ctx, `
SELECT fk.object_id, fk.name, fk.parent_object_id, rs.name, rt.name,
       fk.delete_referential_action_desc, fk.update_referential_action_desc,
       fk.is_disabled, fk.is_not_trusted
FROM sys.foreign_keys fk
JOIN sys.tables rt ON rt.object_id = fk.referenced_object_id
JOIN sys.schemas rs ON rs.schema_id = rt.schema_id
WHERE fk.parent_object_id IN (`+idList(ids)+`)
ORDER BY fk.name`)
	if err != nil {
		return fmt.Errorf("read foreign keys: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var fkID, parentID int64
		var fk model.ForeignKey
		if err := rows.Scan(&fkID, &fk.Name, &parentID, &fk.ReferencedSchema, &fk.ReferencedTable,
			&fk.OnDelete, &fk.OnUpdate, &fk.IsDisabled, &fk.IsNotTrusted); err != nil {
			return err
		}
		if c := cols[fkID]; c != nil {
			fk.Columns, fk.ReferencedColumns = c.local, c.remote
		}
		t := &tables[byID[parentID]]
		if !in.keep(fk.ReferencedSchema, fk.ReferencedTable) {
			in.warn("skipping foreign key %s on %s.%s: referenced table %s.%s is excluded",
				fk.Name, t.Schema, t.Name, fk.ReferencedSchema, fk.ReferencedTable)
			continue
		}
		t.ForeignKeys = append(t.ForeignKeys, fk)
	}
	return rows.Err()
}

func (in *Introspector) loadChecks(ctx context.Context, ids []int64, tables []model.Table, byID map[int64]int) error {
	rows, err := in.DB.QueryContext(ctx, `
SELECT cc.parent_object_id, cc.name, cc.definition, cc.is_disabled
FROM sys.check_constraints cc
WHERE cc.parent_object_id IN (`+idList(ids)+`)
ORDER BY cc.name`)
	if err != nil {
		return fmt.Errorf("read check constraints: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var parentID int64
		var cc model.CheckConstraint
		if err := rows.Scan(&parentID, &cc.Name, &cc.Definition, &cc.IsDisabled); err != nil {
			return err
		}
		t := &tables[byID[parentID]]
		t.CheckConstraints = append(t.CheckConstraints, cc)
	}
	return rows.Err()
}

func (in *Introspector) modules(ctx context.Context, tables []model.Table) ([]model.Module, error) {
	rows, err := in.DB.QueryContext(ctx, `
SELECT s.name, o.name, o.type, m.definition, m.uses_ansi_nulls, m.uses_quoted_identifier,
       m.is_schema_bound, ISNULL(m.execute_as_principal_id, -1),
       ISNULL(ps.name, N''), ISNULL(pt.name, N''),
       ISNULL(tr.is_disabled, 0), ISNULL(tr.is_instead_of_trigger, 0)
FROM sys.sql_modules m
JOIN sys.objects o ON o.object_id = m.object_id
JOIN sys.schemas s ON s.schema_id = o.schema_id
LEFT JOIN sys.triggers tr ON tr.object_id = o.object_id
LEFT JOIN sys.tables pt ON pt.object_id = o.parent_object_id
LEFT JOIN sys.schemas ps ON ps.schema_id = pt.schema_id
WHERE o.is_ms_shipped = 0
  AND o.type IN ('V','P','FN','IF','TF','TR')
ORDER BY o.type, s.name, o.name`)
	if err != nil {
		return nil, fmt.Errorf("read modules: %w", err)
	}
	defer rows.Close()

	var out []model.Module
	for rows.Next() {
		var m model.Module
		var typ string
		var principalID int64
		var parentSchema, parentTable string
		if err := rows.Scan(&m.Schema, &m.Name, &typ, &m.Definition, &m.AnsiNulls, &m.QuotedIdentifier,
			&m.IsSchemaBound, &principalID, &parentSchema, &parentTable,
			&m.IsDisabled, &m.IsInsteadOfTrigger); err != nil {
			return nil, err
		}
		switch strings.TrimSpace(typ) {
		case "V":
			m.Kind, m.OrderHintPreference = model.ModuleView, 0
		case "FN", "IF", "TF":
			m.Kind, m.OrderHintPreference = model.ModuleFunction, 0
		case "P":
			m.Kind, m.OrderHintPreference = model.ModuleProcedure, 1
		case "TR":
			m.Kind, m.OrderHintPreference = model.ModuleTrigger, 2
			m.ParentSchema, m.ParentName = parentSchema, parentTable
		}
		if m.Kind == model.ModuleTrigger && !in.keep(parentSchema, parentTable) {
			continue
		}
		m.Definition = strings.TrimRight(m.Definition, " \t\r\n")
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out, err = in.pruneModulesReferencingExcluded(ctx, out)
	if err != nil {
		return nil, err
	}

	if err := orderModulesByDependency(ctx, in.DB, out); err != nil {
		in.warn("could not resolve module dependencies (%v); falling back to retry ordering", err)
	}
	return out, nil
}

// pruneModulesReferencingExcluded drops views, functions and procedures that
// depend on a table --include or --exclude left out of the dump.
//
// Keeping them produces an archive that cannot be restored: CREATE VIEW fails
// with "Invalid object name" and the import aborts. Removal is transitive,
// since a view built on a dropped view is equally unusable.
func (in *Introspector) pruneModulesReferencingExcluded(ctx context.Context, mods []model.Module) ([]model.Module, error) {
	if in.Filter == nil || len(mods) == 0 {
		return mods, nil
	}

	// Which tables the filter left out. Anything not a table in this database
	// - a system view, a type - is not our concern and must not trigger a drop.
	rows, err := in.DB.QueryContext(ctx, `
SELECT s.name, t.name
FROM sys.tables t
JOIN sys.schemas s ON s.schema_id = t.schema_id
WHERE t.is_ms_shipped = 0 AND t.type = 'U'`)
	if err != nil {
		return nil, fmt.Errorf("read table list for module pruning: %w", err)
	}
	excluded := map[string]bool{}
	for rows.Next() {
		var schema, name string
		if err := rows.Scan(&schema, &name); err != nil {
			rows.Close()
			return nil, err
		}
		if !in.keep(schema, name) {
			excluded[strings.ToLower(schema+"."+name)] = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(excluded) == 0 {
		return mods, nil
	}

	deps, err := moduleDependencies(ctx, in.DB)
	if err != nil {
		// Without the dependency graph the safe choice is to keep everything
		// and let the importer's retry-then-report handle the fallout.
		in.warn("could not read module dependencies (%v); modules referencing excluded tables may fail on restore", err)
		return mods, nil
	}

	// Fixed point: a module goes when it references anything already gone.
	dropped := map[string]bool{}
	for changed := true; changed; {
		changed = false
		for _, m := range mods {
			key := strings.ToLower(m.Schema + "." + m.Name)
			if dropped[key] {
				continue
			}
			for _, ref := range deps[key] {
				if excluded[ref] || dropped[ref] {
					dropped[key] = true
					changed = true
					in.warn("skipping %s %s.%s: it references %s, which is not in this dump",
						m.Kind, m.Schema, m.Name, ref)
					break
				}
			}
		}
	}
	if len(dropped) == 0 {
		return mods, nil
	}

	kept := mods[:0]
	for _, m := range mods {
		if !dropped[strings.ToLower(m.Schema+"."+m.Name)] {
			kept = append(kept, m)
		}
	}
	return kept, nil
}

// moduleDependencies maps each module to the lower-cased "schema.name" of every
// entity it references.
func moduleDependencies(ctx context.Context, db *sql.DB) (map[string][]string, error) {
	rows, err := db.QueryContext(ctx, `
SELECT DISTINCT ss.name, so.name, ISNULL(d.referenced_schema_name, SCHEMA_NAME()), d.referenced_entity_name
FROM sys.sql_expression_dependencies d
JOIN sys.objects so ON so.object_id = d.referencing_id
JOIN sys.schemas ss ON ss.schema_id = so.schema_id
WHERE d.referenced_entity_name IS NOT NULL AND d.is_ambiguous = 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string][]string{}
	for rows.Next() {
		var fromSchema, fromName, toSchema, toName string
		if err := rows.Scan(&fromSchema, &fromName, &toSchema, &toName); err != nil {
			return nil, err
		}
		from := strings.ToLower(fromSchema + "." + fromName)
		out[from] = append(out[from], strings.ToLower(toSchema+"."+toName))
	}
	return out, rows.Err()
}

// orderModulesByDependency topologically sorts modules so that a view or
// function is created after everything it references. Cycles and unresolvable
// entries keep their original relative order; the importer retries anything
// that still fails.
func orderModulesByDependency(ctx context.Context, db *sql.DB, mods []model.Module) error {
	if len(mods) == 0 {
		return nil
	}
	index := make(map[string]int, len(mods))
	for i, m := range mods {
		index[strings.ToLower(m.Schema+"."+m.Name)] = i
	}

	rows, err := db.QueryContext(ctx, `
SELECT DISTINCT ss.name, so.name, ISNULL(d.referenced_schema_name, SCHEMA_NAME()), d.referenced_entity_name
FROM sys.sql_expression_dependencies d
JOIN sys.objects so ON so.object_id = d.referencing_id
JOIN sys.schemas ss ON ss.schema_id = so.schema_id
WHERE d.referenced_entity_name IS NOT NULL AND d.is_ambiguous = 0`)
	if err != nil {
		return err
	}
	defer rows.Close()

	deps := make([][]int, len(mods))
	for rows.Next() {
		var fromSchema, fromName, toSchema, toName string
		if err := rows.Scan(&fromSchema, &fromName, &toSchema, &toName); err != nil {
			return err
		}
		from, ok1 := index[strings.ToLower(fromSchema+"."+fromName)]
		to, ok2 := index[strings.ToLower(toSchema+"."+toName)]
		if ok1 && ok2 && from != to {
			deps[from] = append(deps[from], to)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// Kahn's algorithm over "must be created before" edges, with the original
	// order as a stable tiebreaker.
	indeg := make([]int, len(mods))
	adj := make([][]int, len(mods))
	for from, list := range deps {
		for _, to := range list {
			adj[to] = append(adj[to], from)
			indeg[from]++
		}
	}
	var ready []int
	for i := range mods {
		if indeg[i] == 0 {
			ready = append(ready, i)
		}
	}
	sortByPreference(mods, ready)

	order := make([]int, 0, len(mods))
	placed := make([]bool, len(mods))
	for len(ready) > 0 {
		n := ready[0]
		ready = ready[1:]
		order = append(order, n)
		placed[n] = true
		var next []int
		for _, m := range adj[n] {
			indeg[m]--
			if indeg[m] == 0 {
				next = append(next, m)
			}
		}
		if len(next) > 0 {
			ready = append(ready, next...)
			sortByPreference(mods, ready)
		}
	}
	for i := range mods {
		if !placed[i] {
			order = append(order, i) // part of a cycle; importer will retry
		}
	}

	sorted := make([]model.Module, len(mods))
	for i, idx := range order {
		sorted[i] = mods[idx]
	}
	copy(mods, sorted)
	return nil
}

func sortByPreference(mods []model.Module, idx []int) {
	sort.SliceStable(idx, func(a, b int) bool {
		return mods[idx[a]].OrderHintPreference < mods[idx[b]].OrderHintPreference
	})
}
