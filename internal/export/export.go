// Package export dumps a live SQL Server database into a .dbdump archive.
package export

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/JeePeeTee/dbdumper/internal/archive"
	"github.com/JeePeeTee/dbdumper/internal/model"
	"github.com/JeePeeTee/dbdumper/internal/plan"
	"github.com/JeePeeTee/dbdumper/internal/sqlsrv"
)

// Options controls a dump.
type Options struct {
	Out        string
	SchemaOnly bool
	Include    []string // glob patterns, schema.table; empty means everything
	Exclude    []string
	// ExcludeData names tables whose definition is kept but whose rows are
	// skipped. Unlike Exclude, the table, its indexes and its constraints are
	// still created on restore - it just comes back empty.
	ExcludeData []string
	// ProgressInterval is how often a long-running table reports progress.
	// Zero means the 5s default; negative disables per-table progress.
	ProgressInterval time.Duration
	// Log receives permanent lines. Optional.
	Log func(format string, args ...any)
	// Progress receives transient status updates that the caller is free to
	// render in place and overwrite. Optional. It is handed structured data
	// rather than a formatted string so that all display decisions - bar
	// width, truncation, terminal or not - stay with the caller.
	Progress func(Progress)
	// Warn receives non-fatal problems. Optional.
	Warn func(format string, args ...any)
}

func (o Options) log(format string, args ...any) {
	if o.Log != nil {
		o.Log(format, args...)
	}
}

// Progress describes how far the current table has got.
type Progress struct {
	Table      string // "schema.table"
	TableIndex int    // 1-based position in the dump
	TableCount int
	Rows       int64
	// EstimatedRows is the row count expected for this table, or 0 when the
	// server would not tell us. Progress can exceed it: the estimate is read
	// before the dump and rows can be inserted while it runs.
	EstimatedRows int64
	Bytes         int64
	Elapsed       time.Duration
}

// Fraction returns how far along this table is, in 0..1, and whether that is
// meaningful at all.
func (p Progress) Fraction() (float64, bool) {
	if p.EstimatedRows <= 0 {
		return 0, false
	}
	f := float64(p.Rows) / float64(p.EstimatedRows)
	if f > 1 {
		f = 1
	}
	return f, true
}

// RowsPerSecond is the average rate over this table so far.
func (p Progress) RowsPerSecond() float64 {
	if p.Elapsed <= 0 {
		return 0
	}
	return float64(p.Rows) / p.Elapsed.Seconds()
}

// ETA estimates the time left on this table by extrapolating the average rate.
// It reports false when there is no estimate to work from, or too little
// history for the extrapolation to mean anything.
func (p Progress) ETA() (time.Duration, bool) {
	if p.EstimatedRows <= 0 || p.Rows <= 0 || p.Elapsed < time.Second {
		return 0, false
	}
	remaining := p.EstimatedRows - p.Rows
	if remaining <= 0 {
		return 0, true
	}
	rate := p.RowsPerSecond()
	if rate <= 0 {
		return 0, false
	}
	return time.Duration(float64(remaining)/rate) * time.Second, true
}

func (o Options) progress(p Progress) {
	if o.Progress != nil {
		o.Progress(p)
	}
}

func (o Options) warn(format string, args ...any) {
	if o.Warn != nil {
		o.Warn(format, args...)
	}
}

// Result summarizes a completed dump.
type Result struct {
	Tables int
	Rows   int64
	// DataBytes is the uncompressed size of the JSONL written across all
	// tables. It is not the archive's size on disk: the archive is deflated
	// and also holds the manifest and the DDL scripts.
	DataBytes int64
	Duration  time.Duration
}

