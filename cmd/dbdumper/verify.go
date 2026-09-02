package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/JeePeeTee/dbdumper/internal/archive"
	"github.com/JeePeeTee/dbdumper/internal/model"
	"github.com/JeePeeTee/dbdumper/internal/sqlsrv"
)

// runVerify compares a restored database against the archive it came from.
func runVerify(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	conn := connFlags(fs)
	in := fs.String("in", "", "archive to compare against (required)")
	quiet := fs.Bool("quiet", false, "only report differences")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *in == "" {
		return errors.New("--in is required")
	}
	if err := finishConn(fs, conn); err != nil {
		return err
	}
	if conn.DatabaseName() == "" {
		return errors.New("--database is required")
	}

	ar, err := archive.Open(*in)
	if err != nil {
		return err
	}
	defer ar.Close()

	db, err := sqlsrv.Open(ctx, *conn)
	if err != nil {
		return err
	}
	defer db.Close()

	actual, err := actualRowCounts(ctx, db)
	if err != nil {
		return err
	}
	present, err := presentObjects(ctx, db)
	if err != nil {
		return err
	}

	var problems []string
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	if !*quiet {
		fmt.Fprintln(tw, "TABLE\tEXPECTED\tACTUAL\tSTATUS")
	}

	dbm := &ar.Manifest.Database
	sorted := append([]model.Table(nil), dbm.Tables...)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Schema+"."+sorted[i].Name < sorted[j].Schema+"."+sorted[j].Name
	})

	for _, t := range sorted {
		key := strings.ToLower(t.Schema + "." + t.Name)
		got, ok := actual[key]
		status := "ok"
		switch {
		case !ok:
			status = "MISSING TABLE"
			problems = append(problems, fmt.Sprintf("table %s.%s is missing", t.Schema, t.Name))
		case got != t.RowCount:
			status = "ROW COUNT MISMATCH"
			problems = append(problems, fmt.Sprintf("%s.%s: expected %d rows, found %d", t.Schema, t.Name, t.RowCount, got))
		}
		if !*quiet || status != "ok" {
			fmt.Fprintf(tw, "%s.%s\t%d\t%d\t%s\n", t.Schema, t.Name, t.RowCount, got, status)
		}
	}
	tw.Flush()

	// Objects other than tables: existence only.
	for _, m := range dbm.Modules {
		if !present[strings.ToLower(string(m.Kind)+":"+m.Schema+"."+m.Name)] {
			problems = append(problems, fmt.Sprintf("%s %s.%s is missing", m.Kind, m.Schema, m.Name))
		}
	}
	for _, t := range dbm.Tables {
		for _, ix := range t.Indexes {
			if !present[strings.ToLower("index:"+t.Schema+"."+t.Name+"."+ix.Name)] {
				problems = append(problems, fmt.Sprintf("index %s on %s.%s is missing", ix.Name, t.Schema, t.Name))
			}
		}
		for _, fk := range t.ForeignKeys {
			if !present[strings.ToLower("fk:"+t.Schema+"."+fk.Name)] {
				problems = append(problems, fmt.Sprintf("foreign key %s is missing", fk.Name))
			}
		}
		for _, cc := range t.CheckConstraints {
			if !present[strings.ToLower("check:"+t.Schema+"."+cc.Name)] {
				problems = append(problems, fmt.Sprintf("check constraint %s is missing", cc.Name))
			}
		}
	}

	if len(problems) == 0 {
		fmt.Printf("\nverify: OK - %d tables match\n", len(dbm.Tables))
		return nil
	}
	fmt.Printf("\nverify: %d problem(s)\n", len(problems))
	for i, p := range problems {
		if i >= 50 {
			fmt.Printf("  ... and %d more\n", len(problems)-i)
			break
		}
		fmt.Println("  -", p)
	}
	return fmt.Errorf("verification failed with %d problem(s)", len(problems))
}

func actualRowCounts(ctx context.Context, db *sql.DB) (map[string]int64, error) {
	rows, err := db.QueryContext(ctx, `
SELECT s.name, t.name, ISNULL(SUM(p.rows), 0)
FROM sys.tables t
JOIN sys.schemas s ON s.schema_id = t.schema_id
LEFT JOIN sys.partitions p ON p.object_id = t.object_id AND p.index_id IN (0, 1)
WHERE t.is_ms_shipped = 0
GROUP BY s.name, t.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]int64{}
	for rows.Next() {
		var s, t string
		var n int64
		if err := rows.Scan(&s, &t, &n); err != nil {
			return nil, err
		}
		out[strings.ToLower(s+"."+t)] = n
	}
	return out, rows.Err()
}

// presentObjects returns a set of "kind:qualified-name" keys for everything the
// verifier checks for existence.
func presentObjects(ctx context.Context, db *sql.DB) (map[string]bool, error) {
	out := map[string]bool{}

	rows, err := db.QueryContext(ctx, `
SELECT CASE o.type WHEN 'V' THEN 'view' WHEN 'P' THEN 'procedure'
                   WHEN 'TR' THEN 'trigger' ELSE 'function' END,
       s.name, o.name
FROM sys.objects o
JOIN sys.schemas s ON s.schema_id = o.schema_id
WHERE o.is_ms_shipped = 0 AND o.type IN ('V','P','FN','IF','TF','TR')`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var kind, s, n string
		if err := rows.Scan(&kind, &s, &n); err != nil {
			rows.Close()
			return nil, err
		}
		out[strings.ToLower(kind+":"+s+"."+n)] = true
	}
	// Without this a connection dropped part way through reads as "every object
	// after this point is missing", which is a confident and wrong answer.
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	queries := []struct{ prefix, sql string }{
		{"index", `SELECT s.name + '.' + t.name + '.' + i.name
FROM sys.indexes i
JOIN sys.tables t ON t.object_id = i.object_id
JOIN sys.schemas s ON s.schema_id = t.schema_id
WHERE i.name IS NOT NULL AND t.is_ms_shipped = 0`},
		{"fk", `SELECT s.name + '.' + fk.name FROM sys.foreign_keys fk
JOIN sys.schemas s ON s.schema_id = fk.schema_id`},
		{"check", `SELECT s.name + '.' + cc.name FROM sys.check_constraints cc
JOIN sys.schemas s ON s.schema_id = cc.schema_id`},
	}
	for _, q := range queries {
		r, err := db.QueryContext(ctx, q.sql)
		if err != nil {
			return nil, err
		}
		for r.Next() {
			var name string
			if err := r.Scan(&name); err != nil {
				r.Close()
				return nil, err
			}
			out[strings.ToLower(q.prefix+":"+name)] = true
		}
		if err := r.Err(); err != nil {
			r.Close()
			return nil, err
		}
		r.Close()
	}
	return out, nil
}
