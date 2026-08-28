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
	"github.com/JeePeeTee/dbdumper/internal/spool"
	"github.com/JeePeeTee/dbdumper/internal/sqlsrv"
)

// Options controls a dump.
type Options struct {
	Out        string
	SchemaOnly bool
	Include    []string // glob patterns, schema.table; empty means everything
	Exclude    []string
	// Where holds "<table-glob>:<predicate>" specifications restricting which
	// rows of a table are dumped.
	Where []string
	// ExcludeData names tables whose definition is kept but whose rows are
	// skipped. Unlike Exclude, the table, its indexes and its constraints are
	// still created on restore - it just comes back empty.
	ExcludeData []string
	// Resume continues an interrupted export from its work directory instead
	// of starting over. Restart discards that directory. With neither, an
	// existing work directory is an error rather than a silent choice.
	Resume  bool
	Restart bool

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
	// ResumedTables counts tables taken from an interrupted run's work
	// directory rather than read from the database again.
	ResumedTables int
	Duration      time.Duration
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

	rowFilter, err := sqlsrv.NewRowFilter(opts.Where, opts.warn)
	if err != nil {
		return nil, err
	}
	// Recorded on the model before the fingerprint is taken, so that changing a
	// --where between runs makes a resume refuse rather than mix two filters.
	if rowFilter != nil {
		for i := range dbm.Tables {
			t := &dbm.Tables[i]
			t.RowFilter = rowFilter(t.Schema, t.Name)
		}
	}

	res := &Result{Tables: len(dbm.Tables)}

	// Rows go to a work directory first. archive/zip cannot append to a
	// finished file, so writing them straight into the archive would leave an
	// interrupted run with nothing a later one could continue from.
	sp, states, err := openSpool(dbm, src, opts)
	if err != nil {
		return nil, err
	}

	if !opts.SchemaOnly {
		if err := spoolTables(ctx, db, sp, states, dbm, res, opts); err != nil {
			return nil, err
		}
	}

	warnUntrustedKeys(dbm, opts)

	if err := packageArchive(sp, states, dbm, src, opts); err != nil {
		return nil, err
	}
	if err := sp.Discard(); err != nil {
		opts.warn("could not remove the work directory %s: %v", sp.Dir(), err)
	}

	res.Duration = time.Since(start)
	return res, nil
}

// openSpool creates or resumes the work directory and reports what an earlier
// run already finished.
func openSpool(dbm *model.Database, src model.Source, opts Options) (*spool.Spool, map[string]spool.TableState, error) {
	dir := spool.DirFor(opts.Out)
	meta := spool.Meta{
		Tool:        "dbdumper",
		StartedAt:   time.Now().UTC(),
		Server:      src.Server,
		Database:    src.Database,
		Fingerprint: fingerprint(src.Database, dbm),
	}

	if spool.Exists(dir) && !opts.Restart {
		if !opts.Resume {
			return nil, nil, fmt.Errorf("an interrupted export is present in %s; pass --resume to continue it, or --restart to discard it", dir)
		}
		sp, err := spool.Resume(dir, meta)
		if err != nil {
			return nil, nil, err
		}
		states, err := sp.Completed()
		if err != nil {
			return nil, nil, err
		}
		if len(states) > 0 {
			opts.log("resuming: %d table(s) already dumped in %s", len(states), dir)
		}
		return sp, states, nil
	}

	if opts.Resume && !spool.Exists(dir) {
		opts.warn("--resume was given but there is no interrupted export in %s; starting fresh", dir)
	}
	sp, err := spool.Create(dir, meta)
	if err != nil {
		return nil, nil, err
	}
	return sp, map[string]spool.TableState{}, nil
}

// spoolTables writes every table not already present in the work directory.
func spoolTables(ctx context.Context, db *sql.DB, sp *spool.Spool, states map[string]spool.TableState,
	dbm *model.Database, res *Result, opts Options) error {

	skipData := sqlsrv.NewTableFilter(opts.ExcludeData, nil)
	for i := range dbm.Tables {
		t := &dbm.Tables[i]
		if skipData != nil && skipData(t.Schema, t.Name) {
			t.DataSkipped = true
			opts.log("  %-50s %10s", t.Schema+"."+t.Name, "(data skipped)")
			continue
		}

		identity := strings.ToLower(t.Schema + "." + t.Name)
		if st, done := states[identity]; done {
			t.RowCount, t.DataFile = st.Rows, st.Entry
			res.Rows += st.Rows
			res.DataBytes += int64(st.UncompressedSize)
			res.ResumedTables++
			opts.log("  %-50s %10d rows  %10s (resumed)",
				t.Schema+"."+t.Name, st.Rows, humanBytes(int64(st.UncompressedSize)))
			continue
		}

		st, err := dumpTable(ctx, db, sp, i, t, opts, i+1, len(dbm.Tables))
		if err != nil {
			return fmt.Errorf("dump %s.%s: %w", t.Schema, t.Name, err)
		}
		if st.Entry == "" {
			continue // no insertable columns; nothing was spooled
		}
		states[identity] = st
		t.RowCount, t.DataFile = st.Rows, st.Entry
		res.Rows += st.Rows
		res.DataBytes += int64(st.UncompressedSize)
	}
	return nil
}