// Run performs the dump.
func Run(ctx context.Context, db *sql.DB, opts Options) (*Result, error) {
	start := time.Now()

	in := &sqlsrv.Introspector{
		DB:     db,
		Filter: sqlsrv.NewTableFilter(opts.Include, opts.Exclude),
		Warn:   opts.warn,
	}

	opts.log("reading schema...")
	src, err := in.Source(ctx)
	if err != nil {
		return nil, err
	}
	src.SchemaOnly = opts.SchemaOnly

	dbm, err := in.Database(ctx)
	if err != nil {
		return nil, err
	}
	opts.log("found %d schemas, %d tables, %d modules, %d sequences, %d user types",
		len(dbm.Schemas), len(dbm.Tables), len(dbm.Modules), len(dbm.Sequences), len(dbm.UserTypes))

	w, err := archive.Create(opts.Out)
	if err != nil {
		return nil, err
	}
	// Close is deliberate below; on the error paths the partial file is left in
	// place so the caller can inspect it.
	defer w.Close()

	res := &Result{Tables: len(dbm.Tables)}

	if !opts.SchemaOnly {
		skipData := sqlsrv.NewTableFilter(opts.ExcludeData, nil)
		for i := range dbm.Tables {
			t := &dbm.Tables[i]
			if skipData != nil && skipData(t.Schema, t.Name) {
				t.DataSkipped = true
				opts.log("  %-50s %10s", t.Schema+"."+t.Name, "(data skipped)")
				continue
			}
			n, bytes, err := dumpTable(ctx, db, w, t, opts, i+1, len(dbm.Tables))
			if err != nil {
				return nil, fmt.Errorf("dump %s.%s: %w", t.Schema, t.Name, err)
			}
			t.RowCount = n
			res.Rows += n
			res.DataBytes += bytes
		}
	}

	warnUntrustedKeys(dbm, opts)

	for _, ph := range plan.AllPhases(dbm) {
		if err := w.AddText(ph.File, plan.ScriptFor(ph)); err != nil {
			return nil, err
		}
	}

	manifest := model.Manifest{
		FormatVersion: model.FormatVersion,
		Tool:          "dbdumper",
		CreatedAt:     time.Now().UTC(),
		Source:        src,
		Database:      *dbm,
	}
	if err := w.AddJSON(archive.ManifestName, manifest); err != nil {
		return nil, err
	}
	if err := w.AddText("README.txt", readme); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}

	res.Duration = time.Since(start)
	return res, nil
}

// warnUntrustedKeys reports the foreign keys that --exclude-data has made
// unverifiable. Restoring them WITH NOCHECK is the only way the restore can
// succeed, but it leaves the target database referentially inconsistent, so it
// should never be a silent consequence.
func warnUntrustedKeys(dbm *model.Database, opts Options) {
	skipped := map[string]bool{}
	for _, t := range dbm.Tables {
		if t.DataSkipped {
			skipped[strings.ToLower(t.Schema+"."+t.Name)] = true
		}
	}
	if len(skipped) == 0 {
		return
	}
	var affected []string
	for _, t := range dbm.Tables {
		for _, fk := range t.ForeignKeys {
			if skipped[strings.ToLower(fk.ReferencedSchema+"."+fk.ReferencedTable)] && !t.DataSkipped {
				affected = append(affected, fmt.Sprintf("%s on %s.%s -> %s.%s",
					fk.Name, t.Schema, t.Name, fk.ReferencedSchema, fk.ReferencedTable))
			}
		}
	}
	if len(affected) == 0 {
		return
	}
	opts.warn("%d foreign key(s) point at a table whose data was skipped; they will be created", len(affected))
	opts.warn("WITH NOCHECK and left untrusted, so the restored database will have dangling references:")
	for i, a := range affected {
		if i >= 10 {
			opts.warn("  ... and %d more", len(affected)-i)
			break
		}
		opts.warn("  %s", a)
	}
	opts.warn("exclude the referencing tables' data too if you need referential integrity")
}

// dumpTable writes one table's rows and reports how many, and how many
// uncompressed bytes they took.
func dumpTable(ctx context.Context, db *sql.DB, w *archive.Writer, t *model.Table, opts Options, pos, total int) (int64, int64, error) {
	cols := dataColumns(*t)
	if len(cols) == 0 {
		opts.warn("%s.%s has no insertable columns; skipping data", t.Schema, t.Name)
		return 0, 0, nil
	}
	codec := sqlsrv.NewRowCodec(cols)

	entry := archive.DataPath(t.Schema, t.Name)
	out, err := w.Add(entry)
	if err != nil {
		return 0, 0, err
	}
	bw := bufio.NewWriterSize(out, 1<<20)
	cw := &countingWriter{w: bw}
	enc := json.NewEncoder(cw)
	enc.SetEscapeHTML(false)

	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.Name
	}
	header := struct {
		Table   string   `json:"table"`
		Columns []string `json:"columns"`
	}{t.Schema + "." + t.Name, names}
	if err := enc.Encode(header); err != nil {
		return 0, 0, err
	}

	query := "SELECT " + codec.SelectList() + " FROM " + t.QualifiedName()
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return 0, 0, fmt.Errorf("%s: %w", query, err)
	}
	defer rows.Close()

	// Progress is reported on a timer rather than every N rows. A row count
	// says nothing about how long a table will take - one table's rows are
	// four integers, another's are megabyte blobs - so a fixed row interval
	// either spams the small tables or goes silent for minutes on the big one.
	interval := opts.ProgressInterval
	if interval == 0 {
		interval = 5 * time.Second
	}

	var n int64
	start := time.Now()
	lastReport := start
	buf := make([]any, 0, len(cols))
	for rows.Next() {
		if err := rows.Scan(codec.ScanDest()...); err != nil {
			return 0, 0, err
		}
		if err := enc.Encode(codec.Encode(buf)); err != nil {
			return 0, 0, err
		}
		n++

		// time.Now on every row would be wasteful; rows are cheap and the
		// clock is not, so only look every so often. A blob table can take
		// seconds per row, so also check on the very first few.
		if interval > 0 && (n%64 == 0 || n < 8) {
			if now := time.Now(); now.Sub(lastReport) >= interval {
				lastReport = now
				opts.progress(Progress{
					Table:         t.Schema + "." + t.Name,
					TableIndex:    pos,
					TableCount:    total,
					Rows:          n,
					EstimatedRows: t.EstimatedRows,
					Bytes:         cw.n,
					Elapsed:       now.Sub(start),
				})
			}
		}
	}
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	if err := bw.Flush(); err != nil {
		return 0, 0, err
	}

	t.DataFile = entry
	// A permanent line here replaces whatever transient line was last drawn.
	opts.log("  %-50s %10d rows  %10s", t.Schema+"."+t.Name, n, humanBytes(cw.n))
	return n, cw.n, nil
}

// countingWriter tallies the uncompressed bytes handed to the archive, so
// progress can be reported in terms a user can compare against the source
// table's size.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func dataColumns(t model.Table) []model.Column {
	out := make([]model.Column, 0, len(t.Columns))
	for _, c := range t.Columns {
		if c.Insertable() {
			out = append(out, c)
		}
	}
	return out
}

const readme = `dbdumper archive
================

Layout
  manifest.json          full schema model; this is the authoritative source
  schema/*.sql           the same DDL, rendered for sqlcmd/SSMS (informational)
  data/<schema>.<table>.jsonl
                         first line: {"table":..,"columns":[..]}
                         remaining lines: one JSON array per row, column order
                         matching the header

Value encoding
  bit                    true / false
  integer types          JSON number
  float, real            JSON number
  decimal, numeric,
    money, smallmoney    decimal string, e.g. "12.3400"
  uniqueidentifier       "XXXXXXXX-XXXX-XXXX-XXXX-XXXXXXXXXXXX"
  binary, varbinary,
    image, geography,
    geometry, hierarchyid  base64 string
  date                   "2006-01-02"
  time                   "15:04:05.1234567"
  datetime,
    smalldatetime        "2006-01-02T15:04:05.999"
  datetime2              "2006-01-02T15:04:05.1234567"
  datetimeoffset         "2006-01-02T15:04:05.1234567+02:00"
  everything else        JSON string
  NULL                   null

Computed columns and rowversion/timestamp columns are not stored: they are
regenerated by the server on import.

Restore with:
  dbdumper import --server <host> --database <newdb> --create-database --in <this file>
`