// packageArchive assembles the finished archive from the work directory.
//
// Spooled data is spliced in already compressed, and each table's spool file is
// dropped as soon as it is safely inside, so two full copies of the data never
// exist at once.
func packageArchive(sp *spool.Spool, states map[string]spool.TableState,
	dbm *model.Database, src model.Source, opts Options) error {

	opts.log("packaging %s...", opts.Out)
	w, err := archive.Create(opts.Out)
	if err != nil {
		return err
	}
	defer w.Close()

	for i := range dbm.Tables {
		t := &dbm.Tables[i]
		st, ok := states[strings.ToLower(t.Schema+"."+t.Name)]
		if !ok {
			continue
		}
		if err := copySpooled(w, sp, st); err != nil {
			return fmt.Errorf("package %s.%s: %w", t.Schema, t.Name, err)
		}
		sp.DropData(st)
	}

	for _, ph := range plan.AllPhases(dbm) {
		if err := w.AddText(ph.File, plan.ScriptFor(ph)); err != nil {
			return err
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
		return err
	}
	if err := w.AddText("README.txt", readme); err != nil {
		return err
	}
	return w.Close()
}

func copySpooled(w *archive.Writer, sp *spool.Spool, st spool.TableState) error {
	src, err := sp.OpenData(st)
	if err != nil {
		return err
	}
	defer src.Close()

	dst, err := w.AddRaw(archive.RawEntry{
		Name:             st.Entry,
		UncompressedSize: st.UncompressedSize,
		CompressedSize:   st.CompressedSize,
		CRC32:            st.CRC32,
	})
	if err != nil {
		return err
	}
	n, err := io.Copy(dst, src)
	if err != nil {
		return err
	}
	if uint64(n) != st.CompressedSize {
		return fmt.Errorf("spooled data for %s is %d bytes, expected %d", st.Entry, n, st.CompressedSize)
	}
	return nil
}

// fingerprint summarises the source schema so a resume can refuse a database
// whose shape has changed underneath it.
func fingerprint(database string, dbm *model.Database) string {
	shapes := make([]spool.TableShape, 0, len(dbm.Tables))
	for _, t := range dbm.Tables {
		cols := make([]string, 0, len(t.Columns))
		for _, c := range t.Columns {
			cols = append(cols, c.Name)
		}
		if t.RowFilter != "" {
			// Part of the shape for resume purposes: rows already spooled under
			// one predicate must not be mixed with rows read under another.
			cols = append(cols, "!where="+t.RowFilter)
		}
		shapes = append(shapes, spool.TableShape{Schema: t.Schema, Name: t.Name, Columns: cols})
	}
	return spool.Fingerprint(database, shapes)
}

// warnUntrustedKeys reports the foreign keys that --exclude-data has made
// unverifiable. Restoring them WITH NOCHECK is the only way the restore can
// succeed, but it leaves the target database referentially inconsistent, so it
// should never be a silent consequence.
func warnUntrustedKeys(dbm *model.Database, opts Options) {
	skipped := map[string]bool{}
	for _, t := range dbm.Tables {
		if t.PartialData() {
			skipped[strings.ToLower(t.Schema+"."+t.Name)] = true
		}
	}
	if len(skipped) == 0 {
		return
	}
	var affected []string
	for _, t := range dbm.Tables {
		for _, fk := range t.ForeignKeys {
			if skipped[strings.ToLower(fk.ReferencedSchema+"."+fk.ReferencedTable)] && !t.PartialData() {
				affected = append(affected, fmt.Sprintf("%s on %s.%s -> %s.%s",
					fk.Name, t.Schema, t.Name, fk.ReferencedSchema, fk.ReferencedTable))
			}
		}
	}
	if len(affected) == 0 {
		return
	}
	opts.warn("%d foreign key(s) point at a table that was skipped or filtered; they will be created", len(affected))
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
func dumpTable(ctx context.Context, db *sql.DB, sp *spool.Spool, index int, t *model.Table,
	opts Options, pos, total int) (spool.TableState, error) {

	var none spool.TableState
	cols := dataColumns(*t)
	if len(cols) == 0 {
		opts.warn("%s.%s has no insertable columns; skipping data", t.Schema, t.Name)
		return none, nil
	}
	codec := sqlsrv.NewRowCodec(cols)

	entry := archive.DataPath(t.Schema, t.Name)
	tw, err := sp.NewTable(index, strings.ToLower(t.Schema+"."+t.Name), entry)
	if err != nil {
		return none, err
	}
	// Anything short of Commit leaves the table unfinished, so a resume will
	// simply do it again.
	defer tw.Abort()

	bw := bufio.NewWriterSize(tw, 1<<20)
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
		return none, err
	}

	query := "SELECT " + codec.SelectList() + " FROM " + t.QualifiedName()
	// The predicate is raw T-SQL from the caller's own command line, spliced in
	// as written. There is nothing to escape: it is an expression, not a value,
	// and the caller already controls the connection string.
	estimated := t.EstimatedRows
	if t.RowFilter != "" {
		query += " WHERE " + t.RowFilter
		// The engine's row count is for the whole table, so it would make the
		// bar and the ETA lie. Better no percentage than a wrong one.
		estimated = 0
	}
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return none, fmt.Errorf("%s: %w", query, err)
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
			return none, err
		}
		if err := enc.Encode(codec.Encode(buf)); err != nil {
			return none, err
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
					EstimatedRows: estimated,
					Bytes:         cw.n,
					Elapsed:       now.Sub(start),
				})
			}
		}
	}
	if err := rows.Err(); err != nil {
		return none, err
	}
	if err := bw.Flush(); err != nil {
		return none, err
	}

	st, err := tw.Commit(n)
	if err != nil {
		return none, err
	}
	// A permanent line here replaces whatever transient line was last drawn.
	suffix := ""
	if t.RowFilter != "" {
		suffix = " (filtered)"
	}
	opts.log("  %-50s %10d rows  %10s%s", t.Schema+"."+t.Name, n, humanBytes(cw.n), suffix)
	return st, nil
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

An interrupted export leaves a <archive>.part work directory beside the output.
Re-run the same command with --resume to continue it, or --restart to discard it.

Restore with:
  dbdumper import --server <host> --database <newdb> --create-database --in <this file>
`
